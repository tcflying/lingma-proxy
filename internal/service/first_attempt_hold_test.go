package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/remote"
	"lingma-ipc-proxy/internal/toolemulation"
)

// heldToolStream builds a holding stream and a channel of the events it forwards.
func heldToolStream() (*firstAttemptStream, <-chan StreamEvent) {
	events := make(chan StreamEvent, 16)
	hold := newFirstAttemptStream(func(event StreamEvent) { events <- event }, ChatRequest{
		Tools: []toolemulation.ToolDef{{Name: "read_file"}},
	})
	return hold, events
}

// lowerFirstAttemptHold lowers the package-scope bound for one test. The var is
// package-scoped, so these tests must not run in parallel.
func lowerFirstAttemptHold(t *testing.T, bound time.Duration) {
	t.Helper()
	previous := cliFirstAttemptHoldMax
	cliFirstAttemptHoldMax = bound
	t.Cleanup(func() { cliFirstAttemptHoldMax = previous })
}

// TestFirstAttemptHoldForwardsOnBound pins the silent-turn fix: an unresolved
// tool turn used to hold every delta for the whole turn (measured 19m47s of
// silence), so the bound must release what is held without any resolution.
func TestFirstAttemptHoldForwardsOnBound(t *testing.T) {
	lowerFirstAttemptHold(t, 50*time.Millisecond)
	hold, events := heldToolStream()
	t.Cleanup(hold.drop)

	hold.delta("early")
	select {
	case event := <-events:
		if event.Delta != "early" {
			t.Fatalf("forwarded %q, want the held delta", event.Delta)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("held delta was not forwarded after the bound passed")
	}
	if !hold.streamed() {
		t.Fatal("streamed() is false after the bound released the attempt")
	}

	// Once released the attempt streams directly; release must not replay the
	// deltas that already went out.
	hold.delta("late")
	select {
	case event := <-events:
		if event.Delta != "late" {
			t.Fatalf("forwarded %q after the release, want the new delta", event.Delta)
		}
	case <-time.After(time.Second):
		t.Fatal("a delta after the bound was held again")
	}
	hold.release()
	select {
	case event := <-events:
		t.Fatalf("release replayed %q after the bound had forwarded it", event.Delta)
	default:
	}
}

// TestFirstAttemptHoldIsSilentBeforeTheBound is the control for the test above:
// while the hold is armed nothing may reach the client, or the retry isolation
// the hold exists for would be gone.
func TestFirstAttemptHoldIsSilentBeforeTheBound(t *testing.T) {
	lowerFirstAttemptHold(t, 30*time.Second)
	hold, events := heldToolStream()
	t.Cleanup(hold.drop)

	hold.delta("held")
	select {
	case event := <-events:
		t.Fatalf("forwarded %q before the bound", event.Delta)
	case <-time.After(20 * time.Millisecond):
	}
	if hold.streamed() {
		t.Fatal("streamed() is true while the event is still held")
	}
	hold.release()
	if event := <-events; event.Delta != "held" {
		t.Fatalf("release forwarded %q, want the held delta", event.Delta)
	}
}

// TestEmitFinalAfterEarlyReleaseDoesNotDuplicate pins the tradeoff of the bound:
// the client has already seen that attempt's bytes, so the accepted text must
// not be emitted on top of them.
func TestEmitFinalAfterEarlyReleaseDoesNotDuplicate(t *testing.T) {
	lowerFirstAttemptHold(t, 20*time.Millisecond)
	hold, events := heldToolStream()
	t.Cleanup(hold.drop)

	hold.delta("preamble")
	select {
	case event := <-events:
		if event.Delta != "preamble" {
			t.Fatalf("forwarded %q, want the preamble", event.Delta)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bound never forwarded the preamble")
	}
	// emitFinal is synchronous, so anything it emitted is already buffered.
	hold.emitFinal("accepted text")
	select {
	case event := <-events:
		t.Fatalf("emitFinal duplicated text after an early release: %q", event.Delta)
	default:
	}
}

// TestReleaseBeforeTheBoundForwardsHeldDeltasOnce is the normal resolution: the
// turn survived every retry decision, so the held deltas go out in order, and
// neither a second release nor the still-armed timer may repeat them.
func TestReleaseBeforeTheBoundForwardsHeldDeltasOnce(t *testing.T) {
	lowerFirstAttemptHold(t, 40*time.Millisecond)
	hold, events := heldToolStream()
	t.Cleanup(hold.drop)

	hold.delta("a")
	hold.delta("b")
	hold.release()
	for _, want := range []string{"a", "b"} {
		select {
		case event := <-events:
			if event.Delta != want {
				t.Fatalf("forwarded %q, want %q", event.Delta, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("release did not forward %q", want)
		}
	}
	hold.release()
	hold.drop()
	// Outlive the bound: the timer was disarmed by release, so nothing may fire.
	select {
	case event := <-events:
		t.Fatalf("the hold timer fired after release and replayed %q", event.Delta)
	case <-time.After(120 * time.Millisecond):
	}
}

// TestGenerateRemoteWithModelAcceptedRetryFollowsFlushedPreamble is HTTP-07's
// other half: when the hold bound has already flushed the superseded first
// attempt's preamble, the accepted retry's prose must still follow it. The old
// early return dropped exactly that text, leaving the client with only the
// attempt the turn had replaced.
func TestGenerateRemoteWithModelAcceptedRetryFollowsFlushedPreamble(t *testing.T) {
	lowerFirstAttemptHold(t, 20*time.Millisecond)
	// Pin the backend and base URL: an empty base URL's IDE config scan (~2.5s)
	// would not change this test's race but keeps it off the slow path.
	svc := New(Config{Backend: BackendRemote, RemoteBaseURL: "http://hold.invalid", Model: "kmodel", Timeout: time.Second})
	wire := &wireCollector{}
	client := &scriptedChatClient{
		results: []*remote.ChatResult{
			{Text: "i don't have tools"},
			{Text: "accepted reply", ToolCalls: []toolemulation.ToolCall{{ID: "call_retry", Name: "read_fixture", Arguments: map[string]any{"path": "alpha.txt"}}}},
		},
		wire: wire,
		// Longer than the 20ms hold, so the timer flushes the preamble while the
		// first Chat call is still open.
		delayFirst: 200 * time.Millisecond,
	}
	req := ChatRequest{
		Tools: []toolemulation.ToolDef{{
			Name:        "read_fixture",
			Description: "read one fixture file",
			InputSchema: map[string]any{"type": "object"},
		}},
		ToolChoice: toolemulation.ToolChoice{Mode: "any"},
		Messages:   []ChatMessage{{Role: "user", Text: "read the alpha fixture"}},
	}

	result, emitted, err := svc.generateRemoteWithModel(context.Background(), client, req, "", "read the alpha fixture", "kmodel", wire.onDelta, true)
	if err != nil {
		t.Fatalf("generateRemoteWithModel: %v", err)
	}
	if len(client.requests) != 2 {
		t.Fatalf("chat calls = %d, want the initial attempt plus the emulation retry", len(client.requests))
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "read_fixture" {
		t.Fatalf("result = %#v, want the retry's read_fixture call", result)
	}
	if got := wire.streamed.String(); got != "i don't have toolsaccepted reply" {
		t.Fatalf("streamed %q, want the flushed preamble followed by the accepted retry's prose", got)
	}
	if !emitted {
		t.Fatal("emitted = false, want true: the accepted retry's prose reached the wire")
	}
}

// TestEmitFinalTailForwardsOnlyTheUnstreamedSuffix is the IPC rewritten call
// site's contract (P0): once the bound flushed the preamble, the accepted text
// is that preamble plus the single-call notice, and the client is owed the
// notice once -- not the preamble again, and not silence.
func TestEmitFinalTailForwardsOnlyTheUnstreamedSuffix(t *testing.T) {
	lowerFirstAttemptHold(t, 20*time.Millisecond)
	hold, events := heldToolStream()
	t.Cleanup(hold.drop)

	hold.delta("preamble")
	select {
	case event := <-events:
		if event.Delta != "preamble" {
			t.Fatalf("forwarded %q, want the flushed preamble", event.Delta)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bound never forwarded the preamble")
	}
	// emitFinalTail is synchronous, so anything it emitted is already buffered.
	hold.emitFinalTail("preamble\nnotice")
	if event := <-events; event.Delta != "\nnotice" {
		t.Fatalf("forwarded %q, want only the unstreamed notice suffix", event.Delta)
	}
	select {
	case event := <-events:
		t.Fatalf("emitFinalTail replayed %q after the flushed preamble", event.Delta)
	default:
	}
}

// TestEmitFinalTailEmitsWholeTextBeforeAnyFlush is the other side: a rewritten
// turn whose preamble is still held hands the whole accepted text to the
// client, and the held copy is dropped rather than replayed.
func TestEmitFinalTailEmitsWholeTextBeforeAnyFlush(t *testing.T) {
	lowerFirstAttemptHold(t, 30*time.Second)
	hold, events := heldToolStream()
	t.Cleanup(hold.drop)

	hold.delta("preamble")
	hold.emitFinalTail("preamble\nnotice")
	if event := <-events; event.Delta != "preamble\nnotice" {
		t.Fatalf("forwarded %q, want the whole accepted text", event.Delta)
	}
	select {
	case event := <-events:
		t.Fatalf("emitFinalTail replayed %q", event.Delta)
	default:
	}
}

// TestEmitFinalTailDropsTextThatDoesNotExtendTheStream is the no-duplicate
// guard: when the wire already carries something the accepted text does not
// continue (a superseded attempt's prose), the tail emitter stays silent.
func TestEmitFinalTailDropsTextThatDoesNotExtendTheStream(t *testing.T) {
	lowerFirstAttemptHold(t, 20*time.Millisecond)
	hold, events := heldToolStream()
	t.Cleanup(hold.drop)

	hold.delta("superseded prose")
	select {
	case event := <-events:
		if event.Delta != "superseded prose" {
			t.Fatalf("forwarded %q, want the flushed superseded prose", event.Delta)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the bound never forwarded the superseded prose")
	}
	hold.emitFinalTail("accepted text")
	select {
	case event := <-events:
		t.Fatalf("emitFinalTail emitted %q over unrelated wire text", event.Delta)
	default:
	}
}

// TestGenerateRemoteWithModelSingleCallNoticeFollowsFlushedPreamble is the P2
// regression: acceptToolContract's serial-tooling notice is appended to the
// same attempt's text, so a flushed preamble used to cost the client the whole
// notice. The remote native channel leaves result.Text intact, which is exactly
// the prefix case emitFinalTail completes.
func TestGenerateRemoteWithModelSingleCallNoticeFollowsFlushedPreamble(t *testing.T) {
	lowerFirstAttemptHold(t, 20*time.Millisecond)
	svc := New(Config{Backend: BackendRemote, RemoteBaseURL: "http://hold.invalid", Model: "kmodel", Timeout: time.Second})
	wire := &wireCollector{}
	client := &scriptedChatClient{
		results: []*remote.ChatResult{{
			Text: "preamble",
			ToolCalls: []toolemulation.ToolCall{
				{ID: "call_1", Name: "read_fixture", Arguments: map[string]any{"path": "alpha.txt"}},
				{ID: "call_2", Name: "read_fixture", Arguments: map[string]any{"path": "beta.txt"}},
			},
		}},
		wire: wire,
		// Longer than the 20ms hold, so the timer flushes the preamble while the
		// first Chat call is still open.
		delayFirst: 200 * time.Millisecond,
	}
	parallel := false
	req := ChatRequest{
		Tools: []toolemulation.ToolDef{{
			Name:        "read_fixture",
			Description: "read one fixture file",
			InputSchema: map[string]any{"type": "object"},
		}},
		ToolChoice:        toolemulation.ToolChoice{Mode: "auto"},
		ParallelToolCalls: &parallel,
		Messages:          []ChatMessage{{Role: "user", Text: "read both fixtures"}},
	}

	result, emitted, err := svc.generateRemoteWithModel(context.Background(), client, req, "", "read both fixtures", "kmodel", wire.onDelta, true)
	if err != nil {
		t.Fatalf("generateRemoteWithModel: %v", err)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "read_fixture" {
		t.Fatalf("result = %#v, want only the first of the two calls", result)
	}
	if len(client.requests) != 1 {
		t.Fatalf("chat calls = %d, want the single native attempt", len(client.requests))
	}
	streamed := wire.streamed.String()
	if got := strings.Count(streamed, "preamble"); got != 1 {
		t.Fatalf("preamble streamed %d times, want once: %q", got, streamed)
	}
	if got := strings.Count(streamed, "parallel_tool_calls=false"); got != 1 {
		t.Fatalf("serial-tooling notice streamed %d times, want once: %q", got, streamed)
	}
	if p, n := strings.Index(streamed, "preamble"), strings.Index(streamed, "parallel_tool_calls=false"); p < 0 || n < 0 || p > n {
		t.Fatalf("notice must follow the flushed preamble: %q", streamed)
	}
	if !emitted {
		t.Fatal("emitted = false, want true: the notice reached the wire")
	}
}

// TestForwardedStopsGrowingAfterTheHoldResolves pins the P2 fix: the retained
// prefix stops at the hold window, so a stream that keeps producing after the
// hold resolved does not leave a full copy of the answer behind. forwardedLen
// still counts the whole stream, so a rewritten turn after a timer flush can
// still be handed only its unstreamed notice.
func TestForwardedStopsGrowingAfterTheHoldResolves(t *testing.T) {
	readForwarded := func(h *firstAttemptStream) (string, int) {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.forwarded.String(), h.forwardedLen
	}

	t.Run("release", func(t *testing.T) {
		lowerFirstAttemptHold(t, 30*time.Second)
		hold, events := heldToolStream()
		t.Cleanup(hold.drop)

		hold.delta("held")
		hold.release()
		if event := <-events; event.Delta != "held" {
			t.Fatalf("forwarded %q, want the held delta", event.Delta)
		}
		hold.delta("late")
		if event := <-events; event.Delta != "late" {
			t.Fatalf("forwarded %q, want the delta after resolution", event.Delta)
		}
		if sent, total := readForwarded(hold); sent != "held" || total != len("heldlate") {
			t.Fatalf("retained %q / counted %d after release, want the held window retained and the whole stream counted", sent, total)
		}
	})

	t.Run("timer", func(t *testing.T) {
		lowerFirstAttemptHold(t, 20*time.Millisecond)
		hold, events := heldToolStream()
		t.Cleanup(hold.drop)

		hold.delta("early")
		select {
		case event := <-events:
			if event.Delta != "early" {
				t.Fatalf("forwarded %q, want the flushed delta", event.Delta)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the bound never forwarded the held delta")
		}
		hold.delta("late")
		if event := <-events; event.Delta != "late" {
			t.Fatalf("forwarded %q after the bound, want the new delta", event.Delta)
		}
		if sent, total := readForwarded(hold); sent != "early" || total != len("earlylate") {
			t.Fatalf("retained %q / counted %d after the bound, want the held window retained and the whole stream counted", sent, total)
		}
		hold.emitFinalTail("earlylate\nnotice")
		if event := <-events; event.Delta != "\nnotice" {
			t.Fatalf("forwarded %q, want only the unstreamed notice suffix", event.Delta)
		}
	})
}
