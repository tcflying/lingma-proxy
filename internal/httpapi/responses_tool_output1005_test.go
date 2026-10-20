package httpapi

import (
	"context"
	"encoding/json"
	"lingma-ipc-proxy/internal/remote"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResponsesToolOutput1005DirectRemoteControl(t *testing.T) {
	f := newHistory1005Fixture(t)
	auth := filepath.Join(t.TempDir(), "synthetic-direct.json")
	if err := os.WriteFile(auth, []byte(`{"source":"synthetic-direct","auth":{"cosy_key":"synthetic-key","encrypt_user_info":"synthetic-info","user_id":"synthetic-user","machine_id":"synthetic-machine"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	client := remote.New(remote.Config{BaseURL: "http://history1005.invalid", AuthFile: auth})
	_, err := client.Chat(context.Background(), remote.ChatRequest{Model: "kmodel", Messages: []remote.Message{{Role: "user", Content: "control"}, {Role: "tool", ToolCallID: "direct-empty", Content: ""}, {Role: "tool", ToolCallID: "direct-text", Content: "nonempty"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.requests) != 1 {
		t.Fatalf("direct requests=%d", len(f.requests))
	}
	tools := upstreamTools1005(t, f.requests[0])
	empty, present := tools["direct-empty"]
	if !present || empty != "" || tools["direct-text"] != "nonempty" {
		t.Fatalf("direct remote control changed: %#v", tools)
	}
}

func output1005(id string, output any) map[string]any {
	return map[string]any{"type": "function_call_output", "call_id": id, "output": output}
}
func block1005(kind string, text any) any { return map[string]any{"type": kind, "text": text} }
func upstreamTools1005(t *testing.T, body string) map[string]string {
	t.Helper()
	var payload struct {
		Messages []struct {
			Role    string  `json:"role"`
			ID      string  `json:"tool_call_id"`
			Content *string `json:"content"`
		}
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, m := range payload.Messages {
		if m.Role == "tool" {
			if _, exists := out[m.ID]; exists {
				t.Fatalf("duplicate tool id %q", m.ID)
			}
			if m.Content == nil {
				t.Fatalf("tool %q omitted content or emitted null", m.ID)
			}
			out[m.ID] = *m.Content
		}
	}
	return out
}
func TestResponsesToolOutput1005Projection(t *testing.T) {
	f := newHistory1005Fixture(t)
	cases := []struct {
		name   string
		output any
		want   string
	}{
		{"string", " real ", "real"},
		{"text-array", []any{block1005("text", " first "), block1005("input_text", "second"), block1005("output_text", " third ")}, "first\nsecond\nthird"},
		{"empty-string", "", ""}, {"whitespace", "  ", ""}, {"empty-array", []any{}, ""}, {"empty-text", []any{block1005("text", "")}, ""},
	}
	for _, c := range cases {
		for _, route := range []string{"/v1/responses", "/api/v1/responses"} {
			for _, stream := range []bool{false, true} {
				for _, shape := range []string{"singleton", "array", "history"} {
					var input any = output1005("call_result", c.output)
					if shape == "array" {
						input = []any{input}
					}
					if shape == "history" {
						input = []any{map[string]any{"role": "user", "content": "original-task"}, map[string]any{"type": "function_call", "call_id": "call_result", "name": "read", "arguments": "{}"}, map[string]any{"type": "function_call", "call_id": "call_other", "name": "read", "arguments": "{}"}, output1005("call_other", "other-marker"), input}
					}
					for _, none := range []bool{false, true} {
						body := map[string]any{"model": "kmodel", "input": input, "stream": stream}
						if none {
							body["tools"] = []any{map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object"}}}
							body["tool_choice"] = "none"
						}
						before := len(f.requests)
						w := f.post(t, route, body)
						if w.Code != 200 || len(f.requests) != before+1 {
							t.Errorf("%s %s stream=%v %s none=%v: status=%d requests=%d body=%s", c.name, route, stream, shape, none, w.Code, len(f.requests)-before, w.Body.String())
							continue
						}
						tools := upstreamTools1005(t, f.requests[before])
						value, present := tools["call_result"]
						if !present || value != c.want {
							t.Errorf("%s %s stream=%v %s none=%v: lost result/id: %#v", c.name, route, stream, shape, none, tools)
						}
						if shape == "history" && tools["call_other"] != "other-marker" {
							t.Errorf("second ID lost: %#v", tools)
						}
					}
				}
			}
		}
	}
}
func TestResponsesToolOutput1005RejectArrays(t *testing.T) {
	f := newHistory1005Fixture(t)
	bad := []any{[]any{42}, []any{map[string]any{}}, []any{block1005("text", 12)}, []any{map[string]any{"type": "text"}}, []any{block1005("unknown", "lost")}, []any{map[string]any{"type": "input_image", "image_url": "http://127.0.0.1/forbidden"}}, []any{map[string]any{"type": "input_file", "file_url": "http://127.0.0.1/forbidden"}}, []any{block1005("text", "keep"), block1005("image", "no")}}
	for _, value := range bad {
		for _, route := range []string{"/v1/responses", "/api/v1/responses"} {
			for _, stream := range []bool{false, true} {
				for _, single := range []bool{false, true} {
					var input any = output1005("call_bad", value)
					if !single {
						input = []any{map[string]any{"role": "user", "content": "still reject mixed"}, input}
					}
					before := len(f.requests)
					w := f.post(t, route, map[string]any{"model": "kmodel", "input": input, "stream": stream})
					if w.Code != 400 || len(f.requests) != before || f.images != 0 || strings.Contains(w.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(w.Body.String(), "invalid_request_error") {
						t.Errorf("unsupported array must reject before IO: status=%d upstream=%d images=%d", w.Code, len(f.requests)-before, f.images)
					}
				}
			}
		}
	}
}
func TestResponsesToolOutput1005LegacyDrops(t *testing.T) {
	f := newHistory1005Fixture(t)
	bad := []any{map[string]any{"type": "function_call_output", "call_id": "missing"}, output1005("null", nil), output1005("number", 42), output1005("bool", true), output1005("object", map[string]any{}), output1005("", ""), output1005("   ", []any{}), map[string]any{"type": "function_call_output", "id": "not-call-id", "output": "not-result"}}
	for _, item := range bad {
		before := len(f.requests)
		w := f.post(t, "/v1/responses", map[string]any{"model": "kmodel", "input": []any{map[string]any{"role": "user", "content": "valid control"}, item}})
		if w.Code != 200 || len(f.requests) != before+1 {
			t.Fatalf("legacy mixed control status=%d", w.Code)
		}
		if tools := upstreamTools1005(t, f.requests[before]); len(tools) != 0 {
			t.Fatalf("fabricated result %#v", tools)
		}
	}
}
