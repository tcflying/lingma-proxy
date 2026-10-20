package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestA1Candidate1005MissingFinishBoundaries(t *testing.T) {
	for _, kind := range []string{"whitespace", "mismatch"} {
		t.Run(kind, func(t *testing.T) {
			p := a1Peer(t)
			p.onPrompt = func(p *ipcPeer, id string, _ int) {
				if kind == "whitespace" {
					p.chunk(id, "  ")
					a1Close(p)
				} else {
					p.chunk(id, "partial")
					p.finish(id + "-wrong")
				}
			}
			s := newIPCService(t, p, Config{SessionMode: SessionModeReuse, Timeout: 100 * time.Millisecond})
			result, err := s.Generate(context.Background(), chatRequest("fixture"))
			if err == nil || result != nil || len(p.promptTexts()) != 1 || s.stickySessionForTest() != "" {
				t.Fatalf("incomplete accepted/replayed: result=%v err=%v prompts=%d", result, err, len(p.promptTexts()))
			}
			if kind == "mismatch" && (!errors.Is(err, context.DeadlineExceeded) || len(p.deletedSessions()) != 1) {
				t.Fatalf("wrong finish accepted or cleanup lost: err=%v deleted=%v", err, p.deletedSessions())
			}
		})
	}
}

type a1CandidateTransport func(*http.Request) (*http.Response, error)

func (f a1CandidateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestA1Candidate1005RemoteOuterFallback(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			sandboxProfileDirs(t)
			auth := filepath.Join(t.TempDir(), "synthetic-auth.json")
			if err := os.WriteFile(auth, []byte(`{"source":"synthetic","auth":{"cosy_key":"synthetic-key","encrypt_user_info":"synthetic-info","user_id":"synthetic-user","machine_id":"synthetic-machine"}}`), 0600); err != nil {
				t.Fatal(err)
			}
			old := http.DefaultTransport
			calls := 0
			http.DefaultTransport = a1CandidateTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != "POST" || r.URL.Scheme != "http" || r.URL.Host != "a1.invalid" || r.URL.Path != "/algo/api/v2/service/pro/sse/agent_chat_generation" || r.URL.RawQuery != "FetchKeys=llm_model_result&AgentId=agent_common" {
					return nil, fmt.Errorf("forbidden synthetic target")
				}
				calls++
				if calls > 1 {
					return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("synthetic retry unavailable")), Request: r}, nil
				}
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
				frames := frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ordinary prose"}}}}) + frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}) + "data: [DONE]\n\n"
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames)), Request: r}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = old })
			s := New(Config{Backend: BackendRemote, RemoteBaseURL: "http://a1.invalid", RemoteAuthFile: auth, RemoteFallbackEnabled: true, RemoteFallbackModels: []string{"kmodel", "mmodel"}, Timeout: time.Second})
			t.Cleanup(func() { _ = s.Close() })
			req := a1ToolReq()
			req.Model = "kmodel"
			var callback func(StreamEvent)
			var text strings.Builder
			if stream {
				callback = func(e StreamEvent) { text.WriteString(e.Delta) }
			}
			result, err := s.generateRemote(context.Background(), req, callback)
			if err == nil || result != nil || !isTurnNoReplayError(err) || calls != 2 {
				t.Fatalf("failed retry fell through model fallback: err=%v result=%v calls=%d text=%q", err, result, calls, text.String())
			}
			if stream && text.String() != "ordinary prose" {
				t.Fatalf("deferred output lost: %q", text.String())
			}
		})
	}
}
