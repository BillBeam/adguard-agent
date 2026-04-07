package llm

import (
	"testing"
)

func defaultTestRouter() *ModelRouter {
	return NewModelRouter(DefaultRoutingConfig(), nil)
}

func TestRouteModel_PipelineOnly(t *testing.T) {
	r := defaultTestRouter()
	tests := []struct {
		pipeline, role string
		want           string
	}{
		{"fast", "", "grok-4-1-fast-non-reasoning"},
		{"standard", "", "grok-4-1-fast-reasoning"},
		{"comprehensive", "", "grok-4.20-multi-agent-0309"},
		{"appeal", "", "grok-4.20-0309-reasoning"},
	}
	for _, tt := range tests {
		got := r.RouteModel(tt.pipeline, tt.role)
		if got != tt.want {
			t.Errorf("RouteModel(%q, %q) = %q, want %q", tt.pipeline, tt.role, got, tt.want)
		}
	}
}

func TestRouteModel_PipelineWithRole(t *testing.T) {
	r := defaultTestRouter()
	// "comprehensive:adjudicator" should match the specific route.
	got := r.RouteModel("comprehensive", "adjudicator")
	want := "grok-4.20-0309-reasoning"
	if got != want {
		t.Errorf("RouteModel(comprehensive, adjudicator) = %q, want %q", got, want)
	}

	// "comprehensive:content" should fall back to pipeline-only route.
	got = r.RouteModel("comprehensive", "content")
	want = "grok-4.20-multi-agent-0309"
	if got != want {
		t.Errorf("RouteModel(comprehensive, content) = %q, want %q", got, want)
	}
}

func TestRouteModel_Default(t *testing.T) {
	r := defaultTestRouter()
	got := r.RouteModel("unknown_pipeline", "")
	want := "grok-4-1-fast-reasoning"
	if got != want {
		t.Errorf("RouteModel(unknown) = %q, want default %q", got, want)
	}
}

func TestGetFallback_Chain(t *testing.T) {
	r := defaultTestRouter()
	tests := []struct {
		model    string
		wantFB   string
		wantOK   bool
	}{
		{"grok-4.20-0309-reasoning", "grok-4-1-fast-reasoning", true},
		{"grok-4.20-multi-agent-0309", "grok-4-1-fast-reasoning", true},
		{"grok-4-1-fast-reasoning", "gpt-4o", true},
		{"grok-4-1-fast-non-reasoning", "gpt-4o-mini", true},
		{"gpt-4o", "", false},       // no fallback configured
		{"gpt-4o-mini", "", false},   // no fallback configured
		{"unknown-model", "", false}, // no fallback configured
	}
	for _, tt := range tests {
		fb, ok := r.GetFallback(tt.model)
		if ok != tt.wantOK || fb != tt.wantFB {
			t.Errorf("GetFallback(%q) = (%q, %v), want (%q, %v)",
				tt.model, fb, ok, tt.wantFB, tt.wantOK)
		}
	}
}

func TestRouteModel_CustomConfig(t *testing.T) {
	cfg := RoutingConfig{
		Routes: map[string]string{
			"fast":     "cheap-model",
			"fast:ocr": "ocr-specialist",
		},
		Default: "fallback-model",
	}
	r := NewModelRouter(cfg, nil)

	if got := r.RouteModel("fast", ""); got != "cheap-model" {
		t.Errorf("fast = %q, want cheap-model", got)
	}
	if got := r.RouteModel("fast", "ocr"); got != "ocr-specialist" {
		t.Errorf("fast:ocr = %q, want ocr-specialist", got)
	}
	if got := r.RouteModel("unknown", ""); got != "fallback-model" {
		t.Errorf("unknown = %q, want fallback-model", got)
	}
}

func TestFormatRoutingTable(t *testing.T) {
	r := defaultTestRouter()
	table := r.FormatRoutingTable()
	if table == "" {
		t.Error("FormatRoutingTable returned empty string")
	}
	// Should contain key entries.
	for _, want := range []string{"fast", "standard", "comprehensive", "adjudicator", "fallback chain"} {
		if !contains(table, want) {
			t.Errorf("routing table missing %q", want)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
