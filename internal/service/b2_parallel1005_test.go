package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/toolemulation"
)

// b2Answer builds two distinct, valid action blocks for the declared tools.
const b2Answer = "```json action\n{\"tool\":\"Read\",\"parameters\":{\"path\":\"one.txt\"}}\n```\n" +
	"```json action\n{\"tool\":\"Read\",\"parameters\":{\"path\":\"two.txt\"}}\n```"

// TestB2Parallel1005SerialTruncatesToFirstCall: parallel_tool_calls=false and
// the model answers two calls. Per the COORD ruling the first call is kept and
// the dropped one must become visible prose with the not-sent notice -- no
// error, no silent disappearance.
func TestB2Parallel1005SerialTruncatesToFirstCall(t *testing.T) {
	p := a1Peer(t)
	p.onPrompt = func(p *ipcPeer, id string, n int) { p.chunk(id, b2Answer); p.finish(id) }
	s := newIPCService(t, p, Config{Timeout: time.Second})
	serial := false
	req := chatRequest("read both")
	req.Tools = b1Tools()
	req.ToolChoice = toolemulation.ToolChoice{Mode: "auto"}
	req.ParallelToolCalls = &serial
	result, err := s.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("serial truncation must not fail the turn: %v", err)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Arguments["path"] != "one.txt" {
		t.Fatalf("want only the first call kept, got %+v", result.ToolCalls)
	}
	if !strings.Contains(result.Text, "two.txt") {
		t.Fatalf("dropped call must stay visible in the text, got %q", result.Text)
	}
	if !strings.Contains(result.Text, "not sent") {
		t.Fatalf("notice must say the dropped call was not sent, got %q", result.Text)
	}
	if strings.Contains(result.Text, "were executed") || strings.Contains(result.Text, "was executed") {
		t.Fatalf("notice must not claim execution, got %q", result.Text)
	}
}

// TestB2Parallel1005ParallelAllowedKeepsBothCalls is the counter-example: with
// parallel tool calls allowed (flag unset), both calls are accepted.
func TestB2Parallel1005ParallelAllowedKeepsBothCalls(t *testing.T) {
	p := a1Peer(t)
	p.onPrompt = func(p *ipcPeer, id string, n int) { p.chunk(id, b2Answer); p.finish(id) }
	s := newIPCService(t, p, Config{Timeout: time.Second})
	req := chatRequest("read both")
	req.Tools = b1Tools()
	req.ToolChoice = toolemulation.ToolChoice{Mode: "auto"}
	result, err := s.Generate(context.Background(), req)
	if err != nil {
		t.Fatalf("parallel turn must not fail: %v", err)
	}
	if len(result.ToolCalls) != 2 {
		t.Fatalf("want both calls kept when parallelism is allowed, got %+v", result.ToolCalls)
	}
}
