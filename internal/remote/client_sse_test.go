package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// R1: an expired gateway session 302s to a login page that answers 200 with HTML,
// and http.Client follows the redirect. Zero "data:" lines must never be reported
// as a successful empty completion.
func TestChatTreatsRedirectedHTMLLoginPageAsTransient(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/login") {
			w.Header().Set("Content-Type", "text/html")
			io.WriteString(w, "<!doctype html><html><body>Sign in to Lingma</body></html>")
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer server.Close()

	client := chatTestClient(t, server.URL)
	result, err := client.Chat(context.Background(), ChatRequest{Model: "kmodel", Prompt: "hi"}, nil)
	if err == nil {
		t.Fatalf("empty HTML body reported as success: result=%#v", result)
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil", result)
	}
	if !errors.Is(err, ErrTransientUpstream) {
		t.Fatalf("error %q does not wrap ErrTransientUpstream", err)
	}
}

// R1 control: a stream cut at a line boundary (no data: line at all) is the same
// dead end, while a stream the gateway terminated -- either with a bare [DONE] or
// with the marker inside the envelope -- stays a success.
func TestChatDistinguishesTruncatedStreamFromCleanEnd(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantText string
		wantErr  bool
	}{
		{name: "clean end", body: sseChunk(t, "ok") + "data: [DONE]\n\n", wantText: "ok"},
		{name: "done inside envelope", body: `data: {"body":"[DONE]","statusCodeValue":200}` + "\n\n"},
		{name: "cut at line boundary", body: "\n\n", wantErr: true},
		{name: "only keepalive comments", body: ": ping\n\n", wantErr: true},
		// R1: content chunks alone are not a terminator. Before the predicate asked
		// for a finish_reason, this stream was reported as a complete answer.
		{name: "content with no terminator", body: sseChunk(t, "half an answer"), wantErr: true},
		// The other direction: a gateway that ends the turn with finish_reason and
		// no [DONE] marker is still a success, so the guard must not over-tighten.
		{name: "finish_reason without done marker", body: sseFrame(t, `{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`), wantText: "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			result, err := chatTestClient(t, server.URL).Chat(context.Background(), ChatRequest{Model: "kmodel", Prompt: "hi"}, nil)
			if tc.wantErr {
				if !errors.Is(err, ErrTransientUpstream) {
					t.Fatalf("error %q does not wrap ErrTransientUpstream", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Chat() = %v, want success", err)
			}
			if result.Text != tc.wantText {
				t.Fatalf("text = %q, want %q", result.Text, tc.wantText)
			}
		})
	}
}

func sseChunk(t *testing.T, content string) string {
	t.Helper()
	return sseFrame(t, contentChunk(content))
}

// R2: ChatResult.StopReason is what lets the API layer tell a budget stop apart
// from a finished answer, so the frame's finish_reason has to reach it.
func TestChatCarriesBackendFinishReason(t *testing.T) {
	cases := []struct {
		name         string
		finishReason string
		want         string
	}{
		{name: "budget stop", finishReason: "length", want: "max_tokens"},
		{name: "tool call stop", finishReason: "tool_calls", want: "tool_calls"},
		{name: "plain stop", finishReason: "stop", want: "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := sseFrame(t, contentChunk("partial answer")) +
				sseFrame(t, `{"choices":[{"delta":{"content":" tail"},"finish_reason":"`+tc.finishReason+`"}]}`) +
				"data: [DONE]\n\n"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, body)
			}))
			defer server.Close()
			result, err := chatTestClient(t, server.URL).Chat(context.Background(), ChatRequest{Model: "kmodel", Prompt: "hi"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if result.StopReason != tc.want {
				t.Fatalf("StopReason = %q, want %q", result.StopReason, tc.want)
			}
			if result.Text != "partial answer tail" {
				t.Fatalf("text = %q", result.Text)
			}
		})
	}
}

// R6: the statuses a retry can clear must carry the sentinel, or the API layer
// answers a generic 500 api_error that clients never retry.
func TestChatWrapsTransientGatewayStatuses(t *testing.T) {
	for _, status := range []int{http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, "upstream busy")
			}))
			defer server.Close()
			result, err := chatTestClient(t, server.URL).Chat(context.Background(), ChatRequest{Model: "kmodel", Prompt: "hi"}, nil)
			if result != nil {
				t.Fatalf("result = %#v, want nil", result)
			}
			if !errors.Is(err, ErrTransientUpstream) {
				t.Fatalf("status %d: error %q does not wrap ErrTransientUpstream", status, err)
			}
			if !strings.Contains(err.Error(), fmt.Sprint(status)) {
				t.Fatalf("status %d missing from error %q", status, err)
			}
		})
	}
}

// R6 counterpart: a rejected credential is not transient, and answering 503 would
// make clients hammer a dead login.
func TestChatKeepsAuthStatusesFatal(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				io.WriteString(w, "token expired")
			}))
			defer server.Close()
			_, err := chatTestClient(t, server.URL).Chat(context.Background(), ChatRequest{Model: "kmodel", Prompt: "hi"}, nil)
			if err == nil {
				t.Fatal("expected error")
			}
			if errors.Is(err, ErrTransientUpstream) {
				t.Fatalf("status %d must not be reported as retryable: %q", status, err)
			}
		})
	}
}

// R6 covers the same predicate at the two sibling call sites the HTTP-layer fix
// missed: the gateway reports most per-turn failures inside a 200 SSE envelope, and
// /v1/models answers through the API layer's retryable check. A 429 that reaches
// either as a bare error reads as a permanent 500 to clients.
func TestEnvelopeAndModelListStatusesUseTheTransientSentinel(t *testing.T) {
	for _, tc := range []struct {
		status    int
		transient bool
	}{
		{status: http.StatusRequestTimeout, transient: true},
		{status: http.StatusTooManyRequests, transient: true},
		{status: http.StatusInternalServerError, transient: true},
		{status: http.StatusServiceUnavailable, transient: true},
		{status: http.StatusBadRequest, transient: false},
		{status: http.StatusUnauthorized, transient: false},
		{status: http.StatusForbidden, transient: false},
		{status: http.StatusNotFound, transient: false},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			payload, err := json.Marshal(outerSSE{Body: "upstream said no", StatusCode: tc.status})
			if err != nil {
				t.Fatal(err)
			}
			_, _, envelopeErr := parseSSEPayload(string(payload))
			if envelopeErr == nil {
				t.Fatalf("envelope status %d accepted", tc.status)
			}
			listErr := (&Client{}).modelListStatusError("https://example.invalid", tc.status, "upstream said no")
			for _, got := range []error{envelopeErr, listErr} {
				if errors.Is(got, ErrTransientUpstream) != tc.transient {
					t.Fatalf("status %d retryable = %v, want %v (%q)", tc.status, errors.Is(got, ErrTransientUpstream), tc.transient, got)
				}
				if !strings.Contains(got.Error(), fmt.Sprint(tc.status)) {
					t.Fatalf("status %d missing from %q", tc.status, got)
				}
			}
		})
	}
}

// R8: upstream counts beat the len(runes)/4 estimate, which undercounts Chinese by
// about four times while model_usage bills on the number.
func TestChatPrefersUpstreamUsageOverEstimate(t *testing.T) {
	body := sseFrame(t, contentChunk("你好，世界")) +
		sseFrame(t, `{"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4321,"completion_tokens":876,"total_tokens":5197}}`) +
		"data: [DONE]\n\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, body)
	}))
	defer server.Close()
	result, err := chatTestClient(t, server.URL).Chat(context.Background(), ChatRequest{Model: "kmodel", Prompt: "中文提示词"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.InputTokens != 4321 || result.OutputTokens != 876 {
		t.Fatalf("tokens = %d/%d, want upstream 4321/876", result.InputTokens, result.OutputTokens)
	}
}

// R8 fallback: frames without usage keep the estimate working.
func TestChatFallsBackToEstimateWithoutUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, sseFrame(t, contentChunk("hello there"))+"data: [DONE]\n\n")
	}))
	defer server.Close()
	result, err := chatTestClient(t, server.URL).Chat(context.Background(), ChatRequest{Model: "kmodel", Prompt: "hello my friend"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.InputTokens != estimateTokens("hello my friend") || result.OutputTokens != estimateTokens("hello there") {
		t.Fatalf("tokens = %d/%d, want estimates %d/%d", result.InputTokens, result.OutputTokens,
			estimateTokens("hello my friend"), estimateTokens("hello there"))
	}
}

// R7: cutting on bytes left half of a Chinese rune in the error text clients see.
func TestTruncateKeepsWholeRunes(t *testing.T) {
	value := "上游网关繁忙，请稍后重试"
	for max := 3; max < len(value); max++ {
		got := truncate(value, max)
		if !utf8.ValidString(got) {
			t.Fatalf("max=%d produced invalid UTF-8: %q", max, got)
		}
		if !strings.HasSuffix(got, "... [truncated]") {
			t.Fatalf("max=%d lost the truncation marker: %q", max, got)
		}
		if head := strings.TrimSuffix(got, "... [truncated]"); !strings.HasPrefix(value, head) || head == "" {
			t.Fatalf("max=%d head = %q, want a non-empty prefix of %q", max, head, value)
		}
	}
	if got := truncate("plain ascii", 5); got != "plain... [truncated]" {
		t.Fatalf("ascii truncate = %q", got)
	}
	if got := truncate("short", 50); got != "short" {
		t.Fatalf("short value = %q", got)
	}
}
