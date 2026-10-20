package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/toolemulation"
)

// 1005 E-1 (HIST-03): IPC reuse mode used to send only the last converted user
// message, dropping parallel tool results and the current tool constraints.
// COORD ruling: tool-carrying requests route through the existing fresh full
// replay; pure-text single-message continuation keeps reuse semantics.

func e1ToolDefs() []toolemulation.ToolDef {
	return []toolemulation.ToolDef{{
		Name:        "Read",
		Description: "read a file",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []any{"path"},
		},
	}}
}

func e1ParallelToolRequest() ChatRequest {
	req := chatRequest("check both fixtures")
	req.Tools = e1ToolDefs()
	req.ToolChoice = toolemulation.ToolChoice{Mode: "any"}
	req.Messages = []ChatMessage{
		{Role: "user", Text: "check both fixtures"},
		{Role: "assistant", ToolCalls: []toolemulation.ToolCall{
			{ID: "call-1", Name: "Read", Arguments: map[string]any{"path": "a.txt"}},
			{ID: "call-2", Name: "Read", Arguments: map[string]any{"path": "b.txt"}},
		}},
		{Role: "tool", ToolCallID: "call-1", Text: "alpha contents"},
		{Role: "tool", ToolCallID: "call-2", Text: "beta contents"},
	}
	return req
}

func TestE1Reuse1005ParallelToolResultsEnterPrompt(t *testing.T) {
	p := a1Peer(t)
	// Answer with an action block so the tool_choice contract accepts the turn.
	p.onPrompt = func(p *ipcPeer, id string, n int) { p.chunk(id, a1Action); p.finish(id) }
	s := newIPCService(t, p, Config{SessionMode: SessionModeReuse, Timeout: time.Second})
	r, e := s.generateWithReconnect(context.Background(), e1ParallelToolRequest(), nil)
	prompts := p.promptTexts()
	joined := strings.Join(prompts, "\n--\n")
	effective := ""
	if r != nil {
		effective = string(r.EffectiveSession)
	}
	t.Logf("err=%v effective=%v prompts=%d deleted=%v sticky=%q", e, effective, len(prompts), p.deletedSessions(), s.stickySessionForTest())
	if e != nil || r == nil {
		t.Fatalf("valid tool turn failed: %v", e)
	}
	for _, want := range []string{
		"Tool result for call-1", "alpha contents",
		"Tool result for call-2", "beta contents",
		"Available tools:", "Read(",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("prompt missing %q (HIST-03: reuse shortcut dropped parallel tool results / current tool constraints)", want)
		}
	}
	if len(prompts) != 1 {
		t.Errorf("prompts = %d, want exactly 1", len(prompts))
	}
	if r.EffectiveSession != SessionModeFresh {
		t.Errorf("effective mode = %v, want fresh for a tool-carrying turn", r.EffectiveSession)
	}
}

// TestE1Reuse1005SingleToolResultStillEntersPrompt is the boundary counter-case:
// a single tool result whose message is also the last one still reaches the
// model in full, whichever side of the routing split it lands on.
func TestE1Reuse1005SingleToolResultStillEntersPrompt(t *testing.T) {
	p := a1Peer(t)
	p.onPrompt = func(p *ipcPeer, id string, n int) { p.chunk(id, a1Action); p.finish(id) }
	s := newIPCService(t, p, Config{SessionMode: SessionModeReuse, Timeout: time.Second})
	req := chatRequest("check the fixture")
	req.Tools = e1ToolDefs()
	req.ToolChoice = toolemulation.ToolChoice{Mode: "any"}
	req.Messages = []ChatMessage{
		{Role: "user", Text: "check the fixture"},
		{Role: "assistant", ToolCalls: []toolemulation.ToolCall{
			{ID: "call-1", Name: "Read", Arguments: map[string]any{"path": "a.txt"}},
		}},
		{Role: "tool", ToolCallID: "call-1", Text: "only result"},
	}
	r, e := s.generateWithReconnect(context.Background(), req, nil)
	prompts := p.promptTexts()
	joined := strings.Join(prompts, "\n--\n")
	effective := ""
	if r != nil {
		effective = string(r.EffectiveSession)
	}
	t.Logf("err=%v effective=%v prompts=%d", e, effective, len(prompts))
	if e != nil || r == nil {
		t.Fatalf("valid tool turn failed: %v", e)
	}
	if !strings.Contains(joined, "Tool result for call-1") || !strings.Contains(joined, "only result") {
		t.Errorf("single tool result did not reach the prompt in full: %q", joined)
	}
}

// TestE1Reuse1005PlainTextContinuationUnchanged is the reverse counter-case:
// a pure-text single-message continuation keeps reuse behavior unchanged.
func TestE1Reuse1005PlainTextContinuationUnchanged(t *testing.T) {
	p := a1Peer(t)
	s := newIPCService(t, p, Config{SessionMode: SessionModeReuse, Timeout: time.Second})
	r, e := s.generateWithReconnect(context.Background(), chatRequest("follow up"), nil)
	prompts := p.promptTexts()
	t.Logf("err=%v effective=%v prompts=%q deleted=%v sticky=%q", e, r.EffectiveSession, prompts, p.deletedSessions(), s.stickySessionForTest())
	if e != nil {
		t.Fatal("pure-text reuse turn failed")
	}
	if len(prompts) != 1 || prompts[0] != "follow up" {
		t.Errorf("pure-text single-message reuse prompt changed: %q", prompts)
	}
	if r.EffectiveSession != SessionModeReuse {
		t.Errorf("effective mode = %v, want reuse for pure-text continuation", r.EffectiveSession)
	}
	if sticky := s.stickySessionForTest(); sticky != p.session {
		t.Errorf("sticky session = %q, want the answered session %q kept", sticky, p.session)
	}
}
