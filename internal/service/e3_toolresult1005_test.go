package service

import (
	"testing"

	"lingma-ipc-proxy/internal/toolemulation"
)

func toolCallStub() []toolemulation.ToolCall {
	return []toolemulation.ToolCall{{ID: "toolu_1", Name: "screenshot", Arguments: map[string]any{}}}
}

// 1005 E-3 (HIST-05 service side): the current turn's tool-result attachments
// must survive the CLI "keep only the last user image" filter and reach the IPC
// prompt image extraction. The tool messages that trail the last user message
// are the turn being answered now.

func TestE3ToolResult1005FilterKeepsCurrentToolResultImages(t *testing.T) {
	req := ChatRequest{Messages: []ChatMessage{
		{Role: "user", Text: "first question", Images: []Image{{MediaType: "image/png", Data: "OLD"}}},
		{Role: "assistant", Text: "answered"},
		{Role: "user", Text: "look at the screen"},
		{Role: "assistant", ToolCalls: toolCallStub()},
		{Role: "tool", ToolCallID: "toolu_1", Text: "screenshot", Images: []Image{{MediaType: "image/png", Data: "NEW"}}},
	}}
	kept := requestWithCurrentTurnImagesOnly(req)
	if len(kept.Messages[0].Images) != 0 {
		t.Errorf("history attachment must still be dropped, got %d", len(kept.Messages[0].Images))
	}
	if len(kept.Messages[4].Images) != 1 || kept.Messages[4].Images[0].Data != "NEW" {
		t.Fatalf("HIST-05: current tool-result attachment stripped by the last-user-image filter: %+v", kept.Messages[4])
	}
	if !currentTurnHasImages(req) {
		t.Error("a turn whose tool result carries an image must count as the current turn's attachment")
	}
	// The caller's request must not be mutated by the filter.
	if len(req.Messages[0].Images) != 1 || len(req.Messages[4].Images) != 1 {
		t.Error("filter mutated the caller's request")
	}
}

func TestE3ToolResult1005ExtractLastUserImagesIncludesToolResults(t *testing.T) {
	messages := []ChatMessage{
		{Role: "user", Text: "look at the screen"},
		{Role: "assistant", ToolCalls: toolCallStub()},
		{Role: "tool", ToolCallID: "toolu_1", Text: "screenshot", Images: []Image{{MediaType: "image/png", Data: "SHOT"}}},
	}
	images := extractLastUserImages(messages)
	if len(images) != 1 || images[0].Data != "SHOT" {
		t.Fatalf("HIST-05: IPC prompt image extraction missed the tool-result attachment: %+v", images)
	}
}

// TestE3ToolResult1005FilterCounterCases pins the reverse direction: turns
// without tool-result attachments keep the existing filter behavior.
func TestE3ToolResult1005FilterCounterCases(t *testing.T) {
	historyOnly := ChatRequest{Messages: []ChatMessage{
		{Role: "user", Text: "old", Images: []Image{{MediaType: "image/png", Data: "OLD"}}},
		{Role: "assistant", Text: "answered"},
		{Role: "user", Text: "next question"},
	}}
	if currentTurnHasImages(historyOnly) {
		t.Error("history-only attachment must not count as the current turn's")
	}
	if kept := requestWithCurrentTurnImagesOnly(historyOnly); requestHasImages(kept) {
		t.Error("history-only attachment must be dropped")
	}
}
