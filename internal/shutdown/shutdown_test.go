package shutdown

import (
	"sync"
	"testing"
)

func TestRegisterCleanup_ExecutesInOrder(t *testing.T) {
	Reset()
	var order []int

	RegisterCleanup(func() { order = append(order, 1) })
	RegisterCleanup(func() { order = append(order, 2) })
	RegisterCleanup(func() { order = append(order, 3) })

	runCleanup(nil) // nil logger won't be used unless panic

	if len(order) != 3 {
		t.Fatalf("expected 3 cleanup calls, got %d", len(order))
	}
	for i, v := range order {
		if v != i+1 {
			t.Errorf("cleanup[%d] = %d, want %d", i, v, i+1)
		}
	}
}

func TestRegisterCleanup_PanicRecovery(t *testing.T) {
	Reset()
	var called bool

	RegisterCleanup(func() { panic("boom") })
	RegisterCleanup(func() { called = true })

	// Should not panic, and second cleanup should still run.
	runCleanup(nil)

	if !called {
		t.Error("second cleanup was not called after first panicked")
	}
}

func TestIsShuttingDown_InitiallyFalse(t *testing.T) {
	Reset()
	if IsShuttingDown() {
		t.Error("expected IsShuttingDown() = false initially")
	}
}

func TestIsShuttingDown_TrueAfterSet(t *testing.T) {
	Reset()
	inProgress.Store(true)
	if !IsShuttingDown() {
		t.Error("expected IsShuttingDown() = true after setting flag")
	}
}

func TestReset_ClearsState(t *testing.T) {
	inProgress.Store(true)
	RegisterCleanup(func() {})
	Reset()

	if IsShuttingDown() {
		t.Error("expected IsShuttingDown() = false after Reset")
	}
	cleanupMu.Lock()
	n := len(cleanupFuncs)
	cleanupMu.Unlock()
	if n != 0 {
		t.Errorf("expected 0 cleanup funcs after Reset, got %d", n)
	}
}

func TestConcurrentRegister(t *testing.T) {
	Reset()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			RegisterCleanup(func() {})
		}()
	}
	wg.Wait()

	cleanupMu.Lock()
	n := len(cleanupFuncs)
	cleanupMu.Unlock()
	if n != 100 {
		t.Errorf("expected 100 cleanup funcs, got %d", n)
	}
}
