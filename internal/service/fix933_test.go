package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"lingma-ipc-proxy/internal/lingmaipc"
	"lingma-ipc-proxy/internal/remote"
	"lingma-ipc-proxy/internal/toolemulation"
)

func fix933Tools() []toolemulation.ToolDef {
	return []toolemulation.ToolDef{{
		Name:        "read_fixture",
		Description: "read one fixture file",
		InputSchema: map[string]any{"type": "object"},
	}}
}

// P3-S1: the forced-tooling retry answered in the action-block dialect used to be
// thrown away. The first attempt gets a dialect fallback (932), the retry did
// not, so a model that obeys the forced request in the dialect the prompt taught
// it spends a whole upstream round trip and the client still gets the first
// attempt's "let me ..." prose.
func TestGenerateRemoteWithModelNativeRetryAcceptsDialectAnswer(t *testing.T) {
	svc := New(Config{Model: "kmodel", Timeout: time.Second})
	wire := &wireCollector{}
	client := &scriptedChatClient{results: []*remote.ChatResult{
		// A continuation cue under the 180-rune bound, so the native retry fires.
		{Text: "Let me read the fixture:"},
		{Text: "```json action\n{\"tool\":\"read_fixture\",\"parameters\":{\"path\":\"alpha.txt\"}}\n```"},
	}, wire: wire}
	req := ChatRequest{
		Tools:      fix933Tools(),
		ToolChoice: toolemulation.ToolChoice{Mode: "any"},
		Messages:   []ChatMessage{{Role: "user", Text: "read the alpha fixture"}},
	}

	result, _, err := svc.generateRemoteWithModel(context.Background(), client, req, "", "read the alpha fixture", "kmodel", wire.onDelta, true)
	if err != nil {
		t.Fatalf("generateRemoteWithModel: %v", err)
	}
	if len(client.requests) != 2 {
		t.Fatalf("chat calls = %d, want the initial attempt plus the native retry, no third round: %#v", len(client.requests), client.requests)
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "read_fixture" {
		t.Fatalf("result = %#v, want the retried action block converted into a call", result)
	}
	if path, _ := result.ToolCalls[0].Arguments["path"].(string); path != "alpha.txt" {
		t.Fatalf("converted arguments = %#v, want the block's own path", result.ToolCalls[0].Arguments)
	}
	if strings.Contains(result.Text, "json action") {
		t.Fatalf("text = %q, want the accepted action block stripped out of the prose", result.Text)
	}
	if got := wire.streamed.String(); got != "" {
		t.Fatalf("streamed %q, want nothing: the retry superseded the held first attempt", got)
	}
}

// P3-S8: the dialect conversion entry has no service-level test, so the hold and
// the emulation path around it are only covered end to end through httpapi. This
// is the production flag combination (emulateTools true, which is what
// generateRemoteInternal always passes for a tool request).
//
// The fixture is shaped to isolate that entry rather than applyToolEmulation,
// which converts the same block later and hands back the same answer: the
// leading prose carries a continuation cue and a colon, so an unconverted first
// attempt also trips the native-retry gate and costs a second call. One call is
// therefore what says the conversion ran before the retry was weighed, which is
// the thing 932 put there for.
func TestGenerateRemoteWithModelConvertsDialectAnswerOnTheFirstAttempt(t *testing.T) {
	svc := New(Config{Model: "kmodel", Timeout: time.Second})
	const dialect = "```json action\n{\"tool\":\"read_fixture\",\"parameters\":{\"path\":\"alpha.txt\"}}\n```"
	const answer = "让我读取这个 fixture：\n" + dialect
	wire := &wireCollector{}
	client := &scriptedChatClient{results: []*remote.ChatResult{
		{Text: answer},
		// Reached only when the conversion did not run, where the call count says so.
		{Text: answer},
	}, wire: wire}
	req := ChatRequest{
		Tools:      fix933Tools(),
		ToolChoice: toolemulation.ToolChoice{Mode: "auto"},
		Messages:   []ChatMessage{{Role: "user", Text: "read the alpha fixture"}},
	}

	result, emitted, err := svc.generateRemoteWithModel(context.Background(), client, req, "", "read the alpha fixture", "kmodel", wire.onDelta, true)
	if err != nil {
		t.Fatalf("generateRemoteWithModel: %v", err)
	}
	if len(client.requests) != 1 {
		t.Fatalf("chat calls = %d, want exactly one: a converted call is already a legal native call and must not be retried", len(client.requests))
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "read_fixture" {
		t.Fatalf("result = %#v, want the first attempt's action block converted", result)
	}
	if result.Text != "让我读取这个 fixture：" {
		t.Fatalf("text = %q, want the block removed and the prose kept", result.Text)
	}
	// The hold releases the deltas the conversion accepted; the httpapi stream
	// filter is what keeps the raw block off the wire, so the service must not
	// swallow the attempt.
	if got := wire.streamed.String(); got != answer {
		t.Fatalf("streamed %q, want the held first attempt released verbatim", got)
	}
	if !emitted {
		t.Fatal("emitted = false, want the surviving attempt on the wire")
	}
	if wire.duringCall {
		t.Fatal("the first attempt streamed while its Chat call was still running; a tool turn must hold it back")
	}
}

// P3-S2: the dedupe key was the raw marshalled map, where a nil map and an empty
// map are "null" and "{}" -- so a native no-argument call and the same call
// echoed as a dialect block that omits "parameters" were two calls, and the
// no-argument tool ran twice.
func TestAppendConvertedToolCallsTreatsNilAndEmptyArgumentsAsTheSameCall(t *testing.T) {
	tests := []struct {
		name      string
		existing  []toolemulation.ToolCall
		converted []toolemulation.ToolCall
		want      int
	}{
		{
			name:      "no arguments on both sides is one call",
			existing:  []toolemulation.ToolCall{{ID: "call_native", Name: "list_dir", Arguments: map[string]any{}}},
			converted: []toolemulation.ToolCall{{ID: "call_dialect", Name: "list_dir", Arguments: nil}},
			want:      1,
		},
		{
			name:      "key order does not make a second call",
			existing:  []toolemulation.ToolCall{{Name: "read_file", Arguments: map[string]any{"path": "go.mod", "line": float64(1)}}},
			converted: []toolemulation.ToolCall{{Name: "read_file", Arguments: map[string]any{"line": float64(1), "path": "go.mod"}}},
			want:      1,
		},
		{
			name:      "numeric spellings that marshal alike are one call",
			existing:  []toolemulation.ToolCall{{Name: "read_file", Arguments: map[string]any{"line": 1}}},
			converted: []toolemulation.ToolCall{{Name: "read_file", Arguments: map[string]any{"line": float64(1)}}},
			want:      1,
		},
		{
			name:      "a different tool is not an echo",
			existing:  []toolemulation.ToolCall{{ID: "call_native", Name: "list_dir", Arguments: map[string]any{}}},
			converted: []toolemulation.ToolCall{{ID: "call_dialect", Name: "read_file", Arguments: nil}},
			want:      2,
		},
		{
			name:      "a different argument is not an echo",
			existing:  []toolemulation.ToolCall{{Name: "read_file", Arguments: map[string]any{"path": "go.mod"}}},
			converted: []toolemulation.ToolCall{{Name: "read_file", Arguments: map[string]any{"path": "main.go"}}},
			want:      2,
		},
		{
			name:      "a distinct converted call still joins",
			existing:  []toolemulation.ToolCall{{ID: "call_native", Name: "read_file", Arguments: map[string]any{"path": "go.mod"}}},
			converted: []toolemulation.ToolCall{{ID: "call_dialect", Name: "list_dir", Arguments: map[string]any{}}},
			want:      2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := appendConvertedToolCalls(tc.existing, tc.converted)
			if len(got) != tc.want {
				t.Fatalf("appendConvertedToolCalls = %#v, want %d call(s)", got, tc.want)
			}
			if len(tc.existing) > 0 && got[0].ID != tc.existing[0].ID {
				t.Fatalf("first call = %q, want the turn's own call %q to survive", got[0].ID, tc.existing[0].ID)
			}
		})
	}
}

// P3-S6: LooksLikeMissedToolUse is a bag of narration words, and the retry it
// triggered ignored tool_choice. In auto -- the mode most clients send -- an
// ordinary descriptive sentence ("I will read the fixture.") bought a second
// full upstream turn for nothing.
func TestGenerateRemoteWithModelAutoModeDoesNotRetryOnNarration(t *testing.T) {
	svc := New(Config{Model: "kmodel", Timeout: time.Second})
	client := &scriptedChatClient{results: []*remote.ChatResult{
		{Text: "I will read the fixture."},
	}}
	req := ChatRequest{
		Tools:      fix933Tools(),
		ToolChoice: toolemulation.ToolChoice{Mode: "auto"},
		Messages:   []ChatMessage{{Role: "user", Text: "read the alpha fixture"}},
	}

	result, _, err := svc.generateRemoteWithModel(context.Background(), client, req, "", "read the alpha fixture", "kmodel", nil, true)
	if err != nil {
		t.Fatalf("generateRemoteWithModel: %v", err)
	}
	if len(client.requests) != 1 {
		t.Fatalf("chat calls = %d, want 1: tool_choice auto does not retry on narration", len(client.requests))
	}
	if result.Text != "I will read the fixture." {
		t.Fatalf("text = %q, want the answer untouched", result.Text)
	}
}

// The half of the same rule that must not regress: an explicit refusal is not
// narration, and a client in auto mode that gets one is owed the retry.
func TestGenerateRemoteWithModelAutoModeStillRetriesARefusal(t *testing.T) {
	svc := New(Config{Model: "kmodel", Timeout: time.Second})
	client := &scriptedChatClient{results: []*remote.ChatResult{
		{Text: "i don't have tools"},
		{Text: "```json action\n{\"tool\":\"read_fixture\",\"parameters\":{\"path\":\"alpha.txt\"}}\n```"},
	}}
	req := ChatRequest{
		Tools:      fix933Tools(),
		ToolChoice: toolemulation.ToolChoice{Mode: "auto"},
		Messages:   []ChatMessage{{Role: "user", Text: "read the alpha fixture"}},
	}

	result, _, err := svc.generateRemoteWithModel(context.Background(), client, req, "", "read the alpha fixture", "kmodel", nil, true)
	if err != nil {
		t.Fatalf("generateRemoteWithModel: %v", err)
	}
	if len(client.requests) != 2 {
		t.Fatalf("chat calls = %d, want the retry a refusal still earns", len(client.requests))
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "read_fixture" {
		t.Fatalf("result = %#v, want the retried block converted", result)
	}
}

// P3-S3: a remote-backed turn carrying a picture with no tools was diverted to
// the local IPC pipe, so on a headless deployment (a server bundle has no
// desktop to dial) every image request failed, and the remote client's own
// image projection -- the image_urls its payload's is_vl switch keys off -- was
// unreachable on the request path. The picture now rides the remote request.
func TestGenerateRemoteSendsImagesToTheGatewayWithoutTheLocalIPC(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		t.Setenv(key, "")
	}
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")

	var mu sync.Mutex
	var bodies [][]byte
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
			return
		}
		mu.Lock()
		bodies = append(bodies, raw)
		mu.Unlock()
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"body":"{\"choices\":[{\"delta\":{\"content\":\"go.mod\"},\"finish_reason\":\"stop\"}]}","statusCodeValue":200}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	authFile := filepath.Join(t.TempDir(), "credentials.json")
	credential := `{"source":"test","token_expire_time":"4102444800000","auth":{` +
		`"cosy_key":"cosy-key-value","encrypt_user_info":"encrypted-user-info",` +
		`"user_id":"user-123456","machine_id":"machine-1234567890ab"}}`
	if err := os.WriteFile(authFile, []byte(credential), 0600); err != nil {
		t.Fatal(err)
	}

	svc := New(Config{
		Backend:        BackendRemote,
		RemoteBaseURL:  upstream.URL,
		RemoteAuthFile: authFile,
		Model:          "kmodel",
		Timeout:        30 * time.Second,
		// A pipe that cannot exist: the local IPC path must stay unused, and this
		// keeps the test from reaching a real desktop if it ever is.
		Transport: lingmaipc.TransportPipe,
		Pipe:      filepath.Join(t.TempDir(), "no-such-ipc-pipe"),
	})
	defer svc.Close()

	payload := base64.StdEncoding.EncodeToString([]byte("a-picture"))
	result, err := svc.Generate(context.Background(), ChatRequest{
		Model:    "kmodel",
		Messages: []ChatMessage{{Role: "user", Text: "这张图里是什么？", Images: []Image{{Data: payload, MediaType: "image/png"}}}},
	})
	if err != nil {
		t.Fatalf("Generate with an image over the remote backend: %v", err)
	}
	if !strings.Contains(result.Text, "go.mod") {
		t.Fatalf("text = %q, want the gateway's answer", result.Text)
	}
	if calls.Load() != 1 {
		t.Fatalf("gateway calls = %d, want exactly one: the turn must not detour through the local IPC pipe", calls.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	var body struct {
		ImageURLs []string `json:"image_urls"`
		Model     struct {
			IsVL bool `json:"is_vl"`
		} `json:"model_config"`
	}
	if err := json.Unmarshal(bodies[0], &body); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	if len(body.ImageURLs) != 1 || !strings.HasPrefix(body.ImageURLs[0], "data:image/png;base64,") {
		t.Fatalf("image_urls = %#v, want the attachment projected onto the request", body.ImageURLs)
	}
	if !body.Model.IsVL {
		t.Fatal("model_config.is_vl = false; the gateway keys its vision model off image_urls")
	}
}

// P3-S4: a decode or write failure used to leave the uri pointing at the empty
// file CreateTemp had just made, which reads back as "the picture was ignored"
// with nothing in the log. The item now travels without a file reference, and
// says once why.
func TestImagePromptItemDropsTheURIAfterAFailedWrite(t *testing.T) {
	dir := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, dir)
	}
	original := writeImageTempFile
	writeImageTempFile = func(string, []byte, os.FileMode) error { return errors.New("no space left on device") }
	t.Cleanup(func() { writeImageTempFile = original })

	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	payload := base64.StdEncoding.EncodeToString([]byte("a-picture"))
	item, ok := imagePromptItem("lingma", Image{Data: payload, MediaType: "image/png"})
	if !ok {
		t.Fatal("the attachment itself must still be sent; only the file reference is lost")
	}
	if uri, _ := item["uri"].(string); uri != "" {
		t.Fatalf("uri = %q, want none: it would point at a file the write never filled", uri)
	}
	if item["data"] != payload {
		t.Fatalf("data = %v, want the inline payload untouched", item["data"])
	}
	if !strings.Contains(logs.String(), "no space left on device") {
		t.Fatalf("log = %q, want the write failure reported once", logs.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed write left %d file(s) behind in the temp dir", len(entries))
	}
}

// The other half: base64 the client never sent, decoded to nothing and filed
// under the same "no error" silence.
func TestImagePromptItemDropsTheURIAfterAFailedDecode(t *testing.T) {
	dir := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, dir)
	}
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	item, ok := imagePromptItem("lingma", Image{Data: "!!!not base64!!!", MediaType: "image/png"})
	if !ok {
		t.Fatal("the attachment itself must still be sent; only the file reference is lost")
	}
	if uri, _ := item["uri"].(string); uri != "" {
		t.Fatalf("uri = %q, want none", uri)
	}
	if !strings.Contains(logs.String(), "decoding an inline attachment failed") {
		t.Fatalf("log = %q, want the decode failure reported once", logs.String())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("a failed decode left %d file(s) behind in the temp dir", len(entries))
	}
}

// P3-S5: this package's truncate is what formats a partial reply into a
// client-visible error, and Chinese replies are the common case here. Pin the
// rune boundary: a byte cut is what turns such an error into mojibake.
func TestTruncateCutsOnRuneBoundaries(t *testing.T) {
	const chinese = "读取文件失败：磁盘已满，请检查路径后重试。"
	tests := []struct {
		name  string
		text  string
		limit int
		want  string
	}{
		{name: "under the limit is untouched", text: chinese, limit: 40, want: chinese},
		{name: "empty input", text: "", limit: 10, want: ""},
		{name: "cut lands inside a multi-byte rune", text: chinese, limit: 7, want: "读取文件失败："},
		{name: "limit 0", text: chinese, limit: 0, want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncate(tc.text, tc.limit)
			if got != tc.want {
				t.Fatalf("truncate(%q, %d) = %q, want %q", tc.text, tc.limit, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncate produced invalid UTF-8: %q", got)
			}
		})
	}
}
