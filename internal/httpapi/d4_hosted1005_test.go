package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/service"
)

// 1005 D-4 (HTTP-09): the proxy turned a hosted web_search declaration into a
// client-side tool_use without executing any search, and the shortcut ran
// before the tool_choice=none check. With no server-side search
// implementation, a hosted declaration must be an explicit request error --
// never a fabricated tool_use -- while none, client-tool declarations,
// tool_result follow-ups and empty queries keep flowing to the model.

func d4StartProxy(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")

	calls := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		inner, err := json.Marshal(map[string]any{
			"choices": []any{map[string]any{
				"index": 0,
				"delta": map[string]any{"content": "plain answer"},
			}},
		})
		if err != nil {
			t.Errorf("marshal inner chunk: %v", err)
			return
		}
		outer, err := json.Marshal(map[string]any{"body": string(inner), "statusCodeValue": 200})
		if err != nil {
			t.Errorf("marshal outer frame: %v", err)
			return
		}
		_, _ = io.WriteString(w, "data: "+string(outer)+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(upstream.Close)

	authFile := filepath.Join(t.TempDir(), "credentials.json")
	credential := `{"source":"test","token_expire_time":"4102444800000","auth":{` +
		`"cosy_key":"cosy-key-value","encrypt_user_info":"encrypted-user-info",` +
		`"user_id":"user-123456","machine_id":"machine-1234567890ab"}}`
	if err := os.WriteFile(authFile, []byte(credential), 0600); err != nil {
		t.Fatal(err)
	}
	svc := service.New(service.Config{
		Backend:        service.BackendRemote,
		RemoteBaseURL:  upstream.URL,
		RemoteAuthFile: authFile,
		Model:          "kimi-k2.6",
		Timeout:        30 * time.Second,
	})
	t.Cleanup(func() { _ = svc.Close() })

	proxy := httptest.NewServer(NewServer("", svc).http.Handler)
	t.Cleanup(proxy.Close)
	return proxy, calls
}

func d4PostMessages(t *testing.T, proxy *httptest.Server, body map[string]any) (int, string) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(data)
}

func d4HostedBody(choice any, messages []any) map[string]any {
	return map[string]any{
		"model":      "claude-3",
		"max_tokens": 64,
		"messages":   messages,
		"tools": []any{map[string]any{
			"type": "web_search_20250305", "name": "web_search",
		}},
		"tool_choice": choice,
	}
}

func TestD4Hosted1005RejectedNotFaked(t *testing.T) {
	proxy, calls := d4StartProxy(t)
	status, body := d4PostMessages(t, proxy, d4HostedBody(nil, []any{
		map[string]any{"role": "user", "content": "Search the web for: qwen release notes"},
	}))
	t.Logf("status=%d body=%s upstream_calls=%d", status, truncateForLog(body), calls.Load())
	if status != http.StatusBadRequest {
		t.Fatalf("HTTP-09: hosted web_search must be an explicit request error, got status %d", status)
	}
	if strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("HTTP-09: hosted web_search still fabricates a client tool_use: %s", truncateForLog(body))
	}
	if !strings.Contains(body, "invalid_request_error") {
		t.Fatalf("expected invalid_request_error, got %s", truncateForLog(body))
	}
	if calls.Load() != 0 {
		t.Fatalf("rejection must precede the model call, upstream saw %d calls", calls.Load())
	}
}

func TestD4Hosted1005NoneChoicePrecedesHostedHandling(t *testing.T) {
	proxy, _ := d4StartProxy(t)
	status, body := d4PostMessages(t, proxy, d4HostedBody(
		map[string]any{"type": "none"},
		[]any{map[string]any{"role": "user", "content": "Search the web for: qwen release notes"}},
	))
	t.Logf("status=%d body=%s", status, truncateForLog(body))
	if status != http.StatusOK {
		t.Fatalf("HTTP-09: tool_choice=none must be honoured before hosted handling, got status %d body %s", status, truncateForLog(body))
	}
	if strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("HTTP-09: none must not leak a fabricated tool_use: %s", truncateForLog(body))
	}
	if !strings.Contains(body, "plain answer") {
		t.Fatalf("none + hosted declaration should still answer from the model, got %s", truncateForLog(body))
	}
}

func TestD4Hosted1005RegularClientWebSearchUnchanged(t *testing.T) {
	proxy, _ := d4StartProxy(t)
	body := map[string]any{
		"model":      "claude-3",
		"max_tokens": 64,
		"messages":   []any{map[string]any{"role": "user", "content": "search the web please"}},
		"tools": []any{map[string]any{
			"name":         "web_search",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}},
		}},
	}
	status, respBody := d4PostMessages(t, proxy, body)
	t.Logf("status=%d body=%s", status, truncateForLog(respBody))
	if status != http.StatusOK {
		t.Fatalf("regular client web_search must keep flowing to the model, got %d: %s", status, truncateForLog(respBody))
	}
	if strings.Contains(respBody, `"type":"tool_use"`) {
		t.Fatalf("regular client tool declaration must not be answered by a shortcut: %s", truncateForLog(respBody))
	}
}

func TestD4Hosted1005ToolResultFollowupUnchanged(t *testing.T) {
	proxy, _ := d4StartProxy(t)
	status, body := d4PostMessages(t, proxy, d4HostedBody(nil, []any{
		map[string]any{"role": "user", "content": "Search the web for: qwen release notes"},
		map[string]any{"role": "assistant", "content": []any{map[string]any{
			"type": "tool_use", "id": "toolu_1", "name": "web_search", "input": map[string]any{"query": "qwen release notes"},
		}}},
		map[string]any{"role": "user", "content": []any{map[string]any{
			"type": "tool_result", "tool_use_id": "toolu_1", "content": "found: qwen 4 released",
		}}},
	}))
	t.Logf("status=%d body=%s", status, truncateForLog(body))
	if status != http.StatusOK {
		t.Fatalf("hosted declaration with a tool_result follow-up must reach the model, got %d: %s", status, truncateForLog(body))
	}
	if strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("follow-up turn must not be shortcut into another tool_use: %s", truncateForLog(body))
	}
}

func TestD4Hosted1005EmptyQueryNotRejected(t *testing.T) {
	proxy, _ := d4StartProxy(t)
	// The text is exactly the cleaned prefix, so the hosted query is empty:
	// the request must flow to the model rather than be rejected.
	status, body := d4PostMessages(t, proxy, d4HostedBody(nil, []any{
		map[string]any{"role": "user", "content": "Web search query:"},
	}))
	t.Logf("status=%d body=%s", status, truncateForLog(body))
	if status != http.StatusOK {
		t.Fatalf("empty cleaned query must not be rejected, got %d: %s", status, truncateForLog(body))
	}
	if strings.Contains(body, `"type":"tool_use"`) {
		t.Fatalf("empty query must not hit the hosted path: %s", truncateForLog(body))
	}
}

func truncateForLog(s string) string {
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}
