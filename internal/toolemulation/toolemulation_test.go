package toolemulation

import (
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestLooksLikeMissedToolUseDetectsLocalToolAvoidance(t *testing.T) {
	cases := []string{
		"我需要使用终端工具来查看内存。",
		"由于当前环境限制，请手动运行 top。",
		"当前环境限制，我无法直接执行系统命令查看你的内存占用。",
		"你可以在终端中运行 top -l 1 | grep PhysMem。",
		"I need to read the file first.",
		"Let me use the web search tool.",
		"You can run the following command in your terminal.",
		"现在我需要切换到计划模式。",
		"现在我将编辑文件，在末尾追加一行 beta，然后生成 unified diff。",
	}
	for _, tc := range cases {
		if !LooksLikeMissedToolUse(tc) {
			t.Fatalf("LooksLikeMissedToolUse(%q) = false", tc)
		}
	}
}

func TestLooksLikeRefusalDetectsLocalAccessRefusals(t *testing.T) {
	cases := []string{
		"当前环境限制，我无法直接执行系统命令查看你的内存占用。",
		"我无法访问你的电脑或本机文件。",
		"I cannot execute commands in your local machine.",
		"I can't access your computer directly.",
	}
	for _, tc := range cases {
		if !LooksLikeRefusal(tc) {
			t.Fatalf("LooksLikeRefusal(%q) = false", tc)
		}
	}
}

// memoryProbeTokens maps a platform to the tools its memory probe must use.
// The test asserts the command runs where it will be executed, so a probe that
// only works on another OS fails here instead of in the user's terminal.
var memoryProbeTokens = map[string][]string{
	"darwin":  {"vm_stat", "memory_pressure"},
	"windows": {"powershell"},
	"linux":   {"free"},
}

func TestInferToolCallsFromTextConvertsMemoryRefusalToBash(t *testing.T) {
	calls := InferToolCallsFromText("当前无法执行系统命令。你可以运行 vm_stat 查看内存占用。", []ToolDef{{
		Name: "Bash",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
			},
			"required": []any{"command"},
		},
	}})
	own, known := memoryProbeTokens[runtime.GOOS]
	if !known {
		// A platform with no probe must synthesize nothing rather than a command
		// whose shell does not exist.
		if len(calls) != 0 {
			t.Fatalf("no memory probe is defined for %s, got %+v", runtime.GOOS, calls)
		}
		return
	}
	if len(calls) != 1 {
		t.Fatalf("call count = %d", len(calls))
	}
	if calls[0].Name != "Bash" {
		t.Fatalf("tool name = %q", calls[0].Name)
	}
	command, _ := calls[0].Arguments["command"].(string)
	for _, want := range own {
		if !strings.Contains(command, want) {
			t.Fatalf("the probe for %s must use %q, got %q", runtime.GOOS, want, command)
		}
	}
	for platform, tokens := range memoryProbeTokens {
		if platform == runtime.GOOS {
			continue
		}
		for _, foreign := range tokens {
			if strings.Contains(command, foreign) {
				t.Fatalf("the %s probe must not use the %s-only %q, got %q", runtime.GOOS, platform, foreign, command)
			}
		}
	}
}

func TestLooksLikeMissedToolUseIgnoresFinalAnswers(t *testing.T) {
	text := "这个文件负责 HTTP API 路由和 OpenAI 兼容响应。"
	if LooksLikeMissedToolUse(text) {
		t.Fatalf("LooksLikeMissedToolUse(%q) = true", text)
	}
}

// TestLooksLikeMissedToolUseIgnoresNarration pins the 933 P3-T3 half of the
// contract: the retry gate fires on refusal or unfinished intent, not on an
// answer that merely talks about the same topic. Each of these used to send the
// whole turn through a second upstream round trip because the sentence
// mentioned a file edit, a command or a weather lookup.
func TestLooksLikeMissedToolUseIgnoresNarration(t *testing.T) {
	cases := []string{
		"你可以编辑文件来修改这个配置。",
		"这个模块负责解析执行命令的输出格式。",
		"用户可以读取文件并查看文件内容。",
		"这里不涉及查询天气的逻辑，缓存由上游负责。",
		"如果需要查询天气，请告诉我城市和日期。",
		"The tool reads the file and reports the command output.",
		"This helper returns a web search result for the query.",
	}
	for _, tc := range cases {
		if LooksLikeMissedToolUse(tc) {
			t.Fatalf("LooksLikeMissedToolUse(%q) = true, want plain narration", tc)
		}
	}
}

func TestInjectToolingIncludesAutoToolGuidance(t *testing.T) {
	prompt := InjectTooling("", []ToolDef{{
		Name:        "read_file",
		Description: "Read a text file.",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
			"required": []any{"path"},
		},
	}}, ToolChoice{Mode: "auto"}, nil)
	if prompt == "" {
		t.Fatal("empty prompt")
	}
	for _, want := range []string{
		"tool_choice=auto means you must decide",
		"inspect a local file path",
		"Core tool syntax examples",
		"conceptual question",
		"NEVER ask the user to run a command",
		"Emit at most 5 independent tool actions",
		"no preamble",
		"Shell tool calls are stateless",
		"optional commands such as `tree`",
		"exclude node_modules",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestCoreToolExamplesSupportsCodexStyleToolNames(t *testing.T) {
	prompt := InjectTooling("", []ToolDef{
		{
			Name:        "exec_command",
			Description: "Run a shell command",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"cmd": map[string]any{"type": "string"},
				},
				"required": []any{"cmd"},
			},
		},
		{
			Name:        "apply_patch",
			Description: "Edit files",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"patch": map[string]any{"type": "string"},
				},
				"required": []any{"patch"},
			},
		},
	}, ToolChoice{Mode: "auto"}, nil)

	for _, want := range []string{
		"Run shell commands, inspect memory/CPU/processes/ports, build or test code: use exec_command.",
		"Edit files: use apply_patch.",
		"\"tool\":\"exec_command\"",
		"\"cmd\":\"pwd\"",
		"\"tool\":\"apply_patch\"",
		"\"patch\":\"value\"",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestInjectToolingEditRuleFallsBackToExecCommandWhenNoPatchToolExists(t *testing.T) {
	prompt := InjectTooling("", []ToolDef{
		{
			Name:        "exec_command",
			Description: "Run a shell command",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"cmd": map[string]any{"type": "string"},
				},
				"required": []any{"cmd"},
			},
		},
	}, ToolChoice{Mode: "auto"}, nil)

	if !strings.Contains(prompt, "use exec_command with targeted shell commands to modify the file") {
		t.Fatalf("prompt missing exec_command edit rule:\n%s", prompt)
	}
	if strings.Contains(prompt, "call patch or write_file") {
		t.Fatalf("prompt should not mention unavailable patch/write_file tools:\n%s", prompt)
	}
}

func TestExtractToolsSupportsResponsesFunctionShape(t *testing.T) {
	tools := ExtractTools([]any{
		map[string]any{
			"type":        "function",
			"name":        "exec_command",
			"description": "Runs a command",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"cmd": map[string]any{"type": "string"},
				},
				"required": []any{"cmd"},
			},
		},
	})
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
	if tools[0].Name != "exec_command" {
		t.Fatalf("unexpected tool name %q", tools[0].Name)
	}
	props, _ := tools[0].InputSchema["properties"].(map[string]any)
	if _, ok := props["cmd"]; !ok {
		t.Fatalf("expected responses schema properties to be preserved")
	}
}

func TestExtractAnthropicToolsSkipsHostedWebSearch(t *testing.T) {
	tools := ExtractAnthropicTools([]any{
		map[string]any{
			"name": "web_search",
			"type": "web_search_20250305",
		},
		map[string]any{
			"name": "read_file",
			"input_schema": map[string]any{
				"type": "object",
			},
		},
	})
	if len(tools) != 1 {
		t.Fatalf("tool count = %d", len(tools))
	}
	if tools[0].Name != "read_file" {
		t.Fatalf("tool = %+v", tools[0])
	}
}

func TestParseActionBlocksMapsCommonToolAliases(t *testing.T) {
	text := "```json action\n{\"tool\":\"Bash\",\"parameters\":{\"command\":\"pwd\",\"extra\":true}}\n```"
	calls, clean, err := ParseActionBlocks(text, []ToolDef{{
		Name: "terminal",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
			},
		},
	}}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if clean != "" {
		t.Fatalf("clean = %q", clean)
	}
	if len(calls) != 1 {
		t.Fatalf("call count = %d", len(calls))
	}
	if calls[0].Name != "terminal" {
		t.Fatalf("tool name = %q", calls[0].Name)
	}
	if _, ok := calls[0].Arguments["command"]; !ok {
		t.Fatalf("missing command arg: %+v", calls[0].Arguments)
	}
	if _, ok := calls[0].Arguments["extra"]; ok {
		t.Fatalf("unexpected extra arg: %+v", calls[0].Arguments)
	}
}

func TestParseActionBlocksMapsReadAlias(t *testing.T) {
	text := "```json action\n{\"name\":\"Read\",\"arguments\":{\"path\":\"/tmp/a.txt\"}}\n```"
	calls, _, err := ParseActionBlocks(text, []ToolDef{{Name: "read_file"}}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Name != "read_file" {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestParseActionBlocksDropsCallsMissingRequiredArgs(t *testing.T) {
	text := "```json action\n{\"tool\":\"Read\",\"parameters\":{\"path\":\"/tmp/a.txt\"}}\n```"
	calls, clean, err := ParseActionBlocks(text, []ToolDef{{
		Name: "Read",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"file_path": map[string]any{"type": "string"},
			},
			"required": []any{"file_path"},
		},
	}}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("expected no calls, got %+v", calls)
	}
	if !strings.Contains(clean, "\"path\"") {
		t.Fatalf("clean should preserve unparseable action block, got %q", clean)
	}
}

func TestParseActionBlocksDropsUnknownToolNames(t *testing.T) {
	text := "```json action\n{\"tool\":\"apply_patch\",\"parameters\":{\"patch\":\"*** Begin Patch\"}}\n```"
	calls, clean, err := ParseActionBlocks(text, []ToolDef{{
		Name: "exec_command",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"cmd": map[string]any{"type": "string"},
			},
		},
	}}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("expected no calls, got %+v", calls)
	}
	if !strings.Contains(clean, "\"apply_patch\"") {
		t.Fatalf("clean should preserve unknown tool action block, got %q", clean)
	}
}

// TestParseActionBlocksDeduplicatesCalls pins the dedup half: a call the model
// repeats is the model stuttering, so the repeat's text is removed with the
// first one. That is the one case where a block's text legitimately disappears
// without a call being made.
func TestParseActionBlocksDeduplicatesCalls(t *testing.T) {
	calls, clean, err := ParseActionBlocks(duplicateLimitFixture(12), []ToolDef{{
		Name: "Bash",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
			},
			"required": []any{"command"},
		},
	}}, Config{MaxToolCalls: 12})
	if err != nil {
		t.Fatal(err)
	}
	// 12 blocks carrying 7 distinct commands: the 5 repeats are removed with
	// the call they repeat, and every distinct call is sent.
	if clean != "" {
		t.Fatalf("clean = %q", clean)
	}
	if len(calls) != 7 {
		t.Fatalf("call count = %d, calls = %+v", len(calls), calls)
	}
	if calls[0].Arguments["command"] != "pwd" {
		t.Fatalf("first command = %+v", calls[0].Arguments)
	}
}

// duplicateLimitFixture builds blocks whose commands repeat every other one, so
// a run of n blocks carries ceil(n/2) distinct calls.
func duplicateLimitFixture(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		command := "pwd"
		if i%2 == 1 {
			command = "ls " + string(rune('a'+i))
		}
		b.WriteString("```json action\n")
		b.WriteString(`{"tool":"Bash","parameters":{"command":"` + command + `"}}`)
		b.WriteString("\n```\n")
	}
	return b.String()
}

// TestParseActionBlocksKeepsSuppressedCallsAsProse pins the 933 P2-T3 contract:
// a turn over the call ceiling must not delete a block it is also not sending.
// The client then sees the text that was dropped plus one line saying how many
// calls the ceiling held back — previously it saw neither, and a stop_reason of
// tool_use hid it.
func TestParseActionBlocksKeepsSuppressedCallsAsProse(t *testing.T) {
	tools := []ToolDef{{
		Name: "Bash",
		InputSchema: map[string]any{
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
			},
			"required": []any{"command"},
		},
	}}
	// Six distinct commands with the ceiling at two: four blocks are over it.
	text := "```json action\n" + `{"tool":"Bash","parameters":{"command":"pwd"}}` + "\n```\n" +
		"```json action\n" + `{"tool":"Bash","parameters":{"command":"ls -a"}}` + "\n```\n" +
		"```json action\n" + `{"tool":"Bash","parameters":{"command":"ls -b"}}` + "\n```\n" +
		"```json action\n" + `{"tool":"Bash","parameters":{"command":"ls -c"}}` + "\n```\n" +
		"```json action\n" + `{"tool":"Bash","parameters":{"command":"ls -d"}}` + "\n```\n" +
		"```json action\n" + `{"tool":"Bash","parameters":{"command":"ls -e"}}` + "\n```"

	calls, clean, err := ParseActionBlocks(text, tools, Config{MaxToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].Arguments["command"] != "pwd" || calls[1].Arguments["command"] != "ls -a" {
		t.Fatalf("calls = %+v, want the first two only", calls)
	}
	for _, dropped := range []string{"ls -b", "ls -c", "ls -d", "ls -e"} {
		if !strings.Contains(clean, dropped) {
			t.Fatalf("a block that was not sent must stay in the text; %q missing from %q", dropped, clean)
		}
	}
	for _, sent := range []string{"pwd", "ls -a"} {
		if strings.Contains(clean, `"command":"`+sent+`"`) {
			t.Fatalf("a block that was sent must be removed; %q still in %q", sent, clean)
		}
	}
	if !strings.Contains(clean, "4 additional tool calls not sent") {
		t.Fatalf("the suppression must be visible, clean = %q", clean)
	}
	if !strings.Contains(clean, maxToolCallsEnv) {
		t.Fatalf("the notice must name the knob that lifts the ceiling, clean = %q", clean)
	}
}

// TestMaxToolCallsEnvironmentWiring pins that the ceiling is reachable in
// production: every production caller passes Config{}, so without the
// environment the number would be a constant nobody can move.
func TestMaxToolCallsEnvironmentWiring(t *testing.T) {
	tools := []ToolDef{{Name: "Bash", InputSchema: map[string]any{
		"properties": map[string]any{"command": map[string]any{"type": "string"}},
		"required":   []any{"command"},
	}}}
	text := "```json action\n" + `{"tool":"Bash","parameters":{"command":"pwd"}}` + "\n```\n" +
		"```json action\n" + `{"tool":"Bash","parameters":{"command":"ls -a"}}` + "\n```"

	t.Setenv(maxToolCallsEnv, "1")
	if _, clean, err := ParseActionBlocks(text, tools, Config{}); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(clean, "ls -a") {
		t.Fatalf("LINGMA_MAX_TOOL_CALLS=1 must drop the second call to prose, clean = %q", clean)
	}
	t.Setenv(maxToolCallsEnv, "2")
	if _, clean, err := ParseActionBlocks(text, tools, Config{}); err != nil {
		t.Fatal(err)
	} else if strings.Contains(clean, "not sent") {
		t.Fatalf("LINGMA_MAX_TOOL_CALLS=2 must fit both calls, clean = %q", clean)
	}
	// An explicit Config field still wins over the environment, and a bad value
	// degrades to the default instead of to an unbounded turn.
	t.Setenv(maxToolCallsEnv, "not-a-number")
	calls, _, err := ParseActionBlocks(text, tools, Config{MaxToolCalls: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("Config.MaxToolCalls must win over the environment, calls = %+v", calls)
	}
	if got := effectiveMaxToolCalls(Config{}); got != defaultMaxToolCalls {
		t.Fatalf("a malformed %s must fall back to %d, got %d", maxToolCallsEnv, defaultMaxToolCalls, got)
	}
}

// TestInjectedPromptStatesTheEnforcedCeiling keeps the promise the prompt makes
// and the ceiling the parser enforces as one number.
func TestInjectedPromptStatesTheEnforcedCeiling(t *testing.T) {
	tools := []ToolDef{{Name: "read_file", InputSchema: map[string]any{
		"properties": map[string]any{"path": map[string]any{"type": "string"}},
		"required":   []any{"path"},
	}}}
	want := "- Emit at most " + strconv.Itoa(defaultMaxToolCalls) + " independent tool actions"
	if prompt := InjectTooling("", tools, ToolChoice{Mode: "auto"}, nil); !strings.Contains(prompt, want) {
		t.Fatalf("prompt must promise the enforced ceiling %q:\n%s", want, prompt)
	}
	t.Setenv(maxToolCallsEnv, "7")
	want = "- Emit at most 7 independent tool actions"
	if prompt := InjectTooling("", tools, ToolChoice{Mode: "auto"}, nil); !strings.Contains(prompt, want) {
		t.Fatalf("prompt must follow %s, want %q:\n%s", maxToolCallsEnv, want, prompt)
	}
}

// The XML dialect tags are spelled out so this file cannot itself read like a
// tool call in an agent transcript.
const (
	xOpen      = "<" + "tool_call" + ">"
	xClose     = "</" + "tool_call" + ">"
	xFuncOpen  = "<" + "function="
	xFuncClose = "</" + "function" + ">"
	xParamOpen = "<" + "parameter="
	xParamEnd  = "</" + "parameter" + ">"
)

func xParam(name, value string) string {
	return xParamOpen + name + ">\n" + value + "\n" + xParamEnd
}

func xCall(name string, params ...string) string {
	return xOpen + "\n" + xFuncOpen + name + ">\n" + strings.Join(params, "\n") + "\n" + xFuncClose + "\n" + xClose
}

func xmlSchemaTool(name string, props map[string]any, required ...string) ToolDef {
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		keys := make([]any, 0, len(required))
		for _, key := range required {
			keys = append(keys, key)
		}
		schema["required"] = keys
	}
	return ToolDef{Name: name, InputSchema: schema}
}

func xmlTools() []ToolDef {
	return []ToolDef{
		xmlSchemaTool("Bash", map[string]any{
			"command":                   map[string]any{"type": "string"},
			"timeout":                   map[string]any{"type": "integer"},
			"dangerouslyDisableSandbox": map[string]any{"type": "boolean"},
		}, "command"),
		xmlSchemaTool("Read", map[string]any{"file_path": map[string]any{"type": "string"}}, "file_path"),
	}
}

// TestParseActionBlocksReadsTheNativeXMLDialect uses the shape captured from a
// real session: prose, then calls whose parameters arrive as raw text lines.
func TestParseActionBlocksReadsTheNativeXMLDialect(t *testing.T) {
	text := "核对两件事：\n" +
		xCall("Bash",
			xParam("command", "ls -a"),
			xParam("timeout", "15000"),
			xParam("dangerouslyDisableSandbox", "false")) +
		"\n" +
		xCall("Read", xParam("file_path", `C:\Users\me\output.txt`)) +
		"\n结果回来后我给终审结论。"

	calls, clean, err := ParseActionBlocks(text, xmlTools(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls = %+v", calls)
	}
	if calls[0].Name != "Bash" || calls[0].Arguments["command"] != "ls -a" {
		t.Fatalf("calls[0] = %+v", calls[0])
	}
	if calls[0].Arguments["timeout"] != int64(15000) {
		t.Fatalf("timeout should coerce to an integer, got %#v", calls[0].Arguments["timeout"])
	}
	if calls[0].Arguments["dangerouslyDisableSandbox"] != false {
		t.Fatalf("boolean should coerce, got %#v", calls[0].Arguments["dangerouslyDisableSandbox"])
	}
	if calls[1].Name != "Read" || calls[1].Arguments["file_path"] != `C:\Users\me\output.txt` {
		t.Fatalf("calls[1] = %+v", calls[1])
	}
	if strings.Contains(clean, xOpen) || strings.Contains(clean, xFuncOpen) {
		t.Fatalf("clean text still carries the wire format: %q", clean)
	}
	if !strings.Contains(clean, "核对两件事") || !strings.Contains(clean, "终审结论") {
		t.Fatalf("clean text lost the prose around the calls: %q", clean)
	}
}

func TestParseActionBlocksRejectsXMLCallsForUnknownTools(t *testing.T) {
	text := "示例：" + xCall("NotATool", xParam("command", "ls"))
	calls, clean, err := ParseActionBlocks(text, xmlTools(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("an undeclared tool must not produce a call: %+v", calls)
	}
	if !strings.Contains(clean, xOpen) {
		t.Fatalf("rejected blocks stay verbatim, got %q", clean)
	}
}

func TestParseActionBlocksLeavesUnterminatedXMLAsProse(t *testing.T) {
	text := "开头" + xOpen + "\n" + xFuncOpen + "Bash>\n" + xParam("command", "ls")
	calls, clean, err := ParseActionBlocks(text, xmlTools(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("an open block cannot be a call: %+v", calls)
	}
	if !strings.Contains(clean, xOpen) {
		t.Fatalf("unterminated text must survive, got %q", clean)
	}
}

func TestFindActionBlockSpanHandlesTheXMLDialect(t *testing.T) {
	tools := xmlTools()
	block := xCall("Bash", xParam("command", "ls"))
	text := "前段" + block + "后段"

	start, end, pending := FindActionBlockSpan(text, tools)
	if pending {
		t.Fatal("a closed block must not report pending")
	}
	if start < 0 || end <= start || text[start:end] != block {
		t.Fatalf("span = [%d,%d) of %q", start, end, text)
	}

	open := "前段" + xOpen + "\n" + xFuncOpen + "Bash>\n" + xParam("command", "ls")
	if _, _, stillPending := FindActionBlockSpan(open, tools); !stillPending {
		t.Fatal("an unterminated block must hold the streamer back")
	}
}

// TestActionBlockScannerMatchesOneShotSpan is the "keep the parse results
// identical" gate: a scanner-driven feed (what toolStreamFilter.Push does) must
// emit exactly what the stateless FindActionBlockSpan feed emits, delta by
// delta, for prose, fenced blocks, rejected blocks, the XML dialect, split
// fences and unterminated tails.
func TestActionBlockScannerMatchesOneShotSpan(t *testing.T) {
	tools := xmlTools()
	rejected := "```json action\n" + `{"tool":"NotATool","parameters":{"x":1}}` + "\n```"
	fenceInString := "```json action\n" +
		"{\"tool\":\"Bash\",\"parameters\":{\"command\":\"echo ``` \\\"quoted\\\"\"}}" +
		"\n```"
	deltas := [][]string{
		{"just prose, no fence"},
		{"先看", "```json act", "ion\n{\"tool\":\"Bash\",\"parameters\":{\"command\":\"pwd\"}}", "\n```", "之后"},
		{"a ", rejected, " b ", "```json action\n" + `{"tool":"Bash","parameters":{"command":"pwd"}}` + "\n```", " c"},
		{"x ", xCall("Bash", xParam("command", "ls")), " y"},
		{fenceInString},
		{"开头", xOpen + "\n" + xFuncOpen + "Bash>\n" + xParam("command", "ls"), "\n" + xFuncClose + "\n" + xClose, "尾"},
		{"tail ", "```json act"},
		{"fence ``` inside prose stays ", "```json action\n{\"tool\":\"Bash\",\"parameters\":{\"command\":\"pwd\"}}\n```"},
	}
	// One long scenario fed two bytes at a time stresses every split boundary.
	var bytewise []string
	combined := "序" + rejected + "中" + xCall("Bash", xParam("command", "pwd")) + "```json action\n" + `{"tool":"Bash","parameters":{"command":"pwd"}}` + "\n```尾"
	for i := 0; i < len(combined); i += 2 {
		end := i + 2
		if end > len(combined) {
			end = len(combined)
		}
		bytewise = append(bytewise, combined[i:end])
	}
	deltas = append(deltas, bytewise)

	// Hybrid dialects captured from real sessions: an XML wrapper around a
	// plain JSON body, and a fence closed by an XML tag.
	deltas = append(deltas,
		[][]string{
			{"hybrid ", xOpen + "\n" + `{"tool":"Bash","parameters":{"command":"hy"}}`, " tail"},
			{"```json action\n" + `{"tool":"Bash","parameters":{"command":"hx"}}` + "\n" + xClose, " tail2"},
			{xOpen + "json action\n" + `{"tool":"Bash","parameters":{"command":"hz"}}`, "\n```"},
		}...,
	)

	for i, feed := range deltas {
		scanner := NewActionBlockScanner(tools)
		scanPending, oneShotPending := "", ""
		var scanOut, oneShotOut strings.Builder

		pushScanner := func(delta string) {
			scanPending += delta
			for {
				start, end, unterminated := scanner.FindSpan(scanPending)
				switch {
				case unterminated:
					if start > 0 {
						scanOut.WriteString(scanPending[:start])
						scanner.Discard(start)
						scanPending = scanPending[start:]
					}
					return
				case end > 0:
					if start > 0 {
						scanOut.WriteString(scanPending[:start])
					}
					scanner.Discard(end)
					scanPending = scanPending[end:]
				default:
					safe := len(scanPending) - ActionOpenPrefixHold(scanPending)
					if safe > 0 {
						scanOut.WriteString(scanPending[:safe])
						scanner.Discard(safe)
						scanPending = scanPending[safe:]
					}
					return
				}
			}
		}
		pushOneShot := func(delta string) {
			oneShotPending += delta
			for {
				start, end, unterminated := FindActionBlockSpan(oneShotPending, tools)
				switch {
				case unterminated:
					if start > 0 {
						oneShotOut.WriteString(oneShotPending[:start])
						oneShotPending = oneShotPending[start:]
					}
					return
				case end > 0:
					if start > 0 {
						oneShotOut.WriteString(oneShotPending[:start])
					}
					oneShotPending = oneShotPending[end:]
				default:
					safe := len(oneShotPending) - ActionOpenPrefixHold(oneShotPending)
					if safe > 0 {
						oneShotOut.WriteString(oneShotPending[:safe])
						oneShotPending = oneShotPending[safe:]
					}
					return
				}
			}
		}

		for _, d := range feed {
			pushScanner(d)
			pushOneShot(d)
			if scanOut.String() != oneShotOut.String() {
				t.Fatalf("scenario %d after delta %q: scanner emitted %q, one-shot emitted %q",
					i, d, scanOut.String(), oneShotOut.String())
			}
			if scanPending != oneShotPending {
				t.Fatalf("scenario %d after delta %q: scanner holds %q, one-shot holds %q",
					i, d, scanPending, oneShotPending)
			}
		}
		emitted := scanOut.String()
		scanOut.WriteString(scanPending)
		oneShotOut.WriteString(oneShotPending)
		if scanOut.String() != oneShotOut.String() {
			t.Fatalf("scenario %d after flush: scanner=%q one-shot=%q", i, scanOut.String(), oneShotOut.String())
		}

		// End-of-stream composition: what the stream emitted plus what a final
		// parse keeps from the withheld remainder must equal the one-shot clean
		// text of the whole turn. toolStreamFilter.Flush relies on this.
		var full strings.Builder
		for _, d := range feed {
			full.WriteString(d)
		}
		_, want, wantErr := ParseActionBlocks(full.String(), tools, Config{})
		if wantErr != nil {
			t.Fatalf("scenario %d: %v", i, wantErr)
		}
		_, flushKeeps, flushErr := ParseActionBlocks(scanPending, tools, Config{})
		if flushErr != nil {
			t.Fatalf("scenario %d flush: %v", i, flushErr)
		}
		if got := emitted + flushKeeps; squashSpaces(got) != squashSpaces(want) {
			t.Fatalf("scenario %d composition: streamed+flush=%q want clean=%q", i, got, want)
		}
	}
}

// squashSpaces collapses whitespace runs so the composition check compares
// visible content: ParseActionBlocks trims its clean output, which can differ
// from the streamed rendering by one space at a block boundary.
func squashSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TestParseActionBlocksReadsTheHybridDialects uses shapes captured from real
// sessions (see ITERATION.md 2026-09-28) where the model mixed the fenced and
// XML dialects: an XML wrapper around a plain JSON body, a fence closed by an
// XML tag, an inline label, and a wrapper that never closes at all.
func TestParseActionBlocksReadsTheHybridDialects(t *testing.T) {
	tools := xmlTools()
	jsonBody := `{"tool": "Bash", "parameters": {"command": "ls -a"}}`

	cases := []struct {
		name string
		text string
	}{
		{"xml wrapping plain json, never closed", xOpen + "\n" + jsonBody},
		{"xml open, inner label, xml close junk", xOpen + "\n<json action>\n" + jsonBody + "\n</parameter>\n</function>\n" + xClose + "\n</think>"},
		{"fence open, xml close", "```json action\n" + jsonBody + "\n" + xClose},
		{"xml open with inline label, fence close", xOpen + "json action\n" + jsonBody + "\n```"},
		{"fence open, parameter close junk", "```json action\n" + jsonBody + "\n</parameter>\n</function>\n" + xClose},
		{"prose around a hybrid block", "先说结论。\n" + xOpen + "\n" + jsonBody + "\n" + xClose + "\n后面是正文。"},
	}

	for _, tc := range cases {
		calls, clean, err := ParseActionBlocks(tc.text, tools, Config{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(calls) != 1 || calls[0].Name != "Bash" || calls[0].Arguments["command"] != "ls -a" {
			t.Fatalf("%s: calls = %+v", tc.name, calls)
		}
		if strings.Contains(clean, xOpen) || strings.Contains(clean, "json action") {
			t.Fatalf("%s: wrapper markers must be swallowed, clean = %q", tc.name, clean)
		}
	}
}

// TestParseActionBlocksKeepsRejectingBrokenHybrids pins the negative side of
// the hybrid acceptance: a wrapper with no parseable body stays prose.
func TestParseActionBlocksKeepsRejectingBrokenHybrids(t *testing.T) {
	tools := xmlTools()
	cases := []struct {
		name string
		text string
	}{
		{"empty shell", xOpen + "\n</function>\n" + xClose},
		{"wrapper with no body", xOpen + "\n" + xClose},
		{"unknown tool in json body", xOpen + "\n" + `{"tool": "NotATool", "parameters": {"command": "ls"}}` + "\n" + xClose},
	}
	for _, tc := range cases {
		calls, clean, err := ParseActionBlocks(tc.text, tools, Config{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(calls) != 0 {
			t.Fatalf("%s: no call may survive, got %+v", tc.name, calls)
		}
		if !strings.Contains(clean, xOpen) {
			t.Fatalf("%s: rejected text must stay verbatim, got %q", tc.name, clean)
		}
	}
}

// TestParseActionBlocksRepairsStrayEscapes pins the normalizeJSON repair for
// the \( and \' escapes real models leave inside command strings, captured in
// the same sessions as the hybrid dialects.
func TestParseActionBlocksRepairsStrayEscapes(t *testing.T) {
	tools := xmlTools()
	text := xOpen + "\n" + `{"tool": "Bash", "parameters": {"command": "echo 'a\\(b\\)c'"}}` + "\n" + xClose
	calls, _, err := ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Arguments["command"] != `echo 'a\(b\)c'` {
		t.Fatalf("calls = %+v", calls)
	}
}

// --- 931 phase-one boundary fixtures ---------------------------------------
//
// The cases below pin the mixed-dialect acceptance boundaries from 931.md §2:
// the span of an accepted call may never end before or inside its JSON body,
// nested examples must not turn into extra calls, close junk must not leak
// across delta splits, prose and real code fences after the body must survive,
// a label mentioning a tag is not an XML structure, and legal smart-quoted
// values must parse without normalization damage.

type hybridCase struct {
	name      string
	text      string
	wantCalls []ToolCall // only Name and Arguments are compared
	wantClean string
}

func bashWant(command string) []ToolCall {
	return []ToolCall{{Name: "Bash", Arguments: map[string]any{"command": command}}}
}

func hybridCases() []hybridCase {
	const lsJSON = `{"tool":"Bash","parameters":{"command":"ls"}}`
	// A rejected parent block stays verbatim, so its wantClean is its own text.
	rejectedXML := xOpen + "\n" + xFuncOpen + "NotATool>\n" + xParamOpen + "command>x ```json action\n" + lsJSON + "\n" + xParamEnd + "\n" + xFuncClose + "\n" + xClose
	return []hybridCase{
		// P1-1: the close marker scan must not settle on a fence the label
		// mentions before the JSON body even starts.
		{"marker before body", "```json action\nuse ``` for code\n" + lsJSON + "\n```\n", bashWant("ls"), ""},
		{"glued label", "```json action" + lsJSON, bashWant("ls"), ""},
		{"xml close mentioned before body", xOpen + "\nsaw </tool_call> tags earlier\n" + lsJSON + "\n" + xClose, bashWant("ls"), ""},

		// P1-2: an odd quote in the label must not desync the scanner into the
		// JSON string where the parameter's own backticks live.
		{"odd quote in label", "```json action\nHe said \"hi\n" + `{"tool":"Bash","parameters":` + `{"command":"echo ` + "```" + `x` + "```" + `"}}` + "\n```", bashWant("echo ```x```"), ""},

		// P1-3: a fenced example inside a parent block's parameter is parent
		// payload, never an independent call — for accepted and rejected parents.
		{"nested example inside accepted xml", xOpen + "\n" + xFuncOpen + "Bash>\n" + xParamOpen + "command>Use this shape: ```json action\n" + `{"tool":"Bash","parameters":{"command":"echo hi"}}` + "\n" + xParamEnd + "\n" + xFuncClose + "\n" + xClose,
			bashWant("Use this shape: ```json action\n" + `{"tool":"Bash","parameters":{"command":"echo hi"}}`), ""},
		{"nested example inside rejected xml", rejectedXML,
			nil, rejectedXML},
		{"two independent blocks", "```json action\n" + lsJSON + "\n```\nmid\n```json action\n" + `{"tool":"Bash","parameters":{"command":"pwd"}}` + "\n```",
			[]ToolCall{{Name: "Bash", Arguments: map[string]any{"command": "ls"}}, {Name: "Bash", Arguments: map[string]any{"command": "pwd"}}}, "mid"},

		// 931 final review, red point 1: an unfinished parent structure shields
		// every opening inside it — a fenced example quoted in a parameter that
		// never closes is payload, never an independent call, and the whole
		// unfinished text stays verbatim.
		{"unfinished parent, fenced example inside", xOpen + "\n" + xFuncOpen + "Bash>\n" + xParamOpen + "command>Use this shape: ```json action\n" + `{"tool":"Bash","parameters":{"command":"echo hi"}}` + "\n" + xParamEnd + "\n" + xFuncClose,
			nil, xOpen + "\n" + xFuncOpen + "Bash>\n" + xParamOpen + "command>Use this shape: ```json action\n" + `{"tool":"Bash","parameters":{"command":"echo hi"}}` + "\n" + xParamEnd + "\n" + xFuncClose},
		{"unfinished parent, parameter never closed", xOpen + "\n" + xFuncOpen + "Bash>\n" + xParamOpen + "command>Use this shape: ```json action\n" + `{"tool":"Bash","parameters":{"command":"echo hi"}}`,
			nil, xOpen + "\n" + xFuncOpen + "Bash>\n" + xParamOpen + "command>Use this shape: ```json action\n" + `{"tool":"Bash","parameters":{"command":"echo hi"}}`},

		// 931 final review, red point 2: a literal block close inside a
		// parameter value must not end the consumed span, and the block must
		// not swallow an adjacent sibling that follows the real close.
		{"literal block close inside parameter value", xOpen + "\n" + xFuncOpen + "Bash>\n" + xParamOpen + "command>echo hi && cat x" + xClose + "_tail.txt" + xParamEnd + "\n" + xFuncClose + "\n" + xClose + "\nafter",
			bashWant("echo hi && cat x" + xClose + "_tail.txt"), "after"},
		{"literal close block followed by adjacent block", xOpen + "\n" + xFuncOpen + "Bash>\n" + xParamOpen + "command>echo x" + xClose + "y" + xParamEnd + "\n" + xFuncClose + "\n" + xClose + "\nmid\n" + xCall("Bash", xParam("command", "pwd")),
			[]ToolCall{{Name: "Bash", Arguments: map[string]any{"command": "echo x" + xClose + "y"}}, {Name: "Bash", Arguments: map[string]any{"command": "pwd"}}}, "mid"},

		// P2-1: close junk after the wrapper must be consumed, the answer kept.
		{"junk after close", xOpen + "\n" + lsJSON + "\n" + xClose + "\n</think>\nAnswer: hi", bashWant("ls"), "Answer: hi"},

		// P2-3: prose between the JSON and a distant marker survives; only the
		// call itself is removed.
		{"prose between body and marker", xOpen + "\n" + lsJSON + "\nIMPORTANT PROSE THAT MUST SURVIVE\n" + xClose,
			bashWant("ls"), "IMPORTANT PROSE THAT MUST SURVIVE\n" + xClose},

		// P3-1: a real go fence after a markerless body is a new code block.
		{"code fence after body", xOpen + "\n" + lsJSON + "\n\n```go\nfmt.Println(1)\n```\ntail",
			bashWant("ls"), "```go\nfmt.Println(1)\n```\ntail"},

		// P2-2: a label mentioning the tag shape is prose, not XML structure.
		{"label mentions function tag", xOpen + "\nI'll use <function=Bash> style:\n" + lsJSON, bashWant("ls"), ""},

		// P2-4: smart quotes inside a legal JSON string are payload.
		{"smart quotes in legal json", "```json action\n" + `{"tool":"Bash","parameters":{"command":"echo “}” done"}}` + "\n```",
			bashWant("echo “}” done"), ""},

		// 931 final root-cause pass. Dialect routing keys on the wrapper's
		// first structural token, and structure is never adopted past the
		// wrapper's own boundary — its close, or the next wrapper's opening,
		// whichever comes first. An XML example quoted inside a JSON string
		// is payload: the JSON body wins and the tags in the string never act
		// as structure. An empty or unterminated shell leaves the block after
		// it independent instead of merging both into one hijacked call.
		{"json string with xml function example",
			xOpen + "\n" + `{"tool":"Bash","parameters":{"command":"echo ` + xFuncOpen + `Bash>` + xParamOpen + `command>UNINTENDED` + xParamEnd + xFuncClose + xClose + `"}}` + "\n" + xClose,
			bashWant("echo " + xFuncOpen + `Bash>` + xParamOpen + `command>UNINTENDED` + xParamEnd + xFuncClose + xClose), ""},
		{"json string with xml invoke example",
			xOpen + "\n" + `{"tool":"Bash","parameters":{"command":"echo <invoke name=` + jsonQuote(`Bash`) + `>` + xParamOpen + `command>UNINTENDED` + xParamEnd + `</invoke>"}}` + "\n" + xClose,
			bashWant(`echo <invoke name="Bash">` + xParamOpen + `command>UNINTENDED` + xParamEnd + `</invoke>`), ""},
		{"empty xml shell before valid block",
			xOpen + xClose + "\n" + xOpen + xFuncOpen + "Bash>" + xParamOpen + "command>ls" + xParamEnd + xFuncClose + xClose,
			bashWant("ls"), xOpen + xClose},
		{"unterminated parent before new wrapper",
			xOpen + xFuncOpen + "Bash>" + xParamOpen + "command>first" + xParamEnd + "\n" + xOpen + xFuncOpen + "Bash>" + xParamOpen + "command>second" + xParamEnd + xFuncClose + xClose,
			nil, xOpen + xFuncOpen + "Bash>" + xParamOpen + "command>first" + xParamEnd + "\n" + xOpen + xFuncOpen + "Bash>" + xParamOpen + "command>second" + xParamEnd + xFuncClose + xClose},

		// 931 dialect-routing final pass. The dialect choice is relative, not
		// glued to the first byte: a JSON brace anywhere before the first tag
		// structure — including behind a glued "json action" label — routes
		// the body to the JSON reader. A structural empty shell (body nothing
		// but blanks before its own close) is rejected at that close so the
		// JSON block after it is never adopted. An unknown tool's JSON
		// carrying an XML example is rejected whole — the example's tags are
		// string payload, never a call to fall back to.
		{"json label string with xml function example",
			xOpen + "json action\n" + `{"tool":"Bash","parameters":{"command":"echo ` + xFuncOpen + `Bash>` + xParamOpen + `command>UNINTENDED` + xParamEnd + xFuncClose + xClose + `"}}` + "\n" + xClose,
			bashWant("echo " + xFuncOpen + `Bash>` + xParamOpen + `command>UNINTENDED` + xParamEnd + xFuncClose + xClose), ""},
		{"empty xml shell before json block",
			xOpen + xClose + "\n" + xOpen + "\n" + lsJSON + "\n" + xClose,
			bashWant("ls"), xOpen + xClose},
		{"unknown tool json with xml example rejected",
			xOpen + "\n" + `{"tool":"NotATool","parameters":{"command":"echo ` + xFuncOpen + `Bash>` + xParamOpen + `command>ls` + xParamEnd + xFuncClose + `"}}` + "\n" + xClose,
			nil, xOpen + "\n" + `{"tool":"NotATool","parameters":{"command":"echo ` + xFuncOpen + `Bash>` + xParamOpen + `command>ls` + xParamEnd + xFuncClose + `"}}` + "\n" + xClose},

		// 933 P1-1: a fence body that holds no brace of its own must not adopt
		// the JSON of the block behind it. The XML dialect already bounds that
		// search by the next opening; the fenced one did not, so an array sample
		// swallowed the real call (or, when the adopted body parsed, deleted the
		// prose in between).
		{"brace-less fence before xml call",
			"```json\n[1, 2]\n```\n" + xOpen + "\n" + lsJSON + "\n" + xClose,
			bashWant("ls"), "```json\n[1, 2]\n```"},
		{"brace-less fence before prose and xml call",
			"```json\n[1, 2]\n```\nIMPORTANT NOTE THAT MUST SURVIVE\n" + xOpen + "\n" + lsJSON + "\n" + xClose,
			bashWant("ls"), "```json\n[1, 2]\n```\nIMPORTANT NOTE THAT MUST SURVIVE"},

		// A body can hold more than one JSON object, and the call is not always
		// the first: a model that shows a note and then asks for a tool leaves
		// the note in the body. The reader steps over an object that names no
		// tool, and over nothing else.
		{"note object in front of the call",
			xOpen + "\n" + `{"note":"x"}` + "\n" + lsJSON + "\n" + xClose,
			bashWant("ls"), ""},
		{"note object in front of a fenced call",
			"```json action\n" + `{"note":"x"}` + "\n" + lsJSON + "\n" + "```",
			bashWant("ls"), ""},
		{"brace-less fence before a note and the call",
			"```json\n[1, 2]\n```\n" + xOpen + "\n" + `{"note":"x"}` + "\n" + lsJSON + "\n" + xClose,
			bashWant("ls"), "```json\n[1, 2]\n```"},

		// The step-over is narrow on purpose. An object that names a tool is the
		// block's answer, so an undeclared tool in front still rejects the whole
		// block instead of reaching for the call behind it; and a body that only
		// parses after the tolerant repairs is not a fragment either, so no
		// malformed text can be stepped over to reach a later object.
		{"named unknown tool in front is not stepped over",
			xOpen + "\n" + `{"tool":"NotATool","parameters":{"command":"ls"}}` + "\n" + lsJSON + "\n" + xClose,
			nil, xOpen + "\n" + `{"tool":"NotATool","parameters":{"command":"ls"}}` + "\n" + lsJSON + "\n" + xClose},
		{"trailing-comma fragment is not stepped over",
			xOpen + "\n" + `{"note":"x",}` + "\n" + lsJSON + "\n" + xClose,
			nil, xOpen + "\n" + `{"note":"x",}` + "\n" + lsJSON + "\n" + xClose},

		// 933 P3-T6: a fenced marker that never closed shields the openings
		// behind it only while it shows structure of its own. A bare label
		// cannot become decidable by waiting, so it is prose and the real call
		// behind it still runs.
		{"unterminated brace-less fence before xml call",
			"```json action\n" + xOpen + "\n" + lsJSON + "\n" + xClose,
			bashWant("ls"), "```json action"},

		// The XML dialect is the known limit 933 P3-T6 declined to change: an
		// unclosed wrapper still owns the whole rest of the turn, so the call
		// inside it does not run and its text stays verbatim. 931 pinned that
		// shielding for the unfinished-parent fixtures and the tightening that
		// would relax it belongs to the XML reader, not the fenced one.
		{"unterminated bare xml shell shields (known limit)",
			xOpen + "\n" + xOpen + "\n" + lsJSON + "\n" + xClose,
			nil, xOpen + "\n" + xOpen + "\n" + lsJSON + "\n" + xClose},
	}
}

// jsonQuote wraps s as a JSON string with its double quotes escaped, so test
// fixtures can embed quoted XML attributes inside a JSON body without a
// strconv dependency.
func jsonQuote(s string) string {
	return "\\\"" + strings.ReplaceAll(s, `"`, `\"`) + "\\\""
}

func TestParseActionBlocksHybridBoundaries(t *testing.T) {
	tools := xmlTools()
	for _, tc := range hybridCases() {
		calls, clean, err := ParseActionBlocks(tc.text, tools, Config{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(calls) != len(tc.wantCalls) {
			t.Fatalf("%s: got %d calls (%+v), want %d", tc.name, len(calls), calls, len(tc.wantCalls))
		}
		for i, want := range tc.wantCalls {
			if calls[i].Name != want.Name {
				t.Fatalf("%s: call %d name = %q, want %q", tc.name, i, calls[i].Name, want.Name)
			}
			if fmt.Sprintf("%v", calls[i].Arguments["command"]) != fmt.Sprintf("%v", want.Arguments["command"]) {
				t.Fatalf("%s: call %d command = %#v, want %#v", tc.name, i, calls[i].Arguments["command"], want.Arguments["command"])
			}
		}
		if clean != tc.wantClean {
			t.Fatalf("%s: clean = %q, want %q", tc.name, clean, tc.wantClean)
		}
	}
}

// TestParseActionBlocksKeepsInvalidEscapeSemantics pins the repair policy for
// escapes JSON does not define: the backslash is preserved (requoted as a
// legal escape) so regexes and Windows paths keep their bytes, instead of
// being silently dropped.
func TestParseActionBlocksKeepsInvalidEscapeSemantics(t *testing.T) {
	tools := xmlTools()
	cases := []struct {
		name, jsonBody, wantCommand string
	}{
		{"regex escape", `{"tool":"Bash","parameters":{"command":"grep \d+ f"}}`, `grep \d+ f`},
		{"windows path", `{"tool":"Bash","parameters":{"command":"type C:\Users\x"}}`, `type C:\Users\x`},
		{"stray paren escape", `{"tool":"Bash","parameters":{"command":"echo 'a\(b\)c'"}}`, `echo 'a\(b\)c'`},
		{"legal escaped backslash", `{"tool":"Bash","parameters":{"command":"grep \\d+ f"}}`, `grep \d+ f`},
		{"invalid unicode escape rejects", `{"tool":"Bash","parameters":{"command":"echo \uZZZZ"}}`, ""},
	}
	for _, tc := range cases {
		text := xOpen + "\n" + tc.jsonBody + "\n" + xClose
		calls, _, err := ParseActionBlocks(text, tools, Config{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if tc.wantCommand == "" {
			if len(calls) != 0 {
				t.Fatalf("%s: invalid unicode must be rejected, got %+v", tc.name, calls)
			}
			continue
		}
		if len(calls) != 1 || calls[0].Arguments["command"] != tc.wantCommand {
			t.Fatalf("%s: calls = %+v, want command %q", tc.name, calls, tc.wantCommand)
		}
	}
}

// TestParseActionBlocksRealStreamFixture replays the assistant text captured
// from request 8915bd04 in the live evidence ring (SSE deltas joined): two
// <tool_call> wrappers carrying read/grep JSON bodies in the client's own
// spelling — numeric offset/limit, a regex pattern with \S — each followed by
// fence and </result> close junk, with </think> junk between the two calls.
// The request body was truncated in the ring, so the tool schemas here mirror
// the payload shape instead of guessing Read/file_path.
func TestParseActionBlocksRealStreamFixture(t *testing.T) {
	fixtureTools := []ToolDef{
		xmlSchemaTool("read", map[string]any{
			"path":   map[string]any{"type": "string"},
			"offset": map[string]any{"type": "integer"},
			"limit":  map[string]any{"type": "integer"},
		}, "path"),
		xmlSchemaTool("grep", map[string]any{
			"path":        map[string]any{"type": "string"},
			"pattern":     map[string]any{"type": "string"},
			"output_mode": map[string]any{"type": "string"},
			"context":     map[string]any{"type": "integer"},
			"limit":       map[string]any{"type": "integer"},
		}, "path", "pattern"),
	}
	text := xOpen + "\n" +
		`{"tool":"read","parameters":{"path":"C:\\Users\\datoo\\AppData\\Roaming\\dsh-desktop-dev\\logs\\harness.log","offset":1590,"limit":90}}` + "\n" +
		"```\n</" + "result" + ">\n```\n</" + "think" + ">\n\n" +
		xOpen + "\n" +
		`{"tool":"grep","parameters":{"path":"C:\\Users\\datoo\\AppData\\Roaming\\dsh-desktop-dev\\logs\\harness.log","pattern":"ERR_PNPM|Cannot find|No matching version|peer dep|ECONNREFUSED|ETIMEDOUT|ENOTFOUND|self-signed|certificate|lockfile|resolved \\S+ with|reify|Progress: resolved","output_mode":"content","context":0,"limit":60}}` + "\n" +
		"```\n</" + "result" + ">"

	calls, clean, err := ParseActionBlocks(text, fixtureTools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("fixture must yield the read and the grep call, got %+v", calls)
	}
	if calls[0].Name != "read" || calls[0].Arguments["path"] != `C:\Users\datoo\AppData\Roaming\dsh-desktop-dev\logs\harness.log` {
		t.Fatalf("read call = %+v", calls[0])
	}
	if calls[0].Arguments["offset"] != float64(1590) || calls[0].Arguments["limit"] != float64(90) {
		t.Fatalf("read numeric args = %+v", calls[0].Arguments)
	}
	if calls[1].Name != "grep" || calls[1].Arguments["output_mode"] != "content" {
		t.Fatalf("grep call = %+v", calls[1])
	}
	if pattern, _ := calls[1].Arguments["pattern"].(string); !strings.Contains(pattern, `resolved \S+ with`) {
		t.Fatalf("grep pattern lost its regex backslash: %q", pattern)
	}
	if clean != "" {
		t.Fatalf("close junk must be consumed, clean = %q", clean)
	}

	// The same fixture under every two-part split and byte-by-byte: close junk
	// on either side of any split must not leak, and no prose may be lost.
	streamFixture := func(name string, feed []string) {
		scanner := NewActionBlockScanner(fixtureTools)
		pending := ""
		var out strings.Builder
		for _, delta := range feed {
			pending += delta
		loop:
			for {
				start, end, unterminated := scanner.FindSpan(pending)
				switch {
				case unterminated:
					if start > 0 {
						out.WriteString(pending[:start])
						scanner.Discard(start)
						pending = pending[start:]
					}
					break loop
				case end > 0:
					if start > 0 {
						out.WriteString(pending[:start])
					}
					scanner.Discard(end)
					pending = pending[end:]
				default:
					safe := len(pending) - ActionOpenPrefixHold(pending)
					if safe > 0 {
						out.WriteString(pending[:safe])
						scanner.Discard(safe)
						pending = pending[safe:]
					}
					break loop
				}
			}
		}
		_, flushKeeps, err := ParseActionBlocks(pending, fixtureTools, Config{})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := strings.TrimSpace(out.String() + flushKeeps); got != "" {
			t.Fatalf("%s: close junk leaked or prose lost, got %q", name, got)
		}
	}
	for i := 1; i < len(text); i++ {
		streamFixture(fmt.Sprintf("split %d", i), []string{text[:i], text[i:]})
	}
	bytewise := make([]string, 0, len(text))
	for i := 0; i < len(text); i++ {
		bytewise = append(bytewise, string(text[i]))
	}
	streamFixture("bytewise", bytewise)
}

// TestHybridBoundariesStreamConsistency replays every boundary fixture through
// the streaming scanner under every two-part split (and byte-by-byte) and
// requires the emitted text plus the flush remainder to equal the one-shot
// clean text exactly — no squashSpaces to hide lost prose.
func TestHybridBoundariesStreamConsistency(t *testing.T) {
	tools := xmlTools()
	for _, tc := range hybridCases() {
		splits := [][]string{}
		for i := 1; i < len(tc.text); i++ {
			splits = append(splits, []string{tc.text[:i], tc.text[i:]})
		}
		bytewise := make([]string, 0, len(tc.text))
		for i := 0; i < len(tc.text); i++ {
			bytewise = append(bytewise, string(tc.text[i]))
		}
		splits = append(splits, bytewise)

		for si, feed := range splits {
			scanner := NewActionBlockScanner(tools)
			pending := ""
			var out strings.Builder
			for _, delta := range feed {
				pending += delta
			loop:
				for {
					start, end, unterminated := scanner.FindSpan(pending)
					switch {
					case unterminated:
						if start > 0 {
							out.WriteString(pending[:start])
							scanner.Discard(start)
							pending = pending[start:]
						}
						break loop
					case end > 0:
						if start > 0 {
							out.WriteString(pending[:start])
						}
						scanner.Discard(end)
						pending = pending[end:]
					default:
						safe := len(pending) - ActionOpenPrefixHold(pending)
						if safe > 0 {
							out.WriteString(pending[:safe])
							scanner.Discard(safe)
							pending = pending[safe:]
						}
						break loop
					}
				}
			}
			_, flushKeeps, err := ParseActionBlocks(pending, tools, Config{})
			if err != nil {
				t.Fatalf("%s split %d: %v", tc.name, si, err)
			}
			// Trim only the edges: ParseActionBlocks trims its clean output,
			// while the streamed composition may keep a trailing newline.
			// Whitespace inside the text is compared exactly so lost prose
			// cannot hide behind normalization.
			if got := strings.TrimSpace(out.String() + flushKeeps); got != tc.wantClean {
				t.Fatalf("%s split %d: streamed+flush = %q, want %q", tc.name, si, got, tc.wantClean)
			}
		}
	}
}

// --- 931 final-review red points --------------------------------------------
//
// The two gaps the final acceptance probe added after the original twelve
// checks: an unfinished parent structure must shield every opening inside it
// (a fenced example quoted in an unterminated parameter is payload, never an
// independent call), and the span an accepted XML block consumes must come
// from the block scanner — re-searching the first block close leaks the block
// tail into clean text when a parameter value carries that close literally.

func TestParseActionBlocksUnfinishedParentShieldsInnerOpenings(t *testing.T) {
	tools := xmlTools()
	const inner = "```json action\n" + `{"tool":"Bash","parameters":{"command":"echo hi"}}`
	missingClose := xOpen + "\n" + xFuncOpen + "Bash>\n" + xParamOpen + "command>Use this shape: " + inner + "\n" + xParamEnd + "\n" + xFuncClose
	openParam := xOpen + "\n" + xFuncOpen + "Bash>\n" + xParamOpen + "command>Use this shape: " + inner
	unknown := strings.Replace(missingClose, xFuncOpen+"Bash>", xFuncOpen+"NotATool>", 1)
	cases := []struct{ name, text string }{
		{"missing block close", missingClose},
		{"parameter never closed", openParam},
		{"unknown parent, missing block close", unknown},
	}
	for _, tc := range cases {
		calls, clean, err := ParseActionBlocks(tc.text, tools, Config{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(calls) != 0 {
			t.Fatalf("%s: an unfinished parent must shield its inner example, got %+v", tc.name, calls)
		}
		if clean != tc.text {
			t.Fatalf("%s: unfinished text must stay verbatim, got %q want %q", tc.name, clean, tc.text)
		}
	}

	// A decided block in front still parses; the unfinished tail survives whole.
	calls, clean, err := ParseActionBlocks(xCall("Bash", xParam("command", "ls"))+"\n"+missingClose, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Arguments["command"] != "ls" {
		t.Fatalf("front call = %+v", calls)
	}
	if clean != missingClose {
		t.Fatalf("unfinished tail = %q want %q", clean, missingClose)
	}
}

func TestParseActionBlocksXMLSpanConsumesRealBlockClose(t *testing.T) {
	tools := xmlTools()
	block1 := xOpen + "\n" + xFuncOpen + "Bash>\n" + xParamOpen + "command>echo hi && cat x" + xClose + "_tail.txt" + xParamEnd + "\n" + xFuncClose + "\n" + xClose
	block2 := xCall("Bash", xParam("command", "pwd"))

	calls, clean, err := ParseActionBlocks(block1+"\nafter", tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Arguments["command"] != "echo hi && cat x"+xClose+"_tail.txt" {
		t.Fatalf("calls = %+v", calls)
	}
	if clean != "after" {
		t.Fatalf("a literal close inside a value must not leak the block tail, clean = %q", clean)
	}

	calls, clean, err = ParseActionBlocks(block1+"\nmid\n"+block2+"\npost", tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].Arguments["command"] != "echo hi && cat x"+xClose+"_tail.txt" || calls[1].Arguments["command"] != "pwd" {
		t.Fatalf("adjacent blocks = %+v", calls)
	}
	if clean != "mid\n\npost" {
		t.Fatalf("adjacent clean = %q", clean)
	}
}

// --- 933 parser batch --------------------------------------------------------
//
// The cases below cover the gaps 933 found outside the 931 boundary fixtures:
// the compact trailing comma, a brace-less fence mid-stream, the pending
// bound, name resolution, CRLF and BOM framing, large parameters, the
// MaxScanBytes cap, and the string state machine all three readers share.

// TestParseActionBlocksRepairsTrailingCommas pins the tolerant repair for the
// comma models leave in front of a closing brace. The compact shape {"a":1,}
// was the one the old byte replacer never matched, so the block was rejected
// whole and the call reached the client as prose; the string-aware scanner
// covers it together with the newline and space shapes, without rewriting a
// comma that belongs to a quoted value.
func TestParseActionBlocksRepairsTrailingCommas(t *testing.T) {
	tools := xmlTools()
	cases := []struct {
		name, body, wantCommand string
	}{
		{"compact", `{"tool":"Bash","parameters":{"command":"ls",}}`, "ls"},
		{"compact with spaces", `{"tool":"Bash","parameters":{"command":"ls",   }}`, "ls"},
		{"newline", "{\"tool\":\"Bash\",\"parameters\":{\"command\":\"ls\",\n}}", "ls"},
		{"crlf", "{\"tool\":\"Bash\",\"parameters\":{\"command\":\"ls\",\r\n}}", "ls"},
		{"nested array", `{"tool":"Bash","parameters":{"command":"ls","tags":["a",]}}`, "ls"},
		{"top level", `{"tool":"Bash","parameters":{"command":"ls"},}`, "ls"},
		// The body below only parses after the repair, so the scanner has to run
		// while it is string-aware: the ", }" inside the value is payload and
		// must survive, or the argument is silently mangled into a body that no
		// longer parses at all.
		{"comma inside a value survives the repair",
			"{\"tool\":\"Bash\",\"parameters\":{\"command\":\"echo a, } b\",}}", "echo a, } b"},
		{"closing bracket inside a value survives the repair",
			"{\"tool\":\"Bash\",\"parameters\":{\"command\":\"sed -n 1,2]p f\",}}", "sed -n 1,2]p f"},
	}
	for _, tc := range cases {
		text := xOpen + "\n" + tc.body + "\n" + xClose
		calls, _, err := ParseActionBlocks(text, tools, Config{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(calls) != 1 {
			t.Fatalf("%s: calls = %+v, want one", tc.name, calls)
		}
		if got := calls[0].Arguments["command"]; got != tc.wantCommand {
			t.Fatalf("%s: command = %#v, want %#v", tc.name, got, tc.wantCommand)
		}
	}
}

// TestNormalizeToolNameRefusesFuzzyRedirect pins 933 P3-T1: a name the model
// invented is not the parser's to reinterpret. A version suffix is the same
// name; anything else that merely contains a declared name is a different tool,
// and the namespace side already refuses to guess between two declarations.
func TestNormalizeToolNameRefusesFuzzyRedirect(t *testing.T) {
	names, _ := toolLookupMaps([]ToolDef{{Name: "web_search"}, {Name: "read_file"}})
	cases := []struct{ raw, want string }{
		{"web_search", "web_search"},
		{"web_search_v2", "web_search"},   // version suffix: the same tool
		{"v2_web_search", "web_search"},   // version prefix: the same tool
		{"mcp__web_search", "web_search"}, // the mcp_ prefix layer still applies
		{"WEB-SEARCH", "web_search"},      // separator normalisation
		// A delimiter on both sides is the bar: without one, a longer name that
		// merely contains a declared one is a different tool, and the model
		// asked for something the client never declared.
		{"web_searcher", "web_searcher"},
		{"mywebsearchtool", "mywebsearchtool"},
		{"readfiles", "readfiles"},
	}
	for _, tc := range cases {
		if got := normalizeToolName(tc.raw, names); got != tc.want {
			t.Errorf("normalizeToolName(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	// Two declared tools that both claim the invented name is the ambiguity the
	// namespace side rejects; there is no safe winner here either.
	ambiguous, _ := toolLookupMaps([]ToolDef{{Name: "read_file"}, {Name: "patch"}})
	if got := normalizeToolName("read_file_patch", ambiguous); got != "read_file_patch" {
		t.Errorf("normalizeToolName on an ambiguous match = %q, want it left verbatim", got)
	}

	// End to end: a name that only contains a declared tool produces no call.
	text := "```json action\n" + `{"tool":"web_searcher","parameters":{"command":"ls"}}` + "\n```"
	calls, clean, err := ParseActionBlocks(text, []ToolDef{{Name: "web_search"}}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("an invented name must not be redirected: %+v", calls)
	}
	if !strings.Contains(clean, "web_searcher") {
		t.Fatalf("the rejected block must stay verbatim, clean = %q", clean)
	}
}

// streamFeed replays a delta list the way toolStreamFilter.Push does, one delta
// at a time, and returns the text the client would have seen plus the buffer
// still withheld.
func streamFeed(tools []ToolDef, deltas []string) (emitted, pending string) {
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
				if start > 0 {
					out.WriteString(pending[:start])
				}
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
	return out.String(), pending
}

func splitEvery(text string, n int) []string {
	out := make([]string, 0, len(text)/n+1)
	for i := 0; i < len(text); i += n {
		end := i + n
		if end > len(text) {
			end = len(text)
		}
		out = append(out, text[i:end])
	}
	return out
}

// TestActionBlockScannerHoldsBraceLessFenceUntilFlush pins 933 P3-T8, the cost
// side of the hybrid dialect: a fenced sample whose body never holds a brace is
// textually identical to a "marker before body" block whose body is still
// coming, so the scanner cannot tell them apart and holds. Everything from the
// fence on reaches the client at Flush instead of at the fence. 933 declined the
// heuristic window that would fix it, because the pinned marker-before-body
// fixture carries only eleven bytes between its fence and its body — any window
// small enough to release this case would cut a real body off. The pending bound
// is the only release, and only for a block of pathological size.
func TestActionBlockScannerHoldsBraceLessFenceUntilFlush(t *testing.T) {
	text := "前面。\n```json\n[1, 2]\n```\n后面还有很长的一段正文。"
	emitted, pending := streamFeed(xmlTools(), []string{text})
	if strings.Contains(emitted, "后面还有很长的一段正文") {
		t.Fatalf("a brace-less fence must still hold mid-stream, emitted = %q", emitted)
	}
	if !strings.Contains(pending, "后面还有很长的一段正文") {
		t.Fatalf("the tail must be withheld until Flush, pending = %q", pending)
	}
	// At Flush the final-text parser reads it as prose, and the turn ends intact.
	_, clean, err := ParseActionBlocks(pending, xmlTools(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(clean, "后面还有很长的一段正文") || !strings.Contains(clean, "[1, 2]") {
		t.Fatalf("Flush must return the tail as prose, clean = %q", clean)
	}
}

// TestActionBlockScannerReleasesOversizedPendingBlock pins the pending bound
// added with 933 P2-T2: an opening that holds more than any tool call carries
// and still cannot be decided is prose, and the streamer gives it back instead of
// buffering — and re-scanning — a growing buffer for the rest of the turn.
func TestActionBlockScannerReleasesOversizedPendingBlock(t *testing.T) {
	old := maxPendingBlockBytes
	maxPendingBlockBytes = 4 << 10
	t.Cleanup(func() { maxPendingBlockBytes = old })

	// A fence whose body never completes: nothing can decide it, so only the
	// bound releases it, and only once a later delta pushes the held text past
	// it — the check runs when a block is already pending.
	filler := strings.Repeat("x", 8<<10)
	text := "head\n```json action\n" + filler
	emitted, pending := streamFeed(xmlTools(), splitEvery(text, 1024))
	if !strings.Contains(emitted, filler) {
		t.Fatalf("an oversized pending block must be released as prose, emitted = %q", emitted)
	}
	if strings.Contains(pending, filler) {
		t.Fatalf("the released text must not stay withheld, pending = %q", pending)
	}

	// Under the bound the same shape is still held: a large but decodable body
	// must reach the client as a call, not as prose.
	bigValue := strings.Repeat("y", 3000)
	call := "```json action\n" + `{"tool":"Bash","parameters":{"command":"echo ` + bigValue + `"}}` + "\n```"
	emitted, pending = streamFeed(xmlTools(), []string{"head\n" + call + "\ntail"})
	if strings.Contains(emitted, bigValue) {
		t.Fatalf("a call under the bound must still be consumed, emitted = %q", emitted)
	}
	_, flushKeeps, err := ParseActionBlocks(pending, xmlTools(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(emitted + flushKeeps); got != "head\ntail" {
		t.Fatalf("streamed+flush = %q, want %q", got, "head\ntail")
	}
}

// TestParseActionBlocksHandlesCRLFAndBOMFrames pins the framing the fixtures
// never covered: the opening needle carries a CRLF spelling of its own, and a
// BOM in front of a fence must not hide the block behind it.
func TestParseActionBlocksHandlesCRLFAndBOMFrames(t *testing.T) {
	tools := xmlTools()
	body := `{"tool":"Bash","parameters":{"command":"ls"}}`
	cases := []struct{ name, text, wantClean string }{
		{"crlf labelled fence", "```json action\r\n" + body + "\r\n```\r\ntail", "tail"},
		{"crlf bare fence", "```json\r\n" + body + "\r\n```\r\ntail", "tail"},
		{"bom before a fence", "\uFEFF```json action\n" + body + "\n```\ntail", "\uFEFFtail"},
		{"bom before an xml block", "\uFEFF" + xOpen + "\n" + body + "\n" + xClose + "\ntail", "\uFEFFtail"},
	}
	for _, tc := range cases {
		calls, clean, err := ParseActionBlocks(tc.text, tools, Config{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if len(calls) != 1 || calls[0].Arguments["command"] != "ls" {
			t.Fatalf("%s: calls = %+v, want the fenced call", tc.name, calls)
		}
		if clean != tc.wantClean {
			t.Fatalf("%s: clean = %q, want %q", tc.name, clean, tc.wantClean)
		}
		// The same framing through the streaming path, byte by byte.
		emitted, pending := streamFeed(tools, splitEvery(tc.text, 1))
		_, flushKeeps, err := ParseActionBlocks(pending, tools, Config{})
		if err != nil {
			t.Fatalf("%s flush: %v", tc.name, err)
		}
		if got := strings.TrimSpace(emitted + flushKeeps); got != strings.TrimSpace(tc.wantClean) {
			t.Fatalf("%s: streamed+flush = %q, want %q", tc.name, got, tc.wantClean)
		}
	}
}

// TestParseActionBlocksHandlesLargeParameters pins the scale the parser has to
// survive: a value far past any normal argument, in one line, on both paths.
func TestParseActionBlocksHandlesLargeParameters(t *testing.T) {
	tools := xmlTools()
	value := strings.Repeat("abcdefghij", 20<<10) // ~200 KB
	text := "```json action\n" + `{"tool":"Bash","parameters":{"command":"echo ` + value + `"}}` + "\n```\ntail"

	calls, clean, err := ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("call count = %d", len(calls))
	}
	if got, _ := calls[0].Arguments["command"].(string); got != "echo "+value {
		t.Fatalf("a %d byte argument must survive byte for byte (got %d bytes)", len(value), len(got))
	}
	if clean != "tail" {
		t.Fatalf("clean = %q", clean)
	}

	emitted, pending := streamFeed(tools, splitEvery(text, 4096))
	if strings.Contains(emitted, value) {
		t.Fatal("the streamed call must be consumed, not emitted as prose")
	}
	_, flushKeeps, err := ParseActionBlocks(pending, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(emitted+flushKeeps) != "tail" {
		t.Fatalf("streamed+flush = %q", emitted+flushKeeps)
	}
}

// TestMaxScanBytesCapsTheOneShotParse pins the ceiling every production caller
// leaves at zero, and that the environment reaches it.
func TestMaxScanBytesCapsTheOneShotParse(t *testing.T) {
	tools := xmlTools()
	first := "```json action\n" + `{"tool":"Bash","parameters":{"command":"ls"}}` + "\n```\n"
	second := "```json action\n" + `{"tool":"Bash","parameters":{"command":"pwd"}}` + "\n```"
	text := first + "FILLER" + second

	if got := effectiveMaxScanBytes(Config{}); got != defaultMaxScanBytes {
		t.Fatalf("default scan ceiling = %d, want %d", got, defaultMaxScanBytes)
	}
	calls, _, err := ParseActionBlocks(text, tools, Config{MaxScanBytes: len(first)})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Arguments["command"] != "ls" {
		t.Fatalf("the block inside the cap must still parse, calls = %+v", calls)
	}

	t.Setenv(maxScanBytesEnv, "16")
	if got := effectiveMaxScanBytes(Config{}); got != 16 {
		t.Fatalf("%s must be readable, got %d", maxScanBytesEnv, got)
	}
	calls, _, err = ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("a 16 byte ceiling leaves no room for a block, calls = %+v", calls)
	}
	t.Setenv(maxScanBytesEnv, "-4")
	if got := effectiveMaxScanBytes(Config{}); got != defaultMaxScanBytes {
		t.Fatalf("a negative %s must fall back to %d, got %d", maxScanBytesEnv, defaultMaxScanBytes, got)
	}
}

// TestJSONStringStateAgreesAcrossReaders pins 933 P3-T7: the body balancer, the
// closing-fence scan and the repairs walk one state machine, so an escaped quote
// or a fence inside a value cannot be read as structure by one of them and not
// the others. This is the one case the old hand-rolled copies could disagree on.
func TestJSONStringStateAgreesAcrossReaders(t *testing.T) {
	tools := xmlTools()
	// The value quotes a backslash, a quote and a code fence: all three must
	// stay payload for the balancer, the fence scan and the repairs alike.
	body := "{\"tool\":\"Bash\",\"parameters\":{\"command\":\"echo \\\"a\\\\b\\\" and ``` done\"}}"
	text := "```json action\n" + body + "\n```"

	if end := jsonBodyEnd(text, fenceBodyStart(text, 0)); end <= 0 || text[end-1] != '}' {
		t.Fatalf("jsonBodyEnd = %d, want the end of the body object", end)
	}
	if closing := findClosingFence(text, fenceBodyStart(text, 0)); closing < 0 {
		t.Fatal("findClosingFence must find the closing fence past a quoted one")
	}
	calls, clean, err := ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want the value read as one payload", calls)
	}
	if !strings.Contains(calls[0].Arguments["command"].(string), "done") {
		t.Fatalf("command = %#v", calls[0].Arguments["command"])
	}
	if clean != "" {
		t.Fatalf("clean = %q, want the whole fence consumed", clean)
	}

	// The machine's own transitions: an escape absorbs exactly one byte, so a
	// quote behind a backslash neither opens nor closes a string.
	var str jsonStringState
	for _, tc := range []struct {
		input    string
		inString bool
	}{
		{`"a\"b"`, false},       // the escaped quote is payload, the last one closes
		{`"a\\"`, false},        // the escaped backslash is payload, the last one closes
		{`"a`, true},            // still open
		{`\"`, true},            // outside a string a quote opens one
		{`{`, false},            // structure is not a string
		{`{"a": "b\}"}`, false}, // the escaped brace stayed inside the value
	} {
		str = jsonStringState{}
		for i := 0; i < len(tc.input); i++ {
			str.step(tc.input[i])
		}
		if str.literal() != tc.inString {
			t.Errorf("after %q the machine reports inString=%v, want %v", tc.input, str.literal(), tc.inString)
		}
	}
}

// FuzzActionBlockScannerMatchesOneShotSpan turns the scanner-versus-one-shot
// agreement into a property instead of a fixture list: for any text the
// incremental verdict must equal the stateless one. Seeds carry the 931 boundary
// fixtures, so the corpus starts where the hand-written cases were.
//
// The property is the verdict, not the emitted bytes. A block whose structure
// completes mid-stream can be consumed by the stream and then rejected by the
// final parse of the same text — an XML body that reads as a JSON tool call
// while its tag shape is still open, but as a tag block once the close arrives
// — so "streamed text plus Flush equals the one-shot clean" is not an invariant
// of arbitrary input, only of the fixtures
// (TestHybridBoundariesStreamConsistency). Asserting it here would report a
// 931-era asymmetry as a fuzz failure instead of as the bug it is; asserting the
// verdict keeps the guarantee the parser documents about itself.
func FuzzActionBlockScannerMatchesOneShotSpan(f *testing.F) {
	tools := xmlTools()
	for _, tc := range hybridCases() {
		f.Add(tc.text)
	}
	for _, seed := range []string{
		"",
		"prose only, no fence",
		"```json action\n" + `{"tool":"Bash","parameters":{"command":"ls"}}` + "\n```",
		"```json action\n" + `{"tool":"Bash","parameters":{"command":"ls",}}` + "\n```",
		xOpen + "\n" + `{"note":"x"}` + "\n" + `{"tool":"Bash","parameters":{"command":"ls"}}` + "\n" + xClose,
		"```json\r\n[1,2]\r\n```\r\nprose\r\n",
		"\uFEFF```json\n[1,2]\n```\n" + xOpen + "\n" + `{"tool":"Bash","parameters":{"command":"pwd"}}` + "\n" + xClose,
		"```json action\n" + "{\"tool\":\"Bash\",\"parameters\":{\"command\":\"echo \\q }} ```\"}}" + "\n```",
		// Two regressions this fuzzer found: a fragment object in front of the
		// call must not be decided mid-stream, and a body whose close syntax
		// has not arrived must not be settled.
		"```json\n{}0{\"tool\":\"BAsh\",\"command\":\"0\"}",
		xOpen + `<function={"tool":"BAsh","command":"0"}>` + xClose,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > 4096 {
			t.Skip()
		}
		oneStart, oneEnd, onePending := FindActionBlockSpan(text, tools)
		scanner := NewActionBlockScanner(tools)
		gotStart, gotEnd, gotPending := scanner.FindSpan(text)
		if gotStart != oneStart || gotEnd != oneEnd || gotPending != onePending {
			t.Fatalf("scanner=(%d,%d,%v) one-shot=(%d,%d,%v) for %q",
				gotStart, gotEnd, gotPending, oneStart, oneEnd, onePending, text)
		}
	})
}

// --- benchmarks --------------------------------------------------------------
//
// 933 P2-T2 asked for a baseline rather than a guess at the streaming cost, so
// the two hot shapes are measured: a body still open (the path that re-scanned
// the whole withheld buffer per delta) and a complete one-shot parse. Sizes are
// fixed so a later change can be compared against the same numbers.

const benchBodyBytes = 32 << 10

func BenchmarkActionBlockScannerPendingFence(b *testing.B) {
	tools := xmlTools()
	deltas := splitEvery("```json action\n"+strings.Repeat("a", benchBodyBytes), 256)
	b.SetBytes(int64(benchBodyBytes))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scanner := NewActionBlockScanner(tools)
		pending := ""
		for _, delta := range deltas {
			pending += delta
			if _, _, unterminated := scanner.FindSpan(pending); unterminated {
				continue
			}
			break
		}
	}
}

func BenchmarkParseActionBlocksHybrid(b *testing.B) {
	tools := xmlTools()
	body := strings.Repeat("a", benchBodyBytes)
	text := "lead\n```json action\n" + `{"tool":"Bash","parameters":{"command":"echo ` + body + `"}}` + "\n```\ntail"
	b.SetBytes(int64(len(text)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, _, err := ParseActionBlocks(text, tools, Config{}); err != nil {
			b.Fatal(err)
		}
	}
}
