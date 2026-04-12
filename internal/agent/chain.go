package agent

import (
	"crypto/rand"
	"fmt"
	"strings"
	"time"
)

// Query Chain Tracking — data foundation of the Attribution (归因) stage.
//
//   - chainId: UUID identifying the entire review chain (stays constant)
//   - depth: nesting level (parent=0, specialist agents=1, adjudicator=1)
//
// Allows full reconstruction of "why was this ad rejected":
//   Chain abc123
//   ├─ depth=0: ReviewEngine → comprehensive pipeline
//   ├─ depth=1: ContentAgent → analyze_content → signals → match_policies → violations
//   ├─ depth=1: PolicyAgent → match_policies → check_region_compliance → violations
//   ├─ depth=1: RegionAgent → check_region_compliance → check_landing_page → issues
//   └─ depth=1: AdjudicatorAgent → conflict detection → weighted → REJECTED (0.92)

// QueryChain tracks the execution chain across parent/child agents.
type QueryChain struct {
	ChainID string `json:"chain_id"` // UUID, constant across all agents in one review
	Depth   int    `json:"depth"`    // 0=orchestrator, 1=specialist/adjudicator
}

// NewQueryChain creates a root-level chain for a new review.
func NewQueryChain() *QueryChain {
	return &QueryChain{
		ChainID: generateChainID(),
		Depth:   0,
	}
}

// Child creates a child chain at depth+1, same chainID.
func (qc *QueryChain) Child() *QueryChain {
	return &QueryChain{
		ChainID: qc.ChainID,
		Depth:   qc.Depth + 1,
	}
}

// ChainEntry records one agent's participation in the chain.
type ChainEntry struct {
	ChainID   string        `json:"chain_id"`
	Depth     int           `json:"depth"`
	AgentRole string        `json:"agent_role"` // "content", "policy", "region", "adjudicator", "single"
	Decision  string        `json:"decision"`
	Confidence float64      `json:"confidence"`
	ToolsCalled []string    `json:"tools_called"`
	Duration  time.Duration `json:"duration"`
	Trace     []string      `json:"trace,omitempty"`
}

// ChainLog collects all entries for one review chain.
type ChainLog struct {
	ChainID string       `json:"chain_id"`
	Entries []ChainEntry `json:"entries"`
}

// NewChainLog creates an empty chain log.
func NewChainLog(chainID string) *ChainLog {
	return &ChainLog{
		ChainID: chainID,
		Entries: make([]ChainEntry, 0, 6),
	}
}

// Add appends an entry to the chain log.
func (cl *ChainLog) Add(entry ChainEntry) {
	cl.Entries = append(cl.Entries, entry)
}

// Format returns a human-readable chain visualization.
func (cl *ChainLog) Format() string {
	if len(cl.Entries) == 0 {
		return "(empty chain)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Chain: %s\n", cl.ChainID[:8])
	for _, e := range cl.Entries {
		indent := strings.Repeat("  ", e.Depth)
		fmt.Fprintf(&b, "%s├─ [%s] %s conf=%.2f tools=%v",
			indent, e.AgentRole, e.Decision, e.Confidence, e.ToolsCalled)
		if e.Duration > 0 {
			fmt.Fprintf(&b, " (%s)", e.Duration.Round(time.Millisecond))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func generateChainID() string {
	var uuid [16]byte
	_, _ = rand.Read(uuid[:])
	uuid[6] = (uuid[6] & 0x0f) | 0x40
	uuid[8] = (uuid[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:])
}
