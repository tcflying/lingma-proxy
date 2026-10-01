package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/service"
)

// 931 third-round regressions for Responses function namespaces (Codex
// 0.153.4 declares MCP tools as {type:"namespace",name,tools:[functions]}) and
// for empty tool results on every HTTP entry. A stub Lingma gateway answers
// each turn and captures the exact upstream request, so these pin the wire
// contract end to end without the cloud: two namespaces may share a leaf name
// and must come back as distinct function_call items (name=leaf,
// namespace=ns) in stream and non-stream alike, history function_calls
// re-enter as the qualified name the model was taught, and a legal empty tool
// output survives while a missing or wrongly-typed one is never fabricated.

const (
	ns931Model       = "kmodel"
	ns931ProseHead   = "两个命名空间各读一个文件。"
	ns931ProseTail   = "读完再总结。"
	ns931AlphaPath   = "alpha.txt"
	ns931BetaPath    = "beta.txt"
	ns931FenceOpen   = "```json " + "action"
	ns931FenceClose  = "```"
	ns931EmptyMarker = "(empty tool output)"
)

func ns931AlphaTools() []any {
	return []any{
		map[string]any{
			"type":        "namespace",
			"name":        "mcp__alpha",
			"description": "alpha fixture server",
			"tools": []any{map[string]any{
				"type":        "function",
				"name":        "read_fixture",
				"description": "read an alpha fixture",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			}},
		},
		map[string]any{
			"type":        "namespace",
			"name":        "mcp__beta",
			"description": "beta fixture server",
			"tools": []any{map[string]any{
				"type":        "function",
				"name":        "read_fixture",
				"description": "read a beta fixture",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			}},
		},
	}
}

func ns931FixtureTools() []any {
	return []any{
		map[string]any{
			"type":        "namespace",
			"name":        "mcp__fixture",
			"description": "Tools in the mcp__fixture namespace.",
			"tools": []any{map[string]any{
				"type":        "function",
				"name":        "read_fixture",
				"description": "Read one synthetic acceptance fixture.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			}},
		},
	}
}

// ns931DualCallTurn is the model turn: prose plus one fenced call into each
// namespace, both leaves named read_fixture. Pieces land on their own SSE
// deltas so the streaming filter must hold and release across splits.
var ns931DualCallPieces = []string{
	ns931ProseHead + "\n",
	ns931FenceOpen + "\n",
	`{"tool":"mcp__alpha__read_fixture",`,
	`"parameters":{"path":"alpha.txt"}}`,
	"\n" + ns931FenceClose + "\n",
	ns931FenceOpen + "\n",
	`{"tool":"mcp__beta__read_fixture",`,
	`"parameters":{"path":"beta.txt"}}`,
	"\n" + ns931FenceClose + "\n",
	ns931ProseTail,
}

var ns931ProseOnlyPieces = []string{"已读取，继续。\n", "总结完毕。"}

func ns931RawFrame(t *testing.T, inner any) string {
	t.Helper()
	body, err := json.Marshal(inner)
	if err != nil {
		t.Fatal(err)
	}
	outer, err := json.Marshal(map[string]any{"body": string(body), "statusCodeValue": 200})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(outer) + "\n\n"
}

// ns931NewEnv wires the isolated stack and captures every upstream request
// body, so history-replay and tool-output assertions run against the exact
// prompt the proxy sent to the Lingma gateway.
func ns931NewEnv(t *testing.T, pieces []string) (*httptest.Server, *ns931UpstreamLog) {
	t.Helper()
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	t.Setenv("LINGMA_AGGREGATE_TOOL_STREAM", "")

	log := &ns931UpstreamLog{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		log.add(string(raw))
		w.Header().Set("Content-Type", "text/event-stream")
		for _, piece := range pieces {
			_, _ = io.WriteString(w, ns931RawFrame(t, map[string]any{
				"choices": []any{map[string]any{
					"index": 0,
					"delta": map[string]any{"content": piece},
				}},
			}))
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, ns931RawFrame(t, map[string]any{
			"choices": []any{map[string]any{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 29},
		}))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
	}))
	t.Cleanup(upstream.Close)

	authFile := filepath.Join(t.TempDir(), "credentials.json")
	credential := `{"source":"test","token_expire_time":"4102444800000","auth":{` +
		`"cosy_key":"cosy-key-value","encrypt_user_info":"encrypted-user-info",` +
		`"user_id":"user-123456","machine_id":"machine-1234567890ab"}}`
	if err := os.WriteFile(authFile, []byte(credential), 0600); err != nil {
		t.Fatal(err)
	}

	svc := service.New(service.Config{
		Backend:        service.BackendRemote,
		RemoteBaseURL:  upstream.URL,
		RemoteAuthFile: authFile,
		Model:          ns931Model,
		Timeout:        30 * time.Second,
	})
	t.Cleanup(func() { _ = svc.Close() })

	proxy := httptest.NewServer(NewServer("", svc).http.Handler)
	t.Cleanup(proxy.Close)
	return proxy, log
}

type ns931UpstreamLog struct {
	mu     sync.Mutex
	bodies []string
}

func (l *ns931UpstreamLog) add(body string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.bodies = append(l.bodies, body)
}

func (l *ns931UpstreamLog) joined() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.bodies, "\n---upstream-turn---\n")
}

func ns931Post(t *testing.T, proxy *httptest.Server, path string, body []byte) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(proxy.URL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

func ns931ResponsesBody(t *testing.T, stream bool, tools []any, input []any) []byte {
	t.Helper()
	body := map[string]any{
		"model": ns931Model,
		"input": input,
		"tools": tools,
	}
	if stream {
		body["stream"] = true
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func ns931TextInput(text string) []any {
	return []any{map[string]any{
		"role":    "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	}}
}

// ns931GotCall is one decoded function_call item from a Responses body or SSE
// event, carrying the namespace split the wire contract demands.
type ns931GotCall struct {
	ID        string
	Name      string
	Namespace string
	Arguments string
	Status    string
}

func ns931CallFromItem(m map[string]any) (ns931GotCall, bool) {
	if strings.TrimSpace(stringFromAny(m["type"])) != "function_call" {
		return ns931GotCall{}, false
	}
	return ns931GotCall{
		ID:        stringFromAny(m["call_id"]),
		Name:      stringFromAny(m["name"]),
		Namespace: stringFromAny(m["namespace"]),
		Arguments: stringFromAny(m["arguments"]),
		Status:    stringFromAny(m["status"]),
	}, true
}

// ns931AssertDistinctNamespacedCalls checks the core no-confusion contract:
// two calls whose leaf names are identical but whose namespaces differ, each
// with its own arguments, id and completed status.
func ns931AssertDistinctNamespacedCalls(t *testing.T, calls []ns931GotCall) {
	t.Helper()
	if len(calls) != 2 {
		t.Fatalf("function_call items = %d (%+v), want exactly 2", len(calls), calls)
	}
	alpha, beta := calls[0], calls[1]
	if alpha.Namespace != "mcp__alpha" || beta.Namespace != "mcp__beta" {
		t.Fatalf("namespaces = %q, %q, want mcp__alpha then mcp__beta", alpha.Namespace, beta.Namespace)
	}
	if alpha.Name != "read_fixture" || beta.Name != "read_fixture" {
		t.Fatalf("leaf names = %q, %q, want read_fixture in both namespaces", alpha.Name, beta.Name)
	}
	if alpha.ID == "" || beta.ID == "" || alpha.ID == beta.ID {
		t.Fatalf("call ids = %q, %q, want distinct non-empty", alpha.ID, beta.ID)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(alpha.Arguments), &args); err != nil || args["path"] != ns931AlphaPath {
		t.Fatalf("alpha arguments = %q, want path %q", alpha.Arguments, ns931AlphaPath)
	}
	if err := json.Unmarshal([]byte(beta.Arguments), &args); err != nil || args["path"] != ns931BetaPath {
		t.Fatalf("beta arguments = %q, want path %q", beta.Arguments, ns931BetaPath)
	}
	if alpha.Status != "completed" || beta.Status != "completed" {
		t.Fatalf("statuses = %q, %q, want completed", alpha.Status, beta.Status)
	}
	for _, call := range calls {
		if strings.Contains(call.Name, "mcp__") {
			t.Fatalf("item name %q leaked the qualified spelling; the wire name must be the leaf", call.Name)
		}
	}
}

// ns931AssertAddedIdentity checks the in_progress announcement: the leaf name,
// the namespace and distinct ids must already be there, while the arguments
// arrive later through their own events.
func ns931AssertAddedIdentity(t *testing.T, calls []ns931GotCall) {
	t.Helper()
	if len(calls) != 2 {
		t.Fatalf("added function_call items = %d (%+v), want exactly 2", len(calls), calls)
	}
	alpha, beta := calls[0], calls[1]
	if alpha.Namespace != "mcp__alpha" || beta.Namespace != "mcp__beta" {
		t.Fatalf("added namespaces = %q, %q, want mcp__alpha then mcp__beta", alpha.Namespace, beta.Namespace)
	}
	if alpha.Name != "read_fixture" || beta.Name != "read_fixture" {
		t.Fatalf("added leaf names = %q, %q, want read_fixture in both namespaces", alpha.Name, beta.Name)
	}
	if alpha.ID == "" || beta.ID == "" || alpha.ID == beta.ID {
		t.Fatalf("added call ids = %q, %q, want distinct non-empty", alpha.ID, beta.ID)
	}
}

func ns931CollectBodyCalls(t *testing.T, body string) []ns931GotCall {
	t.Helper()
	var resp struct {
		Output []map[string]any `json:"output"`
		Status string           `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	if resp.Status != "completed" {
		t.Fatalf("response status = %q, want completed", resp.Status)
	}
	calls := make([]ns931GotCall, 0, 2)
	for _, item := range resp.Output {
		if call, ok := ns931CallFromItem(item); ok {
			calls = append(calls, call)
		}
	}
	return calls
}

func TestNamespace931ResponsesNonStreamTwoSameLeaf(t *testing.T) {
	proxy, _ := ns931NewEnv(t, ns931DualCallPieces)
	code, body := ns931Post(t, proxy, "/v1/responses", ns931ResponsesBody(t, false, ns931AlphaTools(), ns931TextInput("read both fixtures")))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	ns931AssertDistinctNamespacedCalls(t, ns931CollectBodyCalls(t, body))
}

// ns931ParseResponsesStream decodes the function_call lifecycle from SSE:
// output_item.added, function_call_arguments.delta/done, output_item.done and
// response.completed, keeping every namespace it saw per stage.
type ns931StreamCalls struct {
	Added     []ns931GotCall
	ArgsDone  []ns931GotCall
	ItemDone  []ns931GotCall
	Completed []ns931GotCall
	SawDone   bool
}

func ns931ParseResponsesStream(t *testing.T, body string) *ns931StreamCalls {
	t.Helper()
	out := &ns931StreamCalls{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			out.SawDone = true
			continue
		}
		var event struct {
			Type     string         `json:"type"`
			Item     map[string]any `json:"item"`
			Name     string         `json:"name"`
			Response *struct {
				Output []map[string]any `json:"output"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("event payload is not JSON: %v\n%s", err, payload)
		}
		switch event.Type {
		case "response.output_item.added":
			if call, ok := ns931CallFromItem(event.Item); ok {
				out.Added = append(out.Added, call)
			}
		case "response.function_call_arguments.done":
			out.ArgsDone = append(out.ArgsDone, ns931GotCall{
				Name:      event.Name,
				Namespace: stringFromAny(decodeMapKey(payload, "namespace")),
			})
		case "response.output_item.done":
			if call, ok := ns931CallFromItem(event.Item); ok {
				out.ItemDone = append(out.ItemDone, call)
			}
		case "response.completed":
			if event.Response != nil {
				for _, item := range event.Response.Output {
					if call, ok := ns931CallFromItem(item); ok {
						out.Completed = append(out.Completed, call)
					}
				}
			}
		}
	}
	return out
}

func decodeMapKey(payload, key string) any {
	var m map[string]any
	if err := json.Unmarshal([]byte(payload), &m); err != nil {
		return nil
	}
	return m[key]
}

func TestNamespace931ResponsesStreamTwoSameLeaf(t *testing.T) {
	proxy, _ := ns931NewEnv(t, ns931DualCallPieces)
	code, body := ns931Post(t, proxy, "/v1/responses", ns931ResponsesBody(t, true, ns931AlphaTools(), ns931TextInput("read both fixtures")))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	stream := ns931ParseResponsesStream(t, body)
	if !stream.SawDone {
		t.Fatalf("the stream never sent [DONE]: %s", body)
	}
	ns931AssertAddedIdentity(t, stream.Added)
	ns931AssertDistinctNamespacedCalls(t, stream.ItemDone)
	ns931AssertDistinctNamespacedCalls(t, stream.Completed)
	if len(stream.ArgsDone) != 2 {
		t.Fatalf("function_call_arguments.done events = %d, want 2 (one per namespaced call)", len(stream.ArgsDone))
	}
	for _, done := range stream.ArgsDone {
		if done.Namespace == "" {
			t.Fatalf("arguments.done for %q carries no namespace; stream and body must agree", done.Name)
		}
	}
	// Stream/non-stream consistency: the completed frame must carry the same
	// namespace/name split the output_item events already delivered.
	for i, call := range stream.Completed {
		if call.Namespace != stream.ItemDone[i].Namespace || call.Name != stream.ItemDone[i].Name {
			t.Fatalf("completed item %d (%+v) diverges from output_item.done (%+v)", i, call, stream.ItemDone[i])
		}
	}
}

func TestNamespace931ResponsesHistoryReplayAndEmptyOutput(t *testing.T) {
	proxy, log := ns931NewEnv(t, ns931ProseOnlyPieces)
	input := append(ns931TextInput("read alpha"),
		map[string]any{
			"type":      "function_call",
			"name":      "read_fixture",
			"namespace": "mcp__fixture",
			"call_id":   "call_hist_1",
			"arguments": `{"path":"alpha.txt"}`,
		},
		map[string]any{
			"type":    "function_call_output",
			"call_id": "call_hist_1",
			"output":  "",
		},
	)
	input = append(input, ns931TextInput("summarize")...)
	code, body := ns931Post(t, proxy, "/v1/responses", ns931ResponsesBody(t, false, ns931FixtureTools(), input))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	upstream := log.joined()
	if !strings.Contains(upstream, "mcp__fixture__read_fixture") {
		t.Fatalf("upstream prompt lost the qualified history name; prompt follows.\n%s", upstream)
	}
	if !strings.Contains(upstream, "alpha.txt") {
		t.Fatalf("upstream prompt lost the history call arguments; prompt follows.\n%s", upstream)
	}
	if !strings.Contains(upstream, ns931EmptyMarker) {
		t.Fatalf("upstream prompt dropped the legal empty tool output; prompt follows.\n%s", upstream)
	}
	var resp struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil || resp.Status != "completed" {
		t.Fatalf("response did not complete: %s", body)
	}
}

func TestNamespace931ResponsesMissingAndMalformedOutputNotFabricated(t *testing.T) {
	proxy, log := ns931NewEnv(t, ns931ProseOnlyPieces)
	input := append(ns931TextInput("check outputs"),
		map[string]any{
			"type":      "function_call",
			"name":      "read_fixture",
			"namespace": "mcp__fixture",
			"call_id":   "call_missing",
			"arguments": `{"path":"alpha.txt"}`,
		},
		map[string]any{
			"type":      "function_call",
			"name":      "read_fixture",
			"namespace": "mcp__fixture",
			"call_id":   "call_badtype",
			"arguments": `{"path":"beta.txt"}`,
		},
		map[string]any{
			"type":      "function_call",
			"name":      "read_fixture",
			"namespace": "mcp__fixture",
			"call_id":   "call_ok",
			"arguments": `{"path":"alpha.txt"}`,
		},
		map[string]any{"type": "function_call_output", "call_id": "call_missing"},
		map[string]any{"type": "function_call_output", "call_id": "call_badtype", "output": 123},
		map[string]any{"type": "function_call_output", "call_id": "call_ok", "output": "real payload 931"},
	)
	code, body := ns931Post(t, proxy, "/v1/responses", ns931ResponsesBody(t, false, ns931FixtureTools(), input))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	upstream := log.joined()
	if !strings.Contains(upstream, "real payload 931") {
		t.Fatalf("upstream prompt lost the well-formed tool output; prompt follows.\n%s", upstream)
	}
	if strings.Contains(upstream, ns931EmptyMarker) {
		t.Fatalf("upstream prompt fabricated a result for a missing or wrongly-typed output; prompt follows.\n%s", upstream)
	}
	if strings.Contains(upstream, "123") && strings.Contains(upstream, "call_badtype") {
		t.Fatalf("the non-string output value must not be stringified into the prompt; prompt follows.\n%s", upstream)
	}
}

func TestNamespace931ChatEmptyToolContentPreserved(t *testing.T) {
	proxy, log := ns931NewEnv(t, ns931ProseOnlyPieces)
	body := map[string]any{
		"model": ns931Model,
		"messages": []any{
			map[string]any{"role": "user", "content": "read alpha"},
			map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id":   "call_chat_1",
					"type": "function",
					"function": map[string]any{
						"name":      "read_fixture",
						"arguments": `{"path":"alpha.txt"}`,
					},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": "call_chat_1", "content": ""},
			map[string]any{"role": "user", "content": "continue"},
		},
		"tools": ns931FixtureTools(),
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	code, resp := ns931Post(t, proxy, "/v1/chat/completions", raw)
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, resp)
	}
	if upstream := log.joined(); !strings.Contains(upstream, ns931EmptyMarker) {
		t.Fatalf("chat entry dropped the legal empty tool content; upstream prompt follows.\n%s", upstream)
	}
}

func TestNamespace931AnthropicEmptyToolResultPreserved(t *testing.T) {
	proxy, log := ns931NewEnv(t, ns931ProseOnlyPieces)
	body := map[string]any{
		"model":      ns931Model,
		"max_tokens": 1024,
		"messages": []any{
			map[string]any{"role": "user", "content": "read alpha"},
			map[string]any{
				"role": "assistant",
				"content": []any{map[string]any{
					"type":  "tool_use",
					"id":    "toolu_931_1",
					"name":  "read_fixture",
					"input": map[string]any{"path": "alpha.txt"},
				}},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_931_1", "content": []any{}},
					map[string]any{"type": "text", "text": "continue"},
				},
			},
		},
		"tools": []any{map[string]any{
			"name":        "read_fixture",
			"description": "read a fixture",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []any{"path"},
			},
		}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	code, resp := ns931Post(t, proxy, "/v1/messages", raw)
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, resp)
	}
	if upstream := log.joined(); !strings.Contains(upstream, ns931EmptyMarker) {
		t.Fatalf("anthropic entry dropped the legal empty tool_result; upstream prompt follows.\n%s", upstream)
	}
}

func TestNamespace931AnthropicToolResultMissingContentDropped(t *testing.T) {
	proxy, log := ns931NewEnv(t, ns931ProseOnlyPieces)
	body := map[string]any{
		"model":      ns931Model,
		"max_tokens": 1024,
		"messages": []any{
			map[string]any{"role": "user", "content": "read alpha"},
			map[string]any{
				"role": "assistant",
				"content": []any{map[string]any{
					"type":  "tool_use",
					"id":    "toolu_931_2",
					"name":  "read_fixture",
					"input": map[string]any{"path": "alpha.txt"},
				}},
			},
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_931_2"},
					map[string]any{"type": "text", "text": "continue"},
				},
			},
		},
		"tools": []any{map[string]any{
			"name":        "read_fixture",
			"description": "read a fixture",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []any{"path"},
			},
		}},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	code, resp := ns931Post(t, proxy, "/v1/messages", raw)
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, resp)
	}
	if upstream := log.joined(); strings.Contains(upstream, ns931EmptyMarker) {
		t.Fatalf("anthropic entry fabricated a result for a tool_result without content; upstream prompt follows.\n%s", upstream)
	}
}

// TestNamespace931AmbiguousQualifiedNameRejected pins the entry-level
// verdict for a declaration whose flattened namespace name equals a real
// top-level tool name: the request is rejected with the colliding name in
// the error, in either declaration order, rather than silently keeping one
// definition's identity and executing the wrong tool.
func TestNamespace931AmbiguousQualifiedNameRejected(t *testing.T) {
	topLevel := func() map[string]any {
		return map[string]any{"type": "function", "name": "mcp__fixture__read_fixture", "parameters": map[string]any{"type": "object"}}
	}
	nsFixture := func() map[string]any {
		return map[string]any{
			"type":        "namespace",
			"name":        "mcp__fixture",
			"description": "Tools in the mcp__fixture namespace.",
			"tools": []any{map[string]any{
				"type":        "function",
				"name":        "read_fixture",
				"description": "Read one synthetic acceptance fixture.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			}},
		}
	}
	for _, order := range []struct {
		name  string
		tools []any
	}{
		{"namespace then top-level", []any{nsFixture(), topLevel()}},
		{"top-level then namespace", []any{topLevel(), nsFixture()}},
	} {
		proxy, _ := ns931NewEnv(t, ns931ProseOnlyPieces)
		code, body := ns931Post(t, proxy, "/v1/responses", ns931ResponsesBody(t, false, order.tools, ns931TextInput("read alpha")))
		if code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d body = %s, want 400", order.name, code, body)
		}
		if !strings.Contains(body, "mcp__fixture__read_fixture") {
			t.Fatalf("%s: error must name the colliding tool, got: %s", order.name, body)
		}
	}
}

// TestNamespace931ToolChoiceResolvesLeafToQualified pins the normalization
// seam: forcing a namespaced tool by its leaf name must force the qualified
// spelling the model was taught, while an ambiguous leaf stays verbatim.
func TestNamespace931ToolChoiceResolvesLeafToQualified(t *testing.T) {
	single, err := normalizeOpenAIRequest(context.Background(), openAIChatRequest{
		Model:    ns931Model,
		Messages: []rawMessage{{Role: "user", Content: "hi"}},
		Tools:    ns931FixtureTools(),
		ToolChoice: map[string]any{
			"type": "function",
			"name": "read_fixture",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if single.ToolChoice.Mode != "tool" || single.ToolChoice.Name != "mcp__fixture__read_fixture" {
		t.Fatalf("resolved choice = %+v, want the qualified mcp__fixture__read_fixture forcing", single.ToolChoice)
	}

	ambiguous, err := normalizeOpenAIRequest(context.Background(), openAIChatRequest{
		Model:    ns931Model,
		Messages: []rawMessage{{Role: "user", Content: "hi"}},
		Tools:    ns931AlphaTools(),
		ToolChoice: map[string]any{
			"type": "function",
			"name": "read_fixture",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ambiguous.ToolChoice.Name != "read_fixture" {
		t.Fatalf("ambiguous leaf choice resolved to %q, want it left verbatim", ambiguous.ToolChoice.Name)
	}
}
