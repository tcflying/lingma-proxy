package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/toolemulation"
)

// b3RemoteService wires a Service to a synthetic remote backend through an
// in-memory RoundTripper: no listener, no credentials, no network.
func b3RemoteService(t *testing.T, answer func(call int) string) *Service {
	t.Helper()
	sandboxProfileDirs(t)
	auth := filepath.Join(t.TempDir(), "synthetic-auth.json")
	if err := os.WriteFile(auth, []byte(`{"source":"synthetic","auth":{"cosy_key":"synthetic-key","encrypt_user_info":"synthetic-info","user_id":"synthetic-user","machine_id":"synthetic-machine"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	old := http.DefaultTransport
	calls := 0
	http.DefaultTransport = b1Transport(func(r *http.Request) (*http.Response, error) {
		calls++
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
		content := frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": answer(calls)}}}})
		frames := content + frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}) + "data: [DONE]\n\n"
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = old })
	s := New(Config{Backend: BackendRemote, RemoteBaseURL: "http://b3.invalid", RemoteAuthFile: auth, Timeout: 10 * time.Second})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// b3Blocks builds n distinct, valid Read blocks in the action-block dialect a
// native-tools request can be answered with.
func b3Blocks(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "```json action\n{\"tool\":\"Read\",\"parameters\":{\"path\":\"file_%d.txt\"}}\n```\n", i)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// TestB3Doubleparse1005RemoteDialectBudget is the in-repo rebuild of red
// evidence #1 (TOOLS-04/HTTP-08/HIST-09): the remote dialect answer was parsed
// twice -- convertDialectToolCalls once, applyToolEmulation again with a reset
// budget -- so a turn capped at 5 calls could hand back 6 or 10. One parse per
// attempt must hold: actual calls stay at min(requested, cap), and every
// over-limit block stays visible in the text with the not-sent notice.
func TestB3Doubleparse1005RemoteDialectBudget(t *testing.T) {
	for _, requested := range []int{1, 3, 5, 6, 10, 11} {
		t.Run(fmt.Sprint(requested), func(t *testing.T) {
			s := b3RemoteService(t, func(int) string { return b3Blocks(requested) })
			req := ChatRequest{Model: "kmodel", Messages: []ChatMessage{{Role: "user", Text: "read the files"}}, Tools: b1Tools(), ToolChoice: toolemulation.ToolChoice{Mode: "auto"}}
			result, err := s.Generate(context.Background(), req)
			if err != nil {
				t.Fatalf("turn failed: %v", err)
			}
			want := requested
			if want > 5 {
				want = 5
			}
			t.Logf("requested=%d actual_calls=%d text=%q", requested, len(result.ToolCalls), result.Text)
			if len(result.ToolCalls) != want {
				t.Fatalf("requested=%d actual_calls=%d want %d", requested, len(result.ToolCalls), want)
			}
			if requested > 5 {
				if !strings.Contains(result.Text, fmt.Sprintf("file_%d.txt", want+1)) {
					t.Fatalf("first over-limit block must stay visible, text=%q", result.Text)
				}
				if !strings.Contains(result.Text, "not sent") {
					t.Fatalf("over-limit notice missing, text=%q", result.Text)
				}
			} else if strings.Contains(result.Text, "not sent") {
				t.Fatalf("in-budget turn must not claim suppressed calls, text=%q", result.Text)
			}
		})
	}
}

// TestB3Doubleparse1005RetryReplaysAcceptedAttempt is HTTP-07: after the
// forced-tooling retry succeeds, the accepted attempt's prose used to vanish
// from the stream -- the first attempt's held deltas were dropped and nothing
// replaced them. The accepted attempt's text must reach onDelta exactly once,
// and the superseded first attempt must not be replayed.
func TestB3Doubleparse1005RetryReplaysAcceptedAttempt(t *testing.T) {
	s := b3RemoteService(t, func(call int) string {
		if call == 1 {
			return "I don't have tools for this."
		}
		return "accepted attempt prose\n```json action\n{\"tool\":\"Read\",\"parameters\":{\"path\":\"a.txt\"}}\n```"
	})
	req := ChatRequest{Model: "kmodel", Messages: []ChatMessage{{Role: "user", Text: "read the file"}}, Tools: b1Tools(), ToolChoice: toolemulation.ToolChoice{Mode: "any"}}
	var streamed strings.Builder
	result, err := s.generateRemote(context.Background(), req, func(event StreamEvent) { streamed.WriteString(event.Delta) })
	if err != nil {
		t.Fatalf("turn failed: %v", err)
	}
	t.Logf("streamed=%q result_text=%q calls=%d", streamed.String(), result.Text, len(result.ToolCalls))
	if len(result.ToolCalls) != 1 {
		t.Fatalf("want the retried call accepted, got %+v", result.ToolCalls)
	}
	if !strings.Contains(streamed.String(), "accepted attempt prose") {
		t.Fatalf("accepted attempt prose lost from the stream: %q", streamed.String())
	}
	if strings.Contains(streamed.String(), "I don't have tools") {
		t.Fatalf("superseded first attempt replayed: %q", streamed.String())
	}
	if streamed.String() != result.Text {
		t.Fatalf("stream must equal the accepted attempt's text: %q vs %q", streamed.String(), result.Text)
	}
}
