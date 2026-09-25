package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lingma-ipc-proxy/internal/lingmaipc"
	"lingma-ipc-proxy/internal/qodercli"
	"lingma-ipc-proxy/internal/remote"
	"lingma-ipc-proxy/internal/toolemulation"
)

type BackendMode string

const (
	BackendIPC BackendMode = "ipc"
	// BackendRemote speaks the legacy signed Lingma/QoderCN gateway protocol.
	BackendRemote BackendMode = "remote"
	// BackendQoderCLI drives the Qoder CN desktop app's bundled CLI, which is
	// the only client able to sign gateway requests since the protocol change.
	BackendQoderCLI BackendMode = "qodercli"
)

type SessionMode string

const (
	SessionModeAuto  SessionMode = "auto"
	SessionModeFresh SessionMode = "fresh"
	SessionModeReuse SessionMode = "reuse"
)

const ipcSetupTimeout = 15 * time.Second

// Config is read by every request path. Backend and Model are the only fields a
// Service may change after construction -- and only under its mutex -- because
// callers hold a copy. Keep every other field immutable once New() returns.
type Config struct {
	Host                  string
	Port                  int
	Backend               BackendMode
	Transport             lingmaipc.Transport
	Pipe                  string
	WebSocketURL          string
	RemoteBaseURL         string
	RemoteAuthFile        string
	RemoteProxyURL        string
	RemoteVersion         string
	Cwd                   string
	CurrentFilePath       string
	Mode                  string
	Model                 string
	ShellType             string
	SessionMode           SessionMode
	Timeout               time.Duration
	WarmupTimeout         time.Duration
	RemoteFallbackEnabled bool
	RemoteFallbackModels  []string
	// QoderCLISites pins which Qoder deployments the CLI backend serves
	// ("cn", "global"). Empty defers to LINGMA_QODERCLI_SITES, then to both.
	QoderCLISites []string
}

type Image struct {
	MediaType string // e.g. "image/jpeg", "image/png"
	Data      string // base64 encoded data without prefix
	URL       string // optional original URL
}

type ChatMessage struct {
	Role       string
	Text       string
	Images     []Image
	ToolCallID string
	ToolCalls  []toolemulation.ToolCall
}

type ChatRequest struct {
	Model             string
	System            string
	Messages          []ChatMessage
	Tools             []toolemulation.ToolDef
	ToolChoice        toolemulation.ToolChoice
	ParallelToolCalls *bool

	// Generation parameters (passed through for API compatibility;
	// actual effect depends on Lingma backend support)
	Temperature      *float64
	TopP             *float64
	TopK             int
	Stop             []string
	PresencePenalty  float64
	FrequencyPenalty float64
	MaxTokens        int
	Seed             int
	User             string
	ReasoningEffort  string
	ResponseFormat   string // "json" or "json_schema"
}

type ChatResult struct {
	Text             string
	ThoughtText      string
	Model            string
	InputTokens      int
	OutputTokens     int
	SessionID        string
	RequestID        string
	FinishReason     string
	StopReason       string
	UsedTokens       int
	LimitTokens      int
	ThinkingDuration int64
	PipePath         string
	Endpoint         string
	Transport        string
	EffectiveSession SessionMode
	ToolCalls        []toolemulation.ToolCall
}

type StreamEvent struct {
	Type  string
	Delta string
}

type StreamResult struct {
	Result *ChatResult
	Err    error
}

type Model struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Scene      string `json:"scene,omitempty"`
	InternalID string `json:"-"`
}

type State struct {
	PipePath        string      `json:"pipe_path,omitempty"`
	Endpoint        string      `json:"endpoint,omitempty"`
	Transport       string      `json:"transport,omitempty"`
	Connected       bool        `json:"connected"`
	StickySessionID string      `json:"sticky_session_id,omitempty"`
	SessionMode     SessionMode `json:"session_mode"`
}

type Service struct {
	cfg              Config
	backendOnce      sync.Once
	mu               sync.Mutex
	connectMu        sync.Mutex // serialises the IPC handshake only; see ensureConnected
	client           *lingmaipc.Client
	pipePath         string
	endpoint         string
	transport        lingmaipc.Transport
	stickySessionID  string
	stickyModelID    string
	modelMap         map[string]string // official name -> internal id
	remoteClient     *remote.Client
	cliClients       map[qodercli.Site]*qodercli.Client
	cliModels        map[qodercli.Site][]string
	cliCatalog       map[qodercli.Site]cliCatalogEntry
	detectedCLISites []qodercli.Site
	cliSitesOnce     sync.Once
	remoteProbeCache map[string]remoteModelProbeEntry
	// imageScheme memoises the filesystem probe behind the IPC image URI scheme: the
	// install layout does not move while the proxy runs, and the probe sat on the
	// request path for every attachment.
	imageSchemeOnce  sync.Once
	imageSchemeValue string
}

type promptRunResult struct {
	PromptResult     map[string]any
	FinishData       map[string]any
	ContextUsage     map[string]any
	AssistantText    string
	ThoughtText      string
	ThinkingDuration int64
	TimedOut         bool
}

const (
	StreamEventText     = "text"
	StreamEventThinking = "thinking"
)

type remoteModelProbeEntry struct {
	Available bool
	ExpiresAt time.Time
}

func New(cfg Config) *Service {
	if strings.TrimSpace(cfg.Cwd) == "" {
		if wd, err := os.Getwd(); err == nil {
			cfg.Cwd = wd
		}
	}
	if strings.TrimSpace(cfg.Mode) == "" {
		cfg.Mode = "agent"
	}
	cfg.Model = strings.TrimSpace(cfg.Model)
	if strings.TrimSpace(cfg.ShellType) == "" {
		cfg.ShellType = lingmaipc.DefaultShellType()
	}
	if cfg.Transport == "" {
		cfg.Transport = lingmaipc.TransportAuto
	}
	if cfg.Backend == "" {
		cfg.Backend = BackendRemote
	}
	if cfg.Backend == BackendRemote && len(cfg.RemoteFallbackModels) == 0 {
		cfg.RemoteFallbackModels = DefaultRemoteFallbackModels()
	}
	// The pinned site set decides which backends are even considered available.
	qodercli.SetEnabledSites(cfg.QoderCLISites)
	cfg.Model = normalizeModelForBackend(cfg.Backend, cfg.Model)
	if cfg.SessionMode == "" {
		cfg.SessionMode = SessionModeAuto
	}
	return &Service{cfg: cfg}
}

// ensureBackend makes the remote/CLI choice on first use rather than in New.
// resolveRemoteBackend opens the login cache and, through qodercli.Available(),
// globs every PATH entry -- about a second on an idle Windows box, and it used to
// run before the listener was bound. A sidecar that pins "backend": "qodercli"
// short-circuits ResolveBackend, so this is insurance for the default remote
// config, not the fix for a box that already pins one. The scan happens outside
// s.mu so it cannot stall another user of the lock; every request path reads the
// backend through backend(), so warm-up triggers it once after the port opens.
func (s *Service) ensureBackend() {
	s.backendOnce.Do(func() {
		s.mu.Lock()
		resolved := s.cfg
		s.mu.Unlock()
		if !ResolveBackend(&resolved) {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.cfg.Backend = resolved.Backend
		s.cfg.Model = resolved.Model
	})
}

// ResolveBackend switches a remote-configured service onto the Qoder CN CLI when
// the legacy gateway login cache is no longer present. It reports whether the
// backend changed.
func ResolveBackend(cfg *Config) bool {
	if cfg.Backend != BackendRemote {
		return false
	}
	backend, switched := resolveRemoteBackend(*cfg)
	if !switched {
		return false
	}
	cfg.Backend = backend
	// The legacy default key only exists on the old gateway protocol; the CLI
	// exposes human-readable names instead, so start from Auto.
	cfg.Model = "Auto"
	return true
}

func DefaultRemoteFallbackModels() []string {
	return []string{
		"kmodel",
		"mmodel",
		"dashscope_qwen3_coder",
		"dashscope_qmodel",
		"dashscope_qwen_max_latest",
		"dashscope_qwen_plus_20250428_thinking",
	}
}

// resolveRemoteBackend keeps the legacy gateway protocol when its login cache is
// present, and otherwise falls through to the Qoder CN CLI, which is the only
// client that can still sign inference requests.
func resolveRemoteBackend(cfg Config) (BackendMode, bool) {
	if strings.TrimSpace(cfg.RemoteAuthFile) != "" {
		return cfg.Backend, false
	}
	if _, err := remote.LoadCredential(""); err == nil {
		return cfg.Backend, false
	}
	if !qodercli.Available() {
		return cfg.Backend, false
	}
	log.Printf("backend: legacy Remote API credentials are unavailable, using the Qoder CN CLI backend instead")
	return BackendQoderCLI, true
}

// cliGlobalPrefix namespaces the international site's models in the merged list.
// Both deployments expose a Qwen3.8-Flash, so an unprefixed id would be ambiguous.
const cliGlobalPrefix = "intl/"

// cliSites returns the sites this machine can serve, resolved once per process.
// The scan globs the install roots and reads login state, so it runs outside mu:
// holding the exclusive lock across it made every other reader -- /health,
// /v1/models, the web console -- wait for the disk.
func (s *Service) cliSites() []qodercli.Site {
	s.cliSitesOnce.Do(func() {
		s.detectedCLISites = qodercli.UsableSites()
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.detectedCLISites) == 0 {
		// Nothing is installed or signed in. Reporting the enabled sites keeps the
		// concrete per-site failure in play instead of an empty list.
		return qodercli.EnabledSites()
	}
	return s.detectedCLISites
}

// cliClientFor returns the client that drives one site's installed CLI.
func (s *Service) cliClientFor(site qodercli.Site) (*qodercli.Client, error) {
	s.mu.Lock()
	if client, ok := s.cliClients[site]; ok {
		s.mu.Unlock()
		return client, nil
	}
	timeout := s.cfg.Timeout
	s.mu.Unlock()

	loc, ok := qodercli.DetectSite(site)
	if !ok || loc.ProfileDir == "" {
		return nil, fmt.Errorf("%s 桌面端未安装或未登录，无法使用它的 CLI 后端", site.Label())
	}
	client := qodercli.NewClient(loc, timeout)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cliClients == nil {
		s.cliClients = map[qodercli.Site]*qodercli.Client{}
	}
	if existing, ok := s.cliClients[site]; ok {
		return existing, nil
	}
	s.cliClients[site] = client
	return client, nil
}

// resolveCLISite keeps a request on the site its model id named. Only an
// unmarked request may move to the machine's other site, and only when the
// default one is absent.
func (s *Service) resolveCLISite(requested qodercli.Site, named bool) (qodercli.Site, error) {
	sites := s.cliSites()
	for _, site := range sites {
		if site.Normalized() == requested.Normalized() {
			return site, nil
		}
	}
	if !named && len(sites) > 0 {
		return sites[0], nil
	}
	return requested, fmt.Errorf("%s 桌面端未安装或未登录，无法使用它的 CLI 后端", requested.Label())
}

// chatClient is the transport surface the shared generate path needs.
type chatClient interface {
	Chat(ctx context.Context, request remote.ChatRequest, onDelta func(string)) (*remote.ChatResult, error)
	ListModels(ctx context.Context) ([]remote.Model, error)
}

func (s *Service) chatClient(site qodercli.Site) (chatClient, error) {
	switch s.backend() {
	case BackendQoderCLI:
		return s.cliClientFor(site)
	case BackendRemote:
		return s.remoteAPI(), nil
	default:
		return nil, fmt.Errorf("backend %q does not support direct chat", s.backend())
	}
}

func (s *Service) SetDefaultModel(model string) {
	s.ensureBackend()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cfg.Model = normalizeModelForBackend(s.cfg.Backend, model)
}

func (s *Service) DefaultModel() string {
	s.ensureBackend()
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(s.cfg.Model)
}

func (s *Service) Warmup(ctx context.Context) error {
	if s.usesRemoteTransport() {
		if s.backend() == BackendQoderCLI {
			return s.warmCLISites(ctx)
		}
		return s.remoteAPI().Warmup(ctx)
	}
	_, err := s.ensureConnected(ctx)
	return err
}

// warmCLISites mints a job token for each served site and caches its model list.
// A site that cannot authenticate is only fatal when no site can: a stale CN
// login must not stop a working international one, and vice versa.
func (s *Service) warmCLISites(ctx context.Context) error {
	var firstErr error
	warmed := false
	for _, site := range s.cliSites() {
		client, err := s.cliClientFor(site)
		if err == nil {
			err = client.Warmup(ctx)
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			log.Printf("backend: %s CLI warmup failed: %v", site.Label(), err)
			continue
		}
		s.cachedCLIModels(ctx, site)
		warmed = true
	}
	if !warmed {
		return firstErr
	}
	return nil
}

// usesRemoteTransport reports whether requests go out over the network instead
// of the local Lingma IPC pipe.
func (s *Service) usesRemoteTransport() bool {
	switch s.backend() {
	case BackendRemote, BackendQoderCLI:
		return true
	default:
		return false
	}
}

func (s *Service) Close() error {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeClientLocked()
}

func contextWithOptionalTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(parent)
	}
	return context.WithTimeout(parent, timeout)
}

func describeIPCSetupError(operation string, err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "context deadline") {
		return fmt.Errorf("Lingma/QoderCN IPC %s timed out after %s; Lingma / QoderCN 后台可能已退出，请重新打开 Lingma App、QoderCN App 或 IDE 插件后重试: %w", operation, ipcSetupTimeout, err)
	}
	return err
}

func (s *Service) State() State {
	s.ensureBackend()
	s.mu.Lock()
	backend := s.cfg.Backend
	configuredBaseURL := s.cfg.RemoteBaseURL
	sessionMode := s.cfg.SessionMode
	remoteConnected := s.remoteClient != nil
	cliConnected := len(s.cliClients) > 0
	pipePath := s.pipePath
	endpoint := s.endpoint
	transport := string(s.transport)
	ipcConnected := s.client != nil
	stickySessionID := s.stickySessionID
	s.mu.Unlock()

	if backend == BackendRemote || backend == BackendQoderCLI {
		if backend == BackendQoderCLI {
			return State{
				Transport:   "qodercli",
				Connected:   cliConnected,
				SessionMode: sessionMode,
			}
		}
		// An unconfigured base URL is looked up in the IDE config files on disk,
		// so resolve it after releasing the lock.
		return State{
			Endpoint:    remote.ResolveBaseURL(configuredBaseURL),
			Transport:   "remote",
			Connected:   remoteConnected,
			SessionMode: sessionMode,
		}
	}
	return State{
		PipePath:        pipePath,
		Endpoint:        endpoint,
		Transport:       transport,
		Connected:       ipcConnected,
		StickySessionID: stickySessionID,
		SessionMode:     sessionMode,
	}
}

// cliSiteListing keeps one site's probe result so the merge below can stay in
// site order no matter which goroutine finished first.
type cliSiteListing struct {
	names []string
	err   error
}

// A probe is a CLI subprocess spawn: measured 3-17s per site. Listing every
// site on every request therefore put /v1/models behind that variance, which is
// what clients time out on. The catalogs change on the order of days, so the
// last good names are reused for cliCatalogTTL and a probe that runs past
// cliProbeTimeout answers from them instead of handing back a half list.
//
// A site with nothing cached yet gets cliColdProbeTimeout instead: cutting it
// off at 8s with no fallback was measured to return the international 17 models
// on its own, which reads to a client as "the CN catalog is empty".
//
// 120s is a ceiling, not a fix. On 192.168.50.239 six cold /v1/models calls each
// returned all 31 ids and each took 149.4-218.3s; two others answered 500 after
// 102s and 182.3s (.scratch/s239_nine.txt, .scratch/s239_verify.txt,
// .scratch/s239_coldtime.txt). Chats are the same shape: 151.9s for one intl
// reply and a CN request that never answered inside 290s. So the catalog became
// reachable on that box and nothing else changed -- its CLI spawn is the cost,
// and no budget makes a client want to wait three minutes. What would is not
// re-deriving the list per request: keep the last good catalog across restarts.
const (
	// One probe costs ~3-17s here but 149-218s on 192.168.50.239, and the answer
	// changes on the order of days. A 5m TTL made that box pay a fresh three-minute
	// discovery about twelve times an hour, i.e. it was probing almost continuously;
	// an hour keeps the cache honest about its own staleness while making one
	// success buy an hour of quiet.
	cliCatalogTTL = time.Hour
	// A warm cache is read on the request path, so a refresh that overruns this
	// answers from the cache the caller already has.
	cliProbeTimeout = 8 * time.Second
	// Nothing cached yet: there is no fallback, so this one is allowed to be slow.
	cliColdProbeTimeout = 120 * time.Second
)

type cliCatalogEntry struct {
	names     []string
	expiresAt time.Time
}

func (s *Service) cliCatalogEntry(site qodercli.Site) cliCatalogEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cliCatalog[site.Normalized()]
}

func (s *Service) setCLICatalog(site qodercli.Site, names []string, expiresAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cliCatalog == nil {
		s.cliCatalog = map[qodercli.Site]cliCatalogEntry{}
	}
	s.cliCatalog[site.Normalized()] = cliCatalogEntry{names: names, expiresAt: expiresAt}
}

// listCLIMergedModels lists the models of every served site. The international
// catalog is namespaced because both sites expose models under the same names.
//
// Do not add a mutex here to stop the startup warm-up and a client request from
// spawning the desktop runtime at once. It cannot be justified: the lock holder can
// itself sit in cmd.Wait() for its whole budget (the grandchild-pipe case noted in
// cmd/lingma-ipc-proxy/main.go) and then every queued caller times out with it, and
// the overlap it prevents was never shown to be why a probe fails -- A/B'd on the LAN
// box, the build that had answered 31 models in 220 ms an hour earlier failed
// /v1/models exactly the same way once the box's own Qoder session went busy, mutex
// or not. The probe budgets and the catalog TTL are what bound this. If concurrent
// probes ever measure as the cost, fix cmd.Wait() to kill the whole process tree.
func (s *Service) listCLIMergedModels(ctx context.Context) ([]Model, error) {
	sites := s.cliSites()
	now := time.Now()
	listings := make([]cliSiteListing, len(sites))
	var pending []int
	for i, site := range sites {
		entry := s.cliCatalogEntry(site)
		if entry.names != nil && now.Before(entry.expiresAt) {
			listings[i].names = entry.names
			continue
		}
		pending = append(pending, i)
	}

	var wg sync.WaitGroup
	for _, i := range pending {
		wg.Add(1)
		go func(i int, site qodercli.Site) {
			defer wg.Done()
			stale := s.cliCatalogEntry(site).names
			timeout := cliProbeTimeout
			if stale == nil {
				timeout = cliColdProbeTimeout
			}
			probeCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			client, err := s.cliClientFor(site)
			var names []string
			if err == nil {
				var models []remote.Model
				if models, err = client.ListModels(probeCtx); err == nil {
					for _, model := range models {
						if name := strings.TrimSpace(model.Key); name != "" {
							names = append(names, name)
						}
					}
				}
			}
			if err != nil {
				log.Printf("backend: %s CLI model discovery failed: %v", site.Label(), err)
				listings[i] = cliSiteListing{names: stale, err: err}
				return
			}
			s.setCLICatalog(site, names, time.Now().Add(cliCatalogTTL))
			listings[i].names = names
		}(i, sites[i])
	}
	wg.Wait()

	return s.mergeCLICatalogs(sites, listings)
}

// mergeCLICatalogs folds one listing per site into the ids clients see. A site
// that errored falls back to the catalog it last answered with; a site with
// nothing to fall back to makes the whole answer a lie, so it is reported.
func (s *Service) mergeCLICatalogs(sites []qodercli.Site, listings []cliSiteListing) ([]Model, error) {
	var out []Model
	var firstErr error
	var silent []string
	seen := map[string]bool{}
	// The prefix exists only to disambiguate the two catalogs; a single-site
	// proxy serves plain CLI model names.
	prefixGlobal := len(sites) > 1
	for i, site := range sites {
		listing := listings[i]
		if listing.err != nil {
			if firstErr == nil {
				firstErr = listing.err
			}
			if listing.names == nil {
				silent = append(silent, site.Label())
				// Nothing cached either: leave the site's ids alone, a failed
				// probe must not retire a model a client may still be naming.
				continue
			}
		}
		ids := make([]string, 0, len(listing.names))
		for _, name := range listing.names {
			id := name
			if prefixGlobal && site.Normalized() == qodercli.SiteGlobal {
				id = cliGlobalPrefix + name
			}
			if seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, Model{ID: id, Name: id})
			// The cache keeps the bare name: that is what --model takes.
			ids = append(ids, name)
		}
		s.setCLIModels(site, ids)
	}
	if len(silent) > 0 {
		// Handing back only the sites that answered would tell a client the
		// silent site's models had been delisted; measured, a client then caches
		// that. Fail instead: the retry pays only for the silent site, the rest
		// is already cached.
		return nil, fmt.Errorf("%s 站点本次没有返回模型列表：%w", strings.Join(silent, "、"), firstErr)
	}
	if len(out) == 0 {
		if firstErr != nil {
			return nil, firstErr
		}
		return nil, errors.New("CLI 后端没有返回任何模型")
	}
	return out, nil
}

func (s *Service) setCLIModels(site qodercli.Site, ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cliModels == nil {
		s.cliModels = map[qodercli.Site][]string{}
	}
	s.cliModels[site.Normalized()] = ids
}

func (s *Service) ListModels(ctx context.Context) ([]Model, error) {
	if s.backend() == BackendQoderCLI {
		return s.listCLIMergedModels(ctx)
	}

	if s.backend() == BackendRemote {
		models, err := s.remoteAPI().ListModels(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]Model, 0, len(models))
		seen := map[string]bool{}
		for _, model := range models {
			id := strings.TrimSpace(model.Key)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			name := strings.TrimSpace(model.DisplayName)
			if name == "" {
				name = id
			}
			out = append(out, Model{ID: id, Name: name})
		}
		out = append(out, s.verifiedRemoteFallbackModels(ctx, seen)...)
		return out, nil
	}

	ipcClient, err := s.ensureConnected(ctx)
	if err != nil {
		return nil, err
	}

	var raw any
	if err := ipcClient.Request(ctx, "config/queryModels", map[string]any{}, &raw); err != nil {
		return nil, err
	}

	models := extractModels(raw)
	if len(models) == 0 {
		models = []Model{{ID: "lingma", Name: "Lingma", Scene: "default"}}
	}

	s.mu.Lock()
	s.modelMap = make(map[string]string, len(models))
	for _, m := range models {
		if m.InternalID != "" {
			s.modelMap[m.ID] = m.InternalID
		}
	}
	s.mu.Unlock()

	return models, nil
}

func (s *Service) Generate(ctx context.Context, req ChatRequest) (*ChatResult, error) {
	if s.usesRemoteTransport() {
		return s.generateRemote(ctx, req, nil)
	}
	return s.generateWithReconnect(ctx, req, nil)
}

func (s *Service) GenerateStream(ctx context.Context, req ChatRequest) (<-chan StreamEvent, <-chan StreamResult, error) {
	events := make(chan StreamEvent, 256)
	done := make(chan StreamResult, 1)

	go func() {
		generate := s.generateWithReconnect
		if s.usesRemoteTransport() {
			generate = s.generateRemote
		}
		result, err := generate(ctx, req, func(event StreamEvent) {
			if event.Delta == "" {
				return
			}
			select {
			case events <- event:
			case <-ctx.Done():
			}
		})

		close(events)
		done <- StreamResult{Result: result, Err: err}
		close(done)
	}()

	return events, done, nil
}

func (s *Service) generateWithReconnect(
	ctx context.Context,
	req ChatRequest,
	onDelta func(StreamEvent),
) (*ChatResult, error) {
	result, err := s.generateLocked(ctx, req, onDelta)
	if err == nil || !isRecoverableIPCError(err) {
		return result, err
	}

	s.resetConnection()
	return s.generateLocked(ctx, req, onDelta)
}

func (s *Service) generateRemote(
	ctx context.Context,
	req ChatRequest,
	onDelta func(StreamEvent),
) (*ChatResult, error) {
	return s.generateRemoteInternal(ctx, req, onDelta, false)
}

func (s *Service) generateRemoteInternal(
	ctx context.Context,
	req ChatRequest,
	onDelta func(StreamEvent),
	emulateTools bool,
) (*ChatResult, error) {
	emulateTools = emulateTools || shouldEmulateRemoteTools(req)
	if requestHasImages(req) {
		if s.backend() == BackendQoderCLI {
			// The CLI takes images as content blocks on the turn being answered.
			// Replayed history still carries older attachments, and re-sending them
			// on every turn would both bloat the frame and show the model pictures
			// it never saw, so keep only the current turn's.
			req = requestWithCurrentTurnImagesOnly(req)
		} else if len(req.Tools) > 0 && req.ToolChoice.Mode != "none" {
			return s.generateRemoteWithImageContext(ctx, req, onDelta)
		} else {
			return s.generateWithReconnect(ctx, req, onDelta)
		}
	}
	if strings.TrimSpace(req.Model) == "" {
		req.Model = s.DefaultModel()
	}
	req.Model = normalizeModelForBackend(s.backend(), req.Model)
	site := qodercli.SiteCN
	namedSite := false
	if s.backend() == BackendQoderCLI {
		site, req.Model, namedSite = splitCLISite(req.Model)
		base, effort := splitCLIModelEffort(req.Model)
		// A suffixed tier is a deliberate pick from the client's model list, so it
		// outranks the request body; an unsuffixed id leaves the client's own
		// reasoning_effort / thinking selection in charge.
		if effort != "" {
			req.ReasoningEffort = effort
		}
		resolved, err := s.resolveCLISite(site, namedSite)
		if err != nil {
			return nil, err
		}
		site = resolved
		req.Model = s.resolveCLIModel(ctx, base, site)
	}
	// The CLI backend keeps the instructions out of the user turn: the gateway
	// reroutes user content that names another product's identity, so they travel
	// as the session's system prompt instead.
	systemInline := s.backend() != BackendQoderCLI
	system, prompt, err := buildLingmaPromptSections(req, SessionModeFresh, emulateTools, systemInline)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("empty user message")
	}

	models := s.remoteAttemptModels(ctx, req.Model)
	client, err := s.chatClient(site)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for i, model := range models {
		attemptCtx, cancel := contextWithOptionalTimeout(ctx, s.cfg.Timeout)
		result, emitted, err := s.generateRemoteWithModel(attemptCtx, client, req, system, prompt, model, onDelta, emulateTools)
		cancel()
		if err == nil {
			if result != nil && namedSite {
				// Echo the id the client asked for, not the bare name --model took.
				result.Model = cliGlobalPrefix + result.Model
			}
			return result, nil
		}
		lastErr = err
		if i == len(models)-1 || emitted || !isRemoteFallbackError(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

func (s *Service) generateRemoteWithImageContext(
	ctx context.Context,
	req ChatRequest,
	onDelta func(StreamEvent),
) (*ChatResult, error) {
	imageReq := requestForImageContext(req)
	imageResult, err := s.generateWithReconnect(ctx, imageReq, nil)
	if err != nil {
		return nil, fmt.Errorf("image context extraction through IPC failed: %w", err)
	}
	remoteReq := requestWithImageContext(req, imageResult.Text)
	return s.generateRemoteInternal(ctx, remoteReq, onDelta, true)
}

func (s *Service) generateRemoteWithModel(
	ctx context.Context,
	client chatClient,
	req ChatRequest,
	system string,
	prompt string,
	model string,
	onDelta func(StreamEvent),
	emulateTools bool,
) (*ChatResult, bool, error) {
	emitted := false
	delta := func(text string) {
		if text != "" {
			emitted = true
		}
		if onDelta != nil {
			onDelta(StreamEvent{Type: StreamEventText, Delta: text})
		}
	}
	remoteResult, err := client.Chat(ctx, remote.ChatRequest{
		Model:           model,
		Prompt:          prompt,
		System:          system,
		Messages:        remoteMessagesForChat(req, prompt, emulateTools),
		Images:          remoteImagesFromRequest(req),
		Stream:          onDelta != nil,
		Temperature:     req.Temperature,
		ReasoningEffort: req.ReasoningEffort,
		Tools:           req.Tools,
		ToolChoice:      req.ToolChoice,
	}, delta)
	if err != nil {
		return nil, emitted, err
	}
	if len(remoteResult.ToolCalls) == 0 && shouldRetryRemoteNativeTool(req, remoteResult.Text) {
		retryResult, retryErr := client.Chat(ctx, remote.ChatRequest{
			Model:           model,
			Prompt:          prompt,
			System:          system,
			Messages:        remoteMessagesForChat(req, prompt, emulateTools),
			Images:          remoteImagesFromRequest(req),
			Stream:          false,
			Temperature:     req.Temperature,
			ReasoningEffort: req.ReasoningEffort,
			Tools:           req.Tools,
			ToolChoice:      toolemulation.ToolChoice{Mode: "any"},
		}, nil)
		if retryErr == nil && len(retryResult.ToolCalls) > 0 {
			remoteResult = retryResult
			emitted = false
		}
	}

	finishReason, stopReason := backendFinishReasons(remoteResult.StopReason)
	result := &ChatResult{
		Text:             remoteResult.Text,
		Model:            valueOr(strings.TrimSpace(model), "lingma"),
		InputTokens:      remoteResult.InputTokens,
		OutputTokens:     remoteResult.OutputTokens,
		SessionID:        "",
		RequestID:        remoteResult.RequestID,
		FinishReason:     finishReason,
		StopReason:       stopReason,
		Endpoint:         remote.ResolveBaseURL(s.cfg.RemoteBaseURL),
		Transport:        string(s.backend()),
		EffectiveSession: SessionModeFresh,
		ToolCalls:        remoteResult.ToolCalls,
	}
	if s.backend() == BackendQoderCLI {
		result.Endpoint = ""
	}
	if emulateTools {
		s.applyToolEmulation(ctx, req, prompt, result, func(hintPrompt string) (string, int, error) {
			retryResult, err := client.Chat(ctx, remote.ChatRequest{
				Model:           model,
				Prompt:          hintPrompt,
				Messages:        remoteMessagesForChat(req, hintPrompt, emulateTools),
				Images:          remoteImagesFromRequest(req),
				Stream:          false,
				Temperature:     req.Temperature,
				ReasoningEffort: req.ReasoningEffort,
				Tools:           req.Tools,
				ToolChoice:      req.ToolChoice,
			}, nil)
			if err != nil {
				return "", 0, err
			}
			if len(retryResult.ToolCalls) > 0 {
				result.Text = retryResult.Text
				result.ToolCalls = retryResult.ToolCalls
				result.OutputTokens = retryResult.OutputTokens
				return "", 0, nil
			}
			return retryResult.Text, retryResult.OutputTokens, nil
		})
	}
	return result, emitted, nil
}

func shouldEmulateRemoteTools(req ChatRequest) bool {
	return len(req.Tools) > 0 && req.ToolChoice.Mode != "none"
}

func remoteMessagesForChat(req ChatRequest, prompt string, emulateTools bool) []remote.Message {
	if emulateTools && shouldEmulateRemoteTools(req) {
		if prompt = strings.TrimSpace(prompt); prompt != "" {
			return []remote.Message{{Role: "user", Content: prompt}}
		}
	}
	return remoteMessagesFromRequest(req)
}

func remoteMessagesFromRequest(req ChatRequest) []remote.Message {
	out := make([]remote.Message, 0, len(req.Messages)+1)
	if system := strings.TrimSpace(req.System); system != "" {
		out = append(out, remote.Message{Role: "system", Content: system})
	}
	for _, message := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "" {
			continue
		}
		content := strings.TrimSpace(message.Text)
		if content == "" && len(message.Images) == 0 && len(message.ToolCalls) == 0 {
			continue
		}
		out = append(out, remote.Message{
			Role:       role,
			Content:    content,
			Images:     remoteImagesFromChatMessage(message),
			ToolCallID: strings.TrimSpace(message.ToolCallID),
			ToolCalls:  message.ToolCalls,
		})
	}
	return out
}

// backendFinishReasons translates the backend's own stop_reason into the pair
// the API layer serves: an OpenAI finish_reason and an Anthropic stop_reason.
// Only a token-budget stop is distinguishable today; everything else stays the
// plain "finished" value, so a turn the model ended on its own is never relabelled.
func backendFinishReasons(stopReason string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(stopReason)) {
	case "max_tokens":
		return "length", "max_tokens"
	default:
		return "stop", "end_turn"
	}
}

func remoteImagesFromChatMessage(message ChatMessage) []remote.Image {
	if len(message.Images) == 0 {
		return nil
	}
	images := make([]remote.Image, 0, len(message.Images))
	for _, img := range message.Images {
		if strings.TrimSpace(img.Data) == "" && strings.TrimSpace(img.URL) == "" {
			continue
		}
		images = append(images, remote.Image{
			MediaType: strings.TrimSpace(img.MediaType),
			Data:      img.Data,
			URL:       strings.TrimSpace(img.URL),
		})
	}
	return images
}

func remoteImagesFromRequest(req ChatRequest) []remote.Image {
	var images []remote.Image
	for _, message := range req.Messages {
		for _, img := range message.Images {
			if strings.TrimSpace(img.Data) == "" && strings.TrimSpace(img.URL) == "" {
				continue
			}
			images = append(images, remote.Image{
				MediaType: strings.TrimSpace(img.MediaType),
				Data:      img.Data,
				URL:       strings.TrimSpace(img.URL),
			})
		}
	}
	return images
}

func requestHasImages(req ChatRequest) bool {
	for _, message := range req.Messages {
		if len(remoteImagesFromChatMessage(message)) > 0 {
			return true
		}
	}
	return false
}

// currentTurnHasImages reports whether the turn being answered now carries an
// attachment. History replay means an older attachment shows up in nearly every
// later request, so scanning all messages would blame the wrong turn.
func currentTurnHasImages(req ChatRequest) bool {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(req.Messages[i].Role), "user") {
			continue
		}
		return len(remoteImagesFromChatMessage(req.Messages[i])) > 0
	}
	return false
}

// requestWithCurrentTurnImagesOnly keeps the attachment on the turn being
// answered and strips the ones replayed from history. currentTurnHasImages names
// the same message: the last user turn, which is what the CLI receives.
func requestWithCurrentTurnImagesOnly(req ChatRequest) ChatRequest {
	keep := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(req.Messages[i].Role), "user") {
			keep = i
			break
		}
	}
	out := req
	out.Messages = make([]ChatMessage, len(req.Messages))
	for i, message := range req.Messages {
		if i != keep {
			message.Images = nil
		}
		out.Messages[i] = message
	}
	return out
}

func requestForImageContext(req ChatRequest) ChatRequest {
	out := req
	out.System = ""
	out.Messages = nil
	out.Tools = nil
	out.ToolChoice = toolemulation.ToolChoice{Mode: "none"}
	out.ParallelToolCalls = nil

	for i := len(req.Messages) - 1; i >= 0; i-- {
		message := req.Messages[i]
		if !strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			continue
		}
		if len(remoteImagesFromChatMessage(message)) == 0 {
			continue
		}
		text := strings.TrimSpace(message.Text)
		if text == "" {
			text = imagePromptFallback(req, i)
		} else {
			text = "请只根据图片内容回答用户这条问题，忽略更早的对话历史：" + text
		}
		out.Messages = []ChatMessage{{
			Role:   "user",
			Text:   text,
			Images: message.Images,
		}}
		return out
	}

	return out
}

func imagePromptFallback(req ChatRequest, imageMessageIndex int) string {
	for i := imageMessageIndex - 1; i >= 0; i-- {
		message := req.Messages[i]
		if strings.EqualFold(strings.TrimSpace(message.Role), "user") {
			if text := strings.TrimSpace(message.Text); text != "" {
				return "请只根据图片内容回答用户这条问题，忽略更早的对话历史：" + text
			}
		}
	}
	system := strings.TrimSpace(req.System)
	if system != "" && len([]rune(system)) <= 1000 {
		return "请只根据图片内容回答这条要求：" + system
	}
	return "请描述这张图片的主要内容。"
}

func requestWithImageContext(req ChatRequest, imageContext string) ChatRequest {
	out := req
	out.Messages = make([]ChatMessage, len(req.Messages))
	copy(out.Messages, req.Messages)
	for i := range out.Messages {
		out.Messages[i].Images = nil
	}
	contextText := strings.TrimSpace(imageContext)
	if contextText == "" {
		return out
	}
	addition := "\n\n[图片上下文]\n" + contextText
	for i := len(out.Messages) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(out.Messages[i].Role), "user") {
			out.Messages[i].Text = strings.TrimSpace(out.Messages[i].Text + addition)
			return out
		}
	}
	out.Messages = append(out.Messages, ChatMessage{Role: "user", Text: strings.TrimSpace("[图片上下文]\n" + contextText)})
	return out
}

func shouldRetryRemoteNativeTool(req ChatRequest, text string) bool {
	if len(req.Tools) == 0 || req.ToolChoice.Mode == "none" {
		return false
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || len([]rune(trimmed)) > 180 {
		return false
	}
	lower := strings.ToLower(trimmed)
	cues := []string{
		"让我", "我来", "我将", "接下来", "继续", "查看", "检查", "搜索", "读取", "运行", "执行",
		"let me", "i'll", "i will", "next", "continue", "check", "inspect", "search", "read", "run",
	}
	hasCue := false
	for _, cue := range cues {
		if strings.Contains(lower, cue) {
			hasCue = true
			break
		}
	}
	if !hasCue {
		return false
	}
	return strings.HasSuffix(trimmed, ":") ||
		strings.HasSuffix(trimmed, "：") ||
		strings.Contains(trimmed, "：\n") ||
		strings.Contains(lower, "use ") ||
		strings.Contains(lower, "call ") ||
		strings.Contains(trimmed, "工具")
}

func (s *Service) remoteAttemptModels(ctx context.Context, primary string) []string {
	if s.backend() == BackendQoderCLI {
		// Each CLI attempt spawns a signed-in subprocess, and the CLI already
		// routes unavailable models itself, so there is nothing to fall back to.
		return []string{primary}
	}
	primary = normalizeModelForBackend(BackendRemote, primary)
	models := []string{primary}
	if !s.cfg.RemoteFallbackEnabled {
		return models
	}

	fallbackModels := s.remoteFallbackModels()
	ordered := make([]string, 0, len(fallbackModels))
	seen := map[string]bool{primary: true}
	primaryIndex := -1
	for _, candidate := range fallbackModels {
		model := normalizeModelForBackend(BackendRemote, candidate)
		if model == "" {
			continue
		}
		if model == primary && primaryIndex == -1 {
			primaryIndex = len(ordered)
		}
		ordered = append(ordered, model)
	}

	start := 0
	if primaryIndex >= 0 {
		start = primaryIndex + 1
	}
	for _, model := range ordered[start:] {
		if seen[model] {
			continue
		}
		seen[model] = true
		models = append(models, model)
	}
	return models
}

// cliModelAliases maps the legacy gateway model keys onto the display names the
// Qoder CN CLI accepts for --model.
var cliModelAliases = map[string]string{
	"kmodel":                                "Kimi-K3",
	"mmodel":                                "MiniMax-M2.7",
	"dashscope_qmodel":                      "Qwen3.8-Max",
	"dashscope_qwen_max_latest":             "Qwen3.8-Max",
	"dashscope_qwen3_coder":                 "Qwen3.8-Max",
	"dashscope_qwen_plus_20250428_thinking": "Qwen3.7-Plus",
	"org_auto":                              "Auto",
	"auto":                                  "Auto",
}

// cliEffortSuffixes let clients that cannot send reasoning_effort pick a thinking
// tier straight from the model id, e.g. "Qwen3.8-Flash-xhigh" or "Qwen3.8-Flash-极高".
// The bare "max" suffix is absent on purpose: it would shadow the real model
// Qwen3.8-Max. 最高 still names the top rung, which the CLI backend clamps per
// model because only some of them offer max.
var cliEffortSuffixes = map[string]string{
	"low":     "low",
	"medium":  "medium",
	"high":    "high",
	"xhigh":   "xhigh",
	"minimal": "low",
	"低":       "low",
	"中":       "medium",
	"高":       "high",
	"极高":      "xhigh",
	"最高":      "max",
}

// splitCLIModelEffort separates an effort suffix from the real model name. Some
// clients namespace the id with their provider key ("lingma-proxy/Qwen3.8-Flash-xhigh"),
// so the namespace is dropped before the suffix is read.
func splitCLIModelEffort(model string) (string, string) {
	trimmed := strings.TrimSpace(model)
	if i := strings.LastIndexByte(trimmed, '/'); i >= 0 {
		trimmed = strings.TrimSpace(trimmed[i+1:])
	}
	i := strings.LastIndexAny(trimmed, "-_")
	if i <= 0 {
		return trimmed, ""
	}
	base, suffix := strings.TrimSpace(trimmed[:i]), strings.ToLower(strings.TrimSpace(trimmed[i+1:]))
	if effort, ok := cliEffortSuffixes[suffix]; ok && base != "" {
		return base, effort
	}
	return trimmed, ""
}

// splitCLISite reads the site marker out of a model id. Clients namespace the id
// with their own provider key ("lingma-proxy/intl/Qwen3.8-Flash"), so the marker
// is looked for in every path segment rather than only the first.
func splitCLISite(model string) (qodercli.Site, string, bool) {
	trimmed := strings.TrimSpace(model)
	segments := strings.Split(trimmed, "/")
	for i, segment := range segments {
		switch strings.ToLower(strings.TrimSpace(segment)) {
		case "intl", "global", "国际", "国际版":
			rest := strings.TrimSpace(strings.Join(segments[i+1:], "/"))
			if rest != "" {
				return qodercli.SiteGlobal, rest, true
			}
		}
	}
	return qodercli.SiteCN, trimmed, false
}

// resolveCLIModel maps whatever the client asked for onto a model the signed-in
// account of that site actually exposes.
func (s *Service) resolveCLIModel(ctx context.Context, model string, site qodercli.Site) string {
	wanted := strings.TrimSpace(model)
	if alias, ok := cliModelAliases[strings.ToLower(wanted)]; ok {
		wanted = alias
	}
	known := s.cachedCLIModels(ctx, site)
	if len(known) == 0 {
		if wanted == "" {
			return "Auto"
		}
		return wanted
	}
	if wanted == "" {
		wanted = "Auto"
	}
	for _, name := range known {
		if strings.EqualFold(name, wanted) {
			return name
		}
	}
	family := cliModelFamily(wanted)
	if family != "" {
		for _, name := range known {
			if cliModelFamily(name) == family {
				return name
			}
		}
	}
	for _, name := range known {
		if strings.EqualFold(name, "Auto") {
			return name
		}
	}
	return known[0]
}

func cliModelFamily(model string) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return ""
	}
	if i := strings.IndexByte(model, '-'); i > 0 {
		return model[:i]
	}
	return model
}

// cachedCLIModels returns one site's model list, discovering it on first use.
// The names are bare, as the CLI wants them.
func (s *Service) cachedCLIModels(ctx context.Context, site qodercli.Site) []string {
	read := func() []string {
		s.mu.Lock()
		defer s.mu.Unlock()
		return append([]string(nil), s.cliModels[site.Normalized()]...)
	}
	if cached := read(); len(cached) > 0 {
		return cached
	}
	if _, err := s.listCLIMergedModels(ctx); err != nil {
		log.Printf("backend: %s CLI model discovery failed: %v", site.Label(), err)
	}
	return read()
}

func (s *Service) remoteFallbackModels() []string {
	fallbackModels := s.cfg.RemoteFallbackModels
	if len(fallbackModels) == 0 {
		fallbackModels = DefaultRemoteFallbackModels()
	}
	out := make([]string, 0, len(fallbackModels))
	seen := map[string]bool{}
	for _, candidate := range fallbackModels {
		model := normalizeModelForBackend(BackendRemote, candidate)
		if model == "" || seen[model] {
			continue
		}
		seen[model] = true
		out = append(out, model)
	}
	return out
}

func (s *Service) verifiedRemoteFallbackModels(ctx context.Context, seen map[string]bool) []Model {
	out := make([]Model, 0, 2)
	for _, model := range s.remoteFallbackModels() {
		if seen[model] || !shouldProbeRemoteModelForList(model) {
			continue
		}
		if !s.probeRemoteModel(ctx, model) {
			continue
		}
		seen[model] = true
		out = append(out, Model{ID: model, Name: remoteModelDisplayName(model)})
	}
	return out
}

func shouldProbeRemoteModelForList(model string) bool {
	switch normalizeModelForBackend(BackendRemote, model) {
	case "kmodel", "mmodel":
		return true
	default:
		return false
	}
}

func (s *Service) probeRemoteModel(ctx context.Context, model string) bool {
	model = normalizeModelForBackend(BackendRemote, model)
	if model == "" {
		return false
	}
	now := time.Now()
	s.mu.Lock()
	if entry, ok := s.remoteProbeCache[model]; ok && now.Before(entry.ExpiresAt) {
		s.mu.Unlock()
		return entry.Available
	}
	s.mu.Unlock()

	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	temperature := 0.0
	_, err := s.remoteAPI().Chat(probeCtx, remote.ChatRequest{
		Model:       model,
		Prompt:      "Reply with OK only.",
		Messages:    []remote.Message{{Role: "user", Content: "Reply with OK only."}},
		Temperature: &temperature,
	}, nil)
	available := err == nil
	ttl := 5 * time.Minute
	if available {
		ttl = 30 * time.Minute
	}

	s.mu.Lock()
	if s.remoteProbeCache == nil {
		s.remoteProbeCache = make(map[string]remoteModelProbeEntry)
	}
	s.remoteProbeCache[model] = remoteModelProbeEntry{Available: available, ExpiresAt: now.Add(ttl)}
	s.mu.Unlock()
	return available
}

func isRemoteFallbackError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "context deadline exceeded") ||
		strings.Contains(msg, "client.timeout") ||
		strings.Contains(msg, "timeout awaiting response") ||
		strings.Contains(msg, "remote chat status 5") ||
		strings.Contains(msg, "remote chat status 429") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "unexpected eof")
}

func (s *Service) generateLocked(
	ctx context.Context,
	req ChatRequest,
	onDelta func(StreamEvent),
) (result *ChatResult, err error) {
	requestCtx, cancel := contextWithOptionalTimeout(ctx, s.cfg.Timeout)
	defer cancel()

	ipcClient, err := s.ensureConnected(requestCtx)
	if err != nil {
		return nil, err
	}

	effectiveMode := resolveSessionMode(req, s.cfg.SessionMode)
	prompt, err := buildLingmaPrompt(req, effectiveMode, true)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, errors.New("empty user message")
	}

	setupCtx, setupCancel := context.WithTimeout(requestCtx, ipcSetupTimeout)
	sessionID, err := s.resolveSession(setupCtx, ipcClient, effectiveMode)
	setupCancel()
	if err != nil {
		return nil, describeIPCSetupError("session setup", err)
	}

	// turnAbandoned marks the paths that drop this session locally. Dropping a reuse
	// session on our side has to be matched by deleting it on the plug-in's, or it
	// keeps generating for a client that is already gone.
	turnAbandoned := false
	abandonTurn := func() {
		turnAbandoned = true
		if effectiveMode == SessionModeReuse {
			s.invalidateStickySession(sessionID)
		}
	}
	defer func() {
		if effectiveMode == SessionModeReuse && !turnAbandoned {
			return
		}
		if strings.TrimSpace(sessionID) == "" {
			return
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		_ = s.deleteSession(cleanupCtx, ipcClient, sessionID)
	}()

	if strings.TrimSpace(req.Model) == "" {
		req.Model = s.DefaultModel()
	}
	internalModelID := s.resolveInternalModelID(req.Model)

	requestID := lingmaipc.CreateRequestID("serve")
	meta := lingmaipc.CreateMeta(lingmaipc.MetaOptions{
		RequestID:       requestID,
		Mode:            s.cfg.Mode,
		Model:           internalModelID,
		ShellType:       s.cfg.ShellType,
		CurrentFilePath: s.cfg.CurrentFilePath,
		EnabledMCP:      []any{},
	})

	modelID := strings.TrimSpace(internalModelID)
	if modelID != "" && s.shouldSetModel(sessionID, effectiveMode, modelID) {
		modelCtx, modelCancel := context.WithTimeout(requestCtx, ipcSetupTimeout)
		err := ipcClient.Request(modelCtx, "session/set_model", map[string]any{
			"sessionId": sessionID,
			"modelId":   modelID,
			"timestamp": time.Now().UnixMilli(),
			"_meta":     meta,
		}, nil)
		modelCancel()
		if err != nil {
			abandonTurn()
			return nil, describeIPCSetupError("model setup", err)
		}
		s.rememberStickyModel(sessionID, modelID)
	}

	images := extractLastUserImages(req.Messages)

	runResult, err := s.runPromptLocked(requestCtx, ipcClient, sessionID, prompt, images, requestID, meta, onDelta)
	if err != nil {
		abandonTurn()
		return nil, err
	}
	if runResult.TimedOut || strings.TrimSpace(runResult.AssistantText) == "" {
		abandonTurn()
	}
	if runResult.TimedOut && strings.TrimSpace(runResult.AssistantText) == "" {
		return nil, errors.New("timed out while waiting for Lingma IPC to finish responding")
	}
	if strings.TrimSpace(runResult.AssistantText) == "" {
		return nil, errors.New("Lingma IPC did not produce an assistant reply")
	}
	if runResult.TimedOut {
		return nil, fmt.Errorf("Lingma IPC response remained incomplete before timeout. Partial reply: %s", truncate(runResult.AssistantText, 120))
	}

	result = s.buildChatResult(req, sessionID, requestID, prompt, runResult, effectiveMode)

	s.applyToolEmulation(requestCtx, req, prompt, result, func(hintPrompt string) (string, int, error) {
		retryRequestID := lingmaipc.CreateRequestID("serve-tool")
		retryMeta := lingmaipc.CreateMeta(lingmaipc.MetaOptions{
			RequestID:       retryRequestID,
			Mode:            s.cfg.Mode,
			Model:           internalModelID,
			ShellType:       s.cfg.ShellType,
			CurrentFilePath: s.cfg.CurrentFilePath,
			EnabledMCP:      []any{},
		})
		// nil onDelta: the client already streamed the first attempt, and result.Text
		// is about to be replaced by this retry. The remote retry passes nil too.
		retryRunResult, retryErr := s.runPromptLocked(requestCtx, ipcClient, sessionID, hintPrompt, images, retryRequestID, retryMeta, nil)
		if retryErr != nil {
			return "", 0, retryErr
		}
		return retryRunResult.AssistantText, estimateTokens(retryRunResult.AssistantText), nil
	})
	return result, nil
}

func (s *Service) backend() BackendMode {
	s.ensureBackend()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.Backend == "" {
		return BackendIPC
	}
	return s.cfg.Backend
}

// Backend reports the mode requests are actually served with, which can differ
// from the configured value until the lazy remote/CLI choice has been made.
func (s *Service) Backend() BackendMode { return s.backend() }

func (s *Service) remoteAPI() *remote.Client {
	s.mu.Lock()
	existing := s.remoteClient
	cfg := remote.Config{
		BaseURL:     s.cfg.RemoteBaseURL,
		AuthFile:    s.cfg.RemoteAuthFile,
		ProxyURL:    s.cfg.RemoteProxyURL,
		CosyVersion: s.cfg.RemoteVersion,
		Timeout:     s.cfg.Timeout,
	}
	s.mu.Unlock()
	if existing != nil {
		return existing
	}
	// remote.New resolves an empty BaseURL by reading every candidate config file
	// it can find -- seconds of IO that must not happen under s.mu. Publish the
	// finished client only, and keep the first winner.
	fresh := remote.New(cfg)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.remoteClient == nil {
		s.remoteClient = fresh
	}
	return s.remoteClient
}

func (s *Service) applyToolEmulation(
	ctx context.Context,
	req ChatRequest,
	prompt string,
	result *ChatResult,
	retry func(string) (string, int, error),
) {
	// tool_choice:"none" is the client forbidding tool calls, so a block that
	// arrives anyway (from an echoed example, or from history) stays prose:
	// turning it into a tool call would hand back an action the client ruled out.
	if len(req.Tools) > 0 && req.ToolChoice.Mode != "none" {
		calls, remaining, parseErr := toolemulation.ParseActionBlocks(result.Text, req.Tools, toolemulation.Config{})
		if parseErr == nil && len(calls) > 0 {
			result.Text = remaining
			result.ToolCalls = calls
		} else if shouldRetryTooling(req.ToolChoice, result.Text) {
			hintPrompt := prompt + "\n\n" + toolemulation.ForceToolingPrompt(req.ToolChoice)
			retryText := ""
			if retry != nil {
				text, outputTokens, retryErr := retry(hintPrompt)
				if retryErr == nil {
					retryText = text
					if outputTokens > 0 {
						result.OutputTokens = outputTokens
					}
				}
			}
			if retryText != "" {
				retryCalls, retryRemaining, retryParseErr := toolemulation.ParseActionBlocks(retryText, req.Tools, toolemulation.Config{})
				if retryParseErr == nil && len(retryCalls) > 0 {
					result.Text = retryRemaining
					result.ToolCalls = retryCalls
					result.OutputTokens = estimateTokens(retryText)
				} else if inferred := toolemulation.InferToolCallsFromText(retryText, req.Tools); len(inferred) > 0 {
					result.Text = ""
					result.ToolCalls = inferred
					result.OutputTokens = estimateTokens(retryText)
				}
			}
			if len(result.ToolCalls) == 0 {
				if inferred := toolemulation.InferToolCallsFromText(result.Text, req.Tools); len(inferred) > 0 {
					result.Text = ""
					result.ToolCalls = inferred
				}
			}
		}
	}
}

func shouldRetryTooling(choice toolemulation.ToolChoice, text string) bool {
	switch choice.Mode {
	case "any", "tool":
		return true
	case "none":
		return false
	}
	return toolemulation.LooksLikeRefusal(text) || toolemulation.LooksLikeMissedToolUse(text)
}

func isRecoverableIPCError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	needles := []string{
		"use of closed network connection",
		"broken pipe",
		"connection reset by peer",
		"connection refused",
		"websocket: close",
		"unexpected eof",
		"io: read/write on closed pipe",
		"lingma ipc notification stream closed",
	}
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			return true
		}
	}
	return false
}

func (s *Service) buildChatResult(
	req ChatRequest,
	sessionID string,
	requestID string,
	prompt string,
	runResult *promptRunResult,
	effectiveMode SessionMode,
) *ChatResult {
	endpoint := s.currentPipePath()
	return &ChatResult{
		Text:             runResult.AssistantText,
		ThoughtText:      runResult.ThoughtText,
		Model:            valueOr(strings.TrimSpace(req.Model), "lingma"),
		InputTokens:      estimateTokens(prompt),
		OutputTokens:     estimateTokens(runResult.AssistantText),
		SessionID:        sessionID,
		RequestID:        requestID,
		FinishReason:     nestedString(runResult.FinishData, "reason"),
		StopReason:       nestedString(runResult.PromptResult, "stopReason"),
		UsedTokens:       int(nestedInt64(runResult.ContextUsage, "usedTokens")),
		LimitTokens:      int(nestedInt64(runResult.ContextUsage, "limitTokens")),
		ThinkingDuration: runResult.ThinkingDuration,
		PipePath:         endpoint,
		Endpoint:         endpoint,
		Transport:        string(s.currentTransport()),
		EffectiveSession: effectiveMode,
	}
}

// ensureConnected dials the plugin IPC endpoint once and reuses the client.
// The handshake runs under connectMu rather than s.mu: dialing and initialize
// can take seconds, and s.mu also guards the remote and CLI backends, so a
// slow reconnect used to freeze unrelated requests.
func (s *Service) ensureConnected(ctx context.Context) (*lingmaipc.Client, error) {
	if client := s.connectedClient(); client != nil {
		return client, nil
	}

	s.connectMu.Lock()
	defer s.connectMu.Unlock()
	if client := s.connectedClient(); client != nil {
		return client, nil
	}

	dialOptions, err := lingmaipc.ResolveDialOptions(s.cfg.Transport, s.cfg.Pipe, s.cfg.WebSocketURL)
	if err != nil {
		return nil, err
	}
	client, err := lingmaipc.Connect(ctx, dialOptions)
	if err != nil {
		return nil, err
	}
	if err := client.Request(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
		"timestamp":          time.Now().UnixMilli(),
	}, nil); err != nil {
		_ = client.Close()
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.client != nil {
		// resetConnection ran while the handshake was in flight; keep that state.
		_ = client.Close()
		return s.client, nil
	}
	s.client = client
	s.pipePath = dialOptions.PipePath
	s.endpoint = client.Address()
	s.transport = client.Transport()
	return client, nil
}

func (s *Service) connectedClient() *lingmaipc.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

func (s *Service) closeClientLocked() error {
	if s.client == nil {
		s.pipePath = ""
		s.endpoint = ""
		s.transport = ""
		s.clearStickyLocked()
		return nil
	}
	client := s.client
	s.client = nil
	s.pipePath = ""
	s.endpoint = ""
	s.transport = ""
	s.clearStickyLocked()
	return client.Close()
}

func (s *Service) resetConnection() {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.closeClientLocked()
}

// resolveSession returns the session this turn should use, creating one when the
// sticky slot is empty or the caller asked for a fresh session. The IPC round trip
// runs outside mu: a session/new over the plugin pipe takes seconds, and holding
// the exclusive lock across it stopped /v1/models, the status page and the console.
// Two requests that both find the slot empty now get their own session instead of
// queueing on each other -- the later one takes the sticky slot, each turn keeps
// the session it created.
func (s *Service) resolveSession(ctx context.Context, client *lingmaipc.Client, mode SessionMode) (string, error) {
	if mode == SessionModeReuse {
		s.mu.Lock()
		sticky := strings.TrimSpace(s.stickySessionID)
		s.mu.Unlock()
		if sticky != "" {
			return sticky, nil
		}
	}

	var created struct {
		SessionID string `json:"sessionId"`
		ID        string `json:"id"`
	}
	if err := client.Request(ctx, "session/new", map[string]any{
		"cwd":        s.cfg.Cwd,
		"mcpServers": []any{},
		"_meta":      map[string]any{},
		"timestamp":  time.Now().UnixMilli(),
	}, &created); err != nil {
		return "", err
	}

	sessionID := strings.TrimSpace(created.SessionID)
	if sessionID == "" {
		sessionID = strings.TrimSpace(created.ID)
	}
	if sessionID == "" {
		return "", errors.New("Lingma IPC did not return a sessionId")
	}

	if mode == SessionModeReuse {
		s.mu.Lock()
		s.stickySessionID = sessionID
		s.stickyModelID = ""
		s.mu.Unlock()
	}
	return sessionID, nil
}

func (s *Service) invalidateStickySession(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The identity check is the whole function: a concurrent request can have
	// installed a newer sticky session by now, and dropping that one would cut off a
	// turn that is still streaming.
	if strings.TrimSpace(s.stickySessionID) == strings.TrimSpace(sessionID) {
		s.clearStickyLocked()
	}
}

func (s *Service) rememberStickyModel(sessionID string, modelID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(s.stickySessionID) == strings.TrimSpace(sessionID) {
		s.stickyModelID = strings.TrimSpace(modelID)
	}
}

func (s *Service) shouldSetModel(sessionID string, mode SessionMode, modelID string) bool {
	if strings.TrimSpace(modelID) == "" {
		return false
	}
	if mode != SessionModeReuse {
		return true
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(s.stickySessionID) != strings.TrimSpace(sessionID) {
		return true
	}
	return strings.TrimSpace(s.stickyModelID) != strings.TrimSpace(modelID)
}

func (s *Service) clearStickyLocked() {
	s.stickySessionID = ""
	s.stickyModelID = ""
}

func (s *Service) currentPipePath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(s.endpoint) != "" {
		return s.endpoint
	}
	return s.pipePath
}

func (s *Service) currentTransport() lingmaipc.Transport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.transport
}

// imageTempHorizon is how long a spooled IPC image file may still be needed. The
// legacy Lingma host resolves the lingma:///agent/file?path= URI on its own
// schedule, so removing the file when the request returns can cut off a slow
// fetch; anything past this age cannot belong to a turn that is still live.
const imageTempHorizon = 24 * time.Hour

// imageTempSweepMinInterval bounds how often the request path may sweep. A sweep is
// one glob plus a stat per match, so running it for every attachment costs O(files)
// per image on a host that piles them up.
const imageTempSweepMinInterval = time.Minute

// lastImageTempSweep is the unix-nano time of the most recent sweep, shared by the
// startup call and every write path.
var lastImageTempSweep atomic.Int64

// SweepImageTemps removes image files the legacy Lingma IPC path spooled into
// the temp directory, plus the leftovers of builds that spooled them for Qoder CN
// too. Call it once at process start: building a Service is not a reason to touch
// the filesystem.
func SweepImageTemps() {
	lastImageTempSweep.Store(time.Now().UnixNano())
	matches, err := filepath.Glob(filepath.Join(os.TempDir(), "lingma-img-*"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-imageTempHorizon)
	for _, path := range matches {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(path)
	}
}

// sweepImageTempsForWrites keeps the startup sweep company: a host that runs for
// weeks and only attaches images would otherwise grow the temp directory forever.
// ponytail: one process-wide timestamp rather than per-Service state, because the
// files are process-wide too and a deferred sweep still happens before the next
// interval passes.
func sweepImageTempsForWrites() {
	now := time.Now()
	last := lastImageTempSweep.Load()
	if last != 0 && now.Sub(time.Unix(0, last)) < imageTempSweepMinInterval {
		return
	}
	// CAS hands the sweep to exactly one of the racing writers.
	if !lastImageTempSweep.CompareAndSwap(last, now.UnixNano()) {
		return
	}
	SweepImageTemps()
}

func (s *Service) runPromptLocked(
	ctx context.Context,
	client *lingmaipc.Client,
	sessionID string,
	text string,
	images []Image,
	requestID string,
	meta map[string]any,
	onDelta func(StreamEvent),
) (*promptRunResult, error) {
	notifications, cancel := client.Subscribe()
	defer cancel()
	// DroppedNotifications is lifetime-wide, so only a delta says anything about
	// this turn. See the timeout path below for why the count belongs in the log.
	droppedAtStart := client.DroppedNotifications()

	promptItems := []map[string]any{
		{"type": "text", "text": text},
	}

	imageScheme := s.ipcImageURIScheme()
	for _, img := range images {
		item, ok := imagePromptItem(imageScheme, img)
		if !ok {
			continue
		}
		promptItems = append(promptItems, item)
	}

	params := map[string]any{
		"sessionId": sessionID,
		"prompt":    promptItems,
		"_meta":     meta,
	}
	// Fallback: if images have URLs, also pass via extra field
	for _, img := range images {
		if img.URL != "" {
			params["extra"] = map[string]any{"imageUrl": img.URL}
			break
		}
	}

	if err := client.Send("session/prompt", params); err != nil {
		return nil, err
	}

	result := &promptRunResult{PromptResult: map[string]any{}}
	var builder strings.Builder
	var thoughtBuilder strings.Builder

	for {
		select {
		case <-ctx.Done():
			result.AssistantText = builder.String()
			result.ThoughtText = thoughtBuilder.String()
			result.TimedOut = true
			if dropped := client.DroppedNotifications() - droppedAtStart; dropped > 0 {
				// A saturated subscriber buffer loses the newest frame first, and the
				// newest frame is chat_finish -- so the turn can be complete in
				// builder and still be abandoned downstream as "response remained
				// incomplete". The count is the only evidence either way.
				log.Printf("lingma IPC: turn %s hit its deadline with %d notification frame(s) dropped, %d bytes of answer held", requestID, dropped, len(result.AssistantText))
			}
			return result, nil
		case notification, ok := <-notifications:
			if !ok {
				result.AssistantText = builder.String()
				result.ThoughtText = thoughtBuilder.String()
				if result.AssistantText == "" {
					return nil, errors.New("Lingma IPC notification stream closed")
				}
				return result, nil
			}
			if notification.Method != "session/update" {
				continue
			}
			if nestedStringFromMap(notification.Params, "_meta", lingmaipc.MetaRequestID) != requestID {
				continue
			}

			update := nestedMap(notification.Params, "update")
			switch nestedString(update, "sessionUpdate") {
			case "agent_thought_chunk":
				chunk := nestedString(nestedMap(update, "content"), "text")
				if chunk != "" {
					thoughtBuilder.WriteString(chunk)
					if onDelta != nil {
						onDelta(StreamEvent{Type: StreamEventThinking, Delta: chunk})
					}
				}
				duration := nestedInt64(nestedMap(update, "_meta"), "ai-coding/thinking-duration-millis")
				if duration > result.ThinkingDuration {
					result.ThinkingDuration = duration
				}
			case "agent_message_chunk":
				chunk := nestedString(nestedMap(update, "content"), "text")
				if chunk != "" {
					builder.WriteString(chunk)
					if onDelta != nil {
						onDelta(StreamEvent{Type: StreamEventText, Delta: chunk})
					}
				}
			case "notification":
				switch nestedString(update, "type") {
				case "context_usage":
					result.ContextUsage = nestedMap(update, "data")
				case "chat_finish":
					result.FinishData = nestedMap(update, "data")
					result.AssistantText = builder.String()
					result.ThoughtText = thoughtBuilder.String()
					return result, nil
				}
			}
		}
	}
}

// imagePromptItem builds one session/prompt image item. Qoder CN reads the
// inline data field and never resolves the agent/file URI, so only the legacy
// Lingma host still gets a spooled file; writing the user's image to the temp
// directory for a reader that ignores it just leaked it.
func imagePromptItem(imageScheme string, img Image) (map[string]any, bool) {
	if img.Data == "" && img.URL == "" {
		return nil, false
	}
	mediaType := img.MediaType
	if mediaType == "" {
		mediaType = "image/jpeg"
	}

	var imageURI string
	if img.Data != "" && imageScheme != "qodercn" {
		// This is the only branch that leaves a file behind, so it pays for the
		// cleanup of the ones it and every earlier request left behind.
		sweepImageTempsForWrites()
		if tmpFile, err := os.CreateTemp("", "lingma-img-*"+imageExtension(mediaType)); err == nil {
			tmpPath := tmpFile.Name()
			_ = tmpFile.Close()
			data, _ := base64.StdEncoding.DecodeString(img.Data)
			if len(data) > 0 {
				_ = os.WriteFile(tmpPath, data, 0600)
				if absPath, err := filepath.Abs(tmpPath); err == nil {
					imageURI = fmt.Sprintf("%s:///agent/file?path=%s", imageScheme, url.QueryEscape(absPath))
				}
			}
		}
	}
	if img.URL != "" {
		imageURI = img.URL
	}
	if imageURI == "" && img.Data == "" {
		return nil, false
	}
	return map[string]any{
		"type":     "image",
		"mimeType": mediaType,
		"data":     img.Data,
		"uri":      imageURI,
	}, true
}

func (s *Service) ipcImageURIScheme() string {
	// pipePath and endpoint belong to the live connection: ensureConnected writes
	// them and closeClientLocked clears them, so read them as one snapshot. The
	// cfg pair never changes after New, and the fallback probe touches the
	// filesystem, which is why it stays outside the lock.
	s.mu.Lock()
	pipePath, endpoint := s.pipePath, s.endpoint
	s.mu.Unlock()
	values := strings.ToLower(strings.Join([]string{s.cfg.Pipe, s.cfg.WebSocketURL, pipePath, endpoint}, " "))
	if strings.Contains(values, "qoder") || strings.Contains(values, "qodercn") {
		return "qodercn"
	}
	// The fallback probes the filesystem for an install layout, which does not move
	// while the proxy runs. Once makes the second and later attachments free, and
	// publishes the value to every reader.
	s.imageSchemeOnce.Do(func() {
		s.imageSchemeValue = lingmaipc.DefaultImageURIScheme(s.cfg.Pipe, s.cfg.WebSocketURL)
	})
	return s.imageSchemeValue
}

func (s *Service) deleteSession(ctx context.Context, client *lingmaipc.Client, sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}

	if err := client.Request(ctx, "chat/deleteSessionById", map[string]any{
		"sessionId": sessionID,
	}, nil); err == nil {
		return nil
	}

	return client.Request(ctx, "chat/deleteSessionById", map[string]any{
		"id": sessionID,
	}, nil)
}

func resolveSessionMode(req ChatRequest, configured SessionMode) SessionMode {
	if configured != SessionModeAuto {
		return configured
	}
	return SessionModeFresh
}

func extractLastUserImages(messages []ChatMessage) []Image {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && len(messages[i].Images) > 0 {
			return messages[i].Images
		}
	}
	return nil
}

func imageExtension(mediaType string) string {
	switch mediaType {
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/bmp":
		return ".bmp"
	default:
		return ".jpg"
	}
}

func buildLingmaPrompt(req ChatRequest, mode SessionMode, emulateTools bool) (string, error) {
	_, prompt, err := buildLingmaPromptSections(req, mode, emulateTools, true)
	return prompt, err
}

// buildLingmaPromptSections splits the assembled request into instructions and
// the text of the user turn. systemInline keeps the prompt byte-identical for
// the ipc and remote backends, which expect the tooling block immediately before
// the "Assistant:" cue; the CLI backend asks for it separately so it can travel
// in the model's system slot instead of inside the user turn.
func buildLingmaPromptSections(req ChatRequest, mode SessionMode, emulateTools, systemInline bool) (string, string, error) {
	messages := filteredMessages(req.Messages)
	var lastUser string
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" {
			lastUser = messages[i].Text
			break
		}
	}
	if strings.TrimSpace(lastUser) == "" {
		if idx := latestImageMessageIndex(req.Messages); idx >= 0 {
			lastUser = imagePromptFallback(req, idx)
			messages = append(messages, ChatMessage{Role: "user", Text: lastUser})
		} else {
			return "", "", errors.New("no user message found in request")
		}
	}
	if mode == SessionModeReuse {
		return "", lastUser, nil
	}

	system := strings.TrimSpace(req.System)
	if reasoningHint := reasoningSystemHint(req.ReasoningEffort); reasoningHint != "" {
		if system == "" {
			system = reasoningHint
		} else {
			system = reasoningHint + "\n\n" + system
		}
	}
	// With systemInline the client's instructions become part of the prompt text,
	// exactly as the ipc and remote backends expect. The CLI backend returns them
	// as a separate section and keeps only the action-block rules in the prompt,
	// because those still have to be the last thing before the "Assistant:" cue.
	section, embedded := "", system
	if !systemInline {
		section, embedded = system, ""
	}
	if emulateTools && len(req.Tools) > 0 && req.ToolChoice.Mode != "none" {
		if systemInline {
			embedded = toolemulation.InjectTooling(system, req.Tools, req.ToolChoice, req.ParallelToolCalls)
		} else {
			embedded = toolemulation.InjectTooling("", req.Tools, req.ToolChoice, req.ParallelToolCalls)
		}
	}

	if embedded == "" && len(messages) == 1 {
		return section, lastUser, nil
	}

	if emulateTools && len(req.Tools) > 0 {
		parts := make([]string, 0, len(messages)+3)
		for _, message := range messages {
			role := "User"
			if message.Role == "assistant" {
				role = "Assistant"
			}
			parts = append(parts, fmt.Sprintf("%s: %s", role, message.Text))
		}
		if embedded != "" {
			// Append tool prompt right before the final "Assistant:" so it
			// is the last thing the model sees before generating a reply.
			parts = append(parts, embedded)
		}
		parts = append(parts, "Assistant:")
		return section, strings.Join(parts, "\n\n"), nil
	}

	parts := make([]string, 0, len(messages)+4)
	if embedded != "" {
		parts = append(parts, "System instructions:", embedded)
	}
	parts = append(parts, "Conversation transcript:")
	for _, message := range messages {
		role := "User"
		if message.Role == "assistant" {
			role = "Assistant"
		}
		parts = append(parts, fmt.Sprintf("%s: %s", role, message.Text))
	}
	parts = append(parts, "Reply as the assistant to the latest user message only. Follow the system instructions and prior transcript naturally.")
	return section, strings.Join(parts, "\n\n"), nil
}

func latestImageMessageIndex(messages []ChatMessage) int {
	for i := len(messages) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(messages[i].Role), "user") {
			continue
		}
		if len(remoteImagesFromChatMessage(messages[i])) > 0 {
			return i
		}
	}
	return -1
}

func filteredMessages(messages []ChatMessage) []ChatMessage {
	out := make([]ChatMessage, 0, len(messages))
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		text := strings.TrimSpace(message.Text)
		if text == "" {
			continue
		}
		if role == "tool" {
			text = toolemulation.ActionOutputPrompt(message.ToolCallID, text)
			role = "user"
		}
		if role != "user" && role != "assistant" {
			continue
		}
		out = append(out, ChatMessage{Role: role, Text: text})
	}
	return out
}

func reasoningSystemHint(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "":
		return ""
	case "low":
		return "Reasoning mode is enabled. Think briefly but deliberately before answering. Do not reveal private chain-of-thought; only provide the final answer."
	case "high":
		return "Reasoning mode is enabled. Take extra time to reason carefully before answering. Do not reveal private chain-of-thought; only provide the final answer and any concise user-facing rationale."
	default:
		return "Reasoning mode is enabled. Think carefully before answering. Do not reveal private chain-of-thought; only provide the final answer and any concise user-facing rationale."
	}
}

func estimateTokens(text string) int {
	text = strings.TrimSpace(text)
	if text == "" {
		return 1
	}
	return max(1, (len([]rune(text))+2)/3)
}

func extractModels(raw any) []Model {
	seen := make(map[string]Model)
	var walk func(scene string, value any)
	walk = func(scene string, value any) {
		switch typed := value.(type) {
		case map[string]any:
			id := firstString(typed, "id", "modelId", "key")
			name := firstString(typed, "name", "label", "displayName", "title")
			currentScene := scene
			if currentScene == "" {
				currentScene = firstString(typed, "scene", "sceneId", "category")
			}
			if id != "" && (name != "" || likelyModelID(id)) {
				if name == "" {
					name = id
				}
				// First entry wins: map order is random, so letting a later scene
				// overwrite this one would reroute /v1/chat on every refresh.
				if _, dup := seen[name]; !dup {
					seen[name] = Model{ID: name, Name: name, Scene: currentScene, InternalID: id}
				}
			}
			keys := make([]string, 0, len(typed))
			for key := range typed {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				child := typed[key]
				nextScene := currentScene
				if nextScene == "" || isSceneKey(key) {
					nextScene = key
				}
				walk(nextScene, child)
			}
		case []any:
			for _, item := range typed {
				walk(scene, item)
			}
		}
	}
	walk("", raw)

	models := make([]Model, 0, len(seen))
	for _, model := range seen {
		models = append(models, model)
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models
}

func likelyModelID(id string) bool {
	lowered := strings.ToLower(id)
	return strings.Contains(lowered, "qwen") || strings.Contains(lowered, "model") || strings.Contains(lowered, "auto") || strings.Contains(lowered, "coder")
}

func (s *Service) resolveInternalModelID(officialName string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if internalID, ok := s.modelMap[officialName]; ok && internalID != "" {
		return internalID
	}
	return officialName
}

func isSceneKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "assistant", "chat", "developer", "inline", "quest":
		return true
	default:
		return false
	}
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := m[key]; ok {
			switch typed := value.(type) {
			case string:
				if strings.TrimSpace(typed) != "" {
					return strings.TrimSpace(typed)
				}
			case json.Number:
				return typed.String()
			}
		}
	}
	return ""
}

func nestedMap(m map[string]any, key string) map[string]any {
	if value, ok := m[key]; ok {
		if typed, ok := value.(map[string]any); ok {
			return typed
		}
	}
	return map[string]any{}
}

func nestedString(m map[string]any, key string) string {
	if value, ok := m[key]; ok {
		switch typed := value.(type) {
		case string:
			return typed
		case json.Number:
			return typed.String()
		case float64:
			return fmt.Sprintf("%.0f", typed)
		}
	}
	return ""
}

func nestedStringFromMap(m map[string]any, parent string, key string) string {
	child := nestedMap(m, parent)
	return nestedString(child, key)
}

func nestedInt64(m map[string]any, key string) int64 {
	if value, ok := m[key]; ok {
		switch typed := value.(type) {
		case int:
			return int64(typed)
		case int64:
			return typed
		case float64:
			return int64(typed)
		case json.Number:
			if n, err := typed.Int64(); err == nil {
				return n
			}
		}
	}
	return 0
}

func truncate(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

func valueOr(value string, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func normalizeModelForBackend(backend BackendMode, model string) string {
	model = strings.TrimSpace(model)
	if backend != BackendRemote {
		return model
	}
	switch strings.ToLower(model) {
	case "":
		return ""
	case "kimi-k2.6":
		return "kmodel"
	case "minimax-m2.7":
		return "mmodel"
	case "qwen3-coder":
		return "dashscope_qwen3_coder"
	case "qwen3-max":
		return "dashscope_qwen_max_latest"
	case "qwen3-thinking":
		return "dashscope_qwen_plus_20250428_thinking"
	case "qwen3.6-plus":
		return "dashscope_qmodel"
	case "auto":
		return "org_auto"
	default:
		return model
	}
}

func remoteModelDisplayName(model string) string {
	switch normalizeModelForBackend(BackendRemote, model) {
	case "kmodel":
		return "Kimi-K2.6"
	case "mmodel":
		return "MiniMax-M2.7"
	default:
		return model
	}
}
