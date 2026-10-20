package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"lingma-ipc-proxy/internal/service"
)

// 1005 E-3 (HIST-05/HIST-06): Anthropic tool_result content carried images and
// an is_error flag; the conversion dropped both, downgrading a screenshot
// result to an empty output and erasing the failure semantics.

func e3AnthropicRequestBody(t *testing.T, toolResultBlocks []any, isError any) map[string]any {
	t.Helper()
	return map[string]any{
		"model":      "claude-3",
		"max_tokens": 100,
		"messages": []any{
			map[string]any{"role": "user", "content": "look at the screen"},
			map[string]any{
				"role": "assistant",
				"content": []any{map[string]any{
					"type": "tool_use", "id": "toolu_1", "name": "screenshot",
					"input": map[string]any{},
				}},
			},
			map[string]any{
				"role": "user",
				"content": []any{map[string]any{
					"type":        "tool_result",
					"tool_use_id": "toolu_1",
					"content":     toolResultBlocks,
					"is_error":    isError,
				}},
			},
		},
	}
}

func e3Normalize(t *testing.T, body map[string]any) (service.ChatRequest, error) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var req anthropicRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return normalizeAnthropicRequest(req)
}

func TestE3ToolResult1005ImagesPreserved(t *testing.T) {
	body := e3AnthropicRequestBody(t, []any{
		map[string]any{"type": "text", "text": "screenshot of the failure"},
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": "aWNvbg==",
		}},
	}, nil)
	converted, err := e3Normalize(t, body)
	if err != nil {
		t.Fatalf("normalizeAnthropicRequest: %v", err)
	}
	found := false
	for _, message := range converted.Messages {
		if strings.EqualFold(message.Role, "tool") && message.ToolCallID == "toolu_1" {
			found = true
			if !strings.Contains(message.Text, "screenshot of the failure") {
				t.Errorf("tool result text lost: %q", message.Text)
			}
			if len(message.Images) != 1 {
				t.Fatalf("HIST-05: tool_result image dropped, images=%d", len(message.Images))
			}
			if message.Images[0].Data != "aWNvbg==" || message.Images[0].MediaType != "image/png" {
				t.Errorf("image payload mutated: %+v", message.Images[0])
			}
		}
	}
	if !found {
		t.Fatal("tool result message not converted at all")
	}
}

func TestE3ToolResult1005IsErrorMarker(t *testing.T) {
	failed, err := e3Normalize(t, e3AnthropicRequestBody(t, []any{
		map[string]any{"type": "text", "text": "exit status 1"},
	}, true))
	if err != nil {
		t.Fatalf("normalizeAnthropicRequest: %v", err)
	}
	ok, err := e3Normalize(t, e3AnthropicRequestBody(t, []any{
		map[string]any{"type": "text", "text": "exit status 1"},
	}, false))
	if err != nil {
		t.Fatalf("normalizeAnthropicRequest: %v", err)
	}
	failedText, okText := "", ""
	for _, message := range failed.Messages {
		if strings.EqualFold(message.Role, "tool") {
			failedText = message.Text
		}
	}
	for _, message := range ok.Messages {
		if strings.EqualFold(message.Role, "tool") {
			okText = message.Text
		}
	}
	if !strings.Contains(failedText, "[Tool execution FAILED]") {
		t.Errorf("HIST-06: is_error=true carries no failure marker: %q", failedText)
	}
	if !strings.Contains(failedText, "exit status 1") {
		t.Errorf("is_error=true lost the original result text: %q", failedText)
	}
	if strings.Contains(okText, "[Tool execution FAILED]") {
		t.Errorf("is_error=false must not carry the failure marker (counter-case): %q", okText)
	}
}

// TestE3ToolResult1005ImageOnlyResultSurvives pins the empty-output downgrade:
// a tool_result whose content is only an image must stay a legal result message
// carrying that image, not an empty output with the attachment silently gone.
func TestE3ToolResult1005ImageOnlyResultSurvives(t *testing.T) {
	body := e3AnthropicRequestBody(t, []any{
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/jpeg", "data": "anBnZGF0YQ==",
		}},
	}, nil)
	converted, err := e3Normalize(t, body)
	if err != nil {
		t.Fatalf("normalizeAnthropicRequest: %v", err)
	}
	for _, message := range converted.Messages {
		if strings.EqualFold(message.Role, "tool") && message.ToolCallID == "toolu_1" {
			if len(message.Images) != 1 || message.Images[0].Data != "anBnZGF0YQ==" {
				t.Fatalf("image-only tool_result lost its attachment: %+v", message)
			}
			return
		}
	}
	t.Fatal("tool result message not converted at all")
}
