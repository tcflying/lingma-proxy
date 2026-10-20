package toolemulation

import (
	"strings"
	"testing"
)

// P2 boundary regressions for inferToolFromArgsBody: inference is a last
// resort, not a shortcut. It may only name a call when the args-only object is
// the block's last object, and it must never guess from an empty argument
// object. A named call behind the args-only object always wins, on the
// one-shot parse and on the streamed final parse alike.

// TestP2InferenceDefersToNamedCall pins the P2 defect the review found: an
// args-only object followed by a real {"tool":...} call must not let inference
// fire first. The named call is the block's answer, and the inferred edit the
// model may have sketched in front of it must not run.
func TestP2InferenceDefersToNamedCall(t *testing.T) {
	tools := inferTools1006()
	text := "```json action\n" +
		`{"parameters":{"file_path":"a.py","old_string":"x","new_string":"inferred"}}` + "\n" +
		`{"tool":"edit","parameters":{"file_path":"a.py","old_string":"x","new_string":"named"}}` + "\n```"

	calls, clean, err := ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("the block must yield exactly the named call, calls = %+v", calls)
	}
	if calls[0].Name != "edit" {
		t.Fatalf("call name = %q, want edit", calls[0].Name)
	}
	if got := calls[0].Arguments["new_string"]; got != "named" {
		t.Fatalf("new_string = %#v, the named call must win over the inferred one", got)
	}
	if strings.Contains(clean, `"parameters"`) || strings.Contains(clean, `"tool"`) {
		t.Fatalf("the consumed objects must not leak as prose, clean = %q", clean)
	}

	// Streaming: the args-only object may not release an inferred call while
	// the named call can still arrive behind it.
	emitted, held := streamFeed(tools, splitEvery(text, 7))
	if strings.Contains(emitted, "inferred") {
		t.Fatalf("inference leaked mid-stream before Flush: emitted = %q", emitted)
	}
	calls, _, err = ParseActionBlocks(emitted+held, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Name != "edit" {
		t.Fatalf("the Flush parse must keep the named call, calls = %+v", calls)
	}
	if got := calls[0].Arguments["new_string"]; got != "named" {
		t.Fatalf("Flush new_string = %#v, the named call must win over the inferred one", got)
	}
}

// TestP2InferenceRefusesEmptyParameters pins the other half of the guard: an
// empty carrier names nothing, so every empty form stays a fragment in the
// prose instead of becoming a guessed call.
func TestP2InferenceRefusesEmptyParameters(t *testing.T) {
	// Every inferTools1006 tool declares its properties as required, so the
	// required-argument gate alone would refuse an empty object and the guard
	// would go untested. The extra tool has properties but no required ones:
	// only the empty-carrier guard keeps {"parameters":{}} from matching it.
	tools := append(inferTools1006(),
		xmlSchemaTool("echo", map[string]any{"text": map[string]any{"type": "string"}}))
	for _, key := range []string{"parameters", "arguments", "input"} {
		body := `{"` + key + `":{}}`
		text := "```json action\n" + body + "\n```"
		calls, clean, err := ParseActionBlocks(text, tools, Config{})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		if len(calls) != 0 {
			t.Fatalf("%s: an empty carrier must never name a tool, calls = %+v", key, calls)
		}
		if !strings.Contains(clean, `"`+key+`"`) {
			t.Fatalf("%s: the block must stay in the prose, clean = %q", key, clean)
		}
	}
}

// TestP2NoteObjectStillStepsOverToNamedCall is the positive control: the
// existing step-over of a tool-less note object must keep reaching the named
// call behind it.
func TestP2NoteObjectStillStepsOverToNamedCall(t *testing.T) {
	tools := inferTools1006()
	text := "```json action\n" +
		`{"note":"nothing to run"}` + "\n" +
		`{"tool":"edit","parameters":{"file_path":"a.py","old_string":"x","new_string":"named"}}` + "\n```"

	calls, clean, err := ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Name != "edit" {
		t.Fatalf("the note object must step over to the named edit, calls = %+v", calls)
	}
	if got := calls[0].Arguments["new_string"]; got != "named" {
		t.Fatalf("new_string = %#v, want the named call's value", got)
	}
	if strings.Contains(clean, "nothing to run") || strings.Contains(clean, `"tool"`) {
		t.Fatalf("the consumed block must not leak as prose, clean = %q", clean)
	}
}
