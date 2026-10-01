package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"
)

// 934: heartbeat frames upgraded from comment lines to real protocol events.
// The 933 comment heartbeat kept the socket honest but real Codex CLI runs
// still died at 329.5s ("idle timeout waiting for SSE"): codex-rs resets its
// idle timer only on parsed SSE events and its eventsource layer discards
// comment lines, so for event-level idle timers the heartbeat itself must be
// an event. Anthropic gets the official ping event; Responses re-sends the
// idempotent response.in_progress status event (only between response.created
// and the terminal event, so the sequence stays legal and no accumulator's
// final state rolls back); Chat keeps comment lines — that protocol defines
// no ping event. These tests pin:
//   - the exact official ping frame reaches /v1/messages and
//     /anthropic/v1/messages clients inside the silent stretch;
//   - Responses re-sends response.in_progress carrying the same response
//     object response.created announced, sequence numbers strictly
//     increasing, and response.created itself still sent exactly once;
//   - both heartbeats are invisible to an official-shape accumulator: the
//     snapshot with beats equals the snapshot without them, and the stream
//     with beat frames stripped is byte-identical to a silent-free turn;
//   - the Responses beat never fires before response.created nor after the
//     terminal event;
//   - event heartbeats still cannot be torn: every "event:" line keeps its
//     immediate "data:" line.

// keepalive934PingFrame is the official Anthropic ping event, byte for byte.
const keepalive934PingFrame = "event: ping\ndata: {\"type\":\"ping\"}\n\n"

// keepalive934InProgressFrame matches one re-sent response.in_progress frame.
// Its data line carries a per-beat sequence_number, so it is a regex, not a
// constant.
var keepalive934InProgressFrame = regexp.MustCompile(`(?m)^event: response\.in_progress\ndata: .*\n\n`)

type keepalive934Frame struct {
	event string
	data  string
}

// keepalive934ParseFrames splits a raw SSE body into frames the way an
// eventsource parser does: an "event:" line, a "data:" line, a blank line.
func keepalive934ParseFrames(raw string) []keepalive934Frame {
	var frames []keepalive934Frame
	var cur keepalive934Frame
	started := false
	flush := func() {
		if started {
			frames = append(frames, cur)
			cur, started = keepalive934Frame{}, false
		}
	}
	for _, line := range strings.Split(raw, "\n") {
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.event = strings.TrimPrefix(line, "event: ")
			started = true
		case strings.HasPrefix(line, "data: "):
			cur.data = strings.TrimPrefix(line, "data: ")
		case line == "":
			flush()
		}
	}
	flush()
	return frames
}

// keepalive934AnthropicSnapshot is what an official Anthropic stream
// accumulator (the SDKs' MessageStream) keeps: the announced message
// identity, the content blocks accumulated per index, the stop reason, the
// final usage, and whether message_stop arrived. A ping event updates none
// of it — that official contract is exactly what the heartbeat leans on.
type keepalive934AnthropicSnapshot struct {
	Model        string
	Role         string
	StopReason   string
	OutputTokens int
	Blocks       map[int]string
	Stopped      bool
}

func keepalive934AccumulateAnthropic(frames []keepalive934Frame) keepalive934AnthropicSnapshot {
	snap := keepalive934AnthropicSnapshot{Blocks: map[int]string{}}
	for _, f := range frames {
		if f.data == "[DONE]" {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Message struct {
				Model string `json:"model"`
				Role  string `json:"role"`
			} `json:"message"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(f.data), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "ping":
			// The official accumulator ignores pings; the heartbeat lives here.
		case "message_start":
			snap.Model, snap.Role = ev.Message.Model, ev.Message.Role
		case "content_block_start":
			if _, ok := snap.Blocks[ev.Index]; !ok {
				snap.Blocks[ev.Index] = ""
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				snap.Blocks[ev.Index] += ev.Delta.Text
			case "thinking_delta":
				snap.Blocks[ev.Index] += ev.Delta.Thinking
			case "input_json_delta":
				snap.Blocks[ev.Index] += ev.Delta.PartialJSON
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				snap.StopReason = ev.Delta.StopReason
			}
			snap.OutputTokens += ev.Usage.OutputTokens
		case "message_stop":
			snap.Stopped = true
		}
	}
	return snap
}

// keepalive934ResponsesSnapshot is a Codex-style view of a Responses stream:
// the current status (re-sent in_progress events are idempotent and change
// nothing), the delta-accumulated text, and the terminal completed snapshot.
type keepalive934ResponsesSnapshot struct {
	Status              string
	Text                string
	CompletedStatus     string
	CompletedOutputText string
}

func keepalive934AccumulateResponses(frames []keepalive934Frame) keepalive934ResponsesSnapshot {
	var snap keepalive934ResponsesSnapshot
	for _, f := range frames {
		if f.data == "[DONE]" {
			continue
		}
		var ev struct {
			Delta    string `json:"delta"`
			Response struct {
				Status     string `json:"status"`
				OutputText string `json:"output_text"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(f.data), &ev); err != nil {
			continue
		}
		switch f.event {
		case "response.created":
			snap.Status = ev.Response.Status
		case "response.in_progress":
			snap.Status = "in_progress" // idempotent re-send: no state change
		case "response.output_text.delta":
			snap.Text += ev.Delta
		case "response.completed":
			snap.CompletedStatus = ev.Response.Status
			snap.CompletedOutputText = ev.Response.OutputText
		}
	}
	return snap
}

func keepalive934Post(t *testing.T, url string, body []byte) string {
	t.Helper()
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
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
	return string(raw)
}

func TestKeepalive934AnthropicPingEventDuringSilence(t *testing.T) {
	keepalive933SetInterval(t, 40*time.Millisecond)

	hang := make(chan struct{})
	proxy, _ := keepalive933NewEnv(t, hang)
	time.AfterFunc(350*time.Millisecond, func() { close(hang) })

	streamed := keepalive934Post(t, proxy.URL+"/v1/messages", keepalive933AnthropicBody(t))

	if !strings.Contains(streamed, keepalive934PingFrame) {
		t.Fatalf("no official ping frame during the silence: %s", streamed)
	}
	if pIdx, aIdx := strings.Index(streamed, "event: ping"), strings.Index(streamed, keepalive933Answer); pIdx < 0 || aIdx < 0 || pIdx > aIdx {
		t.Fatalf("the ping must land inside the silent stretch, before the answer deltas: %s", streamed)
	}
	if !strings.Contains(streamed, "event: message_stop") {
		t.Fatalf("terminal frame never arrived: %s", streamed)
	}

	// Silent-free reference turn: same request, hang released, no heartbeat.
	ref := keepalive934Post(t, proxy.URL+"/v1/messages", keepalive933AnthropicBody(t))
	if strings.Contains(ref, "event: ping") {
		t.Fatalf("silent-free turn pinged: %s", ref)
	}
	stripped := strings.ReplaceAll(streamed, keepalive934PingFrame, "")
	if got, want := keepalive933Normalize(stripped), keepalive933Normalize(ref); got != want {
		t.Fatalf("beat turn drifted from the silent-free turn:\ngot:  %q\nwant: %q", got, want)
	}

	// Official accumulator shape: pings in the stream change no snapshot.
	withSnap := keepalive934AccumulateAnthropic(keepalive934ParseFrames(streamed))
	refSnap := keepalive934AccumulateAnthropic(keepalive934ParseFrames(ref))
	if !reflect.DeepEqual(withSnap, refSnap) {
		t.Fatalf("accumulator snapshot with pings %+v != without %+v", withSnap, refSnap)
	}
}

func TestKeepalive934AnthropicAliasRoutePingsToo(t *testing.T) {
	keepalive933SetInterval(t, 40*time.Millisecond)

	hang := make(chan struct{})
	proxy, _ := keepalive933NewEnv(t, hang)
	time.AfterFunc(350*time.Millisecond, func() { close(hang) })

	streamed := keepalive934Post(t, proxy.URL+"/anthropic/v1/messages", keepalive933AnthropicBody(t))
	if !strings.Contains(streamed, keepalive934PingFrame) {
		t.Fatalf("no ping frame on the alias route: %s", streamed)
	}
	if !strings.Contains(streamed, "event: message_stop") {
		t.Fatalf("terminal frame never arrived: %s", streamed)
	}
}

func TestKeepalive934ResponsesResendsInProgressDuringSilence(t *testing.T) {
	keepalive933SetInterval(t, 40*time.Millisecond)

	hang := make(chan struct{})
	proxy, _ := keepalive933NewEnv(t, hang)
	time.AfterFunc(350*time.Millisecond, func() { close(hang) })

	streamed := keepalive934Post(t, proxy.URL+"/v1/responses", keepalive933ResponsesBody(t))

	if got := strings.Count(streamed, "event: response.created"); got != 1 {
		t.Fatalf("response.created sent %d times, want 1: %s", got, streamed)
	}
	if !strings.Contains(streamed, "event: response.in_progress") {
		t.Fatalf("no response.in_progress re-send during the silence: %s", streamed)
	}
	createdAt := strings.Index(streamed, "event: response.created")
	beatAt := strings.Index(streamed, "event: response.in_progress")
	answerAt := strings.Index(streamed, keepalive933Answer)
	if createdAt > beatAt || beatAt > answerAt {
		t.Fatalf("in_progress must land after created and before the answer deltas: %s", streamed)
	}
	if !strings.Contains(streamed, "event: response.completed") || !strings.Contains(streamed, "data: [DONE]") {
		t.Fatalf("terminal frame never arrived: %s", streamed)
	}

	// Codex-side view: every event parses, ids stay consistent with created,
	// sequence numbers strictly increase across beats and real events alike.
	frames := keepalive934ParseFrames(streamed)
	var createdID string
	var lastSeq int64 = -1
	beats := 0
	for _, f := range frames {
		if f.data == "[DONE]" {
			continue
		}
		var ev struct {
			SequenceNumber int64 `json:"sequence_number"`
			Response       struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(f.data), &ev); err != nil {
			t.Fatalf("unparsable event data %q: %v", f.data, err)
		}
		if ev.SequenceNumber <= lastSeq {
			t.Fatalf("sequence_number %d did not increase past %d (%s)", ev.SequenceNumber, lastSeq, f.data)
		}
		lastSeq = ev.SequenceNumber
		switch f.event {
		case "response.created":
			createdID = ev.Response.ID
		case "response.in_progress":
			beats++
			if ev.Response.ID != createdID {
				t.Fatalf("beat response.id %q != created %q", ev.Response.ID, createdID)
			}
		}
	}
	if beats == 0 {
		t.Fatalf("no in_progress beats counted: %s", streamed)
	}

	ref := keepalive934Post(t, proxy.URL+"/v1/responses", keepalive933ResponsesBody(t))
	if strings.Contains(ref, "event: response.in_progress") {
		t.Fatalf("silent-free turn re-sent in_progress: %s", ref)
	}
	stripped := keepalive934InProgressFrame.ReplaceAllString(streamed, "")
	if got, want := keepalive933Normalize(stripped), keepalive933Normalize(ref); got != want {
		t.Fatalf("beat turn drifted from the silent-free turn:\ngot:  %q\nwant: %q", got, want)
	}

	withSnap := keepalive934AccumulateResponses(frames)
	refSnap := keepalive934AccumulateResponses(keepalive934ParseFrames(ref))
	if !reflect.DeepEqual(withSnap, refSnap) {
		t.Fatalf("accumulator snapshot with beats %+v != without %+v", withSnap, refSnap)
	}
}

// The Responses beat is gated on both ends of the event sequence: no
// in_progress before response.created has actually been written, none after
// the terminal event — a beat past completed would roll a conforming
// accumulator's status back to in_progress.
func TestKeepalive934ResponsesHeartbeatGatedOnCreatedAndTerminal(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := newOpenAIResponseStreamEmitter(rec, rec, "resp_gate934")

	if frame, ok := emitter.heartbeatFrame(); ok {
		t.Fatalf("beat before response.created: %q", frame)
	}

	created := map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id": "resp_gate934", "object": "response", "created_at": 1,
			"status": "in_progress", "model": "kmodel",
		},
	}
	if err := emitter.Event("response.created", created); err != nil {
		t.Fatal(err)
	}
	frame, ok := emitter.heartbeatFrame()
	if !ok {
		t.Fatal("no beat after response.created")
	}
	if !strings.HasPrefix(frame, "event: response.in_progress\n") {
		t.Fatalf("beat frame event wrong: %q", frame)
	}
	if !strings.Contains(frame, `"id":"resp_gate934"`) || !strings.Contains(frame, `"status":"in_progress"`) {
		t.Fatalf("beat frame must reuse the created response object: %q", frame)
	}

	if err := emitter.Event("response.completed", map[string]any{
		"type":     "response.completed",
		"response": map[string]any{"id": "resp_gate934", "status": "completed"},
	}); err != nil {
		t.Fatal(err)
	}
	if frame, ok := emitter.heartbeatFrame(); ok {
		t.Fatalf("beat after the terminal event: %q", frame)
	}
}

// Event heartbeats obey the same serialization contract as comments: with
// frames gapped past the interval, beats interleave and no "event:" line may
// ever lose its immediate "data:" line.
func TestKeepalive934EventFramesStayWhole(t *testing.T) {
	keepalive933SetInterval(t, time.Millisecond)

	rec := httptest.NewRecorder()
	beat := newSSEKeepaliveWriter(rec, rec)
	defer beat.Close()
	beat.setEventFrame(anthropicPingFrame)

	const frames = 60
	for i := 0; i < frames; i++ {
		if err := writeSSEEvent(beat, beat, "e", map[string]any{"i": i}); err != nil {
			t.Fatalf("write frame %d: %v", i, err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	beat.Close()

	raw := rec.Body.String()
	if !strings.Contains(raw, keepalive934PingFrame) {
		t.Fatalf("no ping interleaved with %d gapped frames: %s", frames, raw)
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
