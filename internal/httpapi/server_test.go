package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"lingma-ipc-proxy/internal/remote"
	"lingma-ipc-proxy/internal/service"
	"lingma-ipc-proxy/internal/toolemulation"
)

func TestNormalizeOpenAIRequestCollectsSystemMessages(t *testing.T) {
	req := openAIChatRequest{
		Model: "test-model",
		Messages: []rawMessage{
			{Role: "system", Content: "You are concise."},
			{Role: "user", Content: "Hello"},
			{Role: "assistant", Content: "Hi"},
			{Role: "system", Content: "Answer in Chinese."},
			{Role: "tool", Content: "ignored"},
			{Role: "user", Content: []any{
				map[string]any{"type": "text", "text": "Follow up"},
			}},
		},
	}

	normalized, err := normalizeOpenAIRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("normalizeOpenAIRequest() error = %v", err)
	}
	if normalized.Model != "test-model" {
		t.Fatalf("model = %q", normalized.Model)
	}
	if normalized.System != "You are concise.\n\nAnswer in Chinese." {
		t.Fatalf("system = %q", normalized.System)
	}
	if len(normalized.Messages) != 3 {
		t.Fatalf("message count = %d", len(normalized.Messages))
	}
	if normalized.Messages[0].Role != "user" || normalized.Messages[0].Text != "Hello" {
		t.Fatalf("first message = %+v", normalized.Messages[0])
	}
	if normalized.Messages[1].Role != "assistant" || normalized.Messages[1].Text != "Hi" {
		t.Fatalf("second message = %+v", normalized.Messages[1])
	}
	if normalized.Messages[2].Role != "user" || normalized.Messages[2].Text != "Follow up" {
		t.Fatalf("third message = %+v", normalized.Messages[2])
	}
}

func TestCapabilitiesAdvertiseAgentCompatibility(t *testing.T) {
	server := NewServer("", service.New(service.Config{
		Model:   "Qwen3-Coder",
		Timeout: time.Second,
	}))

	req := httptest.NewRequest(http.MethodGet, "/capabilities", nil)
	rec := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	features, ok := body["features"].(map[string]any)
	if !ok {
		t.Fatalf("missing features: %#v", body)
	}
	for _, key := range []string{"tools", "tool_alias_mapping", "images", "local_image_paths", "image_auto_resize"} {
		if features[key] != true {
			t.Fatalf("feature %s = %#v", key, features[key])
		}
	}
	protocols, ok := body["protocols"].([]any)
	if !ok {
		t.Fatalf("missing protocols: %#v", body)
	}
	foundResponses := false
	for _, item := range protocols {
		if item == "openai.responses" {
			foundResponses = true
			break
		}
	}
	if !foundResponses {
		t.Fatalf("protocols = %#v", protocols)
	}
}

func TestDebugEndpointsAreLoopbackOnlyUnlessOptedIn(t *testing.T) {
	server := NewServer("", service.New(service.Config{
		Model:   "Qwen3-Coder",
		Timeout: time.Second,
	}))
	serve := func(path, remoteAddr string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.RemoteAddr = remoteAddr
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, req)
		return rec.Code
	}

	for _, path := range []string{"/debug/requests", "/debug/app-logs", "/api/requests", "/api/logs"} {
		if code := serve(path, "192.168.50.7:54321"); code != http.StatusForbidden {
			t.Fatalf("%s from a LAN peer = %d, want 403", path, code)
		}
		if code := serve(path, "[::1]:54321"); code == http.StatusForbidden {
			t.Fatalf("%s from loopback = 403, want it served", path)
		}
	}
	t.Setenv("LINGMA_ALLOW_REMOTE_DEBUG", "1")
	if code := serve("/debug/requests", "192.168.50.7:54321"); code == http.StatusForbidden {
		t.Fatal("LINGMA_ALLOW_REMOTE_DEBUG=1 should open the debug endpoints")
	}
}

func TestDebugAppLogsUsesProviderAndSkipsRecorder(t *testing.T) {
	server := NewServer("", service.New(service.Config{
		Model:   "Qwen3-Coder",
		Timeout: time.Second,
	}))
	server.AppLogs = func(limit int, source string) []DebugAppLogRecord {
		if limit != 7 {
			t.Fatalf("limit = %d", limit)
		}
		if source != "app" {
			t.Fatalf("source = %q", source)
		}
		return []DebugAppLogRecord{{
			Time:    "12:00:00",
			Source:  "app",
			Level:   "info",
			Message: "hello",
		}}
	}

	req := httptest.NewRequest(http.MethodGet, "/debug/app-logs?limit=7&source=app", nil)
	req.RemoteAddr = "127.0.0.1:54321"
	rec := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["kind"] != "desktop_app_logs" {
		t.Fatalf("kind = %#v", body["kind"])
	}
	if body["count"].(float64) != 1 {
		t.Fatalf("count = %#v", body["count"])
	}
	if len(server.debugRecords(10)) != 0 {
		t.Fatalf("debug app log inspection should not be recorded as a request")
	}
}

func TestResponsesRequestToChatRequest(t *testing.T) {
	req := openAIResponsesRequest{
		Model:        "resp-model",
		Instructions: "You are concise.",
		Input: []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "input_text", "text": "hello"}},
		}},
		MaxOutputTokens: 64,
		Reasoning:       map[string]any{"effort": "medium"},
		Text:            map[string]any{"format": map[string]any{"type": "json_object"}},
	}

	chatReq, err := responsesRequestToChatRequest(context.Background(), req)
	if err != nil {
		t.Fatalf("responsesRequestToChatRequest() error = %v", err)
	}
	if chatReq.Model != "resp-model" {
		t.Fatalf("model = %q", chatReq.Model)
	}
	if chatReq.MaxCompletionTokens != 64 {
		t.Fatalf("max completion tokens = %d", chatReq.MaxCompletionTokens)
	}
	if chatReq.ReasoningEffort != "medium" {
		t.Fatalf("reasoning effort = %q", chatReq.ReasoningEffort)
	}
	if len(chatReq.Messages) != 2 {
		t.Fatalf("message count = %d", len(chatReq.Messages))
	}
	if chatReq.Messages[0].Role != "system" || extractText(chatReq.Messages[0].Content) != "You are concise." {
		t.Fatalf("first message = %+v", chatReq.Messages[0])
	}
	if chatReq.Messages[1].Role != "user" || extractText(chatReq.Messages[1].Content) != "hello" {
		t.Fatalf("second message = %+v", chatReq.Messages[1])
	}
	if extractResponseFormat(chatReq.ResponseFormat) != "json_object" {
		t.Fatalf("response format = %#v", chatReq.ResponseFormat)
	}
}

func TestResponsesRequestStringInput(t *testing.T) {
	chatReq, err := responsesRequestToChatRequest(context.Background(), openAIResponsesRequest{Input: "hello"})
	if err != nil {
		t.Fatalf("responsesRequestToChatRequest() error = %v", err)
	}
	if len(chatReq.Messages) != 1 {
		t.Fatalf("message count = %d", len(chatReq.Messages))
	}
	if chatReq.Messages[0].Role != "user" || extractText(chatReq.Messages[0].Content) != "hello" {
		t.Fatalf("message = %+v", chatReq.Messages[0])
	}
}

func TestResponsesRequestSingleObjectPreservesRole(t *testing.T) {
	chatReq, err := responsesRequestToChatRequest(context.Background(), openAIResponsesRequest{Input: map[string]any{
		"role":    "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": "hello"}},
	}})
	if err != nil {
		t.Fatalf("responsesRequestToChatRequest() error = %v", err)
	}
	if len(chatReq.Messages) != 1 {
		t.Fatalf("message count = %d", len(chatReq.Messages))
	}
	if chatReq.Messages[0].Role != "assistant" || extractText(chatReq.Messages[0].Content) != "hello" {
		t.Fatalf("message = %+v", chatReq.Messages[0])
	}
}

func TestNormalizeAnthropicRequestMapsThinkingToReasoningEffort(t *testing.T) {
	req := anthropicRequest{
		Model:     "Qwen3.6-Plus",
		MaxTokens: 256,
		Thinking: map[string]any{
			"type":          "enabled",
			"budget_tokens": 2048,
		},
		Messages: []rawMessage{
			{Role: "user", Content: "请先思考再回答"},
		},
	}

	normalized, err := normalizeAnthropicRequest(req)
	if err != nil {
		t.Fatalf("normalizeAnthropicRequest() error = %v", err)
	}
	if normalized.ReasoningEffort != "medium" {
		t.Fatalf("reasoning effort = %q", normalized.ReasoningEffort)
	}
}

func TestNormalizeAnthropicRequestAdaptiveThinkingEnablesReasoning(t *testing.T) {
	req := anthropicRequest{
		Model:     "Qwen3-Thinking",
		MaxTokens: 256,
		Thinking: map[string]any{
			"type": "adaptive",
		},
		Messages: []rawMessage{
			{Role: "user", Content: "请先思考再回答"},
		},
	}

	normalized, err := normalizeAnthropicRequest(req)
	if err != nil {
		t.Fatalf("normalizeAnthropicRequest() error = %v", err)
	}
	if normalized.ReasoningEffort != "medium" {
		t.Fatalf("reasoning effort = %q", normalized.ReasoningEffort)
	}
}

// A named tier is a deliberate selection, so it must outrank the budget heuristic
// no matter which field the client puts it in.
func TestAnthropicReasoningEffortPrefersNamedTier(t *testing.T) {
	tests := []struct {
		name string
		req  anthropicRequest
		want string
	}{
		{
			name: "thinking.effort",
			req:  anthropicRequest{Thinking: map[string]any{"type": "enabled", "effort": "xhigh", "budget_tokens": 2048}},
			want: "xhigh",
		},
		{
			name: "output_config.effort",
			req:  anthropicRequest{Thinking: map[string]any{"type": "adaptive"}, OutputConfig: map[string]any{"effort": "xhigh"}},
			want: "xhigh",
		},
		{
			name: "reasoning_effort",
			req:  anthropicRequest{ReasoningEff: "high", Thinking: map[string]any{"type": "enabled", "budget_tokens": 512}},
			want: "high",
		},
		{
			name: "budget fallback",
			req:  anthropicRequest{Thinking: map[string]any{"type": "enabled", "budget_tokens": 8192}},
			want: "high",
		},
		{
			name: "large budget fallback",
			req:  anthropicRequest{Thinking: map[string]any{"type": "enabled", "budget_tokens": 32000}},
			want: "xhigh",
		},
		{
			name: "top budget fallback",
			req:  anthropicRequest{Thinking: map[string]any{"type": "enabled", "budget_tokens": 65536}},
			want: "max",
		},
		{
			name: "output_config effort max",
			req:  anthropicRequest{Thinking: map[string]any{"type": "adaptive"}, OutputConfig: map[string]any{"effort": "max"}},
			want: "max",
		},
		{
			name: "thinking disabled",
			req:  anthropicRequest{Thinking: map[string]any{"type": "disabled"}},
			want: "none",
		},
		{
			name: "no thinking fields at all",
			req:  anthropicRequest{},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := anthropicReasoningEffort(tc.req); got != tc.want {
				t.Fatalf("reasoning effort = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOpenAIResponsesMethodNotAllowed(t *testing.T) {
	server := NewServer("", service.New(service.Config{
		Model:   "Qwen3-Coder",
		Timeout: time.Second,
	}))

	req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	rec := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
}

func TestWriteOpenAIResponseStreamCompletedToolOnlyEmitsFunctionCallLifecycle(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := newOpenAIResponseStreamEmitter(rec, rec, "resp_1")
	result := &service.ChatResult{
		Model: "kmodel",
		ToolCalls: []toolemulation.ToolCall{{
			ID:        "call_1",
			Name:      "read_file",
			Arguments: map[string]any{"file_path": "go.mod"},
		}},
	}

	writeOpenAIResponseStreamCompleted(emitter, "resp_1", 123, "kmodel", result, "msg_1", false, false, false, "")

	body := rec.Body.String()
	if strings.Contains(body, "\"type\":\"message\"") {
		t.Fatalf("unexpected empty message lifecycle in tool-only response: %s", body)
	}
	if !strings.Contains(body, "\"type\":\"response.output_item.added\"") {
		t.Fatalf("missing function call added event: %s", body)
	}
	if !strings.Contains(body, "\"type\":\"response.function_call_arguments.delta\"") {
		t.Fatalf("missing function call delta event: %s", body)
	}
	if !strings.Contains(body, "\"type\":\"response.function_call_arguments.done\"") {
		t.Fatalf("missing function call done event: %s", body)
	}
	if !strings.Contains(body, "\"type\":\"response.output_item.done\"") {
		t.Fatalf("missing function call done event: %s", body)
	}
	if !strings.Contains(body, "\"call_id\":\"call_1\"") {
		t.Fatalf("missing function call payload: %s", body)
	}
	for _, want := range []string{
		"\"sequence_number\":0",
		"\"response_id\":\"resp_1\"",
		"\"output_index\":0",
		"\"item_id\":\"call_1\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %s in body: %s", want, body)
		}
	}
}

func TestBuildOpenAIResponseBodyIncludesReasoningItem(t *testing.T) {
	result := &service.ChatResult{
		Model:       "kmodel",
		Text:        "final answer",
		ThoughtText: "reasoning summary",
	}
	body := buildOpenAIResponseBody("resp_1", 123, "kmodel", result, "msg_1", true)
	output, ok := body["output"].([]map[string]any)
	if !ok {
		t.Fatalf("output type = %T", body["output"])
	}
	if len(output) < 2 {
		t.Fatalf("output len = %d", len(output))
	}
	if output[0]["type"] != "reasoning" {
		t.Fatalf("first output item = %#v", output[0])
	}
	summary, ok := output[0]["summary"].([]map[string]any)
	if !ok || len(summary) != 1 || summary[0]["text"] != "reasoning summary" {
		t.Fatalf("reasoning summary = %#v", output[0]["summary"])
	}
}

func TestResponseReasoningWriterEmitsLifecycle(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := newOpenAIResponseStreamEmitter(rec, rec, "resp_1")
	r := newResponseReasoningWriter(emitter, "rs_resp_1", 0)
	if err := r.Delta("reasoning summary"); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"\"type\":\"response.output_item.added\"",
		"\"type\":\"response.reasoning_summary_part.added\"",
		"\"type\":\"response.reasoning_summary_text.delta\"",
		"\"type\":\"response.reasoning_summary_text.done\"",
		"\"type\":\"response.reasoning_summary_part.done\"",
		"\"type\":\"response.output_item.done\"",
		"\"type\":\"reasoning\"",
		"reasoning summary",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected %s in body: %s", want, body)
		}
	}
}

func TestShouldEmitThinkingHelpers(t *testing.T) {
	req := service.ChatRequest{ReasoningEffort: "medium"}
	result := &service.ChatResult{ThoughtText: "thought"}
	if !shouldEmitAnthropicThinking(req, result) {
		t.Fatal("expected anthropic thinking to emit")
	}
	if !shouldEmitResponsesReasoning(req, result) {
		t.Fatal("expected responses reasoning to emit")
	}
	if shouldEmitAnthropicThinking(service.ChatRequest{}, result) {
		t.Fatal("unexpected anthropic thinking without reasoning effort")
	}
	if shouldEmitResponsesReasoning(req, &service.ChatResult{}) {
		t.Fatal("unexpected responses reasoning without thought text")
	}
}

func TestNormalizeOpenAIRequestRejectsMissingUserAndAssistantMessages(t *testing.T) {
	req := openAIChatRequest{
		Model: "test-model",
		Messages: []rawMessage{
			{Role: "system", Content: "Only system"},
			{Role: "tool", Content: "ignored"},
		},
	}

	_, err := normalizeOpenAIRequest(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for request without user or assistant messages")
	}
}

func TestNormalizeAnthropicRequestExtractsStructuredText(t *testing.T) {
	req := anthropicRequest{
		Model:  "test-model",
		System: []any{map[string]any{"type": "text", "text": "System prompt"}},
		Messages: []rawMessage{
			{
				Role: "user",
				Content: []any{
					map[string]any{"type": "text", "text": "Hello"},
				},
			},
			{
				Role: "assistant",
				Content: []any{
					map[string]any{"type": "text", "text": "Hi"},
				},
			},
			{
				Role: "metadata",
				Content: []any{
					map[string]any{"type": "text", "text": "ignored"},
				},
			},
		},
	}

	normalized, err := normalizeAnthropicRequest(req)
	if err != nil {
		t.Fatalf("normalizeAnthropicRequest() error = %v", err)
	}
	if normalized.Model != "test-model" {
		t.Fatalf("model = %q", normalized.Model)
	}
	if normalized.System != "System prompt" {
		t.Fatalf("system = %q", normalized.System)
	}
	if len(normalized.Messages) != 2 {
		t.Fatalf("message count = %d", len(normalized.Messages))
	}
	if normalized.Messages[0].Role != "user" || normalized.Messages[0].Text != "Hello" {
		t.Fatalf("first message = %+v", normalized.Messages[0])
	}
	if normalized.Messages[1].Role != "assistant" || normalized.Messages[1].Text != "Hi" {
		t.Fatalf("second message = %+v", normalized.Messages[1])
	}
}

func TestNormalizeAnthropicRequestRejectsEmptyMessages(t *testing.T) {
	req := anthropicRequest{
		Model: "test-model",
		Messages: []rawMessage{
			{Role: "user", Content: ""},
			{Role: "assistant", Content: nil},
		},
	}

	_, err := normalizeAnthropicRequest(req)
	if err == nil {
		t.Fatal("expected error for request without usable messages")
	}
}

func TestAnthropicHostedWebSearchCall(t *testing.T) {
	req := anthropicRequest{
		Model: "Kimi-K2.6",
		Tools: []any{
			map[string]any{
				"name": "web_search",
				"type": "web_search_20250305",
			},
		},
		ToolChoice: map[string]any{
			"type": "tool",
			"name": "web_search",
		},
		Messages: []rawMessage{{
			Role: "user",
			Content: []any{
				map[string]any{
					"type": "text",
					"text": "Perform a web search for the query: Hermes agent web UI documentation",
				},
			},
		}},
	}

	call, ok := anthropicHostedWebSearchCall(req)
	if !ok {
		t.Fatal("expected hosted web_search tool call")
	}
	if call.Name != "web_search" {
		t.Fatalf("tool name = %q", call.Name)
	}
	if call.Arguments["query"] != "Hermes agent web UI documentation" {
		t.Fatalf("query = %#v", call.Arguments["query"])
	}
	if !strings.HasPrefix(call.ID, "toolu_") {
		t.Fatalf("id = %q", call.ID)
	}
}

func TestAnthropicHostedWebSearchCallIgnoresRegularClientWebSearch(t *testing.T) {
	req := anthropicRequest{
		Tools: []any{
			map[string]any{
				"name": "web_search",
				"input_schema": map[string]any{
					"type": "object",
				},
			},
		},
		Messages: []rawMessage{{
			Role:    "user",
			Content: "Perform a web search for the query: Lingma",
		}},
	}

	if _, ok := anthropicHostedWebSearchCall(req); ok {
		t.Fatal("regular client web_search should stay in prompt tool emulation")
	}
}

func TestAnthropicHostedWebSearchCallIgnoresToolResultFollowup(t *testing.T) {
	req := anthropicRequest{
		Tools: []any{
			map[string]any{
				"name": "web_search",
				"type": "web_search_20250305",
			},
		},
		ToolChoice: map[string]any{
			"type": "tool",
			"name": "web_search",
		},
		Messages: []rawMessage{{
			Role: "user",
			Content: []any{
				map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_123",
					"content":     "result",
				},
			},
		}},
	}

	if _, ok := anthropicHostedWebSearchCall(req); ok {
		t.Fatal("hosted web_search should not short-circuit after a tool_result")
	}
}

func TestAnthropicCountTokensEndpoint(t *testing.T) {
	server := NewServer("", service.New(service.Config{
		Model:   "Qwen3-Coder",
		Timeout: time.Second,
	}))

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{
		"model":"kmodel",
		"max_tokens":128,
		"system":"You are concise.",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"name":"read_file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["input_tokens"].(float64) <= 0 {
		t.Fatalf("input_tokens = %#v", body["input_tokens"])
	}
}

// The /anthropic-prefixed aliases exist for clients whose base URL embeds the
// provider prefix (MiniMax's mmx CLI). Both paths must reach the exact same
// handlers: same count_tokens estimate, and a non-streaming /v1/messages turn
// that answers identically apart from the per-call message id.
func TestAnthropicAliasRoutesServeSameHandlers(t *testing.T) {
	server := NewServer("", service.New(service.Config{
		Model:   "Qwen3-Coder",
		Timeout: time.Second,
	}))

	const countBody = `{
		"model":"kmodel",
		"max_tokens":128,
		"system":"You are concise.",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[{"name":"read_file","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}},"required":["file_path"]}}]
	}`
	var canonicalTokens float64
	for _, path := range []string{"/v1/messages/count_tokens", "/anthropic/v1/messages/count_tokens"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(countBody))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status = %d body = %s", path, rec.Code, rec.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		tokens, _ := body["input_tokens"].(float64)
		if tokens <= 0 {
			t.Fatalf("%s: input_tokens = %#v", path, body["input_tokens"])
		}
		if canonicalTokens == 0 {
			canonicalTokens = tokens
		} else if tokens != canonicalTokens {
			t.Fatalf("input_tokens drifted between paths: %v vs %v", canonicalTokens, tokens)
		}
	}

	stub := newStubRemoteProxy(t, "alias route nonce answer")
	requestBody, err := json.Marshal(map[string]any{
		"model":      streamGuardModelName,
		"max_tokens": 64,
		"messages":   []any{map[string]any{"role": "user", "content": "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var canonical map[string]any
	for _, path := range []string{"/v1/messages", "/anthropic/v1/messages"} {
		resp, err := http.Post(stub.proxy.URL+path, "application/json", bytes.NewReader(requestBody))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		raw, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d body = %s", path, resp.StatusCode, raw)
		}
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if body["type"] != "message" || body["role"] != "assistant" || body["stop_reason"] != "end_turn" {
			t.Fatalf("%s: unexpected envelope: %s", path, raw)
		}
		content, ok := body["content"].([]any)
		if !ok || len(content) != 1 {
			t.Fatalf("%s: content = %s", path, raw)
		}
		textBlock, _ := content[0].(map[string]any)
		if textBlock["type"] != "text" || textBlock["text"] != "alias route nonce answer" {
			t.Fatalf("%s: unexpected first block: %s", path, raw)
		}
		delete(body, "id") // msg_<unix-nano> differs per call by design
		if canonical == nil {
			canonical = body
		} else if !reflect.DeepEqual(canonical, body) {
			t.Fatalf("response drifted between paths:\ncanonical: %v\nalias:     %v", canonical, body)
		}
	}
}

func TestDiscoveryCompatibilityEndpoints(t *testing.T) {
	server := NewServer("", service.New(service.Config{
		Model:   "Qwen3-Coder",
		Timeout: time.Second,
	}))

	cases := []string{
		"/version",
		"/props",
		"/v1/props",
	}
	for _, path := range cases {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body = %s", path, rec.Code, rec.Body.String())
		}
	}
}

func streamFilterTools() []toolemulation.ToolDef {
	return []toolemulation.ToolDef{{
		Name: "Bash",
		InputSchema: map[string]any{
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []any{"command"},
		},
	}}
}

func streamFilterRequest() service.ChatRequest {
	return service.ChatRequest{Tools: streamFilterTools(), ToolChoice: toolemulation.ToolChoice{Mode: "auto"}}
}

// TestToolStreamFilterOffWhenToolChoiceNone: with the client forbidding tool
// calls, applyToolEmulation keeps action blocks in the text, so the stream must
// not swallow them either.
func TestToolStreamFilterOffWhenToolChoiceNone(t *testing.T) {
	req := streamFilterRequest()
	req.ToolChoice = toolemulation.ToolChoice{Mode: "none"}
	block := "```json action\n" + `{"tool":"Bash","parameters":{"command":"pwd"}}` + "\n```"
	got := strings.Join(newToolStreamFilter(req).Push(block), "")
	if got != block {
		t.Fatalf("tool_choice:none must leave the block in the text, got %q", got)
	}
}

func streamThrough(t *testing.T, filter *toolStreamFilter, deltas ...string) string {
	t.Helper()
	var out strings.Builder
	for _, delta := range deltas {
		out.WriteString(strings.Join(filter.Push(delta), ""))
	}
	out.WriteString(strings.Join(filter.Flush(), ""))
	return out.String()
}

// TestToolStreamFilterFlushDecidesMarkerlessHybrids: a hybrid block whose JSON
// completed but whose wrapper never closed is undecidable mid-stream, so the
// stream holds it; Flush runs the final-text parse and must keep only the prose
// around it, exactly like the non-streaming path would.
func TestToolStreamFilterFlushDecidesMarkerlessHybrids(t *testing.T) {
	open := "<" + "tool_call" + ">"
	body := `{"tool": "Bash", "parameters": {"command": "pwd"}}`

	filter := newToolStreamFilter(streamFilterRequest())
	if chunks := filter.Push("前段" + open + "\n" + body); len(chunks) != 1 || chunks[0] != "前段" {
		t.Fatalf("prose before an open hybrid must stream, got %#v", chunks)
	}
	if got := strings.Join(filter.Flush(), ""); strings.TrimSpace(got) != "" {
		t.Fatalf("flush must strip the markerless hybrid block, got %q", got)
	}
}

func TestToolStreamFilterStreamsNormalTextWithTools(t *testing.T) {
	filter := newToolStreamFilter(streamFilterRequest())
	var chunks []string
	chunks = append(chunks, filter.Push(strings.Repeat("你", 120))...)
	chunks = append(chunks, filter.Push("后续内容")...)
	chunks = append(chunks, filter.Flush()...)
	out := strings.Join(chunks, "")
	if !strings.Contains(out, "后续内容") {
		t.Fatalf("streamed text = %q", out)
	}
}

func TestShouldAggregateToolStreamRequiresOptIn(t *testing.T) {
	t.Setenv("LINGMA_AGGREGATE_TOOL_STREAM", "")
	req := service.ChatRequest{Tools: []toolemulation.ToolDef{{Name: "Bash"}}}
	if shouldAggregateToolStream(req) {
		t.Fatal("tool streams should remain incremental by default")
	}

	t.Setenv("LINGMA_AGGREGATE_TOOL_STREAM", "1")
	if !shouldAggregateToolStream(req) {
		t.Fatal("explicit aggregate env should enable aggregate tool streams")
	}
}

// TestToolStreamFilterSuppressesRealActionBlockOnly is the whole point of the
// filter: the block goes, but the prose on both sides of it survives. The old
// filter blocked permanently at the first marker, so a turn that called a tool
// lost every word that followed it.
func TestToolStreamFilterSuppressesRealActionBlockOnly(t *testing.T) {
	filter := newToolStreamFilter(streamFilterRequest())
	got := streamThrough(t, filter,
		"先看这段说明\n",
		"```json action\n",
		`{"tool":"Bash","parameters":{"command":"pwd"}}`,
		"\n```\n",
		"最后一句必须完整出现。",
	)
	if !strings.Contains(got, "先看这段说明") || !strings.Contains(got, "最后一句必须完整出现。") {
		t.Fatalf("prose around the action block was lost: %q", got)
	}
	if strings.Contains(got, "```json") || strings.Contains(got, `"tool"`) {
		t.Fatalf("the consumed action block leaked to the client: %q", got)
	}
}

// TestToolStreamFilterKeepsBlockTheParserRejects: a fenced call for a tool the
// client never declared stays in the text on the non-streaming path, so the
// stream must not swallow it either.
func TestToolStreamFilterKeepsBlockTheParserRejects(t *testing.T) {
	filter := newToolStreamFilter(streamFilterRequest())
	got := streamThrough(t, filter,
		"举例说明格式：\n```json action\n",
		`{"tool":"NotATool","parameters":{"x":1}}`,
		"\n```\n",
		"以上只是示例。",
	)
	want := "举例说明格式：\n```json action\n" + `{"tool":"NotATool","parameters":{"x":1}}` + "\n```\n以上只是示例。"
	if got != want {
		t.Fatalf("streamed %q, want %q", got, want)
	}
}

// TestToolStreamFilterKeepsProseAfterInlineJSON is the reproduced truncation: an
// answer quoting a compact JSON object was cut at `{"name"` and the rest of the
// reply never reached the client, with finish_reason=stop and no error.
func TestToolStreamFilterKeepsProseAfterInlineJSON(t *testing.T) {
	filter := newToolStreamFilter(streamFilterRequest())
	var out strings.Builder
	for _, delta := range []string{"前半句START ", "{\"name\":\"Alice\",", "\"age\":7} ", "后半句END"} {
		pushed := filter.Push(delta)
		if len(pushed) == 0 && delta != "后半句END" {
			t.Fatalf("filter withheld %q, which is ordinary prose", delta)
		}
		out.WriteString(strings.Join(pushed, ""))
	}
	out.WriteString(strings.Join(filter.Flush(), ""))
	if got := out.String(); got != `前半句START {"name":"Alice","age":7} 后半句END` {
		t.Fatalf("streamed text = %q, want the whole reply", got)
	}
}

// TestToolStreamFilterHoldsSplitFence keeps a fence that arrives across two
// deltas from leaking half of itself before the block is recognised.
func TestToolStreamFilterHoldsSplitFence(t *testing.T) {
	filter := newToolStreamFilter(streamFilterRequest())
	if chunks := filter.Push("好的```j"); len(chunks) != 1 || chunks[0] != "好的" {
		t.Fatalf("partial fence leaked or prose was held: %#v", chunks)
	}
	if chunks := filter.Push("son\n{\"tool\":\"Bash\",\"parameters\":{\"command\":\"pwd\"}}"); len(chunks) != 0 {
		t.Fatalf("an opened action block leaked: %#v", chunks)
	}
	if chunks := filter.Push("\n```"); len(chunks) != 0 {
		t.Fatalf("a completed action block leaked: %#v", chunks)
	}
	if got := strings.Join(filter.Flush(), ""); got != "" {
		t.Fatalf("nothing should remain after a parsed block, got %q", got)
	}
}

// TestToolStreamFilterSuppressesXMLDialectCall covers the native dialect the
// models actually emit: the block must not stream, and the prose on both sides
// of it must.
func TestToolStreamFilterSuppressesXMLDialectCall(t *testing.T) {
	const (
		open     = "<" + "tool_call" + ">"
		closeTag = "</" + "tool_call" + ">"
		fnOpen   = "<" + "function="
		fnClose  = "</" + "function" + ">"
		pOpen    = "<" + "parameter="
		pClose   = "</" + "parameter" + ">"
	)
	block := open + "\n" + fnOpen + "Bash>\n" + pOpen + "command>\nls\n" + pClose + "\n" + fnClose + "\n" + closeTag

	filter := newToolStreamFilter(streamFilterRequest())
	// Feed the block in small slices so every tag is split mid-name: half a
	// tag must not reach the client either, or the wire format shows up as prose
	// before the block is recognised.
	chunks := []string{"前段"}
	for i := 0; i < len(block); i += 7 {
		end := i + 7
		if end > len(block) {
			end = len(block)
		}
		chunks = append(chunks, block[i:end])
	}
	chunks = append(chunks, "后段")

	var out strings.Builder
	for _, delta := range chunks {
		out.WriteString(strings.Join(filter.Push(delta), ""))
	}
	out.WriteString(strings.Join(filter.Flush(), ""))

	got := out.String()
	if got != "前段后段" {
		t.Fatalf("streamed %q, want only the prose", got)
	}
}

// TestTruncateRecordedStringBoundsRetention: the recorder keeps the last 200
// bodies, so an uncapped SSE answer used to be retained in full forever.
func TestTruncateRecordedStringBoundsRetention(t *testing.T) {
	if got := TruncateRecordedString("short"); got != "short" {
		t.Fatalf("short value changed: %q", got)
	}

	value := strings.Repeat("你", recordedBodyLimit) // 3x over the byte limit
	got := TruncateRecordedString(value)
	if len(got) >= len(value) {
		t.Fatalf("value was not truncated: %d bytes", len(got))
	}
	if !strings.Contains(got, "[truncated,") {
		t.Fatalf("missing truncation marker: %s", got[:40])
	}
	head := got[:strings.Index(got, "…[")]
	if !utf8.ValidString(head) {
		t.Fatal("truncation split a multi-byte rune")
	}
}

func TestParseImageURLReadsLocalFileURL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.jpg")
	data := []byte{0xff, 0xd8, 0xff, 0xd9}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	img := parseImageURL(context.Background(), "file://"+path)
	if img == nil {
		t.Fatal("expected image")
	}
	if img.MediaType != "image/jpeg" {
		t.Fatalf("media type = %q", img.MediaType)
	}
	if img.Data != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("data = %q", img.Data)
	}
}

func TestParseImageURLReadsAbsoluteLocalPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.png")
	data := []byte{0x89, 0x50, 0x4e, 0x47}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}

	img := parseImageURL(context.Background(), path)
	if img == nil {
		t.Fatal("expected image")
	}
	if img.MediaType != "image/png" {
		t.Fatalf("media type = %q", img.MediaType)
	}
	if img.Data != base64.StdEncoding.EncodeToString(data) {
		t.Fatalf("data = %q", img.Data)
	}
}

func TestSanitizeRecordedBodyRedactsImagePayloads(t *testing.T) {
	raw := []byte(`{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + strings.Repeat("a", 8192) + `"}}]}]}`)
	got := sanitizeRecordedBody(raw)
	if strings.Contains(got, "data:image/png;base64") {
		t.Fatalf("image payload was not redacted: %s", got)
	}
	if !strings.Contains(got, "[image payload redacted") {
		t.Fatalf("missing redaction marker: %s", got)
	}
}

// A transient CLI handshake failure must leave the client a retryable status; the
// old behaviour was a bare 500, which ZCode surfaced as retryable=false.
func TestTransientUpstreamIsRetryable(t *testing.T) {
	rec := httptest.NewRecorder()
	writeAnthropicUpstreamError(rec, fmt.Errorf("wrap: %w", remote.ErrTransientUpstream))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("anthropic transient status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var body struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Type != "overloaded_error" {
		t.Fatalf("anthropic transient type = %q", body.Error.Type)
	}

	rec = httptest.NewRecorder()
	writeAnthropicUpstreamError(rec, errors.New("model refused"))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("permanent status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}

	rec = httptest.NewRecorder()
	writeOpenAIUpstreamError(rec, fmt.Errorf("wrap: %w", remote.ErrTransientUpstream))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("openai transient status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
}

// A disconnect must not file itself as an upstream error, or the access log
// blames the proxy for a client that simply gave up.
func TestClientCancelledRequestIsNotLoggedAsError(t *testing.T) {
	if got := debugLogLevel(499); got == "error" {
		t.Fatalf("499 logged as error")
	}
	if got := debugLogLevel(500); got != "error" {
		t.Fatalf("500 level = %q, want error", got)
	}
}

// H1: the body must be capped once, in withRecorder, before it is buffered; an
// over-cap POST has to leave through decodeJSON's 400 exit, not through memory.
func TestWithRecorderCapsRequestBody(t *testing.T) {
	server := NewServer("", service.New(service.Config{Model: "Qwen3-Coder", Timeout: time.Second}))
	// Deliberately unterminated JSON: without the cap this decodes into the whole
	// string; with the cap the reader stops at maxRequestBytes.
	body := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x", maxRequestBytes+(1<<10))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %.200s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "too large") {
		t.Fatalf("oversized body must be refused by the cap, got: %.200s", rec.Body.String())
	}
}

// The cap must stay above this proxy's own image policy. It was first set to
// 1 MiB, which is smaller than one screenshot: every client that sends images as
// `data:` URLs (Claude Code, Cline, ZCode) got a 400 about the body size, so the
// image feature was dead the moment the guard went in.
func TestRequestCapLeavesInlineImagesAlone(t *testing.T) {
	// A 12 MiB inline image is inside the policy (maxImageFetchBytes bounds
	// remote fetches at 20 MiB) and must not be refused for being large.
	if maxRequestBytes <= 12<<20 {
		t.Fatalf("maxRequestBytes = %d cannot carry a 12 MiB inline image", int64(maxRequestBytes))
	}
	png := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x89, 0x50, 0x4e, 0x47}, 3<<20))
	body := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"look"},` +
		`{"type":"image_url","image_url":{"url":"data:image/png;base64,` + png + `"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	NewServer("", service.New(service.Config{Model: "Qwen3-Coder", Timeout: 200 * time.Millisecond})).
		http.Handler.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "too large") {
		t.Fatalf("a legitimate inline image was refused by the request cap: status=%d body=%.200s",
			rec.Code, rec.Body.String())
	}
}

// H3: "0"/"false" in LINGMA_ALLOW_REMOTE_DEBUG must mean OFF.
func TestRemoteDebugGateRequiresTruthyEnv(t *testing.T) {
	server := NewServer("", service.New(service.Config{Model: "Qwen3-Coder", Timeout: time.Second}))
	serve := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/requests", nil)
		req.RemoteAddr = "192.168.50.7:54321"
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, off := range []string{"0", "false", "no", "off", ""} {
		t.Setenv("LINGMA_ALLOW_REMOTE_DEBUG", off)
		if code := serve(); code != http.StatusForbidden {
			t.Fatalf("LINGMA_ALLOW_REMOTE_DEBUG=%q opened the debug endpoints to the LAN (status %d)", off, code)
		}
	}
	for _, on := range []string{"1", "true", "yes", "on"} {
		t.Setenv("LINGMA_ALLOW_REMOTE_DEBUG", on)
		if code := serve(); code == http.StatusForbidden {
			t.Fatalf("LINGMA_ALLOW_REMOTE_DEBUG=%q should open the debug endpoints", on)
		}
	}
}

// H4: a browser page on another host must not read the loopback-gated debug
// routes; curl/CLI (no Origin) and same-origin tooling keep working.
func TestDebugEndpointsRefuseForeignOrigin(t *testing.T) {
	server := NewServer("", service.New(service.Config{Model: "Qwen3-Coder", Timeout: time.Second}))
	serve := func(origin, remoteAddr string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/requests", nil)
		req.RemoteAddr = remoteAddr
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, req)
		return rec.Code
	}
	// httptest.NewRequest sets Host: example.com
	if code := serve("http://evil.example", "127.0.0.1:54321"); code != http.StatusForbidden {
		t.Fatalf("cross-origin loopback read = %d, want 403", code)
	}
	if code := serve("http://example.com", "127.0.0.1:54321"); code != http.StatusOK {
		t.Fatalf("same-origin read = %d, want 200", code)
	}
	if code := serve("", "127.0.0.1:54321"); code != http.StatusOK {
		t.Fatalf("curl-style (no Origin) read = %d, want 200", code)
	}
	t.Setenv("LINGMA_ALLOW_REMOTE_DEBUG", "1")
	if code := serve("http://evil.example", "10.1.2.3:54321"); code != http.StatusForbidden {
		t.Fatalf("foreign origin with remote debug opted in = %d, want 403", code)
	}
}

// H5: thinking normalised from {"type":"disabled"} to "none" must behave like
// no thinking at all on every gate.
func TestThinkingDisabledEffortIsNotRequested(t *testing.T) {
	result := &service.ChatResult{ThoughtText: "thought"}
	for _, off := range []string{"", "none", " NONE ", "None"} {
		if thinkingRequested(off) {
			t.Fatalf("thinkingRequested(%q) = true", off)
		}
		req := service.ChatRequest{ReasoningEffort: off}
		if shouldEmitAnthropicThinking(req, result) {
			t.Fatalf("disabled effort %q still emits an Anthropic thinking block", off)
		}
		if shouldEmitResponsesReasoning(req, result) {
			t.Fatalf("disabled effort %q still emits responses reasoning", off)
		}
	}
	for _, on := range []string{"low", "medium", "max"} {
		if !thinkingRequested(on) {
			t.Fatalf("thinkingRequested(%q) = false", on)
		}
	}
}

// H7: the recording writer must stop retaining past the cap. The client-facing
// stream is untouched either way; only rw.body growth is the leak. Re-injecting
// the old unconditional append makes len(rw.body) equal the full stream again.
func TestRecordingWriterStopsAccumulatingPastLimit(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &recordingResponseWriter{ResponseWriter: rec, statusCode: 200}
	chunk := []byte(strings.Repeat("x", 10<<10))
	for i := 0; i < 20; i++ {
		if _, err := rw.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(rw.body); n > recordedBodyLimit+len(chunk) {
		t.Fatalf("writer kept %d bytes, well past the %d byte cap", n, recordedBodyLimit)
	}
	if !strings.Contains(rec.Body.String(), strings.Repeat("x", 10<<10)) {
		t.Fatal("the client stream must not be affected by the recording cap")
	}
}

// H8: "/" must answer with the same minimal payload as /health.
func TestRootDoesNotLeakRuntimeState(t *testing.T) {
	server := NewServer("", service.New(service.Config{Model: "Qwen3-Coder", Timeout: time.Second}))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "192.168.50.7:54321"
	rec := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if _, ok := body["state"]; ok {
		t.Fatalf("GET / leaked the runtime state: %s", rec.Body.String())
	}
	if body["ok"] != true || body["service"] != "lingma-proxy" {
		t.Fatalf("GET / payload = %#v", body)
	}
}

// H2: remote image fetches must not be able to point the proxy at link-local
// metadata endpoints or non-http schemes.
func TestFetchImageRejectsSSRFTargets(t *testing.T) {
	for _, bad := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://[fe80::1]/x.png",
		"ftp://example.com/x.png",
		"file:///C:/Windows/win.ini",
	} {
		_, err := fetchImageAsBase64(context.Background(), bad)
		if err == nil || !strings.Contains(err.Error(), "not allowed") {
			t.Fatalf("fetchImageAsBase64(%q) err = %v, want an explicit rejection", bad, err)
		}
	}
}

// H2: a dead caller must not keep the fetch alive, the response body is
// byte-capped, and redirects cannot walk into the metadata range. Legitimate
// http fetches keep working.
func TestFetchImageHonorsContextAndByteCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/big":
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(bytes.Repeat([]byte("A"), maxImageFetchBytes+1))
		case "/redirect-metadata":
			http.Redirect(w, r, "http://169.254.169.254/x.png", http.StatusFound)
		default:
			// A real image, declared as one. fetchImageAsBase64 refuses an
			// answer that is not image/* any more: a URL that returns JSON,
			// HTML or plain text is not a picture, and base64-ing it into the
			// model prompt is the read half of the SSRF chain that the Origin
			// gate only closes for browsers. The four PNG magic bytes this used
			// to write are not an image to Go's own content sniffer, which
			// answered text/plain for them.
			w.Header().Set("Content-Type", "image/png")
			_, _ = w.Write(harden933PNG())
		}
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := fetchImageAsBase64(ctx, srv.URL+"/small"); err == nil {
		t.Fatal("a cancelled caller must abort the fetch")
	}
	if _, err := fetchImageAsBase64(context.Background(), srv.URL+"/big"); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("oversized response err = %v, want the byte cap to reject it", err)
	}
	if _, err := fetchImageAsBase64(context.Background(), srv.URL+"/redirect-metadata"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("redirect into link-local err = %v, want rejection", err)
	}
	img, err := fetchImageAsBase64(context.Background(), srv.URL+"/small")
	if err != nil || img == nil || img.Data == "" {
		t.Fatalf("legitimate http fetch broke: img=%#v err=%v", img, err)
	}
}

// sseEventNames lists the `event:` lines in the order they were written.
func sseEventNames(t *testing.T, body string) []string {
	t.Helper()
	var names []string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "event: ") {
			names = append(names, strings.TrimPrefix(line, "event: "))
		}
	}
	return names
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// H9: the URL-level guard only ever sees the literal host, so a public name that
// resolves into the metadata range got through. The policy has to be re-applied
// to the address the transport actually dials.
func TestImageFetchPolicyAppliesToResolvedAddress(t *testing.T) {
	for _, blocked := range []string{
		"169.254.169.254", "169.254.1.1", "fe80::1", "224.0.0.1", "0.0.0.0",
	} {
		if !imageHostBlocked(net.ParseIP(blocked)) {
			t.Fatalf("%s must not be an image origin", blocked)
		}
	}
	if !imageHostBlocked(nil) {
		t.Fatal("an unresolvable address must fail closed")
	}
	// Loopback and the LAN stay reachable on purpose: self-hosted image servers
	// and this machine's own ports are legitimate origins for a local proxy.
	for _, allowed := range []string{"127.0.0.1", "::1", "192.168.50.239", "10.1.2.3", "8.8.8.8"} {
		if imageHostBlocked(net.ParseIP(allowed)) {
			t.Fatalf("%s must stay reachable", allowed)
		}
	}

	tr, ok := imageFetchClient.Transport.(*http.Transport)
	if !ok || tr.DialContext == nil {
		t.Fatal("imageFetchClient must install a dial-time guard; the URL check alone is bypassable by DNS")
	}
	if _, err := tr.DialContext(context.Background(), "tcp", "169.254.169.254:80"); err == nil {
		t.Fatal("dialling the metadata address must fail before the connection is made")
	}
	if err := imageAddressAllowed("192.168.50.239:80"); err != nil {
		t.Fatalf("LAN address rejected: %v", err)
	}
}

// 924 §4.1: reasoning must stream piece by piece, and the piecewise sequence has
// to be the one clients already get from the whole-text writer.
func TestResponseReasoningWriterStreamsSameSequenceAsWholeItem(t *testing.T) {
	pieces := []string{"first ", "second ", "third"}

	streamed := httptest.NewRecorder()
	rw := newResponseReasoningWriter(newOpenAIResponseStreamEmitter(streamed, streamed, "resp_1"), "rs_resp_1", 0)
	if rw.Opened() {
		t.Fatal("a writer with no delta must not announce an item")
	}
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}
	if streamed.Body.Len() != 0 {
		t.Fatalf("closing an unopened writer wrote %q", streamed.Body.String())
	}
	for _, p := range pieces {
		if err := rw.Delta(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := rw.Close(); err != nil {
		t.Fatal(err)
	}

	whole := httptest.NewRecorder()
	wh := newResponseReasoningWriter(newOpenAIResponseStreamEmitter(whole, whole, "resp_1"), "rs_resp_1", 0)
	if err := wh.Delta(strings.Join(pieces, "")); err != nil {
		t.Fatal(err)
	}
	if err := wh.Close(); err != nil {
		t.Fatal(err)
	}

	// A piecewise writer emits one delta per piece, the whole-text writer emits
	// one: collapse the runs so the comparison is about ordering and lifecycle.
	got, want := collapseRuns(sseEventNames(t, streamed.Body.String())), sseEventNames(t, whole.Body.String())
	if !equalStrings(got, want) {
		t.Fatalf("streamed event sequence = %v, whole-item sequence = %v", got, want)
	}
	// Count the event line, not the name: the name also appears in the payload's
	// own "type" field, so a single event matches the bare name twice.
	if n := strings.Count(streamed.Body.String(), "event: response.output_item.done"); n != 1 {
		t.Fatalf("output_item.done emitted %d times, want exactly 1 after a double Close", n)
	}
	texts := sseDeltaTexts(t, streamed.Body.String(), "response.reasoning_summary_text.delta")
	if strings.Join(texts, "") != strings.Join(pieces, "") {
		t.Fatalf("the streamed deltas carry %q, want %q", strings.Join(texts, ""), strings.Join(pieces, ""))
	}
	if len(texts) != len(pieces) {
		t.Fatalf("the writer batched %d deltas into %d: %s", len(pieces), len(texts), streamed.Body.String())
	}
	if !strings.Contains(streamed.Body.String(), "\"text\":\"first second third\"") {
		t.Fatalf("accumulated summary missing from the closing events: %s", streamed.Body.String())
	}
}

// collapseRuns folds adjacent equal names into one, so a stream that emits N
// deltas matches a whole-item writer that emits 1.
func collapseRuns(names []string) []string {
	out := make([]string, 0, len(names))
	for i, name := range names {
		if i > 0 && names[i-1] == name {
			continue
		}
		out = append(out, name)
	}
	return out
}

// sseDeltaTexts returns the "delta" field of every payload that follows an event
// line named eventName.
func sseDeltaTexts(t *testing.T, body, eventName string) []string {
	t.Helper()
	var out []string
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if line != "event: "+eventName || i+1 >= len(lines) {
			continue
		}
		var payload struct {
			Delta string `json:"delta"`
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[i+1], "data: ")), &payload); err != nil {
			t.Fatalf("payload after %s is not JSON: %v", eventName, err)
		}
		out = append(out, payload.Delta)
	}
	return out
}

// 924 §4.1: prose streamed as output_text.delta can be dropped from the final
// frame once a tool call wins, which used to leave the announced item unclosed.
func TestStreamCompletedClosesMessageAnnouncedOnlyByDeltas(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := newOpenAIResponseStreamEmitter(rec, rec, "resp_1")
	result := &service.ChatResult{
		Model: "kmodel",
		Text:  "let me read that {\"tool\":\"read_file\"}",
		ToolCalls: []toolemulation.ToolCall{{
			ID:        "call_1",
			Name:      "read_file",
			Arguments: map[string]any{"file_path": "go.mod"},
		}},
	}

	writeOpenAIResponseStreamCompleted(emitter, "resp_1", 123, "kmodel", result, "msg_1", true, false, false, "let me read that ")

	body := rec.Body.String()
	if !strings.Contains(body, "\"type\":\"response.output_text.done\"") {
		t.Fatalf("an item announced with output_text.delta must still be closed: %s", body)
	}
	if !strings.Contains(body, "\"text\":\"let me read that \"") {
		t.Fatalf("the closing events must carry the text the client saw: %s", body)
	}
}

// The common shape is worse than the prose case above: a model that emits a
// single space before its action block opens the message item, the filter then
// eats the block, and the final frame reports no text at all. Trimming the
// streamed text here re-created the item-that-never-closes defect.
func TestStreamCompletedClosesMessageOpenedByWhitespaceOnlyDelta(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := newOpenAIResponseStreamEmitter(rec, rec, "resp_1")
	result := &service.ChatResult{
		Model: "kmodel",
		ToolCalls: []toolemulation.ToolCall{{
			ID:        "call_1",
			Name:      "read_file",
			Arguments: map[string]any{"file_path": "go.mod"},
		}},
	}

	writeOpenAIResponseStreamCompleted(emitter, "resp_1", 123, "kmodel", result, "msg_1", true, false, false, " ")

	body := rec.Body.String()
	for _, want := range []string{
		"\"type\":\"response.content_part.done\"",
		"\"type\":\"response.output_text.done\"",
		"\"type\":\"response.output_item.done\"",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("an item announced by output_text.delta was never closed (%s): %s", want, body)
		}
	}
}

// H2/H9 review follow-up: the hardening only covered the remote fetch, but the
// local-file branch is reached first from the same client-controlled string, so
// image_url "/etc/passwd" got base64'd and labelled image/jpeg straight into the
// prompt. The gate has to hold on the production entry point.
func TestLocalImagePathCannotReadNonImageFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)

	secret := filepath.Join(dir, "credentials.txt")
	if err := os.WriteFile(secret, []byte("TOP-SECRET-PLAINTEXT"), 0600); err != nil {
		t.Fatal(err)
	}
	if img := parseImageURL(context.Background(), secret); img != nil {
		t.Fatalf("a non-image path must not be attachable, got media_type=%q data_len=%d", img.MediaType, len(img.Data))
	}
	if img := parseImageURL(context.Background(), filepath.Join(dir, "no_extension_at_all")); img != nil {
		t.Fatal("an extension-less path must be rejected now that the default is no longer image/jpeg")
	}

	// The feature stays: a real screenshot path still reads.
	png := filepath.Join(dir, "shot.png")
	if err := os.WriteFile(png, []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a}, 0600); err != nil {
		t.Fatal(err)
	}
	img := parseImageURL(context.Background(), png)
	if img == nil || img.MediaType != "image/png" || img.Data == "" {
		t.Fatalf("local screenshot attach broke: %#v", img)
	}

	// A device or a directory must not be read either, and the size gate matches
	// the cap the remote fetch enforces.
	if img := parseImageURL(context.Background(), dir); img != nil {
		t.Fatal("a directory must not be readable as an image")
	}
	big := filepath.Join(dir, "big.png")
	f, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxImageFetchBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if img := parseImageURL(context.Background(), big); img != nil {
		t.Fatalf("a file over maxImageFetchBytes (%d) must be refused", int64(maxImageFetchBytes))
	}
}

func TestOpenAIStreamErrorObjectKeepsTheRetryHint(t *testing.T) {
	plain := openAIStreamErrorObject(errors.New("boom"))
	if plain["type"] != "api_error" || plain["message"] != "boom" {
		t.Fatalf("plain error object = %#v", plain)
	}
	if _, ok := plain["code"]; ok {
		t.Fatalf("a non-transient failure must not claim a retry code: %#v", plain)
	}
	transient := openAIStreamErrorObject(fmt.Errorf("upstream: %w", remote.ErrTransientUpstream))
	if transient["code"] != "transient_upstream" {
		t.Fatalf("transient error object = %#v, want the retry hint the 503 used to carry", transient)
	}
}

// The Responses protocol defines no ping event, so the keepalive has to be a
// comment line a conforming parser skips.
func TestWriteSSECommentEmitsNoEvent(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := writeSSEComment(rec, rec, "keepalive"); err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if body != ": keepalive\n\n" {
		t.Fatalf("comment frame = %q, want %q", body, ": keepalive\n\n")
	}
	if len(sseEventNames(t, body)) != 0 {
		t.Fatalf("a keepalive must not open an event: %q", body)
	}
}

// H10: the request-side sanitizer used to parse, deep-copy and re-marshal the
// whole body just to keep 8 KiB of it, so widening maxRequestBytes for inline
// images made that cost proportional. Past the ceiling the body must be cut
// first, which shows up as the raw prefix surviving instead of a redacted tree.
func TestSanitizeRecordedBodyStopsAtTheCeiling(t *testing.T) {
	// The body has to stay valid JSON to its end: the ceiling is the only thing
	// that keeps the parse from running, so a fixture that could not parse anyway
	// would take the same branch with or without the guard.
	oversized := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
		strings.Repeat("Q", recordedBodySanitizeCeiling) + `"}}]}]}`

	got := sanitizeRecordedBody([]byte(oversized))
	if strings.Contains(got, "redact") {
		t.Fatalf("a body past the ceiling was still parsed and re-marshalled: %.120s", got)
	}
	if !strings.Contains(got, strings.Repeat("Q", 512)) {
		t.Fatalf("expected the raw prefix of an over-ceiling body, got %.120s", got)
	}
	if len(got) > recordedBodyLimit*4 {
		t.Fatalf("record grew past its limit: %d bytes", len(got))
	}

	// Under the ceiling the redaction still happens, so the guard does not quietly
	// disable the feature for ordinary requests.
	small := `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` +
		strings.Repeat("Q", 4096) + `"}}]}]}`
	if plain := sanitizeRecordedBody([]byte(small)); strings.Contains(plain, strings.Repeat("Q", 256)) {
		t.Fatalf("an under-ceiling image payload was not redacted: %.120s", plain)
	}
}

// H5: the non-streaming responses writer is the fourth call site of the same
// gate, and losing `req` there handed the chain of thought back to clients that
// never asked for it.
func TestWriteOpenAIResponseGatesReasoningOnTheRequest(t *testing.T) {
	result := &service.ChatResult{Model: "kmodel", Text: "final answer", ThoughtText: "private chain of thought"}

	asked := httptest.NewRecorder()
	writeOpenAIResponse(asked, result, service.ChatRequest{ReasoningEffort: "high"})
	if !strings.Contains(asked.Body.String(), "private chain of thought") {
		t.Fatalf("a request that asked for reasoning got no reasoning item: %s", asked.Body.String())
	}

	notAsked := httptest.NewRecorder()
	writeOpenAIResponse(notAsked, result, service.ChatRequest{})
	if strings.Contains(notAsked.Body.String(), "private chain of thought") {
		t.Fatalf("the thought leaked to a client that did not ask for it: %s", notAsked.Body.String())
	}
}

// A saturated execution gate used to park the request with no bytes at all, which
// a client cannot tell apart from a dead upstream: the Qoder SDK's 60s
// first-payload timeout fired and the caller's agent turn died. The gate now has
// to refuse inside that window, and refusing means a retryable 429, not a wait.
func TestFullQueueRefusesInsteadOfGoingSilent(t *testing.T) {
	server := NewServer("", service.New(service.Config{Model: "kmodel", Timeout: time.Second}))
	for i := 0; i < cap(server.sem); i++ {
		server.sem <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(server.sem); i++ {
			<-server.sem
		}
	}()

	previous := slotQueueWait
	slotQueueWait = 20 * time.Millisecond
	defer func() { slotQueueWait = previous }()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"kmodel","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	started := time.Now()
	server.http.Handler.ServeHTTP(rec, req)

	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("a full queue waited %s instead of refusing", elapsed)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusTooManyRequests, rec.Body.String())
	}
}

// 931 real acceptance: a first turn streamed "I'll read both fixture files
// now." plus two function_call items, but response.completed's output array
// carried only the function calls with output_text empty. The final frame has
// to stay faithful to what the stream already announced.
func TestStreamCompletedResponseCarriesStreamedMessageAndCallsFaithfully(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := newOpenAIResponseStreamEmitter(rec, rec, "resp_1")
	streamed := "I'll read both fixture files now.\n\n"
	result := &service.ChatResult{
		Model: "kmodel",
		Text:  streamed,
		ToolCalls: []toolemulation.ToolCall{
			{ID: "call_1", Name: "read_fixture", Arguments: map[string]any{"path": "alpha.txt"}},
			{ID: "call_2", Name: "read_fixture", Arguments: map[string]any{"path": "beta.txt"}},
		},
	}

	writeOpenAIResponseStreamCompleted(emitter, "resp_1", 123, "kmodel", result, "msg_1", true, false, false, streamed)

	var completed map[string]any
	messageDoneIndexes := []int{}
	callDoneIndexes := map[string]int{}
	for _, block := range strings.Split(rec.Body.String(), "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" || data == "" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unparseable SSE payload for %s: %v (%s)", name, err, data)
		}
		switch name {
		case "response.completed":
			completed = payload
		case "response.output_item.done":
			item, _ := payload["item"].(map[string]any)
			index, _ := payload["output_index"].(float64)
			switch item["type"] {
			case "message":
				messageDoneIndexes = append(messageDoneIndexes, int(index))
				content, _ := item["content"].([]any)
				if len(content) != 1 {
					t.Fatalf("message done content = %#v", item["content"])
				}
				part, _ := content[0].(map[string]any)
				if part["text"] != streamed {
					t.Fatalf("message done text = %q, want the streamed bytes verbatim", part["text"])
				}
			case "function_call":
				callDoneIndexes[item["call_id"].(string)] = int(index)
			}
		}
	}
	if completed == nil {
		t.Fatal("no response.completed event in stream")
	}

	response, _ := completed["response"].(map[string]any)
	output, _ := response["output"].([]any)
	if len(output) != 3 {
		t.Fatalf("completed output = %#v, want [message, function_call, function_call]", output)
	}
	msg, _ := output[0].(map[string]any)
	if msg["type"] != "message" || msg["id"] != "msg_1" {
		t.Fatalf("completed output[0] = %#v, want the streamed message item with its id", msg)
	}
	content, _ := msg["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("completed message content = %#v", msg["content"])
	}
	part, _ := content[0].(map[string]any)
	if part["text"] != streamed {
		t.Fatalf("completed message text = %q, want the streamed bytes verbatim", part["text"])
	}
	fc1, _ := output[1].(map[string]any)
	fc2, _ := output[2].(map[string]any)
	if fc1["call_id"] != "call_1" || fc2["call_id"] != "call_2" {
		t.Fatalf("completed call order = %v, %v", fc1["call_id"], fc2["call_id"])
	}
	if response["output_text"] != streamed {
		t.Fatalf("completed output_text = %q, want the streamed text", response["output_text"])
	}

	// The completed array must sit at the same indexes the done events used, so
	// a client reconciling the stream against the final frame finds one history.
	if len(messageDoneIndexes) != 1 || messageDoneIndexes[0] != 0 {
		t.Fatalf("message done indexes = %v, want [0]", messageDoneIndexes)
	}
	if callDoneIndexes["call_1"] != 1 || callDoneIndexes["call_2"] != 2 {
		t.Fatalf("call done indexes = %v, want call_1=1 call_2=2", callDoneIndexes)
	}
}

// The non-streaming Responses body shares the text+tools path with the final
// stream frame: dropping the prose whenever a tool call won lost the answer in
// JSON mode too.
func TestBuildOpenAIResponseBodyKeepsTextAlongsideToolCalls(t *testing.T) {
	result := &service.ChatResult{
		Model: "kmodel",
		Text:  "I'll read both fixture files now.",
		ToolCalls: []toolemulation.ToolCall{{
			ID:        "call_1",
			Name:      "read_fixture",
			Arguments: map[string]any{"path": "alpha.txt"},
		}},
	}

	body := buildOpenAIResponseBody("resp_1", 123, "kmodel", result, "", false)

	output, ok := body["output"].([]map[string]any)
	if !ok {
		t.Fatalf("output type = %T", body["output"])
	}
	if len(output) != 2 {
		t.Fatalf("output len = %d, want message + function_call: %#v", len(output), output)
	}
	if output[0]["type"] != "message" {
		t.Fatalf("output[0] = %#v, want the message item ahead of the call", output[0])
	}
	content, _ := output[0]["content"].([]map[string]any)
	if len(content) != 1 || content[0]["text"] != "I'll read both fixture files now." {
		t.Fatalf("message content = %#v", output[0]["content"])
	}
	if output[1]["type"] != "function_call" || output[1]["call_id"] != "call_1" {
		t.Fatalf("output[1] = %#v, want the function call", output[1])
	}
	if body["output_text"] != "I'll read both fixture files now." {
		t.Fatalf("output_text = %#v", body["output_text"])
	}
}

// 931 real acceptance, aggregate path: turn 2 of cn-agg-responses-sse carried a
// full nonce message in the final frame but not one response.output_text.delta
// -- clients assemble the answer from deltas, so the done-only lifecycle left
// the text invisible. A message the stream never announced has to be opened,
// delta'd, and closed in protocol order.
func TestStreamCompletedEmitsDeltaForUnannouncedText(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := newOpenAIResponseStreamEmitter(rec, rec, "resp_1")
	result := &service.ChatResult{
		Model: "kmodel",
		Text:  "ALPHA_68e5f86ee847d8d2|BETA_65c00ba76aea55bb",
	}

	writeOpenAIResponseStreamCompleted(emitter, "resp_1", 123, "kmodel", result, "msg_1", false, false, false, "")

	var order []string
	var deltaText string
	for _, block := range strings.Split(rec.Body.String(), "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" {
			continue
		}
		switch name {
		case "response.output_text.delta":
			var payload map[string]any
			if err := json.Unmarshal([]byte(data), &payload); err != nil {
				t.Fatalf("unparseable delta: %v (%s)", err, data)
			}
			deltaText = payload["delta"].(string)
			order = append(order, name)
		case "response.output_item.added", "response.content_part.added",
			"response.content_part.done", "response.output_text.done",
			"response.output_item.done", "response.completed":
			order = append(order, name)
		}
	}

	wantOrder := []string{
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.content_part.done",
		"response.output_text.done",
		"response.output_item.done",
		"response.completed",
	}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("lifecycle = %v, want %v", order, wantOrder)
	}
	if deltaText != result.Text {
		t.Fatalf("delta text = %q, want the whole final text %q", deltaText, result.Text)
	}
}

// stubRemoteProxy wires a real service and HTTP handler to a loopback stub of
// the Lingma gateway that streams one answer, so stream-level protocol tests
// run end to end without any cloud or CLI.
type stubRemoteProxy struct {
	proxy    *httptest.Server
	upstream *httptest.Server
	calls    *atomic.Int32
}

func newStubRemoteProxy(t *testing.T, answer string) *stubRemoteProxy {
	t.Helper()
	t.Setenv("LINGMA_AGGREGATE_TOOL_STREAM", "")
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")

	calls := &atomic.Int32{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		for _, piece := range []string{answer[:len(answer)/2], answer[len(answer)/2:]} {
			if _, err := io.WriteString(w, streamGuardUpstreamFrame(t, piece)); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))

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
		Model:          streamGuardModelName,
		Timeout:        30 * time.Second,
	})
	t.Cleanup(func() { _ = svc.Close() })

	proxy := httptest.NewServer(NewServer("", svc).http.Handler)
	t.Cleanup(upstream.Close)
	t.Cleanup(proxy.Close)
	return &stubRemoteProxy{proxy: proxy, upstream: upstream, calls: calls}
}

// anthropicBlockStarts walks one Anthropic SSE body and reports every
// content_block_start as (index, block type), in arrival order.
func anthropicBlockStarts(t *testing.T, body string) [][2]any {
	t.Helper()
	var out [][2]any
	for _, block := range strings.Split(body, "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name != "content_block_start" {
			continue
		}
		var payload struct {
			Index int `json:"index"`
			Block struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unparseable content_block_start: %v (%s)", err, data)
		}
		out = append(out, [2]any{payload.Index, payload.Block.Type})
	}
	return out
}

// 931 real acceptance: the pure-tool first turn of cn-anthropic-sse announced
// its only content block at index 1, and the installed official anthropic SDK
// 0.76.0 rejects that frame sequence with an IndexError -- replaying the very
// same frames against the live cloud produced the same crash
// (official-anthropic-sdk-replay.json / official-sdk-live-before.json). A
// reserved-but-never-opened thinking block must not consume an index: blocks
// are numbered from 0 in announcement order.
func TestAnthropicToolOnlyStreamIndexesContentBlocksFromZero(t *testing.T) {
	stub := newStubRemoteProxy(t, "```json action\n{\"tool\":\"read_fixture\",\"parameters\":{\"path\":\"block-index-alpha.txt\"}}\n```")

	requestBody, err := json.Marshal(map[string]any{
		"model":      streamGuardModelName,
		"stream":     true,
		"max_tokens": 64,
		"messages":   []any{map[string]any{"role": "user", "content": "read the fixture"}},
		"tools": []any{map[string]any{
			"name":         "read_fixture",
			"input_schema": map[string]any{"type": "object"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(stub.proxy.URL+"/v1/messages", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, raw)
	}
	body := string(raw)
	if !strings.Contains(body, "event: message_stop") {
		t.Fatalf("the stream never finished: %s", body)
	}

	starts := anthropicBlockStarts(t, body)
	if len(starts) == 0 {
		t.Fatalf("no content blocks announced: %s", body)
	}
	for i, start := range starts {
		if start[0] != i {
			t.Fatalf("content blocks announced at indexes %v, want dense 0..%d in arrival order: %s", starts, len(starts)-1, body)
		}
	}
	if starts[0][1] != "tool_use" {
		t.Fatalf("first block type = %v, want tool_use for a pure tool turn: %s", starts[0][1], body)
	}
}

// The Responses twin of the same defect: output_index 1 was reserved for the
// message on the request alone, so a turn that asked for reasoning and got
// none streamed its text at index 1 while response.completed's output array
// held the message at position 0. The delta index and the array position have
// to agree without a reasoning item that was never announced.
func TestResponsesStreamUnfulfilledReasoningKeepsIndexZero(t *testing.T) {
	stub := newStubRemoteProxy(t, "reasoning never arrived, plain nonce answer")

	requestBody, err := json.Marshal(map[string]any{
		"model":  streamGuardModelName,
		"stream": true,
		"input":  "say the nonce",
		"reasoning": map[string]any{
			"effort": "medium",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(stub.proxy.URL+"/v1/responses", "application/json", bytes.NewReader(requestBody))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, raw)
	}
	body := string(raw)
	if !strings.Contains(body, "event: response.completed") {
		t.Fatalf("the stream never completed: %s", body)
	}

	deltaIndex := -1
	for _, block := range strings.Split(body, "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name != "response.output_text.delta" {
			continue
		}
		var payload struct {
			OutputIndex int `json:"output_index"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("unparseable delta: %v (%s)", err, data)
		}
		if deltaIndex == -1 {
			deltaIndex = payload.OutputIndex
		}
	}
	if deltaIndex == -1 {
		t.Fatalf("no text deltas announced: %s", body)
	}

	var completed struct {
		Response struct {
			Output []struct {
				Type string `json:"type"`
			} `json:"output"`
		} `json:"response"`
	}
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
			t.Fatalf("unparseable completed: %v (%s)", err, data)
		}
	}
	if len(completed.Response.Output) == 0 {
		t.Fatalf("completed output is empty: %s", body)
	}
	if completed.Response.Output[0].Type != "message" {
		t.Fatalf("completed output[0] = %s, want the message (no unannounced reasoning): %s", completed.Response.Output[0].Type, body)
	}
	if deltaIndex != 0 {
		t.Fatalf("text streamed at output_index %d while response.completed puts the message first: the reserved-but-empty reasoning slot desyncs the two views: %s", deltaIndex, body)
	}
}

// anthropicStreamIndexes summarizes one SSE body's block lifecycle: every
// content_block_start as (index, type), every stop's index, every delta's
// index.
type anthropicStreamIndexes struct {
	starts [][2]any
	stops  []int
	deltas []int
}

func parseAnthropicStreamIndexes(t *testing.T, body string) anthropicStreamIndexes {
	t.Helper()
	var out anthropicStreamIndexes
	for _, block := range strings.Split(body, "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		var payload struct {
			Index int `json:"index"`
			Block struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			continue
		}
		switch name {
		case "content_block_start":
			out.starts = append(out.starts, [2]any{payload.Index, payload.Block.Type})
		case "content_block_stop":
			out.stops = append(out.stops, payload.Index)
		case "content_block_delta":
			out.deltas = append(out.deltas, payload.Index)
		}
	}
	return out
}

// The four shapes one Anthropic stream can take, locked against the
// SDK-facing contract: announced blocks get dense indexes from 0 in
// announcement order, and every stop/delta lands on the index its block
// opened at.
func TestAnthropicStreamBlocksNumberIndexesInAnnouncementOrder(t *testing.T) {
	calls := []toolemulation.ToolCall{
		{ID: "call_1", Name: "read_fixture", Arguments: map[string]any{"path": "a.txt"}},
		{ID: "call_2", Name: "read_fixture", Arguments: map[string]any{"path": "b.txt"}},
	}

	t.Run("tools only", func(t *testing.T) {
		rec := httptest.NewRecorder()
		b := &anthropicStreamBlocks{w: rec, flusher: rec}
		if !b.FinishToolCalls(calls) {
			t.Fatal("client gone")
		}
		idx := parseAnthropicStreamIndexes(t, rec.Body.String())
		want := [][2]any{{0, "tool_use"}, {1, "tool_use"}}
		if !reflect.DeepEqual(idx.starts, want) {
			t.Fatalf("starts = %v, want %v", idx.starts, want)
		}
		if len(idx.stops) != 2 || idx.stops[0] != 0 || idx.stops[1] != 1 {
			t.Fatalf("stops = %v, want one per block at its own index", idx.stops)
		}
	})

	t.Run("text only", func(t *testing.T) {
		rec := httptest.NewRecorder()
		b := &anthropicStreamBlocks{w: rec, flusher: rec}
		if !b.TextDelta("answer") || !b.CloseAnnounced() {
			t.Fatal("client gone")
		}
		idx := parseAnthropicStreamIndexes(t, rec.Body.String())
		if len(idx.starts) != 1 || idx.starts[0][0] != 0 || idx.starts[0][1] != "text" {
			t.Fatalf("starts = %v, want text at 0", idx.starts)
		}
		if len(idx.stops) != 1 || idx.stops[0] != 0 {
			t.Fatalf("stops = %v, want the text stop at 0", idx.stops)
		}
	})

	t.Run("reasoning requested but empty", func(t *testing.T) {
		rec := httptest.NewRecorder()
		b := &anthropicStreamBlocks{w: rec, flusher: rec}
		// No ThinkingDelta ever fires: the backend produced no thought.
		if !b.TextDelta("answer") || !b.CloseAnnounced() || !b.FinishToolCalls(calls) {
			t.Fatal("client gone")
		}
		idx := parseAnthropicStreamIndexes(t, rec.Body.String())
		want := [][2]any{{0, "text"}, {1, "tool_use"}, {2, "tool_use"}}
		if !reflect.DeepEqual(idx.starts, want) {
			t.Fatalf("starts = %v, want the never-announced thinking block to consume no index", idx.starts)
		}
	})

	t.Run("reasoning real with interleaved text and tools", func(t *testing.T) {
		rec := httptest.NewRecorder()
		b := &anthropicStreamBlocks{w: rec, flusher: rec}
		if !b.ThinkingDelta("chain") || !b.TextDelta("answer") || !b.CloseAnnounced() || !b.FinishToolCalls(calls) {
			t.Fatal("client gone")
		}
		idx := parseAnthropicStreamIndexes(t, rec.Body.String())
		want := [][2]any{{0, "thinking"}, {1, "text"}, {2, "tool_use"}, {3, "tool_use"}}
		if !reflect.DeepEqual(idx.starts, want) {
			t.Fatalf("starts = %v, want %v", idx.starts, want)
		}
		for _, delta := range idx.deltas {
			if delta < 0 || delta > 3 {
				t.Fatalf("delta index %d outside announced blocks", delta)
			}
		}
	})
}

// A reasoning item only the final frame knows about (aggregate mode, or a
// thought that surfaced only in the result) is replayed at index 0 ahead of
// the message, so the event stream and response.completed agree instead of
// the array carrying an item the stream never announced.
func TestStreamCompletedReplaysUnstreamedReasoningAtIndexZero(t *testing.T) {
	rec := httptest.NewRecorder()
	emitter := newOpenAIResponseStreamEmitter(rec, rec, "resp_1")
	result := &service.ChatResult{
		Model:       "kmodel",
		Text:        "final answer",
		ThoughtText: "final-frame thought",
	}

	writeOpenAIResponseStreamCompleted(emitter, "resp_1", 123, "kmodel", result, "msg_1", false, true, false, "")

	reasoningIndexes, messageIndexes := []int{}, []int{}
	for _, block := range strings.Split(rec.Body.String(), "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		var payload struct {
			OutputIndex int `json:"output_index"`
			Item        struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		_ = json.Unmarshal([]byte(data), &payload)
		switch name {
		case "response.output_item.added":
			switch payload.Item.Type {
			case "reasoning":
				reasoningIndexes = append(reasoningIndexes, payload.OutputIndex)
			case "message":
				messageIndexes = append(messageIndexes, payload.OutputIndex)
			}
		case "response.reasoning_summary_text.delta":
			reasoningIndexes = append(reasoningIndexes, payload.OutputIndex)
		case "response.output_text.delta":
			messageIndexes = append(messageIndexes, payload.OutputIndex)
		}
	}
	if len(reasoningIndexes) == 0 || reasoningIndexes[0] != 0 {
		t.Fatalf("the unstreamed reasoning item must replay at index 0, got %v: %s", reasoningIndexes, rec.Body.String())
	}
	for _, idx := range messageIndexes {
		if idx != 1 {
			t.Fatalf("message events must follow the replayed reasoning at index 1, got %v: %s", messageIndexes, rec.Body.String())
		}
	}
	var completed struct {
		Response struct {
			Output []struct {
				Type string `json:"type"`
			} `json:"output"`
		} `json:"response"`
	}
	for _, block := range strings.Split(rec.Body.String(), "\n\n") {
		var name, data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			} else if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "response.completed" {
			if err := json.Unmarshal([]byte(data), &completed); err != nil {
				t.Fatalf("unparseable completed: %v", err)
			}
		}
	}
	if len(completed.Response.Output) != 2 ||
		completed.Response.Output[0].Type != "reasoning" ||
		completed.Response.Output[1].Type != "message" {
		t.Fatalf("completed output = %#v, want [reasoning, message] in stream order", completed.Response.Output)
	}
}
