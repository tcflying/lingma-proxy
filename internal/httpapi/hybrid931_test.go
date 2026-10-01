package httpapi

import (
	"bytes"
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

// 931 interface-level regression matrix: a stub Lingma gateway answers every
// turn, so the whole chain (remote client -> service tool emulation -> HTTP
// handler) runs isolated from the real cloud and the production 10095 port.
//
// The stubbed model turn is the "dual hybrid call" shape from the incident:
// two <tool_call> wrappers whose JSON bodies carry the calls, closed by
// hallucinated </result> / </think> junk instead of the dialect's own close
// tag. Tags are assembled by concatenation, following toolemulation.go, so
// this source does not itself look like a tool call in an agent transcript.
const (
	hybrid931ProseHead = "先读入口文件，再按符号搜一遍。"
	hybrid931ProseTail = "两步都做完再下结论。"
	// hybrid931ExpectedClean is what must survive after both spans are
	// consumed: the head's newline stays because it precedes the first opening.
	hybrid931ExpectedClean = hybrid931ProseHead + "\n" + hybrid931ProseTail
	hybrid931Model         = "kmodel"
	hybrid931UserTurn      = "检查 hybrid931 的两处调用。"

	hybrid931ReadPath    = "src/hybrid931/app.go"
	hybrid931GrepPattern = "CloseMarkerSpan"
	hybrid931GrepPath    = "src/hybrid931"
)

// hybrid931DualCallPieces is the model turn split at meaningful boundaries:
// fences, JSON seams and junk lines each land on their own SSE delta, so the
// streaming filter has to hold and release across splits, never see a whole
// block in one push.
var hybrid931DualCallPieces = []string{
	hybrid931ProseHead + "\n",
	"<" + "tool_call" + ">\n",
	`{"tool":"Read",`,
	` "parameters": {"path": "src/hybrid931/app.go",`,
	` "offset": 0, "limit": 64}}`,
	"\n</" + "result" + ">\n",
	"<" + "tool_call" + ">\n",
	`{"tool":"Grep","parameters":`,
	`{"pattern":"CloseMarkerSpan","path":"src/hybrid931"}}`,
	"\n</" + "think" + ">\n",
	hybrid931ProseTail,
}

var hybrid931DualCallTurn = strings.Join(hybrid931DualCallPieces, "")

// hybrid931RunePieces splits the same turn one rune per delta, the worst-case
// chunking a stream filter can face: every tag, brace and quote arrives torn.
func hybrid931RunePieces() []string {
	runes := []rune(hybrid931DualCallTurn)
	out := make([]string, 0, len(runes))
	for _, r := range runes {
		out = append(out, string(r))
	}
	return out
}

// hybrid931RawFrame envelops one OpenAI-style chunk the way the Lingma gateway
// does: the frame's body is the JSON text of the chunk.
func hybrid931RawFrame(t *testing.T, inner any) string {
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

func hybrid931ContentFrame(t *testing.T, content string) string {
	t.Helper()
	return hybrid931RawFrame(t, map[string]any{
		"choices": []any{map[string]any{
			"index": 0,
			"delta": map[string]any{"content": content},
		}},
	})
}

func hybrid931FinishFrame(t *testing.T) string {
	t.Helper()
	return hybrid931RawFrame(t, map[string]any{
		"choices": []any{map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 11, "completion_tokens": 29},
	})
}

// hybrid931NewEnv wires the isolated stack: stub gateway (fixed strings only,
// no command execution) -> remote backend service -> proxy HTTP handler. The
// fake credentials live in a temp dir, an explicit RemoteBaseURL keeps the
// client off the login-cache/config scan, and cleared proxy env keeps the
// remote client dialling the loopback stub itself.
func hybrid931NewEnv(t *testing.T, aggregate string, pieces []string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	t.Setenv("LINGMA_AGGREGATE_TOOL_STREAM", aggregate)
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		if r.URL.Path != "/algo/api/v2/service/pro/sse/agent_chat_generation" {
			t.Errorf("upstream path = %q, want the chat generation endpoint", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, piece := range pieces {
			if _, err := io.WriteString(w, hybrid931ContentFrame(t, piece)); err != nil {
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

func hybrid931ReadTool() map[string]any {
	return map[string]any{
		"name":        "Read",
		"description": "read part of a file",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string"},
				"offset": map[string]any{"type": "integer"},
				"limit":  map[string]any{"type": "integer"},
			},
			"required": []any{"path"},
		},
	}
}

func hybrid931GrepTool() map[string]any {
	return map[string]any{
		"name":        "Grep",
		"description": "search files for a pattern",
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern": map[string]any{"type": "string"},
				"path":    map[string]any{"type": "string"},
			},
			"required": []any{"pattern", "path"},
		},
	}
}

func hybrid931AnthropicBody(t *testing.T, stream bool, toolChoice any) []byte {
	t.Helper()
	body := map[string]any{
		"model":      hybrid931Model,
		"max_tokens": 1024,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": hybrid931UserTurn}},
		}},
		"tools": []any{hybrid931ReadTool(), hybrid931GrepTool()},
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

func hybrid931OpenAIBody(t *testing.T, stream bool, toolChoice any) []byte {
	t.Helper()
	body := map[string]any{
		"model": hybrid931Model,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": hybrid931UserTurn,
		}},
		"tools": []any{
			map[string]any{"type": "function", "function": map[string]any{
				"name":        "Read",
				"description": "read part of a file",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":   map[string]any{"type": "string"},
						"offset": map[string]any{"type": "integer"},
						"limit":  map[string]any{"type": "integer"},
					},
					"required": []any{"path"},
				},
			}},
			map[string]any{"type": "function", "function": map[string]any{
				"name":        "Grep",
				"description": "search files for a pattern",
				"parameters": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"pattern": map[string]any{"type": "string"},
						"path":    map[string]any{"type": "string"},
					},
					"required": []any{"pattern", "path"},
				},
			}},
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

func hybrid931Post(t *testing.T, proxy *httptest.Server, path string, body []byte) (int, string) {
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

// hybrid931GotCall is the wire form of one tool call, normalised across the
// Anthropic and OpenAI shapes so both matrices share the assertions.
type hybrid931GotCall struct {
	ID   string
	Name string
	Args map[string]any
}

// hybrid931AssertDualCalls checks the structured outcome of the dual hybrid
// turn: exactly two calls, complete arguments (numbers included), in order.
func hybrid931AssertDualCalls(t *testing.T, calls []hybrid931GotCall) {
	t.Helper()
	if len(calls) != 2 {
		t.Fatalf("tool calls = %d (%v), want exactly 2", len(calls), calls)
	}
	read, grep := calls[0], calls[1]
	if read.Name != "Read" || grep.Name != "Grep" {
		t.Fatalf("call order/names = %q, %q, want Read then Grep", read.Name, grep.Name)
	}
	if read.ID == "" || grep.ID == "" {
		t.Fatalf("tool call ids must be non-empty: %q, %q", read.ID, grep.ID)
	}
	if read.Args["path"] != hybrid931ReadPath {
		t.Errorf("Read.path = %#v, want %q", read.Args["path"], hybrid931ReadPath)
	}
	if read.Args["offset"] != float64(0) || read.Args["limit"] != float64(64) {
		t.Errorf("Read offset/limit = %#v/%#v, want 0/64 as JSON numbers", read.Args["offset"], read.Args["limit"])
	}
	if grep.Args["pattern"] != hybrid931GrepPattern || grep.Args["path"] != hybrid931GrepPath {
		t.Errorf("Grep pattern/path = %#v/%#v, want %q/%q", grep.Args["pattern"], grep.Args["path"], hybrid931GrepPattern, hybrid931GrepPath)
	}
}

// hybrid931AssertCleanText checks the prose that survives the two consumed
// spans: exactly the head and tail, with no raw marker or action JSON left.
func hybrid931AssertCleanText(t *testing.T, text string) {
	t.Helper()
	if text != hybrid931ExpectedClean {
		t.Fatalf("surviving text = %q, want %q", text, hybrid931ExpectedClean)
	}
	for _, marker := range []string{"<tool_call", "</result>", "</think>", `{"tool"`, `"parameters"`, "```"} {
		if strings.Contains(text, marker) {
			t.Fatalf("raw marker %q leaked into the prose: %q", marker, text)
		}
	}
}

func hybrid931AssertOneUpstreamTurn(t *testing.T, upstreamCalls *atomic.Int32) {
	t.Helper()
	if n := upstreamCalls.Load(); n != 1 {
		t.Fatalf("upstream answered %d turns, want 1 (fixture tripped a retry path)", n)
	}
}

// ---------- Anthropic /v1/messages ----------

func TestHybrid931AnthropicMessagesNonStreamDualHybridCalls(t *testing.T) {
	proxy, upstreamCalls := hybrid931NewEnv(t, "", hybrid931DualCallPieces)
	code, body := hybrid931Post(t, proxy, "/v1/messages", hybrid931AnthropicBody(t, false, nil))
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
	if len(msg.Content) != 3 {
		t.Fatalf("content blocks = %d, want text + 2 tool_use", len(msg.Content))
	}
	if msg.Content[0].Type != "text" {
		t.Fatalf("first block type = %q, want text", msg.Content[0].Type)
	}
	hybrid931AssertCleanText(t, msg.Content[0].Text)
	calls := make([]hybrid931GotCall, 0, 2)
	for _, block := range msg.Content[1:] {
		if block.Type != "tool_use" {
			t.Fatalf("block type = %q, want tool_use", block.Type)
		}
		calls = append(calls, hybrid931GotCall{ID: block.ID, Name: block.Name, Args: block.Input})
	}
	hybrid931AssertDualCalls(t, calls)
}

// hybrid931AnthropicEvent decodes the delta payloads of the Anthropic stream
// events this matrix cares about.
type hybrid931AnthropicEvent struct {
	Type         string `json:"type"`
	Index        int    `json:"index"`
	ContentBlock *struct {
		Type string `json:"type"`
		Name string `json:"name"`
		ID   string `json:"id"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
}

type hybrid931AnthropicStream struct {
	Text     string
	Calls    []hybrid931GotCall
	Stop     string
	SawError bool
}

func hybrid931ParseAnthropicStream(t *testing.T, body string) *hybrid931AnthropicStream {
	t.Helper()
	out := &hybrid931AnthropicStream{}
	var currentTool *hybrid931GotCall
	var text strings.Builder
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if !strings.HasPrefix(line, "data: ") {
			if strings.HasPrefix(line, "event: error") {
				out.SawError = true
			}
			continue
		}
		var event hybrid931AnthropicEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatalf("event payload is not JSON: %v\nline %d: %s", err, i, line)
		}
		switch event.Type {
		case "content_block_start":
			if event.ContentBlock != nil && event.ContentBlock.Type == "tool_use" {
				currentTool = &hybrid931GotCall{ID: event.ContentBlock.ID, Name: event.ContentBlock.Name, Args: map[string]any{}}
			}
		case "content_block_delta":
			if event.Delta == nil {
				continue
			}
			switch event.Delta.Type {
			case "text_delta":
				text.WriteString(event.Delta.Text)
			case "input_json_delta":
				if currentTool == nil {
					t.Fatalf("input_json_delta without an open tool_use block: %s", line)
				}
				var args map[string]any
				if err := json.Unmarshal([]byte(event.Delta.PartialJSON), &args); err != nil {
					t.Fatalf("partial_json is not a JSON object: %v\n%s", err, event.Delta.PartialJSON)
				}
				currentTool.Args = args
				out.Calls = append(out.Calls, *currentTool)
				currentTool = nil
			}
		case "message_delta":
			if event.Delta != nil {
				out.Stop = event.Delta.StopReason
			}
		case "error":
			out.SawError = true
		}
	}
	out.Text = text.String()
	return out
}

func TestHybrid931AnthropicMessagesStreamDualHybridCalls(t *testing.T) {
	proxy, upstreamCalls := hybrid931NewEnv(t, "", hybrid931DualCallPieces)
	code, body := hybrid931Post(t, proxy, "/v1/messages", hybrid931AnthropicBody(t, true, nil))
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
	hybrid931AssertCleanText(t, stream.Text)
	hybrid931AssertDualCalls(t, stream.Calls)
}

func TestHybrid931AnthropicMessagesStreamAggregateDualHybridCalls(t *testing.T) {
	proxy, upstreamCalls := hybrid931NewEnv(t, "1", hybrid931DualCallPieces)
	code, body := hybrid931Post(t, proxy, "/v1/messages", hybrid931AnthropicBody(t, true, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)

	stream := hybrid931ParseAnthropicStream(t, body)
	if stream.SawError {
		t.Fatalf("stream reported an error: %s", body)
	}
	if stream.Stop != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", stream.Stop)
	}
	hybrid931AssertCleanText(t, stream.Text)
	hybrid931AssertDualCalls(t, stream.Calls)
}

// ---------- OpenAI /v1/chat/completions ----------

func TestHybrid931OpenAIChatNonStreamDualHybridCalls(t *testing.T) {
	proxy, upstreamCalls := hybrid931NewEnv(t, "", hybrid931DualCallPieces)
	code, body := hybrid931Post(t, proxy, "/v1/chat/completions", hybrid931OpenAIBody(t, false, nil))
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
	hybrid931AssertCleanText(t, choice.Message.Content)
	calls := make([]hybrid931GotCall, 0, 2)
	for _, tc := range choice.Message.ToolCalls {
		var args map[string]any
		if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
			t.Fatalf("arguments are not a JSON object: %v\n%s", err, tc.Function.Arguments)
		}
		calls = append(calls, hybrid931GotCall{ID: tc.ID, Name: tc.Function.Name, Args: args})
	}
	hybrid931AssertDualCalls(t, calls)
}

// hybrid931OpenAIChunk decodes one chat.completion.chunk of the OpenAI stream.
type hybrid931OpenAIChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type hybrid931OpenAIStream struct {
	Text          string
	Calls         []hybrid931GotCall
	FinishReasons []string
	SawError      bool
	SawDone       bool
}

func hybrid931ParseOpenAIStream(t *testing.T, body string) *hybrid931OpenAIStream {
	t.Helper()
	out := &hybrid931OpenAIStream{}
	var text strings.Builder
	callsByIndex := map[int]*hybrid931GotCall{}
	var order []int
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			out.SawDone = true
			continue
		}
		var chunk hybrid931OpenAIChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %v\n%s", err, payload)
		}
		if chunk.Error != nil {
			out.SawError = true
			t.Errorf("stream error: %s", chunk.Error.Message)
		}
		for _, choice := range chunk.Choices {
			text.WriteString(choice.Delta.Content)
			if choice.FinishReason != nil {
				out.FinishReasons = append(out.FinishReasons, *choice.FinishReason)
			}
			for _, tc := range choice.Delta.ToolCalls {
				call := callsByIndex[tc.Index]
				if call == nil {
					call = &hybrid931GotCall{Args: map[string]any{}}
					callsByIndex[tc.Index] = call
					order = append(order, tc.Index)
				}
				if tc.ID != "" {
					call.ID = tc.ID
				}
				if tc.Function.Name != "" {
					call.Name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					var args map[string]any
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
						t.Fatalf("streamed arguments are not a JSON object: %v\n%s", err, tc.Function.Arguments)
					}
					call.Args = args
				}
			}
		}
	}
	out.Text = text.String()
	for _, index := range order {
		out.Calls = append(out.Calls, *callsByIndex[index])
	}
	return out
}

func hybrid931AssertOpenAIDualStream(t *testing.T, stream *hybrid931OpenAIStream, rawBody string) {
	t.Helper()
	if stream.SawError {
		t.Fatalf("stream reported an error: %s", rawBody)
	}
	if !stream.SawDone {
		t.Fatalf("the stream never sent [DONE]: %s", rawBody)
	}
	if len(stream.FinishReasons) == 0 || stream.FinishReasons[len(stream.FinishReasons)-1] != "tool_calls" {
		t.Fatalf("finish_reason sequence = %v, want it to end on tool_calls", stream.FinishReasons)
	}
	hybrid931AssertCleanText(t, stream.Text)
	hybrid931AssertDualCalls(t, stream.Calls)
}

func TestHybrid931OpenAIChatStreamDualHybridCalls(t *testing.T) {
	proxy, upstreamCalls := hybrid931NewEnv(t, "", hybrid931DualCallPieces)
	code, body := hybrid931Post(t, proxy, "/v1/chat/completions", hybrid931OpenAIBody(t, true, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)
	hybrid931AssertOpenAIDualStream(t, hybrid931ParseOpenAIStream(t, body), body)
}

func TestHybrid931OpenAIChatStreamAggregateDualHybridCalls(t *testing.T) {
	proxy, upstreamCalls := hybrid931NewEnv(t, "1", hybrid931DualCallPieces)
	code, body := hybrid931Post(t, proxy, "/v1/chat/completions", hybrid931OpenAIBody(t, true, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)
	hybrid931AssertOpenAIDualStream(t, hybrid931ParseOpenAIStream(t, body), body)
}

// TestHybrid931OpenAIChatStreamRuneSplitDualHybridCalls feeds the same turn
// one rune per upstream delta: every tag, brace and quote is torn across
// pushes, which is the streaming state machine's worst case.
func TestHybrid931OpenAIChatStreamRuneSplitDualHybridCalls(t *testing.T) {
	proxy, upstreamCalls := hybrid931NewEnv(t, "", hybrid931RunePieces())
	code, body := hybrid931Post(t, proxy, "/v1/chat/completions", hybrid931OpenAIBody(t, true, nil))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	hybrid931AssertOneUpstreamTurn(t, upstreamCalls)
	hybrid931AssertOpenAIDualStream(t, hybrid931ParseOpenAIStream(t, body), body)
}

// ---------- tool_choice:none passthrough ----------

// With the client forbidding tool calls in each protocol's own shape — the
// string "none" for OpenAI, the object {"type":"none"} for Anthropic — the
// same model turn must pass through verbatim: no structured call, no marker
// removed, and the natural end_turn / stop termination.
func TestHybrid931AnthropicToolChoiceNoneObjectPassesTurnThrough(t *testing.T) {
	proxy, upstreamCalls := hybrid931NewEnv(t, "", hybrid931DualCallPieces)
	toolChoice := map[string]any{"type": "none"}
	code, body := hybrid931Post(t, proxy, "/v1/messages", hybrid931AnthropicBody(t, false, toolChoice))
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
	if msg.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", msg.StopReason)
	}
	if len(msg.Content) != 1 || msg.Content[0].Type != "text" {
		t.Fatalf("content = %#v, want a single text block", msg.Content)
	}
	got := msg.Content[0].Text
	if got != hybrid931DualCallTurn {
		t.Fatalf("text = %q, want the verbatim turn %q", got, hybrid931DualCallTurn)
	}
	for _, marker := range []string{"<tool_call", "</result>", "</think>", `{"tool"`} {
		if !strings.Contains(got, marker) {
			t.Fatalf("tool_choice:none must pass %q through untouched", marker)
		}
	}
}

func TestHybrid931OpenAIToolChoiceNoneStringPassesTurnThrough(t *testing.T) {
	proxy, upstreamCalls := hybrid931NewEnv(t, "", hybrid931DualCallPieces)
	code, body := hybrid931Post(t, proxy, "/v1/chat/completions", hybrid931OpenAIBody(t, false, "none"))
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
	if len(resp.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(resp.Choices))
	}
	choice := resp.Choices[0]
	if choice.FinishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", choice.FinishReason)
	}
	if len(choice.Message.ToolCalls) > 0 && string(choice.Message.ToolCalls) != "null" {
		t.Fatalf("tool_calls = %s, want none", choice.Message.ToolCalls)
	}
	if choice.Message.Content != hybrid931DualCallTurn {
		t.Fatalf("content = %q, want the verbatim turn %q", choice.Message.Content, hybrid931DualCallTurn)
	}
	if !strings.Contains(choice.Message.Content, "<tool_call") {
		t.Fatal("tool_choice:none must pass the raw block through untouched")
	}
}

func TestHybrid931OpenAIStreamToolChoiceNoneStringPassesTurnThrough(t *testing.T) {
	proxy, upstreamCalls := hybrid931NewEnv(t, "", hybrid931DualCallPieces)
	code, body := hybrid931Post(t, proxy, "/v1/chat/completions", hybrid931OpenAIBody(t, true, "none"))
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
	if len(stream.Calls) != 0 {
		t.Fatalf("tool_calls chunks = %v, want none", stream.Calls)
	}
	if len(stream.FinishReasons) == 0 || stream.FinishReasons[len(stream.FinishReasons)-1] != "stop" {
		t.Fatalf("finish_reason sequence = %v, want it to end on stop", stream.FinishReasons)
	}
	if stream.Text != hybrid931DualCallTurn {
		t.Fatalf("streamed text = %q, want the verbatim turn %q", stream.Text, hybrid931DualCallTurn)
	}
}
