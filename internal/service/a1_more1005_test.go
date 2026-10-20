package service

import (
	"context"
	"errors"
	"lingma-ipc-proxy/internal/remote"
	"testing"
	"time"
)

func TestA1Completion1005RetryErrorNoCallback(t *testing.T) {
	p := a1Peer(t)
	p.onPrompt = func(p *ipcPeer, id string, n int) {
		if n == 1 {
			p.chunk(id, "first prose")
			p.finish(id)
		} else {
			a1Close(p)
		}
	}
	s := newIPCService(t, p, Config{SessionMode: SessionModeReuse, Timeout: time.Second})
	r, e := s.Generate(context.Background(), a1ToolReq())
	t.Logf("err=%v result=%+v prompts=%d sticky=%q", e, r, len(p.promptTexts()), s.stickySessionForTest())
	if e == nil {
		t.Error("retryErr must fail entire turn even without callback")
	}
	if len(p.promptTexts()) != 2 {
		t.Error("nonstream retry error triggered whole-turn replay")
	}
	if s.stickySessionForTest() != "" {
		t.Error("sticky not abandoned")
	}
}
func TestA1Completion1005FinishBoundary(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "finish_then_close", true: "empty_finish_data"}[empty], func(t *testing.T) {
			p := a1Peer(t)
			p.onPrompt = func(p *ipcPeer, id string, n int) {
				p.chunk(id, "answer")
				if empty {
					p.update(id, map[string]any{"sessionUpdate": "notification", "type": "chat_finish", "data": map[string]any{}})
				} else {
					p.finish(id)
				}
				a1Close(p)
			}
			s := newIPCService(t, p, Config{Timeout: time.Second})
			r, e := s.Generate(context.Background(), chatRequest("fixture"))
			t.Logf("err=%v result=%+v prompts=%d", e, r, len(p.promptTexts()))
			if e != nil || r == nil || r.Text != "answer" || len(p.promptTexts()) != 1 {
				t.Error("queued matching finish was rejected")
			}
		})
	}
}

type a1FakeChat struct{ calls int }

func (c *a1FakeChat) Chat(_ context.Context, _ remote.ChatRequest, d func(string)) (*remote.ChatResult, error) {
	c.calls++
	if c.calls > 1 {
		return nil, errors.New("synthetic retry error")
	}
	if d != nil {
		d("ordinary prose")
	}
	return &remote.ChatResult{Text: "ordinary prose"}, nil
}
func (c *a1FakeChat) ListModels(context.Context) ([]remote.Model, error) {
	return nil, errors.New("forbidden discovery")
}
func TestA1Completion1005SharedRemoteRetryError(t *testing.T) {
	s := &Service{cfg: Config{Backend: BackendRemote, RemoteAuthFile: "synthetic-unused", RemoteBaseURL: "https://synthetic.invalid"}}
	c := &a1FakeChat{}
	r, emitted, e := s.generateRemoteWithModel(context.Background(), c, a1ToolReq(), "fixture", "fixture", "synthetic", nil, true)
	t.Logf("err=%v result=%+v emitted=%v chatCalls=%d", e, r, emitted, c.calls)
	if e == nil {
		t.Error("shared emulation retry error swallowed in remote caller")
	}
	if c.calls != 2 {
		t.Error("expected exactly original plus emulation retry")
	}
}
