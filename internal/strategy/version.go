package strategy

import (
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"time"

	"github.com/BillBeam/adguard-agent/internal/types"
)

// VersionManager manages strategy version lifecycle.
//
// Supports the full lifecycle: DRAFT → CANARY → ACTIVE → ROLLBACK.
// Invariant: at most one ACTIVE version and at most one CANARY version at any time.
// Traffic routing uses deterministic hash-based assignment for reproducibility.

// VersionStatus models the version lifecycle state machine.
type VersionStatus string

const (
	VersionDraft    VersionStatus = "DRAFT"
	VersionCanary   VersionStatus = "CANARY"
	VersionActive   VersionStatus = "ACTIVE"
	VersionRollback VersionStatus = "ROLLBACK"
)

// Version represents one strategy version with metrics.
type Version struct {
	VersionID  string                 `json:"version_id"`
	Status     VersionStatus          `json:"status"`
	TrafficPct int                    `json:"traffic_pct"`
	CreatedAt  time.Time              `json:"created_at"`
	Metrics    *types.StrategyMetrics `json:"metrics,omitempty"`
}

// VersionManager manages strategy version lifecycle. Thread-safe.
type VersionManager struct {
	mu       sync.RWMutex
	versions map[string]*Version
	logger   *slog.Logger
}

// NewVersionManager creates a version manager.
func NewVersionManager(logger *slog.Logger) *VersionManager {
	return &VersionManager{
		versions: make(map[string]*Version),
		logger:   logger,
	}
}

// Create registers a new strategy version in DRAFT status.
func (vm *VersionManager) Create(versionID string) (*Version, error) {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	if _, exists := vm.versions[versionID]; exists {
		return nil, fmt.Errorf("version %q already exists", versionID)
	}
	v := &Version{
		VersionID: versionID,
		Status:    VersionDraft,
		CreatedAt: time.Now(),
	}
	vm.versions[versionID] = v
	vm.logger.Info("version created", slog.String("version", versionID))
	return v, nil
}

// Deploy transitions a DRAFT version to CANARY with the given traffic percentage.
// Enforces the single-CANARY invariant.
func (vm *VersionManager) Deploy(versionID string, trafficPct int) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	v, ok := vm.versions[versionID]
	if !ok {
		return fmt.Errorf("version %q not found", versionID)
	}
	if v.Status != VersionDraft {
		return fmt.Errorf("version %q is %s, must be DRAFT to deploy", versionID, v.Status)
	}
	if trafficPct < 0 || trafficPct > 100 {
		return fmt.Errorf("traffic_pct must be 0-100, got %d", trafficPct)
	}

	// Enforce single-CANARY invariant.
	for _, existing := range vm.versions {
		if existing.Status == VersionCanary {
			return fmt.Errorf("canary version %q already exists, rollback it first", existing.VersionID)
		}
	}

	v.Status = VersionCanary
	v.TrafficPct = trafficPct
	vm.logger.Info("version deployed as canary",
		slog.String("version", versionID),
		slog.Int("traffic_pct", trafficPct),
	)
	return nil
}

// Promote transitions a CANARY version to ACTIVE.
// The previous ACTIVE version (if any) becomes ROLLBACK.
func (vm *VersionManager) Promote(versionID string) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	v, ok := vm.versions[versionID]
	if !ok {
		return fmt.Errorf("version %q not found", versionID)
	}
	if v.Status != VersionCanary {
		return fmt.Errorf("version %q is %s, must be CANARY to promote", versionID, v.Status)
	}

	// Demote current ACTIVE → ROLLBACK.
	for _, existing := range vm.versions {
		if existing.Status == VersionActive {
			existing.Status = VersionRollback
			vm.logger.Info("version demoted to rollback", slog.String("version", existing.VersionID))
		}
	}

	v.Status = VersionActive
	v.TrafficPct = 100
	vm.logger.Info("version promoted to active", slog.String("version", versionID))
	return nil
}

// Rollback transitions a CANARY version back to ROLLBACK.
func (vm *VersionManager) Rollback(versionID string) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()

	v, ok := vm.versions[versionID]
	if !ok {
		return fmt.Errorf("version %q not found", versionID)
	}
	if v.Status != VersionCanary {
		return fmt.Errorf("version %q is %s, must be CANARY to rollback", versionID, v.Status)
	}

	v.Status = VersionRollback
	v.TrafficPct = 0
	vm.logger.Info("version rolled back", slog.String("version", versionID))
	return nil
}

// RouteTraffic determines which version an ad should use.
// Uses deterministic fnv32a hash: same adID always routes to the same version.
// Returns the version ID, or empty string if no ACTIVE version exists.
func (vm *VersionManager) RouteTraffic(adID string) string {
	vm.mu.RLock()
	defer vm.mu.RUnlock()

	var active, canary *Version
	for _, v := range vm.versions {
		switch v.Status {
		case VersionActive:
			active = v
		case VersionCanary:
			canary = v
		}
	}

	if active == nil {
		return ""
	}
	if canary == nil || canary.TrafficPct <= 0 {
		return active.VersionID
	}

	// Deterministic hash-based routing.
	h := fnv.New32a()
	h.Write([]byte(adID))
	bucket := int(h.Sum32() % 100)

	if bucket < canary.TrafficPct {
		return canary.VersionID
	}
	return active.VersionID
}

// GetActive returns the current ACTIVE version, if any.
func (vm *VersionManager) GetActive() (*Version, bool) {
	vm.mu.RLock()
	defer vm.mu.RUnlock()
	for _, v := range vm.versions {
		if v.Status == VersionActive {
			return v, true
		}
	}
	return nil, false
}

// GetCanary returns the current CANARY version, if any.
func (vm *VersionManager) GetCanary() (*Version, bool) {
	vm.mu.RLock()
	defer vm.mu.RUnlock()
	for _, v := range vm.versions {
		if v.Status == VersionCanary {
			return v, true
		}
	}
	return nil, false
}

// Get returns a version by ID.
func (vm *VersionManager) Get(versionID string) (*Version, bool) {
	vm.mu.RLock()
	defer vm.mu.RUnlock()
	v, ok := vm.versions[versionID]
	return v, ok
}

// UpdateMetrics updates the performance metrics for a version.
func (vm *VersionManager) UpdateMetrics(versionID string, m types.StrategyMetrics) error {
	vm.mu.Lock()
	defer vm.mu.Unlock()
	v, ok := vm.versions[versionID]
	if !ok {
		return fmt.Errorf("version %q not found", versionID)
	}
	v.Metrics = &m
	return nil
}

// List returns all versions.
func (vm *VersionManager) List() []*Version {
	vm.mu.RLock()
	defer vm.mu.RUnlock()
	result := make([]*Version, 0, len(vm.versions))
	for _, v := range vm.versions {
		result = append(result, v)
	}
	return result
}
