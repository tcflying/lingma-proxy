package httpapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/service"
)

// 933 hardening batch for the HTTP layer. Each test here pins one finding from
// .scratch/933-review/a-report.md, and every one of them has to fail against
// the code as it stood before this batch.

// ---------- P2-H1: image proxy + CORS * + no auth = local read primitive ----------

// harden933PNG is a real 1x1 PNG, so a test can prove the fetch really produced
// image bytes rather than an arbitrary body the proxy would have accepted.
func harden933PNG() []byte {
	return []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	}
}

// harden933ImageOrigin serves the bytes a remote image fetch would read. It is
// the stand-in for "a local or LAN service a hostile page points the proxy at".
func harden933ImageOrigin(t *testing.T, contentType string, body []byte) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		if _, err := w.Write(body); err != nil {
			t.Errorf("write image body: %v", err)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func harden933ChatImageBody(t *testing.T, imageURL string) []byte {
	t.Helper()
	body := map[string]any{
		"model": hybrid931Model,
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "这张图里是什么？"},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURL}},
			},
		}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func harden933Post(t *testing.T, proxy *httptest.Server, path string, body []byte, origin string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxy.URL+path, strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

// A page in any browser can cross-origin POST to the proxy, and with
// Access-Control-Allow-Origin: * it can read the answer back out of the model's
// reply. When the request also names an image for the proxy to go and read, that
// turns the proxy into a fetch primitive pointed at whatever the page chose --
// 127.0.0.1, a LAN service, this box. The refusal must be on the named-image
// case only, so the loopback clients that send screenshots keep working.
func TestHardening933CrossOriginNamedImageRequestIsRefused(t *testing.T) {
	images, hits := harden933ImageOrigin(t, "image/png", harden933PNG())
	proxy, _ := hybrid931NewEnv(t, "", []string{"一只猫。"})
	body := harden933ChatImageBody(t, images.URL+"/cat.png")

	code, resp := harden933Post(t, proxy, "/v1/chat/completions", body, "http://evil.example")
	if code != http.StatusForbidden {
		t.Fatalf("cross-origin image request = %d, want 403; body = %.200s", code, resp)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("the proxy fetched the named image %d time(s) for a cross-origin caller", got)
	}
}

// The same request from a loopback client is the desktop app's normal path: no
// Origin at all, the fetch happens, the turn is answered. If this ever trips the
// gate the image feature is dead for every non-browser client.
func TestHardening933LoopbackNamedImageRequestStillFetches(t *testing.T) {
	images, hits := harden933ImageOrigin(t, "image/png", harden933PNG())
	proxy, _ := hybrid931NewEnv(t, "", []string{"一只猫。"})

	code, resp := harden933Post(t, proxy, "/v1/chat/completions", harden933ChatImageBody(t, images.URL+"/cat.png"), "")
	if code != http.StatusOK {
		t.Fatalf("loopback image request = %d, want 200; body = %.200s", code, resp)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("image origin hits = %d, want exactly 1: the loopback client never reached the fetch", got)
	}
}

// An inline data: URL reads nothing off the box, so it is not a fetch primitive
// and must stay available to a browser-hosted client.
func TestHardening933CrossOriginInlineImageStaysAvailable(t *testing.T) {
	proxy, _ := hybrid931NewEnv(t, "", []string{"一只猫。"})
	inline := "data:image/png;base64," + base64.StdEncoding.EncodeToString(harden933PNG())
	code, resp := harden933Post(t, proxy, "/v1/chat/completions", harden933ChatImageBody(t, inline), "http://evil.example")
	if code != http.StatusOK {
		t.Fatalf("cross-origin inline image = %d, want 200; body = %.200s", code, resp)
	}
}

// The Origin gate only stops a browser. A named image read directly still has
// to be an image: otherwise the SSRF chain survives for any non-browser caller,
// and a service that answers an internal request with JSON or HTML is exactly
// the target the chain is built to read.
func TestHardening933RemoteImageMustAnswerWithAnImageContentType(t *testing.T) {
	ctx := context.Background()

	secret, _ := harden933ImageOrigin(t, "application/json", []byte(`{"token":"internal-only"}`))
	if img, err := fetchImageAsBase64(ctx, secret.URL+"/state.json"); err == nil {
		t.Fatalf("a JSON answer was accepted as an image: %#v", img.MediaType)
	}

	page, _ := harden933ImageOrigin(t, "text/html; charset=utf-8", []byte("<html>intranet</html>"))
	if img, err := fetchImageAsBase64(ctx, page.URL+"/index.html"); err == nil {
		t.Fatalf("an HTML answer was accepted as an image: %#v", img.MediaType)
	}

	real, _ := harden933ImageOrigin(t, "image/png", harden933PNG())
	img, err := fetchImageAsBase64(ctx, real.URL+"/cat.png")
	if err != nil {
		t.Fatalf("a real image was refused: %v", err)
	}
	if img.MediaType != "image/png" {
		t.Fatalf("media type = %q, want image/png", img.MediaType)
	}
	if decoded, err := base64.StdEncoding.DecodeString(img.Data); err != nil || len(decoded) == 0 {
		t.Fatalf("decoded image = %d bytes, err = %v", len(decoded), err)
	}
}

// ---------- P2-H2: a Responses stream failure was always logged as 200 ----------

// harden933SSEErrorFrame renders the exact bytes the Responses handler writes
// for a generation failure, from the same helper production uses, so the
// fixture cannot drift away from the frame the recorder has to recognise.
func harden933SSEErrorFrame(t *testing.T) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"type":  "error",
		"error": openAIStreamErrorObject(fmt.Errorf("upstream said no")),
	})
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("event: error\ndata: %s\n\n", body)
}

// Once the 200 and the SSE headers are on the wire the client-visible status
// cannot change, but the access log and every client that retries on status
// codes were told the turn succeeded.
func TestHardening933StreamFailureIsRecordedAsABadGateway(t *testing.T) {
	s := &Server{sem: make(chan struct{}, 1)}
	callbackStatus := make(chan int, 4)
	s.OnRequest = func(_, _ string, statusCode int, _ time.Duration, _, _ string) {
		callbackStatus <- statusCode
	}

	failing := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamingHeaders(w)
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, harden933SSEErrorFrame(t))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	rec := httptest.NewRecorder()
	s.withRecorder(failing).ServeHTTP(rec,
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}")))
	if rec.Code != http.StatusOK {
		t.Fatalf("client-visible status = %d, want the committed 200 to be untouched", rec.Code)
	}
	waitForRecordedStatus(t, s, http.StatusBadGateway)
	if got := <-callbackStatus; got != http.StatusBadGateway {
		t.Fatalf("OnRequest status = %d, want the rewritten 502", got)
	}

	// A turn that completed normally must not be reclassified.
	healthy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		streamingHeaders(w)
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\"}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\"}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	rec = httptest.NewRecorder()
	s.withRecorder(healthy).ServeHTTP(rec,
		httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader("{}")))
	waitForRecordedStatus(t, s, http.StatusOK)
}

func waitForRecordedStatus(t *testing.T, s *Server, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		records := s.debugRecords(1)
		if len(records) > 0 {
			if got := records[0].StatusCode; got != want {
				t.Fatalf("recorded status = %d, want %d", got, want)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("no request record appeared within 2s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ---------- P3-H3: decodeJSON accepted anything after the first value ----------

func TestHardening933DecodeJSONRejectsContentAfterTheTopLevelValue(t *testing.T) {
	server := NewServer("", service.New(service.Config{Model: "Qwen3-Coder", Timeout: 200 * time.Millisecond}))
	// The value in every body below is a complete, decodable request, so the
	// only thing that can refuse one is content after it. The proxy has no
	// backend here, which is what a decodable request answers with instead of
	// 400 -- that is the discriminator.
	post := func(body string) (int, string) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	const value = `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	for _, body := range []string{
		value + ` {"model":"other"}`,
		value + " trailing garbage",
		value + "{",
		value + ` "second value"`,
	} {
		code, resp := post(body)
		if code != http.StatusBadRequest {
			t.Fatalf("body %q = %d, want 400; the tail was accepted silently (%s)", body, code, resp)
		}
		if !strings.Contains(resp, "unexpected content") {
			t.Fatalf("body %q got the wrong 400: %s", body, resp)
		}
	}
	for _, body := range []string{value, value + "\n\n  \t\r\n"} {
		code, resp := post(body)
		if code == http.StatusBadRequest {
			t.Fatalf("body %q was refused: trailing whitespace is not content (%s)", body, resp)
		}
	}
}

// ---------- P3-H5: the two history entry points disagreed about an empty id ----

// An assistant turn replayed without a tool_call id cannot be paired with the
// tool_result that follows it, and it reaches the client again as id:"". The
// Responses entry point already dropped it; these two used to keep it.
func TestHardening933HistoricalToolCallWithoutAnIDIsDropped(t *testing.T) {
	openAIMissing := []any{map[string]any{
		"id": "", "type": "function",
		"function": map[string]any{"name": "Read", "arguments": `{"path":"a"}`},
	}}
	if got := extractOpenAIToolCalls(openAIMissing); len(got) != 0 {
		t.Fatalf("chat history kept an id-less call: %#v", got)
	}
	openAIKept := []any{map[string]any{
		"id": "call_1", "type": "function",
		"function": map[string]any{"name": "Read", "arguments": `{"path":"a"}`},
	}}
	if got := extractOpenAIToolCalls(openAIKept); len(got) != 1 || got[0].ID != "call_1" {
		t.Fatalf("chat history dropped a call that has an id: %#v", got)
	}

	_, anthropicCalls := extractAnthropicAssistantContent([]any{map[string]any{
		"type": "tool_use", "id": "", "name": "Read", "input": map[string]any{"path": "a"},
	}})
	if len(anthropicCalls) != 0 {
		t.Fatalf("anthropic history kept an id-less call: %#v", anthropicCalls)
	}
	_, anthropicKept := extractAnthropicAssistantContent([]any{map[string]any{
		"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"path": "a"},
	}})
	if len(anthropicKept) != 1 || anthropicKept[0].ID != "toolu_1" {
		t.Fatalf("anthropic history dropped a call that has an id: %#v", anthropicKept)
	}

	// All three entry points now agree on the same item.
	if got := responsesFunctionCallToRawMessage(map[string]any{"name": "Read", "arguments": "{}"}); got != nil {
		t.Fatalf("responses history kept an id-less call: %#v", got)
	}
}

// ---------- P3-H6: only ReadHeaderTimeout was set ----------

// A finished keep-alive connection otherwise sits in the listener's pool until
// the client closes it. WriteTimeout is deliberately absent -- a streaming turn
// legitimately writes nothing for minutes -- so IdleTimeout is the only lever
// that can reap it.
func TestHardening933ServerSetsIdleTimeoutWithoutWriteTimeout(t *testing.T) {
	server := NewServer("127.0.0.1:0", service.New(service.Config{Model: "Qwen3-Coder", Timeout: time.Second}))
	if server.http.IdleTimeout <= 0 {
		t.Fatal("IdleTimeout is unset: finished keep-alive connections are never reaped")
	}
	if server.http.WriteTimeout != 0 {
		t.Fatalf("WriteTimeout = %v; a streaming turn writes nothing for minutes and would be cut mid-answer", server.http.WriteTimeout)
	}
	if server.http.ReadHeaderTimeout <= 0 {
		t.Fatal("ReadHeaderTimeout regressed")
	}
}

// ---------- P3-H8: OnRequest ran in a bare goroutine ----------

// The desktop callback runs in a goroutine the request handler does not own. A
// panic there used to take the whole proxy process down with it.
func TestHardening933OnRequestPanicDoesNotTakeTheProxyDown(t *testing.T) {
	server := NewServer("", service.New(service.Config{Model: "Qwen3-Coder", Timeout: 200 * time.Millisecond}))
	panicked := make(chan struct{}, 1)
	server.OnRequest = func(_, _ string, _ int, _ time.Duration, _, _ string) {
		panicked <- struct{}{}
		panic("desktop callback exploded")
	}
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, req)
	select {
	case <-panicked:
	case <-time.After(2 * time.Second):
		t.Fatal("the OnRequest callback never ran")
	}
	// Give the panicking goroutine time to take the process down if nothing
	// recovers it.
	time.Sleep(300 * time.Millisecond)

	rec = httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("proxy stopped serving after a panicking callback: %d", rec.Code)
	}
}

// ---------- P3-H9: the body was buffered before the slot was taken ----------

// Resident body memory used to be maxRequestBytes times every open socket,
// because withRecorder read the whole body before the concurrency gate. A
// request that cannot take a slot must not have its body pulled into memory at
// all: the handler is about to refuse it with 429 anyway.
func TestHardening933RequestBodyIsOnlyBufferedWhileASlotIsHeld(t *testing.T) {
	prevWait := slotQueueWait
	slotQueueWait = 50 * time.Millisecond
	t.Cleanup(func() { slotQueueWait = prevWait })

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", "proxy is busy; retry shortly")
	})
	serve := func(fillSlot bool) (int, string) {
		s := &Server{sem: make(chan struct{}, 1)}
		if fillSlot {
			s.sem <- struct{}{}
		}
		rec := httptest.NewRecorder()
		s.withRecorder(inner).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)))
		records := s.debugRecords(1)
		if len(records) != 1 {
			t.Fatalf("records = %d, want 1", len(records))
		}
		return rec.Code, records[0].Request
	}

	code, recorded := serve(false)
	if code != http.StatusTooManyRequests {
		t.Fatalf("slot-free status = %d, want the handler's own 429", code)
	}
	if !strings.Contains(recorded, `"content":"hi"`) {
		t.Fatalf("a request that held the slot lost its recorded body: %q", recorded)
	}

	code, recorded = serve(true)
	if code != http.StatusTooManyRequests {
		t.Fatalf("saturated status = %d, want 429", code)
	}
	if recorded != "" {
		t.Fatalf("the body was buffered without a slot held: %q", recorded)
	}
}

// ---------- P3-H7: message_start usage differed between the two branches ------

func TestHardening933AnthropicMessageStartUsageIsTheSameInBothBranches(t *testing.T) {
	usage := func(aggregate string) map[string]any {
		proxy, _ := hybrid931NewEnv(t, aggregate, []string{"好的。"})
		body := map[string]any{
			"model":      hybrid931Model,
			"max_tokens": 64,
			"messages": []any{map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": "hi"}},
			}},
			"tools":  []any{hybrid931ReadTool()},
			"stream": true,
		}
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		code, resp := harden933Post(t, proxy, "/v1/messages", raw, "")
		if code != http.StatusOK {
			t.Fatalf("stream status = %d; body = %.300s", code, resp)
		}
		start := harden933FirstEventData(t, resp, "message_start")
		var event struct {
			Message struct {
				Usage map[string]any `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(start), &event); err != nil {
			t.Fatalf("message_start is not JSON: %v (%s)", err, start)
		}
		return event.Message.Usage
	}
	incremental, aggregate := usage(""), usage("1")
	if fmt.Sprint(incremental) != fmt.Sprint(aggregate) {
		t.Fatalf("message_start usage differs between the two stream branches:\nincremental %v\naggregate   %v",
			incremental, aggregate)
	}
}

func harden933FirstEventData(t *testing.T, body, event string) string {
	t.Helper()
	for _, block := range strings.Split(body, "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == event {
			return data
		}
	}
	t.Fatalf("event %q never arrived: %s", event, body)
	return ""
}

// ---------- P3-H2: whitespace-only text put the two Anthropic views apart -----

// harden933AnthropicBlockKinds returns the content-block types in order, from
// a streamed body (content_block_start events) and from a non-streaming body
// (its content array), so the two views of one turn can be compared directly.
func harden933AnthropicBlockKinds(t *testing.T, streamBody, bodyBody string) (streamed, reported []string) {
	t.Helper()
	for _, block := range strings.Split(streamBody, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event struct {
				Type         string         `json:"type"`
				Index        int            `json:"index"`
				ContentBlock map[string]any `json:"content_block"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				continue
			}
			if event.Type == "content_block_start" {
				if event.Index != len(streamed) {
					t.Fatalf("stream announced index %d after %d blocks: %s", event.Index, len(streamed), streamBody)
				}
				streamed = append(streamed, fmt.Sprint(event.ContentBlock["type"]))
			}
		}
	}
	var message struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal([]byte(bodyBody), &message); err != nil {
		t.Fatalf("non-stream body is not JSON: %v (%s)", err, bodyBody)
	}
	for _, b := range message.Content {
		reported = append(reported, fmt.Sprint(b["type"]))
	}
	return streamed, reported
}

// A turn whose only prose is the whitespace in front of an action block streamed
// a text block at index 0 and the tool_use at 1, while the non-streaming body
// dropped the blank text and put tool_use at 0. Official SDKs index the content
// array by block index, so a client reading both views sees them disagree.
func TestHardening933AnthropicBlankLeadingTextAgreesAcrossStreamAndBody(t *testing.T) {
	streamed, upstreamCalls := native932NewEnv(t, []native932Script{
		native932ContentFrames("\n", native932JSONBlock),
	})
	code, streamBody := native932Post(t, streamed, "/v1/messages", native932AnthropicBody(t, true, nil))
	if code != http.StatusOK {
		t.Fatalf("stream status = %d body = %s", code, streamBody)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	nonStreamed, _ := native932NewEnv(t, []native932Script{
		native932ContentFrames("\n", native932JSONBlock),
	})
	code, bodyBody := native932Post(t, nonStreamed, "/v1/messages", native932AnthropicBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("non-stream status = %d body = %s", code, bodyBody)
	}

	streamKinds, bodyKinds := harden933AnthropicBlockKinds(t, streamBody, bodyBody)
	if fmt.Sprint(streamKinds) != fmt.Sprint(bodyKinds) {
		t.Fatalf("content block views disagree: stream %v, non-stream %v", streamKinds, bodyKinds)
	}
	// The blank line belongs to neither view: the service trims the prose in
	// front of a consumed block, so the turn is a pure tool turn.
	if fmt.Sprint(bodyKinds) != "[tool_use]" {
		t.Fatalf("blocks = %v, want a pure tool turn with no text block", bodyKinds)
	}
}

// The other end of the same rule: prose that stays whitespace to the very end of
// a turn with no tool calls IS the answer, and dropping it would leave the
// stream with no content block at all while the body still reports one.
func TestHardening933AnthropicWhitespaceOnlyTurnStillReportsItsText(t *testing.T) {
	streamed, _ := native932NewEnv(t, []native932Script{native932ContentFrames("  \n  ")})
	code, streamBody := native932Post(t, streamed, "/v1/messages", native932AnthropicBody(t, true, nil))
	if code != http.StatusOK {
		t.Fatalf("stream status = %d body = %s", code, streamBody)
	}
	nonStreamed, _ := native932NewEnv(t, []native932Script{native932ContentFrames("  \n  ")})
	code, bodyBody := native932Post(t, nonStreamed, "/v1/messages", native932AnthropicBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("non-stream status = %d body = %s", code, bodyBody)
	}
	streamKinds, bodyKinds := harden933AnthropicBlockKinds(t, streamBody, bodyBody)
	if fmt.Sprint(streamKinds) != fmt.Sprint(bodyKinds) || fmt.Sprint(bodyKinds) != "[text]" {
		t.Fatalf("whitespace-only turn: stream %v, non-stream %v, want [text] in both", streamKinds, bodyKinds)
	}
	if !strings.Contains(streamBody, `"text":"  \n  "`) {
		t.Fatalf("the held whitespace never reached the client: %s", streamBody)
	}
}

// ---------- P3-H4: the aggregate branch wrote nothing until Generate returned --

// harden933GatedEnv parks the stub gateway inside the model turn until the test
// releases it, which is what makes "did the response open before Generate"
// observable: a client that has response headers while the turn is still parked
// can only have them from a stream opened ahead of the call.
func harden933GatedEnv(t *testing.T, aggregate string) (*httptest.Server, chan struct{}, chan struct{}) {
	t.Helper()
	t.Setenv("LINGMA_AGGREGATE_TOOL_STREAM", aggregate)
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")

	hit := make(chan struct{}, 1)
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, hybrid931ContentFrame(t, "好的。"))
		_, _ = io.WriteString(w, hybrid931FinishFrame(t))
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
		Model:          hybrid931Model,
		Timeout:        30 * time.Second,
	})
	t.Cleanup(func() { _ = svc.Close() })
	proxy := httptest.NewServer(NewServer("", svc).http.Handler)
	t.Cleanup(proxy.Close)
	return proxy, hit, release
}

func TestHardening933AggregateStreamOpensHeadersBeforeGenerate(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/v1/chat/completions"} {
		t.Run(path, func(t *testing.T) {
			proxy, hit, release := harden933GatedEnv(t, "1")
			// The parked upstream must always be let go: a Fatalf below would
			// otherwise leave it blocked and hang the httptest cleanup.
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			var body []byte
			if path == "/v1/messages" {
				body = native932AnthropicBody(t, true, nil)
			} else {
				body = native932OpenAIBody(t, true, nil)
			}

			type outcome struct {
				resp *http.Response
				err  error
			}
			done := make(chan outcome, 1)
			go func() {
				req, err := http.NewRequest(http.MethodPost, proxy.URL+path, strings.NewReader(string(body)))
				if err != nil {
					done <- outcome{err: err}
					return
				}
				req.Header.Set("Content-Type", "application/json")
				resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
				done <- outcome{resp: resp, err: err}
			}()

			select {
			case <-hit:
			case <-time.After(30 * time.Second):
				t.Fatal("the upstream turn never started")
			}
			var out outcome
			select {
			case out = <-done:
				if out.err != nil {
					t.Fatal(out.err)
				}
				if ct := out.resp.Header.Get("Content-Type"); ct != "text/event-stream" {
					t.Fatalf("content type = %q, want text/event-stream", ct)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no response headers while Generate was still blocked; " +
					"the aggregate branch writes nothing until the turn returns")
			}
			close(release)
			released = true
			// Drain so the proxy has no half-open stream when the test ends.
			if out.resp != nil {
				_, _ = io.Copy(io.Discard, out.resp.Body)
				_ = out.resp.Body.Close()
			}
		})
	}
}

// ---------- P3-H1: a frame's sequence_number could land after the next one ----

// harden933GatedStreamWriter is a stream writer whose Write parks until the
// test lets it through, so a frame can be caught mid-flight.
type harden933GatedStreamWriter struct {
	mu      sync.Mutex
	entered chan string
	release chan struct{}
	frames  []string
}

func (w *harden933GatedStreamWriter) Header() http.Header { return http.Header{} }
func (w *harden933GatedStreamWriter) WriteHeader(int)     {}

func (w *harden933GatedStreamWriter) Write(b []byte) (int, error) {
	w.entered <- string(b)
	<-w.release
	w.mu.Lock()
	w.frames = append(w.frames, string(b))
	w.mu.Unlock()
	return len(b), nil
}

func (w *harden933GatedStreamWriter) Flush() {}

// The emitter used to take its sequence number under its own lock, release it,
// and only then write. A keep-alive beat landing in that window took the next
// number and reached the wire first, so the client saw sequence_number go
// backwards and a strict accumulator could drop the terminal event. Allocating
// and writing are now one critical section, which this pins directly: while a
// real event is inside the writer, a heartbeat frame cannot be produced at all.
func TestHardening933EmitterAllocatesSequenceInsideTheStreamWrite(t *testing.T) {
	writer := &harden933GatedStreamWriter{
		entered: make(chan string, 4),
		release: make(chan struct{}),
	}
	keepalive := newSSEKeepaliveWriter(writer, writer)
	defer keepalive.Close()
	emitter := newOpenAIResponseStreamEmitter(keepalive, keepalive, "resp_1")
	emitter.serial = func(name string, payload map[string]any, after func()) error {
		return keepalive.writeFrame(func() (string, error) {
			emitter.mu.Lock()
			defer emitter.mu.Unlock()
			frame, err := emitter.marshalEventLocked(name, payload)
			if err != nil {
				return "", err
			}
			after()
			return frame, nil
		})
	}
	keepalive.setEventFrame(emitter.heartbeatFrame)
	go func() {
		_ = emitter.Event("response.created", map[string]any{
			"type":     "response.created",
			"response": map[string]any{"id": "resp_1", "object": "response", "status": "in_progress"},
		})
	}()
	select {
	case <-writer.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the created frame never reached the writer")
	}

	// The real event is inside the write. The heartbeat goroutine's entry point
	// is emit, which takes the stream lock; it must block here, because a beat
	// that took the next number now would reach the wire before the number the
	// event already owns.
	beatDone := make(chan struct{})
	go func() {
		defer close(beatDone)
		_ = keepalive.emit(0)
	}()
	select {
	case <-beatDone:
		t.Fatal("a heartbeat beat went out while a real event was mid-write: " +
			"it would take the next sequence_number and overtake that event on the wire")
	case <-time.After(200 * time.Millisecond):
	}
	close(writer.release)
	select {
	case <-beatDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the heartbeat never got the stream back after the event was written")
	}

	// And the numbers on the wire are in the order the frames went out.
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if len(writer.frames) != 2 {
		t.Fatalf("frames on the wire = %d, want the event then the beat", len(writer.frames))
	}
	var numbers []int
	for _, frame := range writer.frames {
		data, _, ok := strings.Cut(strings.TrimPrefix(strings.SplitN(frame, "\n", 2)[1], "data: "), "\n\n")
		if !ok {
			t.Fatalf("frame is not a whole SSE frame: %q", frame)
		}
		var event struct {
			SequenceNumber int `json:"sequence_number"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			t.Fatalf("frame data is not JSON: %v (%s)", err, data)
		}
		numbers = append(numbers, event.SequenceNumber)
	}
	if numbers[1] <= numbers[0] {
		t.Fatalf("sequence numbers did not increase with the wire order: %v", numbers)
	}
}

// ---------- P3-H10: the two streaming dialect rows that were never pinned ------

// native932 covered "streamed + pure dialect" and "non-streamed + mixed"; the
// combination that actually reaches a client -- a stream carrying prose and a
// converted call, reconciled against response.completed -- had no full-chain
// nail.
func TestNative932ResponsesStreamMixedTextAndDialectReconciled(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(
		native932Head+"\n",
		native932JSONBlock,
		"\n"+native932Tail,
	)})
	code, body := native932Post(t, proxy, "/v1/responses", native932ResponsesBody(t, true, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	var streamed strings.Builder
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"response.output_text.delta"`) {
				var payload struct {
					Delta string `json:"delta"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err == nil {
					streamed.WriteString(payload.Delta)
				}
			}
		}
	}
	if got, want := strings.TrimSpace(streamed.String()), native932Head+"\n"+native932Tail; got != want {
		t.Fatalf("streamed deltas = %q, want %q", got, want)
	}
	native932AssertNoDialectLeak(t, streamed.String())

	text, calls := native932ParseResponsesOutput(t, body)
	native932AssertReadGoMod(t, calls)
	if text != streamed.String() {
		t.Fatalf("response.completed text %q does not reconcile with the deltas the client already holds (%q)", text, streamed.String())
	}
}

// The merge test only ran non-streaming, so nothing pinned the streamed
// tool_calls[].index sequence: a client that assembles arguments by index would
// have merged both calls into one silently.
func TestNative932ChatStreamNativeAndDialectCallsMerge(t *testing.T) {
	script := native932Script{
		native932Head + "\n",
		native932JSONBlock,
		native932ToolCallFrame(t, "call_native_1", "write_file", `{"path":"out.txt","content":"hi"}`),
	}
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{script})
	code, body := native932Post(t, proxy, "/v1/chat/completions", native932OpenAIBody(t, true, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	stream := hybrid931ParseOpenAIStream(t, body)
	if stream.SawError {
		t.Fatalf("stream reported an error: %s", body)
	}
	if !stream.SawDone {
		t.Fatalf("the stream never sent [DONE]: %s", body)
	}
	if got, want := strings.TrimSpace(stream.Text), native932Head; got != want {
		t.Fatalf("streamed text = %q, want the prose with the block stripped (%q)", got, want)
	}
	native932AssertNoDialectLeak(t, stream.Text)
	if len(stream.Calls) != 2 {
		t.Fatalf("streamed tool calls = %d (%#v), want the native and the converted call", len(stream.Calls), stream.Calls)
	}
	if stream.Calls[0].ID != "call_native_1" || stream.Calls[0].Name != "write_file" {
		t.Fatalf("first streamed call = %#v, want the gateway's native write_file kept first", stream.Calls[0])
	}
	if stream.Calls[1].Name != "read_file" || stream.Calls[1].Args["path"] != "go.mod" {
		t.Fatalf("second streamed call = %#v, want the dialect-converted read_file", stream.Calls[1])
	}

	// Every tool_calls frame must carry a dense 0..n-1 index, or a client that
	// assembles arguments by index would interleave the two calls.
	seen := map[int]bool{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index int `json:"index"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			continue
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %v (%s)", err, payload)
		}
		for _, choice := range chunk.Choices {
			for _, tc := range choice.Delta.ToolCalls {
				seen[tc.Index] = true
			}
		}
	}
	for i := 0; i < 2; i++ {
		if !seen[i] {
			t.Fatalf("tool_calls[].index %d never appeared; indexes seen: %v", i, seen)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("tool_calls[].index set = %v, want exactly {0,1}", seen)
	}
}
