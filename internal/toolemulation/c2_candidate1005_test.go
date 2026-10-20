package toolemulation

import (
	"strings"
	"testing"
)

// 1005 C-2 regressions: candidate identity (TOOLS-01), dialect stability
// (TOOLS-12), hybrid close markers sharing a line with prose (TOOLS-13), and
// JSON tolerance that tells structure from payload (TOOLS-11).
//
// The TOOLS-12 group replays every fixture shard by shard — one delta per
// shard, reconstructing the client's prose from the streamed output plus the
// Flush parse — because the existing single-buffer verdict check cannot see a
// mid-stream consumption that the final verdict then rejects.

// c2StreamReplay feeds text one shard at a time and returns the prose the
// client ends up holding: streamed emissions plus whatever the final parse of
// the still-withheld buffer keeps.
func c2StreamReplay(t *testing.T, tools []ToolDef, text string, shard int) string {
	t.Helper()
	emitted, pending := streamFeed(tools, splitEvery(text, shard))
	_, flushKeeps, err := ParseActionBlocks(pending, tools, Config{})
	if err != nil {
		t.Fatalf("flush parse: %v", err)
	}
	return strings.TrimSpace(emitted + flushKeeps)
}

// --- TOOLS-01: qualified names never redirect across servers ----------------

// TestC2Tools1005QualifiedLeafNeverRedirectsAcrossServers pins the high-severity
// redirect: a name that already carries a namespace has chosen its server, so
// the leaf layers (alias table, version spelling) must not hand it to a tool
// declared under a different one.
func TestC2Tools1005QualifiedLeafNeverRedirectsAcrossServers(t *testing.T) {
	tools := []ToolDef{{Name: "serverA__web_search", Namespace: "serverA"}}
	names, _ := toolLookupMaps(tools)

	if got := normalizeToolName("serverB__web_search", names); got != "serverB__web_search" {
		t.Fatalf("normalizeToolName redirected an undeclared namespace: got %q", got)
	}
	if got := normalizeToolName("serverB__web_search_v2", names); got != "serverB__web_search_v2" {
		t.Fatalf("a versioned spelling must not cross servers either: got %q", got)
	}
	// The resolutions that stay legal: the exact qualified spelling, and the
	// bare leaf that exactly one declared namespaced tool owns.
	if got := normalizeToolName("serverA__web_search", names); got != "serverA__web_search" {
		t.Fatalf("exact qualified spelling must resolve, got %q", got)
	}
	if got := normalizeToolName("web_search", names); got != "serverA__web_search" {
		t.Fatalf("the unique bare leaf must resolve to its owner, got %q", got)
	}

	// End to end: the undeclared-namespace block is rejected and stays prose.
	text := xOpen + "\n" + `{"tool":"serverB__web_search","parameters":{"query":"q"}}` + "\n" + xClose
	calls, clean, err := ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("an undeclared namespace must not become a call: %+v", calls)
	}
	if !strings.Contains(clean, "serverB__web_search") {
		t.Fatalf("the rejected block must stay verbatim, clean = %q", clean)
	}
}

// --- TOOLS-12: unstable dialect must not consume the JSON fallback ----------

// TestC2Tools1005UnstableDialectDoesNotConsumeJSONFallback pins the flip: while
// a <function= tag's own ">" has not arrived, the JSON behind the tag opening
// reads as a call and the streamer used to suppress that span at once — but
// once the tag closes over it the block is finally rejected, so the text was
// deleted for nothing. The unstable acceptance must hold instead.
func TestC2Tools1005UnstableDialectDoesNotConsumeJSONFallback(t *testing.T) {
	tools := xmlTools()
	// The DONE line is a settle trigger: without it an unsettled close-marker
	// scan keeps the block undecided anyway, so the defect needs a mid-stream
	// line the close-marker reader reads as definitively-not-junk.
	cut := len(xOpen) + len(`<function={"tool":"Bash","parameters":{"command":"ls"}}`)
	withMark := xOpen + `<function={"tool":"Bash","parameters":{"command":"ls"}}` + "\nDONE" + `>` + xClose + "\nafter prose"

	emitted, pending := streamFeed(tools, []string{
		withMark[:cut+len("\nDONE")],
		withMark[cut+len("\nDONE"):],
	})
	if strings.Contains(emitted, "DONE") && !strings.Contains(emitted, xOpen) {
		t.Fatalf("mid-stream suppression consumed an unstable block, emitted = %q", emitted)
	}
	_, flushKeeps, err := ParseActionBlocks(pending, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(emitted + flushKeeps)
	if want := strings.TrimSpace(withMark); got != want {
		t.Fatalf("sharded replay must reconstruct the finally-rejected block:\n got %q\nwant %q", got, want)
	}
	// The fallback span the unstable verdict wanted to delete is still there
	// after the final verdict lands.
	if !strings.Contains(got, `"tool":"Bash"`) {
		t.Fatalf("the JSON fallback span must survive the final verdict, got %q", got)
	}

	// The same text byte by byte: every split must reconstruct, not just the
	// hand-picked one.
	for shard := 1; shard <= 7; shard += 3 {
		if got := c2StreamReplay(t, tools, withMark, shard); got != strings.TrimSpace(withMark) {
			t.Fatalf("shard=%d replay = %q, want %q", shard, got, strings.TrimSpace(withMark))
		}
	}
}

// --- TOOLS-13: hybrid close marker sharing its line with prose --------------

// TestC2Tools1005HybridCloseMarkerSharesLineWithProse pins the low-severity
// residue: a close marker that shares its line with prose must have its exact
// prefix consumed while the rest of the line survives — not the whole line
// deleted, and not the marker left in the client's text.
func TestC2Tools1005HybridCloseMarkerSharesLineWithProse(t *testing.T) {
	tools := xmlTools()
	body := `{"tool":"Bash","parameters":{"command":"ls"}}`
	text := xOpen + "\n" + body + "\n" + xClose + "同一行后续正文 must survive"

	calls, clean, err := ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Arguments["command"] != "ls" {
		t.Fatalf("the call must still parse, calls = %+v", calls)
	}
	if want := "同一行后续正文 must survive"; strings.TrimSpace(clean) != want {
		t.Fatalf("the marker prefix must be consumed and the same-line prose kept:\n got %q\nwant %q", clean, want)
	}
	if strings.Contains(clean, xClose) {
		t.Fatalf("the close marker must not leak into prose, clean = %q", clean)
	}

	// Byte-by-byte replay: the same verdict under every split.
	for shard := 1; shard <= 5; shard += 2 {
		if got := c2StreamReplay(t, tools, text, shard); got != "同一行后续正文 must survive" {
			t.Fatalf("shard=%d replay = %q", shard, got)
		}
	}
}

// TestC2Tools1005FenceWithLanguageTagIsNotConsumed guards the counter-case the
// prefix rule must not eat: a fence carrying a language tag opens a new code
// block, so its line is prose and stays whole.
func TestC2Tools1005FenceWithLanguageTagIsNotConsumed(t *testing.T) {
	tools := xmlTools()
	body := `{"tool":"Bash","parameters":{"command":"ls"}}`
	text := xOpen + "\n" + body + "\n\n```go\nfmt.Println(1)\n```\ntail"

	calls, clean, err := ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("the call must parse, calls = %+v", calls)
	}
	if want := "```go\nfmt.Println(1)\n```\ntail"; strings.TrimSpace(clean) != want {
		t.Fatalf("the code fence must survive whole:\n got %q\nwant %q", clean, want)
	}
}

// --- TOOLS-11: smart-quote repair tells structure from payload --------------

// TestC2Tools1005SmartQuoteRepairKeepsPayloadQuotes pins the repair order: a
// body that only needs the minimal fixes (trailing commas, stray escapes)
// must never run the quote rewrite, because smart quotes inside a legal
// ASCII-quoted string are payload the global replace used to destroy.
func TestC2Tools1005SmartQuoteRepairKeepsPayloadQuotes(t *testing.T) {
	tools := xmlTools()
	// The trailing comma forces the tolerant path; the smart quotes are legal
	// payload inside the command value and must come out byte for byte.
	body := "{\"tool\":\"Bash\",\"parameters\":{\"command\":\"echo “你好” 🎉\",},}"
	text := "```json action\n" + body + "\n```"

	calls, _, err := ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("the minimal repair must recover the call, calls = %+v (clean kept)", calls)
	}
	if got := calls[0].Arguments["command"]; got != "echo “你好” 🎉" {
		t.Fatalf("payload smart quotes must survive the repair, command = %q", got)
	}

	// Sharded: the same body arriving one shard at a time reaches the same
	// verdict — nothing of the call reaches the client, and the withheld
	// buffer's final parse consumes it wholly.
	for shard := 1; shard <= 5; shard += 2 {
		emitted, _ := streamFeed(tools, splitEvery(text, shard))
		if strings.Contains(emitted, "你好") {
			t.Fatalf("shard=%d: the call leaked as prose, emitted = %q", shard, emitted)
		}
		if got := c2StreamReplay(t, tools, text, shard); got != "" {
			t.Fatalf("shard=%d: replay = %q, want everything consumed", shard, got)
		}
	}
}

// TestC2Tools1005SmartQuoteRepairStillFixesStructure guards the positive half:
// smart quotes used as JSON delimiters are structure and still get rewritten.
func TestC2Tools1005SmartQuoteRepairStillFixesStructure(t *testing.T) {
	tools := xmlTools()
	body := "{“tool”:“Bash”,“parameters”:{“command”:“ls”},}"
	text := "```json action\n" + body + "\n```"

	calls, clean, err := ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Arguments["command"] != "ls" {
		t.Fatalf("structural smart quotes must still be repaired, calls = %+v", calls)
	}
	if clean != "" {
		t.Fatalf("clean = %q, want the block consumed", clean)
	}
}

// --- TOOLS-12 companion: sharded replay of the mixed-dialect corpus ---------

// TestC2Tools1005ShardedReplayOfHybridCorpus runs the whole hybrid corpus
// shard by shard: for every fixture the streamed-plus-Flush prose must equal
// the one-shot clean text — the reconstruction check the single-buffer verdict
// never performed.
func TestC2Tools1005ShardedReplayOfHybridCorpus(t *testing.T) {
	tools := xmlTools()
	for _, tc := range hybridCases() {
		for _, shard := range []int{1, 3, 8} {
			emitted, pending := streamFeed(tools, splitEvery(tc.text, shard))
			_, flushKeeps, err := ParseActionBlocks(pending, tools, Config{})
			if err != nil {
				t.Fatalf("%s shard=%d: %v", tc.name, shard, err)
			}
			got := strings.TrimSpace(emitted + flushKeeps)
			if got != strings.TrimSpace(tc.wantClean) {
				t.Fatalf("%s shard=%d: replay = %q, want %q", tc.name, shard, got, strings.TrimSpace(tc.wantClean))
			}
		}
	}
}
