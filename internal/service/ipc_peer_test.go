package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/lingmaipc"
	"lingma-ipc-proxy/internal/toolemulation"

	"github.com/gorilla/websocket"
)

// Session cleanup and the tool-emulation retry both live in the IPC generate path,
// which keeps a session on the desktop app. Testing them needs a peer that speaks
// the plug-in protocol, and websocket is the transport a test can serve: the pipe
// transport needs an installed app.

type peerRequest struct {
	ID     *int           `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

type ipcPeer struct {
	url     string
	session string

	mu      sync.Mutex
	conns   []*websocket.Conn
	prompts []string
	deleted []string

	writeMu  sync.Mutex
	attempts int
	// onPrompt answers one session/prompt. It runs on the read loop, so it must not
	// block: the reply to the cleanup request that follows a timeout is read there.
	onPrompt func(p *ipcPeer, requestID string, attempt int)
}

func startPeer(t *testing.T) *ipcPeer {
	t.Helper()
	p := &ipcPeer{session: "sess-test-1"}
	p.onPrompt = func(peer *ipcPeer, requestID string, _ int) {
		peer.chunk(requestID, "answer")
		peer.finish(requestID)
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			// Logging here can race the test ending; a failed handshake already
			// shows up as the generate call failing.
			return
		}
		p.mu.Lock()
		p.conns = append(p.conns, conn)
		p.mu.Unlock()
		go p.readLoop(conn)
	}))
	t.Cleanup(server.Close)
	p.url = "ws://" + strings.TrimPrefix(server.URL, "http://") + "/"
	return p
}

// newIPCService returns a Service wired to peer and nothing else: the explicit
// websocket transport keeps ensureConnected from probing the machine for a pipe.
func newIPCService(t *testing.T, peer *ipcPeer, cfg Config) *Service {
	t.Helper()
	sandboxProfileDirs(t)
	cfg.Backend = BackendIPC
	cfg.Transport = lingmaipc.TransportWebSocket
	cfg.WebSocketURL = peer.url
	svc := New(cfg)
	t.Cleanup(func() { _ = svc.Close() })
	return svc
}

// sandboxProfileDirs hides the real user profile: this package reads the IDE config
// and login directories while resolving a backend or an image URI scheme.
func sandboxProfileDirs(t *testing.T) {
	t.Helper()
	empty := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "APPDATA", "LOCALAPPDATA", "ProgramData"} {
		t.Setenv(key, empty)
	}
}

func (p *ipcPeer) readLoop(conn *websocket.Conn) {
	for {
		_, frame, err := conn.ReadMessage()
		if err != nil {
			return
		}
		i := bytes.Index(frame, []byte("\r\n\r\n"))
		if i < 0 {
			continue
		}
		var req peerRequest
		if err := json.Unmarshal(frame[i+4:], &req); err != nil {
			continue
		}
		p.handle(req)
	}
}

func (p *ipcPeer) handle(req peerRequest) {
	switch req.Method {
	case "initialize", "session/set_model", "config/queryModels":
		p.reply(req.ID, map[string]any{"ok": true})
	case "session/new":
		p.reply(req.ID, map[string]any{"sessionId": p.session})
	case "chat/deleteSessionById":
		id := peerString(req.Params, "sessionId")
		if id == "" {
			id = peerString(req.Params, "id")
		}
		p.mu.Lock()
		p.deleted = append(p.deleted, id)
		p.mu.Unlock()
		p.reply(req.ID, map[string]any{"ok": true})
	case "session/prompt":
		text, requestID := peerPromptFields(req.Params)
		p.mu.Lock()
		p.prompts = append(p.prompts, text)
		p.attempts++
		attempt := p.attempts
		p.mu.Unlock()
		p.onPrompt(p, requestID, attempt)
	}
}

func (p *ipcPeer) reply(id *int, result any) {
	if id == nil {
		return
	}
	p.send(map[string]any{"jsonrpc": "2.0", "id": *id, "result": result})
}

// chunk and finish emit the two session/update shapes runPromptLocked reads.
func (p *ipcPeer) chunk(requestID, text string) {
	p.update(requestID, map[string]any{
		"sessionUpdate": "agent_message_chunk",
		"content":       map[string]any{"text": text},
	})
}

func (p *ipcPeer) finish(requestID string) {
	p.update(requestID, map[string]any{
		"sessionUpdate": "notification",
		"type":          "chat_finish",
		"data":          map[string]any{"reason": "finished"},
	})
}

func (p *ipcPeer) update(requestID string, update map[string]any) {
	p.send(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"_meta":  map[string]any{lingmaipc.MetaRequestID: requestID},
			"update": update,
		},
	})
}

func (p *ipcPeer) send(payload map[string]any) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	frame := []byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)))
	frame = append(frame, body...)
	p.mu.Lock()
	conns := append([]*websocket.Conn(nil), p.conns...)
	p.mu.Unlock()
	for _, conn := range conns {
		p.writeMu.Lock()
		_ = conn.WriteMessage(websocket.TextMessage, frame)
		p.writeMu.Unlock()
	}
}

func (p *ipcPeer) deletedSessions() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.deleted...)
}

func (p *ipcPeer) promptTexts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.prompts...)
}

func peerString(m map[string]any, key string) string {
	value, _ := m[key].(string)
	return strings.TrimSpace(value)
}

func peerPromptFields(params map[string]any) (string, string) {
	requestID := ""
	if meta, ok := params["_meta"].(map[string]any); ok {
		requestID, _ = meta[lingmaipc.MetaRequestID].(string)
	}
	items, _ := params["prompt"].([]any)
	for _, item := range items {
		block, _ := item.(map[string]any)
		if block == nil || block["type"] != "text" {
			continue
		}
		if text, ok := block["text"].(string); ok && text != "" {
			return text, requestID
		}
	}
	return "", requestID
}

func chatRequest(text string) ChatRequest {
	return ChatRequest{Model: "lingma", Messages: []ChatMessage{{Role: "user", Text: text}}}
}

func (s *Service) stickySessionForTest() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stickySessionID
}

// TestReuseTurnTimeoutReclaimsTheSession pins S2. A reuse turn the client never
// reads used to be dropped only on our side: the sticky slot was cleared, but the
// plug-in kept generating into a session nothing referenced, burning quota and
// holding that session busy, and the cleanup defer skipped reuse explicitly.
func TestReuseTurnTimeoutReclaimsTheSession(t *testing.T) {
	peer := startPeer(t)
	// One chunk, then silence: the desktop is still generating when the request
	// deadline takes the client away.
	peer.onPrompt = func(p *ipcPeer, requestID string, _ int) {
		p.chunk(requestID, "still generating")
	}
	svc := newIPCService(t, peer, Config{SessionMode: SessionModeReuse, Timeout: 15 * time.Second})

	_, err := svc.generateWithReconnect(context.Background(), chatRequest("run it"), nil)
	if err == nil {
		t.Fatal("a stalled reuse turn must not answer")
	}
	if text := err.Error(); !strings.Contains(text, "timed out") && !strings.Contains(text, "remained incomplete") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := peer.deletedSessions(); len(got) != 1 || got[0] != peer.session {
		t.Fatalf("orphaned turn left %v on the plug-in, want exactly [%s]", got, peer.session)
	}
	if sticky := svc.stickySessionForTest(); sticky != "" {
		t.Fatalf("the abandoned session must also leave the sticky slot, got %q", sticky)
	}
}

// TestReuseTurnKeepsASessionItAnswered is the other half of S2: reclaiming must not
// become unconditional, or reuse mode stops reusing anything.
func TestReuseTurnKeepsASessionItAnswered(t *testing.T) {
	peer := startPeer(t)
	svc := newIPCService(t, peer, Config{SessionMode: SessionModeReuse, Timeout: time.Minute})

	result, err := svc.generateWithReconnect(context.Background(), chatRequest("run it"), nil)
	if err != nil {
		t.Fatalf("answered turn failed: %v", err)
	}
	if result.Text != "answer" {
		t.Fatalf("text = %q, want the answered chunk", result.Text)
	}
	if got := peer.deletedSessions(); len(got) != 0 {
		t.Fatalf("a turn the client read must survive on the plug-in, got %v", got)
	}
	if sticky := svc.stickySessionForTest(); sticky != peer.session {
		t.Fatalf("sticky slot = %q, want %q", sticky, peer.session)
	}
}

// TestToolEmulationRetryDoesNotRestreamTheFirstAttempt pins S3. The retry replaces
// result.Text, so streaming it as well handed a streaming client the prose attempt
// and then the retry, while a stream:false client got only the retry tail.
func TestToolEmulationRetryDoesNotRestreamTheFirstAttempt(t *testing.T) {
	peer := startPeer(t)
	const prose = "我先看看项目结构。"
	const action = "```json action\n{\"tool\":\"Bash\",\"parameters\":{\"command\":\"pwd\"}}\n```"
	peer.onPrompt = func(p *ipcPeer, requestID string, attempt int) {
		if attempt == 1 {
			p.chunk(requestID, prose)
		} else {
			p.chunk(requestID, action)
		}
		p.finish(requestID)
	}
	svc := newIPCService(t, peer, Config{Timeout: time.Minute})

	req := chatRequest("list the files")
	req.Tools = []toolemulation.ToolDef{{
		Name: "Bash",
		InputSchema: map[string]any{
			"properties": map[string]any{"command": map[string]any{"type": "string"}},
			"required":   []any{"command"},
		},
	}}
	req.ToolChoice = toolemulation.ToolChoice{Mode: "any"}

	var streamed strings.Builder
	result, err := svc.generateWithReconnect(context.Background(), req, func(event StreamEvent) {
		if event.Type == StreamEventText {
			streamed.WriteString(event.Delta)
		}
	})
	if err != nil {
		t.Fatalf("generate failed: %v", err)
	}
	if len(peer.promptTexts()) != 2 {
		t.Fatalf("prompts = %d, want the answer plus the forced-tooling retry", len(peer.promptTexts()))
	}
	if len(result.ToolCalls) != 1 || result.ToolCalls[0].Name != "Bash" {
		t.Fatalf("retry did not produce a tool call: %+v", result.ToolCalls)
	}
	if got := streamed.String(); got != prose {
		t.Fatalf("streamed %q, want only the first attempt %q: the retry is not part of the answer the client saw", got, prose)
	}
}

// TestIPCImageURISchemeReadsUnderTheMutex pins S1. pipePath and endpoint belong to
// the live connection -- ensureConnected writes them, closeClientLocked clears them
// -- and every other reader in this file takes s.mu to see them.
func TestIPCImageURISchemeReadsUnderTheMutex(t *testing.T) {
	sandboxProfileDirs(t)
	svc := New(Config{Backend: BackendIPC, Transport: lingmaipc.TransportPipe, Pipe: "lingma-default"})
	scheme := make(chan string, 1)

	svc.mu.Lock()
	svc.pipePath = `\\.\pipe\qoder-cn-shared_client`
	go func() { scheme <- svc.ipcImageURIScheme() }()

	select {
	case got := <-scheme:
		svc.mu.Unlock()
		t.Fatalf("ipcImageURIScheme answered %q without s.mu: it read the connection fields mid-update", got)
	case <-time.After(500 * time.Millisecond):
		// The window is generous on purpose: it has to be long enough that an
		// unsynchronised read would certainly have been scheduled and answered.
	}
	svc.mu.Unlock()

	select {
	case got := <-scheme:
		if got != "qodercn" {
			t.Fatalf("scheme = %q, want qodercn from the snapshotted pipe path", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ipcImageURIScheme never resumed after s.mu was released")
	}
}

// TestImagePromptItemSweepsSpooledImages pins S6. Only the legacy Lingma branch
// leaves a file behind, and the sweep used to run once at process start, so a host
// that never restarted accumulated one image per turn.
func TestImagePromptItemSweepsSpooledImages(t *testing.T) {
	dir := t.TempDir()
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, dir)
	}
	// The sweep is rate limited per process, so this test owns the guard's clock.
	lastImageTempSweep.Store(0)

	stale := filepath.Join(dir, "lingma-img-stale.png")
	fresh := filepath.Join(dir, "lingma-img-fresh.png")
	old := time.Now().Add(-2 * imageTempHorizon)
	for _, path := range []string{stale, fresh} {
		if err := os.WriteFile(path, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// NTFS mtime is sub-millisecond, so an explicit age is the only reliable clock.
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	payload := base64.StdEncoding.EncodeToString([]byte("a-picture"))
	// The legacy scheme is the branch that spools a file, so it is the branch that
	// has to pay for the pile it joins.
	if _, ok := imagePromptItem("lingma", Image{Data: payload, MediaType: "image/png"}); !ok {
		t.Fatal("legacy image should be sent")
	}
	if _, err := os.Stat(stale); err == nil {
		t.Fatal("the write path must sweep the images it can no longer reach")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh image vanished: %v", err)
	}
	if lastImageTempSweep.Load() == 0 {
		t.Fatal("the sweep did not record its clock, so every image would rescan")
	}
}
