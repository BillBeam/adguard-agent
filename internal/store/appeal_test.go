package store

import (
	"testing"
)

func TestAppealStore_Submit(t *testing.T) {
	rm := NewReputationManager(testLogger())
	as := NewAppealStore(testLogger(), rm)

	appeal, err := as.Submit("ad_001", "adv_001", "We believe this ad complies")
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	if appeal.Status != AppealSubmitted {
		t.Errorf("expected SUBMITTED, got %s", appeal.Status)
	}
	if appeal.AdID != "ad_001" {
		t.Errorf("expected ad_001, got %s", appeal.AdID)
	}
}

func TestAppealStore_DuplicateAppealBlocked(t *testing.T) {
	rm := NewReputationManager(testLogger())
	as := NewAppealStore(testLogger(), rm)

	as.Submit("ad_001", "adv_001", "first appeal")
	_, err := as.Submit("ad_001", "adv_001", "second appeal")
	if err == nil {
		t.Error("duplicate appeal should be blocked")
	}
}

func TestAppealStore_StatusTransitions(t *testing.T) {
	rm := NewReputationManager(testLogger())
	as := NewAppealStore(testLogger(), rm)

	appeal, _ := as.Submit("ad_001", "adv_001", "reason")

	if err := as.SetReviewing(appeal.AppealID); err != nil {
		t.Fatalf("SetReviewing failed: %v", err)
	}
	got, _ := as.Get(appeal.AppealID)
	if got.Status != AppealReviewing {
		t.Errorf("expected REVIEWING, got %s", got.Status)
	}

	if err := as.Resolve(appeal.AppealID, AppealUpheld, "violations confirmed"); err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	got, _ = as.Get(appeal.AppealID)
	if got.Status != AppealResolved || got.Outcome != AppealUpheld {
		t.Errorf("expected RESOLVED/UPHELD, got %s/%s", got.Status, got.Outcome)
	}
}

func TestAppealStore_ResolveOverturned(t *testing.T) {
	rm := NewReputationManager(testLogger())
	as := NewAppealStore(testLogger(), rm)

	appeal, _ := as.Submit("ad_001", "adv_001", "reason")
	as.SetReviewing(appeal.AppealID)
	as.Resolve(appeal.AppealID, AppealOverturned, "ad is actually compliant")

	got, _ := as.Get(appeal.AppealID)
	if got.Outcome != AppealOverturned {
		t.Errorf("expected OVERTURNED, got %s", got.Outcome)
	}
}

func TestAppealStore_GetByAdID(t *testing.T) {
	rm := NewReputationManager(testLogger())
	as := NewAppealStore(testLogger(), rm)

	as.Submit("ad_001", "adv_001", "reason")

	appeal, ok := as.GetByAdID("ad_001")
	if !ok || appeal.AdID != "ad_001" {
		t.Error("GetByAdID should find the appeal")
	}

	_, ok = as.GetByAdID("ad_999")
	if ok {
		t.Error("GetByAdID should return false for non-existent ad")
	}
}

func TestAppealStore_Stats(t *testing.T) {
	rm := NewReputationManager(testLogger())
	as := NewAppealStore(testLogger(), rm)

	a1, _ := as.Submit("ad_001", "adv_001", "reason")
	as.Resolve(a1.AppealID, AppealUpheld, "confirmed")

	a2, _ := as.Submit("ad_002", "adv_002", "reason")
	as.Resolve(a2.AppealID, AppealOverturned, "reversed")

	stats := as.Stats()
	if stats.Total != 2 {
		t.Errorf("expected 2 total, got %d", stats.Total)
	}
	if stats.ByOutcome[AppealUpheld] != 1 || stats.ByOutcome[AppealOverturned] != 1 {
		t.Errorf("outcome counts wrong: %v", stats.ByOutcome)
	}
}

// --- ReputationManager tests ---

func TestReputationManager_UpdateOnAppeal_Overturned(t *testing.T) {
	rm := NewReputationManager(testLogger())
	rep := rm.Get("adv_001")
	initial := rep.TrustScore

	rm.UpdateOnAppeal("adv_001", AppealOverturned)

	rep = rm.Get("adv_001")
	if rep.TrustScore <= initial {
		t.Errorf("OVERTURNED should increase trust: %.2f → %.2f", initial, rep.TrustScore)
	}
}

func TestReputationManager_UpdateOnAppeal_Upheld(t *testing.T) {
	rm := NewReputationManager(testLogger())
	rep := rm.Get("adv_001")
	initial := rep.TrustScore

	rm.UpdateOnAppeal("adv_001", AppealUpheld)

	rep = rm.Get("adv_001")
	if rep.TrustScore >= initial {
		t.Errorf("UPHELD should decrease trust: %.2f → %.2f", initial, rep.TrustScore)
	}
	if rep.HistoricalViolations != 1 {
		t.Errorf("UPHELD should increment violations, got %d", rep.HistoricalViolations)
	}
}

func TestReputationManager_RiskCategory(t *testing.T) {
	rm := NewReputationManager(testLogger())

	// Default is standard (0.5).
	rep := rm.Get("adv_001")
	if rep.RiskCategory != "standard" {
		t.Errorf("default should be standard, got %s", rep.RiskCategory)
	}

	// Multiple upheld appeals → probation.
	for i := 0; i < 6; i++ {
		rm.UpdateOnAppeal("adv_001", AppealUpheld)
	}
	rep = rm.Get("adv_001")
	if rep.RiskCategory != "probation" {
		t.Errorf("after many upheld appeals should be probation, got %s (trust=%.2f)", rep.RiskCategory, rep.TrustScore)
	}
}

func TestReputationManager_RecordViolation(t *testing.T) {
	rm := NewReputationManager(testLogger())
	rm.Get("adv_001") // initialize
	rm.RecordViolation("adv_001")

	rep := rm.Get("adv_001")
	if rep.HistoricalViolations != 1 {
		t.Errorf("expected 1 violation, got %d", rep.HistoricalViolations)
	}
}
