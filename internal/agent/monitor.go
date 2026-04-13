package agent

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/BillBeam/adguard-agent/internal/store"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// MonitorReport is the output of the system-level health check.
type MonitorReport struct {
	TotalReviews     int
	RejectionRate    float64
	AvgConfidence    float64
	OverrideRate     float64
	Anomalies        []Anomaly
	Recommendations  []string
	PerceptionChecks []PerceptionCheck // always 5 checks, populated by RunMonitor
}

// Anomaly represents a detected operational issue.
type Anomaly struct {
	Type        string // "rejection_spike", "confidence_drop", "advertiser_cluster", "policy_hotspot"
	Description string
	Severity    string // "high", "medium", "low"
}

// PerceptionCheck represents one dimension of the system health checklist.
type PerceptionCheck struct {
	Name    string // display name: "Rejection spike", "Override rate", etc.
	Status  string // "normal", "triggered", "warning"
	Summary string // e.g., "33% (threshold: 50%) — normal"
}

// RunMonitor analyzes ReviewStore data to detect anomalies and generate an operational report.
// This implements the system-level Perception (感知) stage — monitoring the review system itself
// rather than individual ad content.
func RunMonitor(rs *store.ReviewStore, logger *slog.Logger) *MonitorReport {
	if rs == nil {
		return &MonitorReport{}
	}

	stats := rs.Stats()
	report := &MonitorReport{
		TotalReviews:  stats.Total,
		AvgConfidence: stats.AverageConfidence,
	}

	if stats.Total == 0 {
		report.Recommendations = append(report.Recommendations, "No reviews to analyze")
		return report
	}

	// Rejection rate.
	rejected := stats.ByDecision[types.DecisionRejected]
	report.RejectionRate = float64(rejected) / float64(stats.Total)

	// Override rate (verification overrides / total verified).
	if stats.VerifiedCount > 0 {
		report.OverrideRate = float64(stats.OverrideCount) / float64(stats.VerifiedCount)
	}

	// --- Anomaly detection ---

	// 1. Overall rejection rate spike.
	if report.RejectionRate > 0.5 && stats.Total >= 3 {
		report.Anomalies = append(report.Anomalies, Anomaly{
			Type:        "rejection_spike",
			Description: fmt.Sprintf("Overall rejection rate %.0f%% exceeds 50%% threshold (%d/%d ads rejected)", report.RejectionRate*100, rejected, stats.Total),
			Severity:    "high",
		})
	}

	// 2. Verification override rate.
	if report.OverrideRate > 0.2 && stats.VerifiedCount >= 2 {
		report.Anomalies = append(report.Anomalies, Anomaly{
			Type:        "confidence_drop",
			Description: fmt.Sprintf("Verification override rate %.0f%% suggests false positive issues (%d overrides in %d verifications)", report.OverrideRate*100, stats.OverrideCount, stats.VerifiedCount),
			Severity:    "high",
		})
	}

	// 3. Advertiser violation clustering.
	advertiserViolations := map[string]int{}
	for _, rec := range rs.QueryByDecision(types.DecisionRejected) {
		advertiserViolations[rec.AdvertiserID]++
	}
	for advID, count := range advertiserViolations {
		if count >= 3 {
			report.Anomalies = append(report.Anomalies, Anomaly{
				Type:        "advertiser_cluster",
				Description: fmt.Sprintf("Advertiser %s has %d rejections — possible systematic violation pattern", advID, count),
				Severity:    "medium",
			})
		}
	}

	// 4. Policy hotspots — most frequently violated policies.
	policyHits := map[string]int{}
	for _, rec := range rs.QueryByDecision(types.DecisionRejected) {
		for _, v := range rec.Violations {
			policyHits[v.PolicyID]++
		}
	}
	type policyCount struct {
		id    string
		count int
	}
	var sorted []policyCount
	for id, c := range policyHits {
		sorted = append(sorted, policyCount{id, c})
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].count > sorted[j].count })
	if len(sorted) > 0 && sorted[0].count >= 2 {
		report.Anomalies = append(report.Anomalies, Anomaly{
			Type:        "policy_hotspot",
			Description: fmt.Sprintf("Top violated policy: %s (%d hits)", sorted[0].id, sorted[0].count),
			Severity:    "low",
		})
	}

	// 5. Low average confidence.
	if report.AvgConfidence < 0.7 && stats.Total >= 3 {
		report.Anomalies = append(report.Anomalies, Anomaly{
			Type:        "confidence_drop",
			Description: fmt.Sprintf("Average confidence %.2f is below 0.70 threshold — review quality may be degrading", report.AvgConfidence),
			Severity:    "medium",
		})
	}

	// --- Recommendations ---
	for _, a := range report.Anomalies {
		switch a.Type {
		case "rejection_spike":
			report.Recommendations = append(report.Recommendations, "Review recent policy changes that may be causing over-rejection")
		case "confidence_drop":
			report.Recommendations = append(report.Recommendations, "Investigate low-confidence reviews for model calibration issues")
		case "advertiser_cluster":
			report.Recommendations = append(report.Recommendations, "Flag clustered advertisers for enhanced scrutiny or proactive outreach")
		case "policy_hotspot":
			report.Recommendations = append(report.Recommendations, "Consider refining frequently-triggered policy rules to reduce ambiguity")
		}
	}

	// Flag metrics that look concerning but have insufficient sample size.
	if report.OverrideRate > 0.5 && stats.VerifiedCount < 2 {
		report.Recommendations = append(report.Recommendations,
			fmt.Sprintf("Override rate %.0f%% appears high but sample size is too small (%d verification) — monitor as more data arrives",
				report.OverrideRate*100, stats.VerifiedCount))
	}

	if len(report.Anomalies) == 0 && len(report.Recommendations) == 0 {
		report.Recommendations = append(report.Recommendations, "No anomalies detected — system operating normally")
	}

	// --- Build perception checklist (always all 5 dimensions) ---

	// 1. Rejection spike
	rejCheck := PerceptionCheck{Name: "Rejection spike"}
	if report.RejectionRate > 0.5 && stats.Total >= 3 {
		rejCheck.Status = "triggered"
		rejCheck.Summary = fmt.Sprintf("%.0f%% (threshold: 50%%) — ANOMALY", report.RejectionRate*100)
	} else {
		rejCheck.Status = "normal"
		rejCheck.Summary = fmt.Sprintf("%.0f%% (threshold: 50%%) — normal", report.RejectionRate*100)
	}
	report.PerceptionChecks = append(report.PerceptionChecks, rejCheck)

	// 2. Override rate
	overrideCheck := PerceptionCheck{Name: "Override rate"}
	if report.OverrideRate > 0.2 && stats.VerifiedCount >= 2 {
		overrideCheck.Status = "triggered"
		overrideCheck.Summary = fmt.Sprintf("%.0f%% (threshold: 20%%) — false positive signal", report.OverrideRate*100)
	} else if report.OverrideRate > 0.2 && stats.VerifiedCount < 2 {
		overrideCheck.Status = "warning"
		overrideCheck.Summary = fmt.Sprintf("%.0f%% (threshold: 20%%) — sample too small (%d verification)", report.OverrideRate*100, stats.VerifiedCount)
	} else if stats.VerifiedCount == 0 {
		overrideCheck.Status = "normal"
		overrideCheck.Summary = "no verifications yet"
	} else {
		overrideCheck.Status = "normal"
		overrideCheck.Summary = fmt.Sprintf("%.0f%% (threshold: 20%%) — normal", report.OverrideRate*100)
	}
	report.PerceptionChecks = append(report.PerceptionChecks, overrideCheck)

	// 3. Advertiser cluster
	clusterCheck := PerceptionCheck{Name: "Advertiser cluster"}
	clusterFound := false
	for advID, count := range advertiserViolations {
		if count >= 3 {
			clusterCheck.Status = "triggered"
			clusterCheck.Summary = fmt.Sprintf("%s has %d rejections — systematic violation", advID, count)
			clusterFound = true
			break
		}
	}
	if !clusterFound {
		clusterCheck.Status = "normal"
		clusterCheck.Summary = "no repeat offenders detected"
	}
	report.PerceptionChecks = append(report.PerceptionChecks, clusterCheck)

	// 4. Policy hotspot
	hotspotCheck := PerceptionCheck{Name: "Policy hotspot"}
	if len(sorted) > 0 && sorted[0].count >= 2 {
		hotspotCheck.Status = "triggered"
		hotspotCheck.Summary = fmt.Sprintf("%s (%d hits) — above threshold", sorted[0].id, sorted[0].count)
	} else if len(sorted) > 0 {
		hotspotCheck.Status = "normal"
		hotspotCheck.Summary = fmt.Sprintf("%s (%d hits) — below threshold", sorted[0].id, sorted[0].count)
	} else {
		hotspotCheck.Status = "normal"
		hotspotCheck.Summary = "no policy violations detected"
	}
	report.PerceptionChecks = append(report.PerceptionChecks, hotspotCheck)

	// 5. Confidence drop
	confCheck := PerceptionCheck{Name: "Confidence drop"}
	if report.AvgConfidence < 0.7 && stats.Total >= 3 {
		confCheck.Status = "triggered"
		confCheck.Summary = fmt.Sprintf("%.2f (threshold: 0.70) — review quality degrading", report.AvgConfidence)
	} else {
		confCheck.Status = "normal"
		confCheck.Summary = fmt.Sprintf("%.2f (threshold: 0.70) — normal", report.AvgConfidence)
	}
	report.PerceptionChecks = append(report.PerceptionChecks, confCheck)

	logger.Info("monitor report generated",
		slog.Int("total_reviews", report.TotalReviews),
		slog.Float64("rejection_rate", report.RejectionRate),
		slog.Int("anomalies", len(report.Anomalies)),
	)

	return report
}

// FormatReport returns a human-readable string representation of the monitor report.
func (r *MonitorReport) FormatReport() string {
	var b strings.Builder

	fmt.Fprintf(&b, "  Reviews: %d | Rejection rate: %.0f%% | Avg confidence: %.2f | Override rate: %.0f%%\n",
		r.TotalReviews, r.RejectionRate*100, r.AvgConfidence, r.OverrideRate*100)

	// Perception checks: show full checklist when populated by RunMonitor.
	if len(r.PerceptionChecks) > 0 {
		b.WriteString("  Perception checks:\n")
		for _, pc := range r.PerceptionChecks {
			icon := "✓"
			if pc.Status == "triggered" {
				icon = "✗"
			} else if pc.Status == "warning" {
				icon = "⚠"
			}
			fmt.Fprintf(&b, "    %s %-20s %s\n", icon, pc.Name, pc.Summary)
		}
	} else if len(r.Anomalies) > 0 {
		// Fallback: legacy anomaly display (for direct MonitorReport construction).
		b.WriteString("  Anomalies:\n")
		for _, a := range r.Anomalies {
			fmt.Fprintf(&b, "    [%s] %s — %s\n", a.Severity, a.Type, a.Description)
		}
	}

	if len(r.Recommendations) > 0 {
		b.WriteString("  Recommendations:\n")
		for _, rec := range r.Recommendations {
			fmt.Fprintf(&b, "    - %s\n", rec)
		}
	}

	return b.String()
}
