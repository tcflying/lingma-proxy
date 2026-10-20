package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"lingma-ipc-proxy/internal/lingmaipc"
	"lingma-ipc-proxy/internal/service"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestA1Candidate1005HTTPCancellation(t *testing.T) {
	for _, tc := range []struct {
		path      string
		stream    bool
		aggregate bool
	}{{"/v1/chat/completions", true, false}, {"/v1/responses", true, false}, {"/v1/responses", false, false}, {"/v1/messages", true, false}, {"/v1/responses", true, true}} {
		t.Run(fmt.Sprintf("%s/stream%v/aggregate%v", tc.path, tc.stream, tc.aggregate), func(t *testing.T) {
			ctx, cancelTurn := context.WithCancel(context.Background())
			defer cancelTurn()
			var mu sync.Mutex
			var conns []*websocket.Conn
			prompts := 0
			deleted := 0
			deletedDone := make(chan struct{}, 1)
			up := websocket.Upgrader{}
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, e := up.Upgrade(w, r, nil)
				if e != nil {
					return
				}
				mu.Lock()
				conns = append(conns, c)
				mu.Unlock()
				defer c.Close()
				send := func(v any) {
					b, _ := json.Marshal(v)
					_ = c.WriteMessage(websocket.TextMessage, append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(b))), b...))
				}
				for {
					_, b, e := c.ReadMessage()
					if e != nil {
						return
					}
					i := bytes.Index(b, []byte("\r\n\r\n"))
					if i < 0 {
						return
					}
					var q struct {
						ID     *int           `json:"id"`
						Method string         `json:"method"`
						Params map[string]any `json:"params"`
					}
					if json.Unmarshal(b[i+4:], &q) != nil {
						return
					}
					if q.Method == "session/prompt" {
						mu.Lock()
						prompts++
						mu.Unlock()
						meta, _ := q.Params["_meta"].(map[string]any)
						send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"_meta": meta, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"text": "wire-partial"}}}})
						cancelTurn()
						continue
					}
					if q.Method == "chat/deleteSessionById" {
						mu.Lock()
						deleted++
						mu.Unlock()
						select {
						case deletedDone <- struct{}{}:
						default:
						}
					}
					if q.ID != nil {
						result := map[string]any{"ok": true}
						if q.Method == "session/new" {
							result = map[string]any{"sessionId": "a1-http-synthetic"}
						}
						send(map[string]any{"jsonrpc": "2.0", "id": *q.ID, "result": result})
					}
				}
			}))
			defer peer.Close()
			defer func() {
				mu.Lock()
				defer mu.Unlock()
				for _, c := range conns {
					_ = c.Close()
				}
			}()
			svc := service.New(service.Config{SessionMode: service.SessionModeReuse, Backend: service.BackendIPC, Transport: lingmaipc.TransportWebSocket, WebSocketURL: "ws" + strings.TrimPrefix(peer.URL, "http"), Timeout: time.Second})
			defer svc.Close()
			server := NewServer("", svc)
			body := map[string]any{"model": "synthetic", "stream": tc.stream, "messages": []any{map[string]any{"role": "user", "content": "fixture"}}, "input": "fixture", "max_tokens": 10}
			if tc.aggregate {
				t.Setenv("LINGMA_AGGREGATE_TOOL_STREAM", "true")
				body["tools"] = []any{map[string]any{"type": "function", "name": "Read", "parameters": map[string]any{"type": "object"}}}
				body["tool_choice"] = "none"
			}
			raw, _ := json.Marshal(body)
			req := httptest.NewRequest("POST", tc.path, bytes.NewReader(raw))
			req = req.WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			server.http.Handler.ServeHTTP(rec, req)
			wire := rec.Body.String()
			mu.Lock()
			n := prompts
			mu.Unlock()
			t.Logf("status=%d prompts=%d body=%s", rec.Code, n, wire)
			if ctx.Err() == nil {
				t.Error("cancellation did not occur")
			}
			select {
			case <-deletedDone:
			case <-time.After(2 * time.Second):
				t.Error("cleanup did not reach fake peer")
			}
			mu.Lock()
			d := deleted
			mu.Unlock()
			if d != 1 {
				t.Errorf("cancelled session delete count=%d", d)
			}
			for _, bad := range []string{"response.completed", "\"status\":\"completed\"", "\"finish_reason\":\"stop\"", "\"stop_reason\":\"end_turn\""} {
				if strings.Contains(wire, bad) {
					t.Errorf("false success terminal %s", bad)
				}
			}
			if n != 1 {
				t.Error("partial request replayed")
			}
		})
	}
}
