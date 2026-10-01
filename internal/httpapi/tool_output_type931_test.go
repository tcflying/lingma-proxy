package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A tool result is kept only when its content is structurally valid: a
// string, or an array of content blocks -- each an object with a non-empty
// string type whose text field, when present, is a string. Legal empty
// results (empty string, empty array, empty text, text-less blocks) keep the
// call pairing; any malformed shape is dropped rather than fabricated into a
// successful empty result, and one malformed block invalidates the whole
// array so no partial result sneaks through.

func toolOutputType931InvalidShapes() []any {
	return []any{
		123,
		true,
		[]any{nil},
		[]any{123},
		[]any{false},
		[]any{map[string]any{}},
		[]any{map[string]any{"type": "text", "text": false}},
		[]any{map[string]any{"type": "text"}},
		[]any{map[string]any{"type": "text", "text": "partial ok 931"}, map[string]any{"type": "text", "text": false}},
	}
}

func TestToolOutputType931InvalidShapesDroppedBothEntries(t *testing.T) {
	for _, content := range toolOutputType931InvalidShapes() {
		req, err := normalizeOpenAIRequest(context.Background(), openAIChatRequest{Messages: []rawMessage{
			{Role: "user", Content: "continue"},
			{Role: "tool", ToolCallID: "call_invalid_931", Content: content},
		}})
		if err != nil {
			t.Fatalf("chat normalize rejected the whole request for %#v: %v", content, err)
		}
		for _, message := range req.Messages {
			if message.Role == "tool" {
				t.Errorf("chat treated malformed content %#v as a tool result", content)
			}
		}
		_, results := extractAnthropicUserContent([]any{map[string]any{
			"type": "tool_result", "tool_use_id": "toolu_invalid_931", "content": content,
		}})
		if len(results) != 0 {
			t.Errorf("anthropic treated malformed content %#v as a tool result", content)
		}
	}
}

func TestToolOutputType931LegalShapesKeptBothEntries(t *testing.T) {
	cases := []any{
		"",
		"plain payload 931",
		[]any{},
		[]any{map[string]any{"type": "text", "text": ""}},
		[]any{map[string]any{"type": "text", "text": "array payload 931"}},
		[]any{map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": "aGk=",
		}}},
	}
	for _, content := range cases {
		req, err := normalizeOpenAIRequest(context.Background(), openAIChatRequest{Messages: []rawMessage{
			{Role: "user", Content: "continue"},
			{Role: "tool", ToolCallID: "call_legal_931", Content: content},
		}})
		if err != nil {
			t.Fatalf("chat normalize rejected the whole request for %#v: %v", content, err)
		}
		kept := false
		for _, message := range req.Messages {
			if message.Role == "tool" {
				kept = true
			}
		}
		if !kept {
			t.Errorf("chat dropped the legal result shape %#v", content)
		}
		_, results := extractAnthropicUserContent([]any{map[string]any{
			"type": "tool_result", "tool_use_id": "toolu_legal_931", "content": content,
		}})
		if len(results) != 1 {
			t.Errorf("anthropic dropped the legal result shape %#v", content)
		}
	}
}

func toolType931ChatBody(t *testing.T, toolMessages ...map[string]any) []byte {
	t.Helper()
	messages := []any{
		map[string]any{"role": "user", "content": "read alpha"},
		map[string]any{
			"role": "assistant",
			"tool_calls": []any{
				map[string]any{"id": "call_num_931", "type": "function", "function": map[string]any{"name": "read_fixture", "arguments": `{"path":"alpha.txt"}`}},
				map[string]any{"id": "call_bool_931", "type": "function", "function": map[string]any{"name": "read_fixture", "arguments": `{"path":"beta.txt"}`}},
				map[string]any{"id": "call_empty_931", "type": "function", "function": map[string]any{"name": "read_fixture", "arguments": `{"path":"gamma.txt"}`}},
			},
		},
	}
	for _, tm := range toolMessages {
		messages = append(messages, tm)
	}
	messages = append(messages, map[string]any{"role": "user", "content": "continue"})
	raw, err := json.Marshal(map[string]any{
		"model":    ns931Model,
		"messages": messages,
		"tools":    ns931FixtureTools(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestToolOutputType931ChatMalformedBlockArrayNotFabricated(t *testing.T) {
	proxy, log := ns931NewEnv(t, ns931ProseOnlyPieces)
	code, body := ns931Post(t, proxy, "/v1/chat/completions", toolType931ChatBody(t,
		map[string]any{"role": "tool", "tool_call_id": "call_num_931", "content": []any{123}},
		map[string]any{"role": "tool", "tool_call_id": "call_bool_931", "content": []any{
			map[string]any{"type": "text", "text": false},
		}},
	))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	upstream := log.joined()
	if strings.Contains(upstream, ns931EmptyMarker) {
		t.Fatalf("chat entry fabricated an empty result for malformed block arrays; upstream prompt follows.\n%s", upstream)
	}
	for _, id := range []string{"call_num_931", "call_bool_931"} {
		if strings.Contains(upstream, "Tool result for "+id) {
			t.Fatalf("chat entry paired a malformed block array as a result for %q; upstream prompt follows.\n%s", id, upstream)
		}
	}
}

func TestToolOutputType931ChatMixedBlockDropsWholeResult(t *testing.T) {
	proxy, log := ns931NewEnv(t, ns931ProseOnlyPieces)
	code, body := ns931Post(t, proxy, "/v1/chat/completions", toolType931ChatBody(t,
		map[string]any{"role": "tool", "tool_call_id": "call_bool_931", "content": []any{
			map[string]any{"type": "text", "text": "partial ok 931"},
			map[string]any{"type": "text", "text": false},
		}},
	))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	upstream := log.joined()
	if strings.Contains(upstream, "Tool result for call_bool_931") {
		t.Fatalf("chat entry kept a partially malformed array as a result; upstream prompt follows.\n%s", upstream)
	}
	if strings.Contains(upstream, "partial ok 931") {
		t.Fatalf("chat entry smuggled the valid half of a malformed array into the prompt; upstream prompt follows.\n%s", upstream)
	}
}

func TestToolOutputType931ChatLegalArrayShapesKept(t *testing.T) {
	proxy, log := ns931NewEnv(t, ns931ProseOnlyPieces)
	code, body := ns931Post(t, proxy, "/v1/chat/completions", toolType931ChatBody(t,
		map[string]any{"role": "tool", "tool_call_id": "call_empty_931", "content": []any{}},
		map[string]any{"role": "tool", "tool_call_id": "call_bool_931", "content": []any{
			map[string]any{"type": "text", "text": "array payload 931"},
		}},
	))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	upstream := log.joined()
	if !strings.Contains(upstream, "Tool result for call_empty_931") || !strings.Contains(upstream, ns931EmptyMarker) {
		t.Fatalf("chat entry dropped the legal empty content array as a result; upstream prompt follows.\n%s", upstream)
	}
	if !strings.Contains(upstream, "Tool result for call_bool_931") || !strings.Contains(upstream, "array payload 931") {
		t.Fatalf("chat entry lost the text carried by a content array; upstream prompt follows.\n%s", upstream)
	}
}

func toolType931AnthropicBody(t *testing.T, toolResults ...map[string]any) []byte {
	t.Helper()
	toolUses := []any{
		map[string]any{"type": "tool_use", "id": "toolu_num_931", "name": "read_fixture", "input": map[string]any{"path": "alpha.txt"}},
		map[string]any{"type": "tool_use", "id": "toolu_empty_931", "name": "read_fixture", "input": map[string]any{"path": "beta.txt"}},
	}
	userContent := make([]any, 0, len(toolResults)+1)
	for _, tr := range toolResults {
		userContent = append(userContent, tr)
	}
	userContent = append(userContent, map[string]any{"type": "text", "text": "continue"})
	raw, err := json.Marshal(map[string]any{
		"model":      ns931Model,
		"max_tokens": 1024,
		"messages": []any{
			map[string]any{"role": "user", "content": "read alpha"},
			map[string]any{"role": "assistant", "content": toolUses},
			map[string]any{"role": "user", "content": userContent},
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
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestToolOutputType931AnthropicMalformedShapesNotFabricated(t *testing.T) {
	proxy, log := ns931NewEnv(t, ns931ProseOnlyPieces)
	code, body := ns931Post(t, proxy, "/v1/messages", toolType931AnthropicBody(t,
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_num_931", "content": []any{map[string]any{}}},
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_empty_931", "content": []any{
			map[string]any{"type": "text", "text": "partial ok 931"},
			map[string]any{"type": "text", "text": false},
		}},
	))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	upstream := log.joined()
	if strings.Contains(upstream, ns931EmptyMarker) {
		t.Fatalf("anthropic entry fabricated an empty result for malformed tool_result contents; upstream prompt follows.\n%s", upstream)
	}
	if strings.Contains(upstream, "partial ok 931") {
		t.Fatalf("anthropic entry smuggled the valid half of a malformed array into the prompt; upstream prompt follows.\n%s", upstream)
	}
	for _, id := range []string{"toolu_num_931", "toolu_empty_931"} {
		if strings.Contains(upstream, "Tool result for "+id) {
			t.Fatalf("anthropic entry paired a malformed tool_result content as a result for %q; upstream prompt follows.\n%s", id, upstream)
		}
	}
}

func TestToolOutputType931AnthropicLegalShapesKept(t *testing.T) {
	proxy, log := ns931NewEnv(t, ns931ProseOnlyPieces)
	code, body := ns931Post(t, proxy, "/v1/messages", toolType931AnthropicBody(t,
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_num_931", "content": ""},
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_empty_931", "content": []any{}},
		map[string]any{"type": "tool_result", "tool_use_id": "toolu_empty_931", "content": []any{map[string]any{"type": "text", "text": ""}}},
	))
	if code != http.StatusOK {
		t.Fatalf("status = %d body = %s", code, body)
	}
	upstream := log.joined()
	for _, id := range []string{"toolu_num_931", "toolu_empty_931"} {
		if !strings.Contains(upstream, "Tool result for "+id) || !strings.Contains(upstream, ns931EmptyMarker) {
			t.Fatalf("anthropic entry dropped a legal empty tool_result shape for %q; upstream prompt follows.\n%s", id, upstream)
		}
	}
}
