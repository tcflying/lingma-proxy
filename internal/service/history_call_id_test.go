package service

import (
	"encoding/json"
	"strings"
	"testing"

	"lingma-ipc-proxy/internal/toolemulation"
)

// A multi-call assistant turn replays each completed call with its own id and
// an explicit already-executed marker, so results arriving in any order pair
// with the call that produced them -- and intentional repeat calls stay
// distinct instead of being deduplicated away.

func history931Request() ChatRequest {
	return ChatRequest{
		Tools: []toolemulation.ToolDef{{Name: "read_fixture", InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []any{"path"},
		}}},
		Messages: []ChatMessage{
			{Role: "user", Text: "Read both fixture files, then compare their results."},
			{Role: "assistant", ToolCalls: []toolemulation.ToolCall{
				{ID: "call_alpha_931", Name: "read_fixture", Arguments: map[string]any{"path": "alpha.txt"}},
				{ID: "call_beta_931", Name: "read_fixture", Arguments: map[string]any{"path": "beta.txt"}},
			}},
			{Role: "tool", ToolCallID: "call_beta_931", Text: "BETA_ACTUAL"},
			{Role: "tool", ToolCallID: "call_alpha_931", Text: "ALPHA_ACTUAL"},
			{Role: "user", Text: "Now summarise."},
		},
	}
}

func TestHistory931RendersEachCallIDBeforeItsResult(t *testing.T) {
	_, prompt, err := buildLingmaPromptSections(history931Request(), SessionModeFresh, true, false)
	if err != nil {
		t.Fatal(err)
	}
	end := strings.Index(prompt, "User: Tool result for")
	if end < 0 {
		t.Fatal("missing tool result boundary")
	}
	assistantHistory := prompt[:end]
	for _, id := range []string{"call_alpha_931", "call_beta_931"} {
		if !strings.Contains(assistantHistory, id) {
			t.Errorf("completed call identity %q missing from the assistant history before its result", id)
		}
	}
	// Every result pairs with the announced call id, in either order.
	for _, pair := range [][2]string{{"call_alpha_931", "ALPHA_ACTUAL"}, {"call_beta_931", "BETA_ACTUAL"}} {
		id, body := pair[0], pair[1]
		if !strings.Contains(prompt, "Tool result for "+id+":\n"+body) {
			t.Errorf("result %q is not paired with its call id %q", body, id)
		}
	}
}

func TestHistory931MarksCallsAsAlreadyExecuted(t *testing.T) {
	_, prompt, err := buildLingmaPromptSections(history931Request(), SessionModeFresh, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "call_alpha_931") || !strings.Contains(prompt, "call_beta_931") {
		t.Fatal("call ids missing from prompt")
	}
	// The marker must sit with the ids, so the model knows the blocks are
	// history it already ran, not requests to execute again.
	for _, id := range []string{"call_alpha_931", "call_beta_931"} {
		idx := strings.Index(prompt, id)
		window := prompt[max(0, idx-120):idx]
		if !strings.Contains(strings.ToLower(window), "completed") && !strings.Contains(prompt[:idx+len(id)+80], "already") {
			t.Errorf("call %q carries no already-executed marker nearby", id)
		}
	}
}

func TestHistory931PreservesIntentionalRepeatCalls(t *testing.T) {
	req := history931Request()
	req.Messages = []ChatMessage{
		{Role: "user", Text: "Read the same file twice."},
		{Role: "assistant", ToolCalls: []toolemulation.ToolCall{
			{ID: "call_first_931", Name: "read_fixture", Arguments: map[string]any{"path": "alpha.txt"}},
			{ID: "call_second_931", Name: "read_fixture", Arguments: map[string]any{"path": "alpha.txt"}},
		}},
		{Role: "tool", ToolCallID: "call_first_931", Text: "FIRST_ACTUAL"},
		{Role: "tool", ToolCallID: "call_second_931", Text: "SECOND_ACTUAL"},
		{Role: "user", Text: "Compare."},
	}
	_, prompt, err := buildLingmaPromptSections(req, SessionModeFresh, true, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"call_first_931", "call_second_931", "FIRST_ACTUAL", "SECOND_ACTUAL"} {
		if !strings.Contains(prompt, id) {
			t.Errorf("repeat-call history lost %q (a client's intentional duplicate call must survive)", id)
		}
	}
	if got := strings.Count(prompt, `"tool":"read_fixture"`); got != 2 {
		t.Fatalf("read_fixture history blocks = %d, want 2 distinct blocks for the repeat call", got)
	}
}

// The id travels outside the JSON body: the fenced dialect the parser reads
// stays {tool, parameters} only, so an echoed history block can never smuggle
// an id into tool arguments.
func TestHistory931BlockKeepsIDOutOfParserDialect(t *testing.T) {
	block := renderHistoryActionBlock(toolemulation.ToolCall{
		ID: "call_parser_931", Name: "read_fixture", Arguments: map[string]any{"path": "alpha.txt"},
	})
	fenceStart := strings.Index(block, "```json action\n")
	fenceEnd := strings.LastIndex(block, "\n```")
	if fenceStart < 0 || fenceEnd <= fenceStart {
		t.Fatalf("history block lost its action fence: %q", block)
	}
	body := block[fenceStart+len("```json action\n") : fenceEnd]
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("fenced body is not JSON: %v\n%s", err, body)
	}
	if _, hasID := decoded["id"]; hasID {
		t.Fatalf("id leaked into the fenced JSON body: %s", body)
	}
	if decoded["tool"] != "read_fixture" {
		t.Fatalf("tool = %#v, want read_fixture", decoded["tool"])
	}
	args, _ := decoded["parameters"].(map[string]any)
	if args["path"] != "alpha.txt" {
		t.Fatalf("parameters = %#v, want path alpha.txt untouched", decoded["parameters"])
	}

	// And the parser itself stays unpolluted when a model echoes the whole
	// block verbatim: one call, arguments exactly as declared.
	calls, _, err := toolemulation.ParseActionBlocks(block, []toolemulation.ToolDef{{
		Name: "read_fixture",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []any{"path"},
		},
	}}, toolemulation.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want the echoed block to parse as exactly one call", calls)
	}
	if calls[0].Arguments["path"] != "alpha.txt" || len(calls[0].Arguments) != 1 {
		t.Fatalf("arguments = %#v, want only the declared parameters (no id smuggled in)", calls[0].Arguments)
	}
}
