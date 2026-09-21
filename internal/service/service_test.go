package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/qodercli"
	"lingma-ipc-proxy/internal/toolemulation"
)

func TestIsRecoverableIPCError(t *testing.T) {
	cases := []error{
		errors.New("write websocket frame: write tcp 127.0.0.1:64954->127.0.0.1:36510: use of closed network connection"),
		errors.New("broken pipe"),
		errors.New("Lingma IPC notification stream closed"),
	}
	for _, err := range cases {
		if !isRecoverableIPCError(err) {
			t.Fatalf("expected recoverable error: %v", err)
		}
	}
}

func TestIsRecoverableIPCErrorIgnoresModelErrors(t *testing.T) {
	if isRecoverableIPCError(errors.New("timed out while waiting for Lingma IPC to finish responding")) {
		t.Fatal("timeout should not be treated as an immediate reconnect retry")
	}
}

func TestNewKeepsZeroTimeoutUnlimited(t *testing.T) {
	svc := New(Config{Timeout: 0})
	if svc.cfg.Timeout != 0 {
		t.Fatalf("timeout = %v, want 0", svc.cfg.Timeout)
	}
}

func TestContextWithOptionalTimeoutZeroDoesNotSetDeadline(t *testing.T) {
	ctx, cancel := contextWithOptionalTimeout(context.Background(), 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("zero timeout should not set a deadline")
	}
}

func TestContextWithOptionalTimeoutPositiveSetsDeadline(t *testing.T) {
	ctx, cancel := contextWithOptionalTimeout(context.Background(), time.Second)
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("positive timeout should set a deadline")
	}
}

func TestRemoteFallbackModelsNormalizeAndDedupe(t *testing.T) {
	svc := New(Config{
		Backend:              BackendRemote,
		RemoteFallbackModels: []string{"Kimi-K2.6", "kmodel", "MiniMax-M2.7"},
	})
	got := svc.remoteFallbackModels()
	want := []string{"kmodel", "mmodel"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fallback models = %v, want %v", got, want)
	}
}

func TestShouldProbeRemoteModelForListOnlyKnownMissingAliases(t *testing.T) {
	if !shouldProbeRemoteModelForList("Kimi-K2.6") {
		t.Fatal("expected Kimi alias to be probed")
	}
	if !shouldProbeRemoteModelForList("MiniMax-M2.7") {
		t.Fatal("expected MiniMax alias to be probed")
	}
	if shouldProbeRemoteModelForList("dashscope_qwen3_coder") {
		t.Fatal("unexpected probe for normal list model")
	}
}

func TestRemoteModelDisplayNameForVerifiedFallbackAliases(t *testing.T) {
	cases := map[string]string{
		"kmodel":                "Kimi-K2.6",
		"mmodel":                "MiniMax-M2.7",
		"some-enterprise-model": "some-enterprise-model",
	}
	for input, want := range cases {
		if got := remoteModelDisplayName(input); got != want {
			t.Fatalf("remoteModelDisplayName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestDescribeIPCSetupErrorClarifiesClosedLingmaBackend(t *testing.T) {
	err := describeIPCSetupError("session setup", context.DeadlineExceeded)
	if err == nil {
		t.Fatal("expected wrapped error")
	}
	text := err.Error()
	if !strings.Contains(text, "session setup timed out") || !strings.Contains(text, "重新打开 Lingma App、QoderCN App") {
		t.Fatalf("unexpected error text: %s", text)
	}
}

func TestBuildLingmaPromptInjectsToolingWhenEmulationEnabled(t *testing.T) {
	req := ChatRequest{
		Messages: []ChatMessage{{Role: "user", Text: "查看项目结构"}},
		Tools: []toolemulation.ToolDef{{
			Name: "Bash",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"command": map[string]any{"type": "string"},
				},
				"required": []any{"command"},
			},
		}},
		ToolChoice: toolemulation.ToolChoice{Mode: "auto"},
	}

	remotePrompt, err := buildLingmaPrompt(req, SessionModeFresh, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(remotePrompt, "```json action") || strings.Contains(remotePrompt, "DIRECT tool access") {
		t.Fatalf("remote prompt should not include tool emulation:\n%s", remotePrompt)
	}

	ipcPrompt, err := buildLingmaPrompt(req, SessionModeFresh, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ipcPrompt, "```json action") || !strings.Contains(ipcPrompt, "DIRECT tool access") {
		t.Fatalf("ipc prompt should include tool emulation:\n%s", ipcPrompt)
	}
}

// The Qoder CN gateway reroutes user turns that name another product's identity,
// so the CLI backend must keep the client's system text out of the user turn.
func TestBuildLingmaPromptSectionsKeepsClientSystemOutOfUserTurn(t *testing.T) {
	req := ChatRequest{
		System:     "You are ZCode, an interactive coding agent",
		Messages:   []ChatMessage{{Role: "user", Text: "查看项目结构"}},
		Tools:      []toolemulation.ToolDef{{Name: "Bash"}},
		ToolChoice: toolemulation.ToolChoice{Mode: "auto"},
	}

	inlineSystem, inlinePrompt, err := buildLingmaPromptSections(req, SessionModeFresh, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if inlineSystem != "" {
		t.Fatalf("inline mode must not return a separate section: %q", inlineSystem)
	}
	if !strings.Contains(inlinePrompt, "You are ZCode") {
		t.Fatalf("inline mode must keep the client instructions in the prompt:\n%s", inlinePrompt)
	}

	section, prompt, err := buildLingmaPromptSections(req, SessionModeFresh, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if section != req.System {
		t.Fatalf("CLI mode returned the wrong section: %q", section)
	}
	if strings.Contains(prompt, "You are ZCode") {
		t.Fatalf("client instructions leaked into the user turn:\n%s", prompt)
	}
	if !strings.Contains(prompt, "DIRECT tool access") || !strings.HasSuffix(prompt, "Assistant:") {
		t.Fatalf("action-block rules must stay last in the user turn:\n%s", prompt)
	}
}

func TestShouldEmulateRemoteToolsForToolRequests(t *testing.T) {
	req := ChatRequest{
		Messages: []ChatMessage{{Role: "user", Text: "查看项目结构"}},
		Tools:    []toolemulation.ToolDef{{Name: "Bash"}},
		ToolChoice: toolemulation.ToolChoice{
			Mode: "auto",
		},
	}
	if !shouldEmulateRemoteTools(req) {
		t.Fatal("remote tool requests should enable prompt tool emulation fallback")
	}

	req.ToolChoice = toolemulation.ToolChoice{Mode: "none"}
	if shouldEmulateRemoteTools(req) {
		t.Fatal("tool_choice none should disable remote prompt tool emulation")
	}
}

func TestRemoteMessagesForChatUsesPromptWhenToolEmulationEnabled(t *testing.T) {
	req := ChatRequest{
		System:   "original system",
		Messages: []ChatMessage{{Role: "user", Text: "查看项目结构"}},
		Tools:    []toolemulation.ToolDef{{Name: "Bash"}},
		ToolChoice: toolemulation.ToolChoice{
			Mode: "auto",
		},
	}

	messages := remoteMessagesForChat(req, "User: 查看项目结构\n\nDIRECT tool access\n\nAssistant:", true)
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Role != "user" || !strings.Contains(messages[0].Content, "DIRECT tool access") {
		t.Fatalf("expected emulated prompt message, got %#v", messages)
	}

	plain := remoteMessagesForChat(req, "ignored", false)
	if len(plain) != 2 || plain[0].Role != "system" || plain[1].Content != "查看项目结构" {
		t.Fatalf("expected structured messages without emulation, got %#v", plain)
	}
}

func TestBuildLingmaPromptIncludesReasoningHintOnlyWhenRequested(t *testing.T) {
	req := ChatRequest{
		Messages:        []ChatMessage{{Role: "user", Text: "解释这个函数"}},
		ReasoningEffort: "high",
	}
	prompt, err := buildLingmaPrompt(req, SessionModeFresh, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Reasoning mode is enabled") {
		t.Fatalf("prompt should include reasoning hint:\n%s", prompt)
	}

	plainPrompt, err := buildLingmaPrompt(ChatRequest{
		Messages: []ChatMessage{{Role: "user", Text: "解释这个函数"}},
	}, SessionModeFresh, false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(plainPrompt, "Reasoning mode is enabled") {
		t.Fatalf("plain prompt should not include reasoning hint:\n%s", plainPrompt)
	}
}

func TestShouldRetryRemoteNativeToolForContinuationText(t *testing.T) {
	req := ChatRequest{
		Tools: []toolemulation.ToolDef{{Name: "Bash"}},
		ToolChoice: toolemulation.ToolChoice{
			Mode: "auto",
		},
	}
	if !shouldRetryRemoteNativeTool(req, "让我查看一下项目的整体结构，特别是源代码目录：") {
		t.Fatal("expected continuation text to trigger native tool retry")
	}
	if shouldRetryRemoteNativeTool(req, "这是一个 uni-app 项目，核心目录是 src。") {
		t.Fatal("substantive answer should not trigger retry")
	}
	req.ToolChoice = toolemulation.ToolChoice{Mode: "none"}
	if shouldRetryRemoteNativeTool(req, "让我查看一下：") {
		t.Fatal("tool_choice none should not trigger retry")
	}
}

func TestBuildLingmaPromptKeepsToolResultsForIPC(t *testing.T) {
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "user", Text: "查看项目"},
			{Role: "assistant", ToolCalls: []toolemulation.ToolCall{{ID: "call_1", Name: "Bash", Arguments: map[string]any{"command": "pwd"}}}},
			{Role: "tool", ToolCallID: "call_1", Text: "/tmp/project"},
		},
		Tools:      []toolemulation.ToolDef{{Name: "Bash"}},
		ToolChoice: toolemulation.ToolChoice{Mode: "auto"},
	}
	prompt, err := buildLingmaPrompt(req, SessionModeFresh, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "Tool result for call_1") || !strings.Contains(prompt, "/tmp/project") {
		t.Fatalf("ipc prompt should include tool result:\n%s", prompt)
	}
	if strings.Contains(prompt, "Assistant used tool") {
		t.Fatalf("ipc prompt should not include textualized assistant tool calls:\n%s", prompt)
	}
}

func TestRemoteImagesFromRequest(t *testing.T) {
	req := ChatRequest{Messages: []ChatMessage{{Role: "user", Text: "see", Images: []Image{{MediaType: "image/png", Data: "AAAA"}}}}}
	images := remoteImagesFromRequest(req)
	if len(images) != 1 {
		t.Fatalf("images = %#v", images)
	}
	if images[0].MediaType != "image/png" || images[0].Data != "AAAA" {
		t.Fatalf("unexpected image = %#v", images[0])
	}
}

func TestRequestHasImages(t *testing.T) {
	if requestHasImages(ChatRequest{Messages: []ChatMessage{{Role: "user", Text: "plain"}}}) {
		t.Fatal("plain request should not have images")
	}
	if !requestHasImages(ChatRequest{Messages: []ChatMessage{{Role: "user", Images: []Image{{URL: "file:///tmp/a.png"}}}}}) {
		t.Fatal("image URL request should have images")
	}
}

func TestCurrentTurnHasImagesIgnoresReplayedHistory(t *testing.T) {
	req := ChatRequest{Messages: []ChatMessage{
		{Role: "user", Text: "看图", Images: []Image{{MediaType: "image/png", Data: "AAAA"}}},
		{Role: "assistant", Text: "图里是一只猫"},
		{Role: "user", Text: "它是什么动物"},
	}}
	if requestHasImages(req) == false {
		t.Fatal("replayed history still carries the attachment")
	}
	if currentTurnHasImages(req) {
		t.Fatal("the turn being answered has no attachment")
	}

	kept := requestWithCurrentTurnImagesOnly(req)
	if requestHasImages(kept) {
		t.Fatal("a history-only attachment must be dropped")
	}
	if kept.Messages[0].Text != "看图" || kept.Messages[2].Text != "它是什么动物" {
		t.Fatalf("stripping must keep the text, got %#v", kept.Messages)
	}
	if len(req.Messages[0].Images) != 1 {
		t.Fatal("the caller's request must not be modified")
	}

	fresh := ChatRequest{Messages: []ChatMessage{
		{Role: "user", Text: "早上那张图", Images: []Image{{MediaType: "image/png", Data: "AAAA"}}},
		{Role: "assistant", Text: "一张红图"},
		{Role: "user", Text: "这张呢", Images: []Image{{MediaType: "image/png", Data: "BBBB"}}},
	}}
	current := requestWithCurrentTurnImagesOnly(fresh)
	if len(current.Messages[0].Images) != 0 {
		t.Fatal("the history attachment must be dropped")
	}
	if len(current.Messages[2].Images) != 1 || current.Messages[2].Images[0].Data != "BBBB" {
		t.Fatalf("the current turn's attachment must survive: %#v", current.Messages[2])
	}
	if len(fresh.Messages[0].Images) != 1 {
		t.Fatal("the caller's request must not be modified")
	}

	last := ChatRequest{Messages: []ChatMessage{
		{Role: "user", Text: "早上的问题"},
		{Role: "assistant", Text: "答完了"},
		{Role: "user", Text: "再看这张", Images: []Image{{MediaType: "image/png", Data: "AAAA"}}},
	}}
	if !currentTurnHasImages(last) {
		t.Fatal("the newest user turn carries the attachment")
	}
}

func TestRequestForImageContextUsesLatestImageTurnOnly(t *testing.T) {
	req := ChatRequest{
		System: "old system",
		Messages: []ChatMessage{
			{Role: "user", Text: "旧问题"},
			{Role: "assistant", Text: "旧回答"},
			{Role: "user", Text: "[Image #1] 这个图片是什么?", Images: []Image{{MediaType: "image/png", Data: "AAAA"}}},
		},
		Tools: []toolemulation.ToolDef{{
			Name: "Bash",
			InputSchema: map[string]any{
				"required": []any{"command"},
			},
		}},
		ToolChoice: toolemulation.ToolChoice{Mode: "auto"},
	}

	out := requestForImageContext(req)
	if out.System != "" {
		t.Fatalf("system = %q, want empty", out.System)
	}
	if len(out.Tools) != 0 || out.ToolChoice.Mode != "none" {
		t.Fatalf("tools should be disabled: tools=%#v choice=%#v", out.Tools, out.ToolChoice)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %#v, want one compact image turn", out.Messages)
	}
	message := out.Messages[0]
	if message.Role != "user" || len(message.Images) != 1 || message.Images[0].Data != "AAAA" {
		t.Fatalf("unexpected image message = %#v", message)
	}
	if strings.Contains(message.Text, "旧问题") || !strings.Contains(message.Text, "忽略更早的对话历史") {
		t.Fatalf("unexpected compact prompt = %q", message.Text)
	}
}

func TestRequestForImageContextUsesShortSystemPromptForImageOnlyUser(t *testing.T) {
	req := ChatRequest{
		System:   "这张图片是什么？只用两句话回答。",
		Messages: []ChatMessage{{Role: "user", Images: []Image{{MediaType: "image/jpeg", Data: "AAAA"}}}},
	}

	out := requestForImageContext(req)
	if len(out.Messages) != 1 {
		t.Fatalf("messages = %#v, want one compact image turn", out.Messages)
	}
	message := out.Messages[0]
	if message.Role != "user" || len(message.Images) != 1 {
		t.Fatalf("unexpected image message = %#v", message)
	}
	if !strings.Contains(message.Text, "这张图片是什么") {
		t.Fatalf("compact prompt should include short system prompt, got %q", message.Text)
	}
}

func TestBuildLingmaPromptUsesImageFallbackForImageOnlyUser(t *testing.T) {
	req := ChatRequest{
		System:   "这张图片是什么？只用两句话回答。",
		Messages: []ChatMessage{{Role: "user", Images: []Image{{URL: "file:///tmp/a.jpg"}}}},
	}

	prompt, err := buildLingmaPrompt(req, SessionModeFresh, false)
	if err != nil {
		t.Fatalf("buildLingmaPrompt returned error: %v", err)
	}
	if !strings.Contains(prompt, "这张图片是什么") {
		t.Fatalf("prompt should include image fallback question, got %q", prompt)
	}
}

func TestExtractLastUserImagesFindsPreviousImageTurn(t *testing.T) {
	images := extractLastUserImages([]ChatMessage{
		{Role: "user", Text: "看这张图", Images: []Image{{URL: "file:///tmp/a.png"}}},
		{Role: "assistant", Text: "这是一张图片"},
		{Role: "user", Text: "继续基于上图分析"},
	})
	if len(images) != 1 || images[0].URL != "file:///tmp/a.png" {
		t.Fatalf("images = %#v", images)
	}
}

func TestRequestWithImageContextRemovesImagesAndAppendsContext(t *testing.T) {
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "user", Text: "看图", Images: []Image{{URL: "file:///tmp/a.png"}}},
			{Role: "assistant", Text: "好的"},
			{Role: "user", Text: "继续分析"},
		},
	}
	out := requestWithImageContext(req, "海边礁石和海浪")
	for _, message := range out.Messages {
		if len(message.Images) > 0 {
			t.Fatalf("images should be removed: %#v", out.Messages)
		}
	}
	if !strings.Contains(out.Messages[2].Text, "[图片上下文]") || !strings.Contains(out.Messages[2].Text, "海边礁石和海浪") {
		t.Fatalf("latest user message missing image context: %#v", out.Messages[2])
	}
}

func TestBuildLingmaPromptInjectsToolingForImageContextRemoteFallback(t *testing.T) {
	req := ChatRequest{
		Messages: []ChatMessage{
			{Role: "user", Text: "这张图是什么", Images: []Image{{URL: "file:///tmp/a.png"}}},
		},
		Tools: []toolemulation.ToolDef{{
			Name: "exec_command",
			InputSchema: map[string]any{
				"properties": map[string]any{
					"cmd": map[string]any{"type": "string"},
				},
				"required": []any{"cmd"},
			},
		}},
		ToolChoice: toolemulation.ToolChoice{Mode: "auto"},
	}

	withContext := requestWithImageContext(req, "黑色礁石与海浪")
	prompt, err := buildLingmaPrompt(withContext, SessionModeFresh, true)
	if err != nil {
		t.Fatalf("buildLingmaPrompt returned error: %v", err)
	}
	if !strings.Contains(prompt, "[图片上下文]") {
		t.Fatalf("prompt should include image context, got %q", prompt)
	}
	if !strings.Contains(prompt, "DIRECT tool access") || !strings.Contains(prompt, "```json action") {
		t.Fatalf("image-context remote fallback prompt should include tool emulation, got %q", prompt)
	}
}

func TestSplitCLIModelEffort(t *testing.T) {
	cases := []struct {
		in       string
		wantName string
		wantEff  string
	}{
		{"Qwen3.8-Flash-xhigh", "Qwen3.8-Flash", "xhigh"},
		{"Qwen3.8-Flash-极高", "Qwen3.8-Flash", "xhigh"},
		{"Qwen3.8-Flash-最高", "Qwen3.8-Flash", "max"},
		{"lingma-proxy/Qwen3.8-Flash-xhigh", "Qwen3.8-Flash", "xhigh"},
		{"Qwen3.8-Flash", "Qwen3.8-Flash", ""},
		{"Qwen3.8-Max", "Qwen3.8-Max", ""},
		{"Qwen3.8-Flash-max", "Qwen3.8-Flash-max", ""},
		{"MiniMax-M2.7", "MiniMax-M2.7", ""},
		{"Kimi-K2.8-Preview", "Kimi-K2.8-Preview", ""},
		{"Auto", "Auto", ""},
	}
	for _, tc := range cases {
		name, effort := splitCLIModelEffort(tc.in)
		if name != tc.wantName || effort != tc.wantEff {
			t.Fatalf("splitCLIModelEffort(%q) = %q,%q want %q,%q", tc.in, name, effort, tc.wantName, tc.wantEff)
		}
	}
}

func TestSplitCLISiteReadsTheSiteMarkerFromAnySegment(t *testing.T) {
	cases := []struct {
		in      string
		want    qodercli.Site
		wantCmd string
		named   bool
	}{
		{"Qwen3.8-Flash", qodercli.SiteCN, "Qwen3.8-Flash", false},
		{"intl/Qwen3.8-Flash", qodercli.SiteGlobal, "Qwen3.8-Flash", true},
		{"global/Kimi-K3", qodercli.SiteGlobal, "Kimi-K3", true},
		{"国际版/Qwen3.8-Flash", qodercli.SiteGlobal, "Qwen3.8-Flash", true},
		// Clients prefix the id with their own provider key.
		{"lingma-proxy/intl/Qwen3.8-Flash-xhigh", qodercli.SiteGlobal, "Qwen3.8-Flash-xhigh", true},
		{"INTL/Auto", qodercli.SiteGlobal, "Auto", true},
		// A bare marker names no model, so it stays a CN request.
		{"intl", qodercli.SiteCN, "intl", false},
	}
	for _, tc := range cases {
		site, model, named := splitCLISite(tc.in)
		if site != tc.want || model != tc.wantCmd || named != tc.named {
			t.Fatalf("splitCLISite(%q) = %q,%q,%v want %q,%q,%v",
				tc.in, site, model, named, tc.want, tc.wantCmd, tc.named)
		}
	}
}

// TestFreshCLICatalogIsServedWithoutProbing pins the cache hit: a probe is an
// Electron subprocess that measures seconds, so /v1/models must not pay for one
// while a site's catalog is still inside its TTL.
func TestFreshCLICatalogIsServedWithoutProbing(t *testing.T) {
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	sites := s.cliSites()
	if len(sites) == 0 {
		t.Skip("no CLI site is configured")
	}
	// Names no installed CLI would return, so anything else in the output means
	// the merge probed instead of reading the cache.
	for _, site := range sites {
		s.setCLICatalog(site, []string{"cached-a", "cached-b"}, time.Now().Add(time.Minute))
	}

	out, err := s.listCLIMergedModels(context.Background())
	if err != nil {
		t.Fatalf("listCLIMergedModels: %v", err)
	}
	// The global catalog is namespaced, so the two sites never dedup against
	// each other here.
	if len(out) != 2*len(sites) {
		t.Fatalf("merged %d models, want %d cached ones: %v", len(out), 2*len(sites), out)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, site := range sites {
		ids := s.cliModels[site.Normalized()]
		if len(ids) != 2 || ids[0] != "cached-a" {
			t.Fatalf("site %v tier ids = %v, want the bare cached names", site, ids)
		}
	}
}

// TestMergeCLICatalogsSilentSiteFailsLouderThanAHalfList pins the defect that a
// cold `both` proxy measured at 3 of 6 requests: one site's probe failed, the
// other 14 models came back as a 200, and clients read that as a delisting.
func TestMergeCLICatalogsSilentSiteFailsLouderThanAHalfList(t *testing.T) {
	s := &Service{}
	sites := []qodercli.Site{qodercli.SiteCN, qodercli.SiteGlobal}
	boom := errors.New("intl CLI failed")

	out, err := s.mergeCLICatalogs(sites, []cliSiteListing{
		{names: []string{"Auto"}},
		{names: []string{"Auto"}, err: boom},
	})
	if err != nil {
		t.Fatalf("a site with a cached catalog must still answer: %v", err)
	}
	if len(out) != 2 || out[0].ID != "Auto" || out[1].ID != cliGlobalPrefix+"Auto" {
		t.Fatalf("merged = %v, want the CN id and the namespaced stale id", out)
	}

	out, err = s.mergeCLICatalogs(sites, []cliSiteListing{
		{names: []string{"Auto"}},
		{err: boom},
	})
	if err == nil {
		t.Fatalf("half list returned as success: %v", out)
	}
	if out != nil {
		t.Fatalf("models returned alongside the error: %v", out)
	}
	if !strings.Contains(err.Error(), "没有返回模型列表") || !strings.Contains(err.Error(), boom.Error()) {
		t.Fatalf("error must name the site and wrap the cause, got %v", err)
	}
}
