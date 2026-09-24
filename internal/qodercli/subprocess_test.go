package qodercli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/remote"
)

// The CLI subprocess is faked by re-executing this test binary in a mode that only
// prints argv and stdout shapes. Nothing here may launch the real Qoder CLI or read
// real credentials.
const (
	fakeCLIModeEnv = "LINGMA_QODERCLI_TEST_FAKE_MODE"
	fakeCLIArgvEnv = "LINGMA_QODERCLI_TEST_FAKE_ARGV"

	// fakeRejectsPartialFlag is an install old enough to refuse
	// --include-partial-messages, which is what the retry path must survive.
	fakeRejectsPartialFlag = "rejects-partial-flag"
	// fakeHugeLine prints one line past the scan buffer cap, then keeps writing.
	fakeHugeLine = "huge-line"
	// fakeSilentExit dies with a status and prints nothing anywhere.
	fakeSilentExit = "silent-exit"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeCLIModeEnv); mode != "" {
		runFakeCLI(mode)
		panic("unreachable")
	}
	os.Exit(m.Run())
}

func runFakeCLI(mode string) {
	if path := os.Getenv(fakeCLIArgvEnv); path != "" {
		line, _ := json.Marshal(os.Args[1:])
		if f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			f.Write(append(line, '\n'))
			f.Close()
		}
	}
	switch mode {
	case fakeRejectsPartialFlag:
		if hasArg(os.Args[1:], "--include-partial-messages") {
			fmt.Fprintln(os.Stderr, "error: unknown option '--include-partial-messages'")
			os.Exit(2)
		}
		fmt.Println(`{"type":"result","subtype":"success","result":"answered without partial frames"}`)
	case fakeHugeLine:
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"前半段"}]}}`)
		// One line past the cap the test installed, with plenty left over to fill
		// the OS pipe once the scan stops reading.
		os.Stdout.WriteString(strings.Repeat("x", 1<<20))
		fmt.Println()
		fmt.Println(`{"type":"result","subtype":"success","result":"这一帧永远读不到"}`)
	case fakeSilentExit:
		os.Exit(7)
	default:
		fmt.Fprintf(os.Stderr, "unknown fake CLI mode %q\n", mode)
		os.Exit(9)
	}
	os.Exit(0)
}

func hasArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

// fakeCLIClient points a Client at this test binary, including a private
// --config-dir exactly like a real site location has.
func fakeCLIClient(t *testing.T, mode string, timeout time.Duration) *Client {
	t.Helper()
	t.Setenv(fakeCLIModeEnv, mode)
	t.Setenv("LINGMA_QODERCLI_JOB_TOKEN", `{"token":"test-job-token"}`)
	return NewClient(Location{
		HostExe:   os.Args[0],
		Site:      SiteCN,
		EnvPrefix: SiteCN.profile().envPrefix,
		ConfigDir: t.TempDir(),
	}, timeout)
}

// TestChatRetryKeepsValuelessToolsPair is Q1: the fallback retry meant to rescue a
// CLI that rejects --include-partial-messages sliced one element off the end of the
// argv. That element is the empty value of "--tools", so the rejected flag stayed,
// --tools was left dangling to eat whatever the runner appends next (--config-dir),
// and the rescue run failed just as hard as the first one.
func TestChatRetryKeepsValuelessToolsPair(t *testing.T) {
	prev := partialStreamUnsupported.Swap(false)
	t.Cleanup(func() { partialStreamUnsupported.Store(prev) })

	argvPath := t.TempDir() + string(os.PathSeparator) + "argv.jsonl"
	t.Setenv(fakeCLIArgvEnv, argvPath)
	// Generous: this box runs ten agents at once and a single process start has
	// measured 45s, so the budget is about the runner, not about the fix.
	c := fakeCLIClient(t, fakeRejectsPartialFlag, 240*time.Second)

	var streamed strings.Builder
	result, err := c.Chat(context.Background(), remote.ChatRequest{
		Prompt: "hi",
		Model:  "Qwen3.8-Flash",
	}, func(text string) { streamed.WriteString(text) })
	if err != nil {
		t.Fatalf("the retry was supposed to rescue this request: %v", err)
	}
	if result.Text != "answered without partial frames" {
		t.Fatalf("text = %q", result.Text)
	}
	if streamed.String() != result.Text {
		t.Fatalf("the delta callback got %q, want the retried answer", streamed.String())
	}

	runs := readArgvRuns(t, argvPath)
	if len(runs) != 2 {
		t.Fatalf("expected the rejected run plus one retry, got %d runs: %v", len(runs), runs)
	}
	if !hasArg(runs[0], "--include-partial-messages") {
		t.Fatalf("the first run must ask for partial frames, got %v", runs[0])
	}
	want := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--permission-mode", "bypass_permissions",
		"--max-turns", "1",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
		"--model", "Qwen3.8-Flash",
		"--tools", "",
		"--config-dir", c.loc.ConfigDir,
	}
	if strings.Join(runs[1], "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("retry argv drifted:\n got %q\nwant %q", runs[1], want)
	}
}

func readArgvRuns(t *testing.T, path string) [][]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	var runs [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatalf("recorded argv line %q: %v", line, err)
		}
		runs = append(runs, args)
	}
	return runs
}

// TestChatSurvivesALinePastTheScanBuffer is Q2: when one output line exceeded the
// 8 MB scanner cap the scan stopped silently while the CLI kept writing, the pipe
// filled, and cmd.Wait blocked until the deadline -- an answer that had already
// been produced was reported as a timeout.
func TestChatSurvivesALinePastTheScanBuffer(t *testing.T) {
	prev := partialStreamUnsupported.Swap(false)
	t.Cleanup(func() { partialStreamUnsupported.Store(prev) })

	// Reaching the production 8 MB ceiling would mean moving 8 MB through the pipe,
	// which this box measures in tens of seconds; the ceiling itself is what the
	// fix keys off, so the test lowers it and passes a line just past it.
	capPrev := maxCLIOutputLineBytes
	maxCLIOutputLineBytes = 64 * 1024
	t.Cleanup(func() { maxCLIOutputLineBytes = capPrev })

	t.Setenv(fakeCLIArgvEnv, "")
	c := fakeCLIClient(t, fakeHugeLine, 120*time.Second)

	result, err := c.Chat(context.Background(), remote.ChatRequest{
		Prompt: "hi",
		Model:  "Qwen3.8-Flash",
	}, func(string) {})
	if err != nil {
		t.Fatalf("a finished answer must not become %v", err)
	}
	// Only the frames before the oversized line can be read, so the result frame
	// after it never arrives: that is the proof the scan really stopped early.
	if result == nil || result.Text != "前半段" {
		t.Fatalf("text = %#v, want the frames read before the oversized line", result)
	}
}

// TestSilentNonZeroExitReportsItsStatus is Q3: the last error branch wrapped the
// credential error from the top of the function, which is always nil by then, so
// the user got "%!w(<nil>)" and the real exit status was lost.
func TestSilentNonZeroExitReportsItsStatus(t *testing.T) {
	t.Setenv(fakeCLIArgvEnv, "")
	c := fakeCLIClient(t, fakeSilentExit, 180*time.Second)

	_, err := c.run(context.Background(), "--print")
	if err == nil {
		t.Fatal("a non-zero exit with no output must be an error")
	}
	if !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("error must carry the exit status, got %q", err)
	}
	if strings.Contains(err.Error(), "%!w(<nil>)") {
		t.Fatalf("the stale credential error was wrapped again: %q", err)
	}
}
