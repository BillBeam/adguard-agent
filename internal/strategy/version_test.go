package strategy

import (
	"fmt"
	"log/slog"
	"os"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/types"
)

func versionLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestVersionManager_CreateDraft(t *testing.T) {
	vm := NewVersionManager(versionLogger())
	v, err := vm.Create("v1.0")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if v.Status != VersionDraft {
		t.Errorf("expected DRAFT, got %s", v.Status)
	}
}

func TestVersionManager_CreateDuplicate(t *testing.T) {
	vm := NewVersionManager(versionLogger())
	vm.Create("v1.0")
	_, err := vm.Create("v1.0")
	if err == nil {
		t.Error("duplicate creation should fail")
	}
}

func TestVersionManager_DeployCanary(t *testing.T) {
	vm := NewVersionManager(versionLogger())
	vm.Create("v1.0")
	if err := vm.Deploy("v1.0", 10); err != nil {
		t.Fatalf("Deploy failed: %v", err)
	}
	v, ok := vm.GetCanary()
	if !ok || v.VersionID != "v1.0" {
		t.Error("expected v1.0 as canary")
	}
	if v.TrafficPct != 10 {
		t.Errorf("expected 10%% traffic, got %d", v.TrafficPct)
	}
}

func TestVersionManager_PromoteCanaryToActive(t *testing.T) {
	vm := NewVersionManager(versionLogger())

	// v1.0 as initial ACTIVE.
	vm.Create("v1.0")
	vm.Deploy("v1.0", 0)
	vm.Promote("v1.0")

	// v2.0 as canary then promote.
	vm.Create("v2.0")
	vm.Deploy("v2.0", 10)
	if err := vm.Promote("v2.0"); err != nil {
		t.Fatalf("Promote failed: %v", err)
	}

	// v2.0 should be ACTIVE, v1.0 should be ROLLBACK.
	active, ok := vm.GetActive()
	if !ok || active.VersionID != "v2.0" {
		t.Error("expected v2.0 as active")
	}
	v1, _ := vm.Get("v1.0")
	if v1.Status != VersionRollback {
		t.Errorf("v1.0 should be ROLLBACK, got %s", v1.Status)
	}
}

func TestVersionManager_RollbackCanary(t *testing.T) {
	vm := NewVersionManager(versionLogger())
	vm.Create("v1.0")
	vm.Deploy("v1.0", 10)
	if err := vm.Rollback("v1.0"); err != nil {
		t.Fatalf("Rollback failed: %v", err)
	}
	v, _ := vm.Get("v1.0")
	if v.Status != VersionRollback {
		t.Errorf("expected ROLLBACK, got %s", v.Status)
	}
}

func TestVersionManager_SingleActiveInvariant(t *testing.T) {
	vm := NewVersionManager(versionLogger())
	vm.Create("v1.0")
	vm.Deploy("v1.0", 0)
	vm.Promote("v1.0")

	vm.Create("v2.0")
	vm.Deploy("v2.0", 0)
	vm.Promote("v2.0")

	// Count ACTIVE versions.
	activeCount := 0
	for _, v := range vm.List() {
		if v.Status == VersionActive {
			activeCount++
		}
	}
	if activeCount != 1 {
		t.Errorf("expected exactly 1 active, got %d", activeCount)
	}
}

func TestVersionManager_SingleCanaryInvariant(t *testing.T) {
	vm := NewVersionManager(versionLogger())
	vm.Create("v1.0")
	vm.Deploy("v1.0", 10)

	vm.Create("v2.0")
	err := vm.Deploy("v2.0", 20)
	if err == nil {
		t.Error("deploying second canary should fail")
	}
}

func TestVersionManager_InvalidTransitions(t *testing.T) {
	vm := NewVersionManager(versionLogger())
	vm.Create("v1.0")

	// Can't promote DRAFT directly.
	if err := vm.Promote("v1.0"); err == nil {
		t.Error("promoting DRAFT should fail")
	}

	// Can't rollback DRAFT.
	if err := vm.Rollback("v1.0"); err == nil {
		t.Error("rolling back DRAFT should fail")
	}
}

func TestVersionManager_RouteTraffic_Deterministic(t *testing.T) {
	vm := NewVersionManager(versionLogger())
	vm.Create("v1.0")
	vm.Deploy("v1.0", 0)
	vm.Promote("v1.0")

	vm.Create("v2.0")
	vm.Deploy("v2.0", 50)

	// Same adID should always route to the same version.
	first := vm.RouteTraffic("ad_001")
	for i := 0; i < 100; i++ {
		if got := vm.RouteTraffic("ad_001"); got != first {
			t.Fatalf("non-deterministic routing: %s vs %s", first, got)
		}
	}
}

func TestVersionManager_RouteTraffic_Distribution(t *testing.T) {
	vm := NewVersionManager(versionLogger())
	vm.Create("v1.0")
	vm.Deploy("v1.0", 0)
	vm.Promote("v1.0")

	vm.Create("v2.0")
	vm.Deploy("v2.0", 30) // 30% canary

	canaryCount := 0
	total := 1000
	for i := 0; i < total; i++ {
		adID := fmt.Sprintf("ad_%04d", i)
		if vm.RouteTraffic(adID) == "v2.0" {
			canaryCount++
		}
	}

	// Should be roughly 30% ± 10%.
	pct := float64(canaryCount) / float64(total) * 100
	if pct < 20 || pct > 40 {
		t.Errorf("canary traffic %.1f%% is outside expected 20-40%% range", pct)
	}
}

func TestVersionManager_RouteTraffic_NoActive(t *testing.T) {
	vm := NewVersionManager(versionLogger())
	result := vm.RouteTraffic("ad_001")
	if result != "" {
		t.Errorf("expected empty string with no active version, got %q", result)
	}
}

func TestVersionManager_UpdateMetrics(t *testing.T) {
	vm := NewVersionManager(versionLogger())
	vm.Create("v1.0")
	err := vm.UpdateMetrics("v1.0", types.StrategyMetrics{Accuracy: 0.95, Recall: 0.90, FalsePositiveRate: 0.05})
	if err != nil {
		t.Fatalf("UpdateMetrics failed: %v", err)
	}
	v, _ := vm.Get("v1.0")
	if v.Metrics == nil || v.Metrics.Accuracy != 0.95 {
		t.Error("metrics not updated correctly")
	}
}
