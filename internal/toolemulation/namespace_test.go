package toolemulation

import (
	"strings"
	"testing"
)

// Codex 0.153.4 declares MCP tools to /v1/responses as a namespace entry whose
// leaves are flat function objects. These tests pin the root-cause contract:
// the leaf stays callable under an unambiguous qualified name, schema
// validation keeps working, and two namespaces sharing a leaf name (or a
// namespace leaf colliding with a top-level name) never collapse into one
// tool or get resolved by guessing.

func ns931ReadFixtureLeaf() map[string]any {
	return map[string]any{
		"type":        "function",
		"name":        "read_fixture",
		"description": "Read one synthetic acceptance fixture.",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"path": map[string]any{"type": "string"}},
			"required":   []any{"path"},
		},
	}
}

func ns931Namespace(name string, leaves ...map[string]any) map[string]any {
	tools := make([]any, 0, len(leaves))
	for _, leaf := range leaves {
		tools = append(tools, leaf)
	}
	return map[string]any{
		"type":        "namespace",
		"name":        name,
		"description": "Tools in the " + name + " namespace.",
		"tools":       tools,
	}
}

func ns931ActionFence(body string) string {
	return "```json " + "action" + "\n" + body + "\n```\n"
}

func TestNamespace931ExtractToolsFlattensLeafWithSchema(t *testing.T) {
	tools := ExtractTools([]any{
		ns931Namespace("mcp__fixture", ns931ReadFixtureLeaf()),
	})
	if len(tools) != 1 {
		t.Fatalf("tools = %d (%+v), want exactly the namespace leaf", len(tools), tools)
	}
	got := tools[0]
	if got.Name != "mcp__fixture__read_fixture" {
		t.Fatalf("Name = %q, want the qualified mcp__fixture__read_fixture spelling", got.Name)
	}
	if got.Namespace != "mcp__fixture" {
		t.Fatalf("Namespace = %q, want mcp__fixture", got.Namespace)
	}
	if got.InputSchema == nil {
		t.Fatal("InputSchema = nil, want the leaf parameters preserved")
	}
	if req, ok := got.InputSchema["required"].([]any); !ok || len(req) != 1 || req[0] != "path" {
		t.Fatalf("required = %#v, want [path]", got.InputSchema["required"])
	}
	if got.Description == "" {
		t.Fatal("Description lost: the leaf description must survive flattening")
	}
}

func TestNamespace931ExtractToolsCoexistsWithTopLevel(t *testing.T) {
	tools := ExtractTools([]any{
		ns931Namespace("mcp__fixture", ns931ReadFixtureLeaf()),
		map[string]any{"type": "function", "name": "shell", "parameters": map[string]any{"type": "object"}},
	})
	if len(tools) != 2 {
		t.Fatalf("tools = %d (%+v), want namespace leaf + top-level function", len(tools), tools)
	}
	if tools[0].Namespace != "mcp__fixture" || tools[1].Namespace != "" {
		t.Fatalf("namespaces = %q, %q; want only the first namespaced", tools[0].Namespace, tools[1].Namespace)
	}
	if tools[1].Name != "shell" {
		t.Fatalf("top-level Name = %q, want shell untouched", tools[1].Name)
	}
}

func TestNamespace931ExtractToolsSkipsNonFunctionLeaves(t *testing.T) {
	tools := ExtractTools([]any{
		ns931Namespace("mcp__fixture",
			ns931ReadFixtureLeaf(),
			map[string]any{"type": "custom", "name": "freeform", "description": "not a function"},
			map[string]any{"type": "web_search"},
		),
		ns931Namespace("mcp__empty"),
	})
	if len(tools) != 1 || tools[0].Name != "mcp__fixture__read_fixture" {
		t.Fatalf("tools = %+v, want only the function leaf from mcp__fixture", tools)
	}
}

func TestNamespace931ExtractToolsDuplicateQualifiedFirstWins(t *testing.T) {
	tools := ExtractTools([]any{
		map[string]any{"type": "function", "name": "mcp__a__dup", "parameters": map[string]any{"type": "object"}},
		ns931Namespace("mcp__a", map[string]any{"type": "function", "name": "dup", "parameters": map[string]any{"type": "object"}}),
	})
	if len(tools) != 1 {
		t.Fatalf("tools = %+v, want the colliding definitions collapsed to the first", tools)
	}
	if tools[0].Namespace != "" || tools[0].Name != "mcp__a__dup" {
		t.Fatalf("survivor = %+v, want the first-declared top-level tool to win", tools[0])
	}
}

func ns931ParseOne(t *testing.T, text string, tools []ToolDef) []ToolCall {
	t.Helper()
	calls, _, err := ParseActionBlocks(text, tools, Config{})
	if err != nil {
		t.Fatalf("ParseActionBlocks: %v", err)
	}
	return calls
}

func TestNamespace931QualifiedNameCallCarriesNamespace(t *testing.T) {
	tools := ExtractTools([]any{ns931Namespace("mcp__fixture", ns931ReadFixtureLeaf())})
	calls := ns931ParseOne(t, ns931ActionFence(`{"tool":"mcp__fixture__read_fixture","parameters":{"path":"alpha.txt"}}`), tools)
	if len(calls) != 1 {
		t.Fatalf("calls = %+v, want the qualified block accepted", calls)
	}
	if calls[0].Name != "mcp__fixture__read_fixture" || calls[0].Namespace != "mcp__fixture" {
		t.Fatalf("call = %+v, want qualified name with namespace stamped", calls[0])
	}
	if calls[0].Arguments["path"] != "alpha.txt" {
		t.Fatalf("args = %#v, want path preserved", calls[0].Arguments)
	}
}

func TestNamespace931UnambiguousLeafAliasResolves(t *testing.T) {
	tools := ExtractTools([]any{ns931Namespace("mcp__fixture", ns931ReadFixtureLeaf())})
	calls := ns931ParseOne(t, ns931ActionFence(`{"tool":"read_fixture","parameters":{"path":"alpha.txt"}}`), tools)
	if len(calls) != 1 || calls[0].Name != "mcp__fixture__read_fixture" || calls[0].Namespace != "mcp__fixture" {
		t.Fatalf("calls = %+v, want the sole read_fixture leaf resolved to its namespace", calls)
	}
}

func TestNamespace931AmbiguousLeafStaysProse(t *testing.T) {
	tools := ExtractTools([]any{
		ns931Namespace("mcp__alpha", ns931ReadFixtureLeaf()),
		ns931Namespace("mcp__beta", ns931ReadFixtureLeaf()),
	})
	calls, clean, err := ParseActionBlocks(ns931ActionFence(`{"tool":"read_fixture","parameters":{"path":"alpha.txt"}}`), tools, Config{})
	if err != nil {
		t.Fatalf("ParseActionBlocks: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %+v, want the ambiguous bare leaf rejected instead of guessed", calls)
	}
	if !strings.Contains(clean, `"tool"`) {
		t.Fatalf("clean = %q, want the undecided block kept as prose", clean)
	}
	for _, ns := range []string{"mcp__alpha", "mcp__beta"} {
		qualified := ns + "__read_fixture"
		got := ns931ParseOne(t, ns931ActionFence(`{"tool":"`+qualified+`","parameters":{"path":"x.txt"}}`), tools)
		if len(got) != 1 || got[0].Namespace != ns || got[0].LeafName() != "read_fixture" {
			t.Fatalf("qualified %s call = %+v, want it resolved into its own namespace", qualified, got)
		}
	}
}

func TestNamespace931LeafCollidingWithTopLevelDoesNotAlias(t *testing.T) {
	tools := ExtractTools([]any{
		map[string]any{"type": "function", "name": "read_fixture", "parameters": map[string]any{"type": "object"}},
		ns931Namespace("mcp__other", ns931ReadFixtureLeaf()),
	})
	calls := ns931ParseOne(t, ns931ActionFence(`{"tool":"read_fixture","parameters":{}}`), tools)
	if len(calls) != 1 || calls[0].Namespace != "" || calls[0].Name != "read_fixture" {
		t.Fatalf("calls = %+v, want bare read_fixture to hit the top-level tool, not the namespace leaf", calls)
	}
	got := ns931ParseOne(t, ns931ActionFence(`{"tool":"mcp__other__read_fixture","parameters":{"path":"x"}}`), tools)
	if len(got) != 1 || got[0].Namespace != "mcp__other" {
		t.Fatalf("calls = %+v, want the namespace-qualified spelling to reach the namespace tool", got)
	}
}

func TestNamespace931SchemaValidationStillApplies(t *testing.T) {
	tools := ExtractTools([]any{ns931Namespace("mcp__fixture", ns931ReadFixtureLeaf())})
	calls, clean, err := ParseActionBlocks(ns931ActionFence(`{"tool":"mcp__fixture__read_fixture","parameters":{}}`), tools, Config{})
	if err != nil {
		t.Fatalf("ParseActionBlocks: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %+v, want a namespace call missing its required arg rejected", calls)
	}
	if !strings.Contains(clean, `"tool"`) {
		t.Fatalf("clean = %q, want the rejected block kept as prose", clean)
	}
}

func TestNamespace931LeafName(t *testing.T) {
	cases := []struct {
		call ToolCall
		want string
	}{
		{ToolCall{Namespace: "mcp__a", Name: "mcp__a__read_fixture"}, "read_fixture"},
		{ToolCall{Name: "shell"}, "shell"},
		{ToolCall{Namespace: "mcp__a", Name: "unrelated"}, "unrelated"},
	}
	for _, tc := range cases {
		if got := tc.call.LeafName(); got != tc.want {
			t.Errorf("LeafName(%+v) = %q, want %q", tc.call, got, tc.want)
		}
	}
	if got := QualifiedToolName("mcp__a", "read"); got != "mcp__a__read" {
		t.Errorf("QualifiedToolName = %q, want mcp__a__read", got)
	}
	if got := QualifiedToolName("", "read"); got != "read" {
		t.Errorf("QualifiedToolName empty namespace = %q, want read", got)
	}
	if got := QualifiedToolName("mcp__a", " "); got != "" {
		t.Errorf("QualifiedToolName empty leaf = %q, want empty", got)
	}
}

func TestNamespace931ResolveToolChoice(t *testing.T) {
	single := ExtractTools([]any{ns931Namespace("mcp__fixture", ns931ReadFixtureLeaf())})
	if got := ResolveToolChoice(single, ToolChoice{Mode: "tool", Name: "read_fixture"}); got.Name != "mcp__fixture__read_fixture" {
		t.Fatalf("leaf choice resolved to %q, want the qualified spelling", got.Name)
	}
	if got := ResolveToolChoice(single, ToolChoice{Mode: "tool", Name: "mcp__fixture__read_fixture"}); got.Name != "mcp__fixture__read_fixture" {
		t.Fatalf("qualified choice resolved to %q, want it stable", got.Name)
	}
	ambiguous := ExtractTools([]any{
		ns931Namespace("mcp__alpha", ns931ReadFixtureLeaf()),
		ns931Namespace("mcp__beta", ns931ReadFixtureLeaf()),
	})
	if got := ResolveToolChoice(ambiguous, ToolChoice{Mode: "tool", Name: "read_fixture"}); got.Name != "read_fixture" {
		t.Fatalf("ambiguous choice resolved to %q, want it left verbatim", got.Name)
	}
	if got := ResolveToolChoice(single, ToolChoice{Mode: "tool", Name: "no_such_tool"}); got.Name != "no_such_tool" {
		t.Fatalf("unknown choice resolved to %q, want it left verbatim", got.Name)
	}
	if got := ResolveToolChoice(single, ToolChoice{Mode: "auto", Name: "read_fixture"}); got.Mode != "auto" || got.Name != "read_fixture" {
		t.Fatalf("auto choice = %+v, want it untouched", got)
	}
}

func TestNamespace931DecorateToolCallNamespaces(t *testing.T) {
	tools := ExtractTools([]any{
		ns931Namespace("mcp__alpha", ns931ReadFixtureLeaf()),
		map[string]any{"type": "function", "name": "shell", "parameters": map[string]any{"type": "object"}},
	})
	calls := []ToolCall{
		{ID: "1", Name: "mcp__alpha__read_fixture"},
		{ID: "2", Name: "shell"},
		{ID: "3", Name: "mcp__alpha__read_fixture", Namespace: "already"},
	}
	DecorateToolCallNamespaces(calls, tools)
	if calls[0].Namespace != "mcp__alpha" {
		t.Fatalf("qualified native call namespace = %q, want mcp__alpha", calls[0].Namespace)
	}
	if calls[1].Namespace != "" {
		t.Fatalf("top-level call namespace = %q, want empty", calls[1].Namespace)
	}
	if calls[2].Namespace != "already" {
		t.Fatalf("preset namespace overwritten: %q", calls[2].Namespace)
	}
	if calls[0].LeafName() != "read_fixture" {
		t.Fatalf("LeafName = %q, want read_fixture", calls[0].LeafName())
	}
}

func TestNamespace931UnknownToolBlockStaysProse(t *testing.T) {
	tools := ExtractTools([]any{ns931Namespace("mcp__fixture", ns931ReadFixtureLeaf())})
	calls, clean, err := ParseActionBlocks(ns931ActionFence(`{"tool":"mcp__fixture__write_fixture","parameters":{"path":"x"}}`), tools, Config{})
	if err != nil {
		t.Fatalf("ParseActionBlocks: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("calls = %+v, want an undeclared leaf rejected even inside a known namespace", calls)
	}
	if !strings.Contains(clean, `"tool"`) {
		t.Fatalf("clean = %q, want the rejected block kept as prose", clean)
	}
}

// A namespace whose flattened name collides with a real top-level tool is an
// ambiguous declaration: whichever definition won the flatten race, the other
// tool's identity silently changed. The collision must be visible so the
// request can be rejected instead of guessing.
func TestNamespace931QualifiedNameCollisionDetected(t *testing.T) {
	topLevel := func() map[string]any {
		return map[string]any{"type": "function", "name": "mcp__fixture__read_fixture", "parameters": map[string]any{"type": "object"}}
	}
	nsFixture := func() map[string]any {
		return ns931Namespace("mcp__fixture", ns931ReadFixtureLeaf())
	}
	cases := []struct {
		name  string
		tools []any
		want  string
	}{
		{"namespace first then top-level", []any{nsFixture(), topLevel()}, "mcp__fixture__read_fixture"},
		{"top-level first then namespace", []any{topLevel(), nsFixture()}, "mcp__fixture__read_fixture"},
		{"same namespace declared twice", []any{nsFixture(), nsFixture()}, "mcp__fixture__read_fixture"},
		{"top-level declared twice", []any{topLevel(), topLevel()}, "mcp__fixture__read_fixture"},
	}
	for _, tc := range cases {
		got := FindToolNameCollisions(tc.tools)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s: collisions = %v, want [%s]", tc.name, got, tc.want)
		}
	}
}

func TestNamespace931DistinctNamesDoNotCollide(t *testing.T) {
	clean := []any{
		ns931Namespace("mcp__alpha", ns931ReadFixtureLeaf()),
		ns931Namespace("mcp__beta", ns931ReadFixtureLeaf()),
		map[string]any{"type": "function", "name": "read_fixture", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "function", "name": "shell", "parameters": map[string]any{"type": "object"}},
		ns931Namespace("mcp__gamma", map[string]any{
			"type": "function", "name": "other", "parameters": map[string]any{"type": "object"},
		}),
	}
	if got := FindToolNameCollisions(clean); len(got) != 0 {
		t.Fatalf("distinct declarations reported collisions: %v", got)
	}
	if got := FindToolNameCollisions(nil); got != nil {
		t.Fatalf("nil tools reported collisions: %v", got)
	}
	if got := FindToolNameCollisions([]any{"bogus", 42}); got != nil {
		t.Fatalf("malformed entries reported collisions: %v", got)
	}
}
