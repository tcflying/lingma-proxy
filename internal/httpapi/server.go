package httpapi

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"lingma-ipc-proxy/internal/remote"
	"lingma-ipc-proxy/internal/service"
	"lingma-ipc-proxy/internal/toolemulation"
	"lingma-ipc-proxy/internal/version"
)

// streamKeepaliveInterval bounds how long an established SSE stream may stay
// silent before the proxy emits a keep-alive comment line. Clients read silence
// as a dead stream (Codex CLI disconnects on its idle timeout mid-answer), and
// the backend stays silent while the model works. A var, like
// maxCLIOutputLineBytes, so tests can shrink it instead of waiting 15s.
//
// CONCURRENCY: this is a plain package-level var, and the httpapi tests rewrite
// it (keep with keepalive933SetInterval) while the package's other tests are
// running. Adding t.Parallel() to any test in this package that starts a stream
// will immediately be a data race here. The same applies to slotQueueWait.
var streamKeepaliveInterval = 15 * time.Second

type Server struct {
	svc     *service.Service
	http    *http.Server
	sem     chan struct{}
	recMu   sync.RWMutex
	records []debugRequestRecord
	logs    []debugLogRecord
	// OnRequest is called after each request completes with summary info.
	// method, path, statusCode, duration, requestBody, responseBody
	OnRequest func(method, path string, statusCode int, duration time.Duration, reqBody, respBody string)
	// AppLogs returns persisted desktop application logs. It is optional because
	// the CLI server does not have a desktop app state file.
	AppLogs func(limit int, source string) []DebugAppLogRecord
}

type anthropicRequest struct {
	Model         string         `json:"model"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	System        any            `json:"system,omitempty"`
	Messages      []rawMessage   `json:"messages"`
	Stream        bool           `json:"stream,omitempty"`
	Tools         any            `json:"tools,omitempty"`
	ToolChoice    any            `json:"tool_choice,omitempty"`
	Temperature   *float64       `json:"temperature,omitempty"`
	TopP          *float64       `json:"top_p,omitempty"`
	TopK          int            `json:"top_k,omitempty"`
	StopSequences []string       `json:"stop_sequences,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
	Thinking      any            `json:"thinking,omitempty"`
	OutputConfig  map[string]any `json:"output_config,omitempty"`
	Reasoning     any            `json:"reasoning,omitempty"`
	ReasoningEff  any            `json:"reasoning_effort,omitempty"`
}

type openAIChatRequest struct {
	Model               string       `json:"model"`
	Messages            []rawMessage `json:"messages"`
	Stream              bool         `json:"stream,omitempty"`
	MaxTokens           int          `json:"max_tokens,omitempty"`
	MaxCompletionTokens int          `json:"max_completion_tokens,omitempty"`
	Tools               any          `json:"tools,omitempty"`
	ToolChoice          any          `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool        `json:"parallel_tool_calls,omitempty"`
	Temperature         *float64     `json:"temperature,omitempty"`
	TopP                *float64     `json:"top_p,omitempty"`
	Stop                any          `json:"stop,omitempty"`
	PresencePenalty     float64      `json:"presence_penalty,omitempty"`
	FrequencyPenalty    float64      `json:"frequency_penalty,omitempty"`
	Logprobs            bool         `json:"logprobs,omitempty"`
	TopLogprobs         int          `json:"top_logprobs,omitempty"`
	ResponseFormat      any          `json:"response_format,omitempty"`
	Seed                int          `json:"seed,omitempty"`
	User                string       `json:"user,omitempty"`
	ReasoningEffort     string       `json:"reasoning_effort,omitempty"`
}

type openAIResponsesRequest struct {
	Model             string   `json:"model"`
	Input             any      `json:"input"`
	Instructions      string   `json:"instructions,omitempty"`
	Stream            bool     `json:"stream,omitempty"`
	Tools             any      `json:"tools,omitempty"`
	ToolChoice        any      `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool    `json:"parallel_tool_calls,omitempty"`
	Temperature       *float64 `json:"temperature,omitempty"`
	TopP              *float64 `json:"top_p,omitempty"`
	MaxOutputTokens   int      `json:"max_output_tokens,omitempty"`
	MaxTokens         int      `json:"max_tokens,omitempty"`
	Stop              any      `json:"stop,omitempty"`
	User              string   `json:"user,omitempty"`
	Reasoning         any      `json:"reasoning,omitempty"`
	Text              any      `json:"text,omitempty"`
}

type rawMessage struct {
	Role       string `json:"role"`
	Content    any    `json:"content"`
	ToolCalls  []any  `json:"tool_calls,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type modelResponse struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	Name    string `json:"name,omitempty"`
}

type debugRequestRecord struct {
	Time       string `json:"time"`
	Method     string `json:"method"`
	Path       string `json:"path"`
	StatusCode int    `json:"statusCode"`
	DurationMS int64  `json:"durationMs"`
	Request    string `json:"request,omitempty"`
	Response   string `json:"response,omitempty"`
}

type debugLogRecord struct {
	Time    string `json:"time"`
	Level   string `json:"level"`
	Message string `json:"message"`
}

type DebugAppLogRecord struct {
	CreatedAt string `json:"createdAt,omitempty"`
	Time      string `json:"time"`
	Source    string `json:"source,omitempty"`
	SessionID string `json:"sessionId,omitempty"`
	ChatID    string `json:"chatId,omitempty"`
	MessageID string `json:"messageId,omitempty"`
	Level     string `json:"level"`
	Message   string `json:"message"`
}

func NewServer(addr string, svc *service.Service) *Server {
	s := &Server{
		svc: svc,
		sem: make(chan struct{}, maxConcurrentRequests()),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/health", s.handleRoot)
	mux.HandleFunc("/debug/requests", s.handleDebugRequests)
	mux.HandleFunc("/debug/access-logs", s.handleDebugLogs)
	mux.HandleFunc("/debug/app-logs", s.handleDebugAppLogs)
	mux.HandleFunc("/api/requests", s.handleDebugRequests)
	mux.HandleFunc("/api/access-logs", s.handleDebugLogs)
	mux.HandleFunc("/api/app-logs", s.handleDebugAppLogs)
	mux.HandleFunc("/debug/logs", s.handleDebugLogs)
	mux.HandleFunc("/api/logs", s.handleDebugLogs)
	mux.HandleFunc("/capabilities", s.handleCapabilities)
	mux.HandleFunc("/v1/capabilities", s.handleCapabilities)
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/api/v1/models", s.handleLMStudioModels)
	mux.HandleFunc("/api/tags", s.handleOllamaTags)
	mux.HandleFunc("/v1/props", s.handleModelProps)
	mux.HandleFunc("/props", s.handleModelProps)
	mux.HandleFunc("/version", s.handleVersion)
	mux.HandleFunc("/v1/messages/count_tokens", s.handleAnthropicCountTokens)
	mux.HandleFunc("/v1/messages", s.handleAnthropicMessages)
	// Anthropic-protocol clients that embed the provider prefix in their base
	// URL (MiniMax's mmx CLI posts to <base>/anthropic/v1/messages) need the
	// prefixed aliases; they must stay wired to the exact same handlers.
	mux.HandleFunc("/anthropic/v1/messages/count_tokens", s.handleAnthropicCountTokens)
	mux.HandleFunc("/anthropic/v1/messages", s.handleAnthropicMessages)
	mux.HandleFunc("/v1/chat/completions", s.handleOpenAIChatCompletions)
	mux.HandleFunc("/api/v1/chat/completions", s.handleOpenAIChatCompletions)
	mux.HandleFunc("/v1/responses", s.handleOpenAIResponses)
	mux.HandleFunc("/api/v1/responses", s.handleOpenAIResponses)

	s.http = &http.Server{
		Addr:              addr,
		Handler:           s.withRecorder(withCORS(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		// A finished keep-alive connection otherwise waits in the listener's
		// pool until the client closes it, and a client that went away silently
		// holds the slot forever. IdleTimeout reaps it.
		// WriteTimeout is deliberately absent: a streaming turn legitimately
		// writes nothing for minutes between the first and second frame, and a
		// write deadline would cut those answers in half.
		IdleTimeout: 120 * time.Second,
	}
	return s
}

func (s *Server) ListenAndServe() error {
	return s.http.ListenAndServe()
}

// Serve runs on a listener the caller already bound, so the port stays held
// between the check and the first accept.
func (s *Server) Serve(ln net.Listener) error {
	return s.http.Serve(ln)
}

func (s *Server) Shutdown(ctx context.Context) error {
	err := s.http.Shutdown(ctx)
	if err != nil {
		if forceErr := s.http.Close(); forceErr != nil {
			err = fmt.Errorf("%w; force close failed: %v", err, forceErr)
		} else {
			err = nil
		}
	}
	closeErr := s.svc.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func (s *Server) SetDefaultModel(model string) {
	s.svc.SetDefaultModel(model)
}

func (s *Server) applyDefaultModel(req *service.ChatRequest) {
	if strings.TrimSpace(req.Model) == "" {
		req.Model = s.svc.DefaultModel()
	}
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/health" {
		writeOpenAIError(w, http.StatusNotFound, "not_found_error", "not found")
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.URL.Path == "/health" {
		// Liveness only: State() takes the same mutex the CLI site scan uses, and
		// that scan reads the registry and globs install roots -- seconds of it on a
		// loaded box, which used to make /health time out.
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "lingma-proxy"})
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	// "/" answers the same minimal payload as /health. The full state carries
	// PipePath/Endpoint/StickySessionID and must not reach unauthenticated peers;
	// it stays on the gated debug routes.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "lingma-proxy"})
}

const debugAccessMessage = "debug inspection endpoints are loopback-only; set LINGMA_ALLOW_REMOTE_DEBUG=1 to expose them"

// debugAccessAllowed gates the request-inspection endpoints: they return recorded
// conversation bodies, so a proxy bound to 0.0.0.0 must not hand them to the
// network just because the API itself is reachable.
func debugAccessAllowed(r *http.Request) bool {
	// A browser sends Origin on every fetch from another page, curl does not, so
	// refusing a foreign Origin closes "any local webpage can read the recorded
	// conversations" without breaking CLI tooling or the same-origin console.
	if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && !sameHostOrigin(r, origin) {
		return false
	}
	if truthyEnv("LINGMA_ALLOW_REMOTE_DEBUG") {
		return true
	}
	host := r.RemoteAddr
	if colon := strings.LastIndex(host, ":"); colon >= 0 {
		host = host[:colon]
	}
	ip := net.ParseIP(strings.Trim(strings.Trim(host, "[]"), "%"))
	return ip != nil && ip.IsLoopback()
}

// sameHostOrigin reports whether an Origin header names this very proxy. A
// browser sends one on every cross-site fetch; curl, every CLI client and the
// desktop shell send none at all.
func sameHostOrigin(r *http.Request, origin string) bool {
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

// namedImageRequest is a request that makes the proxy go read something it does
// not carry: a remote image URL, or a path on this box. That read is the SSRF
// primitive. The proxy binds loopback, has no API authentication, and answers
// with Access-Control-Allow-Origin: *, so any page in any browser could point
// it at 127.0.0.1 or a LAN service, let the model transcribe what came back,
// and read the transcript out of the reply. Inline data: URLs read nothing off
// the box, so they are deliberately out of scope here.
func namedImageRequest(messages []rawMessage) bool {
	for _, message := range messages {
		if contentNamesAnImage(message.Content) {
			return true
		}
	}
	return false
}

func namedImageResponsesInput(input any) bool {
	items, ok := input.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if imageItemNamesASource(m) || contentNamesAnImage(m["content"]) {
			return true
		}
	}
	return false
}

func contentNamesAnImage(content any) bool {
	items, ok := content.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		// Only the protocol's own image item types count. Matching on the key
		// alone would trip on a tool schema that happens to declare an
		// image_url parameter, and 403 a legitimate client.
		switch stringFromAny(m["type"]) {
		case "image_url", "input_image", "image", "output_image":
			if imageItemNamesASource(m) {
				return true
			}
		}
	}
	return false
}

func imageItemNamesASource(item map[string]any) bool {
	raw := stringFromAny(item["image_url"])
	if raw == "" {
		if nested, ok := item["image_url"].(map[string]any); ok {
			raw = stringFromAny(nested["url"])
		}
	}
	if raw == "" {
		if source, ok := item["source"].(map[string]any); ok {
			raw = stringFromAny(source["url"])
		}
	}
	return raw != "" && !strings.HasPrefix(strings.TrimSpace(raw), "data:")
}

// crossOriginNamedImageRead reports whether this request has to be refused: a
// caller in a foreign web origin asking the proxy to read an image by URL or by
// path. A same-origin page, and every non-browser client, pass.
func crossOriginNamedImageRead(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	return origin != "" && !sameHostOrigin(r, origin)
}

const crossOriginImageMessage = "a request from another web origin may not make the proxy read an image by URL or path; " +
	"inline data: images and non-browser clients are unaffected"

func (s *Server) handleDebugRequests(w http.ResponseWriter, r *http.Request) {
	if !debugAccessAllowed(r) {
		http.Error(w, debugAccessMessage, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			switch {
			case parsed < 1:
				limit = 1
			case parsed > 200:
				limit = 200
			default:
				limit = parsed
			}
		}
	}

	records := s.debugRecords(limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"service":  "lingma-proxy",
		"count":    len(records),
		"requests": records,
		"state":    s.svc.State(),
	})
}

func (s *Server) handleDebugLogs(w http.ResponseWriter, r *http.Request) {
	if !debugAccessAllowed(r) {
		http.Error(w, debugAccessMessage, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			switch {
			case parsed < 1:
				limit = 1
			case parsed > 200:
				limit = 200
			default:
				limit = parsed
			}
		}
	}

	logs := s.debugLogs(limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "lingma-proxy",
		"kind":    "http_access_logs",
		"count":   len(logs),
		"logs":    logs,
		"state":   s.svc.State(),
	})
}

func (s *Server) handleDebugAppLogs(w http.ResponseWriter, r *http.Request) {
	if !debugAccessAllowed(r) {
		http.Error(w, debugAccessMessage, http.StatusForbidden)
		return
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	if s.AppLogs == nil {
		writeOpenAIError(w, http.StatusNotImplemented, "not_supported", "desktop app logs are not available in this process")
		return
	}

	limit := 500
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			switch {
			case parsed < 1:
				limit = 1
			case parsed > 5000:
				limit = 5000
			default:
				limit = parsed
			}
		}
	}
	source := strings.TrimSpace(r.URL.Query().Get("source"))

	logs := s.AppLogs(limit, source)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"service": "lingma-proxy",
		"kind":    "desktop_app_logs",
		"count":   len(logs),
		"source":  source,
		"logs":    logs,
		"state":   s.svc.State(),
	})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	models, err := s.svc.ListModels(r.Context())
	if err != nil {
		writeOpenAIUpstreamError(w, err)
		return
	}

	data := make([]modelResponse, 0, len(models))
	created := time.Now().Unix()
	for _, model := range models {
		data = append(data, modelResponse{
			ID:      model.ID,
			Object:  "model",
			Created: created,
			OwnedBy: "lingma",
			Name:    model.Name,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data":   data,
	})
}

func (s *Server) handleCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"service": "lingma-proxy",
		"protocols": []string{
			"openai.chat_completions",
			"openai.responses",
			"anthropic.messages",
			"lm_studio.discovery",
			"ollama.discovery",
			"llamacpp.discovery",
			"vllm.discovery",
		},
		"features": map[string]any{
			"streaming":                true,
			"tools":                    true,
			"tool_prompt_emulation":    true,
			"tool_alias_mapping":       true,
			"images":                   true,
			"local_image_paths":        true,
			"remote_image_urls":        true,
			"image_auto_resize":        true,
			"request_log_image_redact": true,
		},
		"recommended_models": map[string]any{
			"default":     "kmodel",
			"agent_tools": []string{"kmodel", "MiniMax-M2.7", "Qwen3-Coder", "Qwen3.6-Plus"},
			"vision":      []string{"Kimi-K2.6", "Qwen3-Max", "Qwen3.6-Plus", "MiniMax-M2.7", "Auto"},
			"coding":      []string{"kmodel", "Qwen3-Coder", "MiniMax-M2.7"},
		},
		"model_metadata": map[string]any{
			"Kimi-K2.6": map[string]any{
				"context_window_tokens": 256000,
				"modalities":            []string{"text", "image", "video"},
				"capabilities":          []string{"agent", "coding", "tool_use", "vision"},
				"basis":                 "official_kimi_docs",
				"source":                "https://platform.kimi.ai/docs/guide/kimi-k2-6-quickstart",
			},
			"Qwen3-Coder": map[string]any{
				"context_window_tokens": 256000,
				"context_window_note":   "native 256K; official Qwen material describes extension up to 1M with extrapolation",
				"modalities":            []string{"text"},
				"capabilities":          []string{"agentic_coding", "tool_use"},
				"basis":                 "official_qwen_docs",
				"source":                "https://qwenlm.github.io/blog/qwen3-coder/",
			},
			"MiniMax-M2.7": map[string]any{
				"context_window_tokens": 204800,
				"modalities":            []string{"text"},
				"capabilities":          []string{"agent", "coding", "tool_use", "skills"},
				"basis":                 "minimax_and_nvidia_model_cards",
				"source":                "https://developer.nvidia.com/blog/minimax-m2-7-advances-scalable-agentic-workflows-on-nvidia-platforms-for-complex-ai-applications/",
			},
			"Qwen3.6-Plus": map[string]any{
				"context_window_tokens": nil,
				"modalities":            []string{"text", "image"},
				"capabilities":          []string{"general", "vision_observed_via_lingma"},
				"basis":                 "observed_via_lingma_proxy; no official Lingma-specific context length published in this proxy",
			},
		},
		"endpoints": map[string]any{
			"openai_chat":        []string{"/v1/chat/completions", "/api/v1/chat/completions"},
			"openai_responses":   []string{"/v1/responses", "/api/v1/responses"},
			"anthropic_messages": "/v1/messages",
			"models":             []string{"/v1/models", "/api/v1/models", "/api/tags"},
			"capabilities":       []string{"/capabilities", "/v1/capabilities"},
			"props":              []string{"/props", "/v1/props"},
			"version":            "/version",
		},
	})
}

func (s *Server) handleLMStudioModels(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	models, err := s.svc.ListModels(r.Context())
	if err != nil {
		writeOpenAIUpstreamError(w, err)
		return
	}

	items := make([]map[string]any, 0, len(models))
	for _, model := range models {
		items = append(items, map[string]any{
			"id":                 model.ID,
			"key":                model.ID,
			"display_name":       model.Name,
			"type":               "llm",
			"publisher":          "lingma",
			"max_context_length": 128000,
			"loaded_instances": []map[string]any{
				{
					"id":    model.ID,
					"model": model.ID,
					"config": map[string]any{
						"context_length": 128000,
					},
				},
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": items})
}

func (s *Server) handleOllamaTags(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	models, err := s.svc.ListModels(r.Context())
	if err != nil {
		writeOpenAIUpstreamError(w, err)
		return
	}

	items := make([]map[string]any, 0, len(models))
	for _, model := range models {
		items = append(items, map[string]any{
			"name":        model.ID,
			"model":       model.ID,
			"modified_at": time.Now().UTC().Format(time.RFC3339),
			"size":        0,
			"digest":      "",
			"details": map[string]any{
				"family":             "lingma",
				"families":           []string{"lingma"},
				"parameter_size":     "",
				"quantization_level": "",
			},
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": items})
}

func (s *Server) handleModelProps(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	model := strings.TrimSpace(s.svc.DefaultModel())
	if model == "" {
		model = "kmodel"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"model_alias":   model,
		"chat_template": "{{ .Messages }}",
		"default_generation_settings": map[string]any{
			"n_ctx":       128000,
			"temperature": 0.7,
			"top_p":       1,
		},
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": version.Version,
		"service": "lingma-proxy",
	})
}

func (s *Server) handleAnthropicCountTokens(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}

	var req anthropicRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"input_tokens": estimateAnthropicInputTokens(req),
	})
}

func (s *Server) handleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeAnthropicError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	outcome, release := s.acquireRequestSlot(r)
	defer release()
	switch outcome {
	case slotAcquired:
	case slotClientGone:
		writeAnthropicError(w, http.StatusRequestTimeout, "timeout_error", "request was cancelled while waiting for a proxy execution slot")
		return
	default:
		writeAnthropicError(w, http.StatusTooManyRequests, "rate_limit_error", s.queueFullMessage())
		return
	}

	var req anthropicRequest
	if err := decodeJSON(r, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	if crossOriginNamedImageRead(r) && namedImageRequest(req.Messages) {
		writeAnthropicError(w, http.StatusForbidden, "invalid_request_error", crossOriginImageMessage)
		return
	}

	if call, ok := anthropicHostedWebSearchCall(req); ok {
		if req.Stream {
			s.writeAnthropicHostedToolStream(w, req.Model, call)
			return
		}

		s.writeAnthropicHostedToolResponse(w, req.Model, call)
		return
	}

	normalized, err := normalizeAnthropicRequest(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	s.applyDefaultModel(&normalized)

	if req.Stream {
		s.handleAnthropicStream(w, r, normalized)
		return
	}

	result, err := s.svc.Generate(r.Context(), normalized)
	if err != nil {
		writeAnthropicUpstreamError(w, err)
		return
	}

	content := make([]map[string]any, 0, 2+len(result.ToolCalls))
	if shouldEmitAnthropicThinking(normalized, result) {
		content = append(content, map[string]any{"type": "thinking", "thinking": result.ThoughtText})
	}
	// Whitespace-only text is not text. The service trims the prose in front of
	// a consumed action block, so a pure tool turn that streamed a blank line
	// first reports no text at all; announcing a block for it here put text at
	// index 0 and the tool_use at 1, and official SDKs assemble the content
	// array by index, so the two views of one turn disagreed.
	if strings.TrimSpace(result.Text) != "" || len(result.ToolCalls) == 0 {
		content = append(content, map[string]any{"type": "text", "text": result.Text})
	}
	stopReason := anthropicStopReason(result)
	if len(result.ToolCalls) > 0 {
		for _, tc := range result.ToolCalls {
			content = append(content, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Name,
				"input": tc.Arguments,
			})
		}
		stopReason = "tool_use"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":            fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		"type":          "message",
		"role":          "assistant",
		"content":       content,
		"model":         result.Model,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  result.InputTokens,
			"output_tokens": result.OutputTokens,
		},
	})
}

func (s *Server) handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	outcome, release := s.acquireRequestSlot(r)
	defer release()
	switch outcome {
	case slotAcquired:
	case slotClientGone:
		writeOpenAIError(w, http.StatusRequestTimeout, "timeout_error", "request was cancelled while waiting for a proxy execution slot")
		return
	default:
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", s.queueFullMessage())
		return
	}

	var req openAIChatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if crossOriginNamedImageRead(r) && namedImageRequest(req.Messages) {
		writeOpenAIError(w, http.StatusForbidden, "invalid_request_error", crossOriginImageMessage)
		return
	}

	normalized, err := normalizeOpenAIRequest(r.Context(), req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	s.applyDefaultModel(&normalized)

	if req.Stream {
		s.handleOpenAIStream(w, r, normalized)
		return
	}

	result, err := s.svc.Generate(r.Context(), normalized)
	if err != nil {
		writeOpenAIUpstreamError(w, err)
		return
	}

	writeOpenAIChatCompletion(w, result)
}

func (s *Server) handleOpenAIResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		writeOpenAIError(w, http.StatusMethodNotAllowed, "invalid_request_error", "method not allowed")
		return
	}
	outcome, release := s.acquireRequestSlot(r)
	defer release()
	switch outcome {
	case slotAcquired:
	case slotClientGone:
		writeOpenAIError(w, http.StatusRequestTimeout, "timeout_error", "request was cancelled while waiting for a proxy execution slot")
		return
	default:
		writeOpenAIError(w, http.StatusTooManyRequests, "rate_limit_error", s.queueFullMessage())
		return
	}

	var req openAIResponsesRequest
	if err := decodeJSON(r, &req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if crossOriginNamedImageRead(r) && namedImageResponsesInput(req.Input) {
		writeOpenAIError(w, http.StatusForbidden, "invalid_request_error", crossOriginImageMessage)
		return
	}

	chatReq, err := responsesRequestToChatRequest(r.Context(), req)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	normalized, err := normalizeOpenAIRequest(r.Context(), chatReq)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	s.applyDefaultModel(&normalized)

	if req.Stream {
		s.handleOpenAIResponsesStream(w, r, normalized)
		return
	}

	result, err := s.svc.Generate(r.Context(), normalized)
	if err != nil {
		writeOpenAIUpstreamError(w, err)
		return
	}
	writeOpenAIResponse(w, result, normalized)
}

// anthropicStreamBlocks owns the content-block lifecycle of one streamed
// Anthropic message. The official SDKs index a message's content array by the
// block index, so indexes are handed out in announcement order starting at 0:
// thinking the client asked for but the backend never produced consumes no
// index, which is what left a pure tool turn's only block at 1 and crashed the
// official Python SDK with an IndexError.
type anthropicStreamBlocks struct {
	w       http.ResponseWriter
	flusher http.Flusher

	thinkingAnnounced bool
	thinkingOpen      bool
	textAnnounced     bool
	// textOpen tracks whether the text block's content_block_stop is still
	// owed, which textAnnounced cannot answer: FlushLead opens and closes that
	// block inside one call, and a closed-but-announced block made CloseAnnounced
	// emit a second content_block_stop for an index the client had already seen
	// closed. It mirrors thinkingOpen, and only CloseAnnounced consults it.
	textOpen  bool
	textIndex int
	// lead holds the whitespace deltas that arrived before the first
	// non-whitespace one. They are prepended to the delta that finally opens
	// the block, so not one byte is lost; and a turn whose whole prose was
	// whitespace and that ends in tool calls never opens a text block at all,
	// which is exactly what the non-streaming body reports for it.
	lead string
}

// ThinkingDelta streams the thinking block, announcing it at index 0 on the
// first delta. A thought that arrives after text has opened owns no index and
// is dropped: there is no honest slot ahead of text the client already holds.
func (b *anthropicStreamBlocks) ThinkingDelta(delta string) bool {
	if delta == "" {
		return true
	}
	if b.textAnnounced {
		return true
	}
	if !b.thinkingAnnounced {
		if err := writeSSEEvent(b.w, b.flusher, "content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         0,
			"content_block": map[string]any{"type": "thinking", "thinking": ""},
		}); err != nil {
			return false
		}
		b.thinkingAnnounced = true
		b.thinkingOpen = true
	}
	return writeSSEEvent(b.w, b.flusher, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "thinking_delta", "thinking": delta},
	}) == nil
}

// TextDelta closes an open thinking block and streams the text block,
// announcing it at the next index in announcement order on its first
// non-whitespace delta.
func (b *anthropicStreamBlocks) TextDelta(delta string) bool {
	if delta == "" {
		return true
	}
	if !b.textAnnounced && strings.TrimSpace(delta) == "" {
		b.lead += delta
		return true
	}
	if b.thinkingOpen {
		if err := writeSSEEvent(b.w, b.flusher, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": 0,
		}); err != nil {
			return false
		}
		b.thinkingOpen = false
	}
	if !b.textAnnounced {
		b.textIndex = 0
		if b.thinkingAnnounced {
			b.textIndex = 1
		}
		if err := writeSSEEvent(b.w, b.flusher, "content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         b.textIndex,
			"content_block": map[string]any{"type": "text", "text": ""},
		}); err != nil {
			return false
		}
		b.textAnnounced = true
		b.textOpen = true
	}
	if b.lead != "" {
		delta = b.lead + delta
		b.lead = ""
	}
	return writeSSEEvent(b.w, b.flusher, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": b.textIndex,
		"delta": map[string]any{"type": "text_delta", "text": delta},
	}) == nil
}

// FlushLead emits the held whitespace as a text block when the turn ended
// without any other prose. It runs only for a turn with no tool calls: that is
// precisely the case where the non-streaming body still reports the blank text,
// and dropping it here would leave the stream with no content block at all.
func (b *anthropicStreamBlocks) FlushLead(hasToolCalls bool) bool {
	if b.textAnnounced || b.lead == "" || hasToolCalls {
		return true
	}
	if b.thinkingOpen {
		if err := writeSSEEvent(b.w, b.flusher, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": 0,
		}); err != nil {
			return false
		}
		b.thinkingOpen = false
	}
	b.textIndex = 0
	if b.thinkingAnnounced {
		b.textIndex = 1
	}
	if err := writeSSEEvent(b.w, b.flusher, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         b.textIndex,
		"content_block": map[string]any{"type": "text", "text": ""},
	}); err != nil {
		return false
	}
	b.textAnnounced = true
	b.textOpen = true
	lead := b.lead
	b.lead = ""
	if err := writeSSEEvent(b.w, b.flusher, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": b.textIndex,
		"delta": map[string]any{"type": "text_delta", "text": lead},
	}); err != nil {
		return false
	}
	if err := writeSSEEvent(b.w, b.flusher, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": b.textIndex,
	}); err != nil {
		return false
	}
	b.textOpen = false
	return true
}

// CloseAnnounced stops every open block at its own index.
func (b *anthropicStreamBlocks) CloseAnnounced() bool {
	if b.thinkingOpen {
		if err := writeSSEEvent(b.w, b.flusher, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": 0,
		}); err != nil {
			return false
		}
		b.thinkingOpen = false
	}
	if b.textOpen {
		if err := writeSSEEvent(b.w, b.flusher, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": b.textIndex,
		}); err != nil {
			return false
		}
		b.textOpen = false
	}
	return true
}

// FinishToolCalls appends the turn's tool calls after every announced block,
// each with the full start/delta/stop lifecycle at a dense index.
func (b *anthropicStreamBlocks) FinishToolCalls(calls []toolemulation.ToolCall) bool {
	blockIndex := 0
	if b.thinkingAnnounced {
		blockIndex++
	}
	if b.textAnnounced {
		blockIndex++
	}
	for _, tc := range calls {
		if err := writeSSEEvent(b.w, b.flusher, "content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         blockIndex,
			"content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": map[string]any{}},
		}); err != nil {
			return false
		}
		argsJSON, _ := json.Marshal(tc.Arguments)
		if err := writeSSEEvent(b.w, b.flusher, "content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": blockIndex,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": string(argsJSON)},
		}); err != nil {
			return false
		}
		if err := writeSSEEvent(b.w, b.flusher, "content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": blockIndex,
		}); err != nil {
			return false
		}
		blockIndex++
	}
	return true
}

func (s *Server) handleAnthropicStream(w http.ResponseWriter, r *http.Request, req service.ChatRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming is not supported by this server")
		return
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "lingma"
	}
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())

	if shouldAggregateToolStream(req) {
		// Open the stream before Generate, exactly as the incremental branch
		// does. The aggregate turn blocks for the whole model call, and a client
		// with no first payload inside 60s (the Qoder SDK's own rule) kills the
		// turn. The keep-alive writer covers the silence, and a failure here is
		// reported as the protocol's in-stream error event because the 200 and
		// the SSE headers are already committed.
		streamingHeaders(w)
		flusher.Flush()
		aggregate := newSSEKeepaliveWriter(w, flusher)
		defer aggregate.Close()
		w, flusher = aggregate, aggregate

		result, err := s.svc.Generate(r.Context(), req)
		if err != nil {
			_ = writeSSEEvent(w, flusher, "error", map[string]any{
				"type":  "error",
				"error": map[string]any{"type": "api_error", "message": err.Error()},
			})
			_ = writeSSEEvent(w, flusher, "message_stop", map[string]any{"type": "message_stop"})
			return
		}

		if err := writeSSEEvent(w, flusher, "message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            msgID,
				"type":          "message",
				"role":          "assistant",
				"content":       []any{},
				"model":         model,
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage": map[string]any{
					// Zero, not result.InputTokens: the incremental branch below
					// cannot know the prompt size when message_start is written,
					// and a client that reads usage from message_start must not see
					// it change because one env switch flipped. The real number is
					// on the non-streaming body, which reports it directly.
					"input_tokens":  0,
					"output_tokens": 0,
				},
			},
		}); err != nil {
			return
		}

		index := 0
		if shouldEmitAnthropicThinking(req, result) {
			if err := writeSSEEvent(w, flusher, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         index,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}); err != nil {
				return
			}
			if err := writeSSEEvent(w, flusher, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "thinking_delta", "thinking": result.ThoughtText},
			}); err != nil {
				return
			}
			if err := writeSSEEvent(w, flusher, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			}); err != nil {
				return
			}
			index++
		}
		// Same rule as the incremental path and the non-streaming body: a turn
		// whose prose is only whitespace and that ends in tool calls reports no
		// text block at all.
		if strings.TrimSpace(result.Text) != "" {
			if err := writeSSEEvent(w, flusher, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         index,
				"content_block": map[string]any{"type": "text", "text": ""},
			}); err != nil {
				return
			}
			if err := writeSSEEvent(w, flusher, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "text_delta", "text": result.Text},
			}); err != nil {
				return
			}
			if err := writeSSEEvent(w, flusher, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			}); err != nil {
				return
			}
			index++
		}

		for _, tc := range result.ToolCalls {
			if err := writeSSEEvent(w, flusher, "content_block_start", map[string]any{
				"type":          "content_block_start",
				"index":         index,
				"content_block": map[string]any{"type": "tool_use", "id": tc.ID, "name": tc.Name, "input": map[string]any{}},
			}); err != nil {
				return
			}
			argsJSON, _ := json.Marshal(tc.Arguments)
			if err := writeSSEEvent(w, flusher, "content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": index,
				"delta": map[string]any{"type": "input_json_delta", "partial_json": string(argsJSON)},
			}); err != nil {
				return
			}
			if err := writeSSEEvent(w, flusher, "content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": index,
			}); err != nil {
				return
			}
			index++
		}

		stopReason := anthropicStopReason(result)
		if len(result.ToolCalls) > 0 {
			stopReason = "tool_use"
		}
		_ = writeSSEEvent(w, flusher, "message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": map[string]any{
				"output_tokens": result.OutputTokens,
			},
		})
		_ = writeSSEEvent(w, flusher, "message_stop", map[string]any{"type": "message_stop"})
		return
	}

	events, done, err := s.svc.GenerateStream(r.Context(), req)
	if err != nil {
		writeAnthropicUpstreamError(w, err)
		return
	}

	streamingHeaders(w)
	// From here to the last frame the turn can fall silent for whole minutes
	// (the model thinking, the tool filter holding an action block back), and
	// clients read silence as a dead stream. The keep-alive writer beats with
	// the official ping event: the SDK accumulators ignore it, and clients
	// whose idle timer only counts parsed events (Codex CLI) still see it.
	heartbeat := newSSEKeepaliveWriter(w, flusher)
	defer heartbeat.Close()
	heartbeat.setEventFrame(anthropicPingFrame)
	w, flusher = heartbeat, heartbeat
	if err := writeSSEEvent(w, flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	}); err != nil {
		return
	}

	filter := newToolStreamFilter(req)
	eventsCh := events
	doneCh := done
	var final *service.ChatResult
	var finalErr error
	thinkingEnabled := thinkingRequested(req.ReasoningEffort)
	blocks := &anthropicStreamBlocks{w: w, flusher: flusher}

	// emitText streams already-filtered deltas through the block allocator,
	// which opens and closes thinking/text blocks as the turn requires. It
	// reports false once the client is gone, which is the caller's signal to
	// stop writing.
	emitText := func(deltas []string) bool {
		for _, delta := range deltas {
			if delta == "" {
				continue
			}
			if !blocks.TextDelta(delta) {
				return false
			}
		}
		return true
	}

	// A turn can stay silent until the model's first delta lands, which is long
	// enough to trip the first-token timeouts in IDE clients; the keep-alive
	// writer installed above keeps the stream alive instead.

	for eventsCh != nil || doneCh != nil {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-eventsCh:
			if !ok {
				eventsCh = nil
				continue
			}
			switch event.Type {
			case service.StreamEventThinking:
				if !thinkingEnabled || strings.TrimSpace(event.Delta) == "" {
					continue
				}
				if !blocks.ThinkingDelta(event.Delta) {
					return
				}
			default:
				if !emitText(filter.Push(event.Delta)) {
					return
				}
			}
		case result, ok := <-doneCh:
			if !ok {
				doneCh = nil
				continue
			}
			final = result.Result
			finalErr = result.Err
			doneCh = nil
		}
	}

	if finalErr != nil {
		_ = writeSSEEvent(w, flusher, "error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": finalErr.Error(),
			},
		})
		return
	}
	if final == nil {
		_ = writeSSEEvent(w, flusher, "error", map[string]any{
			"type": "error",
			"error": map[string]any{
				"type":    "api_error",
				"message": "stream finished without a final result",
			},
		})
		return
	}
	// Whatever the filter still holds is prose: an action block it consumed is
	// already gone from pending, and an unterminated fence is not a block.
	if !emitText(filter.Flush()) {
		return
	}
	if !blocks.FlushLead(len(final.ToolCalls) > 0) {
		return
	}
	if !blocks.CloseAnnounced() {
		return
	}
	if !blocks.FinishToolCalls(final.ToolCalls) {
		return
	}
	stopReason := anthropicStopReason(final)
	if len(final.ToolCalls) > 0 {
		stopReason = "tool_use"
	}
	if err := writeSSEEvent(w, flusher, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   stopReason,
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": final.OutputTokens,
		},
	}); err != nil {
		return
	}
	_ = writeSSEEvent(w, flusher, "message_stop", map[string]any{
		"type": "message_stop",
	})
}

func (s *Server) writeAnthropicHostedToolResponse(w http.ResponseWriter, model string, call toolemulation.ToolCall) {
	model = strings.TrimSpace(model)
	if model == "" {
		model = "lingma"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":   fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		"type": "message",
		"role": "assistant",
		"content": []map[string]any{{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Name,
			"input": call.Arguments,
		}},
		"model":         model,
		"stop_reason":   "tool_use",
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
		},
	})
}

func (s *Server) writeAnthropicHostedToolStream(w http.ResponseWriter, model string, call toolemulation.ToolCall) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "streaming is not supported by this server")
		return
	}

	model = strings.TrimSpace(model)
	if model == "" {
		model = "lingma"
	}
	streamingHeaders(w)
	msgID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	if err := writeSSEEvent(w, flusher, "message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"content":       []any{},
			"model":         model,
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens":  0,
				"output_tokens": 0,
			},
		},
	}); err != nil {
		return
	}
	if err := writeSSEEvent(w, flusher, "content_block_start", map[string]any{
		"type":          "content_block_start",
		"index":         0,
		"content_block": map[string]any{"type": "tool_use", "id": call.ID, "name": call.Name, "input": map[string]any{}},
	}); err != nil {
		return
	}
	argsJSON, _ := json.Marshal(call.Arguments)
	if err := writeSSEEvent(w, flusher, "content_block_delta", map[string]any{
		"type":  "content_block_delta",
		"index": 0,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": string(argsJSON)},
	}); err != nil {
		return
	}
	if err := writeSSEEvent(w, flusher, "content_block_stop", map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	}); err != nil {
		return
	}
	_ = writeSSEEvent(w, flusher, "message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason":   "tool_use",
			"stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens": 0,
		},
	})
	_ = writeSSEEvent(w, flusher, "message_stop", map[string]any{"type": "message_stop"})
}

func (s *Server) handleOpenAIStream(w http.ResponseWriter, r *http.Request, req service.ChatRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "streaming is not supported by this server")
		return
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "lingma"
	}
	chatID := fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano())
	created := time.Now().Unix()

	if shouldAggregateToolStream(req) {
		// Same shape as the Anthropic aggregate branch: open the stream and
		// start beating before the blocking Generate, so the turn is never a
		// silent socket. A failure now travels as the stream's own error chunk,
		// which is the only shape a committed 200 can still carry.
		streamingHeaders(w)
		flusher.Flush()
		aggregate := newSSEKeepaliveWriter(w, flusher)
		defer aggregate.Close()
		w, flusher = aggregate, aggregate

		result, err := s.svc.Generate(r.Context(), req)
		if err != nil {
			_ = writeOpenAIChunk(w, flusher, map[string]any{
				"error": map[string]any{
					"message": err.Error(),
					"type":    "api_error",
					"code":    nil,
					"param":   nil,
				},
			})
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		_ = writeOpenAIChunk(w, flusher, map[string]any{
			"id": chatID, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}},
		})
		if result.Text != "" {
			_ = writeOpenAIChunk(w, flusher, map[string]any{
				"id": chatID, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []map[string]any{{"index": 0, "delta": map[string]any{"content": result.Text}, "finish_reason": nil}},
			})
		}
		for i, tc := range result.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			_ = writeOpenAIChunk(w, flusher, map[string]any{
				"id": chatID, "object": "chat.completion.chunk", "created": created, "model": model,
				"choices": []map[string]any{{
					"index": 0,
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index": i, "id": tc.ID, "type": "function",
							"function": map[string]any{"name": tc.Name, "arguments": string(argsJSON)},
						}},
					},
					"finish_reason": nil,
				}},
			})
		}
		finishReason := openAIFinishReason(result)
		if len(result.ToolCalls) > 0 {
			finishReason = "tool_calls"
		}
		_ = writeOpenAIChunk(w, flusher, map[string]any{
			"id": chatID, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []map[string]any{{"index": 0, "delta": map[string]any{}, "finish_reason": finishReason}},
		})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	events, done, err := s.svc.GenerateStream(r.Context(), req)
	if err != nil {
		writeOpenAIUpstreamError(w, err)
		return
	}

	streamingHeaders(w)
	// Same silence hazard as the Anthropic path: the tool filter can hold the
	// whole answer back, and the backend is mute while it works. Comment lines
	// from the keep-alive writer keep the client's idle timer honest.
	heartbeat := newSSEKeepaliveWriter(w, flusher)
	defer heartbeat.Close()
	w, flusher = heartbeat, heartbeat
	if err := writeOpenAIChunk(w, flusher, map[string]any{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index": 0,
				"delta": map[string]any{
					"role": "assistant",
				},
				"finish_reason": nil,
			},
		},
	}); err != nil {
		return
	}

	filter := newToolStreamFilter(req)
	eventsCh := events
	doneCh := done
	var final *service.ChatResult
	var finalErr error

	for eventsCh != nil || doneCh != nil {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-eventsCh:
			if !ok {
				eventsCh = nil
				continue
			}
			for _, delta := range filter.Push(event.Delta) {
				if delta == "" {
					continue
				}
				if err := writeOpenAIChunk(w, flusher, map[string]any{
					"id":      chatID,
					"object":  "chat.completion.chunk",
					"created": created,
					"model":   model,
					"choices": []map[string]any{
						{
							"index": 0,
							"delta": map[string]any{
								"content": delta,
							},
							"finish_reason": nil,
						},
					},
				}); err != nil {
					return
				}
			}
		case result, ok := <-doneCh:
			if !ok {
				doneCh = nil
				continue
			}
			final = result.Result
			finalErr = result.Err
			doneCh = nil
		}
	}

	if finalErr != nil {
		_ = writeOpenAIChunk(w, flusher, map[string]any{
			"error": map[string]any{
				"message": finalErr.Error(),
				"type":    "api_error",
				"code":    nil,
				"param":   nil,
			},
		})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	if final == nil {
		_ = writeOpenAIChunk(w, flusher, map[string]any{
			"error": map[string]any{
				"message": "stream finished without a final result",
				"type":    "api_error",
				"code":    nil,
				"param":   nil,
			},
		})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	for _, delta := range filter.Flush() {
		if delta == "" {
			continue
		}
		if err := writeOpenAIChunk(w, flusher, map[string]any{
			"id":      chatID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
			"choices": []map[string]any{
				{
					"index": 0,
					"delta": map[string]any{
						"content": delta,
					},
					"finish_reason": nil,
				},
			},
		}); err != nil {
			return
		}
	}
	for i, tc := range final.ToolCalls {
		argsJSON, _ := json.Marshal(tc.Arguments)
		_ = writeOpenAIChunk(w, flusher, map[string]any{
			"id": chatID, "object": "chat.completion.chunk", "created": created, "model": model,
			"choices": []map[string]any{{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": i, "id": tc.ID, "type": "function",
						"function": map[string]any{"name": tc.Name, "arguments": string(argsJSON)},
					}},
				},
				"finish_reason": nil,
			}},
		})
	}
	finishReason := openAIFinishReason(final)
	if len(final.ToolCalls) > 0 {
		finishReason = "tool_calls"
	}
	if err := writeOpenAIChunk(w, flusher, map[string]any{
		"id":      chatID,
		"object":  "chat.completion.chunk",
		"created": created,
		"model":   model,
		"choices": []map[string]any{
			{
				"index":         0,
				"delta":         map[string]any{},
				"finish_reason": finishReason,
			},
		},
	}); err != nil {
		return
	}
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (s *Server) handleOpenAIResponsesStream(w http.ResponseWriter, r *http.Request, req service.ChatRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeOpenAIError(w, http.StatusInternalServerError, "api_error", "streaming is not supported by this server")
		return
	}

	model := strings.TrimSpace(req.Model)
	if model == "" {
		model = "lingma"
	}
	responseID := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	messageID := fmt.Sprintf("msg_%d", time.Now().UnixNano())
	created := time.Now().Unix()
	streamingHeaders(w)
	// The keep-alive writer takes over before the emitter is built, so it also
	// covers the aggregate branch below, whose blocking Generate leaves the
	// established stream silent for the whole model turn. Its beat is a
	// re-sent response.in_progress: idempotent, invisible to accumulators,
	// and a real event for clients (Codex CLI) whose idle timer only counts
	// parsed events. response.created below opens the gate; the terminal
	// event closes it.
	heartbeat := newSSEKeepaliveWriter(w, flusher)
	defer heartbeat.Close()
	w, flusher = heartbeat, heartbeat
	emitter := newOpenAIResponseStreamEmitter(w, flusher, responseID)
	// Frames go out under the keep-alive writer's own lock, so the number a
	// frame carries and the order frames reach the wire cannot disagree.
	emitter.serial = func(name string, payload map[string]any, after func()) error {
		return heartbeat.writeFrame(func() (string, error) {
			emitter.mu.Lock()
			defer emitter.mu.Unlock()
			frame, err := emitter.marshalEventLocked(name, payload)
			if err != nil {
				return "", err
			}
			after()
			return frame, nil
		})
	}
	heartbeat.setEventFrame(emitter.heartbeatFrame)
	if err := emitter.Event("response.created", map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         responseID,
			"object":     "response",
			"created_at": created,
			"status":     "in_progress",
			"model":      model,
		},
	}); err != nil {
		return
	}

	if shouldAggregateToolStream(req) {
		result, err := s.svc.Generate(r.Context(), req)
		if err != nil {
			_ = emitter.Event("error", map[string]any{"type": "error", "error": openAIStreamErrorObject(err)})
			_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
			flusher.Flush()
			return
		}
		writeOpenAIResponseStreamCompleted(emitter, responseID, created, model, result, messageID, false, thinkingRequested(req.ReasoningEffort), false, "")
		return
	}

	events, done, err := s.svc.GenerateStream(r.Context(), req)
	if err != nil {
		// The 200 and text/event-stream headers are already on the wire, so a JSON
		// error body here would be parsed by clients as a malformed stream frame.
		_ = emitter.Event("error", map[string]any{"type": "error", "error": openAIStreamErrorObject(err)})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}

	filter := newToolStreamFilter(req)
	eventsCh := events
	doneCh := done
	var final *service.ChatResult
	var finalErr error

	// output_index is handed out in announcement order. Thinking owns index 0
	// only when it actually streams: reserving it on the request alone left a
	// message the backend answered without thought stranded at index 1 while
	// response.completed's array put it first, desyncing the two views. A
	// thought that arrives after the message has opened owns no index at all.
	reasoningReserved := thinkingRequested(req.ReasoningEffort)
	reasoning := newResponseReasoningWriter(emitter, "rs_"+responseID, 0)
	textOutputIndex := -1
	messageOpened := false
	var streamedText strings.Builder

	// emitText streams already-filtered deltas, closing an open reasoning item and
	// announcing the message item on the first one. Reports false once the client
	// is gone, which is the caller's signal to stop writing. Mirrors the emitText
	// closure in the Anthropic path so the two streams open and close alike.
	emitText := func(deltas []string) bool {
		for _, delta := range deltas {
			if delta == "" {
				continue
			}
			if !messageOpened {
				if err := reasoning.Close(); err != nil {
					return false
				}
				textOutputIndex = 0
				if reasoning.Opened() {
					textOutputIndex = 1
				}
				if err := writeOpenAIResponseMessageStarted(emitter, messageID, textOutputIndex); err != nil {
					return false
				}
				messageOpened = true
			}
			if err := emitter.Event("response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       messageID,
				"output_index":  textOutputIndex,
				"content_index": 0,
				"delta":         delta,
			}); err != nil {
				return false
			}
			streamedText.WriteString(delta)
		}
		return true
	}

	for eventsCh != nil || doneCh != nil {
		select {
		case <-r.Context().Done():
			return
		case event, ok := <-eventsCh:
			if !ok {
				eventsCh = nil
				continue
			}
			switch event.Type {
			case service.StreamEventThinking:
				if !reasoningReserved || strings.TrimSpace(event.Delta) == "" {
					continue
				}
				if messageOpened {
					// A thought arriving after the message opened has no index
					// left to announce at: index 0 already belongs to the text.
					// The final frame drops it too, so the two views agree.
					continue
				}
				if err := reasoning.Delta(event.Delta); err != nil {
					return
				}
			default:
				if !emitText(filter.Push(event.Delta)) {
					return
				}
			}
		case result, ok := <-doneCh:
			if !ok {
				doneCh = nil
				continue
			}
			final = result.Result
			finalErr = result.Err
			doneCh = nil
		}
	}

	if finalErr != nil {
		_ = reasoning.Close()
		_ = emitter.Event("error", map[string]any{"type": "error", "error": openAIStreamErrorObject(finalErr)})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	if final == nil {
		_ = reasoning.Close()
		_ = emitter.Event("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": "stream finished without a final result"}})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
		return
	}
	// The filter holds back everything after an unclosed fence or XML opening, and
	// at end of turn it is the only one that knows whether that was an action block
	// or prose the client never got. Both sibling streams drain it; skipping this
	// silently truncated the answer in the delta stream while response.completed
	// carried the full text.
	if !emitText(filter.Flush()) {
		return
	}
	// A backend that reports the thought only in the final frame gets no streamed
	// item; response.completed still carries it, replayed by the completed
	// writer at index 0 -- unless the message already claimed that index, in
	// which case there is no honest place left for it and it is dropped from
	// both views.
	reasoningWanted := reasoningReserved && (reasoning.Opened() || !messageOpened)
	if err := reasoning.Close(); err != nil {
		return
	}
	writeOpenAIResponseStreamCompleted(emitter, responseID, created, model, final, messageID, messageOpened, reasoningWanted, reasoning.Opened(), streamedText.String())
}

func shouldAggregateToolStream(req service.ChatRequest) bool {
	return len(req.Tools) > 0 && truthyEnv("LINGMA_AGGREGATE_TOOL_STREAM")
}

// toolStreamFilter withholds an action block from a streaming response while
// letting the prose around it through. It decides with toolemulation's own
// acceptance test, so the client never sees a block the parser is going to
// consume, and never loses text the parser is going to keep.
type toolStreamFilter struct {
	enabled bool
	scan    *toolemulation.ActionBlockScanner
	tools   []toolemulation.ToolDef
	pending string
}

func newToolStreamFilter(req service.ChatRequest) *toolStreamFilter {
	// tool_choice:"none" also disables suppression: applyToolEmulation leaves such
	// blocks in the text, so withholding them here would drop prose the client keeps.
	enabled := len(req.Tools) > 0 && req.ToolChoice.Mode != "none"
	return &toolStreamFilter{
		enabled: enabled,
		scan:    toolemulation.NewActionBlockScanner(req.Tools),
		tools:   req.Tools,
	}
}

func (f *toolStreamFilter) Push(delta string) []string {
	if delta == "" {
		return nil
	}
	if !f.enabled {
		return []string{delta}
	}
	f.pending += delta
	var out []string
	for {
		start, end, unterminated := f.scan.FindSpan(f.pending)
		switch {
		case unterminated:
			// A block has opened but not closed. Its prose is safe; the rest is
			// withheld until Flush can decide whether it was ever an action block.
			if start > 0 {
				out = append(out, f.pending[:start])
				f.scan.Discard(start)
				f.pending = f.pending[start:]
			}
			return out
		case end > 0:
			if start > 0 {
				out = append(out, f.pending[:start])
			}
			f.scan.Discard(end)
			f.pending = f.pending[end:]
		default:
			safe := len(f.pending) - toolemulation.ActionOpenPrefixHold(f.pending)
			if safe > 0 {
				out = append(out, f.pending[:safe])
				f.scan.Discard(safe)
				f.pending = f.pending[safe:]
			}
			return out
		}
	}
}

// Flush returns whatever the filter withheld. An opening fence that never closed
// is not an action block by the parser's own rules, so it is prose and has to go
// back to the client instead of being dropped. A hybrid block that completed on
// its braces but never saw a close marker was undecidable mid-stream; here, at
// end of stream, the final-text parser decides, and whatever it would consume
// must not reach the client as prose.
func (f *toolStreamFilter) Flush() []string {
	if f.pending == "" {
		return nil
	}
	out := f.pending
	f.pending = ""
	if !f.enabled {
		return []string{out}
	}
	_, clean, err := toolemulation.ParseActionBlocks(out, f.tools, toolemulation.Config{})
	if err != nil {
		return []string{out}
	}
	if strings.TrimSpace(clean) == "" {
		return nil
	}
	return []string{clean}
}

// anthropicStopReason names how a turn with no pending tool call ended. Callers
// override it with "tool_use" whenever the model asked for a tool, because that
// outranks everything else the backend reported.
func anthropicStopReason(result *service.ChatResult) string {
	if result != nil && result.StopReason == "max_tokens" {
		return "max_tokens"
	}
	return "end_turn"
}

// openAIFinishReason is the same signal in OpenAI's vocabulary: "length" means
// the answer stopped because the budget ran out, not because the model finished.
func openAIFinishReason(result *service.ChatResult) string {
	if result != nil && result.FinishReason == "length" {
		return "length"
	}
	return "stop"
}

func truthyEnv(name string) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(name)))
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func anthropicHostedWebSearchCall(req anthropicRequest) (toolemulation.ToolCall, bool) {
	if !hasAnthropicHostedWebSearchTool(req.Tools) {
		return toolemulation.ToolCall{}, false
	}
	if hasAnthropicToolResult(req.Messages) {
		return toolemulation.ToolCall{}, false
	}
	if !anthropicHostedWebSearchRequested(req.Tools, req.ToolChoice) {
		return toolemulation.ToolCall{}, false
	}

	query := anthropicHostedWebSearchQuery(req.Messages)
	if query == "" {
		return toolemulation.ToolCall{}, false
	}
	return toolemulation.ToolCall{
		ID:        fmt.Sprintf("toolu_%d", time.Now().UnixNano()),
		Name:      "web_search",
		Arguments: map[string]any{"query": query},
	}, true
}

func hasAnthropicToolResult(messages []rawMessage) bool {
	for _, message := range messages {
		items, ok := message.Content.([]any)
		if !ok {
			continue
		}
		for _, item := range items {
			m, ok := item.(map[string]any)
			if ok && stringFromAny(m["type"]) == "tool_result" {
				return true
			}
		}
	}
	return false
}

func estimateAnthropicInputTokens(req anthropicRequest) int {
	payload := map[string]any{
		"model":       req.Model,
		"system":      req.System,
		"messages":    req.Messages,
		"tools":       req.Tools,
		"tool_choice": req.ToolChoice,
		"thinking":    req.Thinking,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 1
	}
	runes := len([]rune(string(raw)))
	if runes == 0 {
		return 1
	}
	tokens := (runes + 2) / 3
	if tokens < 1 {
		return 1
	}
	return tokens
}

func hasAnthropicHostedWebSearchTool(raw any) bool {
	items, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(stringFromAny(m["name"])) == "web_search" &&
			toolemulation.IsAnthropicHostedToolType(stringFromAny(m["type"])) {
			return true
		}
	}
	return false
}

func anthropicHostedWebSearchRequested(tools any, choice any) bool {
	if m, ok := choice.(map[string]any); ok {
		if strings.TrimSpace(stringFromAny(m["name"])) == "web_search" {
			return true
		}
	}

	items, ok := tools.([]any)
	if !ok || len(items) != 1 {
		return false
	}
	m, ok := items[0].(map[string]any)
	if !ok {
		return false
	}
	return strings.TrimSpace(stringFromAny(m["name"])) == "web_search" &&
		toolemulation.IsAnthropicHostedToolType(stringFromAny(m["type"]))
}

func anthropicHostedWebSearchQuery(messages []rawMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if strings.ToLower(strings.TrimSpace(messages[i].Role)) != "user" {
			continue
		}
		text := strings.TrimSpace(extractText(messages[i].Content))
		if text == "" {
			continue
		}
		return cleanHostedWebSearchQuery(text)
	}
	return ""
}

func cleanHostedWebSearchQuery(text string) string {
	text = strings.TrimSpace(text)
	prefixes := []string{
		"Perform a web search for the query:",
		"Search the web for:",
		"Web search query:",
	}
	lower := strings.ToLower(text)
	for _, prefix := range prefixes {
		idx := strings.Index(lower, strings.ToLower(prefix))
		if idx >= 0 {
			text = strings.TrimSpace(text[idx+len(prefix):])
			break
		}
	}
	text = strings.Trim(text, " \t\r\n\"'`")
	return text
}

func normalizeAnthropicRequest(req anthropicRequest) (service.ChatRequest, error) {
	messages := make([]service.ChatMessage, 0, len(req.Messages))
	for _, message := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		switch role {
		case "user":
			text, toolResults := extractAnthropicUserContent(message.Content)
			images := extractAnthropicImages(message.Content)
			if text != "" || len(images) > 0 {
				messages = append(messages, service.ChatMessage{Role: role, Text: text, Images: images})
			}
			for _, tr := range toolResults {
				messages = append(messages, service.ChatMessage{Role: "tool", Text: tr.Content, ToolCallID: tr.ToolUseID})
			}
		case "assistant":
			text, calls := extractAnthropicAssistantContent(message.Content)
			if text != "" || len(calls) > 0 {
				messages = append(messages, service.ChatMessage{Role: role, Text: text, ToolCalls: calls})
			}
		}
	}
	if len(messages) == 0 {
		return service.ChatRequest{}, fmt.Errorf("no user or assistant messages found")
	}

	toolChoice := toolemulation.ToolChoice{Mode: "auto"}
	if req.ToolChoice != nil {
		toolChoice = toolemulation.ExtractAnthropicToolChoice(req.ToolChoice)
	}

	return service.ChatRequest{
		Model:           strings.TrimSpace(req.Model),
		System:          strings.TrimSpace(extractText(req.System)),
		Messages:        messages,
		Tools:           toolemulation.ExtractAnthropicTools(req.Tools),
		ToolChoice:      toolChoice,
		Temperature:     req.Temperature,
		TopP:            req.TopP,
		TopK:            req.TopK,
		Stop:            req.StopSequences,
		MaxTokens:       req.MaxTokens,
		ReasoningEffort: anthropicReasoningEffort(req),
	}, nil
}

func normalizeOpenAIRequest(ctx context.Context, req openAIChatRequest) (service.ChatRequest, error) {
	messages := make([]service.ChatMessage, 0, len(req.Messages))
	systemParts := make([]string, 0, 2)
	for _, message := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		switch role {
		case "system", "developer":
			text := strings.TrimSpace(extractText(message.Content))
			if text != "" {
				systemParts = append(systemParts, text)
			}
		case "user":
			text := strings.TrimSpace(extractText(message.Content))
			images := extractOpenAIImages(ctx, message.Content)
			if text != "" || len(images) > 0 {
				messages = append(messages, service.ChatMessage{Role: role, Text: text, Images: images})
			}
		case "assistant":
			text := strings.TrimSpace(extractText(message.Content))
			calls := extractOpenAIToolCalls(message.ToolCalls)
			if text != "" || len(calls) > 0 {
				messages = append(messages, service.ChatMessage{Role: role, Text: text, ToolCalls: calls})
			}
		case "tool":
			// Only a structurally valid content is a result; absent or
			// malformed content is dropped, never fabricated into an empty
			// result.
			if message.ToolCallID == "" || !validToolResultContent(message.Content) {
				continue
			}
			output := strings.TrimSpace(extractText(message.Content))
			messages = append(messages, service.ChatMessage{Role: "tool", Text: output, ToolCallID: message.ToolCallID})
		}
	}
	if len(messages) == 0 {
		return service.ChatRequest{}, fmt.Errorf("no user or assistant messages found")
	}
	// A declaration whose flattened namespace name collides with another tool
	// is ambiguous: extracting it first-wins would silently change which tool
	// a call executes, so the request is rejected with the colliding name.
	if collisions := toolemulation.FindToolNameCollisions(req.Tools); len(collisions) > 0 {
		return service.ChatRequest{}, fmt.Errorf("ambiguous tool declaration: %q is declared more than once once namespaces are flattened", collisions[0])
	}
	tools := toolemulation.ExtractTools(req.Tools)
	return service.ChatRequest{
		Model:             strings.TrimSpace(req.Model),
		System:            strings.Join(systemParts, "\n\n"),
		Messages:          messages,
		Tools:             tools,
		ToolChoice:        toolemulation.ResolveToolChoice(tools, toolemulation.ExtractToolChoice(req.ToolChoice)),
		ParallelToolCalls: req.ParallelToolCalls,
		Temperature:       req.Temperature,
		TopP:              req.TopP,
		Stop:              extractStop(req.Stop),
		PresencePenalty:   req.PresencePenalty,
		FrequencyPenalty:  req.FrequencyPenalty,
		MaxTokens:         maxTokens(req.MaxTokens, req.MaxCompletionTokens),
		Seed:              req.Seed,
		User:              req.User,
		ReasoningEffort:   req.ReasoningEffort,
		ResponseFormat:    extractResponseFormat(req.ResponseFormat),
	}, nil
}

func extractStop(stop any) []string {
	if stop == nil {
		return nil
	}
	switch typed := stop.(type) {
	case string:
		if typed != "" {
			return []string{typed}
		}
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s := stringFromAny(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		return typed
	}
	return nil
}

func extractResponseFormat(rf any) string {
	if rf == nil {
		return ""
	}
	m, ok := rf.(map[string]any)
	if !ok {
		return ""
	}
	if format, ok := m["format"].(map[string]any); ok {
		return stringFromAny(format["type"])
	}
	return stringFromAny(m["type"])
}

func responsesRequestToChatRequest(ctx context.Context, req openAIResponsesRequest) (openAIChatRequest, error) {
	messages, err := responsesInputToMessages(ctx, req.Input)
	if err != nil {
		return openAIChatRequest{}, err
	}
	if instructions := strings.TrimSpace(req.Instructions); instructions != "" {
		messages = append([]rawMessage{{Role: "system", Content: instructions}}, messages...)
	}
	return openAIChatRequest{
		Model:               strings.TrimSpace(req.Model),
		Messages:            messages,
		Stream:              req.Stream,
		MaxTokens:           req.MaxTokens,
		MaxCompletionTokens: req.MaxOutputTokens,
		Tools:               req.Tools,
		ToolChoice:          req.ToolChoice,
		ParallelToolCalls:   req.ParallelToolCalls,
		Temperature:         req.Temperature,
		TopP:                req.TopP,
		Stop:                req.Stop,
		User:                req.User,
		ReasoningEffort:     extractReasoningEffort(req.Reasoning),
		ResponseFormat:      req.Text,
	}, nil
}

func responsesInputToMessages(ctx context.Context, input any) ([]rawMessage, error) {
	switch typed := input.(type) {
	case nil:
		return nil, fmt.Errorf("input is required")
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil, fmt.Errorf("input is required")
		}
		return []rawMessage{{Role: "user", Content: typed}}, nil
	case []any:
		messages := make([]rawMessage, 0, len(typed))
		for _, item := range typed {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch stringFromAny(m["type"]) {
			case "function_call":
				if toolCall := responsesFunctionCallToRawMessage(m); toolCall != nil {
					messages = append(messages, *toolCall)
				}
				continue
			case "function_call_output":
				if toolResult := responsesFunctionCallOutputToRawMessage(m); toolResult != nil {
					messages = append(messages, *toolResult)
				}
				continue
			}
			role := stringFromAny(m["role"])
			if role == "" {
				role = "user"
			}
			content := normalizeResponsesContent(m["content"])
			if text := strings.TrimSpace(extractText(content)); text == "" && len(extractOpenAIImages(ctx, content)) == 0 {
				continue
			}
			messages = append(messages, rawMessage{Role: role, Content: content})
		}
		if len(messages) == 0 {
			return nil, fmt.Errorf("no usable input messages found")
		}
		return messages, nil
	case map[string]any:
		switch stringFromAny(typed["type"]) {
		case "function_call":
			if toolCall := responsesFunctionCallToRawMessage(typed); toolCall != nil {
				return []rawMessage{*toolCall}, nil
			}
		case "function_call_output":
			if toolResult := responsesFunctionCallOutputToRawMessage(typed); toolResult != nil {
				return []rawMessage{*toolResult}, nil
			}
		}
		role := stringFromAny(typed["role"])
		if role == "" {
			role = "user"
		}
		if content := normalizeResponsesContent(typed); strings.TrimSpace(extractText(content)) != "" || len(extractOpenAIImages(ctx, content)) > 0 {
			return []rawMessage{{Role: role, Content: content}}, nil
		}
	}
	return nil, fmt.Errorf("unsupported input format")
}

func responsesFunctionCallToRawMessage(item map[string]any) *rawMessage {
	name := strings.TrimSpace(stringFromAny(item["name"]))
	// A history function_call may arrive split into leaf name plus namespace;
	// replay it as the single qualified name the model was taught, so the
	// model recognises its own past call.
	if ns := strings.TrimSpace(stringFromAny(item["namespace"])); ns != "" && name != "" {
		name = toolemulation.QualifiedToolName(ns, name)
	}
	if name == "" {
		return nil
	}
	callID := strings.TrimSpace(stringFromAny(item["call_id"]))
	if callID == "" {
		callID = strings.TrimSpace(stringFromAny(item["id"]))
	}
	if callID == "" {
		return nil
	}
	arguments := strings.TrimSpace(stringFromAny(item["arguments"]))
	return &rawMessage{
		Role: "assistant",
		ToolCalls: []any{
			map[string]any{
				"id":   callID,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": arguments,
				},
			},
		},
	}
}

func responsesFunctionCallOutputToRawMessage(item map[string]any) *rawMessage {
	callID := strings.TrimSpace(stringFromAny(item["call_id"]))
	if callID == "" {
		return nil
	}
	// An empty-but-present output is a legal result (a tool that produced no
	// text); dropping it broke the call/result pairing. A missing output, or
	// one of the wrong JSON type, is not a result at all and must not be
	// fabricated into one.
	raw, present := item["output"]
	if !present {
		return nil
	}
	output, isString := raw.(string)
	if !isString {
		return nil
	}
	return &rawMessage{
		Role:       "tool",
		Content:    strings.TrimSpace(output),
		ToolCallID: callID,
	}
}

func normalizeResponsesContent(content any) any {
	switch typed := content.(type) {
	case nil:
		return nil
	case string:
		return typed
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch stringFromAny(m["type"]) {
			case "input_text", "output_text", "text":
				text := stringFromAny(m["text"])
				if text == "" {
					text = stringFromAny(m["input_text"])
				}
				out = append(out, map[string]any{"type": "text", "text": text})
			case "input_image", "image_url":
				url := stringFromAny(m["image_url"])
				if nested, ok := m["image_url"].(map[string]any); ok {
					url = stringFromAny(nested["url"])
				}
				if url != "" {
					out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				}
			}
		}
		if len(out) == 0 {
			return content
		}
		return out
	case map[string]any:
		if role := stringFromAny(typed["role"]); role != "" {
			return normalizeResponsesContent(typed["content"])
		}
		if text := stringFromAny(typed["text"]); text != "" {
			return text
		}
	}
	return content
}

func extractReasoningEffort(reasoning any) string {
	m, ok := reasoning.(map[string]any)
	if !ok {
		return ""
	}
	return stringFromAny(m["effort"])
}

// anthropicReasoningEffort resolves the tier a client deliberately picked. Clients
// that expose a thinking-level selector disagree on where to put it on the
// Anthropic wire ("thinking.effort", "output_config.effort", a borrowed
// "reasoning_effort"), so every named tier is checked before the budget heuristic
// is allowed to infer one from "thinking.budget_tokens".
func anthropicReasoningEffort(req anthropicRequest) string {
	for _, candidate := range []string{
		strings.TrimSpace(stringFromAny(req.ReasoningEff)),
		anthropicThinkingEffort(req.Thinking),
		strings.TrimSpace(stringFromAny(req.OutputConfig["effort"])),
		strings.TrimSpace(stringFromAny(req.Reasoning)),
		strings.TrimSpace(extractReasoningEffort(req.Reasoning)),
	} {
		if candidate != "" {
			return candidate
		}
	}
	return inferAnthropicThinkingEffort(req.Thinking)
}

func anthropicThinkingEffort(thinking any) string {
	m, ok := thinking.(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	return strings.TrimSpace(stringFromAny(m["effort"]))
}

// inferAnthropicThinkingEffort guesses a tier from the thinking budget. It is a
// last resort: clients that pick a budget on their own (rather than from a
// deliberate selection) land here, so the result is coarse on purpose.
func inferAnthropicThinkingEffort(thinking any) string {
	m, ok := thinking.(map[string]any)
	if !ok || len(m) == 0 {
		return ""
	}
	mode := strings.ToLower(strings.TrimSpace(stringFromAny(m["type"])))
	switch mode {
	case "disabled":
		return "none"
	case "", "enabled", "adaptive":
		// treat adaptive as an enabled reasoning request with default effort
	default:
		return ""
	}
	budget := parseReasoningBudget(m["budget_tokens"])
	switch {
	case budget > 49152:
		return "max"
	case budget >= 16384:
		return "xhigh"
	case budget >= 4096:
		return "high"
	case budget > 0 && budget < 1024:
		return "low"
	default:
		return "medium"
	}
}

func parseReasoningBudget(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case json.Number:
		if n, err := typed.Int64(); err == nil {
			return int(n)
		}
	case string:
		if typed == "" {
			return 0
		}
		if n, err := strconv.Atoi(typed); err == nil {
			return n
		}
	}
	return 0
}

func maxTokens(a, b int) int {
	if b > 0 {
		return b
	}
	return a
}

// validToolResultContent accepts the content shapes a tool result may carry:
// a string, or an array of content blocks, each an object with a non-empty
// string type; a text block must carry a string text field (empty allowed),
// and a text field on any other block, when present, must be a string. Any
// other shape is not a result; one malformed block invalidates the whole
// array.
func validToolResultContent(content any) bool {
	switch typed := content.(type) {
	case string:
		return true
	case []any:
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				return false
			}
			blockType := strings.TrimSpace(stringFromAny(block["type"]))
			if blockType == "" {
				return false
			}
			text, hasText := block["text"]
			if hasText {
				if _, ok := text.(string); !ok {
					return false
				}
			} else if strings.EqualFold(blockType, "text") {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func extractText(content any) string {
	switch typed := content.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			text := extractText(item)
			if text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text := stringFromAny(typed["text"]); text != "" {
			return text
		}
		if text := stringFromAny(typed["input_text"]); text != "" {
			return text
		}
		if nested := extractText(typed["content"]); nested != "" {
			return nested
		}
		return ""
	default:
		return ""
	}
}

func stringFromAny(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return ""
	}
}

func decodeJSON(r *http.Request, out any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	// A body carries exactly one JSON value. Whatever follows it -- a second
	// concatenated value, or garbage from a proxy chain that spliced two
	// requests -- was never part of what the client meant to send, and stopping
	// at the first value accepted it silently. Token returns io.EOF once only
	// whitespace is left, which is the one case that must keep decoding.
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("invalid JSON body: unexpected content after the top-level JSON value")
	}
	return nil
}

// errorReadCloser replays a read error to whoever reads the body next.
type errorReadCloser struct{ err error }

func (e errorReadCloser) Read([]byte) (int, error) { return 0, e.err }
func (e errorReadCloser) Close() error             { return nil }

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeAnthropicError(w http.ResponseWriter, status int, kind string, message string) {
	writeJSON(w, status, map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    kind,
			"message": message,
		},
	})
}

func writeOpenAIError(w http.ResponseWriter, status int, kind string, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"message": message,
			"type":    kind,
			"code":    nil,
			"param":   nil,
		},
	})
}

// writeAnthropicUpstreamError and writeOpenAIUpstreamError answer a generation
// failure. A transient one -- the bundled CLI losing the openapi call that turns a
// job token into a session -- is reported as 503 so clients retry it instead of
// surfacing a dead end, which is how those failures used to look.
func writeAnthropicUpstreamError(w http.ResponseWriter, err error) {
	status, kind := http.StatusInternalServerError, "api_error"
	if errors.Is(err, remote.ErrTransientUpstream) {
		status, kind = http.StatusServiceUnavailable, "overloaded_error"
	}
	writeAnthropicError(w, status, kind, err.Error())
}

func writeOpenAIUpstreamError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	if errors.Is(err, remote.ErrTransientUpstream) {
		status = http.StatusServiceUnavailable
	}
	writeOpenAIError(w, status, "api_error", err.Error())
}

// openAIStreamErrorObject renders a generation failure for a stream whose 200 and
// text/event-stream headers are already committed, where the 503 above is no
// longer reachable. The retryable hint is what the status code used to carry.
func openAIStreamErrorObject(err error) map[string]any {
	out := map[string]any{"type": "api_error", "message": err.Error()}
	if errors.Is(err, remote.ErrTransientUpstream) {
		out["code"] = "transient_upstream"
	}
	return out
}

func writeOpenAIChatCompletion(w http.ResponseWriter, result *service.ChatResult) {
	created := time.Now().Unix()
	message := map[string]any{
		"role":    "assistant",
		"content": result.Text,
	}
	finishReason := openAIFinishReason(result)
	if len(result.ToolCalls) > 0 {
		toolCalls := make([]map[string]any, 0, len(result.ToolCalls))
		for _, tc := range result.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Arguments)
			toolCalls = append(toolCalls, map[string]any{
				"id":   tc.ID,
				"type": "function",
				"function": map[string]any{
					"name":      tc.Name,
					"arguments": string(argsJSON),
				},
			})
		}
		message["tool_calls"] = toolCalls
		finishReason = "tool_calls"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		"object":  "chat.completion",
		"created": created,
		"model":   result.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
		"usage": map[string]any{
			"prompt_tokens":     result.InputTokens,
			"completion_tokens": result.OutputTokens,
			"total_tokens":      result.InputTokens + result.OutputTokens,
		},
	})
}

// writeOpenAIResponse answers a non-streaming /v1/responses request. req carries
// the reasoning the client asked for: without it a client that explicitly
// disabled thinking still got a reasoning item whenever the backend leaked
// ThoughtText -- the fourth call site H5 was supposed to close.
func writeOpenAIResponse(w http.ResponseWriter, result *service.ChatResult, req service.ChatRequest) {
	responseID := fmt.Sprintf("resp_%d", time.Now().UnixNano())
	created := time.Now().Unix()
	writeJSON(w, http.StatusOK, buildOpenAIResponseBody(responseID, created, result.Model, result, "", shouldEmitResponsesReasoning(req, result)))
}

func buildOpenAIResponseMessageItem(messageID string, text string, status string) map[string]any {
	item := map[string]any{
		"id":      messageID,
		"type":    "message",
		"status":  status,
		"role":    "assistant",
		"content": []map[string]any{},
	}
	// Whitespace-only text is still text: the streamed bytes are the item's
	// identity, so trimming here would make the completed frame diverge from
	// the deltas the client already holds.
	if text != "" {
		item["content"] = []map[string]any{{
			"type": "output_text",
			"text": text,
		}}
	}
	return item
}

// responseTextForResponses is the text of the final frame for a turn the
// stream never opened: trimmed backend prose, tools or not. The 931 acceptance
// caught the old rule of blanking it whenever tool calls existed -- the model's
// answer to "read both files" vanished from response.completed and from the
// non-streaming body even though it streamed fine.
func responseTextForResponses(result *service.ChatResult) string {
	if result == nil {
		return ""
	}
	return strings.TrimSpace(result.Text)
}

func buildOpenAIResponseBody(responseID string, created int64, model string, result *service.ChatResult, messageID string, includeReasoning bool) map[string]any {
	return buildOpenAIResponseBodyWithText(responseID, created, model, result, messageID, includeReasoning, responseTextForResponses(result))
}

// responsesFunctionCallItem renders one function_call output item with the
// wire contract the Responses protocol round-trips: name is the leaf and the
// namespace, when the call belongs to one, rides its own field. Un-namespaced
// calls keep the pre-namespace shape exactly.
func responsesFunctionCallItem(tc toolemulation.ToolCall, arguments string, status string) map[string]any {
	item := map[string]any{
		"type":      "function_call",
		"id":        tc.ID,
		"call_id":   tc.ID,
		"name":      tc.LeafName(),
		"arguments": arguments,
		"status":    status,
	}
	if tc.Namespace != "" {
		item["namespace"] = tc.Namespace
	}
	return item
}

// buildOpenAIResponseBodyWithText lets the streaming path pin the message text
// to the bytes the client already saw, so response.completed reconciles with
// the output_item.done events instead of re-deriving a divergent answer.
func buildOpenAIResponseBodyWithText(responseID string, created int64, model string, result *service.ChatResult, messageID string, includeReasoning bool, text string) map[string]any {
	output := make([]map[string]any, 0, 1+len(result.ToolCalls))
	if includeReasoning && strings.TrimSpace(result.ThoughtText) != "" {
		output = append(output, buildOpenAIResponseReasoningItem("rs_"+responseID, result.ThoughtText, "completed"))
	}
	if text != "" {
		if strings.TrimSpace(messageID) == "" {
			messageID = fmt.Sprintf("msg_%d", time.Now().UnixNano())
		}
		output = append(output, buildOpenAIResponseMessageItem(messageID, text, "completed"))
	}
	for _, tc := range result.ToolCalls {
		argsJSON, _ := json.Marshal(tc.Arguments)
		output = append(output, responsesFunctionCallItem(tc, string(argsJSON), "completed"))
	}
	if strings.TrimSpace(model) == "" {
		model = result.Model
	}
	return map[string]any{
		"id":          responseID,
		"object":      "response",
		"created_at":  created,
		"status":      "completed",
		"model":       model,
		"output":      output,
		"output_text": text,
		"usage": map[string]any{
			"input_tokens":  result.InputTokens,
			"output_tokens": result.OutputTokens,
			"total_tokens":  result.InputTokens + result.OutputTokens,
		},
	}
}

type openAIResponseStreamEmitter struct {
	w          http.ResponseWriter
	flusher    http.Flusher
	responseID string
	// mu guards the fields below. The handler's Event calls and the keep-alive
	// heartbeat goroutine both reach them.
	mu              sync.Mutex
	sequence        int
	started         bool
	terminal        bool
	createdResponse map[string]any
	// serial, when set, marshals and writes one frame while the stream's
	// single-writer lock is held, so a frame's sequence_number is allocated in
	// exactly the order frames reach the wire. Injected by the keep-alive
	// writer; nil means this emitter owns its own (test) writer.
	serial func(name string, payload map[string]any, after func()) error
}

func newOpenAIResponseStreamEmitter(w http.ResponseWriter, flusher http.Flusher, responseID string) *openAIResponseStreamEmitter {
	return &openAIResponseStreamEmitter{w: w, flusher: flusher, responseID: responseID}
}

// marshalEventLocked renders one SSE frame with the sequence_number and
// response_id fields every stream event carries. The caller holds e.mu; Event
// writes the returned frame itself because the keep-alive goroutine must not
// call back into the writer it is beating inside of.
func (e *openAIResponseStreamEmitter) marshalEventLocked(name string, payload map[string]any) (string, error) {
	if _, ok := payload["sequence_number"]; !ok {
		payload["sequence_number"] = e.sequence
		e.sequence++
	}
	if _, ok := payload["response_id"]; !ok && e.responseID != "" {
		payload["response_id"] = e.responseID
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	var frame strings.Builder
	fmt.Fprintf(&frame, "event: %s\n", name)
	fmt.Fprintf(&frame, "data: %s\n\n", body)
	return frame.String(), nil
}

// markEventLocked returns the closure that records an event's effect on the
// stream's state. The caller must hold e.mu, which is what lets the keep-alive
// writer run it under the same lock that ordered the frame: the terminal flag
// then closes the heartbeat gate before the terminal bytes are out, not after.
func (e *openAIResponseStreamEmitter) markEventLocked(name string, payload map[string]any) func() {
	return func() {
		switch name {
		case "response.created":
			// Remember the response object exactly as announced: heartbeat
			// re-sends must carry the same identity, not a reconstruction.
			if resp, ok := payload["response"].(map[string]any); ok {
				e.createdResponse = resp
			}
			e.started = true
		case "response.completed", "error":
			e.terminal = true
		}
	}
}

func (e *openAIResponseStreamEmitter) Event(name string, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	after := e.markEventLocked(name, payload)
	if e.serial != nil {
		return e.serial(name, payload, after)
	}
	e.mu.Lock()
	frame, err := e.marshalEventLocked(name, payload)
	if err == nil {
		after()
	}
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(e.w, frame); err != nil {
		return err
	}
	e.flusher.Flush()
	return nil
}

// heartbeatFrame re-sends the idempotent response.in_progress status event
// during a silent stretch, so event-level idle timers (Codex CLI resets only
// on parsed events, discarding comment lines) see liveness. It stays silent
// before response.created has actually been written — in_progress ahead of
// created opens the event sequence out of order — and after the terminal
// event, where a beat would roll a conforming accumulator's status back from
// completed to in_progress. The re-sent frame consumes a sequence_number like
// any event; the protocol requires them increasing, not contiguous.
func (e *openAIResponseStreamEmitter) heartbeatFrame() (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started || e.terminal || e.createdResponse == nil {
		return "", false
	}
	frame, err := e.marshalEventLocked("response.in_progress", map[string]any{
		"type":     "response.in_progress",
		"response": e.createdResponse,
	})
	if err != nil {
		return "", false
	}
	return frame, true
}

func writeOpenAIResponseMessageStarted(emitter *openAIResponseStreamEmitter, messageID string, outputIndex int) error {
	if err := emitter.Event("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": outputIndex,
		"item":         buildOpenAIResponseMessageItem(messageID, "", "in_progress"),
	}); err != nil {
		return err
	}
	return emitter.Event("response.content_part.added", map[string]any{
		"type":          "response.content_part.added",
		"item_id":       messageID,
		"output_index":  outputIndex,
		"content_index": 0,
		"part": map[string]any{
			"type": "output_text",
			"text": "",
		},
	})
}

func buildOpenAIResponseReasoningItem(itemID string, reasoningText string, status string) map[string]any {
	return map[string]any{
		"id":     itemID,
		"type":   "reasoning",
		"status": status,
		"summary": []map[string]any{{
			"type": "summary_text",
			"text": reasoningText,
		}},
	}
}

// responseReasoningWriter streams one OpenAI Responses reasoning item. The
// announce events fire on the first delta, so a request that asked for reasoning
// but got none emits nothing.
type responseReasoningWriter struct {
	emitter   *openAIResponseStreamEmitter
	itemID    string
	outputIdx int
	opened    bool
	closed    bool
	text      strings.Builder
}

func newResponseReasoningWriter(emitter *openAIResponseStreamEmitter, itemID string, outputIdx int) *responseReasoningWriter {
	return &responseReasoningWriter{emitter: emitter, itemID: itemID, outputIdx: outputIdx}
}

func (r *responseReasoningWriter) Opened() bool { return r.opened }

func (r *responseReasoningWriter) Delta(delta string) error {
	if !r.opened {
		if err := r.emitter.Event("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": r.outputIdx,
			"item":         buildOpenAIResponseReasoningItem(r.itemID, "", "in_progress"),
		}); err != nil {
			return err
		}
		if err := r.emitter.Event("response.reasoning_summary_part.added", map[string]any{
			"type":          "response.reasoning_summary_part.added",
			"item_id":       r.itemID,
			"output_index":  r.outputIdx,
			"summary_index": 0,
			"part": map[string]any{
				"type": "summary_text",
				"text": "",
			},
		}); err != nil {
			return err
		}
		r.opened = true
	}
	if _, err := r.text.WriteString(delta); err != nil {
		return err
	}
	return r.emitter.Event("response.reasoning_summary_text.delta", map[string]any{
		"type":          "response.reasoning_summary_text.delta",
		"item_id":       r.itemID,
		"output_index":  r.outputIdx,
		"summary_index": 0,
		"delta":         delta,
	})
}

func (r *responseReasoningWriter) Close() error {
	if !r.opened || r.closed {
		return nil
	}
	r.closed = true
	summary := r.text.String()
	if err := r.emitter.Event("response.reasoning_summary_text.done", map[string]any{
		"type":          "response.reasoning_summary_text.done",
		"item_id":       r.itemID,
		"output_index":  r.outputIdx,
		"summary_index": 0,
		"text":          summary,
	}); err != nil {
		return err
	}
	if err := r.emitter.Event("response.reasoning_summary_part.done", map[string]any{
		"type":          "response.reasoning_summary_part.done",
		"item_id":       r.itemID,
		"output_index":  r.outputIdx,
		"summary_index": 0,
		"part": map[string]any{
			"type": "summary_text",
			"text": summary,
		},
	}); err != nil {
		return err
	}
	return r.emitter.Event("response.output_item.done", map[string]any{
		"type":         "response.output_item.done",
		"output_index": r.outputIdx,
		"item":         buildOpenAIResponseReasoningItem(r.itemID, summary, "completed"),
	})
}

// writeOpenAIResponseStreamCompleted closes one Responses stream. Indexes are
// assigned by what the client actually saw: reasoningOpened reports an item the
// stream already announced at index 0, reasoningWanted one the final frame
// still owes it, and everything after follows from there -- a reserved but
// never-delivered reasoning slot must not shift the message off index 0.
func writeOpenAIResponseStreamCompleted(emitter *openAIResponseStreamEmitter, responseID string, created int64, model string, result *service.ChatResult, messageID string, messageStarted bool, reasoningWanted bool, reasoningOpened bool, streamedText string) {
	includeReasoning := reasoningWanted && (reasoningOpened || (result != nil && strings.TrimSpace(result.ThoughtText) != ""))
	outputIndex := 0
	if reasoningOpened || includeReasoning {
		if !reasoningOpened {
			// The turn owes the client a reasoning item it never streamed
			// (aggregate mode, or a thought that only surfaced in the final
			// frame): replay its full lifecycle at index 0 so the event stream
			// and the completed array agree.
			late := newResponseReasoningWriter(emitter, "rs_"+responseID, 0)
			// Original bytes, untrimmed: the completed snapshot carries
			// result.ThoughtText verbatim, so a trimmed replay would hand the
			// client two different texts for one item.
			if err := late.Delta(result.ThoughtText); err != nil {
				return
			}
			if err := late.Close(); err != nil {
				return
			}
		}
		outputIndex++
	}
	text := responseTextForResponses(result)
	if messageStarted {
		// An item already announced with output_text.delta has to close -- and
		// has to appear in response.completed -- with the text the client
		// actually saw. Not the trimmed final frame, and not blanked because a
		// tool call won: the streamed bytes are the history the client holds.
		// Not trimmed either: a model that streams " " before its action block
		// opened the item, so closing it with "" would skip the done events and
		// resurrect the dangling-item defect.
		text = streamedText
	}
	if text != "" {
		if !messageStarted {
			_ = writeOpenAIResponseMessageStarted(emitter, messageID, outputIndex)
			// A message the stream never announced still owes the client the
			// delta lifecycle: clients assemble the answer from deltas, and the
			// done-only sequence left the aggregate turn's text invisible.
			_ = emitter.Event("response.output_text.delta", map[string]any{
				"type":          "response.output_text.delta",
				"item_id":       messageID,
				"output_index":  outputIndex,
				"content_index": 0,
				"delta":         text,
			})
		}
		_ = emitter.Event("response.content_part.done", map[string]any{
			"type":          "response.content_part.done",
			"item_id":       messageID,
			"output_index":  outputIndex,
			"content_index": 0,
			"part": map[string]any{
				"type": "output_text",
				"text": text,
			},
		})
		_ = emitter.Event("response.output_text.done", map[string]any{
			"type":          "response.output_text.done",
			"item_id":       messageID,
			"output_index":  outputIndex,
			"content_index": 0,
			"text":          text,
		})
		_ = emitter.Event("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": outputIndex,
			"item":         buildOpenAIResponseMessageItem(messageID, text, "completed"),
		})
		outputIndex++
	}
	for _, tc := range result.ToolCalls {
		argsJSON, _ := json.Marshal(tc.Arguments)
		argsText := string(argsJSON)
		_ = emitter.Event("response.output_item.added", map[string]any{
			"type":         "response.output_item.added",
			"output_index": outputIndex,
			"item":         responsesFunctionCallItem(tc, "", "in_progress"),
		})
		_ = emitter.Event("response.function_call_arguments.delta", map[string]any{
			"type":         "response.function_call_arguments.delta",
			"item_id":      tc.ID,
			"output_index": outputIndex,
			"delta":        argsText,
		})
		argsDone := map[string]any{
			"type":         "response.function_call_arguments.done",
			"item_id":      tc.ID,
			"output_index": outputIndex,
			"name":         tc.LeafName(),
			"arguments":    argsText,
		}
		if tc.Namespace != "" {
			argsDone["namespace"] = tc.Namespace
		}
		_ = emitter.Event("response.function_call_arguments.done", argsDone)
		_ = emitter.Event("response.output_item.done", map[string]any{
			"type":         "response.output_item.done",
			"output_index": outputIndex,
			"item":         responsesFunctionCallItem(tc, argsText, "completed"),
		})
		outputIndex++
	}
	_ = emitter.Event("response.completed", map[string]any{
		"type":     "response.completed",
		"response": buildOpenAIResponseBodyWithText(responseID, created, model, result, messageID, includeReasoning, text),
	})
	_, _ = fmt.Fprint(emitter.w, "data: [DONE]\n\n")
	emitter.flusher.Flush()
}

// thinkingRequested reports whether the client actually asked for reasoning.
// "none" is what an explicit thinking:{"type":"disabled"} normalises to, so it
// must not reserve a content-block index or emit a thinking block either.
func thinkingRequested(effort string) bool {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "", "none":
		return false
	default:
		return true
	}
}

func shouldEmitAnthropicThinking(req service.ChatRequest, result *service.ChatResult) bool {
	return thinkingRequested(req.ReasoningEffort) && result != nil && strings.TrimSpace(result.ThoughtText) != ""
}

func shouldEmitResponsesReasoning(req service.ChatRequest, result *service.ChatResult) bool {
	if !thinkingRequested(req.ReasoningEffort) {
		return false
	}
	if result == nil {
		return true
	}
	return strings.TrimSpace(result.ThoughtText) != ""
}

func streamingHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// writeSSEEvent emits one complete SSE frame in a single Write call. The
// keep-alive writer serializes at Write granularity, so a frame split across
// two Writes could have a comment spliced between its event and data lines,
// where the comment's trailing blank line would dispatch a data-less event.
func writeSSEEvent(w http.ResponseWriter, flusher http.Flusher, event string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var frame strings.Builder
	fmt.Fprintf(&frame, "event: %s\n", event)
	fmt.Fprintf(&frame, "data: %s\n\n", body)
	if _, err := io.WriteString(w, frame.String()); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// writeSSEComment emits a line the SSE spec defines but that carries no event,
// so every conforming parser skips it. Used to hold a stream open before the
// first delta without inventing an event type the protocol does not define.
func writeSSEComment(w http.ResponseWriter, flusher http.Flusher, text string) error {
	if _, err := fmt.Fprintf(w, ": %s\n\n", text); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// sseKeepaliveWriter keeps an established SSE stream from going silent: when
// nothing has left the proxy for streamKeepaliveInterval it emits a heartbeat
// frame. The default is a comment line, which the SSE spec defines as
// carrying no event — enough for byte-level idle timeouts. Clients whose
// idle timer only counts parsed events (Codex CLI's eventsource layer
// discards comments) need setEventFrame: a real protocol event, invisible to
// the protocol's own accumulators, that still resets an event-level timer.
// All three protocol streams share this one writer instead of each carrying
// its own timer. Frames and heartbeats hold the same mutex, so a beat can
// only ever land between frames, never inside one.
type sseKeepaliveWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
	last    time.Time
	once    sync.Once
	stop    chan struct{}
	done    chan struct{}
	// event, when set, replaces the comment beat with a protocol event frame.
	// Called with mu held; ok=false skips this beat without pushing last
	// forward, so the next tick retries as soon as the gate opens.
	event func() (frame string, ok bool)
}

// newSSEKeepaliveWriter starts the heartbeat for a stream whose headers are
// already on the wire. The returned writer must take over as the handler's
// ResponseWriter and Flusher for the rest of the response, and Close must be
// called when the stream ends. A non-positive interval disables the heartbeat;
// the writer then only serializes writes.
func newSSEKeepaliveWriter(w http.ResponseWriter, flusher http.Flusher) *sseKeepaliveWriter {
	k := &sseKeepaliveWriter{
		w:       w,
		flusher: flusher,
		last:    time.Now(),
		stop:    make(chan struct{}),
		done:    make(chan struct{}),
	}
	if interval := streamKeepaliveInterval; interval > 0 {
		go k.run(interval)
	} else {
		close(k.done)
	}
	return k
}

func (k *sseKeepaliveWriter) run(interval time.Duration) {
	defer close(k.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-ticker.C:
			// A failed write means the client is gone; stop beating.
			if k.emit(interval) != nil {
				return
			}
		}
	}
}

// setEventFrame upgrades the heartbeat from a comment line to a real protocol
// event. Safe to call right after construction, while the stream is young.
func (k *sseKeepaliveWriter) setEventFrame(event func() (string, bool)) {
	k.mu.Lock()
	k.event = event
	k.mu.Unlock()
}

// emit writes the heartbeat frame once the stream has been silent for the
// whole interval. The mutex it shares with Write is what keeps the beat from
// ever interleaving with a frame. A gated event beat (ok=false) writes
// nothing and leaves last untouched, so it retries on the next tick.
func (k *sseKeepaliveWriter) emit(interval time.Duration) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if time.Since(k.last) < interval {
		return nil
	}
	if k.event != nil {
		frame, ok := k.event()
		if !ok {
			return nil
		}
		if _, err := io.WriteString(k.w, frame); err != nil {
			return err
		}
		k.flusher.Flush()
	} else if err := writeSSEComment(k.w, k.flusher, "keep-alive"); err != nil {
		return err
	}
	k.last = time.Now()
	return nil
}

// writeFrame runs fn and writes the frame it returns while the stream is held
// exclusively, then stamps it as activity. The Responses emitter installs this
// as its write path: a frame's sequence_number is then allocated under the same
// lock that orders the bytes, so a keep-alive beat can no longer take the next
// number and overtake a real event that was mid-write. Lock order is k.mu then
// e.mu, the same order emit already takes through heartbeatFrame.
func (k *sseKeepaliveWriter) writeFrame(fn func() (string, error)) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	frame, err := fn()
	if err != nil {
		return err
	}
	if _, err := io.WriteString(k.w, frame); err != nil {
		return err
	}
	k.flusher.Flush()
	k.last = time.Now()
	return nil
}

// anthropicPingFrame is the official Anthropic stream ping, byte for byte:
// the API itself emits these to hold a stream open, the official SDK
// accumulators ignore them, and repeats are harmless — exactly the shape a
// heartbeat needs. Comment lines were not enough on this protocol: Codex
// CLI's idle timer only counts parsed events and discards comments.
func anthropicPingFrame() (string, bool) {
	return "event: ping\ndata: {\"type\":\"ping\"}\n\n", true
}

func (k *sseKeepaliveWriter) Header() http.Header { return k.w.Header() }

func (k *sseKeepaliveWriter) WriteHeader(code int) { k.w.WriteHeader(code) }

func (k *sseKeepaliveWriter) Write(p []byte) (int, error) {
	k.mu.Lock()
	n, err := k.w.Write(p)
	if err == nil {
		k.flusher.Flush()
		k.last = time.Now()
	}
	k.mu.Unlock()
	return n, err
}

func (k *sseKeepaliveWriter) Flush() {
	k.mu.Lock()
	k.flusher.Flush()
	k.mu.Unlock()
}

// Close stops the heartbeat and waits for it to leave, so no comment line can
// land after the handler has returned. Safe to call more than once.
func (k *sseKeepaliveWriter) Close() {
	k.once.Do(func() { close(k.stop) })
	<-k.done
}

func writeOpenAIChunk(w http.ResponseWriter, flusher http.Flusher, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

// recordedTailBytes is how much of the response tail the recorder keeps on top
// of the truncated head, so a terminal SSE error frame -- which always lands at
// the very end of a stream -- is still recognisable after the head was capped.
const recordedTailBytes = 8 << 10

// sseTerminalError reports whether a recorded response carried a terminal error
// event. A stream that has already committed its 200 and the text/event-stream
// headers cannot change its status line afterwards, so the failure reached the
// access log and every status-code-driven retry policy as a success. Only the
// recorded status is rewritten; the client sees exactly the same bytes.
//
// The named `error` event is the Anthropic and Responses terminal frame. The
// chat stream has no named event for it, so there a chunk whose only payload is
// a top-level error object is the same thing; a successful chunk has choices,
// never error.
func sseTerminalError(contentType string, tail []byte) bool {
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(contentType)), "text/event-stream") {
		return false
	}
	for _, block := range strings.Split(string(tail), "\n\n") {
		var data string
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				if strings.TrimSpace(strings.TrimPrefix(line, "event: ")) == "error" {
					return true
				}
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			}
		}
		if data == "" || data == "[DONE]" {
			continue
		}
		// A block cut in half by the tail window is not JSON; skipping it can
		// only miss a detection, never invent one.
		var chunk struct {
			Error *struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil && chunk.Error.Type != "" {
			return true
		}
	}
	return false
}

type recordingResponseWriter struct {
	http.ResponseWriter
	statusCode int
	body       []byte
	tail       []byte
	wrote      bool
	truncated  bool
	total      int
}

func (rw *recordingResponseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.wrote = true
	rw.ResponseWriter.WriteHeader(code)
}

func (rw *recordingResponseWriter) Write(b []byte) (int, error) {
	if !rw.wrote {
		rw.WriteHeader(http.StatusOK)
	}
	rw.total += len(b)
	// Stop retaining past the recording cap: an SSE answer used to be buffered in
	// full here, so the 8 KiB limit only ever applied after the memory was spent.
	// The client-facing stream is untouched.
	if !rw.truncated && len(rw.body) <= recordedBodyLimit {
		rw.body = append(rw.body, b...)
		rw.truncated = len(rw.body) > recordedBodyLimit
	}
	// A terminal error frame lives at the end of a stream, which is exactly what
	// the truncated head above throws away.
	rw.tail = append(rw.tail, b...)
	if len(rw.tail) > recordedTailBytes {
		rw.tail = append(rw.tail[:0], rw.tail[len(rw.tail)-recordedTailBytes:]...)
	}
	return rw.ResponseWriter.Write(b)
}

func (rw *recordingResponseWriter) Flush() {
	if flusher, ok := rw.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// slotContextKey marks a request whose slot the recorder already decided, so
// the generation handlers do not queue for it a second time.
type slotContextKey struct{}

// enterSlot returns the slot decision withRecorder already made for this
// request, if any. The recorder holds that slot until the handler returns, so
// the handler must neither take a second one nor release it.
func enterSlot(r *http.Request) (slotOutcome, bool) {
	outcome, ok := r.Context().Value(slotContextKey{}).(slotOutcome)
	return outcome, ok
}

// acquireRequestSlot resolves the execution slot for a generation request. On
// the routes withRecorder pre-acquires, the decision is already in the context
// and that slot is held for the whole request; everywhere else the handler
// takes and releases its own, exactly as before. The returned release is always
// safe to defer.
func (s *Server) acquireRequestSlot(r *http.Request) (slotOutcome, func()) {
	if outcome, ok := enterSlot(r); ok {
		return outcome, func() {}
	}
	outcome := s.acquire(r.Context())
	if outcome == slotAcquired {
		return outcome, s.release
	}
	return outcome, func() {}
}

// generationPath reports whether a POST route runs a model turn. Those are the
// only routes that take an execution slot, and the only ones whose request body
// can run to the 32 MiB image ceiling, so they are the only ones whose body the
// recorder reads under a held slot. count_tokens is a local estimate and never
// took a slot, so pre-acquiring one there would only add queueing to a cheap
// endpoint.
func generationPath(r *http.Request) bool {
	if r.Method != http.MethodPost {
		return false
	}
	switch r.URL.Path {
	case "/v1/messages", "/v1/chat/completions", "/api/v1/chat/completions",
		"/v1/responses", "/api/v1/responses":
		return true
	default:
		return false
	}
}

func (s *Server) withRecorder(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isDebugInspectionPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()

		// Cap the body once, here, before any handler can read it: an uncapped
		// ReadAll let a handful of huge POSTs OOM the proxy ahead of the
		// concurrency semaphore. Reading past the limit surfaces to handlers as
		// the existing decodeJSON 400 exit.
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)

		// Read request body for recording, then restore for downstream handler.
		// The read happens with an execution slot already held, so the worst
		// case resident body is maxRequestBytes times the concurrency and not
		// times every connection the listener has accepted; that bound was the
		// documented upgrade path for the ceiling above. A request that cannot
		// take a slot is left unbuffered -- a generation route is about to be
		// refused with 429, and pulling 32 MiB in to throw it away was the leak.
		var reqBody string
		slotHeld := false
		canRead := r.Body != nil && r.Body != http.NoBody && r.Method == http.MethodPost
		if canRead && generationPath(r) {
			// A generation route holds its slot for the whole request: the
			// handler must not queue a second time for a slot it already has,
			// and the body must not stay resident after the handler lets go.
			outcome := s.acquire(r.Context())
			r = r.WithContext(context.WithValue(r.Context(), slotContextKey{}, outcome))
			slotHeld = outcome == slotAcquired
			canRead = slotHeld
		} else if canRead {
			// A body that is only read to record it. Take a slot if one is
			// free; never queue a cheap local endpoint (count_tokens) behind
			// model turns for a debug record.
			select {
			case s.sem <- struct{}{}:
				defer s.release()
			default:
				canRead = false
			}
		}
		if canRead {
			body, readErr := io.ReadAll(r.Body)
			if readErr != nil {
				// Hand the capped reader's error to the decoder too, so an
				// oversized body answers "too large" instead of a misleading
				// JSON-syntax error about the partial bytes that arrived.
				r.Body = errorReadCloser{err: readErr}
			} else {
				r.Body = io.NopCloser(bytes.NewReader(body))
			}
			reqBody = sanitizeRecordedBody(body)
		}

		rw := &recordingResponseWriter{ResponseWriter: w, statusCode: 200}
		next.ServeHTTP(rw, r)
		if slotHeld {
			s.release()
		}
		duration := time.Since(start)

		// The client hung up while a subprocess was still working. Answering that
		// as a 500 made a plain disconnect read as an upstream failure in every
		// log and dashboard; 499 is the "client closed request" code nginx uses.
		if rw.statusCode >= 500 && r.Context().Err() != nil {
			rw.statusCode = 499
		}
		// A stream that failed after committing its 200 cannot say so in a
		// status line, so the failure is invisible to the access log and to
		// every client that retries on status codes. Reclassify the record; the
		// bytes the client received are untouched.
		if rw.statusCode == http.StatusOK && sseTerminalError(rw.Header().Get("Content-Type"), rw.tail) {
			rw.statusCode = http.StatusBadGateway
		}

		respBody := sanitizeRecordedBody(rw.body)
		if rw.truncated {
			// The retained prefix stopped growing at the cap, so restate the
			// true total rather than the count TruncateRecordedString saw.
			if i := strings.Index(respBody, "…[truncated,"); i >= 0 {
				respBody = respBody[:i]
			}
			respBody += fmt.Sprintf("…[truncated, %d bytes total]", rw.total)
		}

		s.recordRequest(r.Method, r.URL.Path, rw.statusCode, duration, reqBody, respBody)
		if s.OnRequest != nil {
			callback, method, path, status := s.OnRequest, r.Method, r.URL.Path, rw.statusCode
			go func() {
				// This goroutine belongs to the caller, not to the handler: the
				// desktop console's callback runs here, and an unrecovered panic
				// in a goroutine takes the whole proxy process down with it.
				defer func() {
					if rec := recover(); rec != nil {
						s.logf("OnRequest callback for %s %s panicked: %v\n%s", method, path, rec, debug.Stack())
					}
				}()
				callback(method, path, status, duration, reqBody, respBody)
			}()
		}
	})
}

func isDebugInspectionPath(path string) bool {
	switch path {
	case "/debug/requests", "/debug/logs", "/api/requests", "/api/logs", "/debug/access-logs", "/api/access-logs", "/debug/app-logs", "/api/app-logs":
		return true
	default:
		return false
	}
}

func (s *Server) recordRequest(method, path string, statusCode int, duration time.Duration, reqBody, respBody string) {
	s.recMu.Lock()
	defer s.recMu.Unlock()

	s.records = append(s.records, debugRequestRecord{
		Time:       time.Now().Format(time.RFC3339),
		Method:     method,
		Path:       path,
		StatusCode: statusCode,
		DurationMS: duration.Milliseconds(),
		Request:    reqBody,
		Response:   respBody,
	})
	if len(s.records) > 200 {
		s.records = s.records[len(s.records)-200:]
	}
	s.logs = append(s.logs, debugLogRecord{
		Time:    time.Now().Format(time.RFC3339),
		Level:   debugLogLevel(statusCode),
		Message: fmt.Sprintf("%s %s -> %d (%dms)", method, path, statusCode, duration.Milliseconds()),
	})
	if len(s.logs) > 200 {
		s.logs = s.logs[len(s.logs)-200:]
	}
}

func (s *Server) debugRecords(limit int) []debugRequestRecord {
	s.recMu.RLock()
	defer s.recMu.RUnlock()

	if limit > len(s.records) {
		limit = len(s.records)
	}
	out := make([]debugRequestRecord, 0, limit)
	for i := len(s.records) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.records[i])
	}
	return out
}

// logf appends one line to the in-process log the debug console reads. It is
// also what a recovered panic in a caller-supplied callback lands in: the
// console is the only place this process reports anything to a human.
func (s *Server) logf(level, format string, args ...any) {
	s.recMu.Lock()
	defer s.recMu.Unlock()
	s.logs = append(s.logs, debugLogRecord{
		Time:    time.Now().Format(time.RFC3339),
		Level:   level,
		Message: fmt.Sprintf(format, args...),
	})
	if len(s.logs) > 200 {
		s.logs = s.logs[len(s.logs)-200:]
	}
}

func (s *Server) debugLogs(limit int) []debugLogRecord {
	s.recMu.RLock()
	defer s.recMu.RUnlock()

	if limit > len(s.logs) {
		limit = len(s.logs)
	}
	out := make([]debugLogRecord, 0, limit)
	for i := len(s.logs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, s.logs[i])
	}
	return out
}

func debugLogLevel(statusCode int) string {
	switch {
	case statusCode >= 500:
		return "error"
	case statusCode >= 400:
		return "warn"
	default:
		return "info"
	}
}

func sanitizeRecordedBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	// Redacting the tree costs an unmarshal, a deep copy and a re-marshal of a body
	// that can be tens of megabytes -- and the result is thrown down to
	// recordedBodyLimit right after. Past this ceiling cut first: the parse then
	// fails and the existing fallback shows the raw prefix, a bounded preview in an
	// operator-only view. Widening maxRequestBytes for inline images made the
	// uncapped cost proportionally worse, so the ceiling is what keeps the request
	// path O(recorded) instead of O(body).
	if len(body) > recordedBodySanitizeCeiling {
		body = body[:recordedBodySanitizeCeiling]
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		return TruncateRecordedString(string(body))
	}
	return TruncateRecordedString(string(mustMarshalJSON(redactRecordedValue(value))))
}

func redactRecordedValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for k, v := range typed {
			lower := strings.ToLower(k)
			if lower == "data" || lower == "url" {
				if s := stringFromAny(v); looksLikeImagePayload(s) {
					out[k] = imageRedaction(s)
					continue
				}
			}
			out[k] = redactRecordedValue(v)
		}
		return out
	case []any:
		out := make([]any, 0, len(typed))
		for _, item := range typed {
			out = append(out, redactRecordedValue(item))
		}
		return out
	case string:
		if looksLikeImagePayload(typed) {
			return imageRedaction(typed)
		}
		return typed
	default:
		return typed
	}
}

func looksLikeImagePayload(value string) bool {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "data:image/") {
		return true
	}
	if len(value) > 4096 && isLikelyBase64(value) {
		return true
	}
	return false
}

func imageRedaction(value string) string {
	return fmt.Sprintf("[image payload redacted, %d chars]", len(value))
}

func isLikelyBase64(value string) bool {
	for _, r := range value {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=' || r == '\n' || r == '\r' {
			continue
		}
		return false
	}
	return true
}

func mustMarshalJSON(value any) []byte {
	body, err := json.Marshal(value)
	if err != nil {
		return []byte("{}")
	}
	return body
}

// recordedBodyLimit bounds what the debug recorder keeps per request. The record
// list holds the last 200 requests, so an uncapped body turns a few long SSE
// answers into hundreds of megabytes that never come back.
const recordedBodyLimit = 8 << 10

// recordedBodySanitizeCeiling bounds how much of a request body is ever parsed
// for the debug record. See sanitizeRecordedBody.
const recordedBodySanitizeCeiling = 64 << 10

// maxRequestBytes is the request-body ceiling enforced once in withRecorder.
// It has to stay above what this proxy's own image policy admits: images travel
// as `data:` URLs (base64 grows a 20 MiB image to ~27 MB of JSON), so a cap
// sized for a plain chat request silently 400s every screenshot a client sends.
// The desktop console keeps its own, much smaller, 1 MiB limit for admin routes.
// ponytail: worst case resident body is this cap times the request concurrency,
// because withRecorder buffers before the semaphore is taken. Upgrade path: read
// the body inside the acquired slot instead of widening the ceiling.
const maxRequestBytes = 32 << 20

// TruncateRecordedString caps one recorded body at recordedBodyLimit, on a rune
// boundary. Exported because the desktop re-persists what it loaded: a fat
// record saved before this bound existed would otherwise be rewritten forever.
func TruncateRecordedString(value string) string {
	if len(value) <= recordedBodyLimit {
		return value
	}
	cut := recordedBodyLimit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + fmt.Sprintf("…[truncated, %d bytes total]", len(value))
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, x-api-key, anthropic-version")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func maxConcurrentRequests() int {
	raw := strings.TrimSpace(os.Getenv("LINGMA_PROXY_MAX_CONCURRENT"))
	if raw == "" {
		return 4
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 4
	}
	if n > 16 {
		return 16
	}
	return n
}

// slotQueueWait bounds how long a request may sit behind the concurrency gate.
// A queued request writes no bytes, and clients cannot tell that from a dead
// connection: the Qoder SDK abandons a response with no first payload after 60s
// and kills the turn, which is how a fan-out of agents loses workers. Refusing
// inside that window hands the client a retryable answer instead.
// Var so the httpapi test can shrink it and still cover the refusal path.
//
// CONCURRENCY: plain package-level var, rewritten by the httpapi tests while
// the package's other tests run. t.Parallel() anywhere in this package is
// immediately a data race on it. Same warning as streamKeepaliveInterval.
var slotQueueWait = 45 * time.Second

type slotOutcome int

const (
	slotAcquired slotOutcome = iota
	slotQueueFull
	slotClientGone
)

func (s *Server) acquire(ctx context.Context) slotOutcome {
	select {
	case s.sem <- struct{}{}:
		return slotAcquired
	case <-time.After(slotQueueWait):
	case <-ctx.Done():
		return slotClientGone
	}
	// The wait is up: take a slot only if one opened at this instant, otherwise
	// blocking here would put the silent queue back.
	select {
	case s.sem <- struct{}{}:
		return slotAcquired
	case <-ctx.Done():
		return slotClientGone
	default:
		return slotQueueFull
	}
}

func (s *Server) queueFullMessage() string {
	return fmt.Sprintf("proxy is serving its maximum of %d concurrent requests; retry shortly", cap(s.sem))
}

func (s *Server) release() {
	select {
	case <-s.sem:
	default:
	}
}

// historicalToolCallID is the id a replayed assistant tool call must carry. An
// empty id cannot be paired with the tool_result that follows it in the same
// conversation, and it goes back out on the wire as id:"", which is not a
// legal value on any of the three protocols. The Responses entry point already
// dropped such calls; the Chat and Anthropic ones kept them, so the same history
// was served or lost depending on which protocol the client spoke.
func historicalToolCallID(id string) (string, bool) {
	id = strings.TrimSpace(id)
	return id, id != ""
}

func extractOpenAIToolCalls(raw []any) []toolemulation.ToolCall {
	if len(raw) == 0 {
		return nil
	}
	out := make([]toolemulation.ToolCall, 0, len(raw))
	for _, item := range raw {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		id, ok := historicalToolCallID(stringFromAny(m["id"]))
		if !ok {
			continue
		}
		fn, ok := m["function"].(map[string]any)
		if !ok {
			continue
		}
		name := stringFromAny(fn["name"])
		if name == "" {
			continue
		}
		argsRaw := stringFromAny(fn["arguments"])
		var args map[string]any
		if argsRaw != "" {
			_ = json.Unmarshal([]byte(argsRaw), &args)
		}
		out = append(out, toolemulation.ToolCall{
			ID:        id,
			Name:      name,
			Arguments: args,
		})
	}
	return out
}

type anthropicToolResult struct {
	ToolUseID string
	Content   string
}

func extractAnthropicUserContent(content any) (string, []anthropicToolResult) {
	items, ok := content.([]any)
	if !ok {
		return extractText(content), nil
	}
	var results []anthropicToolResult
	var textParts []string
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch stringFromAny(m["type"]) {
		case "text":
			if t := stringFromAny(m["text"]); t != "" {
				textParts = append(textParts, t)
			}
		case "thinking", "redacted_thinking":
			// Skip thinking blocks in user messages
			continue
		case "tool_result":
			toolUseID := stringFromAny(m["tool_use_id"])
			content, present := m["content"]
			// Only a structurally valid content is a result; an absent key or
			// a malformed shape is dropped, never fabricated into an empty
			// result.
			if !present || !validToolResultContent(content) {
				continue
			}
			results = append(results, anthropicToolResult{
				ToolUseID: toolUseID,
				Content:   extractText(content),
			})
		}
	}
	text := ""
	if len(textParts) > 0 {
		text = strings.Join(textParts, "\n")
	}
	return text, results
}

func extractAnthropicAssistantContent(content any) (string, []toolemulation.ToolCall) {
	items, ok := content.([]any)
	if !ok {
		return extractText(content), nil
	}
	calls := make([]toolemulation.ToolCall, 0, len(items))
	var textParts []string
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		switch stringFromAny(m["type"]) {
		case "text":
			if t := stringFromAny(m["text"]); t != "" {
				textParts = append(textParts, t)
			}
		case "thinking", "redacted_thinking":
			// Skip thinking blocks — they are not part of the conversation text
			continue
		case "tool_use":
			id, ok := historicalToolCallID(stringFromAny(m["id"]))
			if !ok {
				continue
			}
			name := stringFromAny(m["name"])
			if name == "" {
				continue
			}
			var args map[string]any
			if rawInput, ok := m["input"].(map[string]any); ok {
				args = rawInput
			} else if inputStr, ok := m["input"].(string); ok && inputStr != "" {
				if err := json.Unmarshal([]byte(inputStr), &args); err != nil {
					args = map[string]any{}
				}
			}
			calls = append(calls, toolemulation.ToolCall{
				ID:        id,
				Name:      name,
				Arguments: args,
			})
		}
	}
	text := ""
	if len(textParts) > 0 {
		text = strings.Join(textParts, "\n")
	}
	return text, calls
}

func extractOpenAIImages(ctx context.Context, content any) []service.Image {
	items, ok := content.([]any)
	if !ok {
		return nil
	}
	var images []service.Image
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if stringFromAny(m["type"]) != "image_url" {
			continue
		}
		imageURL, ok := m["image_url"].(map[string]any)
		if !ok {
			continue
		}
		rawURL := stringFromAny(imageURL["url"])
		if rawURL == "" {
			continue
		}
		img := parseImageURL(ctx, rawURL)
		if img != nil {
			images = append(images, *img)
		}
	}
	return images
}

func extractAnthropicImages(content any) []service.Image {
	items, ok := content.([]any)
	if !ok {
		return nil
	}
	var images []service.Image
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if stringFromAny(m["type"]) != "image" {
			continue
		}
		source, ok := m["source"].(map[string]any)
		if !ok {
			continue
		}
		if stringFromAny(source["type"]) != "base64" {
			continue
		}
		mediaType := stringFromAny(source["media_type"])
		data := stringFromAny(source["data"])
		if data == "" {
			continue
		}
		images = append(images, service.Image{
			MediaType: mediaType,
			Data:      data,
		})
	}
	return images
}

func parseImageURL(ctx context.Context, raw string) *service.Image {
	if strings.HasPrefix(raw, "data:") {
		return normalizeImage(parseDataURL(raw))
	}
	if img := parseLocalImagePath(raw); img != nil {
		return normalizeImage(img)
	}
	img, err := fetchImageAsBase64(ctx, raw)
	if err != nil {
		return nil
	}
	return normalizeImage(img)
}

func parseLocalImagePath(raw string) *service.Image {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	path := raw
	if strings.HasPrefix(raw, "file://") {
		if strings.Contains(raw, `\`) {
			path = strings.TrimPrefix(raw, "file://")
		} else {
			u, err := url.Parse(raw)
			if err != nil {
				return nil
			}
			path = u.Path
			if host := strings.TrimSpace(u.Host); host != "" {
				path = "//" + host + path
			}
		}
	}
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}
		path = home + strings.TrimPrefix(path, "~")
	}
	path = filepath.FromSlash(path)
	if len(path) >= 3 && path[0] == filepath.Separator && path[2] == ':' {
		path = path[1:]
	}
	if !filepath.IsAbs(path) {
		return nil
	}
	// Attaching a local screenshot is the feature; attaching an arbitrary file is
	// not. Without this gate any client -- or a prompt-injected agent -- could send
	// image_url "/etc/passwd" and have its contents base64'd and labelled
	// image/jpeg straight into the model prompt.
	mediaType := mediaTypeForImagePath(path)
	if mediaType == "" {
		return nil
	}
	// Regular-file only also rejects directories and device nodes, so `/dev/zero`
	// cannot stream an unbounded read into memory, and the size gate matches the
	// cap the remote fetch already enforces.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxImageFetchBytes {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return nil
	}
	return &service.Image{
		MediaType: mediaType,
		Data:      base64.StdEncoding.EncodeToString(data),
		URL:       raw,
	}
}

// mediaTypeForImagePath maps the extensions this proxy is willing to read off
// disk, and returns "" for anything else so the caller rejects it rather than
// defaulting an arbitrary file to image/jpeg.
func mediaTypeForImagePath(path string) string {
	lower := strings.ToLower(path)
	switch {
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".bmp"):
		return "image/bmp"
	default:
		return ""
	}
}

func parseDataURL(url string) *service.Image {
	const prefix = "data:"
	if !strings.HasPrefix(url, prefix) {
		return nil
	}
	rest := url[len(prefix):]
	commaIdx := strings.Index(rest, ",")
	if commaIdx < 0 {
		return nil
	}
	meta := rest[:commaIdx]
	data := rest[commaIdx+1:]

	mediaType := ""
	if strings.HasSuffix(meta, ";base64") {
		mediaType = strings.TrimSuffix(meta, ";base64")
	} else {
		mediaType = meta
	}

	return &service.Image{
		MediaType: mediaType,
		Data:      data,
	}
}

// maxImageFetchBytes bounds one remote image download. The old code had no cap
// and no client timeout, so a slow-drip endpoint could hold proxy slots open
// indefinitely.
const maxImageFetchBytes = 20 << 20

// imageFetchClient bounds remote image fetches. CheckRedirect re-applies the
// guard so a redirect cannot walk an allowed public URL into the metadata range,
// and Control applies it to the address that is actually dialled, after DNS --
// otherwise a public name that resolves to 169.254.169.254 gets through.
var imageFetchClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return imageURLAllowed(req.URL)
	},
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if err := imageAddressAllowed(address); err != nil {
				return nil, err
			}
			var d net.Dialer
			return d.DialContext(ctx, network, address)
		},
	},
}

// imageHostBlocked is the address policy for image fetches. Link-local is where
// the cloud metadata services live; multicast and the unspecified address have
// no business being an image origin. Loopback and private ranges stay allowed on
// purpose: self-hosted image servers on the LAN and on this box are a real use,
// and local file paths have their own reader above.
func imageHostBlocked(ip net.IP) bool {
	return ip == nil || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified()
}

// imageAddressAllowed checks the resolved host:port the transport is about to
// connect to. By dial time the host is always a literal IP.
func imageAddressAllowed(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	if ip := net.ParseIP(host); ip == nil || imageHostBlocked(ip) {
		return fmt.Errorf("image URL host %q not allowed (link-local/metadata)", host)
	}
	return nil
}

func imageURLAllowed(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("image URL scheme %q not allowed", u.Scheme)
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil && imageHostBlocked(ip) {
		return fmt.Errorf("image URL host %q not allowed (link-local/metadata)", u.Hostname())
	}
	return nil
}

func fetchImageAsBase64(ctx context.Context, rawURL string) (*service.Image, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, err
	}
	if err := imageURLAllowed(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := imageFetchClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch image failed: %s", resp.Status)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxImageFetchBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxImageFetchBytes {
		return nil, fmt.Errorf("fetch image failed: response over %d bytes", maxImageFetchBytes)
	}

	mediaType := resp.Header.Get("Content-Type")
	if idx := strings.Index(mediaType, ";"); idx >= 0 {
		// Strip parameters like "image/png; charset=utf-8"
		mediaType = strings.TrimSpace(mediaType[:idx])
	}
	// The Origin gate stops a browser page from naming a target; this stops
	// every other caller from turning the proxy into "base64 whatever that URL
	// returns". A service that answers with JSON, HTML or plain text is not an
	// image, and its bytes must never reach the model prompt. Go's own HTTP
	// server sniffs a missing Content-Type off the first bytes, so a real image
	// host still presents image/* here.
	if !isImageMediaType(mediaType) {
		return nil, fmt.Errorf("fetch image failed: %q is not an image content type", mediaType)
	}

	return &service.Image{
		MediaType: mediaType,
		Data:      base64.StdEncoding.EncodeToString(data),
	}, nil
}

// isImageMediaType is the content-type half of the image read policy.
func isImageMediaType(mediaType string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(mediaType)), "image/")
}

func normalizeImage(img *service.Image) *service.Image {
	if img == nil || strings.TrimSpace(img.Data) == "" {
		return img
	}
	data, err := base64.StdEncoding.DecodeString(img.Data)
	if err != nil || len(data) == 0 {
		return img
	}
	const maxImageBytes = 2 * 1024 * 1024
	const maxImageSide = 1568
	if len(data) <= maxImageBytes {
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			if cfg.Width <= maxImageSide && cfg.Height <= maxImageSide {
				return img
			}
		}
	}

	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return img
	}
	bounds := decoded.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()
	if width <= 0 || height <= 0 {
		return img
	}
	targetWidth, targetHeight := scaledDimensions(width, height, maxImageSide)
	dst := resizeNearest(decoded, targetWidth, targetHeight)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
		return img
	}
	img.MediaType = "image/jpeg"
	img.Data = base64.StdEncoding.EncodeToString(buf.Bytes())
	return img
}

func resizeNearest(src image.Image, width int, height int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	bounds := src.Bounds()
	srcWidth := bounds.Dx()
	srcHeight := bounds.Dy()
	for y := 0; y < height; y++ {
		sy := bounds.Min.Y + y*srcHeight/height
		for x := 0; x < width; x++ {
			sx := bounds.Min.X + x*srcWidth/width
			dst.Set(x, y, src.At(sx, sy))
		}
	}
	return dst
}

func scaledDimensions(width int, height int, maxSide int) (int, int) {
	if width <= maxSide && height <= maxSide {
		return width, height
	}
	if width >= height {
		scaledHeight := height * maxSide / width
		if scaledHeight < 1 {
			scaledHeight = 1
		}
		return maxSide, scaledHeight
	}
	scaledWidth := width * maxSide / height
	if scaledWidth < 1 {
		scaledWidth = 1
	}
	return scaledWidth, maxSide
}
