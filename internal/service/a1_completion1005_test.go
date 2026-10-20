package service

import (
	"context"
	"lingma-ipc-proxy/internal/toolemulation"
	"strings"
	"testing"
	"time"
)

func a1Close(p *ipcPeer) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, c := range p.conns {
		_ = c.Close()
	}
}
func a1Peer(t *testing.T) *ipcPeer { p := startPeer(t); t.Cleanup(func() { a1Close(p) }); return p }
func a1ToolReq() ChatRequest {
	r := chatRequest("read synthetic fixture")
	r.Tools = []toolemulation.ToolDef{{Name: "Read", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}, "required": []any{"path"}}}}
	r.ToolChoice = toolemulation.ToolChoice{Mode: "any"}
	return r
}

const a1Action = "```json action\n{\"tool\":\"Read\",\"parameters\":{\"path\":\"fixture\"}}\n```"

func TestA1Completion1005PartialDisconnect(t *testing.T) {
	p := a1Peer(t)
	p.onPrompt = func(p *ipcPeer, id string, n int) { p.chunk(id, "visible-partial"); a1Close(p) }
	s := newIPCService(t, p, Config{Timeout: time.Second})
	var out strings.Builder
	r, e := s.generateWithReconnect(context.Background(), chatRequest("fixture"), func(v StreamEvent) { out.WriteString(v.Delta) })
	t.Logf("err=%v result=%+v callbacks=%q prompts=%d deleted=%v", e, r, out.String(), len(p.promptTexts()), p.deletedSessions())
	if e == nil {
		t.Error("missing chat_finish accepted as success")
	}
	if len(p.promptTexts()) != 1 {
		t.Error("visible partial transparently replayed")
	}
	if out.String() != "visible-partial" {
		t.Error("partial output lost or duplicated")
	}
}
func TestA1Completion1005RetryTimeout(t *testing.T) {
	p := a1Peer(t)
	p.onPrompt = func(p *ipcPeer, id string, n int) {
		if n == 1 {
			p.chunk(id, "first prose")
			p.finish(id)
		} else {
			p.chunk(id, a1Action)
		}
	}
	s := newIPCService(t, p, Config{SessionMode: SessionModeReuse, Timeout: 150 * time.Millisecond})
	r, e := s.generateWithReconnect(context.Background(), a1ToolReq(), nil)
	calls := 0
	if r != nil {
		calls = len(r.ToolCalls)
	}
	t.Logf("err=%v calls=%d prompts=%d deleted=%v sticky=%q", e, calls, len(p.promptTexts()), p.deletedSessions(), s.stickySessionForTest())
	if e == nil || calls != 0 {
		t.Error("unfinished retry accepted")
	}
	if s.stickySessionForTest() != "" || len(p.deletedSessions()) != 1 {
		t.Error("incomplete retry session not reclaimed")
	}
}
func TestA1Completion1005RetryError(t *testing.T) {
	p := a1Peer(t)
	p.onPrompt = func(p *ipcPeer, id string, n int) {
		if n == 1 {
			p.chunk(id, "first prose")
			p.finish(id)
		} else {
			a1Close(p)
		}
	}
	svc := newIPCService(t, p, Config{SessionMode: SessionModeReuse, Timeout: time.Second})
	var out strings.Builder
	r, e := svc.generateWithReconnect(context.Background(), a1ToolReq(), func(v StreamEvent) { out.WriteString(v.Delta) })
	t.Logf("err=%v result=%+v callbacks=%q prompts=%d deleted=%v sticky=%q", e, r, out.String(), len(p.promptTexts()), p.deletedSessions(), svc.stickySessionForTest())
	if e == nil {
		t.Error("Generate swallowed retry transport error")
	}
	if svc.stickySessionForTest() != "" {
		t.Error("failed retry retained sticky session")
	}
	if len(p.promptTexts()) != 2 {
		t.Error("failed retry transparently replayed")
	}
}
func TestA1Completion1005Controls(t *testing.T) {
	for _, mode := range []string{"finish", "tool", "reconnect"} {
		t.Run(mode, func(t *testing.T) {
			p := a1Peer(t)
			p.onPrompt = func(p *ipcPeer, id string, n int) {
				if mode == "reconnect" && n == 1 {
					a1Close(p)
					return
				}
				if mode == "tool" {
					p.chunk(id, a1Action)
				} else {
					p.chunk(id, "answer")
				}
				p.finish(id)
			}
			s := newIPCService(t, p, Config{SessionMode: SessionModeReuse, Timeout: time.Second})
			req := chatRequest("fixture")
			if mode == "tool" {
				req = a1ToolReq()
			}
			r, e := s.Generate(context.Background(), req)
			t.Logf("err=%v result=%+v prompts=%d deleted=%v", e, r, len(p.promptTexts()), p.deletedSessions())
			if e != nil || r == nil {
				t.Fatal("valid turn failed")
			}
			want := 1
			if mode == "reconnect" {
				want = 2
			}
			if len(p.promptTexts()) != want {
				t.Errorf("prompt count != %d", want)
			}
			if mode == "tool" && len(r.ToolCalls) != 1 {
				t.Error("legitimate finished tool lost")
			}
		})
	}
}
