package httpapi

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// A text-only turn whose prose is entirely whitespace opens and closes its text
// block inside FlushLead. CloseAnnounced must not then emit a second
// content_block_stop for that index: the official SDKs index the content array
// by block index, and a block that is stopped twice is not a shape any client
// is required to tolerate.
func TestAnthropicFlushLeadDoesNotDoubleStopTheTextBlock(t *testing.T) {
	rec := httptest.NewRecorder()
	blocks := &anthropicStreamBlocks{w: rec, flusher: nopFlusher{}}
	blocks.TextDelta("\n")
	if !blocks.FlushLead(false) {
		t.Fatal("FlushLead reported a failed write")
	}
	if !blocks.CloseAnnounced() {
		t.Fatal("CloseAnnounced reported a failed write")
	}
	body := rec.Body.String()
	starts := strings.Count(body, `"type":"content_block_start"`)
	stops := strings.Count(body, `"type":"content_block_stop"`)
	if starts != 1 || stops != 1 {
		t.Fatalf("one started block must be stopped exactly once, got starts=%d stops=%d:\n%s", starts, stops, body)
	}
}

// The ordinary shape still owes its stop to CloseAnnounced, and a text block
// stays open across deltas.
func TestAnthropicCloseAnnouncedStopsAnOpenTextBlockOnce(t *testing.T) {
	rec := httptest.NewRecorder()
	blocks := &anthropicStreamBlocks{w: rec, flusher: nopFlusher{}}
	blocks.TextDelta("hello ")
	blocks.TextDelta("world")
	if !blocks.CloseAnnounced() {
		t.Fatal("CloseAnnounced reported a failed write")
	}
	if !blocks.CloseAnnounced() {
		t.Fatal("CloseAnnounced reported a failed write on the second call")
	}
	body := rec.Body.String()
	if got := strings.Count(body, `"type":"content_block_stop"`); got != 1 {
		t.Fatalf("text block must be stopped once across repeated CloseAnnounced, got %d:\n%s", got, body)
	}
	if !strings.Contains(body, `"text":"hello "`) || !strings.Contains(body, `"text":"world"`) {
		t.Fatalf("both deltas must survive:\n%s", body)
	}
}

type nopFlusher struct{}

func (nopFlusher) Flush() {}
