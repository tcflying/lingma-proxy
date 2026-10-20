package service

import (
	"strings"
	"testing"
)

// dsmlMarkup spells DeepSeek's native tool-call marker for the fixtures below.
// Concatenation keeps a full literal out of source: in a transcript it would
// look like a real tool call.
const dsmlMarkup = "<" + "\uFF5C\uFF5CDSML\uFF5C\uFF5C"

func drainEvents(events <-chan StreamEvent) string {
	var b strings.Builder
	for {
		select {
		case event := <-events:
			b.WriteString(event.Delta)
		default:
			return b.String()
		}
	}
}

// TestFirstAttemptStreamSuppressesNativeToolMarkup is the wire-level half of
// the fix: clients render streamed deltas even when the final parse later
// succeeds, so a native DSML block must stop at the marker even when the marker
// itself arrives split across two deltas.
func TestFirstAttemptStreamSuppressesNativeToolMarkup(t *testing.T) {
	hold, events := heldToolStream()
	t.Cleanup(hold.drop)

	hold.delta("hello " + dsmlMarkup[:7]) // "<" plus two fullwidth bars
	hold.delta(dsmlMarkup[7:] + " calls>\n" + dsmlMarkup + " invoke name=\"read_file\">\n" +
		dsmlMarkup + " parameter name=\"file_path\">C:/tmp/x.md</parameter>\n</invoke>\n</calls>")
	hold.delta("late prose the call owns")
	hold.release()

	got := drainEvents(events)
	if got != "hello " {
		t.Fatalf("client saw %q, want only the prose before the marker", got)
	}
	if strings.Contains(got, "DSML") {
		t.Fatalf("native markup reached the client: %q", got)
	}
}

// TestFirstAttemptStreamFlushesProseCarry is the control: with no marker the
// held-back tail is released exactly once, in order.
func TestFirstAttemptStreamFlushesProseCarry(t *testing.T) {
	hold, events := heldToolStream()
	t.Cleanup(hold.drop)

	hold.delta("hello there, ")
	hold.delta("friend")
	hold.release()

	got := drainEvents(events)
	if got != "hello there, friend" {
		t.Fatalf("client saw %q, want the two deltas once and in order", got)
	}
}
