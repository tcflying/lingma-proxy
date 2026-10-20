package toolemulation

import (
	"math"
	"strings"
	"testing"
)

// TestB4Nonfinite1005XMLNumberRejected: the XML dialect's number coercion
// used strconv.ParseFloat, which happily accepts NaN/Inf -- a value
// encoding/json then refuses to serialize, so the call died later as an empty
// body instead of being refused at the acceptance gate.
func TestB4Nonfinite1005XMLNumberRejected(t *testing.T) {
	tools := []ToolDef{xmlSchemaTool("Bash", map[string]any{
		"command": map[string]any{"type": "string"},
		"timeout": map[string]any{"type": "number"},
	}, "command")}
	for _, bad := range []string{"NaN", "Inf", "-Inf", "Infinity", "+Inf"} {
		t.Run(bad, func(t *testing.T) {
			text := xCall("Bash", xParam("command", "pwd"), xParam("timeout", bad))
			calls, clean, err := ParseActionBlocks(text, tools, Config{})
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 0 {
				t.Fatalf("non-finite number %q must be refused at the gate, got %+v", bad, calls[0].Arguments)
			}
			if !strings.Contains(clean, bad) {
				t.Fatalf("refused block must stay visible, clean = %q", clean)
			}
		})
	}
}

// TestB4Nonfinite1005FiniteAndStringPass is the counter-example pair: finite
// numbers still coerce, and the literal text "NaN" bound to a string-typed
// parameter is an ordinary value, not a non-finite number.
func TestB4Nonfinite1005FiniteAndStringPass(t *testing.T) {
	numberTools := []ToolDef{xmlSchemaTool("Bash", map[string]any{
		"command": map[string]any{"type": "string"},
		"timeout": map[string]any{"type": "number"},
	}, "command")}
	calls, _, err := ParseActionBlocks(xCall("Bash", xParam("command", "pwd"), xParam("timeout", "-2.5")), numberTools, Config{})
	if err != nil || len(calls) != 1 {
		t.Fatalf("finite number must pass: calls=%+v err=%v", calls, err)
	}
	if got, ok := calls[0].Arguments["timeout"].(float64); !ok || got != -2.5 || math.IsNaN(got) || math.IsInf(got, 0) {
		t.Fatalf("timeout = %#v, want finite -2.5", calls[0].Arguments["timeout"])
	}

	stringTools := []ToolDef{xmlSchemaTool("Note", map[string]any{
		"text": map[string]any{"type": "string"},
	}, "text")}
	calls, _, err = ParseActionBlocks(xCall("Note", xParam("text", "NaN")), stringTools, Config{})
	if err != nil || len(calls) != 1 {
		t.Fatalf("string-typed NaN text must pass: calls=%+v err=%v", calls, err)
	}
	if got, _ := calls[0].Arguments["text"].(string); got != "NaN" {
		t.Fatalf("text = %#v, want the literal string", calls[0].Arguments["text"])
	}
}
