package httpapi

import (
	"testing"
)

// TestB2Parallel1005AnthropicDisableParallelMapped: Anthropic carries the
// "one tool at a time" selection as tool_choice.disable_parallel_tool_use.
// The conversion used to drop it, so the rest of the chain never saw the
// client's serial-tooling decision.
func TestB2Parallel1005AnthropicDisableParallelMapped(t *testing.T) {
	build := func(toolChoice any) anthropicRequest {
		return anthropicRequest{
			Model:     "synthetic",
			MaxTokens: 16,
			Messages:  []rawMessage{{Role: "user", Content: "fixture"}},
			Tools: []any{map[string]any{
				"name":         "Read",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}},
			}},
			ToolChoice: toolChoice,
		}
	}
	cases := []struct {
		name       string
		toolChoice any
		wantFalse  bool
	}{
		{"any disabled", map[string]any{"type": "any", "disable_parallel_tool_use": true}, true},
		{"auto disabled", map[string]any{"type": "auto", "disable_parallel_tool_use": true}, true},
		{"tool disabled", map[string]any{"type": "tool", "name": "Read", "disable_parallel_tool_use": true}, true},
		{"any parallel allowed", map[string]any{"type": "any", "disable_parallel_tool_use": false}, false},
		{"no choice", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeAnthropicRequest(build(tc.toolChoice))
			if err != nil {
				t.Fatalf("normalizeAnthropicRequest: %v", err)
			}
			if tc.wantFalse {
				if got.ParallelToolCalls == nil || *got.ParallelToolCalls {
					t.Fatalf("disable_parallel_tool_use=true must map to ParallelToolCalls=false, got %v", got.ParallelToolCalls)
				}
				return
			}
			if got.ParallelToolCalls != nil {
				t.Fatalf("selection without the flag must stay unset, got %v", *got.ParallelToolCalls)
			}
		})
	}
}
