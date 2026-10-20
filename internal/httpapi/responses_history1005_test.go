package httpapi

import (
	"encoding/json"
	"fmt"
	"io"
	"lingma-ipc-proxy/internal/service"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type history1005Transport func(*http.Request) (*http.Response, error)

func (f history1005Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type history1005Fixture struct {
	server   *Server
	requests []string
	images   int
}

func newHistory1005Fixture(t *testing.T) *history1005Fixture {
	t.Helper()
	t.Setenv("LINGMA_PROXY_MAX_CONCURRENT", "1")
	f := &history1005Fixture{}
	frames := hybrid931ContentFrame(t, "synthetic-history-answer") + hybrid931FinishFrame(t) + "data: [DONE]\n\n"
	oldTransport, oldImageClient := http.DefaultTransport, imageFetchClient
	http.DefaultTransport = history1005Transport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Scheme != "http" || r.URL.Host != "history1005.invalid" || r.URL.Path != "/algo/api/v2/service/pro/sse/agent_chat_generation" || r.URL.RawQuery != "FetchKeys=llm_model_result&AgentId=agent_common" {
			return nil, fmt.Errorf("unexpected synthetic request target")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		f.requests = append(f.requests, string(body))
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames)), Request: r}, nil
	})
	imageFetchClient = &http.Client{Transport: history1005Transport(func(*http.Request) (*http.Response, error) {
		f.images++
		return nil, fmt.Errorf("synthetic image fetch forbidden")
	})}
	t.Cleanup(func() { http.DefaultTransport = oldTransport; imageFetchClient = oldImageClient })
	authFile := filepath.Join(t.TempDir(), "synthetic-auth.json")
	if err := os.WriteFile(authFile, []byte(`{"source":"synthetic-history1005","auth":{"cosy_key":"synthetic-key","encrypt_user_info":"synthetic-info","user_id":"synthetic-user","machine_id":"synthetic-machine"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	svc := service.New(service.Config{Backend: service.BackendRemote, RemoteBaseURL: "http://history1005.invalid", RemoteAuthFile: authFile, Model: "kmodel", Timeout: time.Second})
	t.Cleanup(func() { _ = svc.Close() })
	f.server = NewServer("", svc)
	return f
}
func (f *history1005Fixture) post(t *testing.T, path string, body map[string]any) *httptest.ResponseRecorder {
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

func TestResponsesHistory1005GeneratedID(t *testing.T) {
	f := newHistory1005Fixture(t)
	first := f.post(t, "/v1/responses", map[string]any{"model": "kmodel", "input": "original-task-marker"})
	if first.Code != 200 {
		t.Fatalf("first status=%d", first.Code)
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &result); err != nil || result.ID == "" {
		t.Fatal("first response did not generate an ID")
	}
	second := f.post(t, "/v1/responses", map[string]any{"model": "kmodel", "input": "followup-marker", "previous_response_id": result.ID})
	if second.Code != 400 || len(f.requests) != 1 {
		lost := len(f.requests) > 1 && !strings.Contains(f.requests[1], "original-task-marker")
		t.Fatalf("continuation status=%d upstream_calls=%d original_task_missing=%v; want 400 and no second upstream request", second.Code, len(f.requests), lost)
	}
	if !strings.Contains(second.Body.String(), "previous_response_id") || !strings.Contains(second.Body.String(), "invalid_request_error") {
		t.Fatalf("rejection lacks explicit error: %s", second.Body.String())
	}
}

func TestResponsesHistory1005RejectBeforeProjection(t *testing.T) {
	f := newHistory1005Fixture(t)
	values := []any{"resp-synthetic", "", "   ", 0, false, []any{}, map[string]any{}}
	for _, path := range []string{"/v1/responses", "/api/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, value := range values {
				before := len(f.requests)
				w := f.post(t, path, map[string]any{"model": "kmodel", "input": "synthetic prompt", "stream": stream, "previous_response_id": value})
				if w.Code != 400 || len(f.requests) != before || !strings.Contains(w.Body.String(), "previous_response_id") || strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") {
					t.Errorf("%s stream=%v value=%#v status=%d extra_upstream=%d", path, stream, value, w.Code, len(f.requests)-before)
				}
			}
			beforeImage := len(f.requests)
			w := f.post(t, path, map[string]any{"model": "kmodel", "stream": stream, "previous_response_id": "resp-image", "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "http://127.0.0.1/synthetic-forbidden-image"}}}}})
			if w.Code != 400 || f.images != 0 || len(f.requests) != beforeImage || !strings.Contains(w.Body.String(), "previous_response_id") {
				t.Errorf("reject must precede image projection: status=%d image_reads=%d", w.Code, f.images)
			}
			// Rejections must leave the only slot available to an ordinary request.
			good := f.post(t, path, map[string]any{"model": "kmodel", "input": "after-rejection"})
			if good.Code != 200 {
				t.Fatalf("request after rejection status=%d", good.Code)
			}
		}
	}
}

func TestResponsesHistory1005StatelessInputs(t *testing.T) {
	f := newHistory1005Fixture(t)
	for _, path := range []string{"/v1/responses", "/api/v1/responses"} {
		for _, stream := range []bool{false, true} {
			for _, explicitNull := range []bool{false, true} {
				body := map[string]any{"model": "kmodel", "stream": stream, "future_compatible_field": true, "input": []any{
					map[string]any{"role": "user", "content": "original-task-marker"},
					map[string]any{"role": "assistant", "content": "prior-answer-marker"},
					map[string]any{"role": "user", "content": "followup-marker"},
				}}
				if explicitNull {
					body["previous_response_id"] = nil
				}
				before := len(f.requests)
				w := f.post(t, path, body)
				if w.Code != 200 || len(f.requests) != before+1 {
					t.Fatalf("stateless path=%s stream=%v null=%v status=%d", path, stream, explicitNull, w.Code)
				}
				for _, marker := range []string{"original-task-marker", "prior-answer-marker", "followup-marker"} {
					if !strings.Contains(f.requests[before], marker) {
						t.Errorf("upstream lost %s", marker)
					}
				}
				if !strings.Contains(w.Body.String(), "synthetic-history-answer") {
					t.Error("synthetic output missing")
				}
			}
		}
	}
}
