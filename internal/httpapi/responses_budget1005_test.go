package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"lingma-ipc-proxy/internal/service"
	"lingma-ipc-proxy/internal/toolemulation"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type budget1005Transport func(*http.Request) (*http.Response, error)

func (f budget1005Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type budget1005Fixture struct {
	server   *Server
	requests []string
	images   int
}

func newBudget1005Fixture(t *testing.T, reason string) *budget1005Fixture {
	t.Helper()
	t.Setenv("LINGMA_PROXY_MAX_CONCURRENT", "1")
	f := &budget1005Fixture{}
	frames := hybrid931ContentFrame(t, "partial-budget-answer") + hybrid931RawFrame(t, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}}, "usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 29}}) + "data: [DONE]\n\n"
	oldTransport, oldImageClient := http.DefaultTransport, imageFetchClient
	http.DefaultTransport = budget1005Transport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Scheme != "http" || r.URL.Host != "budget1005.invalid" || r.URL.Path != "/algo/api/v2/service/pro/sse/agent_chat_generation" || r.URL.RawQuery != "FetchKeys=llm_model_result&AgentId=agent_common" {
			return nil, fmt.Errorf("unexpected synthetic request target")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		f.requests = append(f.requests, string(body))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames)), Request: r}, nil
	})
	imageFetchClient = &http.Client{Transport: budget1005Transport(func(*http.Request) (*http.Response, error) {
		f.images++
		return nil, fmt.Errorf("synthetic image fetch forbidden")
	})}
	t.Cleanup(func() { http.DefaultTransport = oldTransport; imageFetchClient = oldImageClient })
	authFile := filepath.Join(t.TempDir(), "synthetic-auth.json")
	if err := os.WriteFile(authFile, []byte(`{"source":"synthetic-budget1005","auth":{"cosy_key":"synthetic-key","encrypt_user_info":"synthetic-info","user_id":"synthetic-user","machine_id":"synthetic-machine"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	svc := service.New(service.Config{Backend: service.BackendRemote, RemoteBaseURL: "http://budget1005.invalid", RemoteAuthFile: authFile, Model: "kmodel", Timeout: time.Second})
	t.Cleanup(func() { _ = svc.Close() })
	f.server = NewServer("", svc)
	return f
}
func (f *budget1005Fixture) post(t *testing.T, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	f.server.http.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw))))
	if len(f.server.sem) != 0 {
		t.Fatalf("request leaked %d execution slots", len(f.server.sem))
	}
	return w
}

func budget1005AssertBody(t *testing.T, body map[string]any, status string) {
	t.Helper()
	if body["status"] != status || body["output_text"] != "partial-budget-answer" {
		t.Fatalf("status/text: %#v", body)
	}
	if status == "incomplete" {
		d, ok := body["incomplete_details"].(map[string]any)
		if !ok || d["reason"] != "max_output_tokens" {
			t.Fatalf("missing budget reason: %#v", body)
		}
	}
	usage := body["usage"].(map[string]any)
	if usage["input_tokens"] != float64(11) || usage["output_tokens"] != float64(29) {
		t.Fatalf("usage lost: %#v", usage)
	}
	output := body["output"].([]any)
	if len(output) != 1 || output[0].(map[string]any)["status"] != status {
		t.Fatalf("message status diverges: %#v", output)
	}
}

func TestResponsesBudget1005Handler(t *testing.T) {
	for _, reason := range []string{"length", "stop"} {
		for _, path := range []string{"/v1/responses", "/api/v1/responses"} {
			for _, mode := range []string{"nonstream", "direct", "aggregate"} {
				t.Run(reason+path+mode, func(t *testing.T) {
					f := newBudget1005Fixture(t, reason)
					t.Setenv("LINGMA_AGGREGATE_TOOL_STREAM", "0")
					request := map[string]any{"model": "kmodel", "input": "synthetic budget prompt", "stream": mode != "nonstream"}
					if mode == "aggregate" {
						t.Setenv("LINGMA_AGGREGATE_TOOL_STREAM", "1")
						request["tools"] = []any{map[string]any{"type": "function", "name": "synthetic_tool", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}
						request["tool_choice"] = "none"
					}
					w := f.post(t, path, request)
					if w.Code != 200 || len(f.requests) != 1 || f.images != 0 {
						t.Fatalf("status=%d requests=%d images=%d", w.Code, len(f.requests), f.images)
					}
					status := "completed"
					if reason == "length" {
						status = "incomplete"
					}
					if mode == "nonstream" {
						var body map[string]any
						if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
							t.Fatal(err)
						}
						budget1005AssertBody(t, body, status)
						return
					}
					terminal := 0
					lastSeq := float64(-1)
					id := ""
					delta := ""
					var doneItem map[string]any
					var final map[string]any
					for _, frame := range strings.Split(w.Body.String(), "\n\n") {
						if !strings.HasPrefix(frame, "event: ") {
							continue
						}
						lines := strings.Split(frame, "\n")
						if len(lines) != 2 || !strings.HasPrefix(lines[1], "data: ") {
							t.Fatalf("malformed frame %q", frame)
						}
						name := strings.TrimPrefix(lines[0], "event: ")
						var data map[string]any
						if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &data); err != nil {
							t.Fatal(err)
						}
						if data["type"] != name {
							t.Fatalf("event/type mismatch %s", name)
						}
						seq := data["sequence_number"].(float64)
						if seq <= lastSeq {
							t.Fatal("sequence regressed")
						}
						lastSeq = seq
						if name == "response.created" {
							id = data["response"].(map[string]any)["id"].(string)
						}
						if name == "response.output_text.delta" {
							delta += data["delta"].(string)
						}
						if name == "response.output_item.done" {
							doneItem = data["item"].(map[string]any)
						}
						if name == "response.completed" || name == "response.incomplete" {
							terminal++
							if name != "response."+status {
								t.Fatalf("wrong terminal %s", name)
							}
							final = data["response"].(map[string]any)
							if final["id"] != id {
								t.Fatal("response identity changed")
							}
						}
					}
					if terminal != 1 || strings.Count(w.Body.String(), "data: [DONE]") != 1 {
						t.Fatalf("terminal=%d", terminal)
					}
					budget1005AssertBody(t, final, status)
					if delta != final["output_text"] {
						t.Fatal("delta text diverges")
					}
					if !reflect.DeepEqual(doneItem, final["output"].([]any)[0]) {
						t.Fatal("done item diverges from final output")
					}
				})
			}
		}
	}
}

func TestResponsesBudget1005Projection(t *testing.T) {
	for _, stop := range []string{"length", "max_tokens"} {
		result := &service.ChatResult{Text: "partial-budget-answer", FinishReason: stop, InputTokens: 11, OutputTokens: 29}
		if stop == "max_tokens" {
			result.FinishReason = ""
			result.StopReason = stop
		}
		b := buildOpenAIResponseBody("resp_fixture", 1, "kmodel", result, "msg_fixture", false)
		if b["status"] != "incomplete" {
			t.Fatalf("%s status=%v", stop, b["status"])
		}
		result.ToolCalls = []toolemulation.ToolCall{{ID: "call_fixture", Name: "synthetic_tool", Arguments: map[string]any{}}}
		b = buildOpenAIResponseBody("resp_fixture", 1, "kmodel", result, "msg_fixture", false)
		if b["status"] != "completed" || b["incomplete_details"] != nil {
			t.Fatal("legal tool handoff precedence changed")
		}
		rec := httptest.NewRecorder()
		emitter := newOpenAIResponseStreamEmitter(rec, rec, "resp_fixture")
		writeOpenAIResponseStreamCompleted(emitter, "resp_fixture", 1, "kmodel", result, "msg_fixture", false, false, false, "")
		if strings.Count(rec.Body.String(), "event: response.completed\n") != 1 || strings.Contains(rec.Body.String(), "response.incomplete") {
			t.Fatal("mixed tool/budget stream changed handoff terminal")
		}

	}
}

func TestResponsesBudget1005Heartbeat(t *testing.T) {
	for _, serial := range []bool{false, true} {
		w := httptest.NewRecorder()
		e := newOpenAIResponseStreamEmitter(w, w, "resp_fixture")
		if serial {
			e.serial = func(name string, p map[string]any, after func()) error {
				e.mu.Lock()
				defer e.mu.Unlock()
				frame, err := e.marshalEventLocked(name, p)
				if err != nil {
					return err
				}
				after()
				_, err = io.WriteString(w, frame)
				return err
			}
		}
		if err := e.Event("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_fixture", "status": "in_progress"}}); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.heartbeatFrame(); !ok {
			t.Fatal("heartbeat missing before terminal")
		}
		if err := e.Event("response.incomplete", map[string]any{"type": "response.incomplete", "response": map[string]any{"id": "resp_fixture", "status": "incomplete"}}); err != nil {
			t.Fatal(err)
		}
		if _, ok := e.heartbeatFrame(); ok {
			t.Fatalf("heartbeat after incomplete serial=%v", serial)
		}
	}
}
