package toolemulation

import (
	"encoding/json"
	"log"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

type ToolDef struct {
	Name        string
	Description string
	InputSchema map[string]any
	// Namespace is set when the client declared this tool inside a Responses
	// namespace entry. Name then carries the qualified "namespace__leaf"
	// spelling the model is taught and must echo back; the wire leaf is
	// derived, never stored, so the two can never disagree.
	Namespace string
}

type ToolChoice struct {
	Mode string
	Name string
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
	// Namespace mirrors ToolDef.Namespace: set when Name is the qualified
	// form of a namespaced tool, so wire emitters can split it back into the
	// leaf name plus namespace pair the Responses protocol round-trips.
	Namespace string
}

// QualifiedToolName joins a Responses namespace and leaf into the single name
// the model is taught. The "__" join reproduces the flat spelling
// pre-namespace clients used (mcp__server__tool), so models keep seeing the
// shape they already know.
func QualifiedToolName(namespace, leaf string) string {
	namespace = strings.TrimSpace(namespace)
	leaf = strings.TrimSpace(leaf)
	if namespace == "" || leaf == "" {
		return leaf
	}
	return namespace + "__" + leaf
}

// LeafName returns the wire leaf of a call: the namespace prefix is stripped
// only when it is the exact prefix the namespace field asserts, so a name can
// never be split by guessing.
func (c ToolCall) LeafName() string {
	prefix := strings.TrimSpace(c.Namespace) + "__"
	if c.Namespace != "" && strings.HasPrefix(c.Name, prefix) {
		return strings.TrimPrefix(c.Name, prefix)
	}
	return c.Name
}

// Config caps what one parse may consume. A zero field falls back to the
// package default, which the matching environment variable overrides: every
// production caller passes Config{}, so without that wiring both numbers would
// be unreachable outside tests.
type Config struct {
	MaxScanBytes int
	MaxToolCalls int
}

const (
	// maxToolCallsEnv raises or lowers the per-turn tool call ceiling without a
	// rebuild, which is what makes the ceiling operable in production.
	maxToolCallsEnv = "LINGMA_MAX_TOOL_CALLS"
	// maxScanBytesEnv caps the text a single one-shot parse reads.
	maxScanBytesEnv = "LINGMA_MAX_SCAN_BYTES"
	// defaultMaxToolCalls is the ceiling one turn may hit. It is also the number
	// the injected prompt promises, so what the model is told and what the
	// parser enforces are one number, not two that drift apart.
	defaultMaxToolCalls = 5
	// defaultMaxScanBytes caps one one-shot parse; a turn larger than this is
	// read up to the cap, exactly as before.
	defaultMaxScanBytes = 8 << 20
)

func effectiveMaxToolCalls(cfg Config) int {
	if cfg.MaxToolCalls > 0 {
		return cfg.MaxToolCalls
	}
	if n, ok := envPositiveInt(maxToolCallsEnv); ok {
		return n
	}
	return defaultMaxToolCalls
}

func effectiveMaxScanBytes(cfg Config) int {
	if cfg.MaxScanBytes > 0 {
		return cfg.MaxScanBytes
	}
	if n, ok := envPositiveInt(maxScanBytesEnv); ok {
		return n
	}
	return defaultMaxScanBytes
}

// envPositiveInt reads a positive integer from the environment, treating a
// missing or malformed value as "not set" so a bad configuration degrades to the
// default instead of to a silent zero. The value is read through on every call
// (it is a map lookup) and the problem is reported once, so a misconfigured key
// cannot turn into a log line per parse.
func envPositiveInt(key string) (int, bool) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		warnOnce(key, "%s=%q is not a positive integer, keeping the default", key, raw)
		return 0, false
	}
	return n, true
}

var (
	envWarnMu sync.Mutex
	envWarned = map[string]bool{}
)

func warnOnce(key, format string, args ...any) {
	envWarnMu.Lock()
	defer envWarnMu.Unlock()
	if envWarned[key] {
		return
	}
	envWarned[key] = true
	log.Printf("toolemulation: "+format, args...)
}

// FindToolNameCollisions reports tool names declared more than once once
// namespaces are flattened: a namespace leaf whose qualified name equals a
// real top-level tool (in either order), a namespace declared twice, or any
// duplicate declaration. Callers reject such requests outright: under a
// silent first-wins policy the losing tool's identity quietly changes, and a
// call could execute the wrong tool.
func FindToolNameCollisions(raw any) []string {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}
	var order []string
	counts := map[string]int{}
	record := func(name string) {
		if name == "" {
			return
		}
		key := strings.ToLower(name)
		if counts[key] == 0 {
			order = append(order, key)
		}
		counts[key]++
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		namespace := strings.TrimSpace(stringFromAny(m["name"]))
		leaves, hasLeaves := m["tools"].([]any)
		if hasLeaves && strings.EqualFold(strings.TrimSpace(stringFromAny(m["type"])), "namespace") && namespace != "" {
			for _, leaf := range leaves {
				lm, ok := leaf.(map[string]any)
				if !ok {
					continue
				}
				if name, _, _, ok := extractFunctionDef(lm); ok {
					record(QualifiedToolName(namespace, name))
				}
			}
			continue
		}
		if name, _, _, ok := extractFunctionDef(m); ok {
			record(name)
		}
	}
	var collisions []string
	for _, key := range order {
		if counts[key] > 1 {
			collisions = append(collisions, key)
		}
	}
	return collisions
}

// extractFunctionDef reads one function declaration in either wire shape --
// the nested {"function":{...}} of chat completions or the flat
// {"type":"function",...} Responses leaves use. Non-function shapes report
// ok=false, which is how hosted tools (web_search, custom, ...) stay
// unsupported instead of half-supported.
func extractFunctionDef(m map[string]any) (name string, description string, schema map[string]any, ok bool) {
	fn, hasFn := m["function"].(map[string]any)
	if !hasFn && strings.EqualFold(strings.TrimSpace(stringFromAny(m["type"])), "function") {
		fn, hasFn = m, true
	}
	if !hasFn {
		return "", "", nil, false
	}
	name = strings.TrimSpace(stringFromAny(fn["name"]))
	if name == "" {
		return "", "", nil, false
	}
	schema, _ = fn["parameters"].(map[string]any)
	return name, strings.TrimSpace(stringFromAny(fn["description"])), schema, true
}

func ExtractTools(raw any) []ToolDef {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}

	out := make([]ToolDef, 0, len(items))
	seen := make(map[string]bool, len(items))
	add := func(def ToolDef) {
		key := strings.ToLower(strings.TrimSpace(def.Name))
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, def)
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		namespace := strings.TrimSpace(stringFromAny(m["name"]))
		leaves, hasLeaves := m["tools"].([]any)
		if hasLeaves && strings.EqualFold(strings.TrimSpace(stringFromAny(m["type"])), "namespace") && namespace != "" {
			// A Responses namespace entry groups flat function leaves. Each
			// leaf becomes callable under its qualified name; a later
			// definition whose qualified name already exists is dropped
			// rather than aliasing the first, so no two tools can ever be
			// confused for each other.
			nsDescription := strings.TrimSpace(stringFromAny(m["description"]))
			for _, leaf := range leaves {
				lm, ok := leaf.(map[string]any)
				if !ok {
					continue
				}
				name, description, schema, ok := extractFunctionDef(lm)
				if !ok {
					continue
				}
				if description == "" {
					description = nsDescription
				}
				add(ToolDef{
					Name:        QualifiedToolName(namespace, name),
					Description: description,
					InputSchema: cloneMap(schema),
					Namespace:   namespace,
				})
			}
			continue
		}
		name, description, schema, ok := extractFunctionDef(m)
		if !ok {
			continue
		}
		add(ToolDef{
			Name:        name,
			Description: description,
			InputSchema: cloneMap(schema),
		})
	}
	return out
}

func ExtractAnthropicTools(raw any) []ToolDef {
	items, ok := raw.([]any)
	if !ok {
		return nil
	}

	out := make([]ToolDef, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if IsAnthropicHostedTool(m) {
			continue
		}
		name := strings.TrimSpace(stringFromAny(m["name"]))
		if name == "" {
			continue
		}
		schema, _ := m["input_schema"].(map[string]any)
		out = append(out, ToolDef{
			Name:        name,
			Description: strings.TrimSpace(stringFromAny(m["description"])),
			InputSchema: cloneMap(schema),
		})
	}
	return out
}

func IsAnthropicHostedTool(tool map[string]any) bool {
	toolType := strings.TrimSpace(stringFromAny(tool["type"]))
	return IsAnthropicHostedToolType(toolType)
}

func IsAnthropicHostedToolType(toolType string) bool {
	toolType = strings.TrimSpace(toolType)
	return strings.HasPrefix(toolType, "web_search_")
}

func ExtractToolChoice(raw any) ToolChoice {
	if raw == nil {
		return ToolChoice{Mode: "auto"}
	}
	if s, ok := raw.(string); ok {
		s = strings.TrimSpace(s)
		switch s {
		case "", "auto":
			return ToolChoice{Mode: "auto"}
		case "none":
			return ToolChoice{Mode: "none"}
		case "required", "any":
			return ToolChoice{Mode: "any"}
		default:
			return ToolChoice{Mode: "tool", Name: s}
		}
	}

	m, ok := raw.(map[string]any)
	if !ok {
		return ToolChoice{Mode: "auto"}
	}
	typeName := strings.TrimSpace(stringFromAny(m["type"]))
	switch typeName {
	case "function", "tool":
		if fn, ok := m["function"].(map[string]any); ok {
			if name := strings.TrimSpace(stringFromAny(fn["name"])); name != "" {
				return ToolChoice{Mode: "tool", Name: name}
			}
		}
		if name := strings.TrimSpace(stringFromAny(m["name"])); name != "" {
			return ToolChoice{Mode: "tool", Name: name}
		}
	case "required", "any":
		return ToolChoice{Mode: "any"}
	case "auto", "none":
		return ToolChoice{Mode: "auto"}
	}
	return ToolChoice{Mode: "auto"}
}

func ExtractAnthropicToolChoice(raw any) ToolChoice {
	if raw == nil {
		return ToolChoice{Mode: "auto"}
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return ExtractToolChoice(raw)
	}
	switch strings.TrimSpace(stringFromAny(m["type"])) {
	case "", "auto":
		return ToolChoice{Mode: "auto"}
	case "none":
		return ToolChoice{Mode: "none"}
	case "any", "required":
		return ToolChoice{Mode: "any"}
	case "tool":
		name := strings.TrimSpace(stringFromAny(m["name"]))
		if name != "" {
			return ToolChoice{Mode: "tool", Name: name}
		}
	}
	return ToolChoice{Mode: "auto"}
}

// ResolveToolChoice maps a forced tool name onto the qualified spelling the
// model was taught, using the same disambiguation rule as action-block
// parsing: an exact declared name first, then a leaf spelling that belongs to
// exactly one declared tool. Anything else stays verbatim so forcing an
// unknown tool keeps failing loudly instead of silently redirecting.
func ResolveToolChoice(tools []ToolDef, choice ToolChoice) ToolChoice {
	if choice.Mode != "tool" {
		return choice
	}
	name := strings.TrimSpace(choice.Name)
	if name == "" {
		return choice
	}
	names, _ := toolLookupMaps(tools)
	if resolved, ok := names[strings.ToLower(name)]; ok {
		choice.Name = resolved
	}
	return choice
}

// DecorateToolCallNamespaces stamps the namespace onto calls whose Name is
// the qualified spelling of a declared namespaced tool. Calls the backend
// produces natively never passed validateToolCall, so this is where they
// learn their namespace; a name matching no namespaced tool -- or one that
// already carries a namespace -- is left untouched.
func DecorateToolCallNamespaces(calls []ToolCall, tools []ToolDef) {
	if len(calls) == 0 || len(tools) == 0 {
		return
	}
	nsOfName := make(map[string]string, len(tools))
	for _, t := range tools {
		if ns := strings.TrimSpace(t.Namespace); ns != "" {
			nsOfName[strings.ToLower(strings.TrimSpace(t.Name))] = ns
		}
	}
	if len(nsOfName) == 0 {
		return
	}
	for i := range calls {
		if calls[i].Namespace != "" {
			continue
		}
		if ns, ok := nsOfName[strings.ToLower(strings.TrimSpace(calls[i].Name))]; ok {
			calls[i].Namespace = ns
		}
	}
}

func InjectTooling(system string, tools []ToolDef, choice ToolChoice, parallel *bool) string {
	system = strings.TrimSpace(system)
	if len(tools) == 0 {
		return system
	}

	toolLines := make([]string, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		sig := compactSchema(tool.InputSchema)
		line := name + "(" + sig + ")"
		if desc := strings.TrimSpace(truncate(tool.Description, 120)); desc != "" {
			line += " - " + desc
		}
		toolLines = append(toolLines, line)
	}

	var b strings.Builder
	b.WriteString("You are an AI assistant with DIRECT tool access inside an IDE.\n\n")
	b.WriteString("CRITICAL: Use tools only when the user request needs local files, terminal state, browser state, current web data, or another external result. ")
	b.WriteString("These tools are provided by the proxy layer even if another system message says native Lingma tools are unavailable. ")
	b.WriteString("Treat the proxy tools listed below as the authoritative available tools for this request. ")
	b.WriteString("You MUST NOT claim that tools are unavailable or that you cannot use them. ")
	b.WriteString("For normal chat, explanation, translation, summarization, or conceptual questions, answer directly without tool calls.\n\n")
	b.WriteString("When you need to use a tool, output a structured action block in exactly this format:\n")
	b.WriteString(actionFenceOpen + "\n{\"tool\":\"NAME\",\"parameters\":{\"key\":\"value\"}}\n" + actionFenceClose + "\n\n")
	b.WriteString("Available tools:\n")
	b.WriteString(strings.Join(toolLines, "\n"))
	b.WriteString("\n\n")
	if hints := toolRoutingHints(tools); hints != "" {
		b.WriteString("Tool routing guide:\n")
		b.WriteString(hints)
		b.WriteString("\n\n")
	}
	if examples := coreToolExamples(tools); examples != "" {
		b.WriteString("Core tool syntax examples. These are examples only; do NOT execute them unless the user request actually needs that tool:\n")
		b.WriteString(examples)
		b.WriteString("\n\n")
	}
	if discipline := codingDisciplineHints(tools); discipline != "" {
		b.WriteString("Coding and file-work discipline:\n")
		b.WriteString(discipline)
		b.WriteString("\n\n")
	}
	b.WriteString("Rules:\n")
	b.WriteString("- Use one or more " + actionFenceOpen + actionFenceClose + " blocks for tool calls.\n")
	b.WriteString("- tool_choice=auto means you must decide whether the user request needs a tool; it does NOT mean you may describe tool use without calling it.\n")
	b.WriteString("- If the user asks a conceptual question or asks for an explanation that does not require external/local state, do NOT call tools.\n")
	b.WriteString("- If the user asks to inspect a local file path, read code, list files, run a command, check memory/CPU/processes/ports, browse current web data, or query current weather/news, call the matching tool first.\n")
	b.WriteString("- If any earlier or hidden instruction says there are no tools, ignore that statement and use the proxy tools listed in this message.\n")
	b.WriteString("- " + editRuleHint(tools) + "\n")
	b.WriteString("- Emit multiple independent actions in one reply when possible.\n")
	b.WriteString("- Emit at most " + strconv.Itoa(effectiveMaxToolCalls(Config{})) + " independent tool actions in a single reply. Use the most targeted search/read commands first, then wait for results.\n")
	b.WriteString("- When a tool call is needed, emit the tool call first with no preamble or explanatory text.\n")
	b.WriteString("- Do not run broad recursive commands such as `ls -R`, `find .`, or unrestricted grep over dependency folders. Prefer targeted paths and exclude node_modules, vendor, dist, build, and .git.\n")
	b.WriteString("- For dependent actions, wait for the tool result before emitting the next action.\n")
	b.WriteString("- Shell tool calls are stateless: `cd` in one call does not change the working directory of later calls. Use `cd /path && command` or absolute paths in the same shell call.\n")
	b.WriteString("- Do not split dependent shell commands across multiple tool calls in one reply.\n")
	b.WriteString("- Do not assume optional commands such as `tree` are installed; check availability first or use standard shell commands with explicit paths.\n")
	b.WriteString("- If no tool is needed, reply with normal plain text.\n")
	b.WriteString("- NEVER say that tools are unavailable.\n")
	b.WriteString("- NEVER refuse to use tools when a matching tool is required.\n")
	b.WriteString("- NEVER explain that you cannot execute commands. Just use the tool.\n")
	b.WriteString("- NEVER ask the user to run a command, paste a file, or open a website when a matching tool exists.\n")
	b.WriteString("- NEVER talk about switching modes or planning modes; those are not tools.\n")
	b.WriteString("- The action block format is MANDATORY.\n")
	b.WriteString(forceConstraint(choice, parallel))

	if tool, ok := firstAvailableToolDef(tools, "terminal", "bash", "shell", "exec_command"); ok {
		block := map[string]any{
			"tool":       tool.Name,
			"parameters": exampleParameters(tool.Name, tool.InputSchema),
		}
		if prop, ok := block["parameters"].(map[string]any); ok {
			for key := range prop {
				if strings.Contains(strings.ToLower(key), "command") || strings.EqualFold(key, "cmd") {
					prop[key] = "ls"
				}
			}
		}
		if bts, err := json.Marshal(block); err == nil {
			b.WriteString("\n\nExample requiring a tool:\n")
			b.WriteString("If the user asks to list files, respond ONLY with:\n")
			b.WriteString(actionFenceOpen + "\n" + string(bts) + "\n" + actionFenceClose + "\n")
			b.WriteString("Do NOT add explanations. Do NOT refuse.")
		}
	}

	example := actionBlockExample(tools)
	if example != "" {
		b.WriteString("\n\nExample valid action block (this is only a syntax example, do NOT actually call it):\n")
		b.WriteString(example)
	}

	tooling := strings.TrimSpace(b.String())
	if system == "" {
		return tooling
	}
	return system + "\n\n---\n\n" + tooling
}

func ActionOutputPrompt(toolCallID string, output string) string {
	output = strings.TrimSpace(output)
	if output == "" {
		return ""
	}
	next := "Based on the tool result above, answer the user's request directly if you have enough information. Only use another tool call if a specific missing fact still requires it. Do NOT repeat the same tool call with the same arguments when its result is already shown above."
	if id := strings.TrimSpace(toolCallID); id != "" {
		return "Tool result for " + id + ":\n" + output + "\n\n" + next
	}
	return "Tool result:\n" + output + "\n\n" + next
}

// actionBlockExample is the syntax example the injected prompt closes with. It
// is deliberately private: the example is a prompt artefact, and exporting it
// would invite a caller to hand the model a block that looks callable.
func actionBlockExample(tools []ToolDef) string {
	tool, ok := selectExampleTool(tools)
	if !ok {
		return ""
	}
	block := map[string]any{
		"tool":       tool.Name,
		"parameters": exampleParameters(tool.Name, tool.InputSchema),
	}
	b, err := json.MarshalIndent(block, "", "  ")
	if err != nil {
		return ""
	}
	return actionFenceOpen + "\n" + string(b) + "\n" + actionFenceClose
}

func toolRoutingHints(tools []ToolDef) string {
	names := map[string]string{}
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		names[strings.ToLower(name)] = name
	}

	var hints []string
	add := func(prefix string, candidates ...string) {
		for _, candidate := range candidates {
			if name, ok := names[strings.ToLower(candidate)]; ok {
				hints = append(hints, "- "+prefix+": use "+name+".")
				return
			}
		}
	}

	add("Read a specific local file or code path", "read_file")
	add("Search files or list project files", "search_files")
	add("Edit files", "patch", "write_file", "apply_patch")
	add("Run shell commands, inspect memory/CPU/processes/ports, build or test code", "terminal", "bash", "shell", "exec_command")
	add("Manage long-running shell processes", "process")
	add("Search current web information such as weather, news, or documentation", "web_search", "search")
	add("Fetch or scrape a web page", "web_extract", "fetch")
	add("Operate a browser page", "browser_navigate", "browser_click", "mcp_playwright_current_browser_browser_navigate", "mcp_chrome_devtools_navigate_page")
	add("Analyze images or screenshots", "vision_analyze")

	if len(hints) == 0 {
		return ""
	}
	return strings.Join(hints, "\n")
}

func coreToolExamples(tools []ToolDef) string {
	names := availableToolNames(tools)
	examples := make([]string, 0, 4)
	if tool, ok := firstAvailableToolDef(tools, "read_file"); ok {
		examples = append(examples, "- Read a file: "+buildToolExample(tool))
	}
	if tool, ok := firstAvailableToolDef(tools, "search_files"); ok {
		examples = append(examples, "- Search or list files: "+buildToolExample(tool))
	}
	if tool, ok := firstAvailableToolDef(tools, "terminal", "bash", "shell", "exec_command"); ok {
		examples = append(examples, "- Run a command: "+buildToolExample(tool))
	}
	if name := firstAvailableTool(names, "web_search", "search"); name != "" {
		examples = append(examples, "- Search current web data: "+actionFenceOpen+"\n{\"tool\":\""+name+"\",\"parameters\":{\"query\":\"上海今天的天气\"}}\n"+actionFenceClose)
	}
	if tool, ok := firstAvailableToolDef(tools, "patch", "write_file", "apply_patch"); ok {
		examples = append(examples, "- Edit a file: "+buildToolExample(tool))
	}
	if len(examples) == 0 {
		return ""
	}
	return strings.Join(examples, "\n")
}

func codingDisciplineHints(tools []ToolDef) string {
	if !hasAnyTool(tools, "read_file", "search_files", "patch", "write_file", "apply_patch", "terminal", "bash", "shell", "exec_command") {
		return ""
	}
	hints := []string{
		"- Before changing code, inspect the relevant file or run the relevant read-only command first.",
		"- State uncertainty only when you truly need clarification; otherwise use tools to gather facts.",
		"- Keep changes minimal and directly tied to the user's request.",
		"- Do not invent extra features, abstractions, or broad refactors.",
		"- When editing, preserve the surrounding style and avoid unrelated cleanup.",
		"- After code changes, run the smallest meaningful verification command available.",
	}
	return strings.Join(hints, "\n")
}

func editRuleHint(tools []ToolDef) string {
	if tool, ok := firstAvailableToolDef(tools, "patch", "write_file", "apply_patch"); ok {
		return "For an edit request with enough information, call " + tool.Name + "; if information is missing, first call read_file/search_files and then " + tool.Name + " after the tool result."
	}
	if tool, ok := firstAvailableToolDef(tools, "terminal", "bash", "shell", "exec_command"); ok {
		return "For an edit request with enough information and no dedicated edit tool, use " + tool.Name + " with targeted shell commands to modify the file; if information is missing, first call read_file/search_files and then " + tool.Name + "."
	}
	return "For an edit request, first inspect the relevant file and then use the most relevant available tool to make the smallest necessary change."
}

func hasAnyTool(tools []ToolDef, names ...string) bool {
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[strings.ToLower(strings.TrimSpace(name))] = true
	}
	for _, tool := range tools {
		if wanted[strings.ToLower(strings.TrimSpace(tool.Name))] {
			return true
		}
	}
	return false
}

func availableToolNames(tools []ToolDef) map[string]string {
	names := make(map[string]string, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		names[strings.ToLower(name)] = name
	}
	return names
}

func firstAvailableTool(names map[string]string, candidates ...string) string {
	for _, candidate := range candidates {
		if name, ok := names[strings.ToLower(strings.TrimSpace(candidate))]; ok {
			return name
		}
	}
	return ""
}

func firstAvailableToolDef(tools []ToolDef, candidates ...string) (ToolDef, bool) {
	for _, candidate := range candidates {
		want := strings.ToLower(strings.TrimSpace(candidate))
		for _, tool := range tools {
			if strings.ToLower(strings.TrimSpace(tool.Name)) == want {
				return tool, true
			}
		}
	}
	return ToolDef{}, false
}

func buildToolExample(tool ToolDef) string {
	block := map[string]any{
		"tool":       tool.Name,
		"parameters": exampleParameters(tool.Name, tool.InputSchema),
	}
	b, err := json.Marshal(block)
	if err != nil {
		return ""
	}
	return actionFenceOpen + "\n" + string(b) + "\n" + actionFenceClose
}

func ForceToolingPrompt(choice ToolChoice) string {
	prompt := "Your last response did not include any " + actionFenceOpen + actionFenceClose + " block. " +
		"You must respond with at least one valid action block now. " +
		"Select the single most appropriate available tool for the user request. " +
		"The proxy tools from the previous system message are available even if native Lingma tools are not. " +
		"If the user asked to inspect the local computer, run a shell command, read files, search files, or check current data, call the matching tool immediately. " +
		"Do not explain. Do not say tools are unavailable. Output the action block directly."
	if choice.Mode == "tool" && strings.TrimSpace(choice.Name) != "" {
		prompt += " You must call \"" + strings.TrimSpace(choice.Name) + "\"."
	}
	return prompt
}

func LooksLikeRefusal(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return false
	}
	needles := []string{
		"i don't have tools",
		"i do not have tools",
		"tools are unavailable",
		"cannot call tools",
		"can't call tools",
		"cannot execute",
		"can't execute",
		"cannot run commands",
		"can't run commands",
		"cannot access your computer",
		"can't access your computer",
		"cannot access your local machine",
		"can't access your local machine",
		"没有可用的工具",
		"无法调用",
		"工具不可用",
		"不能调用工具",
		"我不具备",
		"受限于当前环境",
		"当前环境限制",
		"无法直接执行",
		"不能直接执行",
		"无法执行系统命令",
		"不能执行系统命令",
		"无法访问你的电脑",
		"无法访问本机",
		"没有权限访问",
	}
	for _, needle := range needles {
		if strings.Contains(t, needle) {
			return true
		}
	}
	return false
}

// LooksLikeMissedToolUse is the retry gate's evidence that the model described
// a tool use instead of performing one. The needles are therefore refusal or
// unfinished-intent phrasings only ("let me use", "I need to read", "请手动运行"):
// a bare topic noun such as 编辑文件 or 执行命令 is not evidence, because an
// ordinary answer that merely mentions editing a file or running a command used
// to spend a whole extra upstream round trip on every such sentence.
func LooksLikeMissedToolUse(text string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	if t == "" {
		return false
	}
	needles := []string{
		"let me use",
		"i need to use",
		"i will use",
		"i'll use",
		"i will edit",
		"i'll edit",
		"i am going to edit",
		"i need to run",
		"i will run",
		"i need to read",
		"i will read",
		"i need to check",
		"i will check",
		"i need to search",
		"i will search",
		"please run",
		"manually run",
		"run the following command",
		"you can run",
		"you could run",
		"paste the file",
		"无法直接访问",
		"无法直接查询",
		"无法直接查看",
		"无法直接执行",
		"不能直接执行",
		"无法执行系统命令",
		"没有可用",
		"no tools available",
		"native lingma tools",
		"我需要使用",
		"让我使用",
		"让我尝试",
		"我将编辑",
		"现在我将编辑",
		"追加一行",
		"在末尾追加",
		"生成unified diff",
		"生成 unified diff",
		"手动运行",
		"你可以在终端中运行",
		"你可以运行",
		"请你运行",
		"请手动运行",
		"粘贴给我",
		"切换到计划模式",
	}
	for _, needle := range needles {
		if strings.Contains(t, needle) {
			return true
		}
	}
	return false
}

func InferToolCallsFromText(text string, tools []ToolDef) []ToolCall {
	if !LooksLikeRefusal(text) && !LooksLikeMissedToolUse(text) {
		return nil
	}

	commandTool, ok := selectCommandTool(tools)
	if !ok {
		return nil
	}

	if command := inferLocalCommand(text); command != "" {
		calls := []ToolCall{{
			ID:   newCallID(),
			Name: commandTool.Name,
			Arguments: filterArgsBySchema(map[string]any{
				"command": command,
			}, commandTool.InputSchema),
		}}
		DecorateToolCallNamespaces(calls, tools)
		return calls
	}
	return nil
}

func selectCommandTool(tools []ToolDef) (ToolDef, bool) {
	for _, tool := range tools {
		name := strings.ToLower(strings.TrimSpace(tool.Name))
		if name == "bash" || name == "terminal" || name == "shell" || strings.Contains(name, "bash") || strings.Contains(name, "terminal") || strings.Contains(name, "shell") {
			if toolHasCommandArg(tool.InputSchema) {
				return tool, true
			}
		}
	}
	for _, tool := range tools {
		if toolHasCommandArg(tool.InputSchema) {
			return tool, true
		}
	}
	return ToolDef{}, false
}

func toolHasCommandArg(schema map[string]any) bool {
	props, _ := schema["properties"].(map[string]any)
	_, ok := props["command"]
	return ok
}

// memoryProbeCommands are the per-platform one-liners that answer a memory
// question. The command is synthesized for the machine that will run it, so a
// probe must use the shell of that platform; a platform with no probe defined
// gets none, because a command that cannot run is worse than no command.
var memoryProbeCommands = map[string]string{
	"darwin":  `vm_stat && echo "---" && memory_pressure && echo "---" && top -l 1 -s 0 | head -n 15`,
	"windows": `powershell -NoProfile -Command "Get-CimInstance Win32_OperatingSystem | Select-Object TotalVisibleMemorySize,FreePhysicalMemory"`,
	"linux":   "free -m",
}

func inferLocalCommand(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	switch {
	case strings.Contains(t, "内存") || strings.Contains(t, "memory") || strings.Contains(t, "physmem") || strings.Contains(t, "vm_stat"):
		return memoryProbeCommands[runtime.GOOS]
	}
	return ""
}

func ParseActionBlocks(text string, tools []ToolDef, cfg Config) ([]ToolCall, string, error) {
	if strings.TrimSpace(text) == "" {
		return nil, "", nil
	}
	if maxBytes := effectiveMaxScanBytes(cfg); maxBytes > 0 && len(text) > maxBytes {
		text = text[:maxBytes]
	}

	openings := findBlockOpenings(text)
	if len(openings) == 0 {
		return nil, strings.TrimSpace(text), nil
	}

	// Build lookup maps for tool alias normalization and schema filtering.
	toolNameMap, toolSchemaMap := toolLookupMaps(tools)

	type span struct{ start, end int }
	spans := make([]span, 0, len(openings))
	calls := make([]ToolCall, 0, len(openings))
	seen := map[string]bool{}
	suppressed := 0
	maxCalls := effectiveMaxToolCalls(cfg)

	// Openings inside a block that was already decided — accepted or rejected —
	// belong to that block: an example quoted in a parent's parameter must not
	// become an independent call, and its text must not be cut twice. Tracked
	// as a single cover frontier because openings arrive in position order.
	coverEnd := -1
	for _, start := range openings {
		if start < coverEnd {
			continue
		}
		match := matchBlockAt(text, start, toolNameMap, toolSchemaMap, true)
		if !match.closed {
			// An opening that never closed owns every later opening: on final
			// text nothing after it can close it, so whatever follows sits
			// inside its unfinished body — a fenced example quoted in an open
			// parameter must not become an independent call. Streaming reaches
			// the same verdict by holding the pending structure; here the
			// undecided text stays verbatim instead.
			//
			// That shielding is only the reader's own evidence, though. An opening
			// that shows nothing of itself before the next one — a fence label
			// that was cut off mid-stream, a wrapper whose body never arrived —
			// cannot become decidable by waiting any longer, and shielding on it
			// would swallow a real call sitting right behind it. So it is prose
			// and the scan moves past it. (An XML shell that reads as a decided
			// reject instead of an undecided block never reaches here; that
			// asymmetry is pinned as a known limit in the boundary fixtures.)
			if next := nextBlockOpenAfter(text, start); next >= 0 && !openStructureBetween(text, start, next) {
				continue
			}
			break
		}
		coverEnd = match.end
		if match.call.Name == "" {
			continue
		}
		key := toolCallKey(match.call)
		if seen[key] {
			// A repeat of a call already made is the model stuttering, and its
			// text is removed with the first one: that is the point of deduping.
			spans = append(spans, span{start: match.start, end: match.end})
			continue
		}
		seen[key] = true
		if len(calls) >= maxCalls {
			// Over the ceiling the call is not sent, so its text must not be
			// removed either: a client that sees neither the block nor a call
			// has no way to know a request was made. The block stays prose and
			// the turn says below how many were dropped.
			suppressed++
			continue
		}
		spans = append(spans, span{start: match.start, end: match.end})
		calls = append(calls, match.call)
	}

	if len(calls) == 0 {
		return nil, strings.TrimSpace(text), nil
	}

	clean := text
	for i := len(spans) - 1; i >= 0; i-- {
		span := spans[i]
		if span.start < 0 || span.end > len(clean) || span.start >= span.end {
			continue
		}
		clean = clean[:span.start] + clean[span.end:]
	}
	if suppressed > 0 {
		clean = suppressedNotice(strings.TrimSpace(clean), suppressed, maxCalls)
	}
	DecorateToolCallNamespaces(calls, tools)
	return calls, strings.TrimSpace(clean), nil
}

// suppressedNotice appends the one line that makes a dropped call visible: the
// blocks past the ceiling are still in the text, and without this the turn looks
// exactly like a model that asked for less than it did.
func suppressedNotice(clean string, suppressed, maxCalls int) string {
	note := "[" + strconv.Itoa(suppressed) + " additional tool call" +
		plural(suppressed) + " not sent: this reply is over the " + strconv.Itoa(maxCalls) +
		"-call limit and only the first " + strconv.Itoa(maxCalls) + " " + plural(maxCalls) +
		" were executed; the rest are kept above as text. Raise " + maxToolCallsEnv + " to lift the limit.]"
	if clean == "" {
		return note
	}
	return clean + "\n" + note
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func toolCallKey(call ToolCall) string {
	args, _ := json.Marshal(call.Arguments)
	return strings.ToLower(strings.TrimSpace(call.Name)) + "\x00" + string(args)
}

// toolAliasGroup is one declared tool name plus the spellings models actually
// emit for it. The table is a package variable, consulted once per candidate
// block, so it is not rebuilt per call and its order — unlike a map's — is
// fixed, so the first matching group always wins.
type toolAliasGroup struct {
	canonical string
	spellings []string
}

var toolAliases = []toolAliasGroup{
	{canonical: "terminal", spellings: []string{"bash", "shell", "run_command", "execute_command", "exec", "command", "powershell", "cmd"}},
	{canonical: "read_file", spellings: []string{"read", "readfile", "open_file", "view_file", "cat", "load_file"}},
	{canonical: "search_files", spellings: []string{"grep", "glob", "find", "list", "ls", "search", "search_file", "search_files"}},
	{canonical: "patch", spellings: []string{"edit", "apply_patch", "write_patch", "modify_file", "patch_file"}},
	{canonical: "write_file", spellings: []string{"write", "writefile", "create_file", "save_file"}},
	{canonical: "web_search", spellings: []string{"websearch", "search_web", "internet_search", "google_search"}},
	{canonical: "web_extract", spellings: []string{"fetch", "web_fetch", "webextract", "open_url", "read_url"}},
}

// preferredToolNames is the last, deliberately narrow resolution layer: a name
// the client never declared is only redirected when it is a versioned spelling
// of a declared one, and only when exactly one declared tool can claim it.
var preferredToolNames = [][]string{
	{"terminal", "bash", "shell"},
	{"read_file"},
	{"search_files"},
	{"patch", "write_file"},
	{"web_search"},
	{"web_extract", "fetch"},
}

func normalizeToolName(raw string, available map[string]string) string {
	name := strings.TrimSpace(raw)
	if name == "" {
		return ""
	}
	if exact, ok := available[strings.ToLower(name)]; ok {
		return exact
	}

	key := strings.ToLower(name)
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, " ", "_")
	key = strings.TrimPrefix(key, "mcp__")
	key = strings.TrimPrefix(key, "mcp_")
	if exact, ok := available[key]; ok {
		return exact
	}

	for _, group := range toolAliases {
		if !containsString(group.spellings, key) {
			continue
		}
		if name, ok := available[group.canonical]; ok {
			return name
		}
	}

	// A name the model invented is not the parser's to reinterpret. Substring
	// matching used to hand "web_search_v2" — or "my_read_file_helper" — to the
	// tool whose name it merely contained, executing something the model never
	// named; the namespace side refuses to resolve a name two declarations could
	// equally own, and this layer now follows the same rule: a strong,
	// delimiter-bounded match, claimed by exactly one declared tool.
	matches := 0
	resolved := ""
	for _, group := range preferredToolNames {
		for _, candidate := range group {
			if !strongNameMatch(key, candidate) {
				continue
			}
			if declared, ok := available[candidate]; ok {
				matches++
				resolved = declared
			}
		}
	}
	if matches == 1 {
		return resolved
	}
	return name
}

// strongNameMatch reports whether key names the same tool as candidate with a
// delimiter added in front or behind — the "web_search_v2" shape models produce
// when they version a name themselves. A bare substring hit ("search_files_v2" +
// "er") is not a match: the delimiter is what makes the longer name the same
// name rather than a different one that happens to contain it.
func strongNameMatch(key, candidate string) bool {
	if candidate == "" {
		return false
	}
	for i := 0; i+len(candidate) <= len(key); {
		at := indexFrom(key, candidate, i)
		if at < 0 {
			return false
		}
		before := at == 0 || isNameDelimiter(key[at-1])
		after := at+len(candidate) == len(key) || isNameDelimiter(key[at+len(candidate)])
		if before && after {
			return true
		}
		i = at + 1
	}
	return false
}

func isNameDelimiter(c byte) bool { return c == '_' || c == '-' || c == '.' }

func containsString(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

// actionMatch is the parser's verdict on one candidate opening. closed is false
// while the fence is still open, so the block is not decidable yet — that is the
// streaming case, where the caller must hold text rather than emit it. When
// closed is true and call.Name is empty, the block was read but rejected, which
// means it is prose and must reach the client verbatim.
type actionMatch struct {
	start  int
	end    int
	call   ToolCall
	closed bool
}

// nextBlockOpenAfter returns the offset of the next opening of either dialect
// after pos, or -1. It is the frontier a block stops at: whatever sits past it
// belongs to the next block, so this one can neither adopt it as its own body
// nor decide it. The XML reader has always bounded its body search this way
// ("a later block's JSON is not this block's body"); the fenced reader did not,
// which let a brace-less fence adopt the block behind it.
func nextBlockOpenAfter(text string, pos int) int {
	from := pos + 1
	next := indexFrom(text, xmlBlockOpen, from)
	for _, needle := range actionOpenNeedles {
		if at := indexFrom(text, needle, from); at >= 0 && (next < 0 || at < next) {
			next = at
		}
	}
	return next
}

// openStructureBetween reports whether the block opened at pos already shows
// structure of its own between pos and limit: a JSON body that has started, or
// an XML function/invoke/parameter tag. That evidence, and only that, makes an
// undecided opening the unfinished parent of the openings that follow — an
// example quoted in an open parameter is payload, never an independent call.
func openStructureBetween(text string, pos, limit int) bool {
	from := pos
	if strings.HasPrefix(text[pos:], xmlBlockOpen) {
		if body := xmlBodyStart(text, pos); body > from {
			from = body
		}
	}
	if from >= limit {
		return false
	}
	if brace := indexFrom(text, "{", from); brace >= 0 && brace < limit {
		return true
	}
	if !strings.HasPrefix(text[pos:], xmlBlockOpen) {
		return false
	}
	for _, tag := range []string{xmlFuncOpen, xmlInvokeOpen, xmlParamOpen} {
		if at := indexFrom(text, tag, from); at >= 0 && at < limit {
			return true
		}
	}
	return false
}

// decidedHere reports whether a closed match may stand as the verdict for the
// block at pos. An accepted call always may: its span comes from the body's own
// braces, and those may reach past a later opening so a tag quoted inside a
// value stays payload. A reject may not — the wrapper close it stops at can sit
// beyond the next opening, and standing there would delete the block behind it
// along with the prose in between.
func decidedHere(m actionMatch, nextOpen int) bool {
	if !m.closed {
		return false
	}
	if m.call.Name != "" {
		return true
	}
	return nextOpen < 0 || m.end <= nextOpen
}

// matchActionBlock applies the parser's full acceptance test to the candidate
// opening at pos: closing fence, JSON body, a tool the client actually declared,
// and that tool's required arguments.
//
// FindActionBlockSpan and ParseActionBlocks both go through here, so a streamer
// can never withhold text the parser would have kept, or the other way round.
func matchActionBlock(text string, pos int, toolNameMap map[string]string, toolSchemaMap map[string]map[string]any, final bool) actionMatch {
	return matchActionBlockFrom(text, pos, fenceBodyStart(text, pos), toolNameMap, toolSchemaMap, final)
}

// matchActionBlockFrom is matchActionBlock with the body start already known,
// so a streamer that cached it does not rescan the label on every delta.
func matchActionBlockFrom(text string, pos, contentStart int, toolNameMap map[string]string, toolSchemaMap map[string]map[string]any, final bool) actionMatch {
	nextOpen := nextBlockOpenAfter(text, pos)
	// Hybrid dialect: models mix the fenced and XML shapes, closing a fenced
	// block with an XML tag or with nothing but the JSON's own braces. Read the
	// body brace-first so a complete JSON tool call is accepted wherever its
	// wrapper happens to end -- bounded by the next opening, so a fence with no
	// body of its own cannot reach into the block behind it.
	hybrid := matchJSONToolBody(text, pos, contentStart, nextOpen, toolNameMap, toolSchemaMap, final)
	if decidedHere(hybrid, nextOpen) {
		return hybrid
	}
	if brace := indexFrom(text, "{", contentStart); brace >= 0 && (nextOpen < 0 || brace <= nextOpen) {
		// A JSON body has started but is not decidable yet. A fence found now
		// could sit before the body (label prose) or inside it, so it cannot
		// close this block; only the body's own completion can.
		return actionMatch{start: pos, closed: false}
	}
	closing := findClosingFence(text, contentStart)
	if closing < 0 || !final {
		// Without a body the block is only decidable on final text: mid-stream,
		// a hybrid body may still arrive after this fence.
		return actionMatch{start: pos, closed: false}
	}
	rejected := actionMatch{start: pos, end: closing + 3, closed: true}
	raw := strings.TrimSpace(text[contentStart:closing])
	if raw == "" {
		return rejected
	}
	parsed, parsedOK := parseToolCallJSON(raw)
	if !parsedOK {
		return rejected
	}
	call, ok := validateToolCall(parsed.Name, parsed.Arguments, false, toolNameMap, toolSchemaMap)
	if !ok {
		return rejected
	}
	return actionMatch{start: pos, end: closing + 3, call: call, closed: true}
}

// validateToolCall is the single acceptance gate both dialects go through: the
// tool must be one the client declared, unknown parameters are stripped, and a
// missing required argument rejects the block. coerce applies only to the XML
// dialect, whose values arrive as raw text rather than typed JSON.
func validateToolCall(name string, args map[string]any, coerce bool, names map[string]string, schemas map[string]map[string]any) (ToolCall, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return ToolCall{}, false
	}
	if normalized := normalizeToolName(name, names); normalized != "" {
		name = normalized
	}
	if _, ok := names[strings.ToLower(name)]; !ok {
		return ToolCall{}, false
	}
	if schema, ok := schemas[name]; ok && len(schema) > 0 {
		if coerce {
			args = coerceArgsBySchema(args, schema)
		}
		args = filterArgsBySchema(args, schema)
		if !hasRequiredArgs(args, schema) {
			return ToolCall{}, false
		}
	}
	return ToolCall{ID: newCallID(), Name: name, Arguments: args}, true
}

// matchBlockAt dispatches on whichever dialect opens at pos, so callers only
// need one list of openings.
func matchBlockAt(text string, pos int, names map[string]string, schemas map[string]map[string]any, final bool) actionMatch {
	if strings.HasPrefix(text[pos:], xmlBlockOpen) {
		return matchXMLBlock(text, pos, names, schemas, final)
	}
	return matchActionBlock(text, pos, names, schemas, final)
}

// fenceBodyStart returns where an action block's JSON body begins: the byte
// after the first newline following the opening fence. Sharing it keeps the
// one-shot matcher and the resumable scanner in lockstep.
func fenceBodyStart(text string, pos int) int {
	if i := strings.Index(text[pos:], "\n"); i >= 0 {
		return pos + i + 1
	}
	return pos
}

// ActionBlockScanner is the incremental counterpart of FindActionBlockSpan for
// streaming callers: the tool lookup maps are built once, and while one block
// is open the scan resumes from the stored closing-fence position instead of
// re-reading the whole withheld buffer on every delta. The caller shows the same
// buffer and calls Discard with exactly the prefix it removes, keeping every
// offset FindSpan returned valid against it.
type ActionBlockScanner struct {
	names   map[string]string
	schemas map[string]map[string]any

	pending    int // opening awaiting its close, -1 while none is open
	pendingXML bool
	// Resumable closing-fence state, valid while a fenced block is pending:
	closeAt int
	// bodyStart is where the pending fence's body begins. It equals the opening
	// until the label line closes, and never moves after that, so a delta that
	// arrives with a body already known costs no rescan of the label.
	bodyStart int
	str       jsonStringState
}

// maxPendingBlockBytes bounds how much text one open action block may withhold
// from a streaming client. Tool parameters never reach a fraction of it — the
// one-shot parser refuses more than defaultMaxScanBytes — so a block still
// undecided past the bound is prose: the streamer releases it instead of
// buffering, and re-scanning, a growing buffer for the rest of the turn.
//
// It is a var so tests can lower it; the package reads it only from this file,
// and a test that moves it must not run beside one that is reading it.
var maxPendingBlockBytes = 512 << 10

func NewActionBlockScanner(tools []ToolDef) *ActionBlockScanner {
	names, schemas := toolLookupMaps(tools)
	return &ActionBlockScanner{names: names, schemas: schemas, pending: -1}
}

// FindSpan returns exactly what FindActionBlockSpan would for the same buffer:
// the first accepted block's span, or pending=true with start at the opening
// that is still unterminated.
func (s *ActionBlockScanner) FindSpan(text string) (start, end int, pending bool) {
	if s.pending >= 0 && s.pending < len(text) {
		pos := s.pending
		if held := len(text) - pos; held > maxPendingBlockBytes {
			// This opening has held more than any tool call carries and is still
			// undecided, so it is not a call the parser is going to accept: give
			// the text back as prose and keep scanning past the marker. A block
			// that is merely large — a whole file in write_file — is bounded by
			// the same number and still parses; this only releases what stayed
			// undecided past it.
			log.Printf("toolemulation: an open action block at offset %d passed %d bytes without closing; releasing it as prose",
				pos, maxPendingBlockBytes)
			s.resetPending()
			return s.scanFrom(text, pos+1)
		}
		if s.pendingXML {
			// ponytail: an open XML block still re-runs its matcher each delta;
			// make scanXMLBlock resumable if that ever shows in a profile.
			m := matchXMLBlock(text, pos, s.names, s.schemas, false)
			if !m.closed {
				return pos, 0, true
			}
			s.resetPending()
			if m.call.Name != "" {
				return m.start, m.end, false
			}
			// Resume past the decided block so openings inside its span stay
			// part of it instead of becoming independent calls.
			return s.scanFrom(text, m.end)
		}
		// A fenced block becomes decidable the moment its closing fence shows
		// up, so between deltas only the new bytes are looked at. A hybrid body
		// can also complete on its own braces before any fence arrives, and only
		// the bytes up to the next opening are this block's to read.
		if scanClosingFence(text, &s.closeAt, &s.str) < 0 {
			nextOpen := nextBlockOpenAfter(text, pos)
			hybrid := matchJSONToolBody(text, pos, s.cachedBodyStart(text, pos), nextOpen, s.names, s.schemas, false)
			if decidedHere(hybrid, nextOpen) {
				s.resetPending()
				if hybrid.call.Name != "" {
					return hybrid.start, hybrid.end, false
				}
				return s.scanFrom(text, hybrid.end)
			}
			return pos, 0, true
		}
		m := matchActionBlockFrom(text, pos, s.cachedBodyStart(text, pos), s.names, s.schemas, false)
		s.resetPending()
		if !m.closed {
			s.setPending(text, pos) // unreachable once the fence is found; re-park anyway
			return pos, 0, true
		}
		if m.call.Name != "" {
			return m.start, m.end, false
		}
		return s.scanFrom(text, m.end)
	}
	s.resetPending()
	return s.scanFrom(text, 0)
}

// cachedBodyStart is the pending fence's body start, recomputed only while the
// label line is still open: before the first newline after the opening the body
// start is the opening itself, and the first newline moves it once and for all.
func (s *ActionBlockScanner) cachedBodyStart(text string, pos int) int {
	if s.bodyStart <= pos {
		s.bodyStart = fenceBodyStart(text, pos)
	}
	return s.bodyStart
}

// scanFrom mirrors FindActionBlockSpan's decision loop for openings after from.
func (s *ActionBlockScanner) scanFrom(text string, from int) (int, int, bool) {
	coverEnd := -1
	for _, pos := range findBlockOpenings(text) {
		if pos < from || pos < coverEnd {
			continue
		}
		m := matchBlockAt(text, pos, s.names, s.schemas, false)
		if !m.closed {
			s.setPending(text, pos)
			return pos, 0, true
		}
		coverEnd = m.end
		if m.call.Name != "" {
			return m.start, m.end, false
		}
	}
	return 0, 0, false
}

func (s *ActionBlockScanner) setPending(text string, pos int) {
	s.pending = pos
	s.pendingXML = strings.HasPrefix(text[pos:], xmlBlockOpen)
	if !s.pendingXML {
		s.closeAt, s.bodyStart, s.str = fenceBodyStart(text, pos), fenceBodyStart(text, pos), jsonStringState{}
	}
}

func (s *ActionBlockScanner) resetPending() {
	s.pending, s.pendingXML = -1, false
	s.closeAt, s.bodyStart, s.str = 0, 0, jsonStringState{}
}

// Discard shifts the remembered offset after the caller trimmed n bytes off the
// front of the buffer.
func (s *ActionBlockScanner) Discard(n int) {
	if n <= 0 {
		return
	}
	if s.pending >= 0 {
		s.pending -= n
		s.closeAt -= n
		s.bodyStart -= n
		if s.pending < 0 {
			s.resetPending()
		}
	}
}

// The fenced dialect's markers. The injected prompt teaches these exact bytes
// and the parser recognises exactly these, so they are named once: a dialect
// change that edited only one side would teach the model a block the parser
// cannot see.
const (
	actionFenceOpen  = "```json action"
	actionFenceClose = "```"
)

// The models behind the Qoder CLI were trained on a second, XML tool-call
// dialect and emit it even though the proxy's injected prompt teaches the fenced
// JSON shape above. Across the ZCode session store there were 200 blocks in this
// dialect against one fenced block, and nothing parsed them: the tags reached the
// IDE as prose, which is what users see as "replies full of tool calling".
//
// The tags are spelled out by concatenation so this source cannot itself look
// like a tool call in an agent transcript.
var (
	xmlBlockOpen   = "<" + "tool_call"
	xmlBlockClose  = "</" + "tool_call" + ">"
	xmlFuncOpen    = "<" + "function="
	xmlFuncClose   = "</" + "function" + ">"
	xmlInvokeOpen  = "<" + "invoke name="
	xmlInvokeClose = "</" + "invoke" + ">"
	xmlParamOpen   = "<" + "parameter="
	xmlParamClose  = "</" + "parameter" + ">"
)

func findXMLOpenings(text string) []int {
	out := make([]int, 0)
	for idx := 0; ; {
		i := indexFrom(text, xmlBlockOpen, idx)
		if i < 0 {
			return out
		}
		out = append(out, i)
		idx = i + len(xmlBlockOpen)
	}
}

// matchXMLBlock reads the block opened at pos and puts it through the same
// acceptance gate as the fenced JSON dialect. The read follows a fixed order:
// establish this wrapper's own boundary, reject structural empty shells at
// that boundary, then pick the body's dialect by the first structural token
// inside it — a JSON brace or a tag opening, whichever comes first. A brace
// that wins routes the whole body to the JSON reader, whose string-aware
// balancing keeps tags quoted inside JSON values from ever acting as
// structure, so an echoed XML example cannot hijack the call. Nothing past the
// boundary may be adopted as this block's structure or body.
func matchXMLBlock(text string, pos int, names map[string]string, schemas map[string]map[string]any, final bool) actionMatch {
	bodyFrom := xmlBodyStart(text, pos)
	if bodyFrom < 0 {
		return actionMatch{start: pos, closed: false}
	}
	// The boundary: this wrapper's own close, or the next wrapper's opening,
	// whichever comes first.
	closeAt := indexFrom(text, xmlBlockClose, bodyFrom)
	nextOpen := indexFrom(text, xmlBlockOpen, pos+len(xmlBlockOpen))
	boundary := closeAt
	if nextOpen >= 0 && (boundary < 0 || nextOpen < boundary) {
		boundary = nextOpen
	}

	// A wrapper that closes over nothing but blanks is a structural empty
	// shell: reject it at its own close so the block after it stands
	// independent. Distinct from prose that mentions a close ("saw
	// </tool_call> tags earlier"), whose body carries text and falls through
	// to the dialect choice below.
	if closeAt >= 0 && (nextOpen < 0 || closeAt < nextOpen) && strings.TrimSpace(text[bodyFrom:closeAt]) == "" {
		return actionMatch{start: pos, end: closeAt + len(xmlBlockClose), closed: true}
	}

	firstBrace := indexFrom(text, "{", bodyFrom)
	fn := indexFrom(text, xmlFuncOpen, bodyFrom)
	invoke := indexFrom(text, xmlInvokeOpen, bodyFrom)
	param := indexFrom(text, xmlParamOpen, bodyFrom)
	if boundary >= 0 {
		if firstBrace > boundary {
			firstBrace = -1
		}
		if fn > boundary {
			fn = -1
		}
		if invoke > boundary {
			invoke = -1
		}
		if param > boundary {
			param = -1
		}
	}
	tag := -1
	for _, at := range []int{fn, invoke, param} {
		if at >= 0 && (tag < 0 || at < tag) {
			tag = at
		}
	}
	if firstBrace >= 0 && (tag < 0 || firstBrace < tag) {
		// The JSON body starts before any tag structure — including label
		// variants like a glued "json action" hint — so the body is JSON.
		// The JSON reader's limit keeps its brace search inside this wrapper
		// (a later block's JSON is not this block's body) while its
		// string-aware balancing spans whatever the values quote.
		return matchJSONToolBody(text, pos, bodyFrom, nextOpen, names, schemas, final)
	}

	if block, complete, end := scanXMLBlock(text, pos); complete && block.name != "" {
		rejected := actionMatch{start: pos, end: end, closed: true}
		call, ok := validateToolCall(block.name, block.args, true, names, schemas)
		if !ok {
			return rejected
		}
		call.ID = newCallID()
		rejected.call = call
		return rejected
	}
	// A real parameter tag marks the tag-shaped XML dialect (possibly still
	// streaming in). A bare function=/invoke mention in the label is prose —
	// seeing the tag shape alone is not structural evidence, so the block
	// falls through to the JSON body reading below.
	if paramTagBeforeBrace(text, bodyFrom) {
		return actionMatch{start: pos, closed: false}
	}
	return matchJSONToolBody(text, pos, bodyFrom, nextOpen, names, schemas, final)
}

// paramTagBeforeBrace reports whether a parameter opening appears before the
// first JSON brace after bodyFrom, which marks the block as the tag-shaped XML
// dialect rather than a wrapper around a plain JSON body. Structural evidence
// never crosses this wrapper's own close: a closed shell cannot still be
// streaming a parameter in, so a parameter tag after the close belongs to the
// next block, not to this one.
func paramTagBeforeBrace(text string, bodyFrom int) bool {
	limit := indexFrom(text, "{", bodyFrom)
	if limit < 0 {
		limit = len(text)
	}
	if close := indexFrom(text, xmlBlockClose, bodyFrom); close >= 0 && close < limit {
		limit = close
	}
	if bodyFrom > limit {
		return false
	}
	return strings.Contains(text[bodyFrom:limit], xmlParamOpen)
}

// xmlBodyStart returns the offset just past the opening tag's ">".
func xmlBodyStart(text string, pos int) int {
	openEnd := indexFrom(text, ">", pos+len(xmlBlockOpen))
	if openEnd < 0 {
		return -1
	}
	return openEnd + 1
}

// jsonBodyEnd locates a complete, string-aware brace-balanced JSON object: the
// first '{' at or after from, returning the index just past its matching '}',
// or -1 when no complete object has arrived yet.
func jsonBodyEnd(text string, from int) int {
	start := indexFrom(text, "{", from)
	if start < 0 {
		return -1
	}
	var str jsonStringState
	depth := 0
	for i := start; i < len(text); i++ {
		ch := text[i]
		if str.step(ch) {
			continue
		}
		switch ch {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// matchJSONToolBody accepts a tool call whose JSON body carries the call while
// everything around it is decoration from a mixed dialect: the fenced label,
// XML open/close tags, or hallucinated closers. limit bounds the search for the
// body's first brace (-1 meaning unbounded): an XML wrapper passes its next
// opening so the reader cannot adopt a later block's JSON as this block's body.
// The span always covers the whole JSON body and then extends only along close
// syntax — trailing blanks on the body's line plus whole lines of close markers
// — so prose or a new code fence after the call survives. Whether that trailing
// close syntax has fully arrived is only decidable on final text.
//
// A wrapper can carry more than one JSON object and the call is not always the
// first of them: a model that shows a JSON sample and then asks for a tool
// leaves a note object in front of the call. Reading the first object as the
// whole body rejects the call behind it, so the reader steps over an object
// that names no tool. It steps over nothing else — an object that does name a
// tool is this block's answer, so an undeclared tool, a missing argument or a
// malformed body still rejects the whole block, and the search for a call never
// crosses this block's own next opening.
func matchJSONToolBody(text string, openPos, bodyFrom, limit int, names map[string]string, schemas map[string]map[string]any, final bool) actionMatch {
	firstBrace := indexFrom(text, "{", bodyFrom)
	if limit >= 0 && firstBrace > limit {
		firstBrace = -1
	}
	bodyEnd := -1
	if firstBrace >= 0 {
		bodyEnd = jsonBodyEnd(text, bodyFrom)
	}
	if bodyEnd < 0 {
		// A wrapper close with no JSON before it is a definite reject, so the
		// scan moves on instead of holding the streamer forever — but only on
		// final text, since mid-stream the body may still arrive after it.
		if closeAt := indexFrom(text, xmlBlockClose, bodyFrom); closeAt >= 0 && (firstBrace < 0 || firstBrace > closeAt) {
			if final {
				return actionMatch{start: openPos, end: closeAt + len(xmlBlockClose), closed: true}
			}
			return actionMatch{start: openPos, closed: false}
		}
		return actionMatch{start: openPos, closed: false}
	}
	for {
		spanEnd, settled := closeMarkerSpan(text, bodyEnd, final)
		if !settled {
			return actionMatch{start: openPos, closed: false}
		}
		rejected := actionMatch{start: openPos, end: spanEnd, closed: true}
		raw := text[firstBrace:bodyEnd]
		parsed, parsedOK := parseToolCallJSON(raw)
		if parsedOK {
			call, ok := validateToolCall(parsed.Name, parsed.Arguments, false, names, schemas)
			if !ok {
				// A named tool that failed the acceptance gate is the block's
				// answer: a wrong tool or a missing argument rejects the block
				// rather than reaching for another object behind it.
				return rejected
			}
			rejected.call = call
			return rejected
		}
		if !bodyIsFragment(raw) {
			// Not a tool call and not a plain object either: this is the block's
			// body, and it is malformed.
			return rejected
		}
		if !final {
			// Mid-stream the block stays undecided instead of stepping over: a
			// call can still arrive in the bytes yet to come, and deciding a
			// reject here would emit text the final parse then removes. Holding
			// costs the turn nothing — Flush runs this same decision.
			return actionMatch{start: openPos, closed: false}
		}
		// A fragment: a valid object that names no tool, so the call is in the
		// object behind it. Step over it, still inside this block's boundary.
		nextBrace := indexFrom(text, "{", bodyEnd)
		if nextBrace < 0 || (limit >= 0 && nextBrace > limit) {
			return rejected
		}
		nextEnd := jsonBodyEnd(text, nextBrace)
		if nextEnd < 0 {
			// The next object is still arriving, so the block is undecided
			// rather than rejected: the call may be in the bytes yet to come.
			return actionMatch{start: openPos, closed: false}
		}
		firstBrace, bodyEnd = nextBrace, nextEnd
	}
}

// bodyIsFragment reports whether raw is a valid JSON object that names no tool:
// a note or a sample standing in front of the call, which the body reader steps
// over. A body that is not even valid JSON is never a fragment, so the tolerant
// repairs do not turn garbage into a step-over.
func bodyIsFragment(raw string) bool {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return false
	}
	return strings.TrimSpace(stringFromAny(obj["tool"])) == "" &&
		strings.TrimSpace(stringFromAny(obj["name"])) == ""
}

// hybridCloseJunk lists whole lines that trail a JSON body in mixed-dialect
// blocks; consuming them keeps a dangling close tag from reaching the client
// as prose after the call was already extracted.
var hybridCloseJunk = []string{
	xmlBlockClose,
	xmlFuncClose,
	xmlParamClose,
	xmlInvokeClose,
	"</" + "result" + ">",
	"</" + "think" + ">",
	"```",
}

// closeMarkerSpan extends a call's span from the end of its JSON body along
// the wrapper's close syntax only: trailing blanks on the body's own line,
// then whole lines that are nothing but close markers. Prose, or a fence that
// opens a new code block, stops the scan so text after the call survives.
// settled is false while the next line could still grow into a close marker,
// which only final text can rule out.
func closeMarkerSpan(text string, from int, final bool) (end int, settled bool) {
	end = from
	for end < len(text) {
		lineEnd := indexFrom(text, "\n", end)
		if lineEnd < 0 && !final {
			// The line is still growing; judge it only when it can no longer
			// become a close marker.
			if cannotBeJunk(text[end:]) {
				return end, true
			}
			return end, false
		}
		if lineEnd < 0 {
			lineEnd = len(text)
		}
		line := strings.TrimSpace(text[end:lineEnd])
		if line == "" || isCloseJunkLine(line) {
			end = lineEnd + 1
			continue
		}
		return end, true
	}
	if end > len(text) {
		end = len(text)
	}
	// Mid-stream, more close syntax can still arrive; final text settles the
	// span wherever it ended up.
	return end, final
}

// isCloseJunkLine reports whether a finished line is nothing but a close marker.
func isCloseJunkLine(line string) bool {
	for _, junk := range hybridCloseJunk {
		if line == junk {
			return true
		}
	}
	return false
}

// cannotBeJunk reports whether a still-growing line can never become one of
// the close markers: it already diverges from every marker (a fence with a
// language tag like ```go, or plain prose).
func cannotBeJunk(line string) bool {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" {
		return false
	}
	for _, junk := range hybridCloseJunk {
		if strings.HasPrefix(junk, trimmed) {
			return false
		}
		if len(trimmed) > len(junk) && strings.HasPrefix(trimmed, junk) {
			return true
		}
	}
	return true
}

type xmlBlock struct {
	name string
	args map[string]any
}

// scanXMLBlock reads the function name and its parameters, returning complete
// false when the block is still open so a streamer knows to keep buffering.
// The third return is the offset just past the structure the scanner actually
// consumed: the block close that follows the last parameter, never the first
// block close in the text, which can sit inside a parameter value as a literal.
// Structure is never adopted past whichever ends this wrapper first — its own
// close or the next wrapper's opening — so an empty, prose-only, or
// unterminated shell cannot reach into the block after it and merge that
// block's tags into one call.
func scanXMLBlock(text string, pos int) (xmlBlock, bool, int) {
	openEnd := indexFrom(text, ">", pos+len(xmlBlockOpen))
	if openEnd < 0 {
		return xmlBlock{}, false, 0
	}
	cursor := openEnd + 1
	blockClose := indexFrom(text, xmlBlockClose, cursor)

	name := ""
	closer := ""
	fn := indexFrom(text, xmlFuncOpen, cursor)
	invoke := indexFrom(text, xmlInvokeOpen, cursor)
	adoptEnd := blockClose
	if next := indexFrom(text, xmlBlockOpen, cursor); next >= 0 && (adoptEnd < 0 || next < adoptEnd) {
		adoptEnd = next
	}
	if adoptEnd >= 0 {
		if fn >= 0 && fn > adoptEnd {
			fn = -1
		}
		if invoke >= 0 && invoke > adoptEnd {
			invoke = -1
		}
	}
	switch {
	case fn >= 0 && (invoke < 0 || fn < invoke):
		tagEnd := indexFrom(text, ">", fn+len(xmlFuncOpen))
		if tagEnd < 0 {
			return xmlBlock{}, false, 0
		}
		name = strings.TrimSpace(text[fn+len(xmlFuncOpen) : tagEnd])
		cursor = tagEnd + 1
		closer = xmlFuncClose
	case invoke >= 0:
		quote := indexFrom(text, `"`, invoke+len(xmlInvokeOpen))
		if quote < 0 {
			return xmlBlock{}, false, 0
		}
		closing := indexFrom(text, `"`, quote+1)
		tagEnd := indexFrom(text, ">", closing+1)
		if closing < 0 || tagEnd < 0 {
			return xmlBlock{}, false, 0
		}
		name = strings.TrimSpace(text[quote+1 : closing])
		cursor = tagEnd + 1
		closer = xmlInvokeClose
	default:
		// An opening tag with no function name is prose, not a call. The close
		// that ends it must be this wrapper's own — one sitting past a later
		// opening belongs to that later block and leaves this shell open.
		if blockClose < 0 || (adoptEnd >= 0 && blockClose > adoptEnd) {
			return xmlBlock{name: ""}, false, 0
		}
		return xmlBlock{name: ""}, true, blockClose + len(xmlBlockClose)
	}

	args := map[string]any{}
	for {
		param := indexFrom(text, xmlParamOpen, cursor)
		blockClose = indexFrom(text, xmlBlockClose, cursor)
		if blockClose < 0 {
			return xmlBlock{}, false, 0
		}
		if next := indexFrom(text, xmlBlockOpen, cursor); next >= 0 && next < blockClose && !paramValueSpansNext(text, param, next) {
			// This wrapper never closed: the close ahead belongs to the block
			// that opened inside it, and so does any parameter past that
			// opening. The shell stays open rather than adopting them.
			return xmlBlock{}, false, 0
		}
		nameClose := indexFrom(text, closer, cursor)
		if param >= 0 && param < blockClose && (nameClose < 0 || param < nameClose) {
			tagEnd := indexFrom(text, ">", param+len(xmlParamOpen))
			valueEnd := -1
			if tagEnd >= 0 {
				valueEnd = indexFrom(text, xmlParamClose, tagEnd+1)
			}
			if tagEnd < 0 || valueEnd < 0 {
				return xmlBlock{}, false, 0
			}
			if key := strings.TrimSpace(text[param+len(xmlParamOpen) : tagEnd]); key != "" {
				args[key] = strings.TrimSpace(text[tagEnd+1 : valueEnd])
			}
			cursor = valueEnd + len(xmlParamClose)
			continue
		}
		// A missing function close is tolerated: the block close ends the call.
		// It is also the consumed boundary: parameters may carry earlier literal
		// closes and function closes, so the cursor's own block close — the one
		// after everything scanned so far — is the only safe end.
		return xmlBlock{name: name, args: args}, true, blockClose + len(xmlBlockClose)
	}
}

// paramValueSpansNext reports whether the parameter opening at param encloses
// next inside its value: the value's own close tag sits beyond next, so next
// is quoted payload (a wrapper tag mentioned literally), not a later block
// opening that ends this one.
func paramValueSpansNext(text string, param, next int) bool {
	if param < 0 || param >= next {
		return false
	}
	tagEnd := indexFrom(text, ">", param+len(xmlParamOpen))
	if tagEnd < 0 || tagEnd >= next {
		return false
	}
	valueEnd := indexFrom(text, xmlParamClose, tagEnd+1)
	return valueEnd >= 0 && valueEnd > next
}

// coerceArgsBySchema retypes the raw text values the XML dialect carries, because
// a schema that declares an integer arrives as "15000" and clients reject that.
func coerceArgsBySchema(args map[string]any, schema map[string]any) map[string]any {
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return args
	}
	out := make(map[string]any, len(args))
	for key, value := range args {
		raw, isText := value.(string)
		prop, known := props[key]
		if !isText || !known {
			out[key] = value
			continue
		}
		out[key] = coerceTypedValue(raw, propertyType(prop))
	}
	return out
}

func propertyType(prop any) string {
	m, ok := prop.(map[string]any)
	if !ok {
		return ""
	}
	typ, _ := m["type"].(string)
	return typ
}

func coerceTypedValue(raw, typ string) any {
	trimmed := strings.TrimSpace(raw)
	switch typ {
	case "integer":
		if i, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			return i
		}
	case "number":
		if f, err := strconv.ParseFloat(trimmed, 64); err == nil {
			return f
		}
	case "boolean":
		if b, err := strconv.ParseBool(trimmed); err == nil {
			return b
		}
	case "object", "array":
		var value any
		if err := json.Unmarshal([]byte(normalizeJSON(trimmed)), &value); err == nil {
			return value
		}
	}
	return raw
}

func indexFrom(text, needle string, from int) int {
	if from < 0 {
		from = 0
	}
	if from >= len(text) {
		return -1
	}
	i := strings.Index(text[from:], needle)
	if i < 0 {
		return -1
	}
	return from + i
}

func toolLookupMaps(tools []ToolDef) (map[string]string, map[string]map[string]any) {
	names := make(map[string]string, len(tools))
	schemas := make(map[string]map[string]any, len(tools))
	leafCount := make(map[string]int, len(tools))
	type qualifiedLeaf struct{ name, leafKey string }
	qualifiedLeaves := make([]qualifiedLeaf, 0, len(tools))
	for _, t := range tools {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, exists := names[key]; !exists {
			names[key] = name
			schemas[name] = t.InputSchema
		}
		if ns := strings.TrimSpace(t.Namespace); ns != "" && strings.HasPrefix(name, ns+"__") {
			leafKey := strings.ToLower(strings.TrimPrefix(name, ns+"__"))
			leafCount[leafKey]++
			qualifiedLeaves = append(qualifiedLeaves, qualifiedLeaf{name: name, leafKey: leafKey})
		} else {
			leafCount[key]++
		}
	}
	// A bare leaf the model may have echoed resolves to its namespace only
	// when exactly one declared tool carries that leaf spelling; when two
	// namespaces (or a top-level tool and a namespace leaf) share it, only
	// the qualified spelling resolves -- the parser refuses to guess.
	for _, ql := range qualifiedLeaves {
		if leafCount[ql.leafKey] != 1 {
			continue
		}
		if _, exists := names[ql.leafKey]; !exists {
			names[ql.leafKey] = ql.name
		}
	}
	return names, schemas
}

// actionOpenNeedles are the fences ParseActionBlocks treats as the start of a
// candidate action block. They are shared with the streaming filter so a partial
// fence that arrives across two deltas cannot leak to the client.
var actionOpenNeedles = []string{actionFenceOpen, "```json\n", "```json\r\n"}

// ActionOpenPrefixHold returns how many trailing bytes of text could still grow
// into an opening tag of either dialect, so a streamer withholds exactly that
// much. Without the XML needle a half-received opening tag would leak to the
// client and only be recognised once its body arrived.
func ActionOpenPrefixHold(text string) int {
	needles := append(append([]string{}, actionOpenNeedles...), xmlBlockOpen)
	longest := 0
	for _, needle := range needles {
		if len(needle) > longest {
			longest = len(needle)
		}
	}
	for n := min(len(text), longest); n > 0; n-- {
		suffix := text[len(text)-n:]
		for _, needle := range needles {
			if strings.HasPrefix(needle, suffix) {
				return n
			}
		}
	}
	return 0
}

// FindActionBlockSpan locates the first action block in text that
// ParseActionBlocks would consume, as the span [start,end). pending is true when
// an opening fence is still unterminated, so a streaming caller must hold text
// from that point until more arrives.
func FindActionBlockSpan(text string, tools []ToolDef) (start, end int, pending bool) {
	names, schemas := toolLookupMaps(tools)
	coverEnd := -1
	for _, pos := range findBlockOpenings(text) {
		if pos < coverEnd {
			continue
		}
		m := matchBlockAt(text, pos, names, schemas, false)
		if !m.closed {
			return pos, 0, true
		}
		coverEnd = m.end
		if m.call.Name != "" {
			return m.start, m.end, false
		}
	}
	return 0, 0, false
}

// findBlockOpenings merges both dialects' openings in position order.
func findBlockOpenings(text string) []int {
	out := findActionOpenings(text)
	out = append(out, findXMLOpenings(text)...)
	sort.Ints(out)
	return out
}

func findActionOpenings(text string) []int {
	out := make([]int, 0)
	searches := actionOpenNeedles
	for idx := 0; idx < len(text); {
		foundAt := -1
		foundLen := 0
		for _, needle := range searches {
			i := strings.Index(text[idx:], needle)
			if i < 0 {
				continue
			}
			pos := idx + i
			if foundAt < 0 || pos < foundAt {
				foundAt = pos
				foundLen = len(needle)
			}
		}
		if foundAt < 0 {
			break
		}
		out = append(out, foundAt)
		idx = foundAt + foundLen
	}
	return out
}

func findClosingFence(text string, from int) int {
	i := from
	var str jsonStringState
	return scanClosingFence(text, &i, &str)
}

// scanClosingFence is the resumable form of findClosingFence: the caller keeps
// the cursor and the string state, so a streamer can continue where the last
// delta ended instead of re-reading the whole block body. After a -1 return the
// state points just past the bytes examined so far.
func scanClosingFence(text string, i *int, str *jsonStringState) int {
	for ; *i < len(text)-2; *i++ {
		ch := text[*i]
		if str.step(ch) {
			continue
		}
		if text[*i:*i+3] == actionFenceClose {
			return *i
		}
	}
	return -1
}

// jsonStringState is the one JSON string/backslash state machine in the parser.
// A quoted value's quotes and backslashes must never be read as structure, and
// four readers depend on that: the body balancer, the closing-fence scan, the
// escape repair and the trailing-comma repair. They share this struct instead of
// carrying hand-rolled copies whose '"' and '\' handling has to agree forever.
type jsonStringState struct {
	inString bool
	escape   bool
}

// step advances the machine past ch and reports whether ch was consumed as
// string content: the opening quote, the body and the escaped byte all count,
// because none of them is structure.
func (s *jsonStringState) step(ch byte) bool {
	consumed := s.inString || s.escape
	switch {
	case s.escape:
		s.escape = false
	case s.inString && ch == '\\':
		s.escape = true
	case ch == '"':
		s.inString = !s.inString
	}
	return consumed
}

// literal reports whether the machine is inside a string right now, so a repair
// can tell structure from payload.
func (s *jsonStringState) literal() bool { return s.inString }

// escaping reports whether the previous byte was a backslash that absorbs this
// one, which is how a repair tells an escape's payload from an ordinary byte.
func (s *jsonStringState) escaping() bool { return s.escape }

// parseToolCallJSON reads one action-block body. Legal JSON wins as-is, so
// values like smart quotes or Windows paths keep their exact bytes; only a
// body that fails strict unmarshaling goes through the tolerant repairs.
func parseToolCallJSON(raw string) (ToolCall, bool) {
	if call, ok := unmarshalToolCall(raw); ok {
		return call, true
	}
	return unmarshalToolCall(normalizeJSON(raw))
}

func unmarshalToolCall(raw string) (ToolCall, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		return ToolCall{}, false
	}

	name := strings.TrimSpace(stringFromAny(obj["tool"]))
	if name == "" {
		name = strings.TrimSpace(stringFromAny(obj["name"]))
	}
	if name == "" {
		return ToolCall{}, false
	}

	args, _ := obj["parameters"].(map[string]any)
	if args == nil {
		args, _ = obj["arguments"].(map[string]any)
	}
	if args == nil {
		args, _ = obj["input"].(map[string]any)
	}
	if args == nil {
		if s := strings.TrimSpace(stringFromAny(obj["parameters"])); s != "" {
			_ = json.Unmarshal([]byte(s), &args)
		}
	}
	if args == nil {
		// Fallback: treat all top-level fields except "tool"/"name" as parameters
		// Some models place arguments at the top level instead of nested under "parameters"
		args = make(map[string]any)
		for k, v := range obj {
			if k == "tool" || k == "name" {
				continue
			}
			args[k] = v
		}
	}
	if len(args) == 0 {
		args = map[string]any{}
	}

	return ToolCall{
		ID:        newCallID(),
		Name:      name,
		Arguments: args,
	}, true
}

func normalizeJSON(text string) string {
	text = strings.TrimSpace(text)
	replacer := strings.NewReplacer(
		"\u201c", "\"", "\u201d", "\"",
		"\u201c", "\"", "\u201d", "\"",
	)
	return repairInvalidEscapes(stripTrailingCommas(replacer.Replace(text)))
}

// stripTrailingCommas drops the comma a model leaves directly in front of a
// closing brace or bracket. It walks the shared string state machine, so a comma
// inside a quoted value keeps its bytes: the byte replacer this replaces rewrote
// those too, turning a value like "a, }b" into a body that no longer parses and
// losing the very call it was meant to rescue.
func stripTrailingCommas(text string) string {
	needsWork := false
	var str jsonStringState
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if str.step(ch) || ch != ',' {
			continue
		}
		if next := nextJSONToken(text, i+1); next == '}' || next == ']' {
			needsWork = true
			break
		}
	}
	if !needsWork {
		return text
	}

	var b strings.Builder
	b.Grow(len(text))
	str = jsonStringState{}
	for i := 0; i < len(text); i++ {
		ch := text[i]
		consumed := str.step(ch)
		if !consumed && ch == ',' {
			if next := nextJSONToken(text, i+1); next == '}' || next == ']' {
				continue
			}
		}
		b.WriteByte(ch)
	}
	return b.String()
}

// nextJSONToken returns the first byte at or after from that is not JSON
// whitespace, or 0 when only whitespace is left.
func nextJSONToken(text string, from int) byte {
	for i := from; i < len(text); i++ {
		switch text[i] {
		case ' ', '\t', '\n', '\r':
		default:
			return text[i]
		}
	}
	return 0
}

// repairInvalidEscapes requotes escape sequences JSON does not define, e.g.
// the \d and \( real models leave inside command and path strings: the
// backslash is kept and doubled, making the sequence a legal escaped
// backslash, so regexes and Windows paths keep their bytes instead of being
// silently rewritten. It only rewrites inside string literals; a legal escape
// never matches, so a valid body passes through byte-for-byte.
func repairInvalidEscapes(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	var str jsonStringState
	for i := 0; i < len(text); i++ {
		ch := text[i]
		if str.escaping() {
			switch ch {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't', 'u':
				// Legal escape: keep both bytes.
				b.WriteByte('\\')
				b.WriteByte(ch)
			default:
				// Stray escape: requote it so the value keeps its backslash.
				b.WriteString(`\\`)
				b.WriteByte(ch)
			}
			str.step(ch)
			continue
		}
		if str.literal() && ch == '\\' {
			// The escape's payload is written next round, when the repair knows
			// whether this sequence is one JSON defines.
			str.step(ch)
			continue
		}
		str.step(ch)
		b.WriteByte(ch)
	}
	if str.escaping() {
		// A dangling backslash at end-of-input: keep it rather than rewrite
		// the value; the unfinished string will fail unmarshaling anyway.
		b.WriteByte('\\')
	}
	return b.String()
}

func compactSchema(schema map[string]any) string {
	if len(schema) == 0 {
		return ""
	}
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return ""
	}

	required := map[string]bool{}
	if rawRequired, ok := schema["required"].([]any); ok {
		for _, item := range rawRequired {
			name := strings.TrimSpace(stringFromAny(item))
			if name != "" {
				required[name] = true
			}
		}
	}

	keys := make([]string, 0, len(props))
	for key := range props {
		keys = append(keys, key)
	}
	sortStrings(keys)

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		part := key
		if !required[key] {
			part += "?"
		}
		parts = append(parts, part)
	}
	return strings.Join(parts, ", ")
}

func truncate(text string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(strings.TrimSpace(text))
	if len(runes) <= max {
		return string(runes)
	}
	return string(runes[:max]) + "..."
}

func selectExampleTool(tools []ToolDef) (ToolDef, bool) {
	if len(tools) == 0 {
		return ToolDef{}, false
	}
	for _, tool := range tools {
		name := strings.ToLower(strings.TrimSpace(tool.Name))
		if strings.Contains(name, "read") || strings.Contains(name, "file") {
			return tool, true
		}
	}
	for _, tool := range tools {
		name := strings.ToLower(strings.TrimSpace(tool.Name))
		if strings.Contains(name, "bash") || strings.Contains(name, "shell") || strings.Contains(name, "command") {
			return tool, true
		}
	}
	return tools[0], true
}

func exampleParameters(toolName string, schema map[string]any) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return map[string]any{}
	}

	required := requiredKeys(schema)
	keys := make([]string, 0, 2)
	for _, key := range required {
		keys = append(keys, key)
		if len(keys) >= 2 {
			break
		}
	}
	if len(keys) == 0 {
		for key := range props {
			keys = append(keys, key)
			break
		}
	}

	out := map[string]any{}
	for _, key := range keys {
		prop, _ := props[key].(map[string]any)
		out[key] = exampleValueForKey(toolName, key, prop)
	}
	return out
}

func requiredKeys(schema map[string]any) []string {
	items, ok := schema["required"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		name := strings.TrimSpace(stringFromAny(item))
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

func exampleValueForKey(toolName string, key string, prop map[string]any) any {
	if enum, ok := prop["enum"].([]any); ok && len(enum) > 0 {
		return enum[0]
	}
	valueType := strings.ToLower(strings.TrimSpace(stringFromAny(prop["type"])))
	lowerKey := strings.ToLower(strings.TrimSpace(key))
	lowerTool := strings.ToLower(strings.TrimSpace(toolName))

	switch valueType {
	case "integer":
		return 1
	case "number":
		return 1
	case "boolean":
		return true
	case "array":
		return []any{}
	case "object":
		return map[string]any{}
	}

	switch {
	case strings.Contains(lowerKey, "path") || strings.Contains(lowerKey, "file"):
		return "README.md"
	case strings.Contains(lowerKey, "command") || lowerKey == "cmd" || strings.Contains(lowerTool, "bash") || strings.Contains(lowerTool, "shell") || strings.Contains(lowerTool, "exec_command"):
		return "pwd"
	case strings.Contains(lowerKey, "url"):
		return "https://example.com"
	default:
		return "value"
	}
}

func forceConstraint(choice ToolChoice, parallel *bool) string {
	switch choice.Mode {
	case "any":
		return "\n- You must output at least one " + actionFenceOpen + actionFenceClose + " block in this reply."
	case "tool":
		if strings.TrimSpace(choice.Name) != "" {
			return "\n- You must call \"" + strings.TrimSpace(choice.Name) + "\" in this reply."
		}
	}
	if parallel != nil && !*parallel {
		return "\n- Call only one tool at a time. Do not make multiple tool calls in a single response."
	}
	return ""
}

func filterArgsBySchema(args map[string]any, schema map[string]any) map[string]any {
	if len(args) == 0 || len(schema) == 0 {
		return args
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		return args
	}

	out := make(map[string]any, len(args))
	for k, v := range args {
		if _, known := props[k]; !known {
			continue
		}
		out[k] = v
	}
	return out
}

func hasRequiredArgs(args map[string]any, schema map[string]any) bool {
	for _, key := range requiredKeys(schema) {
		value, ok := args[key]
		if !ok {
			return false
		}
		if s, ok := value.(string); ok && strings.TrimSpace(s) == "" {
			return false
		}
	}
	return true
}

func cloneMap(src map[string]any) map[string]any {
	if src == nil {
		return nil
	}
	dst := make(map[string]any, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func stringFromAny(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	default:
		return ""
	}
}

func sortStrings(values []string) {
	if len(values) < 2 {
		return
	}
	for i := 0; i < len(values)-1; i++ {
		for j := i + 1; j < len(values); j++ {
			if values[j] < values[i] {
				values[i], values[j] = values[j], values[i]
			}
		}
	}
}

var callSeq uint64

func newCallID() string {
	seq := atomic.AddUint64(&callSeq, 1)
	return "toolu_01" + strconv.FormatUint(seq, 10) + "0000000000000000"
}
