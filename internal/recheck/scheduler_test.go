package recheck

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BillBeam/adguard-agent/internal/types"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestSchedule(t *testing.T) {
	s := NewRecheckScheduler(testLogger(), "")
	task, err := s.Schedule("ad_001", 24*time.Hour, "test")
	if err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if task.AdID != "ad_001" || task.Completed {
		t.Errorf("unexpected task: %+v", task)
	}
	if s.PendingCount() != 1 {
		t.Errorf("PendingCount = %d, want 1", s.PendingCount())
	}
}

func TestSchedule_DuplicateBlocked(t *testing.T) {
	s := NewRecheckScheduler(testLogger(), "")
	s.Schedule("ad_001", 24*time.Hour, "test")
	_, err := s.Schedule("ad_001", 24*time.Hour, "test again")
	if err == nil {
		t.Error("expected error on duplicate schedule")
	}
}

func TestDueTasks(t *testing.T) {
	s := NewRecheckScheduler(testLogger(), "")
	// Task 1: due now (0 delay).
	s.Schedule("ad_001", 0, "immediate")
	// Task 2: due in 24h (not yet).
	s.Schedule("ad_002", 24*time.Hour, "future")

	due := s.DueTasks(time.Now())
	if len(due) != 1 {
		t.Fatalf("DueTasks = %d, want 1", len(due))
	}
	if due[0].AdID != "ad_001" {
		t.Errorf("due task AdID = %s, want ad_001", due[0].AdID)
	}
}

func TestComplete(t *testing.T) {
	s := NewRecheckScheduler(testLogger(), "")
	task, _ := s.Schedule("ad_001", 0, "test")
	err := s.Complete(task.TaskID, "unchanged")
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if s.PendingCount() != 0 {
		t.Errorf("PendingCount = %d, want 0 after complete", s.PendingCount())
	}

	// Can re-schedule same ad after completion.
	_, err = s.Schedule("ad_001", 0, "again")
	if err != nil {
		t.Errorf("re-schedule after complete should succeed: %v", err)
	}
}

func TestComplete_AlreadyCompleted(t *testing.T) {
	s := NewRecheckScheduler(testLogger(), "")
	task, _ := s.Schedule("ad_001", 0, "test")
	s.Complete(task.TaskID, "unchanged")
	err := s.Complete(task.TaskID, "again")
	if err == nil {
		t.Error("expected error on double complete")
	}
}

func TestRun_ExecutesAndCompletes(t *testing.T) {
	s := NewRecheckScheduler(testLogger(), "")
	s.Schedule("ad_001", 0, "test") // due immediately

	reviewed := make(chan string, 1)
	reviewFn := func(_ context.Context, ad *types.AdContent) (*types.ReviewResult, error) {
		reviewed <- ad.ID
		return &types.ReviewResult{AdID: ad.ID, Decision: types.DecisionPassed, Confidence: 0.9}, nil
	}
	lookupFn := func(adID string) (*types.AdContent, bool) {
		return &types.AdContent{ID: adID}, true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go s.Run(ctx, 50*time.Millisecond, reviewFn, lookupFn)

	select {
	case id := <-reviewed:
		if id != "ad_001" {
			t.Errorf("reviewed ad = %s, want ad_001", id)
		}
	case <-ctx.Done():
		t.Fatal("timeout: task was not executed")
	}

	// Wait a bit for Complete to execute.
	time.Sleep(100 * time.Millisecond)
	if s.PendingCount() != 0 {
		t.Errorf("PendingCount = %d, want 0 after run", s.PendingCount())
	}
}

func TestRun_ContextCancel(t *testing.T) {
	s := NewRecheckScheduler(testLogger(), "")
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		s.Run(ctx, time.Second, nil, nil)
		close(done)
	}()

	cancel()
	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after context cancel")
	}
}

func TestStats(t *testing.T) {
	s := NewRecheckScheduler(testLogger(), "")
	s.Schedule("ad_001", 0, "test")
	s.Schedule("ad_002", 24*time.Hour, "future")

	task1 := s.DueTasks(time.Now())[0]
	s.Complete(task1.TaskID, "unchanged")

	stats := s.Stats()
	if stats.Total != 2 {
		t.Errorf("Total = %d, want 2", stats.Total)
	}
	if stats.Pending != 1 {
		t.Errorf("Pending = %d, want 1", stats.Pending)
	}
	if stats.Completed != 1 {
		t.Errorf("Completed = %d, want 1", stats.Completed)
	}
}

func TestJSONLPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rechecks.jsonl")

	// Write tasks.
	s1 := NewRecheckScheduler(testLogger(), path)
	s1.Schedule("ad_001", 24*time.Hour, "test1")
	s1.Schedule("ad_002", 24*time.Hour, "test2")
	s1.Flush()

	// Recover from same file.
	s2 := NewRecheckScheduler(testLogger(), path)
	if s2.PendingCount() != 2 {
		t.Errorf("recovered PendingCount = %d, want 2", s2.PendingCount())
	}
}

func TestMissedTaskDetection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rechecks.jsonl")

	// Write a task that's already overdue.
	s1 := NewRecheckScheduler(testLogger(), path)
	s1.mu.Lock()
	task := &RecheckTask{
		TaskID:      "recheck_old",
		AdID:        "ad_old",
		ScheduledAt: time.Now().Add(-1 * time.Hour), // 1 hour ago
		Reason:      "overdue test",
	}
	s1.tasks[task.TaskID] = task
	s1.byAdID[task.AdID] = task.TaskID
	if s1.jsonl != nil {
		s1.jsonl.Append(task)
	}
	s1.mu.Unlock()
	s1.Flush()

	// Recover — should detect as missed.
	s2 := NewRecheckScheduler(testLogger(), path)
	stats := s2.Stats()
	if stats.Missed != 1 {
		t.Errorf("Missed = %d, want 1", stats.Missed)
	}
	if s2.PendingCount() != 1 {
		t.Errorf("PendingCount = %d, want 1 (missed but still pending)", s2.PendingCount())
	}
}

func TestRecheckHook_SchedulesHighRiskPassed(t *testing.T) {
	s := NewRecheckScheduler(testLogger(), "")
	riskLookup := func(category string) types.RiskLevel {
		if category == "healthcare" {
			return types.RiskCritical
		}
		return types.RiskLow
	}
	hook := NewRecheckHook(s, riskLookup, 24*time.Hour)

	hook.PostReview(
		types.ReviewResult{AdID: "ad_health", Decision: types.DecisionPassed},
		"adv_001", "US", "healthcare", "comprehensive",
	)

	if s.PendingCount() != 1 {
		t.Errorf("PendingCount = %d, want 1 (high-risk PASSED should schedule)", s.PendingCount())
	}
}

func TestRecheckHook_SkipsLowRisk(t *testing.T) {
	s := NewRecheckScheduler(testLogger(), "")
	riskLookup := func(string) types.RiskLevel { return types.RiskLow }
	hook := NewRecheckHook(s, riskLookup, 24*time.Hour)

	hook.PostReview(
		types.ReviewResult{AdID: "ad_safe", Decision: types.DecisionPassed},
		"adv_001", "US", "ecommerce", "fast",
	)

	if s.PendingCount() != 0 {
		t.Errorf("PendingCount = %d, want 0 (low-risk should not schedule)", s.PendingCount())
	}
}

func TestRecheckHook_SkipsRejected(t *testing.T) {
	s := NewRecheckScheduler(testLogger(), "")
	riskLookup := func(string) types.RiskLevel { return types.RiskCritical }
	hook := NewRecheckHook(s, riskLookup, 24*time.Hour)

	hook.PostReview(
		types.ReviewResult{AdID: "ad_bad", Decision: types.DecisionRejected},
		"adv_001", "US", "healthcare", "comprehensive",
	)

	if s.PendingCount() != 0 {
		t.Errorf("PendingCount = %d, want 0 (REJECTED should not schedule)", s.PendingCount())
	}
}
