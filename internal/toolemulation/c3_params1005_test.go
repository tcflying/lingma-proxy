package toolemulation

import (
	"strings"
	"testing"
)

// TestC3Params1005AdditionalPropertiesPolicy: the parameter filter used to
// drop every undeclared field unconditionally, so a schema that explicitly
// allows extension fields lost them before execution, and one that forbids
// them silently deleted the evidence instead of refusing the call.
func TestC3Params1005AdditionalPropertiesPolicy(t *testing.T) {
	build := func(additional any) ToolDef {
		schema := map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required": []any{"path"},
		}
		if additional != nil {
			schema["additionalProperties"] = additional
		}
		return ToolDef{Name: "Read", InputSchema: schema}
	}
	text := "```json action\n{\"tool\":\"Read\",\"parameters\":{\"path\":\"a.txt\",\"tag\":\"dynamic\"}}\n```"

	calls, _, err := ParseActionBlocks(text, []ToolDef{build(true)}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("allowed dynamic fields must keep the call, got %+v", calls)
	}
	if got, _ := calls[0].Arguments["tag"].(string); got != "dynamic" {
		t.Fatalf("additionalProperties=true must keep the extension field, got %+v", calls[0].Arguments)
	}

	calls, clean, err := ParseActionBlocks(text, []ToolDef{build(false)}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("additionalProperties=false must refuse the call, not silently delete the field: %+v", calls)
	}
	if !strings.Contains(clean, "dynamic") {
		t.Fatalf("refused block must stay visible, clean = %q", clean)
	}
}

// TestC3Params1005RequiredIsExistenceNotValue: "required" says the field must
// be present; an empty string was rejected as if required implied non-empty.
// Only a schema that says so (minLength) may reject the empty value.
func TestC3Params1005RequiredIsExistenceNotValue(t *testing.T) {
	plain := xmlSchemaTool("Note", map[string]any{"text": map[string]any{"type": "string"}}, "text")
	calls, _, err := ParseActionBlocks(xCall("Note", xParam("text", "")), []ToolDef{plain}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("required-but-empty must stay accepted, got %+v", calls)
	}

	minLength := ToolDef{Name: "Note", InputSchema: map[string]any{
		"type": "object",
		"properties": map[string]any{
			"text": map[string]any{"type": "string", "minLength": 1},
		},
		"required": []any{"text"},
	}}
	calls, clean, err := ParseActionBlocks(xCall("Note", xParam("text", "")), []ToolDef{minLength}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("minLength=1 must reject the empty value, got %+v", calls)
	}
	if !strings.Contains(clean, xOpen) {
		t.Fatalf("refused block must stay visible, clean = %q", clean)
	}
}

// TestC3Params1005DeclaredBasicTypesValidated: a declared field whose basic
// type does not match used to be promoted into a call anyway, handing the
// client arguments the tool cannot read.
func TestC3Params1005DeclaredBasicTypesValidated(t *testing.T) {
	tools := []ToolDef{xmlSchemaTool("Bash", map[string]any{
		"command": map[string]any{"type": "string"},
		"timeout": map[string]any{"type": "integer"},
		"force":   map[string]any{"type": "boolean"},
	}, "command")}

	calls, _, err := ParseActionBlocks(xCall("Bash", xParam("command", "pwd"), xParam("timeout", "soon")), tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("non-integer timeout must be refused, got %+v", calls)
	}

	calls, _, err = ParseActionBlocks(xCall("Bash", xParam("command", "pwd"), xParam("force", "maybe")), tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("non-boolean force must be refused, got %+v", calls)
	}

	jsonTools := []ToolDef{xmlSchemaTool("Read", map[string]any{
		"path":    map[string]any{"type": "string"},
		"timeout": map[string]any{"type": "integer"},
	}, "path")}
	calls, _, err = ParseActionBlocks("```json action\n{\"tool\":\"Read\",\"parameters\":{\"path\":7,\"timeout\":3}}\n```", jsonTools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("numeric path must be refused, got %+v", calls)
	}

	// Counter-examples: an integral JSON number and a coerced XML integer both
	// stay legal integers, and a real number still coerces.
	calls, _, err = ParseActionBlocks("```json action\n{\"tool\":\"Read\",\"parameters\":{\"path\":\"a.txt\",\"timeout\":3}}\n```", jsonTools, Config{})
	if err != nil || len(calls) != 1 || calls[0].Arguments["timeout"] != float64(3) {
		t.Fatalf("integral JSON integer must pass, calls=%+v err=%v", calls, err)
	}
	calls, _, err = ParseActionBlocks(xCall("Bash", xParam("command", "pwd"), xParam("timeout", "15000")), tools, Config{})
	if err != nil || len(calls) != 1 || calls[0].Arguments["timeout"] != int64(15000) {
		t.Fatalf("coerced XML integer must pass, calls=%+v err=%v", calls, err)
	}
}

// TestC3Params1005XMLStringPreservedVerbatim: the XML dialect used to
// TrimSpace parameter values, rewriting file content that legitimately starts
// or ends with whitespace. Only the single wire-format newline next to the
// tags is a separator; the payload itself must survive byte-for-byte.
func TestC3Params1005XMLStringPreservedVerbatim(t *testing.T) {
	tools := []ToolDef{xmlSchemaTool("Write", map[string]any{
		"content": map[string]any{"type": "string"},
	}, "content")}

	const payload = "\n  indented line\nlast line\n"
	calls, _, err := ParseActionBlocks(xCall("Write", xParam("content", payload)), tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("verbatim payload must stay a call, got %+v", calls)
	}
	if got, _ := calls[0].Arguments["content"].(string); got != payload {
		t.Fatalf("content = %q, want the payload verbatim %q", got, payload)
	}
}
