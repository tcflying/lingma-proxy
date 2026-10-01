package qodercli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"lingma-ipc-proxy/internal/remote"
)

// Client drives the bundled Qoder CLI as a subprocess. It exists because the
// inference gateway signs every request from inside the CLI (a WASM module with
// hardware-bound key material), so signing cannot be replayed from Go. One
// client serves one site.
type Client struct {
	loc     Location
	tokens  *TokenSource
	timeout time.Duration
	// partialStreamUnsupported latches when this site's CLI install refuses
	// --include-partial-messages, so one old build does not fail every streamed
	// turn. It is per site and not per process: the flag is worth 51s of average
	// first-byte latency (see Chat), and a CN install old enough to reject it
	// says nothing about the global build installed next to it.
	partialStreamUnsupported atomic.Bool
}

// maxSystemPromptArgChars keeps the CLI command line below the ~32767 character
// CreateProcess limit Windows enforces on the whole argv.
const maxSystemPromptArgChars = 20000

// cliStdoutDrainDelay is how long cmd.Wait() waits for exec's stdout copy
// goroutine after the CLI process itself has exited.
const cliStdoutDrainDelay = 3 * time.Second

// cliResultTeardownGrace is the fallback teardown delay for an answered turn: the
// terminal result frame arrived, the read stopped, and the process is still with
// us. What normally ends it is the job guard's release on the way out of the run
// (closing a kill-on-close job handle is the kill), so this delay only decides
// what happens to a tree the guard could not cover -- a child the Task Scheduler
// re-homed into another job, and every install off Windows, where the guard is a
// no-op and this timer is the only thing left.
const cliResultTeardownGrace = 5 * time.Second

// cliReaperSlowLogDelay is how long the answered-path reaper stays quiet about a
// child that has not finished leaving after the result frame. Observability for
// wedge diagnostics, nothing more: a kill that reached the child is over before
// this threshold, so a wait past it means the kill did not reach.
const cliReaperSlowLogDelay = 10 * time.Second

// maxCLIOutputLineBytes caps a single JSONL frame: the terminal result frame
// carries the whole answer, and 8 MB is far above any measured turn. A var so the
// oversized-line regression test can reach the ceiling without shipping 8 MB.
var maxCLIOutputLineBytes = 8 * 1024 * 1024

// maxCLICapturedOutputBytes caps the whole turn. The deadline bounds how long the
// child may talk, not how fast: a CLI stuck in an error loop can push hundreds of
// megabytes into the capture buffer inside one timeout. 64 MB is eight times the
// per-frame cap, and a turn that fills it has no answer in it worth parsing. A var
// so the regression test can reach the ceiling without allocating 64 MB.
var maxCLICapturedOutputBytes = 64 * 1024 * 1024

// maxCLICapturedStderrBytes caps the diagnostic stream, which had no cap at all:
// the same error loop that fills the stdout capture fills this one, and nothing
// ever reads more than the last few lines of it. 4 MB keeps every transcript an
// operator has ever needed to read intact. A var for the same test reason as
// maxCLICapturedOutputBytes.
var maxCLICapturedStderrBytes = 4 * 1024 * 1024

// turnCeilingEnv is the operator's backstop for a turn that was given no
// deadline. Empty means the default, and 0 switches the backstop off.
const turnCeilingEnv = "LINGMA_QODERCLI_TURN_CEILING"

// defaultTurnCeiling bounds a turn that has nothing else to end it. A wedged
// read and a capture-ceiling drain both park forever when Timeout is 0, and each
// parked turn holds one of the service's four CLI slots, so four of them take
// the whole proxy offline. Half an hour is far above any measured turn (the
// longest on record is ~50s) and far below the point where an operator has
// stopped watching.
const defaultTurnCeiling = 30 * time.Minute

// turnCeiling reads the backstop. It is not the proxy deadline: Timeout keeps its
// own meaning (0 = no proxy deadline) and wins whenever it is set, because an
// operator who asked for an hour of budget does not get cut off at half an hour
// by a safety net. A caller's own context is a deadline too, and this never
// lengthens it -- the model probes run on their own 8s/120s/15m budgets and end
// there long before this one is in reach.
func turnCeiling() time.Duration {
	raw := strings.TrimSpace(os.Getenv(turnCeilingEnv))
	if raw == "" {
		return defaultTurnCeiling
	}
	ceiling, err := time.ParseDuration(raw)
	if err != nil || ceiling < 0 {
		log.Printf("qodercli: %s=%q is not a duration, keeping the %s backstop ceiling", turnCeilingEnv, raw, defaultTurnCeiling)
		return defaultTurnCeiling
	}
	return ceiling
}

func NewClient(loc Location, timeout time.Duration) *Client {
	return &Client{
		loc:     loc,
		tokens:  NewTokenSource(loc.ProfileDir, loc.Site),
		timeout: timeout,
	}
}

func (c *Client) Location() Location { return c.loc }

// label names the site this client drives in user-facing messages.
func (c *Client) label() string { return c.loc.Site.Label() }

// credential returns the JSON blob the CLI reads from its job token env var.
func (c *Client) credential(ctx context.Context) (string, error) {
	cred, err := c.tokens.JobToken(ctx)
	if err != nil {
		return "", err
	}
	raw := cred.Raw
	if len(raw) == 0 {
		raw, _ = json.Marshal(map[string]any{"token": cred.Token, "expires_at": cred.ExpiresAt})
	}
	return string(raw), nil
}

// ListModels returns the models the signed-in account can use. The CLI prints
// display names, which are also what --model accepts.
func (c *Client) ListModels(ctx context.Context) ([]remote.Model, error) {
	stdout, err := c.run(ctx, "--list-models")
	if err != nil {
		// A discovery that dies mid-probe is the failure this file keeps chasing,
		// and whatever the child did manage to print is the only clue about why.
		// The timeout case already carries it inside err; log it too, so the
		// headless log records the byte count even when the caller logs only err.
		log.Printf("qodercli: %s CLI model discovery failed after %d bytes on stdout: %v", c.label(), len(stdout), err)
		return nil, err
	}
	var models []remote.Model
	for _, line := range strings.Split(stdout, "\n") {
		name := strings.TrimSpace(line)
		if name == "" || strings.EqualFold(name, "MODEL") || strings.HasPrefix(name, "{") {
			continue
		}
		models = append(models, remote.Model{Key: name, DisplayName: name, Model: name, Enable: true})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("%s CLI reported no models", c.label())
	}
	return models, nil
}

// Chat sends one prompt and waits for the final result frame.
func (c *Client) Chat(ctx context.Context, request remote.ChatRequest, onDelta func(string)) (*remote.ChatResult, error) {
	prompt := strings.TrimSpace(request.Prompt)
	if prompt == "" && len(request.Messages) > 0 {
		prompt = strings.TrimSpace(request.Messages[len(request.Messages)-1].Content)
	}
	if prompt == "" {
		return nil, errors.New("empty user message")
	}

	args := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--permission-mode", "bypass_permissions",
		"--max-turns", "1",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
	}
	// --include-partial-messages is what makes this backend stream at all: without
	// it the CLI reports nothing until the whole turn is over, which measured a
	// 51s average and 88s worst-case before the client saw a single character.
	// Older installs reject the flag outright, so the first such failure disables
	// it for the rest of this client's life instead of failing every request.
	partial := onDelta != nil && !c.partialStreamUnsupported.Load()
	if partial {
		args = append(args, "--include-partial-messages")
	}
	if strings.TrimSpace(request.Model) != "" {
		args = append(args, "--model", strings.TrimSpace(request.Model))
	}
	if effort := c.clampReasoningEffort(request.Model, normalizeReasoningEffort(request.ReasoningEffort)); effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	// Client instructions go to the CLI's system slot: the gateway reroutes user
	// turns that assert another product's identity, and the prompt text is a user
	// turn here.
	if system := strings.TrimSpace(request.System); system != "" {
		if len(system) <= maxSystemPromptArgChars {
			args = append(args, "--append-system-prompt", system)
		} else {
			prompt = "System instructions:\n" + system + "\n\n" + prompt
		}
	}
	// Native tool calling would fight the proxy's own action-block protocol, and
	// leaving it on makes the CLI load its full agent toolchain per request.
	args = append(args, "--tools", "")

	frame, frameErr := userFrame(prompt, request.Images)
	if frameErr != nil {
		return nil, frameErr
	}

	streamed := false
	var onLine func(string)
	if partial {
		onLine = func(line string) {
			text, ok := partialTextDelta(line)
			if !ok || text == "" {
				return
			}
			streamed = true
			onDelta(text)
		}
	}

	stdout, runErr := c.runWithStdinData(ctx, frame, onLine, args...)
	if runErr != nil && partial && !streamed && strings.Contains(runErr.Error(), "include-partial-messages") {
		if c.partialStreamUnsupported.CompareAndSwap(false, true) {
			// One line, naming the site: without it a degraded stream looks like a
			// slow model, and the flag that is missing is the whole reason.
			log.Printf("qodercli: %s CLI refused --include-partial-messages; streamed turns on this site now arrive whole", c.label())
		}
		// args ends with "--tools", "" so dropping the last element would orphan
		// --tools and keep the flag the CLI just rejected.
		args = withoutArg(args, "--include-partial-messages")
		stdout, runErr = c.runWithStdinData(ctx, frame, nil, args...)
	}
	result, sawResult, parseErr := parseResult(stdout, request.Model, c.label(), c.loc.Site.Normalized())
	if !streamed && result != nil && onDelta != nil && parseErr == nil {
		onDelta(result.Text)
	}
	if runErr == nil {
		return result, parseErr
	}
	// The CLI can exit non-zero after a completed turn (teardown races on
	// Windows), so a usable answer or a real result-frame error both outrank
	// whatever stderr happened to hold. Without a result frame there is no
	// evidence the turn finished, and the text on stdout may stop mid-sentence:
	// that is the one case where the exit status is the only truth.
	if parseErr == nil && sawResult {
		return result, nil
	}
	var cliErr *cliError
	if errors.As(parseErr, &cliErr) {
		return nil, parseErr
	}
	return nil, runErr
}

func (c *Client) Warmup(ctx context.Context) error {
	_, err := c.credential(ctx)
	return err
}

func (c *Client) run(ctx context.Context, args ...string) (string, error) {
	return c.runWithStdinData(ctx, nil, nil, args...)
}

// userFrame builds the stream-json user turn. Images ride along as Anthropic
// content blocks, which is the shape the CLI's own content normalizer accepts.
func userFrame(prompt string, images []remote.Image) ([]byte, error) {
	content := []map[string]any{{"type": "text", "text": prompt}}
	for _, image := range images {
		data := strings.TrimSpace(image.Data)
		if data == "" {
			continue
		}
		media := strings.TrimSpace(image.MediaType)
		if media == "" {
			media = "image/png"
		}
		content = append(content, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": media,
				"data":       data,
			},
		})
	}
	frame, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	})
	if err != nil {
		return nil, err
	}
	return append(frame, '\n'), nil
}

// isTerminalFrame reports the CLI's own "this turn is over" frame. It is the
// signal that no more answer text can arrive, so the process may be given a short
// grace to exit on its own and be killed after that.
func isTerminalFrame(line string) bool {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") || !strings.Contains(line, `"result"`) {
		return false
	}
	var frame struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		return false
	}
	return frame.Type == "result"
}

// partialTextDelta picks the text out of a stream_event delta frame. Everything
// else - thinking, tool spans, lifecycle events - stays buffered for the final
// parse, because the proxy's own text filter needs the whole answer.
func partialTextDelta(line string) (string, bool) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") || !strings.Contains(line, "content_block_delta") {
		return "", false
	}
	var frame struct {
		Type  string `json:"type"`
		Event struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		} `json:"event"`
	}
	if err := json.Unmarshal([]byte(line), &frame); err != nil {
		return "", false
	}
	if frame.Type != "stream_event" || frame.Event.Delta.Type != "text_delta" {
		return "", false
	}
	return frame.Event.Delta.Text, true
}

// tailWriter keeps the newest limit bytes and drops the rest. The cause of a CLI
// failure is in what it printed last, and the volume that pushed it past a cap is
// the volume nobody reads, so a head cut is the wrong one to make.
type tailWriter struct {
	limit int
	buf   []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	// Trim on a band rather than on every write past the cap: dropping the head
	// copies the retained window, and a loop writing 4 KB at a time would pay
	// that copy a thousand times per megabyte.
	if w.limit > 0 && len(w.buf) > 2*w.limit {
		w.buf = append(w.buf[:0], w.buf[len(w.buf)-w.limit:]...)
	}
	return len(p), nil
}

func (w *tailWriter) String() string { return string(w.buf) }

// capturedOutput is one turn's stdout. The scan loop appends whole lines to it;
// in the rare StdoutPipe fallback the process writes into it directly, and then
// it keeps a tail instead of growing without end, because a process that never
// comes back would otherwise fill memory at whatever rate it likes.
//
// It deliberately does not embed bytes.Buffer: that promotes ReadFrom, which
// io.Copy prefers over Write, and the fallback capture would fill the buffer it
// was built to avoid while reporting itself empty.
type capturedOutput struct {
	buf  bytes.Buffer
	tail *tailWriter
}

func (o *capturedOutput) capAt(limit int) { o.tail = &tailWriter{limit: limit} }

func (o *capturedOutput) Write(p []byte) (int, error) {
	if o.tail != nil {
		return o.tail.Write(p)
	}
	return o.buf.Write(p)
}

func (o *capturedOutput) WriteString(s string) (int, error) {
	if o.tail != nil {
		return o.tail.Write([]byte(s))
	}
	return o.buf.WriteString(s)
}

func (o *capturedOutput) WriteByte(b byte) error {
	if o.tail != nil {
		_, err := o.tail.Write([]byte{b})
		return err
	}
	return o.buf.WriteByte(b)
}

func (o *capturedOutput) Len() int { return o.buf.Len() }

func (o *capturedOutput) String() string {
	if o.tail != nil {
		return o.tail.String()
	}
	return o.buf.String()
}

// openStdoutPipe is the seam the tests use to reach the fallback: StdoutPipe only
// fails when the OS refuses one more pipe, which a test cannot ask this box for.
var openStdoutPipe = func(cmd *exec.Cmd) (io.ReadCloser, error) { return cmd.StdoutPipe() }

func (c *Client) runWithStdinData(ctx context.Context, stdin []byte, onLine func(string), args ...string) (string, error) {
	credential, err := c.credential(ctx)
	if err != nil {
		return "", fmt.Errorf("%s 登录态不可用：%w", c.label(), err)
	}

	runCtx := ctx
	var cancel context.CancelFunc
	// ceiling is the backstop that bounds what no proxy deadline bounds. cmd.Cancel
	// only fires from a deadline, so without one the two ways this function can
	// park -- a wedged read with no terminal frame, and the drain after a capture
	// ceiling -- stay parked, and the request holds its CLI slot until the client
	// gives up. A configured Timeout wins outright: the backstop is a net under
	// "no deadline asked for", not a shorter version of the deadline that was.
	ceiling := time.Duration(0)
	if c.timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	} else {
		ceiling = turnCeiling()
		if ceiling > 0 {
			runCtx, cancel = context.WithTimeout(ctx, ceiling)
			defer cancel()
		}
	}

	name, argv := c.commandArgs(args)
	guard := newTreeGuard()
	defer guard.release()
	cmd := exec.CommandContext(runCtx, name, argv...)
	cmd.Cancel = func() error {
		// Kill the tree, not just the child: exec only reaps the process it started,
		// and a descendant that inherited the stdout write end is exactly what keeps
		// the scan below parked past its deadline.
		guard.release()
		return cmd.Process.Kill()
	}
	// The guard covers descendants that were alive when it was released, not ones
	// the CLI forks on its way out: measured on 192.168.50.239 the CN CLI logged
	// `process.exiting exit_code=0 uptime_ms=36337` and the /v1/models call that
	// started with it was still unanswered at 400 s, i.e. the exit status had
	// arrived and only the stdout EOF had not. That EOF is exec's copy goroutine
	// finishing, so cmd.Wait() was the parked call and nothing bounded it --
	// WaitDelay is the stdlib version of "give the leftover holder three seconds,
	// then close the pipe and take what was written".
	cmd.WaitDelay = cliStdoutDrainDelay
	cmd.Env = c.environment(credential)
	var stdout capturedOutput
	stderr := &tailWriter{limit: maxCLICapturedStderrBytes}
	// teardown kills the CLI process once the answer is complete but the process is
	// still with us; declared here so the Wait below can stop it.
	var teardown *time.Timer
	// answered marks that the scan stopped because the CLI's terminal frame arrived.
	var answered bool
	if stdin == nil {
		cmd.Stdin = strings.NewReader("")
	} else {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stderr = stderr
	// stdout always goes through a pipe, even when nobody streams: the loop below
	// is the only place that can tell "the turn is over" from "the CLI is still
	// running", and that distinction is what the teardown timer needs.
	var pipe io.ReadCloser
	if opened, pipeErr := openStdoutPipe(cmd); pipeErr != nil {
		// No pipe means no frame detection, so this capture is written by the
		// child itself and nothing checks the turn ceiling below: the cap moves
		// into the buffer.
		log.Printf("qodercli: %s CLI stdout pipe unavailable, capturing its last %d bytes instead: %v",
			c.label(), maxCLICapturedOutputBytes, pipeErr)
		stdout.capAt(maxCLICapturedOutputBytes)
		cmd.Stdout = &stdout
	} else {
		pipe = opened
	}
	startErr := cmd.Start()
	if startErr == nil {
		// Assign before it can fork: descendants join the job at creation, so this is
		// the only point that can cover the whole tree.
		guard.assign(cmd.Process)
	}
	if startErr == nil && pipe != nil {
		// ponytail: a read that is still parked past the last frame has no deadline
		// of its own -- Windows leaves a parked pipe read alone (measured: a
		// timer-driven pipe.Close came back 15.03s later, exactly when the write-end
		// holder exited, and a read end made with os.Pipe here never saw EOF at all).
		// Two things bound it: the terminal-frame teardown below, and cmd.Cancel,
		// whose deadline releases the job guard and kills every process in the tree.
		// The frame check only helps a turn that produced a frame, so a wedged CLI on
		// a non-chat call still waits for its own exit; off Windows the guard is a
		// no-op, so the upgrade path there is a temporary file for stdout (a file read
		// cannot be wedged by another holder) or setpgid plus kill(-pgid).
		scanner := bufio.NewScanner(pipe)
		scanner.Buffer(make([]byte, 0, 64*1024), maxCLIOutputLineBytes)
		var scanErr error
		for scanner.Scan() {
			line := scanner.Text()
			// The newline is written too, so it counts against the ceiling: checking
			// the line alone let the buffer finish one byte past the cap.
			if stdout.Len()+len(line)+1 > maxCLICapturedOutputBytes {
				scanErr = fmt.Errorf("capture ceiling of %d bytes reached", maxCLICapturedOutputBytes)
				break
			}
			stdout.WriteString(line)
			stdout.WriteByte('\n')
			if onLine != nil {
				onLine(line)
			}
			// The CLI's own terminal frame means the answer is complete. Keep reading
			// and this loop parks on the *next* line forever: the write end is still
			// held by whatever the install left behind, so neither cmd.Wait() nor its
			// WaitDelay is ever reached. Measured on 192.168.50.239, where the CLI
			// logged `Headless session completed successfully uptime_ms=47433` and the
			// /v1/chat/completions that started with it was still unanswered at 400 s.
			if isTerminalFrame(line) {
				answered = true
				// The tree is taken down when this function returns, by the deferred
				// guard.release: closing a kill-on-close job handle is itself the
				// kill. This timer is the fallback for the two shapes the guard does
				// not reach -- a child the Task Scheduler already put in another job,
				// and every turn off Windows, where the guard is a no-op. Killing the
				// tree is not what unblocks us either way; stopping the read is.
				teardown = time.AfterFunc(cliResultTeardownGrace, func() {
					// Best-effort tree kill: a child that the Task Scheduler already
					// put in another job cannot be assigned here, so the guard may
					// hold nothing and only the direct child dies.
					guard.release()
					_ = cmd.Process.Kill()
				})
				break
			}
		}
		if err := scanner.Err(); err != nil && scanErr == nil {
			scanErr = err
		}
		if scanErr != nil {
			// A single line past the buffer cap stops the scan while the CLI is
			// still writing. Left unread, the 64 KB pipe fills and cmd.Wait parks
			// until the deadline, turning an answer that already arrived into a
			// timeout, so the rest is drained first. The drain has to be synchronous:
			// a goroutine races cmd.Wait, which closes this read end, and loses.
			log.Printf("qodercli: %s CLI stdout stopped after %d bytes: %v", c.label(), stdout.Len(), scanErr)
			// The drain only ends when the child stops writing, and cmd.Cancel --
			// the thing that would close the write end -- is driven by a deadline.
			// With no configured timeout nothing else ever stops it, so the read
			// goes on until the CLI itself dies, which is the failure this whole
			// path exists to avoid. Take the tree down before draining.
			// A configured timeout is left alone: its deadline closes the write end
			// moments later, and killing the child here instead would turn "the
			// answer arrived, then one huge line followed" into a failed turn.
			if c.timeout == 0 {
				guard.release()
				_ = cmd.Process.Kill()
			}
			_, _ = io.Copy(io.Discard, pipe)
		}
	}
	var waitErr error
	if startErr == nil {
		if answered {
			// Stopping the read is not enough: cmd.Wait() would then block on the
			// process leaving instead, and an answered CLI that lingers for minutes
			// is exactly the shape this box has. Reap it in the background. What takes
			// the process down is the deferred guard.release() on the way out of this
			// function, not the teardown timer -- with a healthy job object the handle
			// closes the moment this returns, and the timer only ever fires for a tree
			// the guard could not cover. The timer is deliberately left to run out.
			go func() {
				// Diagnostics only, and quiet in the normal case: a kill already
				// happened before this goroutine started, so a Wait that takes this
				// long is a child the kill did not reach.
				waitStart := time.Now()
				reapErr := cmd.Wait()
				if took := time.Since(waitStart); took > cliReaperSlowLogDelay {
					log.Printf("qodercli: %s CLI process took %s to leave after its result frame, "+
						"which is longer than a kill that reached it should: %v",
						c.label(), took.Round(time.Millisecond), reapErr)
				}
			}()
			return stdout.String(), nil
		}
		waitErr = cmd.Wait()
		if teardown != nil {
			teardown.Stop()
		}
	} else {
		waitErr = startErr
	}
	if waitErr != nil {
		if runCtx.Err() != nil {
			// The deadline won, but the child still spoke: fold what it said into the
			// error instead of reporting the deadline as if it were the cause.
			note := cancelledChildNote(stdout.Len(), stderr.String())
			switch {
			// Only the deadline itself reads as a timeout: a configured timeout
			// with a caller that hung up dies as context.Canceled, and calling
			// that "timed out" blames the CLI for the client's disconnect.
			case c.timeout > 0 && errors.Is(runCtx.Err(), context.DeadlineExceeded):
				return stdout.String(), fmt.Errorf("%s CLI timed out after %s (%s)", c.label(), c.timeout, note)
			case ceiling > 0 && ctx.Err() == nil && errors.Is(runCtx.Err(), context.DeadlineExceeded):
				// ctx.Err() is nil while runCtx is dead, so the deadline that fired
				// is the backstop this function added and not one the caller
				// brought. It is named apart from "timed out after <timeout>" because
				// nobody configured it and no Timeout field will explain it.
				return stdout.String(), fmt.Errorf(
					"%s CLI hit the %s backstop ceiling, which is what ends a turn given no timeout (%s)",
					c.label(), ceiling, note)
			default:
				return stdout.String(), fmt.Errorf("%s CLI was cancelled before it finished: %w (%s)", c.label(), runCtx.Err(), note)
			}
		}
		detail := errorLines(stderr.String(), 6)
		if reason := rejectedCredential(stderr.String()); reason != "" {
			// "the CLI rejected this credential", not "your login is dead": the
			// same phrase also appears when the CLI reads the desktop app's own
			// config root, which --config-dir now keeps it out of.
			return stdout.String(), fmt.Errorf(
				"%s CLI 拒绝了当前会话凭据（%s）：请在桌面版重新登录后再试", c.label(), reason)
		}
		if detail != "" {
			if transientAuthHandshake(detail) {
				return stdout.String(), fmt.Errorf(
					"%s CLI could not exchange its job token for a session (openapi call failed): %w",
					c.label(), remote.ErrTransientUpstream)
			}
			return stdout.String(), fmt.Errorf("%s CLI failed: %s", c.label(), detail)
		}
		// err here is the long-gone credential error, which is nil by this point:
		// wrapping it printed "%!w(<nil>)" where the exit status belonged.
		return stdout.String(), fmt.Errorf("%s CLI failed: %w with no output", c.label(), waitErr)
	}
	return stdout.String(), nil
}

func (c *Client) commandArgs(args []string) (string, []string) {
	argv := args
	if c.loc.useWorker() {
		argv = append([]string{c.loc.RuntimeJS}, argv...)
	}
	if c.loc.ConfigDir != "" {
		// Best-effort: the CLI creates the directory itself, and the failure is
		// then visible in its own error rather than swallowed here.
		if err := os.MkdirAll(c.loc.ConfigDir, 0o700); err != nil {
			log.Printf("qodercli: %s CLI config dir %s could not be created: %v", c.label(), c.loc.ConfigDir, err)
		}
		argv = append(argv, "--config-dir", c.loc.ConfigDir)
	}
	return c.loc.HostExe, argv
}

// withoutArg drops every occurrence of a boolean flag, keeping each option's
// value next to its own name. Positional trimming cannot do this: the argv ends
// with the valueless "--tools", so the last element is not the flag to drop.
func withoutArg(args []string, flag string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == flag {
			continue
		}
		out = append(out, arg)
	}
	return out
}

func (c *Client) environment(credential string) []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+4)
	for _, entry := range env {
		switch {
		// Inheriting the desktop agent's SDK handshake would make the child wait
		// for a control channel that this subprocess host never provides.
		case strings.HasPrefix(entry, "QODER_AGENT_SDK_"),
			strings.HasPrefix(entry, "QODER_SDK_"),
			strings.HasPrefix(entry, "QODER_WORKER_"),
			strings.HasPrefix(entry, "ELECTRON_RUN_AS_NODE="),
			strings.HasPrefix(entry, "NODE_OPTIONS="),
			strings.HasPrefix(entry, c.loc.EnvPrefix+"JOB_TOKEN="):
			continue
		default:
			out = append(out, entry)
		}
	}
	out = append(out,
		c.loc.EnvPrefix+"JOB_TOKEN="+credential,
		"ELECTRON_RUN_AS_NODE=1",
	)
	return out
}

type outputFrame struct {
	Type       string          `json:"type"`
	Subtype    string          `json:"subtype"`
	Message    *chatMessage    `json:"message"`
	Result     string          `json:"result"`
	StopReason string          `json:"stop_reason"`
	Errors     []string        `json:"errors"`
	ErrorCode  int             `json:"error_code"`
	IsError    bool            `json:"is_error"`
	SessionID  string          `json:"session_id"`
	Usage      json.RawMessage `json:"usage"`
	Model      string          `json:"model"`
}

type chatMessage struct {
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Content []textModule `json:"content"`
	Usage   *usageBlock  `json:"usage"`
}

type textModule struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
}

type usageBlock struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// parseResult folds the CLI's JSONL output into one answer. sawResult reports
// whether a terminal result frame was seen, which is the only evidence that the
// turn actually finished: text without it can stop mid-sentence.
func parseResult(stdout, model, label string, site Site) (*remote.ChatResult, bool, error) {
	var (
		text      strings.Builder
		frames    []string
		result    *outputFrame
		inputTok  int
		outputTok int
		requestID string
	)
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	// The same cap the capture used, or raising it there only moves the failure:
	// a frame the capture admitted and this scanner refuses reads as a turn that
	// answered nothing.
	scanner.Buffer(make([]byte, 0, 64*1024), maxCLIOutputLineBytes)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		frames = append(frames, line)
		var frame outputFrame
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			continue
		}
		switch frame.Type {
		case "assistant":
			if frame.Message == nil {
				continue
			}
			if frame.Message.ID != "" {
				requestID = frame.Message.ID
			}
			if frame.Message.Usage != nil {
				inputTok = frame.Message.Usage.InputTokens
				outputTok = frame.Message.Usage.OutputTokens
			}
			for _, block := range frame.Message.Content {
				if block.Type == "text" && block.Text != "" {
					text.WriteString(block.Text)
				}
			}
		case "result":
			copied := frame
			result = &copied
			if frame.SessionID != "" {
				requestID = firstNonEmpty(requestID, frame.SessionID)
			}
		}
	}

	if result != nil {
		if result.IsError || result.Subtype != "" && result.Subtype != "success" {
			return nil, true, &cliError{label: label, text: cliErrorText(*result, frames)}
		}
		if strings.TrimSpace(result.Result) != "" {
			text.Reset()
			text.WriteString(result.Result)
		}
		if len(result.Usage) > 0 {
			var usage usageBlock
			if err := json.Unmarshal(result.Usage, &usage); err == nil {
				if usage.InputTokens > 0 {
					inputTok = usage.InputTokens
				}
				if usage.OutputTokens > 0 {
					outputTok = usage.OutputTokens
				}
			}
		}
	}

	out := strings.TrimRight(text.String(), " \t\r\n")
	sawResult := result != nil
	if out == "" {
		return nil, sawResult, fmt.Errorf("%s CLI returned no answer: %s", label, tailLines(stdout, 8))
	}
	stopReason := ""
	if result != nil {
		stopReason = strings.TrimSpace(result.StopReason)
	}
	return &remote.ChatResult{
		Text:          out,
		InputTokens:   inputTok,
		OutputTokens:  outputTok,
		RequestID:     requestID,
		CredentialSrc: "qodercli:" + string(site),
		StopReason:    stopReason,
	}, sawResult, nil
}

func cliErrorText(result outputFrame, frames []string) string {
	if strings.TrimSpace(result.Result) != "" {
		return truncate(result.Result, 400)
	}
	if len(result.Errors) > 0 {
		text := strings.Join(result.Errors, "; ")
		if result.ErrorCode != 0 {
			text = fmt.Sprintf("%s (code %d)", text, result.ErrorCode)
		}
		return truncate(text, 400)
	}
	for i := len(frames) - 1; i >= 0; i-- {
		if strings.Contains(frames[i], "error") {
			return truncate(frames[i], 400)
		}
	}
	return "unknown error"
}

// cliError is a failure the CLI reported in its own result frame. Chat prefers it
// over stderr, where the runtime prints per-run startup warnings regardless.
type cliError struct {
	label string
	text  string
}

func (e *cliError) Error() string {
	label := strings.TrimSpace(e.label)
	if label == "" {
		label = SiteCN.Label()
	}
	return label + " CLI error: " + e.text
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// reasoningEffortAliases maps client-side effort words (including the Qoder CN UI's
// Chinese labels) onto the tier names the CLI parses. Which of those a given model
// applies is a separate question, answered by reasoningLadders below.
var reasoningEffortAliases = map[string]string{
	"none":     "none",
	"off":      "none",
	"disabled": "none",
	"minimal":  "low",
	"low":      "low",
	"medium":   "medium",
	"auto":     "medium",
	"high":     "high",
	"xhigh":    "xhigh",
	"ultra":    "max",
	"highest":  "max",
	"max":      "max",
	"关闭":       "none",
	"不思考":      "none",
	"低":        "low",
	"中":        "medium",
	"高":        "high",
	"极高":       "xhigh",
	"最高":       "max",
}

// normalizeReasoningEffort returns the CLI level for a client-supplied effort. It
// returns "" only when the client named no tier at all: the CLI's own default does
// start thinking, so "off" has to travel explicitly as none.
func normalizeReasoningEffort(effort string) string {
	key := strings.ToLower(strings.TrimSpace(effort))
	if key == "" {
		return ""
	}
	if level, ok := reasoningEffortAliases[key]; ok {
		return level
	}
	return ""
}

// reasoningTierRank orders the tier names the CLI's internal ladder accepts.
var reasoningTierRank = map[string]int{
	"none":   0,
	"low":    1,
	"medium": 2,
	"high":   3,
	"xhigh":  4,
	"max":    5,
}

// reasoningLadders records which tiers each model actually applies. The CLI parses
// any tier name, then gates it against the signed model catalog, and a tier the
// model does not offer is simply not applied -- the run log then reports
// reasoning_effort=none, meaning "unset", while the turn still thinks at the
// model's own default. So the ladder has to be resolved here, not upstream.
// Measured from the CLI's per-run logs on 2026-09-19 by sending every tier to every
// model and reading back what stuck. nil means the model ignored all of them.
var reasoningLadders = map[string][]string{
	// Qwen3.8: off / low / medium / xhigh. There is no "high" and no "max".
	"Qwen3.8-Flash": {"none", "low", "medium", "xhigh"},
	"Qwen3.8-Max":   {"none", "low", "medium", "xhigh"},
	// GLM, Kimi and DeepSeek-Flash: off / low / high / max. No medium, no xhigh.
	"GLM-5.3":           {"none", "low", "high", "max"},
	"Kimi-K3":           {"none", "low", "high", "max"},
	"Kimi-K2.8-Preview": {"none", "low", "high", "max"},
	"DeepSeek-Flash":    {"none", "low", "high", "max"},
	// These three skipped the low rung as well.
	"GLM-5.2":         {"none", "high", "max"},
	"GLM-5.3-Flash":   {"none", "high", "max"},
	"DeepSeek-V4-Pro": {"none", "high", "max"},
	// Measured to ignore every tier name, thinking level included.
	"Auto":          nil,
	"Qwen3.7-Max":   nil,
	"Qwen3.7-Plus":  nil,
	"Qwen3.7-Flash": nil,
	"MiniMax-M2.7":  nil,
}

// clampReasoningEffort maps the requested tier onto the model's ladder: the
// strongest supported tier at or below the request, or the model's top tier when
// the request is at or above it. reasoningLadders was measured against the CN
// catalog, so other sites pass the tier through and let their own signed catalog
// drop what it does not offer.
func (c *Client) clampReasoningEffort(model, effort string) string {
	if c.loc.Site.Normalized() != SiteCN {
		return effort
	}
	ladder, ok := reasoningLadders[strings.TrimSpace(model)]
	if !ok || effort == "" {
		return effort
	}
	clamped := clampTier(ladder, effort)
	if clamped != effort {
		log.Printf("qodercli: model %s does not offer tier %s, using %s", model, effort, clamped)
	}
	return clamped
}

func clampTier(ladder []string, effort string) string {
	rank, ok := reasoningTierRank[effort]
	if !ok {
		return effort
	}
	if len(ladder) == 0 {
		return ""
	}
	top := ladder[len(ladder)-1]
	if rank >= reasoningTierRank[top] {
		return keepOrOmit(top, rank)
	}
	best := ladder[0]
	for _, tier := range ladder {
		if r := reasoningTierRank[tier]; r <= rank && r > reasoningTierRank[best] {
			best = tier
		}
	}
	return keepOrOmit(best, rank)
}

// keepOrOmit drops the tier entirely when clamping would only have produced
// "none": a model that offers no thinking tiers (Auto is one) still thinks by
// default, and sending none would switch that off rather than pick a level.
func keepOrOmit(tier string, requestedRank int) string {
	if tier == "none" && requestedRank > reasoningTierRank["none"] {
		return ""
	}
	return tier
}

func tailLines(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if len(lines) > count {
		lines = lines[len(lines)-count:]
	}
	return strings.Join(lines, " | ")
}

// cliNoisePrefixes are startup warnings the CLI prints on every run; keeping
// them out of error messages is what makes a real failure readable.
var cliNoisePrefixes = []string{
	`Skill "`,
	"Warning:",
}

// chromiumLogRe matches the runtime's own stderr diagnostics, e.g.
// "[0920/043244.997:ERROR:third_party\crashpad\util\win\...:108] CreateFile: ...".
// They appear whatever the request did and quote internal source paths, so quoting
// one at the user reads like a refusal while hiding the real cause.
var chromiumLogRe = regexp.MustCompile(`^\[\d{4}/\d{6}\.\d{3}:`)

// authHandshakeFrames name the CLI's job-token-to-session exchange. When that
// network call dies the CLI prints a rejected promise chain and no message at all,
// so the frame names are the only way to tell "the account call failed, retry" from
// a real refusal -- and quoting the stack at the user hides both.
var authHandshakeFrames = []string{"loginWithJobToken", "fetchOpenApiUserInfo", "openApiJsonRequest"}

// rejectedCredentialReasons are the gateway's words for a job token it will not
// serve. They can sit anywhere in stderr while the tail holds only promise
// frames, so this must not share errorLines' window -- without it the user reads
// six frames of obfuscated runtime as if it were the failure. Only token-state
// wording counts: "auth.getUserInfo failed" also fronts plain network failures,
// which must stay retryable.
var rejectedCredentialReasons = []string{
	"token is not active",
	"token has expired",
	"invalid token",
}

func rejectedCredential(stderr string) string {
	lowered := strings.ToLower(stderr)
	for _, reason := range rejectedCredentialReasons {
		if strings.Contains(lowered, strings.ToLower(reason)) {
			return reason
		}
	}
	return ""
}

func transientAuthHandshake(detail string) bool {
	for _, frame := range authHandshakeFrames {
		if strings.Contains(detail, frame) {
			return true
		}
	}
	return false
}

// errorLines keeps the tail of stderr but drops the per-run noise lines. It
// returns "" when nothing but noise was printed, so callers can say so instead
// of quoting a startup warning as if it were the failure.
func errorLines(text string, count int) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	meaningful := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || isCLINoise(trimmed) {
			continue
		}
		meaningful = append(meaningful, trimmed)
	}
	if len(meaningful) > count {
		meaningful = meaningful[len(meaningful)-count:]
	}
	return strings.Join(meaningful, " | ")
}

// cancelledChildNote explains a child that was killed by its deadline instead of
// exiting on its own.
//
// By the time this runs the child's stderr is already fully captured, so throwing
// it away is what collapses every distinct failure -- a rejected credential, a lost
// session, a startup crash -- into the same opaque "context deadline exceeded".
// The stdout byte count rides along because the known shape of this failure is a
// child that writes a hundred-odd bytes and then closes its pipe without ever
// reaching a terminal frame: the count is what tells "it answered nothing" apart
// from "it answered at length and we still cut it off".
//
// It is a note, not a verdict: callers fold it into their own error text.
func cancelledChildNote(stdoutLen int, stderr string) string {
	parts := []string{fmt.Sprintf("child wrote %d bytes to stdout", stdoutLen)}
	if detail := errorLines(stderr, 6); detail != "" {
		parts = append(parts, "stderr: "+detail)
	}
	return strings.Join(parts, "; ")
}

func isCLINoise(line string) bool {
	for _, prefix := range cliNoisePrefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return chromiumLogRe.MatchString(line)
}

func truncate(text string, limit int) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) <= limit {
		return trimmed
	}
	// Back off to a rune boundary: the CLI answers and fails in Chinese, and a
	// byte cut leaves half of a multi-byte rune for the client to render as
	// mojibake. Same reasoning as remote's truncate.
	cut := limit
	for cut > 0 && !utf8.RuneStart(trimmed[cut]) {
		cut--
	}
	return trimmed[:cut] + "…"
}
