package qodercli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"
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
	// fakeFlood keeps writing well past the capture ceiling.
	fakeFlood = "flood"
	// fakeFloodForever is fakeFlood with no end: the capture ceiling is the only
	// thing that can stop it, which is what makes it the shape a drain hangs on.
	fakeFloodForever = "flood-forever"
	// fakeFloodStderr is the same volume on the diagnostic stream, which had no
	// ceiling of its own at all.
	fakeFloodStderr = "flood-stderr"
	// fakeLingersAfterResult prints its terminal frame and then refuses to leave,
	// which is what 192.168.50.239's CLI does to a finished turn.
	fakeLingersAfterResult = "linger-after-result"
	// fakeShortWriteThenHang is the shape this package kept meeting on real
	// hardware: a little on stdout, the actual reason on stderr, then nothing at
	// all until the caller's deadline cuts it off.
	fakeShortWriteThenHang = "short-write-then-hang"
	// fakeSpawnsTreeThenHang is the cancellation shape: the CLI host spawns a
	// grandchild that inherits stdout and both then refuse to leave. A cancel
	// that only killed the direct child would leave the grandchild holding the
	// write end, so the caller would stay parked on its read.
	fakeSpawnsTreeThenHang = "spawns-tree-then-hang"
	// fakeGrandchildHang is the grandchild half of fakeSpawnsTreeThenHang.
	fakeGrandchildHang = "grandchild-hang"
)

// fakeCLIPidFileEnv, when set for a fakeSpawnsTreeThenHang child, makes it
// record its own pid and the grandchild's pid so the test can watch them die.
const fakeCLIPidFileEnv = "LINGMA_QODERCLI_TEST_FAKE_PID_FILE"

// fakeFloodLine is the line fakeFlood prints. The tests that measure the capture
// ceiling build it from here so a change to the fixture cannot silently move the
// ceiling off a boundary.
var fakeFloodLine = `{"type":"assistant","message":{"content":[{"type":"text","text":"` +
	strings.Repeat("y", 100) + `"}]}}`

// stderrTailMarker is the last thing fakeFloodStderr prints. A capped capture
// keeps the newest bytes, so this is how a test tells a tail from a head.
const stderrTailMarker = "STDERR TAIL MARKER"

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
	case fakeLingersAfterResult:
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"answered, process still here"}]}}`)
		fmt.Println(`{"type":"result","subtype":"success","result":"answered, process still here"}`)
		time.Sleep(2 * time.Minute)
	case fakeShortWriteThenHang:
		// Never reaches a terminal frame, so the caller only ever gets as far as
		// its own deadline. The reason it failed is on stderr and nowhere else --
		// which is the whole point: an operator reading the error has to be able to
		// tell a dead login from a slow disk without attaching a debugger.
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"partial"}]}}`)
		fmt.Fprintln(os.Stderr, "fatal: could not mint a session token (login expired)")
		time.Sleep(10 * time.Minute)
	case fakeSpawnsTreeThenHang:
		// The grandchild inherits stdout, so after the host is gone it is the one
		// keeping the caller's pipe read from seeing EOF. This is the shape a
		// cancelled real CLI turn leaves behind on Windows.
		grandchild := exec.Command(os.Args[0])
		grandchild.Env = append(os.Environ(), fakeCLIModeEnv+"="+fakeGrandchildHang)
		grandchild.Stdout = os.Stdout
		grandchild.Stderr = os.Stderr
		if err := grandchild.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "grandchild start failed: %v\n", err)
			os.Exit(8)
		}
		if path := os.Getenv(fakeCLIPidFileEnv); path != "" {
			_ = os.WriteFile(path, []byte(fmt.Sprintf("%d\n%d\n", os.Getpid(), grandchild.Process.Pid)), 0o600)
		}
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"tree"}]}}`)
		time.Sleep(10 * time.Minute)
	case fakeGrandchildHang:
		time.Sleep(10 * time.Minute)
	case fakeSilentExit:
		os.Exit(7)
	case fakeFlood, fakeFloodForever:
		// Past whatever capture ceiling the test installed, with a terminal frame at
		// the end that must never be reached. The forever half has no end at all:
		// the ceiling is the only thing that can stop it.
		for i := 0; i < 4000; i++ {
			fmt.Println(fakeFloodLine)
		}
		if mode == fakeFloodForever {
			// Long enough that any test budget is shorter than the child's own life,
			// so "the turn ended" can only mean something stopped the child.
			time.Sleep(2 * time.Minute)
		}
		fmt.Println(`{"type":"result","subtype":"success","result":"beyond the ceiling"}`)
	case fakeFloodStderr:
		// The same volume on stderr, which is where an error loop that cannot get
		// its complaint onto stdout ends up. It is one long line on purpose: the
		// error message keeps the last few lines, and a flood of short lines would
		// fit in that window whatever the capture did.
		fmt.Fprintln(os.Stderr, strings.Repeat("e", 2*1024*1024))
		fmt.Fprintln(os.Stderr, stderrTailMarker)
		os.Exit(3)
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
	return fakeCLIClientFor(t, SiteCN, mode, timeout)
}

// fakeCLIClientFor is the per-site shape: the service builds one client per site,
// which is what the streaming latch and its degradation are scoped to.
func fakeCLIClientFor(t *testing.T, site Site, mode string, timeout time.Duration) *Client {
	t.Helper()
	t.Setenv(fakeCLIModeEnv, mode)
	// The operator-supplied job token is a single package-level env var, not a
	// per-site one, so both sites in a test read the same credential.
	t.Setenv(jobTokenEnv, `{"token":"test-job-token"}`)
	return NewClient(Location{
		HostExe:   os.Args[0],
		Site:      site,
		EnvPrefix: site.profile().envPrefix,
		ConfigDir: t.TempDir(),
	}, timeout)
}

// TestChatRetryKeepsValuelessToolsPair is Q1: the fallback retry meant to rescue a
// CLI that rejects --include-partial-messages sliced one element off the end of the
// argv. That element is the empty value of "--tools", so the rejected flag stayed,
// --tools was left dangling to eat whatever the runner appends next (--config-dir),
// and the rescue run failed just as hard as the first one.
func TestChatRetryKeepsValuelessToolsPair(t *testing.T) {
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

// TestCapturedOutputStopsAtTheCeiling is the volume half of the same pipe: the
// deadline bounds how long the child may talk, not how fast, so a CLI in an error
// loop could push hundreds of megabytes into the capture buffer inside one timeout.
func TestCapturedOutputStopsAtTheCeiling(t *testing.T) {
	t.Setenv(fakeCLIArgvEnv, "")

	prev := maxCLICapturedOutputBytes
	maxCLICapturedOutputBytes = 64 * 1024
	t.Cleanup(func() { maxCLICapturedOutputBytes = prev })

	c := fakeCLIClient(t, fakeFlood, 60*time.Second)
	text, err := c.runWithStdinData(context.Background(), nil, func(string) {}, "--print")
	if err != nil {
		t.Fatalf("a capped stream must still finish cleanly: %v", err)
	}
	if !strings.Contains(text, `"type":"assistant"`) {
		t.Fatalf("nothing was captured: %q", text[:min(len(text), 200)])
	}
	if strings.Contains(text, "beyond the ceiling") {
		t.Fatal("the terminal frame after the ceiling was captured, so no cap applied")
	}
	if len(text) > maxCLICapturedOutputBytes+512 {
		t.Fatalf("captured %d bytes, want the %d byte ceiling held", len(text), maxCLICapturedOutputBytes)
	}
}

// TestCaptureCeilingHoldsToTheByte is the same ceiling with the slack taken out:
// the check ran before the write and counted the line without the newline it
// wrote next, so a turn could finish one byte past the cap. The cap here is one
// byte short of a whole number of lines, which is where that byte shows up.
func TestCaptureCeilingHoldsToTheByte(t *testing.T) {
	t.Setenv(fakeCLIArgvEnv, "")

	lineBytes := len(fakeFloodLine) + 1
	prev := maxCLICapturedOutputBytes
	maxCLICapturedOutputBytes = 3*lineBytes - 1
	t.Cleanup(func() { maxCLICapturedOutputBytes = prev })

	c := fakeCLIClient(t, fakeFlood, 60*time.Second)
	text, err := c.runWithStdinData(context.Background(), nil, func(string) {}, "--print")
	if err != nil {
		t.Fatalf("a capped stream must still finish cleanly: %v", err)
	}
	if !strings.Contains(text, `"type":"assistant"`) {
		t.Fatalf("nothing was captured: %q", text)
	}
	if len(text) > maxCLICapturedOutputBytes {
		t.Fatalf("captured %d bytes against a %d byte ceiling", len(text), maxCLICapturedOutputBytes)
	}
}

// TestStderrCaptureIsCappedAndKeepsTheTail is the diagnostic half of the volume
// problem: stdout had two ceilings and stderr had none, so an error loop that
// could not get its complaint onto stdout filled memory at whatever rate it
// liked. A cap that kept the head would be useless -- the cause of a failure is
// in what the CLI printed last.
func TestStderrCaptureIsCappedAndKeepsTheTail(t *testing.T) {
	t.Setenv(fakeCLIArgvEnv, "")

	prev := maxCLICapturedStderrBytes
	maxCLICapturedStderrBytes = 32 * 1024
	t.Cleanup(func() { maxCLICapturedStderrBytes = prev })

	c := fakeCLIClient(t, fakeFloodStderr, 120*time.Second)
	_, err := c.run(context.Background(), "--print")
	if err == nil {
		t.Fatal("a child that exited 3 must be an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, stderrTailMarker) {
		t.Fatalf("the capture kept the head of stderr and lost the cause at the end: %q", msg)
	}
	// The fake wrote 2 MB; anything near that is an uncapped capture.
	if len(msg) > 64*1024 {
		t.Fatalf("the error quotes %d bytes of stderr against a 32 KB cap", len(msg))
	}
}

// TestStdoutFallbackCaptureIsCapped covers the other uncapped path: when
// StdoutPipe cannot be opened the child writes straight into the buffer and
// nothing checks the turn ceiling any more, so the cap has to be in the writer.
func TestStdoutFallbackCaptureIsCapped(t *testing.T) {
	t.Setenv(fakeCLIArgvEnv, "")

	prevPipe := openStdoutPipe
	openStdoutPipe = func(*exec.Cmd) (io.ReadCloser, error) {
		return nil, errors.New("no pipes left on this box")
	}
	t.Cleanup(func() { openStdoutPipe = prevPipe })

	prevCap := maxCLICapturedOutputBytes
	maxCLICapturedOutputBytes = 64 * 1024
	t.Cleanup(func() { maxCLICapturedOutputBytes = prevCap })

	c := fakeCLIClient(t, fakeFlood, 60*time.Second)
	text, err := c.runWithStdinData(context.Background(), nil, func(string) {}, "--print")
	if err != nil {
		t.Fatalf("the fallback capture must still finish cleanly: %v", err)
	}
	if len(text) == 0 {
		t.Fatal("nothing was captured through the fallback")
	}
	// The tail writer trims on a band, so a capture that just missed a trim can
	// sit at twice the cap; the flood is 560 KB, well past even that.
	if len(text) > 2*maxCLICapturedOutputBytes {
		t.Fatalf("captured %d bytes through a fallback with no scanner, want the %d byte ceiling held",
			len(text), maxCLICapturedOutputBytes)
	}
	if !strings.Contains(text, "beyond the ceiling") {
		t.Error("the fallback capture kept the head of stdout and lost what the CLI said last")
	}
}

// TestTurnBackstopCeilingEndsAWedgedReadWithoutAConfiguredTimeout is the default
// configuration: Timeout is 0, cmd.Cancel is therefore never driven, and a CLI
// that writes a little and then stops used to hold its request -- and one of the
// four CLI slots -- for as long as the client cared to wait.
func TestTurnBackstopCeilingEndsAWedgedReadWithoutAConfiguredTimeout(t *testing.T) {
	t.Setenv(fakeCLIArgvEnv, "")
	t.Setenv(turnCeilingEnv, "2s")

	c := fakeCLIClient(t, fakeShortWriteThenHang, 0)

	done := make(chan error, 1)
	go func() {
		_, err := c.run(context.Background(), "--print")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a wedged read with no configured deadline returned success")
		}
		if !strings.Contains(err.Error(), "backstop ceiling") {
			t.Fatalf("the backstop must name itself, an operator has no Timeout to check: %v", err)
		}
		if !strings.Contains(err.Error(), "could not mint a session token") {
			t.Errorf("the backstop error lost the child's own reason: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the run outlived every bound it was given; nothing ended the wedged read")
	}
}

// TestTurnBackstopCeilingCanBeSwitchedOff pins that the net is a net: an
// operator who sets it to 0 gets a turn with no deadline again, which is what
// the environment variable promises.
func TestTurnBackstopCeilingCanBeSwitchedOff(t *testing.T) {
	t.Setenv(fakeCLIArgvEnv, "")
	t.Setenv(turnCeilingEnv, "0")

	c := fakeCLIClient(t, fakeShortWriteThenHang, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.run(ctx, "--print")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a backstop set to 0 still ended the turn: %v", err)
	case <-time.After(2 * time.Second):
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("run succeeded against a cancelled context")
		}
		if strings.Contains(err.Error(), "backstop") {
			t.Errorf("the cancelled turn blamed the backstop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not return within 10s of the cancel")
	}
}

// TestConfiguredTimeoutOutranksTheBackstop is the other half of "the backstop
// does not change what Timeout means": a turn that was given a deadline keeps
// it, even when the backstop is set well below it.
func TestConfiguredTimeoutOutranksTheBackstop(t *testing.T) {
	t.Setenv(fakeCLIArgvEnv, "")
	t.Setenv(turnCeilingEnv, "1s")

	c := fakeCLIClient(t, fakeShortWriteThenHang, 10*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := c.run(ctx, "--print")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("a turn with a 10 minute deadline was cut off by a 1s backstop: %v", err)
	case <-time.After(3 * time.Second):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run did not return within 10s of the cancel")
	}
}

// TestCaptureCeilingDrainStopsAChildThatNeverFinishes is the hang the backstop
// would otherwise have to wait half an hour for: the capture ceiling stopped the
// scan while the child went on writing, and the drain that follows only ends
// when the child stops. With nothing to stop it, the request never returned.
//
// The child holds for two minutes, so the timing assertion below is not a
// stopwatch race: nothing but the kill can end the turn inside its budget.
func TestCaptureCeilingDrainStopsAChildThatNeverFinishes(t *testing.T) {
	t.Setenv(fakeCLIArgvEnv, "")
	// The backstop is off on purpose: the kill before the drain is what has to
	// end this, and a passing ceiling test would prove nothing.
	t.Setenv(turnCeilingEnv, "0")

	prev := maxCLICapturedOutputBytes
	maxCLICapturedOutputBytes = 64 * 1024
	t.Cleanup(func() { maxCLICapturedOutputBytes = prev })

	c := fakeCLIClient(t, fakeFloodForever, 0)

	const budget = 60 * time.Second
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := c.run(context.Background(), "--print")
		done <- err
	}()

	select {
	case <-done:
		// A turn that ends in the drain is a truncated capture, not a success, and
		// that is the same contract the ceiling already has: the caller is told
		// what the CLI managed to say, and no result frame means no finished turn.
		if took := time.Since(start); took > budget/2 {
			t.Fatalf("the drain took %s, so the turn only ended because the child did", took.Round(time.Millisecond))
		}
	case <-time.After(budget):
		t.Fatal("the drain after the capture ceiling outlived the child it was draining")
	}
}

// TestPartialFlagRefusalStaysInsideTheSiteThatRefusedIt is the process-level
// latch this removed: the CN install refusing the flag said nothing about the
// global install next to it, and the latch took the streaming off both.
func TestPartialFlagRefusalStaysInsideTheSiteThatRefusedIt(t *testing.T) {
	argvPath := t.TempDir() + string(os.PathSeparator) + "argv.jsonl"
	t.Setenv(fakeCLIArgvEnv, argvPath)

	logged := &syncedLogBuffer{}
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	cn := fakeCLIClientFor(t, SiteCN, fakeRejectsPartialFlag, 240*time.Second)
	if _, err := cn.Chat(context.Background(), remote.ChatRequest{Prompt: "hi", Model: "Qwen3.8-Flash"}, func(string) {}); err != nil {
		t.Fatalf("the CN retry was supposed to rescue this request: %v", err)
	}

	runs := readArgvRuns(t, argvPath)
	if len(runs) != 2 {
		t.Fatalf("the CN refusal should cost one retry, got %d runs: %v", len(runs), runs)
	}
	if !strings.Contains(logged.String(), SiteCN.Label()) {
		t.Errorf("the site that lost streaming was not named in the log: %q", logged.String())
	}

	intl := fakeCLIClientFor(t, SiteGlobal, fakeRejectsPartialFlag, 240*time.Second)
	if _, err := intl.Chat(context.Background(), remote.ChatRequest{Prompt: "hi", Model: "m"}, func(string) {}); err != nil {
		t.Fatalf("the global retry was supposed to rescue this request: %v", err)
	}
	runs = readArgvRuns(t, argvPath)
	if len(runs) != 4 {
		t.Fatalf("the global site inherited the CN site's latch and never asked for partial frames: %v", runs)
	}
	if !hasArg(runs[2], "--include-partial-messages") {
		t.Fatalf("the global site's first run must still ask for partial frames, got %v", runs[2])
	}
	if !strings.Contains(logged.String(), SiteGlobal.Label()) {
		t.Errorf("the second site's own refusal was not logged either: %q", logged.String())
	}
}

// TestChatDoesNotWaitForAProcessThatAlreadyAnswered is the 192.168.50.239 chat
// shape: the CLI emits its terminal frame ~50s in (`process.exiting exit_code=0
// uptime_ms=49493`) and keeps its host process alive, while the HTTP request that
// started it was still unanswered at 400s. The answer must not be held hostage to
// the process leaving.
func TestChatDoesNotWaitForAProcessThatAlreadyAnswered(t *testing.T) {
	t.Setenv(fakeCLIArgvEnv, "")
	c := fakeCLIClient(t, fakeLingersAfterResult, 0)

	type outcome struct {
		text string
		err  error
	}
	done := make(chan outcome, 1)
	started := time.Now()
	go func() {
		res, err := c.Chat(context.Background(), remote.ChatRequest{Prompt: "hi", Model: "m"}, nil)
		text := ""
		if res != nil {
			text = res.Text
		}
		done <- outcome{text: text, err: err}
	}()

	// The grace period is five seconds, so anything that has to wait for the kill
	// misses this bar: the read must stop at the frame, not at the teardown.
	budget := 4 * time.Second
	select {
	case got := <-time.After(budget):
		_ = got
		t.Fatalf("Chat did not return within %s of a CLI that emitted its result frame", budget)
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Chat: %v", out.err)
		}
		if out.text != "answered, process still here" {
			t.Fatalf("text = %q, want the terminal frame's answer", out.text)
		}
		if elapsed := time.Since(started); elapsed > budget {
			t.Fatalf("returned in %s, want the read to stop at the frame", elapsed.Round(time.Millisecond))
		}
	}
}

// syncedLogBuffer collects log output from goroutines the test cannot join, like
// the answered-path reaper that outlives the call it belongs to.
type syncedLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncedLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestAnsweredReaperStaysQuietBelowTheSlowLogThreshold pins the quiet half of the
// answered-path reaper: the guard release on the way out of the run kills the
// lingering child, so the background cmd.Wait returns well inside
// cliReaperSlowLogDelay and nothing may be logged. The loud half cannot be
// reached from a test: it needs a child that survives TerminateProcess for ten
// seconds, which Windows does not allow, and the threshold is a production
// constant with no clock to inject. TestChatDoesNotWaitForAProcessThatAlreadyAnswered
// above already proves the reaper never blocks the return.
func TestAnsweredReaperStaysQuietBelowTheSlowLogThreshold(t *testing.T) {
	t.Setenv(fakeCLIArgvEnv, "")

	logged := &syncedLogBuffer{}
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	c := fakeCLIClient(t, fakeLingersAfterResult, 0)
	if _, err := c.Chat(context.Background(), remote.ChatRequest{Prompt: "hi", Model: "m"}, nil); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// The reaper starts when Chat returns, and the slow log lands a full
	// cliReaperSlowLogDelay after that. An observation window that ends at or
	// before the threshold would leave a teardown regression sitting exactly on
	// the edge, where the poll loop leaves first and the test stays green --
	// which is the failure mode this window is sized against.
	window := cliResultTeardownGrace + cliReaperSlowLogDelay + 5*time.Second
	if window <= cliReaperSlowLogDelay {
		t.Fatalf("the observation window %s cannot outlast the %s threshold it is looking for",
			window, cliReaperSlowLogDelay)
	}
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if strings.Contains(logged.String(), "to leave after its result frame") {
			t.Fatalf("the reaper logged inside the %s threshold: %q", cliReaperSlowLogDelay, logged.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
}
