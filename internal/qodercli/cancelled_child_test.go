package qodercli

import (
	"context"
	"strings"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/remote"
)

// TestTimeoutCarriesTheChildsOwnAccountOfWhyItDied is the end-to-end version of the
// note test: a real child process, a real deadline, and the error an operator would
// actually be handed.
//
// Before the fix this error was the same sentence for every possible cause -- a
// rejected credential, a lost session, a startup crash -- so a failing box could
// only be diagnosed by attaching a debugger to the process. Two facts have to
// survive into it here: that the child wrote something before it stalled, and what
// it said on the way out.
func TestTimeoutCarriesTheChildsOwnAccountOfWhyItDied(t *testing.T) {
	c := fakeCLIClient(t, fakeShortWriteThenHang, 3*time.Second)

	_, err := c.ListModels(context.Background())
	if err == nil {
		t.Fatal("ListModels succeeded against a child that never finished")
	}
	msg := err.Error()

	if !strings.Contains(msg, "timed out") {
		t.Fatalf("error no longer names the deadline, so it reads as a cause: %q", msg)
	}
	if !strings.Contains(msg, "bytes to stdout") {
		t.Errorf("error carries no stdout size, so 'it answered nothing' and 'it answered and we cut it off' stay indistinguishable: %q", msg)
	}
	if !strings.Contains(msg, "could not mint a session token") {
		t.Errorf("error lost the child's own reason from stderr: %q", msg)
	}
	// The stderr line is a cause, not a licence to quote the whole transcript.
	if strings.Contains(msg, "partial") {
		t.Errorf("error quoted the stdout payload back instead of summarising it: %q", msg)
	}
}

// TestChatTimeoutCarriesTheSameEvidence keeps the guarantee on the chat path, which
// is where a stranded request is the expensive one: the caller has already spent
// its whole request on this child.
func TestChatTimeoutCarriesTheSameEvidence(t *testing.T) {
	c := fakeCLIClient(t, fakeShortWriteThenHang, 3*time.Second)

	_, err := c.Chat(context.Background(), remote.ChatRequest{
		Prompt: "hi",
		Model:  "Qwen3.8-Flash",
	}, nil)
	if err == nil {
		t.Fatal("Chat succeeded against a child that never finished")
	}
	if !strings.Contains(err.Error(), "could not mint a session token") {
		t.Errorf("chat error lost the child's reason: %q", err.Error())
	}
}

// TestCancelledChildNoteKeepsTheCluesThatDiagnoseTheHang pins the fix for the
// failure this package kept producing on real hardware: a CLI child that writes a
// hundred-odd bytes and then closes its pipe, so the caller only ever saw
// "cancelled before it finished: context deadline exceeded" and had no way to tell
// a rejected credential, a lost session and a startup crash apart.
//
// Two things have to survive into the error. The stderr tail, which names the
// cause. And the stdout byte count, which distinguishes "the child said nothing"
// from "the child said a lot and we still cut it off" -- those two need opposite
// responses and used to be indistinguishable.
func TestCancelledChildNoteKeepsTheCluesThatDiagnoseTheHang(t *testing.T) {
	cases := []struct {
		name       string
		stdoutLen  int
		stderr     string
		wantBytes  string
		wantStderr string
		wantAbsent []string
	}{
		{
			name:       "the known short-write shape reports its size and its reason",
			stdoutLen:  176,
			stderr:     "Error: session token expired, run `qoder login`\n",
			wantBytes:  "176 bytes",
			wantStderr: "session token expired",
		},
		{
			name:       "a child that said nothing still reports zero, not silence",
			stdoutLen:  0,
			stderr:     "",
			wantBytes:  "0 bytes",
			wantStderr: "",
			// No stderr section at all: an empty one would read as "the child told us
			// nothing" when it actually means "there was nothing to read".
			wantAbsent: []string{"stderr"},
		},
		{
			name:       "a long answer cut off is distinguishable from a short one",
			stdoutLen:  4096,
			stderr:     "Warning: runtime fell back to a bundled build\n",
			wantBytes:  "4096 bytes",
			wantStderr: "",
			// "Warning:" is in cliNoisePrefixes: it prints on every run, so quoting
			// it would bury the one line that matters.
			wantAbsent: []string{"runtime fell back"},
		},
		{
			name:       "the runtime's own crashpad log is not mistaken for the cause",
			stdoutLen:  176,
			stderr:     "[0920/043244.997:ERROR:third_party\\crashpad\\util\\win\\x:108] CreateFile: nope\nfatal: port already bound\n",
			wantBytes:  "176 bytes",
			wantStderr: "port already bound",
			wantAbsent: []string{"crashpad"},
		},
		{
			name:       "several causes are all kept, tail first",
			stdoutLen:  12,
			stderr:     "warn: slow disk\nfatal: could not bind port\nfatal: aborting\n",
			wantBytes:  "12 bytes",
			wantStderr: "could not bind port",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cancelledChildNote(tc.stdoutLen, tc.stderr)
			if !strings.Contains(got, tc.wantBytes) {
				t.Errorf("note %q does not mention %q", got, tc.wantBytes)
			}
			if tc.wantStderr != "" && !strings.Contains(got, tc.wantStderr) {
				t.Errorf("note %q lost the stderr cause %q", got, tc.wantStderr)
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("note %q contains %q, which should have been filtered out", got, absent)
				}
			}
		})
	}
}

// TestCancelledChildNoteIsNeverEmpty guards the reason the whole thing exists: a
// note that renders as "" would let the timeout error go back to being the opaque
// sentence it was before, with nothing to show for the work.
func TestCancelledChildNoteIsNeverEmpty(t *testing.T) {
	if got := cancelledChildNote(0, ""); strings.TrimSpace(got) == "" {
		t.Fatal("cancelledChildNote returned nothing; the timeout error would carry no evidence")
	}
}
