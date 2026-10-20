package service

import (
	"lingma-ipc-proxy/internal/toolemulation"
	"testing"
)

func TestEmptyToolResult1005NativeAndControls(t *testing.T) {
	for _, choice := range []string{"absent", "none"} {
		req := ChatRequest{Messages: []ChatMessage{{Role: "user", Text: "task"}, {Role: "assistant", ToolCalls: []toolemulation.ToolCall{{ID: "call_empty", Name: "read"}}}, {Role: "tool", ToolCallID: "call_empty", Text: "  "}, {Role: "tool", Text: ""}, {Role: "user", Text: ""}, {Role: "tool", ToolCallID: "call_text", Text: "real"}}}
		if choice == "none" {
			req.Tools = []toolemulation.ToolDef{{Name: "read"}}
			req.ToolChoice = toolemulation.ToolChoice{Mode: "none"}
		}
		got := remoteMessagesForChat(req, "ignored", true)
		if len(got) != 4 || got[2].Role != "tool" || got[2].ToolCallID != "call_empty" || got[2].Content != "" || got[3].Content != "real" {
			t.Errorf("%s: empty paired tool must survive, invalid blank must not: %#v", choice, got)
		}
	}
	req := ChatRequest{Messages: []ChatMessage{{Role: "tool", ToolCallID: "call_empty", Text: ""}}, Tools: []toolemulation.ToolDef{{Name: "read"}}, ToolChoice: toolemulation.ToolChoice{Mode: "auto"}}
	got := remoteMessagesForChat(req, "synthetic paired prompt", true)
	if len(got) != 1 || got[0].Role != "user" || got[0].Content != "synthetic paired prompt" {
		t.Fatalf("emulation changed: %#v", got)
	}
}
