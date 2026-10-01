package httpapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/service"
)

// 932 native-channel dialect fallback: a request that declared native tools
// can still be answered in the action-block dialect, and the proxy must serve
// that answer as real protocol tool calls (tool_calls / tool_use /
// function_call) with the raw block stripped from the prose. The fixtures and
// the matrix follow the 931 interface style: a stub Lingma gateway answers
// every turn from a scripted queue, so the whole chain (remote client ->
// service conversion -> HTTP handler) runs isolated from the cloud.
//
// The three real-leak strings below are byte-exact captures:
//   - mmx turn 2 (Anthropic native tools, 2026-09-30, .scratch/932-clients/
//     mmx-report.json): the model emitted two wrappers; the first lost its
//     <function= header, the second is a complete call to the declared
//     read_file.
//   - the Codex/chat capture (.scratch/931-real-accept-20260929/
//     postfix-cn-chat-sse.json): the wrapper names Read while the request
//     declared read_fixture, and the deployed build streamed it to the client
//     as content next to a converted call.
const (
	native932Head = "我先看一下 go.mod 的内容。"
	native932Tail = "看完再决定下一步。"

	// The fenced dialect the proxy's own prompt teaches.
	native932JSONBlock = "```json action\n" +
		`{"tool":"read_file","parameters":{"path":"go.mod"}}` + "\n```"

	// Byte-exact second wrapper of the mmx turn-2 leak: a complete call to a
	// declared tool.
	native932MMXBlock2 = "<" + "tool_call" + ">\n" +
		"<" + "function=read_file" + ">\n" +
		"<" + "parameter=path" + ">\n" +
		"tokenizer.go\n" +
		"</" + "parameter" + ">\n" +
		"</" + "function" + ">\n" +
		"</" + "tool_call" + ">"

	// Byte-exact full text of the mmx turn-2 leak: the first wrapper lost its
	// function header, which by the parser's unclosed-parent conservatism owns
	// everything after it, so the whole turn must stay prose.
	native932MMXTurn2 = "<" + "tool_call" + ">\n" +
		"<" + "parameter=read_file" + ">\n" +
		"<" + "parameter=path" + ">\n" +
		"go.mod\n" +
		"</" + "parameter" + ">\n" +
		"</" + "function" + ">\n" +
		"</" + "tool_call" + ">\n" +
		"<" + "tool_call" + ">\n" +
		"<" + "function=read_file" + ">\n" +
		"<" + "parameter=path" + ">\n" +
		"tokenizer.go\n" +
		"</" + "parameter" + ">\n" +
		"</" + "function" + ">\n" +
		"</" + "tool_call" + ">"

	// Byte-exact wrapper of the postfix-cn-chat-sse leak: the tool name Read
	// is not among the declared tools (read_fixture was), so converting it
	// would execute a hallucinated tool.
	native932PostfixBlock = "<" + "tool_call" + ">\n" +
		"<" + "function=Read" + ">\n" +
		"<" + "parameter=file_path" + ">\n" +
		`G:\qoder-intl-project\lingma-proxy\.scratch\931-real-accept-20260929\workspace\postfix-cn-chat-sse-alpha.txt` + "\n" +
		"</" + "parameter" + ">\n" +
		"</" + "function" + ">\n" +
		"</" + "tool_call" + ">"

	native932UserTurn = "读一下 go.mod。"
)

// native932Script is one scripted upstream turn: frames are written verbatim
// in order, so a script can mix content deltas with native tool_calls deltas.
type native932Script []string

func native932ContentFrames(pieces ...string) native932Script {
	out := make(native932Script, 0, len(pieces)+1)
	for _, piece := range pieces {
		out = append(out, piece)
	}
	return out
}

func native932ToolCallFrame(t *testing.T, id, name, argsJSON string) string {
	t.Helper()
	return hybrid931RawFrame(t, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"tool_calls": []any{map[string]any{
				"index": 0, "id": id, "type": "function",
				"function": map[string]any{"name": name, "arguments": argsJSON},
			}}},
		}},
	})
}

// native932NewEnv serves scripts in order, one per upstream hit; once the
// queue is empty the last script repeats, so an unexpected retry still gets a
// well-formed answer instead of a broken stream.
func native932NewEnv(t *testing.T, scripts []native932Script) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	t.Setenv("LINGMA_AGGREGATE_TOOL_STREAM", "")
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	if len(scripts) == 0 {
		t.Fatal("native932NewEnv needs at least one scripted answer")
	}

	var upstreamCalls atomic.Int32
	var mu atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(mu.Add(1)) - 1
		upstreamCalls.Add(1)
		if index >= len(scripts) {
			index = len(scripts) - 1
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, frame := range scripts[index] {
			var payload string
			if strings.HasPrefix(frame, "data:") {
				payload = frame
			} else {
				payload = hybrid931ContentFrame(t, frame)
			}
			if _, err := io.WriteString(w, payload); err != nil {
				t.Errorf("write upstream frame: %v", err)
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if _, err := io.WriteString(w, hybrid931FinishFrame(t)); err != nil {
			t.Errorf("write upstream finish frame: %v", err)
			return
		}
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
		Model:          hybrid931Model,
		Timeout:        30 * time.Second,
	})
	t.Cleanup(func() { _ = svc.Close() })

	proxy := httptest.NewServer(NewServer("", svc).http.Handler)
	t.Cleanup(proxy.Close)
	return proxy, &upstreamCalls
}

// native932ReadTools declares the mmx tool set under its Anthropic shape.
func native932ReadTools() []any {
	return []any{
		map[string]any{
			"name":        "read_file",
			"description": "Read one file from the workspace. path is relative to the workspace root.",
			"input_schema": map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []any{"path"},
			},
		},
		map[string]any{
			"name":        "write_file",
			"description": "Create or overwrite one file in the workspace.",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"content": map[string]any{"type": "string"},
				},
				"required": []any{"path", "content"},
			},
		},
	}
}

// native932ReadToolsOpenAI declares the same set under the nested OpenAI
// chat-completions shape.
func native932ReadToolsOpenAI() []any {
	return []any{
		map[string]any{"type": "function", "function": map[string]any{
			"name":        "read_file",
			"description": "Read one file from the workspace. path is relative to the workspace root.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []any{"path"},
			},
		}},
		map[string]any{"type": "function", "function": map[string]any{
			"name":        "write_file",
			"description": "Create or overwrite one file in the workspace.",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"content": map[string]any{"type": "string"},
				},
				"required": []any{"path", "content"},
			},
		}},
	}
}

func native932AnthropicBody(t *testing.T, stream bool, toolChoice any) []byte {
	t.Helper()
	body := map[string]any{
		"model":      hybrid931Model,
		"max_tokens": 1024,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": native932UserTurn}},
		}},
		"tools": native932ReadTools(),
	}
	if stream {
		body["stream"] = true
	}
	if toolChoice != nil {
		body["tool_choice"] = toolChoice
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func native932OpenAIBody(t *testing.T, stream bool, toolChoice any) []byte {
	t.Helper()
	body := map[string]any{
		"model": hybrid931Model,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": native932UserTurn,
		}},
		"tools": native932ReadToolsOpenAI(),
	}
	if stream {
		body["stream"] = true
	}
	if toolChoice != nil {
		body["tool_choice"] = toolChoice
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func native932ResponsesBody(t *testing.T, stream bool, toolChoice any) []byte {
	t.Helper()
	body := map[string]any{
		"model": hybrid931Model,
		"input": native932UserTurn,
		"tools": []any{
			map[string]any{
				"type":        "function",
				"name":        "read_file",
				"description": "Read one file from the workspace. path is relative to the workspace root.",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"path": map[string]any{"type": "string"}},
					"required":   []any{"path"},
				},
			},
			map[string]any{
				"type":        "function",
				"name":        "write_file",
				"description": "Create or overwrite one file in the workspace.",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"content": map[string]any{"type": "string"},
					},
					"required": []any{"path", "content"},
				},
			},
		},
	}
	if stream {
		body["stream"] = true
	}
	if toolChoice != nil {
		body["tool_choice"] = toolChoice
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func native932Post(t *testing.T, proxy *httptest.Server, path string, body []byte) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Post(proxy.URL+path, "application/json", strings.NewReader(string(body)))
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

// native932SplitLines keeps each line's newline attached, so a block streamed
// line by line reassembles byte-for-byte.
func native932SplitLines(text string) []string {
	out := strings.SplitAfter(text, "\n")
	filtered := out[:0]
	for _, line := range out {
		if line != "" {
			filtered = append(filtered, line)
		}
	}
	return filtered
}

// native932AssertNoDialectLeak fails when any raw dialect marker reached the
// client as content text.
func native932AssertNoDialectLeak(t *testing.T, text string) {
	t.Helper()
	for _, marker := range []string{"<tool_call", "</function>", "```json action", `{"tool"`, `"parameters"`} {
		if strings.Contains(text, marker) {
			t.Fatalf("raw marker %q leaked into the prose: %q", marker, text)
		}
	}
}

// native932AssertReadGoMod checks the one converted call every positive case
// in this matrix expects: read_file with the argument intact.
func native932AssertReadGoMod(t *testing.T, calls []hybrid931GotCall) {
	t.Helper()
	if len(calls) != 1 {
		t.Fatalf("tool calls = %d (%v), want exactly 1", len(calls), calls)
	}
	call := calls[0]
	if call.Name != "read_file" {
		t.Fatalf("call name = %q, want read_file", call.Name)
	}
	if call.ID == "" {
		t.Fatal("tool call id must be non-empty")
	}
	if call.Args["path"] != "go.mod" {
		t.Fatalf("read_file.path = %#v, want go.mod", call.Args["path"])
	}
}

// ---------- OpenAI /v1/chat/completions ----------

func TestNative932ChatNonStreamPureDialectCall(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(native932JSONBlock)})
	code, body := native932Post(t, proxy, "/v1/chat/completions", native932OpenAIBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	choice := resp.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", choice.FinishReason)
	}
	if strings.TrimSpace(choice.Message.Content) != "" {
		t.Fatalf("content = %q, want empty after the block was stripped", choice.Message.Content)
	}
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %d, want 1", len(choice.Message.ToolCalls))
	}
	tc := choice.Message.ToolCalls[0]
	if tc.Function.Name != "read_file" {
		t.Fatalf("function name = %q, want read_file", tc.Function.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments is not JSON: %v (%s)", err, tc.Function.Arguments)
	}
	if args["path"] != "go.mod" {
		t.Fatalf("arguments.path = %#v, want go.mod", args["path"])
	}
}

func TestNative932ChatStreamPureDialectCall(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(
		"```json action\n",
		`{"tool":"read_file",`,
		` "parameters":{"path":"go.mod"}}`,
		"\n```",
	)})
	code, body := native932Post(t, proxy, "/v1/chat/completions", native932OpenAIBody(t, true, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	stream := hybrid931ParseOpenAIStream(t, body)
	if stream.SawError {
		t.Fatalf("stream reported an error: %s", body)
	}
	if !stream.SawDone {
		t.Fatalf("the stream never sent [DONE]: %s", body)
	}
	if strings.TrimSpace(stream.Text) != "" {
		t.Fatalf("streamed text = %q, want no prose for a pure dialect turn", stream.Text)
	}
	native932AssertNoDialectLeak(t, stream.Text)
	native932AssertReadGoMod(t, stream.Calls)
	if len(stream.FinishReasons) == 0 || stream.FinishReasons[len(stream.FinishReasons)-1] != "tool_calls" {
		t.Fatalf("finish_reason sequence = %v, want it to end on tool_calls", stream.FinishReasons)
	}
}

func TestNative932ChatNonStreamMixedTextAndDialect(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(
		native932Head+"\n",
		native932JSONBlock,
		"\n"+native932Tail,
	)})
	code, body := native932Post(t, proxy, "/v1/chat/completions", native932OpenAIBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	message := resp.Choices[0].Message
	if strings.TrimSpace(message.Content) != native932Head+"\n"+native932Tail {
		t.Fatalf("content = %q, want the prose around the stripped block", message.Content)
	}
	native932AssertNoDialectLeak(t, message.Content)
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("tool_calls = %#v, want one read_file", message.ToolCalls)
	}
}

func TestNative932ChatStreamMixedTextAndDialect(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(
		native932Head+"\n",
		native932JSONBlock,
		"\n"+native932Tail,
	)})
	code, body := native932Post(t, proxy, "/v1/chat/completions", native932OpenAIBody(t, true, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	stream := hybrid931ParseOpenAIStream(t, body)
	if stream.SawError {
		t.Fatalf("stream reported an error: %s", body)
	}
	if got, want := strings.TrimSpace(stream.Text), native932Head+"\n"+native932Tail; got != want {
		t.Fatalf("streamed text = %q, want %q", got, want)
	}
	native932AssertNoDialectLeak(t, stream.Text)
	native932AssertReadGoMod(t, stream.Calls)
}

func TestNative932ChatUnknownToolStaysText(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(native932PostfixBlock)})
	code, body := native932Post(t, proxy, "/v1/chat/completions", native932OpenAIBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	var resp struct {
		Choices []struct {
			Message struct {
				Content   string          `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	choice := resp.Choices[0]
	if choice.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) > 0 && string(choice.Message.ToolCalls) != "null" {
		t.Fatalf("tool_calls = %s, want none for an undeclared tool name", choice.Message.ToolCalls)
	}
	if choice.Message.Content != native932PostfixBlock {
		t.Fatalf("content = %q, want the verbatim block: a name outside the declaration must stay prose", choice.Message.Content)
	}
}

func TestNative932ChatStreamUnknownToolStaysText(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(
		native932SplitLines(native932PostfixBlock)...,
	)})
	code, body := native932Post(t, proxy, "/v1/chat/completions", native932OpenAIBody(t, true, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	stream := hybrid931ParseOpenAIStream(t, body)
	if stream.SawError {
		t.Fatalf("stream reported an error: %s", body)
	}
	if len(stream.Calls) != 0 {
		t.Fatalf("tool_calls chunks = %v, want none for an undeclared tool name", stream.Calls)
	}
	if stream.Text != native932PostfixBlock {
		t.Fatalf("streamed text = %q, want the verbatim block", stream.Text)
	}
}

func TestNative932ChatToolChoiceNoneKeepsDialect(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(native932JSONBlock)})
	code, body := native932Post(t, proxy, "/v1/chat/completions", native932OpenAIBody(t, false, "none"))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	var resp struct {
		Choices []struct {
			Message struct {
				Content   string          `json:"content"`
				ToolCalls json.RawMessage `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	choice := resp.Choices[0]
	if choice.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) > 0 && string(choice.Message.ToolCalls) != "null" {
		t.Fatalf("tool_choice none must not convert: %s", choice.Message.ToolCalls)
	}
	if choice.Message.Content != native932JSONBlock {
		t.Fatalf("content = %q, want the verbatim block under tool_choice none", choice.Message.Content)
	}
}

// A convertible dialect call is already a legal native call: cue-sounding
// prose around it must not send the proxy into a second upstream round trip.
func TestNative932ChatConvertedCallSkipsRetryLoop(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(
		"让我读取文件：\n",
		native932JSONBlock,
	)})
	code, body := native932Post(t, proxy, "/v1/chat/completions", native932OpenAIBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	message := resp.Choices[0].Message
	if strings.TrimSpace(message.Content) != "让我读取文件：" {
		t.Fatalf("content = %q, want the prose without the block", message.Content)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("tool_calls = %#v, want one converted read_file", message.ToolCalls)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(message.ToolCalls[0].Function.Arguments), &args); err != nil {
		t.Fatalf("arguments is not JSON: %v", err)
	}
	if args["path"] != "go.mod" {
		t.Fatalf("arguments.path = %#v, want go.mod", args["path"])
	}
}

// A turn that carries a native call from the gateway plus a dialect call in
// the text must end with both: the conversion may not overwrite the native
// one, and the echo may not hide the dialect one.
func TestNative932ChatNativeAndDialectCallsMerge(t *testing.T) {
	script := native932Script{
		native932Head + "\n",
		native932JSONBlock,
		native932ToolCallFrame(t, "call_native_1", "write_file", `{"path":"out.txt","content":"hi"}`),
	}
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{script})
	code, body := native932Post(t, proxy, "/v1/chat/completions", native932OpenAIBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	message := resp.Choices[0].Message
	if strings.TrimSpace(message.Content) != native932Head {
		t.Fatalf("content = %q, want the prose with the block stripped", message.Content)
	}
	if len(message.ToolCalls) != 2 {
		t.Fatalf("tool_calls = %d (%#v), want the native and the converted call", len(message.ToolCalls), message.ToolCalls)
	}
	if message.ToolCalls[0].ID != "call_native_1" || message.ToolCalls[0].Function.Name != "write_file" {
		t.Fatalf("first call = %#v, want the gateway's native write_file kept first", message.ToolCalls[0])
	}
	if message.ToolCalls[1].Function.Name != "read_file" {
		t.Fatalf("second call = %#v, want the dialect-converted read_file appended", message.ToolCalls[1])
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(message.ToolCalls[1].Function.Arguments), &args); err != nil {
		t.Fatalf("converted arguments is not JSON: %v", err)
	}
	if args["path"] != "go.mod" {
		t.Fatalf("converted arguments.path = %#v, want go.mod", args["path"])
	}
}

// ---------- Anthropic /v1/messages ----------

func TestNative932AnthropicNonStreamPureDialectCall(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(native932JSONBlock)})
	code, body := native932Post(t, proxy, "/v1/messages", native932AnthropicBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	var msg struct {
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			ID    string         `json:"id"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	if msg.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "tool_use" {
		t.Fatalf("content = %#v, want a single tool_use block", msg.Content)
	}
	block := msg.Content[0]
	if block.Name != "read_file" || block.Input["path"] != "go.mod" || block.ID == "" {
		t.Fatalf("tool_use = %s %v %q, want read_file go.mod with an id", block.Name, block.Input, block.ID)
	}
}

func TestNative932AnthropicStreamPureDialectCall(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(
		"```json action\n",
		`{"tool":"read_file",`,
		` "parameters":{"path":"go.mod"}}`,
		"\n```",
	)})
	code, body := native932Post(t, proxy, "/v1/messages", native932AnthropicBody(t, true, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	stream := hybrid931ParseAnthropicStream(t, body)
	if stream.SawError {
		t.Fatalf("stream reported an error: %s", body)
	}
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("the stream never stopped: %s", body)
	}
	if stream.Stop != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", stream.Stop)
	}
	if strings.TrimSpace(stream.Text) != "" {
		t.Fatalf("streamed text = %q, want no prose for a pure dialect turn", stream.Text)
	}
	native932AssertReadGoMod(t, stream.Calls)
}

func TestNative932AnthropicNonStreamMixedTextAndDialect(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(
		native932Head+"\n",
		native932JSONBlock,
		"\n"+native932Tail,
	)})
	code, body := native932Post(t, proxy, "/v1/messages", native932AnthropicBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	var msg struct {
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	if msg.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", msg.StopReason)
	}
	if len(msg.Content) != 2 {
		t.Fatalf("content blocks = %d, want text + tool_use", len(msg.Content))
	}
	if msg.Content[0].Type != "text" || strings.TrimSpace(msg.Content[0].Text) != native932Head+"\n"+native932Tail {
		t.Fatalf("text block = %#v, want the prose around the stripped block", msg.Content[0])
	}
	native932AssertNoDialectLeak(t, msg.Content[0].Text)
	if msg.Content[1].Type != "tool_use" || msg.Content[1].Name != "read_file" || msg.Content[1].Input["path"] != "go.mod" {
		t.Fatalf("tool_use block = %#v, want read_file go.mod", msg.Content[1])
	}
}

func TestNative932AnthropicUnknownToolStaysText(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(native932PostfixBlock)})
	code, body := native932Post(t, proxy, "/v1/messages", native932AnthropicBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	if msg.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" {
		t.Fatalf("content = %#v, want a single text block", msg.Content)
	}
	if msg.Content[0].Text != native932PostfixBlock {
		t.Fatalf("text = %q, want the verbatim block: a name outside the declaration must stay prose", msg.Content[0].Text)
	}
}

func TestNative932AnthropicToolChoiceNoneKeepsDialect(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(native932JSONBlock)})
	code, body := native932Post(t, proxy, "/v1/messages", native932AnthropicBody(t, false, map[string]any{"type": "none"}))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	if msg.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" {
		t.Fatalf("content = %#v, want a single text block", msg.Content)
	}
	if msg.Content[0].Text != native932JSONBlock {
		t.Fatalf("text = %q, want the verbatim block under tool_choice none", msg.Content[0].Text)
	}
}

// ---------- OpenAI /v1/responses ----------

// native932ResponsesOutput reads the output array out of a non-streaming
// response body or the response.completed event of a stream.
type native932ResponsesOutput struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	CallID    string `json:"call_id"`
	Arguments string `json:"arguments"`
	Status    string `json:"status"`
}

func native932ParseResponsesOutput(t *testing.T, body string) (text string, calls []hybrid931GotCall) {
	t.Helper()
	var completed map[string]any
	if strings.Contains(body, "response.completed") {
		for _, block := range strings.Split(body, "\n\n") {
			var name, data string
			for _, line := range strings.Split(block, "\n") {
				if strings.HasPrefix(line, "event: ") {
					name = strings.TrimPrefix(line, "event: ")
				} else if strings.HasPrefix(line, "data: ") {
					data = strings.TrimPrefix(line, "data: ")
				}
			}
			if name != "response.completed" {
				continue
			}
			if err := json.Unmarshal([]byte(data), &completed); err != nil {
				t.Fatalf("response.completed is not JSON: %v (%s)", err, data)
			}
			break
		}
		if completed == nil {
			t.Fatalf("no response.completed event found: %s", body)
		}
		response, _ := completed["response"].(map[string]any)
		if response == nil {
			t.Fatalf("response.completed carries no response object: %s", body)
		}
		text, _ = response["output_text"].(string)
		items, _ := response["output"].([]any)
		for _, item := range items {
			m, _ := item.(map[string]any)
			if m == nil {
				continue
			}
			if m["type"] == "function_call" {
				args, _ := m["arguments"].(string)
				var parsed map[string]any
				if err := json.Unmarshal([]byte(args), &parsed); err != nil {
					t.Fatalf("function_call arguments is not JSON: %v (%s)", err, args)
				}
				id, _ := m["call_id"].(string)
				name, _ := m["name"].(string)
				calls = append(calls, hybrid931GotCall{ID: id, Name: name, Args: parsed})
			}
		}
		return text, calls
	}
	var resp struct {
		Output     []native932ResponsesOutput `json:"output"`
		OutputText string                     `json:"output_text"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	for _, item := range resp.Output {
		if item.Type != "function_call" {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(item.Arguments), &parsed); err != nil {
			t.Fatalf("function_call arguments is not JSON: %v (%s)", err, item.Arguments)
		}
		calls = append(calls, hybrid931GotCall{ID: item.CallID, Name: item.Name, Args: parsed})
	}
	return resp.OutputText, calls
}

func TestNative932ResponsesNonStreamPureDialectCall(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(native932JSONBlock)})
	code, body := native932Post(t, proxy, "/v1/responses", native932ResponsesBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	text, calls := native932ParseResponsesOutput(t, body)
	if strings.TrimSpace(text) != "" {
		t.Fatalf("output_text = %q, want empty after the block was stripped", text)
	}
	native932AssertReadGoMod(t, calls)
}

func TestNative932ResponsesStreamPureDialectCall(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(
		"```json action\n",
		`{"tool":"read_file",`,
		` "parameters":{"path":"go.mod"}}`,
		"\n```",
	)})
	code, body := native932Post(t, proxy, "/v1/responses", native932ResponsesBody(t, true, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("the stream never completed: %s", body)
	}
	var leaked strings.Builder
	for _, block := range strings.Split(body, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"response.output_text.delta"`) {
				var payload struct {
					Delta string `json:"delta"`
				}
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err == nil {
					leaked.WriteString(payload.Delta)
				}
			}
		}
	}
	if strings.TrimSpace(leaked.String()) != "" {
		t.Fatalf("output_text deltas carried %q, want no prose for a pure dialect turn", leaked.String())
	}
	native932AssertNoDialectLeak(t, leaked.String())
	_, calls := native932ParseResponsesOutput(t, body)
	native932AssertReadGoMod(t, calls)
}

func TestNative932ResponsesMixedTextAndDialect(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(
		native932Head+"\n",
		native932JSONBlock,
		"\n"+native932Tail,
	)})
	code, body := native932Post(t, proxy, "/v1/responses", native932ResponsesBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	text, calls := native932ParseResponsesOutput(t, body)
	if strings.TrimSpace(text) != native932Head+"\n"+native932Tail {
		t.Fatalf("output_text = %q, want the prose around the stripped block", text)
	}
	native932AssertNoDialectLeak(t, text)
	native932AssertReadGoMod(t, calls)
}

func TestNative932ResponsesUnknownToolStaysText(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(native932PostfixBlock)})
	code, body := native932Post(t, proxy, "/v1/responses", native932ResponsesBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	text, calls := native932ParseResponsesOutput(t, body)
	if len(calls) != 0 {
		t.Fatalf("function_call items = %v, want none for an undeclared tool name", calls)
	}
	if text != native932PostfixBlock {
		t.Fatalf("output_text = %q, want the verbatim block: a name outside the declaration must stay prose", text)
	}
}

func TestNative932ResponsesToolChoiceNoneKeepsDialect(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(native932JSONBlock)})
	code, body := native932Post(t, proxy, "/v1/responses", native932ResponsesBody(t, false, "none"))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	text, calls := native932ParseResponsesOutput(t, body)
	if len(calls) != 0 {
		t.Fatalf("tool_choice none must not convert: %v", calls)
	}
	if text != native932JSONBlock {
		t.Fatalf("output_text = %q, want the verbatim block under tool_choice none", text)
	}
}

// ---------- real-leak regressions ----------

// Byte-exact second wrapper of the mmx turn-2 leak (Anthropic native tools,
// 2026-09-30): a complete dialect call to the declared read_file must reach
// the client as a tool_use block, not as content text.
func TestNative932AnthropicMMXLeakBlockConverts(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(native932MMXBlock2)})
	code, body := native932Post(t, proxy, "/v1/messages", native932AnthropicBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	var msg struct {
		Content []struct {
			Type  string         `json:"type"`
			Text  string         `json:"text"`
			Name  string         `json:"name"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	if msg.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "tool_use" {
		t.Fatalf("content = %#v, want a single tool_use block", msg.Content)
	}
	block := msg.Content[0]
	if block.Name != "read_file" || block.Input["path"] != "tokenizer.go" {
		t.Fatalf("tool_use = %s %v, want read_file with path tokenizer.go", block.Name, block.Input)
	}
	native932AssertNoDialectLeak(t, block.Text)
}

// Byte-exact full text of the mmx turn-2 leak: the first wrapper lost its
// function header and by the parser's unclosed-parent conservatism owns the
// rest of the turn, so the proxy must hand the text through verbatim and
// invent no call.
func TestNative932AnthropicMMXTurn2MalformedStaysText(t *testing.T) {
	proxy, upstreamCalls := native932NewEnv(t, []native932Script{native932ContentFrames(native932MMXTurn2)})
	code, body := native932Post(t, proxy, "/v1/messages", native932AnthropicBody(t, false, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	if msg.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" {
		t.Fatalf("content = %#v, want a single text block", msg.Content)
	}
	if msg.Content[0].Text != native932MMXTurn2 {
		t.Fatalf("text = %q, want the verbatim turn: nothing parseable, nothing invented", msg.Content[0].Text)
	}
}

// Byte-exact leak of postfix-cn-chat-sse.json: the wrapper names Read while
// the request declared read_fixture with tool_choice required. The untrusted
// wrapper must never reach the client nor become a call; the forced-tooling
// retry answers with a clean block, and that call is what the client gets.
func TestNative932ChatPostfixLeakRecoversViaRetry(t *testing.T) {
	requestBody, err := json.Marshal(map[string]any{
		"model": hybrid931Model,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": "Read both local fixtures with read_fixture before answering.",
		}},
		"tools": []any{map[string]any{"type": "function", "function": map[string]any{
			"name":        "read_fixture",
			"description": "Read a local acceptance fixture file. Returns the actual file contents.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"path": map[string]any{"type": "string"}},
				"required":   []any{"path"},
			},
		}}},
		"tool_choice": "required",
	})
	if err != nil {
		t.Fatal(err)
	}

	proxy, upstreamCalls := native932NewEnv(t, []native932Script{
		native932ContentFrames(native932PostfixBlock),
		native932ContentFrames("```json action\n" + `{"tool":"read_fixture","parameters":{"path":"postfix-cn-chat-sse-alpha.txt"}}` + "\n```"),
	})
	code, body := native932Post(t, proxy, "/v1/chat/completions", requestBody)
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	if n := upstreamCalls.Load(); n != 2 {
		t.Fatalf("upstream answered %d turns, want 2 (the leak plus the forced-tooling retry)", n)
	}

	var resp struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, body)
	}
	choice := resp.Choices[0]
	if choice.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", choice.FinishReason)
	}
	if strings.TrimSpace(choice.Message.Content) != "" {
		t.Fatalf("content = %q, want the replaced attempt's text gone", choice.Message.Content)
	}
	native932AssertNoDialectLeak(t, choice.Message.Content)
	if len(choice.Message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %#v, want exactly the retry's call", choice.Message.ToolCalls)
	}
	tc := choice.Message.ToolCalls[0]
	if tc.Function.Name != "read_fixture" {
		t.Fatalf("function name = %q, want read_fixture", tc.Function.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		t.Fatalf("arguments is not JSON: %v (%s)", err, tc.Function.Arguments)
	}
	if args["path"] != "postfix-cn-chat-sse-alpha.txt" {
		t.Fatalf("arguments.path = %#v, want postfix-cn-chat-sse-alpha.txt", args["path"])
	}
}
