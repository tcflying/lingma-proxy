package service

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/toolemulation"
)

// b1Tools declares two tools so a forced name can be answered by the wrong one.
func b1Tools() []toolemulation.ToolDef {
	return []toolemulation.ToolDef{
		{Name: "Read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}}},
		{Name: "Write", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}}},
	}
}

const (
	b1ReadBlock  = "```json action\n{\"tool\":\"Read\",\"parameters\":{\"path\":\"a.txt\"}}\n```"
	b1WriteBlock = "```json action\n{\"tool\":\"Write\",\"parameters\":{\"path\":\"b.txt\"}}\n```"
)

// TestB1Choice1005ForcedNameMismatchRejected: tool_choice names Read but the
// model answers Write (also declared). The old acceptance returned the wrong
// call as a silent success; the common acceptance point must reject it.
func TestB1Choice1005ForcedNameMismatchRejected(t *testing.T) {
	p := a1Peer(t)
	p.onPrompt = func(p *ipcPeer, id string, n int) { p.chunk(id, b1WriteBlock); p.finish(id) }
	s := newIPCService(t, p, Config{Timeout: time.Second})
	req := chatRequest("edit the file")
	req.Tools = b1Tools()
	req.ToolChoice = toolemulation.ToolChoice{Mode: "tool", Name: "Read"}
	result, err := s.Generate(context.Background(), req)
	calls := 0
	if result != nil {
		calls = len(result.ToolCalls)
	}
	t.Logf("err=%v calls=%d", err, calls)
	if err == nil || result != nil {
		t.Fatalf("forced-name mismatch accepted silently: result=%+v err=%v", result, err)
	}
}

// TestB1Choice1005RequiredMissingAfterRetryErrors: choice=any, both attempts
// answer plain prose. The existing forced-tooling retry is the one retry layer;
// when it also fails to produce a call the turn must end in an explicit error,
// not in a call-free success.
func TestB1Choice1005RequiredMissingAfterRetryErrors(t *testing.T) {
	p := a1Peer(t)
	p.onPrompt = func(p *ipcPeer, id string, n int) { p.chunk(id, "普通回答，没有工具调用。"); p.finish(id) }
	s := newIPCService(t, p, Config{Timeout: time.Second})
	req := chatRequest("do something")
	req.Tools = b1Tools()
	req.ToolChoice = toolemulation.ToolChoice{Mode: "any"}
	result, err := s.Generate(context.Background(), req)
	t.Logf("err=%v result=%+v prompts=%d", err, result, len(p.promptTexts()))
	if err == nil || result != nil {
		t.Fatalf("required choice with zero calls accepted silently: result=%+v err=%v", result, err)
	}
}

// TestB1Choice1005AnyDeclaredToolSucceeds is the counter-example that must keep
// passing: choice=any and the model answers with a declared tool call.
func TestB1Choice1005AnyDeclaredToolSucceeds(t *testing.T) {
	p := a1Peer(t)
	p.onPrompt = func(p *ipcPeer, id string, n int) { p.chunk(id, b1WriteBlock); p.finish(id) }
	s := newIPCService(t, p, Config{Timeout: time.Second})
	req := chatRequest("do something")
	req.Tools = b1Tools()
	req.ToolChoice = toolemulation.ToolChoice{Mode: "any"}
	result, err := s.Generate(context.Background(), req)
	if err != nil || result == nil || len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "Write" {
		t.Fatalf("any + declared tool must succeed: result=%+v err=%v", result, err)
	}
}

// TestB1Choice1005NoneNativeRemoteCallRejected: tool_choice=none on the remote
// native channel. The gateway answered with a native tool call anyway; the old
// path passed it through because emulation is skipped for none.
func TestB1Choice1005NoneNativeRemoteCallRejected(t *testing.T) {
	sandboxProfileDirs(t)
	auth := filepath.Join(t.TempDir(), "synthetic-auth.json")
	if err := os.WriteFile(auth, []byte(`{"source":"synthetic","auth":{"cosy_key":"synthetic-key","encrypt_user_info":"synthetic-info","user_id":"synthetic-user","machine_id":"synthetic-machine"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	old := http.DefaultTransport
	http.DefaultTransport = b1Transport(func(r *http.Request) (*http.Response, error) {
		frame := func(payload any) string {
			b, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			outer, err := json.Marshal(map[string]any{"body": string(b), "statusCodeValue": 200})
			if err != nil {
				t.Fatal(err)
			}
			return "data: " + string(outer) + "\n\n"
		}
		toolFrame := frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_b1", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{\"path\":\"a.txt\"}"}}}}}}})
		frames := toolFrame + frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}}) + "data: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	s := New(Config{Backend: BackendRemote, RemoteBaseURL: "http://b1.invalid", RemoteAuthFile: auth, Timeout: 5 * time.Second})
	t.Cleanup(func() { _ = s.Close() })
	req := ChatRequest{Model: "kmodel", Messages: []ChatMessage{{Role: "user", Text: "plain answer only"}}, Tools: b1Tools(), ToolChoice: toolemulation.ToolChoice{Mode: "none"}}
	result, err := s.Generate(context.Background(), req)
	calls := 0
	if result != nil {
		calls = len(result.ToolCalls)
	}
	t.Logf("err=%v calls=%d", err, calls)
	if err == nil || result != nil {
		t.Fatalf("none + native remote call accepted silently: result=%+v err=%v", result, err)
	}
}

type b1Transport func(*http.Request) (*http.Response, error)

func (f b1Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
