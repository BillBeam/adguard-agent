package agent

import (
	"log/slog"
	"os"
	"sync"
	"testing"

	"github.com/BillBeam/adguard-agent/internal/types"
)

func testHookLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// recordingHook records calls for test verification.
type recordingHook struct {
	mu    sync.Mutex
	calls []string
}

func (h *recordingHook) PostReview(result types.ReviewResult, advertiserID, region, category, pipeline string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, result.AdID)
}

// panickingHook always panics.
type panickingHook struct{}

func (h *panickingHook) PostReview(_ types.ReviewResult, _, _, _, _ string) {
	panic("intentional panic for testing")
}

func TestHookChain_OrderedExecution(t *testing.T) {
	hook1 := &recordingHook{}
	hook2 := &recordingHook{}

	chain := NewHookChain(testHookLogger())
	chain.Add(hook1).Add(hook2)

	result := types.ReviewResult{AdID: "ad_001", Decision: types.DecisionPassed}
	chain.PostReview(result, "adv_001", "US", "ecommerce", "standard")

	if len(hook1.calls) != 1 || hook1.calls[0] != "ad_001" {
		t.Errorf("hook1: expected [ad_001], got %v", hook1.calls)
	}
	if len(hook2.calls) != 1 || hook2.calls[0] != "ad_001" {
		t.Errorf("hook2: expected [ad_001], got %v", hook2.calls)
	}
}

func TestHookChain_PanicRecovery(t *testing.T) {
	hook1 := &recordingHook{}
	hook3 := &recordingHook{}

	chain := NewHookChain(testHookLogger())
	chain.Add(hook1).Add(&panickingHook{}).Add(hook3)

	result := types.ReviewResult{AdID: "ad_002"}
	// Should not panic even though hook2 panics.
	chain.PostReview(result, "adv_001", "US", "ecommerce", "standard")

	if len(hook1.calls) != 1 {
		t.Errorf("hook1 should have been called, got %d calls", len(hook1.calls))
	}
	if len(hook3.calls) != 1 {
		t.Errorf("hook3 should have been called despite hook2 panic, got %d calls", len(hook3.calls))
	}
}

func TestHookChain_Empty(t *testing.T) {
	chain := NewHookChain(testHookLogger())
	// Should not panic on empty chain.
	chain.PostReview(types.ReviewResult{AdID: "ad_003"}, "adv", "US", "ecommerce", "fast")
}
