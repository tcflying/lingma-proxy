package httpapi

import (
	"strings"
	"testing"

	"lingma-ipc-proxy/internal/service"
	"lingma-ipc-proxy/internal/toolemulation"
)

// 1005 D-5 (HTTP-04 + TOOLS-05): the OpenAI Chat SSE dispatch pushed every
// stream event's delta into content without checking its type, so IPC thought
// chunks leaked as prose even under reasoning_effort=none; and the stream
// filter had no call budget, so action blocks the final acceptance suppresses
// (over the whole-turn cap) were silently deleted from the delta stream
// instead of surfacing as prose.

func d5ToolRequest(effort string) service.ChatRequest {
	req := service.ChatRequest{
		Model:    "synthetic",
		Messages: []service.ChatMessage{{Role: "user", Text: "run the tools"}},
		Tools: []toolemulation.ToolDef{{
			Name:        "Read",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}},
		}},
		ToolChoice:      toolemulation.ToolChoice{Mode: "any"},
		ReasoningEffort: effort,
	}
	return req
}

func d5ActionBlock(n int) string {
	return "```json action\n{\"tool\":\"Read\",\"parameters\":{\"path\":\"f" + string(rune('a'+n)) + ".txt\"}}\n```"
}

// TestD5Stream1005ChatThinkingNeverEntersContent drives the Chat SSE event
// classification for a thought chunk under both settings: the Chat completion
// surface this proxy serves declares no reasoning extension, so the thought
// has no legal slot there and must be dropped rather than folded into prose.
func TestD5Stream1005ChatThinkingNeverEntersContent(t *testing.T) {
	for _, effort := range []string{"none", "" /* default */, "high"} {
		t.Run("effort="+effort, func(t *testing.T) {
			event := service.StreamEvent{Type: service.StreamEventThinking, Delta: "secret reasoning"}
			if openAIChatStreamCarriesText(event) {
				t.Fatalf("HTTP-04: thinking delta must not enter Chat content (effort=%q)", effort)
			}
			text := service.StreamEvent{Type: service.StreamEventText, Delta: "real prose"}
			if !openAIChatStreamCarriesText(text) {
				t.Fatal("text deltas must keep flowing")
			}
		})
	}
}

// TestD5Stream1005OverCapBlocksStayVisibleAsProse pushes one more action block
// than the whole-turn cap through the live stream filter: the final acceptance
// keeps the surplus as prose, so the delta stream must surface it too instead
// of silently deleting it (TOOLS-05).
func TestD5Stream1005OverCapBlocksStayVisibleAsProse(t *testing.T) {
	t.Setenv("LINGMA_MAX_TOOL_CALLS", "3")
	filter := newToolStreamFilter(d5ToolRequest(""))
	var emitted strings.Builder
	push := func(text string) {
		for _, delta := range filter.Push(text) {
			emitted.WriteString(delta)
		}
	}
	push("intro prose\n")
	for i := 0; i < 5; i++ {
		push(d5ActionBlock(i))
		push("\n")
	}
	push("closing prose\n")
	for _, delta := range filter.Flush() {
		emitted.WriteString(delta)
	}
	got := emitted.String()
	t.Logf("emitted=%q", got)
	if !strings.Contains(got, "intro prose") || !strings.Contains(got, "closing prose") {
		t.Fatalf("prose lost: %q", got)
	}
	// Blocks 4 and 5 are over the cap: the acceptance keeps them as prose, so
	// they must be visible here, fences included.
	for _, i := range []int{3, 4} {
		if !strings.Contains(got, "\"path\":\"f"+string(rune('a'+i))+".txt\"") {
			t.Fatalf("TOOLS-05: over-cap block %d silently deleted from the stream: %q", i+1, got)
		}
	}
}

// TestD5Stream1005WithinCapStillSuppressed is the reverse counter-case: blocks
// the acceptance consumes as calls stay out of the prose stream.
func TestD5Stream1005WithinCapStillSuppressed(t *testing.T) {
	t.Setenv("LINGMA_MAX_TOOL_CALLS", "3")
	filter := newToolStreamFilter(d5ToolRequest(""))
	var emitted strings.Builder
	for _, delta := range filter.Push("prose\n" + d5ActionBlock(0) + "\n" + d5ActionBlock(1) + "\n") {
		emitted.WriteString(delta)
	}
	for _, delta := range filter.Flush() {
		emitted.WriteString(delta)
	}
	got := emitted.String()
	if strings.Contains(got, "json action") || strings.Contains(got, "\"tool\"") {
		t.Fatalf("within-cap blocks must stay suppressed as calls: %q", got)
	}
	if !strings.Contains(got, "prose") {
		t.Fatalf("prose lost: %q", got)
	}
}

// TestD5Stream1005CounterCases pins the untouched directions: no tools or
// tool_choice=none disables suppression, and an unterminated fence stays prose
// at Flush (the Responses late-text and aggregate behaviours are driven by the
// completed frame / their own paths and are not asserted as regressions here).
func TestD5Stream1005CounterCases(t *testing.T) {
	t.Setenv("LINGMA_MAX_TOOL_CALLS", "3")
	noTools := d5ToolRequest("")
	noTools.Tools = nil
	filter := newToolStreamFilter(noTools)
	if out := strings.Join(filter.Push(d5ActionBlock(0)), ""); !strings.Contains(out, "json action") {
		t.Fatal("filter must be disabled without tools")
	}
	choiceNone := d5ToolRequest("")
	choiceNone.ToolChoice = toolemulation.ToolChoice{Mode: "none"}
	filter2 := newToolStreamFilter(choiceNone)
	if out := strings.Join(filter2.Push(d5ActionBlock(0)), ""); !strings.Contains(out, "json action") {
		t.Fatal("filter must be disabled under tool_choice=none")
	}

	fenced := newToolStreamFilter(d5ToolRequest(""))
	var out strings.Builder
	for _, delta := range fenced.Push("opening fence never closed ```json action\n{\"tool\"") {
		out.WriteString(delta)
	}
	for _, delta := range fenced.Flush() {
		out.WriteString(delta)
	}
	if !strings.Contains(out.String(), "```json action") {
		t.Fatalf("an unterminated fence is prose and must survive Flush: %q", out.String())
	}
}
