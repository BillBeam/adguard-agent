package agent

import (
	"strings"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/store"
	"github.com/BillBeam/adguard-agent/internal/types"
)

func storeWithRecords(records ...*store.ReviewRecord) *store.ReviewStore {
	rs := store.NewReviewStore(testLogger(), "")
	for _, r := range records {
		rs.Store(r)
	}
	return rs
}

func rec(adID, advID, region, category string, decision types.ReviewDecision, conf float64, violations []types.PolicyViolation) *store.ReviewRecord {
	return &store.ReviewRecord{
		ReviewResult: types.ReviewResult{
			AdID:       adID,
			Decision:   decision,
			Confidence: conf,
			Violations: violations,
		},
		AdvertiserID: advID,
		Region:       region,
		Category:     category,
	}
}

func TestMonitor_EmptyStore(t *testing.T) {
	rs := store.NewReviewStore(testLogger(), "")
	report := RunMonitor(rs, testLogger())

	if report.TotalReviews != 0 {
		t.Errorf("TotalReviews = %d, want 0", report.TotalReviews)
	}
	if len(report.Anomalies) != 0 {
		t.Errorf("expected 0 anomalies, got %d", len(report.Anomalies))
	}
	if len(report.Recommendations) != 1 || report.Recommendations[0] != "No reviews to analyze" {
		t.Errorf("unexpected recommendations: %v", report.Recommendations)
	}
}

func TestMonitor_NilStore(t *testing.T) {
	report := RunMonitor(nil, testLogger())
	if report.TotalReviews != 0 {
		t.Errorf("TotalReviews = %d, want 0", report.TotalReviews)
	}
}

func TestMonitor_RejectionSpike(t *testing.T) {
	rs := storeWithRecords(
		rec("ad1", "adv1", "US", "healthcare", types.DecisionRejected, 0.9, nil),
		rec("ad2", "adv2", "US", "healthcare", types.DecisionRejected, 0.8, nil),
		rec("ad3", "adv3", "US", "healthcare", types.DecisionRejected, 0.85, nil),
		rec("ad4", "adv4", "US", "healthcare", types.DecisionRejected, 0.9, nil),
		rec("ad5", "adv5", "US", "healthcare", types.DecisionPassed, 0.95, nil),
	)
	report := RunMonitor(rs, testLogger())

	// 4/5 = 80% > 50% threshold
	found := false
	for _, a := range report.Anomalies {
		if a.Type == "rejection_spike" {
			found = true
		}
	}
	if !found {
		t.Error("expected rejection_spike anomaly for 80% rejection rate")
	}
}

func TestMonitor_NoRejectionSpike(t *testing.T) {
	rs := storeWithRecords(
		rec("ad1", "adv1", "US", "healthcare", types.DecisionRejected, 0.9, nil),
		rec("ad2", "adv2", "US", "healthcare", types.DecisionPassed, 0.95, nil),
		rec("ad3", "adv3", "US", "healthcare", types.DecisionPassed, 0.9, nil),
	)
	report := RunMonitor(rs, testLogger())

	for _, a := range report.Anomalies {
		if a.Type == "rejection_spike" {
			t.Error("should not trigger rejection_spike for 33% rejection rate")
		}
	}
}

func TestMonitor_OverrideAnomaly(t *testing.T) {
	rs := storeWithRecords(
		rec("ad1", "adv1", "US", "hc", types.DecisionRejected, 0.9, nil),
		rec("ad2", "adv2", "US", "hc", types.DecisionRejected, 0.8, nil),
		rec("ad3", "adv3", "US", "hc", types.DecisionPassed, 0.95, nil),
	)
	// Simulate 2 verifications, both overridden.
	rs.UpdateVerification("ad1", store.VerificationOverride, types.DecisionManualReview, "test")
	rs.UpdateVerification("ad2", store.VerificationOverride, types.DecisionManualReview, "test")

	report := RunMonitor(rs, testLogger())

	found := false
	for _, a := range report.Anomalies {
		if a.Type == "confidence_drop" && strings.Contains(a.Description, "override rate") {
			found = true
		}
	}
	if !found {
		t.Error("expected confidence_drop anomaly for 100% override rate with 2+ verifications")
	}
}

func TestMonitor_AdvertiserCluster(t *testing.T) {
	rs := storeWithRecords(
		rec("ad1", "adv_bad", "US", "hc", types.DecisionRejected, 0.9, nil),
		rec("ad2", "adv_bad", "US", "hc", types.DecisionRejected, 0.85, nil),
		rec("ad3", "adv_bad", "US", "hc", types.DecisionRejected, 0.8, nil),
		rec("ad4", "adv_good", "US", "hc", types.DecisionPassed, 0.95, nil),
	)
	report := RunMonitor(rs, testLogger())

	found := false
	for _, a := range report.Anomalies {
		if a.Type == "advertiser_cluster" && strings.Contains(a.Description, "adv_bad") {
			found = true
		}
	}
	if !found {
		t.Error("expected advertiser_cluster anomaly for adv_bad with 3 rejections")
	}
}

func TestMonitor_PolicyHotspot(t *testing.T) {
	v := []types.PolicyViolation{{PolicyID: "POL_001", Severity: "critical"}}
	rs := storeWithRecords(
		rec("ad1", "adv1", "US", "hc", types.DecisionRejected, 0.9, v),
		rec("ad2", "adv2", "US", "hc", types.DecisionRejected, 0.85, v),
		rec("ad3", "adv3", "US", "hc", types.DecisionPassed, 0.95, nil),
	)
	report := RunMonitor(rs, testLogger())

	found := false
	for _, a := range report.Anomalies {
		if a.Type == "policy_hotspot" && strings.Contains(a.Description, "POL_001") {
			found = true
		}
	}
	if !found {
		t.Error("expected policy_hotspot anomaly for POL_001 with 2 hits")
	}
}

func TestMonitor_LowConfidence(t *testing.T) {
	rs := storeWithRecords(
		rec("ad1", "adv1", "US", "hc", types.DecisionPassed, 0.5, nil),
		rec("ad2", "adv2", "US", "hc", types.DecisionPassed, 0.6, nil),
		rec("ad3", "adv3", "US", "hc", types.DecisionPassed, 0.55, nil),
	)
	report := RunMonitor(rs, testLogger())

	found := false
	for _, a := range report.Anomalies {
		if a.Type == "confidence_drop" && strings.Contains(a.Description, "below 0.70") {
			found = true
		}
	}
	if !found {
		t.Error("expected confidence_drop anomaly for avg confidence 0.55")
	}
}

func TestMonitor_FormatReport(t *testing.T) {
	report := &MonitorReport{
		TotalReviews:  5,
		RejectionRate: 0.6,
		AvgConfidence: 0.85,
		OverrideRate:  0.0,
		Anomalies: []Anomaly{
			{Type: "rejection_spike", Description: "test desc", Severity: "high"},
		},
		Recommendations: []string{"test recommendation"},
	}
	output := report.FormatReport()

	if !strings.Contains(output, "Reviews: 5") {
		t.Error("FormatReport should contain total reviews")
	}
	if !strings.Contains(output, "rejection_spike") {
		t.Error("FormatReport should contain anomaly type")
	}
	if !strings.Contains(output, "test recommendation") {
		t.Error("FormatReport should contain recommendation")
	}
}
