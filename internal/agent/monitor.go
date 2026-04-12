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
	TotalReviews    int
	RejectionRate   float64
	AvgConfidence   float64
	OverrideRate    float64
	Anomalies       []Anomaly
	Recommendations []string
}

// Anomaly represents a detected operational issue.
type Anomaly struct {
	Type        string // "rejection_spike", "confidence_drop", "advertiser_cluster", "policy_hotspot"
	Description string
	Severity    string // "high", "medium", "low"
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

	if len(r.Anomalies) > 0 {
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
