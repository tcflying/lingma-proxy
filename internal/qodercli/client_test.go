package qodercli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"lingma-ipc-proxy/internal/remote"
)

func TestClampTierStopsAtTheStrongestTierTheModelOffers(t *testing.T) {
	// Measured shapes: Qwen3.8-Flash has no high/max rung, GLM-5.3 has no
	// medium/xhigh rung. Asking for a missing tier must never return it, because
	// the CLI would silently run the turn with reasoning_effort=none.
	cases := []struct {
		name   string
		ladder []string
		in     string
		want   string
	}{
		{"qwen keeps its own tiers", []string{"none", "low", "medium", "xhigh"}, "low", "low"},
		{"qwen steps high down to medium", []string{"none", "low", "medium", "xhigh"}, "high", "medium"},
		{"qwen caps max at its top", []string{"none", "low", "medium", "xhigh"}, "max", "xhigh"},
		{"glm keeps its top tier", []string{"none", "low", "high", "max"}, "max", "max"},
		{"glm steps xhigh down to high", []string{"none", "low", "high", "max"}, "xhigh", "high"},
		{"glm steps medium down to low", []string{"none", "low", "high", "max"}, "medium", "low"},
		{"unknown tier passes through", []string{"none", "low"}, "banana", "banana"},
		{"measured tier-less model sends nothing", nil, "xhigh", ""},
		// Auto exposes no thinking tier at all: forcing none would switch its
		// default thinking off instead of picking a level.
		{"tier-less model omits instead of forcing none", []string{"none"}, "medium", ""},
		{"explicit none still reaches a tier-less model", []string{"none"}, "none", "none"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampTier(tc.ladder, tc.in); got != tc.want {
				t.Fatalf("clampTier(%v, %q) = %q, want %q", tc.ladder, tc.in, got, tc.want)
			}
		})
	}
}

func TestErrorLinesDropRuntimeLogNoise(t *testing.T) {
	// Both shapes were observed standing in for the failure message, so the user
	// read a crashpad path or a skill collision instead of what went wrong.
	stderr := "[0920/043244.997:ERROR:third_party\\crashpad\\crashpad\\util\\win\\registration_protocol_win.cc:108] CreateFile: 系统找不到指定的文件。 (0x2)\n" +
		"[0920/043244.998:FATAL:gin\\v8_initializer.cc:681] Error loading V8 startup snapshot file\n" +
		"Skill \"paseo-advisor\" overrides same-source skill at /x/y\n" +
		"Warning: native tool hooks are not fully supported\n" +
		"upstream rejected the prompt"
	if got := errorLines(stderr, 6); got != "upstream rejected the prompt" {
		t.Fatalf("errorLines kept noise: %q", got)
	}

	// Nothing but noise must read as "no reason given", not as the noise itself.
	if got := errorLines("[0920/043224.168:FATAL:gin\\v8_initializer.cc:681] boom\n", 6); got != "" {
		t.Fatalf("pure noise should yield empty, got %q", got)
	}

	// A real failure that happens to mention a log-looking line still surfaces.
	if got := errorLines("gateway returned 403 for model Qwen3.8-Flash", 6); got == "" {
		t.Fatal("plain error dropped")
	}
}

func TestRejectedCredentialFindsTheCauseAboveTheTail(t *testing.T) {
	// The measured shape: the gateway's reason arrives first, then six frames of
	// obfuscated promise chain, so the tail alone never mentions the token.
	stderr := "auth.getUserInfo failed: token is not active\n" +
		"    at async xoe.initAuthWithOptions (file:///runtime.obf.mjs:1:3958178)\n" +
		"    at async xoe.initAuth (file:///runtime.obf.mjs:1:3955357)\n" +
		"    at async C$.refreshAuth (file:///runtime.obf.mjs:267:819330)\n"
	if got := rejectedCredential(stderr); got == "" {
		t.Fatal("a rejected credential must be named, not reported as a stack")
	}
	// The same prefix fronts plain network failures, which have to stay retryable.
	if got := rejectedCredential("auth.getUserInfo failed: connection reset"); got != "" {
		t.Fatalf("a network hiccup is not a rejected token, matched %q", got)
	}
}

// TestParseResultDistinguishesFinishedFromKilled: a CLI killed mid-turn leaves
// assistant frames but never a result frame. Chat uses that flag to refuse to
// report a half answer as a completed turn, which is what surfaced as replies
// that simply stopped mid-sentence with no error.
func TestParseResultDistinguishesFinishedFromKilled(t *testing.T) {
	partial := `{"type":"assistant","message":{"content":[{"type":"text","text":"说到一半"}]}}` + "\n"

	result, sawResult, err := parseResult(partial, "Qwen3.8-Flash", "CN", SiteCN)
	if err != nil {
		t.Fatal(err)
	}
	if result == nil || result.Text != "说到一半" {
		t.Fatalf("partial text must still parse, got %#v", result)
	}
	if sawResult {
		t.Fatal("frames without a result frame cannot prove the turn finished")
	}

	finished := partial + `{"type":"result","subtype":"success","result":"说到一半就说完了"}` + "\n"
	result, sawResult, err = parseResult(finished, "Qwen3.8-Flash", "CN", SiteCN)
	if err != nil {
		t.Fatal(err)
	}
	if !sawResult {
		t.Fatal("a success result frame is the evidence that the turn finished")
	}
	if result.Text != "说到一半就说完了" {
		t.Fatalf("text = %q", result.Text)
	}
}

// TestPartialTextDeltaStreamsOnlyText guards the latency fix: the CLI's
// stream_event frames are the only source of early text, and thinking, tool
// spans and lifecycle events must not be forwarded as answer text.
func TestPartialTextDeltaStreamsOnlyText(t *testing.T) {
	text := `{"type":"stream_event","event":{"type":"content_block_delta","index":1,` +
		`"delta":{"type":"text_delta","text":"你好"}}}`
	if got, ok := partialTextDelta(text); !ok || got != "你好" {
		t.Fatalf("text delta = %q ok=%v", got, ok)
	}

	skip := []string{
		`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"想"}}}`,
		`{"type":"stream_event","event":{"type":"message_start"}}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"整段"}]}}`,
		`{"type":"result","subtype":"success","result":"整段"}`,
		`not json at all`,
	}
	for _, line := range skip {
		if got, ok := partialTextDelta(line); ok && got != "" {
			t.Fatalf("must not stream %q from %s", got, line)
		}
	}
}

// TestUserFrameCarriesImagesAsContentBlocks is the CLI's own shape: a base64
// source block next to the text, which is what makes image input work at all.
func TestUserFrameCarriesImagesAsContentBlocks(t *testing.T) {
	frame, err := userFrame("看图", []remote.Image{
		{MediaType: "image/jpeg", Data: "/9j/AA=="},
		{MediaType: "", Data: "AAAA"},
		{MediaType: "image/png", Data: "  "},
	})
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(frame, &decoded); err != nil {
		t.Fatalf("frame is not valid JSON: %v (%s)", err, frame)
	}
	if decoded.Type != "user" || decoded.Message.Role != "user" {
		t.Fatalf("frame head = %#v", decoded)
	}
	if len(decoded.Message.Content) != 3 {
		t.Fatalf("content = %+v", decoded.Message.Content)
	}
	if decoded.Message.Content[0].Text != "看图" {
		t.Fatalf("text block = %+v", decoded.Message.Content[0])
	}
	jpeg := decoded.Message.Content[1]
	if jpeg.Type != "image" || jpeg.Source.Type != "base64" ||
		jpeg.Source.MediaType != "image/jpeg" || jpeg.Source.Data != "/9j/AA==" {
		t.Fatalf("jpeg block = %+v", jpeg)
	}
	if decoded.Message.Content[2].Source.MediaType != "image/png" {
		t.Fatalf("an unspecified media type must default to png, got %+v", decoded.Message.Content[2])
	}
}

// TestParseResultReportsTheBackendStopReason is what lets the API layer tell a
// budget-stopped answer apart from one the model finished.
func TestParseResultReportsTheBackendStopReason(t *testing.T) {
	stdout := `{"type":"result","subtype":"success","stop_reason":"max_tokens","result":"停在预算上"}` + "\n"
	result, sawResult, err := parseResult(stdout, "Qwen3.8-Flash", "CN", SiteCN)
	if err != nil {
		t.Fatal(err)
	}
	if !sawResult {
		t.Fatal("a result frame is terminal evidence")
	}
	if result.StopReason != "max_tokens" {
		t.Fatalf("stop reason = %q", result.StopReason)
	}
}

// TestParseResultReadsLinesTheCaptureWouldAdmit: the second parse ran a scanner
// with its own hardcoded 8 MB while the capture used maxCLIOutputLineBytes, so
// raising the var (which is a var for exactly that) only moved the failure -- the
// capture admitted the frame and the parse called the turn silent. The frame here
// is past the literal the parse used to hold.
func TestParseResultReadsLinesTheCaptureWouldAdmit(t *testing.T) {
	prev := maxCLIOutputLineBytes
	maxCLIOutputLineBytes = 16 * 1024 * 1024
	t.Cleanup(func() { maxCLIOutputLineBytes = prev })

	// 8.4 MB of payload, which is over the 8 MB the parse used to allow and well
	// under the cap the capture is now configured with.
	answer := strings.Repeat("字", 2_800_000)
	stdout := `{"type":"result","subtype":"success","result":"` + answer + `"}` + "\n"
	if len(stdout) <= 8*1024*1024 {
		t.Fatalf("the fixture is %d bytes, which does not clear the old 8 MB ceiling", len(stdout))
	}

	result, sawResult, err := parseResult(stdout, "Qwen3.8-Flash", "CN", SiteCN)
	if err != nil {
		t.Fatalf("a frame the capture admitted must not read as an unanswered turn: %v", err)
	}
	if !sawResult {
		t.Fatal("the oversized result frame was dropped, so the turn looks unfinished")
	}
	if result.Text != answer {
		t.Fatalf("text is %d bytes, want the %d byte answer", len(result.Text), len(answer))
	}
}

// TestTruncateNeverSplitsARune: the CLI answers and fails in Chinese, and a byte
// cut leaves half a multi-byte rune for the client to render as mojibake.
func TestTruncateNeverSplitsARune(t *testing.T) {
	text := strings.Repeat("错", 200) // 600 bytes, three per rune
	// 401 lands inside the third rune, at offset 2 of the bytes 402..404.
	got := truncate(text, 401)
	if !utf8.ValidString(got) {
		t.Fatalf("truncate cut a rune in half: %q", got)
	}
	if want := strings.Repeat("错", 133) + "…"; got != want {
		t.Fatalf("truncate = %q, want the rune-aligned prefix %q", got, want)
	}

	// A limit that lands exactly on a boundary keeps every byte it was given.
	aligned := truncate(text, 402)
	if want := strings.Repeat("错", 134) + "…"; aligned != want {
		t.Fatalf("truncate at a boundary = %q, want %q", aligned, want)
	}

	// Short enough to pass through untouched, and a limit of zero must not slice
	// the string into something unprintable.
	if got := truncate("  已经够短  ", 400); got != "已经够短" {
		t.Fatalf("truncate changed a short string: %q", got)
	}
	if got := truncate(text, 0); !utf8.ValidString(got) || got != "…" {
		t.Fatalf("truncate at 0 = %q", got)
	}
}

// TestWorkerLocationRunsTheRuntimeBeforeTheFlags covers the argv shape a desktop
// install produces, which is the primary install: the Electron host is launched
// as a node runtime, so the worker script is argv[0] and every CLI flag follows
// it. Nothing checked that the two argv shapes are not swapped.
func TestWorkerLocationRunsTheRuntimeBeforeTheFlags(t *testing.T) {
	loc := Location{HostExe: `C:\Qoder CN\Qoder CN.exe`, RuntimeJS: `C:\Qoder CN\worker.mjs`}
	if !loc.useWorker() {
		t.Fatal("a location with a runtime JS must be a worker location")
	}
	c := NewClient(loc, 0)
	name, argv := c.commandArgs([]string{"--print", "--tools", ""})
	if name != loc.HostExe {
		t.Fatalf("host = %q, want the Electron executable %q", name, loc.HostExe)
	}
	want := []string{loc.RuntimeJS, "--print", "--tools", ""}
	if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", argv, want)
	}

	// The standalone install has no runtime script, so the flags stand alone.
	standalone := NewClient(Location{HostExe: `/usr/local/bin/qodercli`}, 0)
	name, argv = standalone.commandArgs([]string{"--print"})
	if name != `/usr/local/bin/qodercli` || strings.Join(argv, "\x00") != "--print" {
		t.Fatalf("standalone argv = %q %q", name, argv)
	}
}

// TestTurnCeilingReadsTheOperatorOverride covers the switch itself: empty means
// the default, 0 means off, and a value that is not a duration must not silently
// become "no ceiling at all" -- the default is the safe answer to a typo.
func TestTurnCeilingReadsTheOperatorOverride(t *testing.T) {
	t.Setenv(turnCeilingEnv, "")
	if got := turnCeiling(); got != defaultTurnCeiling {
		t.Fatalf("unset = %s, want the %s default", got, defaultTurnCeiling)
	}
	t.Setenv(turnCeilingEnv, "0")
	if got := turnCeiling(); got != 0 {
		t.Fatalf("0 = %s, want the backstop switched off", got)
	}
	t.Setenv(turnCeilingEnv, "90s")
	if got := turnCeiling(); got != 90*time.Second {
		t.Fatalf("90s = %s", got)
	}
	t.Setenv(turnCeilingEnv, "half an hour")
	if got := turnCeiling(); got != defaultTurnCeiling {
		t.Fatalf("a bad value = %s, want the default rather than no ceiling", got)
	}
	if defaultTurnCeiling != 30*time.Minute {
		t.Fatalf("the default ceiling is %s, not the documented 30 minutes", defaultTurnCeiling)
	}
}
