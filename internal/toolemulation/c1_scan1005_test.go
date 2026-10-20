package toolemulation

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 1005 C-1 (TOOLS-02 + HIST-10): the one-shot scan budget used to truncate the
// response text itself, silently dropping everything past the cap — and the
// byte cut could land inside a multi-byte UTF-8 character. The budget must
// bound only the search for parse candidates: confirmed calls inside the
// budget are removed, the unscanned suffix is appended back to the returned
// prose verbatim, the boundary never splits a rune, and a candidate that only
// exists past the budget stays prose exactly as the streaming side holds
// undecided text.

// TestC1Scan1005BudgetKeepsUnscannedTailAsProse pins the tail: a turn whose
// prose runs past the cap keeps every byte of it in the returned text while
// the in-budget call still parses.
func TestC1Scan1005BudgetKeepsUnscannedTailAsProse(t *testing.T) {
	tools := xmlTools()
	call := "```json action\n" + `{"tool":"Bash","parameters":{"command":"ls"}}` + "\n```"
	tail := "预算之外的收尾正文：中文与 emoji 🎉 必须原样回到正文。" + strings.Repeat("尾部保真", 64)
	text := "head\n" + call + "\n" + tail

	calls, clean, err := ParseActionBlocks(text, tools, Config{MaxScanBytes: len("head\n"+call) + 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Arguments["command"] != "ls" {
		t.Fatalf("the in-budget call must still parse, calls = %+v", calls)
	}
	if want := "head\n" + tail; strings.TrimSpace(clean) != want {
		t.Fatalf("unscanned tail must survive verbatim:\n got %q\nwant %q", clean, want)
	}
	if strings.Contains(clean, "\uFFFD") {
		t.Fatalf("the scan boundary must not manufacture U+FFFD, clean = %q", clean)
	}
	if !utf8.ValidString(clean) {
		t.Fatalf("returned prose must stay valid UTF-8, clean = %q", clean)
	}
}

// TestC1Scan1005OutOfBudgetCandidateStaysProse pins the out-of-budget
// candidate policy: a complete second call past the cap is not scanned, so it
// is prose — the same verdict the streaming side reaches for a candidate it
// can never confirm.
func TestC1Scan1005OutOfBudgetCandidateStaysProse(t *testing.T) {
	tools := xmlTools()
	first := "```json action\n" + `{"tool":"Bash","parameters":{"command":"ls"}}` + "\n```"
	second := "```json action\n" + `{"tool":"Bash","parameters":{"command":"pwd"}}` + "\n```"
	text := first + "\nmid\n" + second + "\ntail"

	calls, clean, err := ParseActionBlocks(text, tools, Config{MaxScanBytes: len(first) + 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Arguments["command"] != "ls" {
		t.Fatalf("only the in-budget call may parse, calls = %+v", calls)
	}
	want := "mid\n" + second + "\ntail"
	if strings.TrimSpace(clean) != want {
		t.Fatalf("the out-of-budget candidate must stay prose verbatim:\n got %q\nwant %q", clean, want)
	}
	if !utf8.ValidString(clean) {
		t.Fatalf("returned prose must stay valid UTF-8, clean = %q", clean)
	}
}

// TestC1Scan1005BoundaryNeverSplitsRune pins the byte cut: with the cap
// landing in the middle of a 4-byte emoji, the boundary backs off to the rune
// start and the character reassembles whole in the returned prose.
func TestC1Scan1005BoundaryNeverSplitsRune(t *testing.T) {
	tools := xmlTools()
	call := "```json action\n" + `{"tool":"Bash","parameters":{"command":"ls"}}` + "\n```"
	prefix := "head\n" + call + "\n"
	text := prefix + "🎉🎉🎉 tail"

	// Two bytes into the first emoji: a naive text[:cap] cut would leave two
	// stray continuation bytes at the end of the scanned region.
	calls, clean, err := ParseActionBlocks(text, tools, Config{MaxScanBytes: len(prefix) + 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("the in-budget call must still parse, calls = %+v", calls)
	}
	if want := "head\n🎉🎉🎉 tail"; strings.TrimSpace(clean) != want {
		t.Fatalf("a rune-straddling boundary must reassemble whole:\n got %q\nwant %q", clean, want)
	}
	if strings.Contains(clean, "\uFFFD") || !utf8.ValidString(clean) {
		t.Fatalf("the boundary must not damage UTF-8, clean = %q", clean)
	}
}

// TestC1Scan1005ChineseAcrossBoundaryEveryOffset sweeps every cut offset
// through a Chinese+emoji tail: no matter where the cap lands, the returned
// prose ends with the original tail verbatim and never carries U+FFFD or
// invalid UTF-8. (A cap that lands between a confirmed body and its close
// syntax accepts the call with the close markers left as visible text — a
// boundary artifact recorded in ITERATION.md — so the invariant is the tail,
// not the exact whole string.)
func TestC1Scan1005ChineseAcrossBoundaryEveryOffset(t *testing.T) {
	tools := xmlTools()
	call := "```json action\n" + `{"tool":"Bash","parameters":{"command":"ls"}}` + "\n```"
	tail := "中文跨边界用例，🎉 表情横跨截断点。"
	text := "head\n" + call + "\n" + tail

	for cap := 1; cap <= len(text); cap++ {
		calls, clean, err := ParseActionBlocks(text, tools, Config{MaxScanBytes: cap})
		if err != nil {
			t.Fatalf("cap=%d: %v", cap, err)
		}
		if strings.Contains(clean, "\uFFFD") {
			t.Fatalf("cap=%d: U+FFFD manufactured, clean = %q", cap, clean)
		}
		if !utf8.ValidString(clean) {
			t.Fatalf("cap=%d: invalid UTF-8, clean = %q", cap, clean)
		}
		if len(calls) > 1 {
			t.Fatalf("cap=%d: calls = %+v", cap, calls)
		}
		if !strings.HasSuffix(strings.TrimSpace(clean), tail) {
			t.Fatalf("cap=%d: the tail must survive verbatim, clean = %q", cap, clean)
		}
	}
}
