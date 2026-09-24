package httpapi

import (
	"bytes"
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

// The assistant turn the stub gateway streams. Its tail is an unterminated
// ```json action opening, which is exactly what toolStreamFilter holds back while
// it waits to learn whether an action block follows: only filter.Flush() at end of
// turn gives it back to the client as prose. Dropping that call left the
// response.output_text.delta stream short while response.completed kept reporting
// the whole text, so the truncation was invisible to any test that looked only at
// the final frame.
const (
	streamGuardProse     = "最终答案：本题 42 分。"
	streamGuardHeldTail  = "顺便贴一段还没写完的示例\n```json\n{\"tool\":\"Bash\",\"parameters\":{\"command\":\"ls -la\""
	streamGuardAnswer    = streamGuardProse + streamGuardHeldTail
	streamGuardToolName  = "Bash"
	streamGuardUserTurn  = "这道题的答案是多少？"
	streamGuardModelName = "kmodel"
)

// streamGuardUpstreamFrame envelops one OpenAI-style chunk the way the Lingma
// gateway does: the frame's body is the JSON text of the chunk.
func streamGuardUpstreamFrame(t *testing.T, content string) string {
	t.Helper()
	inner, err := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"content": content},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal(map[string]any{"body": string(inner), "statusCodeValue": 200})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(outer) + "\n\n"
}

// TestOpenAIResponsesStreamFlushesWithheldTailToDeltas guards the filter.Flush()
// call in handleOpenAIResponsesStream by driving a real streamed /v1/responses
// request end to end: stub gateway -> remote client -> service -> HTTP handler.
// The filter unit tests call Flush themselves and the completed-frame tests build
// the final frame by hand, so neither reaches that call; this one does.
func TestOpenAIResponsesStreamFlushesWithheldTailToDeltas(t *testing.T) {
	// The aggregate path replaces the whole streamed branch, so the guard would
	// silently stop covering Flush() if the machine opted into it.
	t.Setenv("LINGMA_AGGREGATE_TOOL_STREAM", "")
	// The remote client dials this loopback URL itself; a proxy from the
	// environment would turn the stub into an unreachable host.
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		// Split so the fence opens on its own delta and its (missing) body arrives
		// on the next: the withheld tail has to survive several pushes, not one.
		for _, piece := range []string{streamGuardProse, "顺便贴一段还没写完的示例\n```json\n", "{\"tool\":\"Bash\",\"parameters\":{\"command\":\"ls -la\""} {
			if _, err := io.WriteString(w, streamGuardUpstreamFrame(t, piece)); err != nil {
				t.Errorf("write upstream frame: %v", err)
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	// A credentials.json in a temp dir keeps the remote backend off the real login
	// cache, and an explicit RemoteAuthFile keeps it off the CLI/PATH scan.
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
		Model:          streamGuardModelName,
		Timeout:        30 * time.Second,
	})
	t.Cleanup(func() { _ = svc.Close() })

	proxy := httptest.NewServer(NewServer("", svc).http.Handler)
	defer proxy.Close()

	requestBody, err := json.Marshal(map[string]any{
		"model":       streamGuardModelName,
		"stream":      true,
		"input":       streamGuardUserTurn,
		"tool_choice": "auto",
		"tools": []any{map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": streamGuardToolName,
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"command": map[string]any{"type": "string"}},
					"required":   []any{"command"},
				},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, raw)
	}
	body := string(raw)
	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("the stream never completed: %s", body)
	}
	// One turn only: a second upstream call would mean the missed-tool-use retry
	// fired, and then the final text no longer equals what was streamed.
	if n := upstreamCalls.Load(); n != 1 {
		t.Fatalf("upstream answered %d turns, want 1 (fixture tripped a tool retry): %s", n, body)
	}

	// The load-bearing assertion: every character of the answer has to reach the
	// delta stream, including the tail the filter was still holding.
	deltas := strings.Join(sseDeltaTexts(t, body, "response.output_text.delta"), "")
	if deltas != streamGuardAnswer {
		t.Fatalf("response.output_text.delta carried %q, want the whole answer %q: the withheld tail never reached the client", deltas, streamGuardAnswer)
	}

	// And the two views agree: response.completed reports the same text, which is
	// what made the original truncation invisible from the final frame alone.
	if completed := streamGuardCompletedOutputText(t, body); completed != streamGuardAnswer {
		t.Fatalf("response.completed output_text = %q, want %q (fixture drift, not the Flush defect)", completed, streamGuardAnswer)
	}
}

func streamGuardCompletedOutputText(t *testing.T, body string) string {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if line != "event: response.completed" || i+1 >= len(lines) {
			continue
		}
		var payload struct {
			Response struct {
				OutputText string `json:"output_text"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[i+1], "data: ")), &payload); err != nil {
			t.Fatalf("response.completed payload is not JSON: %v", err)
		}
		return payload.Response.OutputText
	}
	t.Fatal("no response.completed event in the stream")
	return ""
}
