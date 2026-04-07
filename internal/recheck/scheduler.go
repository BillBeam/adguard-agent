// Package recheck implements scheduled post-approval re-review of ads.
//
// High-risk ads that pass initial review are scheduled for a follow-up check
// after a configurable delay (default 24h). This defends against adversarial
// landing page swaps — advertisers who modify their landing page content
// after receiving initial approval.
//
// Design patterns adopted from production cron schedulers:
//   - JSONL persistence: tasks survive process restarts
//   - Missed task detection: overdue tasks on startup are executed immediately
//   - Auto-expiry: pending tasks older than 72h are discarded (prevent unbounded growth)
//   - One-pending-per-ad: prevents duplicate rechecks for the same ad
package recheck

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/BillBeam/adguard-agent/internal/store"
	"github.com/BillBeam/adguard-agent/internal/types"
)

// DefaultExpiryDuration is the maximum age for a pending task before auto-discard.
const DefaultExpiryDuration = 72 * time.Hour

// RecheckTask represents a scheduled post-approval recheck.
type RecheckTask struct {
	TaskID      string    `json:"task_id"`
	AdID        string    `json:"ad_id"`
	ScheduledAt time.Time `json:"scheduled_at"`
	Reason      string    `json:"reason"`
	Completed   bool      `json:"completed"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	Result      string    `json:"result,omitempty"` // "unchanged", "changed", "error"
}

// ReviewFunc is the signature for re-reviewing an ad.
type ReviewFunc func(ctx context.Context, ad *types.AdContent) (*types.ReviewResult, error)

// AdLookupFunc retrieves AdContent by ID.
type AdLookupFunc func(adID string) (*types.AdContent, bool)

// RecheckStats provides aggregate recheck statistics.
type RecheckStats struct {
	Total     int `json:"total"`
	Pending   int `json:"pending"`
	Completed int `json:"completed"`
	Missed    int `json:"missed"` // tasks that were overdue on startup
	Expired   int `json:"expired"`
}

// RecheckScheduler manages post-approval recheck tasks.
type RecheckScheduler struct {
	mu     sync.RWMutex
	tasks  map[string]*RecheckTask // key = task_id
	byAdID map[string]string       // ad_id → task_id (one pending per ad)
	jsonl  *store.JSONLWriter      // nil = no persistence
	logger *slog.Logger
	stats  RecheckStats
}

// NewRecheckScheduler creates a scheduler. If jsonlPath is non-empty, enables
// JSONL persistence and recovers existing tasks on startup.
// Recovered tasks are checked for missed/expired status.
func NewRecheckScheduler(logger *slog.Logger, jsonlPath string) *RecheckScheduler {
	s := &RecheckScheduler{
		tasks:  make(map[string]*RecheckTask),
		byAdID: make(map[string]string),
		logger: logger,
	}

	if jsonlPath == "" {
		return s
	}

	// Recover tasks from JSONL.
	records, skipped, err := store.ReadJSONL[RecheckTask](jsonlPath)
	if err != nil {
		logger.Error("failed to read recheck JSONL", slog.String("error", err.Error()))
	}

	now := time.Now()
	for i := range records {
		r := &records[i]
		if r.Completed {
			s.tasks[r.TaskID] = r // keep completed for stats
			continue
		}
		// Auto-expiry: discard tasks older than DefaultExpiryDuration.
		if now.Sub(r.ScheduledAt) > DefaultExpiryDuration {
			s.stats.Expired++
			logger.Info("recheck task expired",
				slog.String("task_id", r.TaskID),
				slog.String("ad_id", r.AdID),
				slog.Duration("age", now.Sub(r.ScheduledAt)),
			)
			continue
		}
		s.tasks[r.TaskID] = r
		s.byAdID[r.AdID] = r.TaskID

		// Missed task detection: overdue pending tasks.
		if r.ScheduledAt.Before(now) {
			s.stats.Missed++
			logger.Info("missed recheck task detected (will execute on next tick)",
				slog.String("task_id", r.TaskID),
				slog.String("ad_id", r.AdID),
			)
		}
	}

	if len(records) > 0 || skipped > 0 {
		logger.Info("restored recheck tasks from JSONL",
			slog.Int("count", len(s.tasks)),
			slog.Int("skipped", skipped),
			slog.Int("missed", s.stats.Missed),
			slog.Int("expired", s.stats.Expired),
		)
	}

	// Open writer.
	w, err := store.NewJSONLWriter(jsonlPath, logger)
	if err != nil {
		logger.Error("failed to open recheck JSONL writer", slog.String("error", err.Error()))
		return s
	}
	s.jsonl = w
	return s
}

// Schedule adds a recheck task. Returns error if a pending task already exists for this ad.
func (s *RecheckScheduler) Schedule(adID string, delay time.Duration, reason string) (*RecheckTask, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.byAdID[adID]; exists {
		return nil, fmt.Errorf("recheck already scheduled for ad %s", adID)
	}

	task := &RecheckTask{
		TaskID:      fmt.Sprintf("recheck_%s_%d", adID, time.Now().UnixMilli()),
		AdID:        adID,
		ScheduledAt: time.Now().Add(delay),
		Reason:      reason,
	}

	s.tasks[task.TaskID] = task
	s.byAdID[adID] = task.TaskID

	if s.jsonl != nil {
		s.jsonl.Append(task)
	}

	s.logger.Info("recheck scheduled",
		slog.String("task_id", task.TaskID),
		slog.String("ad_id", adID),
		slog.Time("scheduled_at", task.ScheduledAt),
		slog.String("reason", reason),
	)
	return task, nil
}

// DueTasks returns all pending tasks whose ScheduledAt is at or before now.
func (s *RecheckScheduler) DueTasks(now time.Time) []*RecheckTask {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var due []*RecheckTask
	for _, t := range s.tasks {
		if !t.Completed && !t.ScheduledAt.After(now) {
			due = append(due, t)
		}
	}
	return due
}

// Complete marks a task as completed with the given result.
func (s *RecheckScheduler) Complete(taskID, result string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[taskID]
	if !ok {
		return fmt.Errorf("task %q not found", taskID)
	}
	if t.Completed {
		return fmt.Errorf("task %q already completed", taskID)
	}

	t.Completed = true
	t.CompletedAt = time.Now()
	t.Result = result
	delete(s.byAdID, t.AdID)

	if s.jsonl != nil {
		s.jsonl.Append(t)
	}

	s.logger.Info("recheck completed",
		slog.String("task_id", taskID),
		slog.String("ad_id", t.AdID),
		slog.String("result", result),
	)
	return nil
}

// Run starts the background check loop. Blocks until ctx is cancelled.
// On each tick, checks for due tasks, executes reviews, and records outcomes.
func (s *RecheckScheduler) Run(ctx context.Context, interval time.Duration, reviewFn ReviewFunc, lookupFn AdLookupFunc) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.processDueTasks(ctx, reviewFn, lookupFn)
		}
	}
}

func (s *RecheckScheduler) processDueTasks(ctx context.Context, reviewFn ReviewFunc, lookupFn AdLookupFunc) {
	due := s.DueTasks(time.Now())
	for _, task := range due {
		ad, found := lookupFn(task.AdID)
		if !found {
			s.Complete(task.TaskID, "error: ad not found")
			continue
		}

		result, err := reviewFn(ctx, ad)
		if err != nil || result == nil {
			s.Complete(task.TaskID, "error: review failed")
			continue
		}

		// Compare with original: if the ad was originally PASSED but now gets
		// a different result, this indicates a potential landing page swap.
		outcome := "unchanged"
		if result.Decision != types.DecisionPassed {
			outcome = fmt.Sprintf("changed: now %s (was PASSED)", result.Decision)
			s.logger.Warn("recheck found decision change",
				slog.String("ad_id", task.AdID),
				slog.String("new_decision", string(result.Decision)),
			)
		}
		s.Complete(task.TaskID, outcome)
	}
}

// Stats returns aggregate recheck statistics.
func (s *RecheckScheduler) Stats() RecheckStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := s.stats // copy startup stats (missed, expired)
	for _, t := range s.tasks {
		stats.Total++
		if t.Completed {
			stats.Completed++
		} else {
			stats.Pending++
		}
	}
	return stats
}

// PendingCount returns the number of pending tasks.
func (s *RecheckScheduler) PendingCount() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byAdID)
}

// Flush ensures all JSONL data is written to disk. No-op if persistence is disabled.
func (s *RecheckScheduler) Flush() {
	if s.jsonl != nil {
		s.jsonl.Flush()
	}
}
