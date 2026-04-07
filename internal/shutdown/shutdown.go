// Package shutdown provides graceful shutdown handling for the AdGuard Agent.
//
// On SIGINT/SIGTERM, the shutdown sequence:
//  1. Sets shutting-down flag (checked by agent loops via IsShuttingDown)
//  2. Waits for in-flight reviews to complete (bounded by failsafe timeout)
//  3. Executes registered cleanup functions (JSONL flush, etc.) in order
//  4. Exits the process
//
// A failsafe timer ensures the process exits even if cleanup hangs.
package shutdown

import (
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const failsafeTimeout = 5 * time.Second

var (
	inProgress   atomic.Bool
	cleanupMu    sync.Mutex
	cleanupFuncs []func()
	setupOnce    sync.Once
)

// Setup registers signal handlers for SIGINT and SIGTERM.
// The provided WaitGroup should track in-flight reviews — shutdown waits
// for it to complete before running cleanup functions.
func Setup(wg *sync.WaitGroup, logger *slog.Logger) {
	setupOnce.Do(func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

		go func() {
			sig := <-sigCh
			inProgress.Store(true)
			logger.Info("shutdown signal received, waiting for in-flight reviews",
				slog.String("signal", sig.String()),
			)

			// Failsafe: force exit if cleanup hangs.
			time.AfterFunc(failsafeTimeout, func() {
				logger.Error("shutdown failsafe triggered, forcing exit")
				os.Exit(1)
			})

			// Wait for in-flight work to finish.
			wg.Wait()

			// Execute cleanup functions in registration order.
			runCleanup(logger)

			logger.Info("shutdown complete")
			os.Exit(0)
		}()
	})
}

// RegisterCleanup adds a function to be called during graceful shutdown.
// Functions execute in the order they are registered.
// Typical use: JSONL writers register their Flush method here.
func RegisterCleanup(fn func()) {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	cleanupFuncs = append(cleanupFuncs, fn)
}

// IsShuttingDown returns true after a shutdown signal has been received.
// Agent loops should check this to abort early.
func IsShuttingDown() bool {
	return inProgress.Load()
}

// runCleanup executes all registered cleanup functions, recovering from panics.
func runCleanup(logger *slog.Logger) {
	cleanupMu.Lock()
	fns := make([]func(), len(cleanupFuncs))
	copy(fns, cleanupFuncs)
	cleanupMu.Unlock()

	for i, fn := range fns {
		func() {
			defer func() {
				if r := recover(); r != nil && logger != nil {
					logger.Error("panic in cleanup function",
						slog.Int("index", i),
						slog.Any("panic", r),
					)
				}
			}()
			fn()
		}()
	}
}

// Reset clears all state for testing purposes.
func Reset() {
	inProgress.Store(false)
	cleanupMu.Lock()
	cleanupFuncs = nil
	cleanupMu.Unlock()
	setupOnce = sync.Once{}
}
