package tool

import (
	"encoding/json"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/types"
)

func TestPolicyKBLookup_ByPolicyID(t *testing.T) {
	matrix := loadMatrix(t)
	kb := NewPolicyKBLookup(matrix, testLogger())

	result, err := kb.Execute(nil, json.RawMessage(`{"policy_id":"POL_001"}`))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	var policies []types.Policy
	if err := json.Unmarshal([]byte(result), &policies); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(policies) != 1 {
		t.Fatalf("expected 1 policy, got %d", len(policies))
	}
	if policies[0].ID != "POL_001" {
		t.Errorf("ID = %q, want POL_001", policies[0].ID)
	}
	if policies[0].RuleText == "" {
		t.Error("RuleText should not be empty")
	}
}

func TestPolicyKBLookup_ByRegion(t *testing.T) {
	matrix := loadMatrix(t)
	kb := NewPolicyKBLookup(matrix, testLogger())

	result, err := kb.Execute(nil, json.RawMessage(`{"region":"MENA_SA"}`))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	var policies []types.Policy
	if err := json.Unmarshal([]byte(result), &policies); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(policies) == 0 {
		t.Fatal("expected MENA_SA policies (including Global), got 0")
	}
	// 应包含 Global 策略和 MENA 前缀匹配的策略。
	hasGlobal := false
	hasMENA := false
	for _, p := range policies {
		if p.Region == "Global" {
			hasGlobal = true
		}
		if p.Region == "MENA_SA" || p.Region == "MENA" {
			hasMENA = true
		}
	}
	if !hasGlobal {
		t.Error("should include Global policies")
	}
	if !hasMENA {
		t.Error("should include MENA/MENA_SA policies")
	}
}

func TestPolicyKBLookup_ByCategory(t *testing.T) {
	matrix := loadMatrix(t)
	kb := NewPolicyKBLookup(matrix, testLogger())

	result, err := kb.Execute(nil, json.RawMessage(`{"category":"alcohol"}`))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	var policies []types.Policy
	if err := json.Unmarshal([]byte(result), &policies); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(policies) == 0 {
		t.Fatal("expected alcohol policies, got 0")
	}
	for _, p := range policies {
		if p.Category != "alcohol" && p.Category != "all" {
			t.Errorf("unexpected category %q in alcohol query", p.Category)
		}
	}
}

func TestPolicyKBLookup_ByKeyword(t *testing.T) {
	matrix := loadMatrix(t)
	kb := NewPolicyKBLookup(matrix, testLogger())

	result, err := kb.Execute(nil, json.RawMessage(`{"keyword":"gambling"}`))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	var policies []types.Policy
	if err := json.Unmarshal([]byte(result), &policies); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(policies) == 0 {
		t.Fatal("expected policies containing 'gambling', got 0")
	}
}

func TestPolicyKBLookup_CombinedFilters(t *testing.T) {
	matrix := loadMatrix(t)
	kb := NewPolicyKBLookup(matrix, testLogger())

	result, err := kb.Execute(nil, json.RawMessage(`{"region":"US","category":"healthcare"}`))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	var policies []types.Policy
	if err := json.Unmarshal([]byte(result), &policies); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(policies) == 0 {
		t.Fatal("expected US+healthcare policies, got 0")
	}
	for _, p := range policies {
		if p.Category != "healthcare" && p.Category != "all" {
			t.Errorf("unexpected category %q", p.Category)
		}
	}
}

func TestPolicyKBLookup_NoFilters(t *testing.T) {
	matrix := loadMatrix(t)
	kb := NewPolicyKBLookup(matrix, testLogger())

	err := kb.ValidateInput(json.RawMessage(`{}`))
	if err == nil {
		t.Error("expected error for empty query, got nil")
	}
}

func TestPolicyKBLookup_NoResults(t *testing.T) {
	matrix := loadMatrix(t)
	kb := NewPolicyKBLookup(matrix, testLogger())

	result, err := kb.Execute(nil, json.RawMessage(`{"keyword":"xyznonexistent"}`))
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	var policies []types.Policy
	if err := json.Unmarshal([]byte(result), &policies); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(policies) != 0 {
		t.Errorf("expected 0 results, got %d", len(policies))
	}
}
