package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/service"
)

// 933 SSE idle heartbeat regression. Real Codex CLI runs died inside the
// model's long thinking stretches ("idle timeout waiting for SSE" at 383.7s
// and 321.8s, 2026-09-30) even after the client's own idle timeout was
// relaxed: the upstream stays silent while it works, and so did the proxy. The
// fix is the shared sseKeepaliveWriter, which emits heartbeat frames on an
// established stream. Chat keeps the ": keep-alive" comment line — the SSE
// spec defines comment lines as carrying no event, so no parser, accumulator
// or final snapshot sees them. Anthropic and Responses upgraded to real
// protocol events (keepalive934_test.go): clients whose idle timer only
// counts parsed events, Codex CLI among them, discard comment lines.
// These tests drive the whole chain (stub gateway -> remote client -> service
// -> HTTP handler) in the 931/932 style:
//   - while the upstream hangs mid-answer, the heartbeat frame reaches the
//     client within ~2x the interval, on all three protocols;
//   - apart from the heartbeat frames, the event stream is byte-identical to
//     the same turn answered without any silence, and the terminal frame
//     lands;
//   - a fast turn emits no heartbeat frames at all.
const (
	keepalive933Model    = "kmodel"
	keepalive933UserTurn = "数到十，慢一点。"
	// keepalive933Answer is the delta text that only flows once the hang
	// releases, so a comment observed before it proves the heartbeat fired
	// inside the silent stretch, not after the turn resumed.
	keepalive933Answer = "一 二 三 四 五 六 七 八 九 十"
)

var (
	keepalive933StreamIDs  = regexp.MustCompile(`\b(?:msg_|chatcmpl-|resp_|rs_)\d+`)
	keepalive933Timestamps = regexp.MustCompile(`"(?:created|created_at)":\d+`)
	// Every Responses heartbeat consumes a sequence_number, so a beat turn's
	// numbers sit higher than a silent-free turn's; monotonicity itself is
	// asserted separately in keepalive934_test.go.
	keepalive933Sequences = regexp.MustCompile(`"sequence_number":\d+`)
)

// keepalive933Normalize erases the per-request identities (nanosecond ids,
// created timestamps, heartbeat-shifted sequence numbers) so two runs of the
// same turn can be byte-compared.
func keepalive933Normalize(body string) string {
	body = keepalive933StreamIDs.ReplaceAllString(body, "streamID")
	body = keepalive933Timestamps.ReplaceAllString(body, `"created":0`)
	return keepalive933Sequences.ReplaceAllString(body, `"sequence_number":0`)
}

// keepalive933SetInterval shrinks the heartbeat interval for one test, the
// maxCLIOutputLineBytes precedent, and restores it on cleanup.
func keepalive933SetInterval(t *testing.T, interval time.Duration) {
	t.Helper()
	prev := streamKeepaliveInterval
	streamKeepaliveInterval = interval
	t.Cleanup(func() { streamKeepaliveInterval = prev })
}

// keepalive933NewEnv wires the isolated stack with an upstream that answers
// one content delta and then goes silent until the hang channel closes,
// modelling the silent thinking stretch that killed the real Codex runs.
func keepalive933NewEnv(t *testing.T, hang <-chan struct{}) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	t.Setenv("LINGMA_AGGREGATE_TOOL_STREAM", "")
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := io.WriteString(w, hybrid931ContentFrame(t, "长思考开始，稍等。")); err != nil {
			t.Errorf("write upstream frame: %v", err)
			return
		}
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		// The silence under test: no further frame until the test releases.
		select {
		case <-hang:
		case <-r.Context().Done():
			return
		}
		if _, err := io.WriteString(w, hybrid931ContentFrame(t, keepalive933Answer)); err != nil {
			t.Errorf("write upstream frame: %v", err)
			return
		}
		if _, err := io.WriteString(w, hybrid931FinishFrame(t)); err != nil {
			t.Errorf("write upstream finish frame: %v", err)
			return
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
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
		Model:          keepalive933Model,
		Timeout:        30 * time.Second,
	})
	t.Cleanup(func() { _ = svc.Close() })

	proxy := httptest.NewServer(NewServer("", svc).http.Handler)
	t.Cleanup(proxy.Close)
	return proxy, &upstreamCalls
}

// keepalive933Beat names one protocol's heartbeat frame: await is the
// substring whose arrival proves the beat fired, strip removes every beat
// frame so the rest of the stream can be byte-compared against a turn with
// no silence.
type keepalive933Beat struct {
	await string
	strip func(string) string
}

// keepalive933AwaitBeat reads the live stream until the awaited heartbeat
// frame shows up, and reports when it arrived. It returns at EOF with
// saw=false when the stream ends silent, which is exactly the defect.
func keepalive933AwaitBeat(t *testing.T, body io.Reader, await string) (head string, beatAt time.Time, saw bool) {
	t.Helper()
	var collected bytes.Buffer
	buf := make([]byte, 8192)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			collected.Write(buf[:n])
			if bytes.Contains(collected.Bytes(), []byte(await)) {
				return collected.String(), time.Now(), true
			}
		}
		if err == io.EOF {
			return collected.String(), time.Time{}, false
		}
		if err != nil {
			t.Fatalf("reading the stream: %v", err)
		}
	}
}

// keepalive933RunHeartbeatTurn drives one streamed request through the hung
// upstream and pins the heartbeat contract: the beat frame lands inside the
// silent stretch within ~2x the interval, the terminal frame still arrives,
// and with the beat frames removed the event stream is byte-identical to the
// same turn answered with no silence at all.
func keepalive933RunHeartbeatTurn(t *testing.T, path string, requestBody []byte, terminal string, beat keepalive933Beat) {
	t.Helper()
	keepalive933SetInterval(t, 40*time.Millisecond)

	hang := make(chan struct{})
	proxy, upstreamCalls := keepalive933NewEnv(t, hang)
	// The hang always releases on its own, so a broken heartbeat fails an
	// assertion instead of wedging the test.
	time.AfterFunc(350*time.Millisecond, func() { close(hang) })

	started := time.Now()
	resp, err := http.Post(proxy.URL+path, "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d body = %s", resp.StatusCode, raw)
	}
	head, beatAt, saw := keepalive933AwaitBeat(t, resp.Body, beat.await)
	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	streamed := head + string(rest)

	if !saw {
		t.Fatalf("no heartbeat frame reached the client while the upstream hung: %s", streamed)
	}
	if delay := beatAt.Sub(started); delay > 2*streamKeepaliveInterval+200*time.Millisecond {
		t.Fatalf("first beat after %v, want within ~2x%v", delay, streamKeepaliveInterval)
	}
	if cIdx, aIdx := strings.Index(streamed, beat.await), strings.Index(streamed, keepalive933Answer); cIdx < 0 || aIdx < 0 || cIdx > aIdx {
		t.Fatalf("the beat must land inside the silent stretch, before the answer deltas: %s", streamed)
	}
	if !strings.Contains(streamed, terminal) {
		t.Fatalf("terminal frame %q never arrived: %s", terminal, streamed)
	}

	// Reference turn with the hang long released: identical request, no
	// silence, so no heartbeat. Stripping the beat frames from the heartbeat
	// turn must leave the two byte-identical — nothing reordered, duplicated
	// or torn by the concurrent heartbeat writes.
	refResp, err := http.Post(proxy.URL+path, "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	defer refResp.Body.Close()
	refRaw, err := io.ReadAll(refResp.Body)
	if err != nil {
		t.Fatal(err)
	}
	stripped := beat.strip(streamed)
	if got, want := keepalive933Normalize(stripped), keepalive933Normalize(string(refRaw)); got != want {
		t.Fatalf("heartbeat turn drifted from the silence-free turn:\ngot:  %q\nwant: %q", got, want)
	}
	if n := upstreamCalls.Load(); n != 2 {
		t.Fatalf("upstream answered %d turns, want 2", n)
	}
}

func keepalive933AnthropicBody(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"model":      keepalive933Model,
		"max_tokens": 1024,
		"stream":     true,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": keepalive933UserTurn}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func keepalive933ChatBody(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"model":  keepalive933Model,
		"stream": true,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": keepalive933UserTurn,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func keepalive933ResponsesBody(t *testing.T) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"model":  keepalive933Model,
		"stream": true,
		"input":  keepalive933UserTurn,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestKeepalive933AnthropicMessagesStreamPingsDuringSilence(t *testing.T) {
	keepalive933RunHeartbeatTurn(t, "/v1/messages", keepalive933AnthropicBody(t), "event: message_stop", keepalive933Beat{
		await: "event: ping",
		strip: func(s string) string { return strings.ReplaceAll(s, keepalive934PingFrame, "") },
	})
}

func TestKeepalive933OpenAIChatStreamCommentsDuringSilence(t *testing.T) {
	keepalive933RunHeartbeatTurn(t, "/v1/chat/completions", keepalive933ChatBody(t), "data: [DONE]", keepalive933Beat{
		await: ": keep-alive",
		strip: func(s string) string { return strings.ReplaceAll(s, ": keep-alive\n\n", "") },
	})
}

func TestKeepalive933OpenAIResponsesStreamResendsInProgressDuringSilence(t *testing.T) {
	keepalive933RunHeartbeatTurn(t, "/v1/responses", keepalive933ResponsesBody(t), "event: response.completed", keepalive933Beat{
		await: "event: response.in_progress",
		strip: func(s string) string { return keepalive934InProgressFrame.ReplaceAllString(s, "") },
	})
}

// A turn with no silent stretch must not emit any heartbeat frame, comment or
// event alike: heartbeats are purely an idle-time measure and must not
// decorate fast traffic.
func TestKeepalive933FastStreamsSendNoComments(t *testing.T) {
	hang := make(chan struct{})
	close(hang)
	proxy, upstreamCalls := keepalive933NewEnv(t, hang)

	for _, turn := range []struct {
		name     string
		path     string
		body     []byte
		terminal string
	}{
		{"anthropic", "/v1/messages", keepalive933AnthropicBody(t), "event: message_stop"},
		{"chat", "/v1/chat/completions", keepalive933ChatBody(t), "data: [DONE]"},
		{"responses", "/v1/responses", keepalive933ResponsesBody(t), "event: response.completed"},
	} {
		resp, err := http.Post(proxy.URL+turn.path, "application/json", bytes.NewReader(turn.body))
		if err != nil {
			t.Fatalf("%s: post: %v", turn.name, err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("%s: read: %v", turn.name, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d body = %s", turn.name, resp.StatusCode, raw)
		}
		for _, leak := range []string{": keep-alive", "event: ping", "event: response.in_progress"} {
			if strings.Contains(string(raw), leak) {
				t.Fatalf("%s: fast turn emitted a heartbeat frame %q: %s", turn.name, leak, raw)
			}
		}
		if !strings.Contains(string(raw), turn.terminal) {
			t.Fatalf("%s: terminal frame %q missing: %s", turn.name, turn.terminal, raw)
		}
	}
	if n := upstreamCalls.Load(); n != 3 {
		t.Fatalf("upstream answered %d turns, want 3", n)
	}
}

// The serialization contract behind the byte-identity checks above: with one
// writer goroutine (every handler's shape) streaming frames whose gaps exceed
// the heartbeat interval, comments are guaranteed to interleave — and the raw
// body must still show every "event:" line immediately followed by its
// "data:" line, i.e. no comment ever spliced into a frame.
func TestKeepalive933CommentFramesStayWhole(t *testing.T) {
	keepalive933SetInterval(t, time.Millisecond)

	rec := httptest.NewRecorder()
	beat := newSSEKeepaliveWriter(rec, rec)
	defer beat.Close()

	const frames = 60
	for i := 0; i < frames; i++ {
		if err := writeSSEEvent(beat, beat, "e", map[string]any{"i": i}); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	beat.Close()

	raw := rec.Body.String()
	if !strings.Contains(raw, ": keep-alive\n\n") {
		t.Fatalf("no comment interleaved with %d gapped frames: %s", frames, raw)
	}
	if got := strings.Count(raw, `"i":`); got != frames {
		t.Fatalf("payload frames = %d, want %d", got, frames)
	}
	lines := strings.Split(raw, "\n")
	for i := 0; i+1 < len(lines); i++ {
		if !strings.HasPrefix(lines[i], "event: ") {
			continue
		}
		if !strings.HasPrefix(lines[i+1], "data: ") {
			t.Fatalf("frame torn at line %d: %q is not followed by its data line (raw=%q)", i, lines[i], raw)
		}
	}
}
