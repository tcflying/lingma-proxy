package toolemulation

import (
	"strings"
	"testing"
)

// TestB2Force1005ConstraintsCompose: a forced tool selection and the
// single-call rule must both reach the prompt when the client sent both.
// forceConstraint used to return the forced-tool line alone, silently dropping
// the parallel_tool_calls=false instruction.
func TestB2Force1005ConstraintsCompose(t *testing.T) {
	tools := []ToolDef{{Name: "Read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}}}}
	serial := false
	prompt := InjectTooling("", tools, ToolChoice{Mode: "tool", Name: "Read"}, &serial)
	if !strings.Contains(prompt, `"Read"`) {
		t.Fatalf("forced tool line missing: %q", prompt)
	}
	if !strings.Contains(prompt, "only one tool at a time") {
		t.Fatalf("single-call rule missing alongside a forced tool: %q", prompt)
	}

	anyPrompt := InjectTooling("", tools, ToolChoice{Mode: "any"}, &serial)
	if !strings.Contains(anyPrompt, "at least one") || !strings.Contains(anyPrompt, "only one tool at a time") {
		t.Fatalf("required mode must keep the single-call rule too: %q", anyPrompt)
	}

	parallel := true
	if got := forceConstraint(ToolChoice{Mode: "any"}, &parallel); strings.Contains(got, "only one tool at a time") {
		t.Fatalf("parallel allowed must not add the single-call rule, got %q", got)
	}
	if got := forceConstraint(ToolChoice{Mode: "auto"}, &serial); !strings.Contains(got, "only one tool at a time") {
		t.Fatalf("serial selection alone must add the single-call rule, got %q", got)
	}
}
