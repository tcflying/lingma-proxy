package toolemulation

import (
	"strings"
	"testing"
)

// 1005 C-4 regressions: the pending fenced block's JSON body is balanced with
// a saved cursor so a sharded big parameter costs one scan per byte
// (TOOLS-14), and the 512 KiB undecided-hold bound is one acceptance boundary
// on both the streaming and the final side (TOOLS-06) — a candidate the
// streamer releases as prose is never handed back as a call, and a candidate
// that completes on the crossing delta is judged before any release.

// c4RunFeed drives the scanner the way toolStreamFilter does and returns the
// streamed prose, the withheld buffer, and the bytes the body balancer walked
// (the scanner's incremental walk plus jsonBodyEnd's full rescans).
func c4RunFeed(tools []ToolDef, deltas []string) (emitted, pending string, scanned int) {
	jsonBodyScanTotal.Store(0)
	scanner := NewActionBlockScanner(tools)
	pending = ""
	var out strings.Builder
	for _, delta := range deltas {
		pending += delta
	drain:
		for {
			start, end, unterminated := scanner.FindSpan(pending)
			switch {
			case unterminated:
				if start > 0 {
					out.WriteString(pending[:start])
					scanner.Discard(start)
					pending = pending[start:]
				}
				break drain
			case end > 0:
				out.WriteString(pending[:start])
				scanner.Discard(end)
				pending = pending[end:]
			default:
				safe := len(pending) - ActionOpenPrefixHold(pending)
				if safe > 0 {
					out.WriteString(pending[:safe])
					scanner.Discard(safe)
					pending = pending[safe:]
				}
				break drain
			}
		}
	}
	return out.String(), pending, int(jsonBodyScanTotal.Load()) + scanner.bodyScanned
}

// TestC4Pending1005ChunkedBigParameterMatchesOneShot pins the correctness side
// of the cursor reuse: a big parameter arriving in many shards is accepted with
// the exact one-shot arguments and the surrounding prose survives.
func TestC4Pending1005ChunkedBigParameterMatchesOneShot(t *testing.T) {
	tools := xmlTools()
	value := strings.Repeat("abcdefghij", 600) // ~6 KB, under the default bound
	text := "head\n```json action\n" + `{"tool":"Bash","parameters":{"command":"echo ` + value + `"}}` + "\n```\ntail"

	oneShotCalls, oneShotClean, err := ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(oneShotCalls) != 1 || oneShotCalls[0].Arguments["command"] != "echo "+value {
		t.Fatalf("one-shot baseline: calls = %+v", oneShotCalls)
	}
	if oneShotClean != "head\ntail" {
		t.Fatalf("one-shot clean = %q", oneShotClean)
	}

	for _, shards := range []int{1, 13, 97} {
		size := (len(text) + shards - 1) / shards
		emitted, pending, _ := c4RunFeed(tools, splitEvery(text, size))
		_, flushKeeps, err := ParseActionBlocks(pending, tools, Config{})
		if err != nil {
			t.Fatalf("shards=%d: %v", shards, err)
		}
		if got := strings.TrimSpace(emitted + flushKeeps); got != "head\ntail" {
			t.Fatalf("shards=%d: streamed+flush = %q, want %q", shards, got, "head\ntail")
		}
		if strings.Contains(emitted, value) {
			t.Fatalf("shards=%d: the call leaked as prose", shards)
		}
	}
}

// TestC4Pending1005ScanGrowsNearlyLinearly pins TOOLS-14: at a fixed shard
// size, doubling the parameter doubles the bytes the balancer walks. The
// quadratic baseline — re-balancing the whole withheld body on every delta —
// would quadruple on the same doubling instead.
func TestC4Pending1005ScanGrowsNearlyLinearly(t *testing.T) {
	tools := xmlTools()
	body := func(scale int) string {
		value := strings.Repeat("z", scale*(48<<10))
		return "```json action\n" + `{"tool":"Bash","parameters":{"command":"echo ` + value + `"}}` + "\n```\ntail"
	}
	const shard = 2048
	scannedFor := func(text string) int {
		_, _, scanned := c4RunFeed(tools, splitEvery(text, shard))
		return scanned
	}
	smallText := body(1)
	few := scannedFor(smallText)
	many := scannedFor(body(2))
	t.Logf("scanned: body=%d bytes -> %d walked; body=%d bytes -> %d walked (shard=%d)",
		len(smallText), few, 2*len(smallText), many, shard)

	// Near-linear: twice the body at the same shard size costs about twice the
	// walk. The quadratic shape sits near 4x; the threshold leaves headroom
	// for the single full read that settles each completed body.
	if ratio := float64(many) / float64(few); ratio > 3 {
		t.Fatalf("TOOLS-14: doubling the body multiplied the balancer walk by %.2f (quadratic shape), few=%d many=%d", ratio, few, many)
	}
	// Absolute sanity: the walk stays within a small multiple of the body.
	if many > 6*2*len(smallText) {
		t.Fatalf("TOOLS-14: balancer walked %d bytes for a %d byte body", many, 2*len(smallText))
	}
}

// TestC4Pending1005OversizeBoundaryAgrees pins TOOLS-06: with the undecided
// hold bound lowered, a legal call whose span exceeds it is released as prose
// by the streamer AND refused by the final parse — never "leaked as prose
// mid-stream, then accepted as a call at the end".
func TestC4Pending1005OversizeBoundaryAgrees(t *testing.T) {
	old := maxPendingBlockBytes
	maxPendingBlockBytes = 8 << 10
	t.Cleanup(func() { maxPendingBlockBytes = old })

	tools := xmlTools()
	value := strings.Repeat("z", 10<<10) // span well past the lowered bound
	text := "head\n```json action\n" + `{"tool":"Bash","parameters":{"command":"echo ` + value + `"}}` + "\n```\ntail"

	// The streamer releases the undecided block as prose...
	emitted, pending, _ := c4RunFeed(tools, splitEvery(text, 512))
	if !strings.Contains(emitted, value) {
		t.Fatalf("the oversized undecided block must be released as prose, emitted %d bytes", len(emitted))
	}
	_, flushKeeps, err := ParseActionBlocks(pending, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	// ...and the final parse of the same full text must agree: no call.
	finalCalls, _, err := ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(finalCalls) != 0 {
		t.Fatalf("TOOLS-06: the final parse accepted a call the stream released as prose (leaked-then-accepted), calls = %+v", finalCalls)
	}
	got := strings.TrimSpace(emitted + flushKeeps)
	if want := strings.TrimSpace(text); got != want {
		t.Fatalf("oversized block must reconstruct whole as prose:\n got %d bytes\nwant %d bytes", len(got), len(want))
	}
}

// TestC4Pending1005CompletionOnCrossingDeltaIsJudgedFirst pins the release
// order: the delta that pushes the held text past the bound also carries the
// closing fence, so the completion check runs before any release. The block
// is over the bound either way, so the ruled behaviour is prose on both
// sides — decided, not leaked.
func TestC4Pending1005CompletionOnCrossingDeltaIsJudgedFirst(t *testing.T) {
	old := maxPendingBlockBytes
	maxPendingBlockBytes = 4 << 10
	t.Cleanup(func() { maxPendingBlockBytes = old })

	tools := xmlTools()
	value := strings.Repeat("q", 5<<10)
	head := "head\n```json action\n" + `{"tool":"Bash","parameters":{"command":"echo `
	closeTail := value + `"}}` + "\n```\ntail"
	// Shard 1 stays under the bound; shard 2 crosses it and completes the
	// block in the same delta.
	emitted, pending, _ := c4RunFeed(tools, []string{head + value[:1024], value[1024:] + `"}}` + "\n```\ntail"})
	_, flushKeeps, err := ParseActionBlocks(pending, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	finalCalls, _, err := ParseActionBlocks(head+closeTail, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(finalCalls) != 0 {
		t.Fatalf("over-bound completion must be refused by the final parse too, calls = %+v", finalCalls)
	}
	if got, want := strings.TrimSpace(emitted+flushKeeps), strings.TrimSpace(head+closeTail); got != want {
		t.Fatalf("crossing-delta completion must reconstruct whole as prose:\n got %d bytes\nwant %d bytes", len(got), len(want))
	}
}

// TestC4Pending1005UnderBoundStillAccepted guards the floor: with the bound
// lowered, a call whose span fits under it is still accepted while streaming —
// the unified boundary tightens acceptance, it does not drop legal calls.
func TestC4Pending1005UnderBoundStillAccepted(t *testing.T) {
	old := maxPendingBlockBytes
	maxPendingBlockBytes = 8 << 10
	t.Cleanup(func() { maxPendingBlockBytes = old })

	tools := xmlTools()
	value := strings.Repeat("y", 3000)
	text := "head\n```json action\n" + `{"tool":"Bash","parameters":{"command":"echo ` + value + `"}}` + "\n```\ntail"
	emitted, pending, _ := c4RunFeed(tools, splitEvery(text, 256))
	if strings.Contains(emitted, value) {
		t.Fatal("a call under the bound must be consumed while streaming, not emitted")
	}
	_, flushKeeps, err := ParseActionBlocks(pending, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(emitted + flushKeeps); got != "head\ntail" {
		t.Fatalf("under-bound replay = %q", got)
	}
}
