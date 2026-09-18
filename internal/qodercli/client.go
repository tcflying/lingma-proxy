package qodercli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"lingma-ipc-proxy/internal/remote"
)

// Client drives the bundled Qoder CN CLI as a subprocess. It exists because the
// inference gateway signs every request from inside the CLI (a WASM module with
// hardware-bound key material), so signing cannot be replayed from Go.
type Client struct {
	loc     Location
	tokens  *TokenSource
	timeout time.Duration
}

// maxSystemPromptArgChars keeps the CLI command line below the ~32767 character
// CreateProcess limit Windows enforces on the whole argv.
const maxSystemPromptArgChars = 20000

func NewClient(loc Location, timeout time.Duration) *Client {
	return &Client{
		loc:     loc,
		tokens:  NewTokenSource(loc.ProfileDir),
		timeout: timeout,
	}
}

func (c *Client) Location() Location { return c.loc }

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
		return nil, errors.New("Qoder CN CLI reported no models")
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
	if len(request.Images) > 0 {
		return nil, errors.New("the Qoder CN CLI backend does not support image input")
	}

	args := []string{
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--permission-mode", "bypass_permissions",
		"--max-turns", "1",
		"--strict-mcp-config",
		"--mcp-config", `{"mcpServers":{}}`,
	}
	if strings.TrimSpace(request.Model) != "" {
		args = append(args, "--model", strings.TrimSpace(request.Model))
	}
	if effort := normalizeReasoningEffort(request.ReasoningEffort); effort != "" {
		args = append(args, "--reasoning-effort", effort)
	}
	// Client instructions go to the CLI's system slot: the Qoder CN gateway
	// reroutes user turns that assert another product's identity, and the prompt
	// text is a user turn here.
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

	stdout, runErr := c.runWithStdin(ctx, prompt, args...)
	result, parseErr := parseResult(stdout, request.Model, onDelta)
	if runErr == nil {
		return result, parseErr
	}
	// The CLI can exit non-zero after a completed turn (teardown races on
	// Windows), so a usable answer or a real result-frame error both outrank
	// whatever stderr happened to hold.
	if parseErr == nil {
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
	return c.runWithStdinData(ctx, nil, args...)
}

func (c *Client) runWithStdin(ctx context.Context, prompt string, args ...string) (string, error) {
	frame, err := json.Marshal(map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": prompt}},
		},
	})
	if err != nil {
		return "", err
	}
	return c.runWithStdinData(ctx, append(frame, '\n'), args...)
}

func (c *Client) runWithStdinData(ctx context.Context, stdin []byte, args ...string) (string, error) {
	credential, err := c.credential(ctx)
	if err != nil {
		return "", fmt.Errorf("Qoder CN 登录态不可用：%w", err)
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if c.timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}

	name, argv := c.commandArgs(args)
	cmd := exec.CommandContext(runCtx, name, argv...)
	cmd.Env = c.environment(credential)
	var stdout, stderr bytes.Buffer
	if stdin == nil {
		cmd.Stdin = strings.NewReader("")
	} else {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if runCtx.Err() != nil {
			if c.timeout > 0 {
				return stdout.String(), fmt.Errorf("Qoder CN CLI timed out after %s", c.timeout)
			}
			return stdout.String(), fmt.Errorf("Qoder CN CLI was cancelled before it finished: %w", runCtx.Err())
		}
		detail := errorLines(stderr.String(), 6)
		if detail != "" {
			return stdout.String(), fmt.Errorf("Qoder CN CLI failed: %s", detail)
		}
		return stdout.String(), fmt.Errorf("Qoder CN CLI failed: %w with no output", err)
	}
	return stdout.String(), nil
}

func (c *Client) commandArgs(args []string) (string, []string) {
	if !c.loc.useWorker() {
		return c.loc.HostExe, args
	}
	return c.loc.HostExe, append([]string{c.loc.RuntimeJS}, args...)
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
	Type      string          `json:"type"`
	Subtype   string          `json:"subtype"`
	Message   *chatMessage    `json:"message"`
	Result    string          `json:"result"`
	Errors    []string        `json:"errors"`
	ErrorCode int             `json:"error_code"`
	IsError   bool            `json:"is_error"`
	SessionID string          `json:"session_id"`
	Usage     json.RawMessage `json:"usage"`
	Model     string          `json:"model"`
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

func parseResult(stdout, model string, onDelta func(string)) (*remote.ChatResult, error) {
	var (
		text      strings.Builder
		frames    []string
		result    *outputFrame
		inputTok  int
		outputTok int
		requestID string
	)
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
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
			return nil, &cliError{text: cliErrorText(*result, frames)}
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
	if out == "" {
		return nil, fmt.Errorf("Qoder CN CLI returned no answer: %s", tailLines(stdout, 8))
	}
	if onDelta != nil {
		onDelta(out)
	}
	return &remote.ChatResult{
		Text:          out,
		InputTokens:   inputTok,
		OutputTokens:  outputTok,
		RequestID:     requestID,
		CredentialSrc: "qodercli",
	}, nil
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
type cliError struct{ text string }

func (e *cliError) Error() string { return "Qoder CN CLI error: " + e.text }

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

// reasoningEffortAliases maps client-side effort words (including the Qoder CN
// UI's Chinese labels) onto the levels --reasoning-effort accepts.
var reasoningEffortAliases = map[string]string{
	"minimal": "low",
	"low":     "low",
	"medium":  "medium",
	"auto":    "medium",
	"high":    "high",
	"xhigh":   "xhigh",
	"ultra":   "xhigh",
	"highest": "xhigh",
	"max":     "xhigh",
	"低":       "low",
	"中":       "medium",
	"高":       "high",
	"极高":      "xhigh",
	"最高":      "xhigh",
}

// normalizeReasoningEffort returns the CLI level for a client-supplied effort,
// or "" when the client asked for no thinking.
func normalizeReasoningEffort(effort string) string {
	key := strings.ToLower(strings.TrimSpace(effort))
	switch key {
	case "":
		return ""
	case "none", "disabled", "off":
		return ""
	}
	if level, ok := reasoningEffortAliases[key]; ok {
		return level
	}
	return ""
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

func isCLINoise(line string) bool {
	for _, prefix := range cliNoisePrefixes {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

func truncate(text string, limit int) string {
	trimmed := strings.TrimSpace(text)
	if len(trimmed) <= limit {
		return trimmed
	}
	return trimmed[:limit] + "…"
}
