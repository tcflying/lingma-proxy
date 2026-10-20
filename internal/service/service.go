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
	"strconv"
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
	cfg             Config
	backendOnce     sync.Once
	mu              sync.Mutex
	connectMu       sync.Mutex // serialises the IPC handshake only; see ensureConnected
	client          *lingmaipc.Client
	pipePath        string
	endpoint        string
	transport       lingmaipc.Transport
	stickySessionID string
	stickyModelID   string
	modelMap        map[string]string // official name -> internal id
	remoteClient    *remote.Client
	cliClients      map[qodercli.Site]*qodercli.Client
	cliModels       map[qodercli.Site][]string
	cliCatalog      map[qodercli.Site]cliCatalogEntry
	// cliFlights dedupes catalog discovery per site: the first caller owns the
	// probe, late callers wait on the answer it publishes. Kept apart from mu so
	// a flight never delays /health or the other site's probe.
	cliFlightsMu sync.Mutex
	cliFlights   map[qodercli.Site]*cliCatalogFlight
	// cliProbeGate lets one catalog discovery run at a time. It is a gate, not a
	// queue: a caller that finds it held does not wait, because the holder may be
	// the startup warm-up, which is allowed the priming budget rather than the
	// request-path one, and a client must not inherit that. See cachedCLIModels
	// for who it serves, and listCLIMergedModels for the /v1/models caller that
	// deliberately does not take it.
	cliProbeGate     sync.Mutex
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
//
// Sites warm in parallel. Serially, one wedged CN spawn held the international
// site's warm-up hostage for the whole prime budget, and the two probes share
// nothing -- separate clients, separate catalogs -- so there is no ordering to
// preserve. First error wins for the fatal case, same as before.
func (s *Service) warmCLISites(ctx context.Context) error {
	sites := s.cliSites()
	type siteResult struct {
		err    error
		warmed bool
	}
	results := make([]siteResult, len(sites))
	var wg sync.WaitGroup
	for i, site := range sites {
		wg.Add(1)
		go func(i int, site qodercli.Site) {
			defer wg.Done()
			client, err := s.cliClientFor(site)
			if err == nil {
				err = client.Warmup(ctx)
			}
			if err != nil {
				log.Printf("backend: %s CLI warmup failed: %v", site.Label(), err)
				results[i].err = err
				return
			}
			s.cachedCLIModels(ctx, site, cliPrimeProbeTimeout)
			results[i].warmed = true
		}(i, site)
	}
	wg.Wait()
	var firstErr error
	warmed := false
	for _, res := range results {
		if res.err != nil && firstErr == nil {
			firstErr = res.err
		}
		warmed = warmed || res.warmed
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

// cliCatalogFlight is one catalog discovery shared by every caller that asked
// for the same site while it ran. The owner publishes names/err and then closes
// done; waiters read those fields only after the close, so no lock is needed on
// them.
type cliCatalogFlight struct {
	done  chan struct{}
	names []string
	err   error
}

// cliCatalogWaiterGrace is the window in which a waiter whose deadline fired
// still prefers the owner's answer if it lands at the same moment. A var so the
// race test can widen it; production only needs enough to cover the closing of
// done against the waiter's own deadline.
var cliCatalogWaiterGrace = 50 * time.Millisecond

// joinCLICatalogFlight returns the flight for site, creating it for the caller
// that must run the probe. owner is true only for that caller.
func (s *Service) joinCLICatalogFlight(site qodercli.Site) (*cliCatalogFlight, bool) {
	key := site.Normalized()
	s.cliFlightsMu.Lock()
	defer s.cliFlightsMu.Unlock()
	if flight, ok := s.cliFlights[key]; ok {
		return flight, false
	}
	if s.cliFlights == nil {
		s.cliFlights = map[qodercli.Site]*cliCatalogFlight{}
	}
	flight := &cliCatalogFlight{done: make(chan struct{})}
	s.cliFlights[key] = flight
	return flight, true
}

// finishCLICatalogFlight publishes the owner's answer and retires the entry so
// the next discovery starts fresh instead of joining a finished one.
func (s *Service) finishCLICatalogFlight(site qodercli.Site, flight *cliCatalogFlight) {
	s.cliFlightsMu.Lock()
	defer s.cliFlightsMu.Unlock()
	close(flight.done)
	delete(s.cliFlights, site.Normalized())
}

// cliListModels is the seam tests use to stand in for a real CLI probe.
var cliListModels = func(c *qodercli.Client, ctx context.Context) ([]remote.Model, error) {
	return c.ListModels(ctx)
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
	// The warm-up pass has no client waiting on it, so it is the one caller allowed
	// to outlast the two budgets above. It matters because those budgets sit at or
	// below what one probe costs on the slow box (36-47s when it is quiet there,
	// 149-218s when it is not): priming that inherits them never writes a cache, so
	// headless.log on 192.168.50.239 shows every discovery attempt ending in
	// "cancelled before it finished: context deadline exceeded" while the
	// cli-catalog-*.json files never appear at all. Raising the client-visible cap
	// instead would make a caller wait minutes for a list, which is its own outage.
	cliPrimeProbeTimeout = 15 * time.Minute
	// A probe that fails gets no cliCatalogTTL -- only a success does -- so without
	// this every /v1/models while a site is unreachable paid the full
	// cliProbeTimeout again: measured live, three consecutive calls at 8046/8028/
	// 8030ms each, all serving the same stale disk list. The failure TTL keeps the
	// stale answer instant for a short window and lets exactly one caller per
	// window pay the probe. Short on purpose: it only masks a dead site's list
	// refresh, never a chat turn, and recovery is one window away.
	cliProbeFailureTTL = 30 * time.Second
)

// CLIColdProbeTimeout publishes the budget a first, cache-less model discovery is
// allowed to spend. Callers that ask this same process for its model list must not
// give up sooner, or the UI shows an empty catalog while the backend is still
// working inside its own budget.
func CLIColdProbeTimeout() time.Duration { return cliColdProbeTimeout }

type cliCatalogEntry struct {
	names     []string
	expiresAt time.Time
	// failedAt is when the last probe against this site failed. It is only
	// consulted when names is non-nil: there is nothing to serve negatively for a
	// site that never answered, so that path keeps probing.
	failedAt time.Time
}

// persistedCLICatalog is one site's last good model list on disk. Without it a
// restart pays a full CLI cold start before it can answer /v1/models: on
// 192.168.50.239 the CLI's own logs show that cold start is the whole cost
// (uptime_ms=24799 and 46591 for --list-models runs whose actual model work was
// duration=1407ms catalog fetch + 2266ms runtime ready), and the first client
// after a restart was measured answering 500 at 120.7s
// (.scratch/s239_phases.txt, .scratch/s239_final2.txt). A seeded entry is also
// expired-but-usable, so the probe that follows gets the short warm budget and
// the caller gets the old list instead of an error.
type persistedCLICatalog struct {
	ExpiresAt time.Time `json:"expires_at"`
	Names     []string  `json:"names"`
}

// cliCatalogDir is the seam the tests redirect.
var cliCatalogDir = defaultCLICatalogDir

func defaultCLICatalogDir() string {
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "lingma-proxy")
}

func cliCatalogPath(site qodercli.Site) string {
	dir := strings.TrimSpace(cliCatalogDir())
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "cli-catalog-"+string(site.Normalized())+".json")
}

// readCLICatalog is best-effort: a missing or unreadable file is just no cache.
func readCLICatalog(site qodercli.Site) cliCatalogEntry {
	path := cliCatalogPath(site)
	if path == "" {
		return cliCatalogEntry{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cliCatalogEntry{}
	}
	var doc persistedCLICatalog
	if err := json.Unmarshal(data, &doc); err != nil || len(doc.Names) == 0 {
		return cliCatalogEntry{}
	}
	return cliCatalogEntry{names: doc.Names, expiresAt: doc.ExpiresAt}
}

// writeCLICatalog mirrors the same best-effort rule: the in-memory cache already
// answered this request, so a disk failure must not turn it into an error.
func writeCLICatalog(site qodercli.Site, names []string, expiresAt time.Time) {
	path := cliCatalogPath(site)
	if path == "" || len(names) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(persistedCLICatalog{ExpiresAt: expiresAt, Names: names})
	if err != nil {
		return
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		log.Printf("backend: %s CLI catalog could not be persisted to %s: %v", site.Label(), path, err)
	}
}

func (s *Service) cliCatalogEntry(site qodercli.Site) cliCatalogEntry {
	key := site.Normalized()
	s.mu.Lock()
	entry := s.cliCatalog[key]
	s.mu.Unlock()
	if entry.names != nil {
		return entry
	}
	// ponytail: reads the file on every miss (no disk list is only a failed Stat),
	// because promoting it into memory would claim a freshness the file does not have.
	return readCLICatalog(key)
}

func (s *Service) setCLICatalog(site qodercli.Site, names []string, expiresAt time.Time) {
	s.mu.Lock()
	if s.cliCatalog == nil {
		s.cliCatalog = map[qodercli.Site]cliCatalogEntry{}
	}
	// A success supersedes any recorded failure: the entry is replaced whole, so
	// failedAt goes back to zero with it.
	s.cliCatalog[site.Normalized()] = cliCatalogEntry{names: names, expiresAt: expiresAt}
	s.mu.Unlock()
	writeCLICatalog(site, names, expiresAt)
}

// markCLIProbeFailed records a failed probe without disturbing the stale names
// or the old expiry: the failure TTL in listCLIMergedModels is what reads it.
func (s *Service) markCLIProbeFailed(site qodercli.Site) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := site.Normalized()
	entry := s.cliCatalog[key]
	if entry.names == nil {
		// After a restart the only record of this site's names is the on-disk
		// tier, and the map lookup above cannot see it. Pull the disk entry in
		// so the mark has something to annotate -- otherwise a dead site
		// re-probes on every /v1/models forever, which is the exact tax the
		// mark exists to stop. Measured before this seeding: three consecutive
		// calls at 8.0s each against a TLS-dead CN, all serving stale names
		// that were already on disk.
		entry = readCLICatalog(key)
	}
	if entry.names == nil {
		return
	}
	entry.failedAt = time.Now()
	if s.cliCatalog == nil {
		s.cliCatalog = map[qodercli.Site]cliCatalogEntry{}
	}
	s.cliCatalog[key] = entry
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
//
// This is also why /v1/models does not take cliProbeGate, which is the other half
// of the story and used to read as a contradiction: that gate serves callers that
// have an answer in hand to fall back on, so a held gate costs them nothing, while
// a listing has nothing but the probe. Queueing it behind the warm-up's priming
// budget would turn "a second discovery" into "no answer at all", which is worse
// than the overlap.
//
// What stops the overlap is a shared result, not a lock: one cliCatalogFlight per
// site, where the first caller owns the probe and a late caller waits on the
// answer the owner publishes. The wait is bounded by the waiter's own budget, so
// a client never inherits the warm-up's priming budget, and only the owner writes
// the catalog or records a failure. The owner's probe is detached from its
// caller's context (context.WithoutCancel), so a client that hangs up cannot fail
// the callers still waiting on it.
//
// probeBudget raises the per-site bound above the client-facing ones; that is what
// the warm-up pass passes so priming can win on a box where one probe costs more
// than a client should ever wait. 0 keeps the request-path behaviour.
func (s *Service) listCLIMergedModels(ctx context.Context, probeBudget time.Duration) ([]Model, error) {
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
		// Negative cache: a probe that failed less than cliProbeFailureTTL ago
		// fails the same way again (the 8s was a TLS handshake, not a race), so
		// serve the stale list now and let the next window retry. Without this
		// a dead site taxed every /v1/models with the full probe timeout.
		if entry.names != nil && now.Before(entry.failedAt.Add(cliProbeFailureTTL)) {
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
			if probeBudget > timeout {
				timeout = probeBudget
			}
			flight, owner := s.joinCLICatalogFlight(site)
			if !owner {
				waitCtx, cancel := context.WithTimeout(ctx, timeout)
				defer cancel()
				accepted := func() {
					names := flight.names
					if flight.err != nil {
						names = stale
					}
					listings[i] = cliSiteListing{names: names, err: flight.err}
				}
				select {
				case <-flight.done:
					accepted()
				case <-waitCtx.Done():
					// done and the deadline can become ready together, and the
					// timeout branch then reported a stale error over an answer
					// the owner had already published. Re-check it, bounded by
					// the grace: the owner either lands right now or it did not.
					select {
					case <-flight.done:
						accepted()
					case <-time.After(cliCatalogWaiterGrace):
						// No probe of this caller's own ran, so no failure mark and no
						// duplicate log: the owner alone records those.
						listings[i] = cliSiteListing{names: stale, err: waitCtx.Err()}
					}
				}
				return
			}
			probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
			defer cancel()
			client, err := s.cliClientFor(site)
			var names []string
			if err == nil {
				var models []remote.Model
				if models, err = cliListModels(client, probeCtx); err == nil {
					for _, model := range models {
						if name := strings.TrimSpace(model.Key); name != "" {
							names = append(names, name)
						}
					}
				}
			}
			if err != nil {
				log.Printf("backend: %s CLI model discovery failed: %v", site.Label(), err)
				s.markCLIProbeFailed(site)
				flight.names = stale
			} else {
				s.setCLICatalog(site, names, time.Now().Add(cliCatalogTTL))
				flight.names = names
			}
			flight.err = err
			s.finishCLICatalogFlight(site, flight)
			listings[i].names = flight.names
			// Without the error a failed owner reads to mergeCLICatalogs as a
			// site that legitimately listed nothing, so a cold failure silently
			// shrinks the merged catalog instead of being reported.
			listings[i].err = err
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

// ListModels answers a model listing. On the CLI backend it discovers, which is
// the one request path that does not take cliProbeGate; the reason, and what
// bounds the resulting overlap instead, is on listCLIMergedModels.
func (s *Service) ListModels(ctx context.Context) ([]Model, error) {
	if s.backend() == BackendQoderCLI {
		return s.listCLIMergedModels(ctx, 0)
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
	// Count callbacks at the outer boundary, including deferred held output.
	emitted := false
	deliver := onDelta
	if onDelta != nil {
		deliver = func(event StreamEvent) {
			if event.Delta != "" {
				emitted = true
			}
			onDelta(event)
		}
	}
	result, err := s.generateLocked(ctx, req, deliver)
	if err == nil || emitted || ctx.Err() != nil || isTurnNoReplayError(err) || !isRecoverableIPCError(err) {
		return result, err
	}

	s.resetConnection()
	return s.generateLocked(ctx, req, deliver)
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
		} else if s.backend() != BackendRemote {
			return s.generateWithReconnect(ctx, req, onDelta)
		}
		// BackendRemote without tools: the gateway takes the pictures on the
		// request itself, through remoteImagesFromRequest below -- the image_urls
		// the is_vl switch in its own payload keys off. Routing this turn over
		// the local IPC pipe instead made vision depend on an installed desktop:
		// every image request on a headless deployment died in the IPC dial, and
		// the remote client's image projection stayed unreachable.
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
		// Resolution can discover the catalog (8s warm / 120s cold), and it runs
		// before attemptCtx below. Sharing the per-attempt budget here means a
		// configured timeout bounds discovery + turn together instead of letting
		// a cold resolution prefix every request with an unbudgeted 120s.
		resolveCtx, cancelResolve := contextWithOptionalTimeout(ctx, s.cfg.Timeout)
		req.Model = s.resolveCLIModel(resolveCtx, base, site)
		cancelResolve()
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
				// Echo the id the client asked for, not the bare name --model took,
				// prefixed by the site that actually served the turn (D05).
				result.Model = cliSiteEchoPrefix(site) + result.Model
			}
			return result, nil
		}
		lastErr = err
		if i == len(models)-1 || emitted || isTurnNoReplayError(err) || !isRemoteFallbackError(err) {
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

// firstAttemptStream keeps the first attempt of a turn a tool retry can still
// rewrite off the wire: bytes already handed to the client cannot be taken
// back, so the attempt is held until it is known final -- released when it
// survives every retry decision, dropped when a retry supersedes it. Both
// resolutions are idempotent, so a deferred release after an explicit drop is
// a no-op. Turns with no tools, or tool_choice:"none", never retry and keep
// the direct low-latency stream.
//
// The hold is bounded by cliFirstAttemptHoldMax: a tool turn can stay
// unresolved for many minutes (forced retries, emulation), and holding every
// delta for the whole turn measured as 19m47s of client silence while the CLI
// had already finished its answer. Once the timer fires the attempt is treated
// as released and later deltas stream directly.
type firstAttemptStream struct {
	onDelta func(StreamEvent)
	// mu covers every field below: the hold timer races the CLI delta goroutine
	// and the caller's post-turn reads.
	mu      sync.Mutex
	held    []StreamEvent
	holding bool
	wrote   bool
	// forwarded retains only the text the hold flush put on the wire -- at most
	// one hold bound of it -- while forwardedLen counts every text byte handed
	// out, so a stream that outgrows the hold is still sliced without keeping a
	// copy of the whole turn.
	forwarded    strings.Builder
	forwardedLen int
	timer        *time.Timer
	// carry is the text tail held back only to catch a DSML marker split
	// across deltas. suppressed latches once a marker is seen: everything after
	// it belongs to the native call the final parse will turn into a tool call,
	// so streaming it would show the user the tag flood the parse hides.
	carry      string
	suppressed bool
}

// cliFirstAttemptHoldMax bounds how long a tool-bearing first attempt may be
// held off the wire. A package var so tests can lower it.
var cliFirstAttemptHoldMax = 2500 * time.Millisecond

func newFirstAttemptStream(onDelta func(StreamEvent), req ChatRequest) *firstAttemptStream {
	h := &firstAttemptStream{
		onDelta: onDelta,
		holding: onDelta != nil && len(req.Tools) > 0 && req.ToolChoice.Mode != "none",
	}
	if h.holding {
		h.timer = time.AfterFunc(cliFirstAttemptHoldMax, h.flushHeld)
	}
	return h
}

func (h *firstAttemptStream) delta(text string) {
	h.event(StreamEvent{Type: StreamEventText, Delta: text})
}

func (h *firstAttemptStream) event(event StreamEvent) {
	if event.Delta == "" || h.onDelta == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.holding {
		h.forwardLocked(event)
		return
	}
	h.held = append(h.held, event)
}

// flushHeld is the timer callback: forward what the attempt has produced so far
// and stop holding, releasing the turn instead of keeping it silent.
func (h *firstAttemptStream) flushHeld() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.timer = nil
	if !h.holding {
		return
	}
	h.releaseLocked()
}

// releaseLocked forwards the held events and stops holding. The forwarding runs
// before holding drops so the flushed bytes stay in the tail emitter's prefix:
// a rewritten turn can still resolve after this flush. Callers hold mu.
func (h *firstAttemptStream) releaseLocked() {
	for _, event := range h.held {
		h.forwardLocked(event)
	}
	h.holding = false
	h.held = nil
}

// forwardLocked hands one delta to the client and records it for the tail
// emitter. Callers hold mu: event, releaseLocked and emit all call onDelta with
// the mutex held, and the tail emitter keeps that same pattern.
func (h *firstAttemptStream) forwardLocked(event StreamEvent) {
	// Only text deltas belong to the accepted text; thinking chunks are stream
	// output but never part of result.Text, so they would break prefix matching.
	if event.Type != StreamEventText {
		h.wrote = true
		h.onDelta(event)
		return
	}
	if h.suppressed {
		return
	}
	pending := h.carry + event.Delta
	h.carry = ""
	if idx := strings.Index(pending, toolemulation.DSMLMarker); idx >= 0 {
		// The client renders streamed deltas even when the final parse turns
		// this markup into a call, so nothing from the marker on may reach it.
		h.forwardTextLocked(pending[:idx])
		h.suppressed = true
		return
	}
	// Hold only the suffix that could still become the marker: that is what a
	// delta boundary can split, and nothing else stays behind. The carried
	// bytes are a byte prefix of the marker, so merging completes it whole;
	// release flushes an incomplete prefix as the prose it is.
	hold := len(pending) - longestMarkerPrefixSuffix(pending)
	h.carry = pending[hold:]
	h.forwardTextLocked(pending[:hold])
}

// longestMarkerPrefixSuffix returns the length of the longest proper prefix of
// the marker that ends text, or 0 when the next delta cannot complete one.
func longestMarkerPrefixSuffix(text string) int {
	for k := len(toolemulation.DSMLMarker) - 1; k > 0; k-- {
		if k <= len(text) && strings.HasSuffix(text, toolemulation.DSMLMarker[:k]) {
			return k
		}
	}
	return 0
}

// forwardTextLocked is the text half of forwardLocked. Every byte is counted,
// but only the held window is retained: once the hold is off the wire a long
// answer must not stay copied here until the turn ends (P2).
func (h *firstAttemptStream) forwardTextLocked(text string) {
	if text == "" {
		return
	}
	h.wrote = true
	h.forwardedLen += len(text)
	if h.holding {
		h.forwarded.WriteString(text)
	}
	h.onDelta(StreamEvent{Type: StreamEventText, Delta: text})
}

// stopTimerLocked disarms the hold timer. Callers hold mu.
func (h *firstAttemptStream) stopTimerLocked() {
	if h.timer != nil {
		h.timer.Stop()
		h.timer = nil
	}
}

func (h *firstAttemptStream) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopTimerLocked()
	if h.holding {
		h.releaseLocked()
	}
	h.flushCarryLocked()
}

// flushCarryLocked sends the marker peek's tail as prose. Called only where the
// attempt stops waiting for more deltas; a suppressed tail belongs to the call
// and is dropped.
func (h *firstAttemptStream) flushCarryLocked() {
	if h.carry != "" && !h.suppressed {
		h.forwardTextLocked(h.carry)
	}
	h.carry = ""
}

func (h *firstAttemptStream) drop() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopTimerLocked()
	h.holding = false
	h.held = nil
	h.carry, h.suppressed = "", false
}

// streamed reports whether a delta of this attempt has reached the client. It
// is read under the mutex because the hold timer may still be about to flush.
func (h *firstAttemptStream) streamed() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.wrote
}

// emitFinal hands the surviving attempt's final prose to the client once the
// held first attempt has been dropped: the attempt whose calls were accepted is
// the one the client must see, never the superseded one. An empty final text
// emits nothing, which keeps an all-blocks answer stream-silent on purpose.
func (h *firstAttemptStream) emitFinal(text string) {
	h.emit(text, false)
}

// emitFinalAccepted replays the accepted attempt's prose even when the hold
// bound already flushed the superseded attempt. The flushed preamble cannot be
// unsent; dropping the retry's text too left the client with only the wrong
// attempt (HTTP-07).
func (h *firstAttemptStream) emitFinalAccepted(text string) {
	h.emit(text, true)
}

// emitFinalTail hands the accepted text's unstreamed suffix to the client. A
// rewritten turn (the serial-tooling notice) keeps the same first attempt, so
// its prose may already be on the wire; only the suffix is owed, and emitFinal's
// all-or-nothing rule would drop that suffix once the hold bound flushed the
// preamble. A prefix that does not match means the wire text came from another
// attempt, so nothing is emitted, the same no-duplicate rule emitFinal keeps.
func (h *firstAttemptStream) emitFinalTail(text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopTimerLocked()
	h.holding = false
	h.held = nil
	h.carry = ""
	if text == "" || h.onDelta == nil {
		return
	}
	tail := text
	if h.wrote {
		sent := h.forwarded.String()
		if !strings.HasPrefix(text, sent) {
			// The mismatch means the text was rewritten after forwarding, so the
			// notice is accepted copy loss.
			return
		}
		if h.forwardedLen > len(text) {
			return
		}
		tail = text[h.forwardedLen:]
	}
	if tail == "" {
		return
	}
	h.forwardTextLocked(tail)
}

func (h *firstAttemptStream) emit(text string, force bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.stopTimerLocked()
	h.holding = false
	h.held = nil
	h.carry = ""
	// shortcut: a non-forced emit after the hold timer released this attempt
	// would duplicate text already on the wire, which is right only for a
	// rewritten first-attempt result; an accepted retry sets force.
	if text == "" || h.onDelta == nil || (h.wrote && !force) {
		return
	}
	h.forwardTextLocked(text)
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
) (result *ChatResult, emitted bool, err error) {
	hold := newFirstAttemptStream(onDelta, req)
	// Every exit resolves the hold first: an aborted attempt's partial prose is
	// real output the client keeps, and emitted must report the wire truth
	// after that release, not before.
	defer func() {
		hold.release()
		emitted = hold.streamed()
	}()
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
	}, hold.delta)
	if err != nil {
		return nil, hold.streamed(), err
	}
	// Dialect fallback for the native channel: the model answered a
	// native-tools request in the action-block dialect instead. The attempt is
	// finished and still held, so converting here reuses the emulated channel's
	// own acceptance gate before any retry is weighed -- a call the parser
	// accepts is already a legal native call and must not cost another round
	// trip, least of all the forced-tooling ones below.
	convertDialectToolCalls(remoteResult, req)
	superseded := false
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
		if retryErr == nil {
			// The forced retry answers the same turn under the same framing, so it
			// gets the same dialect fallback the first attempt had. A model that
			// obeys "call a tool" in the dialect the prompt taught it has produced a
			// legal call; dropping it would spend the whole round trip on an answer
			// the client could have run.
			convertDialectToolCalls(retryResult, req)
		}
		if retryErr == nil && len(retryResult.ToolCalls) > 0 {
			remoteResult = retryResult
			hold.drop()
			// The forced retry superseded the first attempt; its own prose is the
			// turn's accepted text and must reach the stream at the resolution
			// below, exactly like an emulation retry's (HTTP-07).
			superseded = true
		}
	}

	finishReason, stopReason := backendFinishReasons(remoteResult.StopReason)
	// Native calls echo the qualified names we declared, but never learned
	// their namespace; stamp it here so Responses emitters can split them
	// back into leaf plus namespace.
	toolemulation.DecorateToolCallNamespaces(remoteResult.ToolCalls, req.Tools)
	// The CLI backend serves no endpoint, so resolving the (empty) configured
	// base URL there would pay for a cold IDE config/log scan and discard it.
	endpoint := ""
	if s.backend() != BackendQoderCLI {
		endpoint = remote.ResolveBaseURL(s.cfg.RemoteBaseURL)
	}
	result = &ChatResult{
		Text:             remoteResult.Text,
		Model:            valueOr(strings.TrimSpace(model), "lingma"),
		InputTokens:      remoteResult.InputTokens,
		OutputTokens:     remoteResult.OutputTokens,
		SessionID:        "",
		RequestID:        remoteResult.RequestID,
		FinishReason:     finishReason,
		StopReason:       stopReason,
		Endpoint:         endpoint,
		Transport:        string(s.backend()),
		EffectiveSession: SessionModeFresh,
		ToolCalls:        remoteResult.ToolCalls,
	}
	if emulateTools {
		replaced, retryErr := s.applyToolEmulation(ctx, req, prompt, result, func(hintPrompt string) (string, int, bool, error) {
			retryResult, err := client.Chat(ctx, remote.ChatRequest{
				Model:           model,
				Prompt:          hintPrompt,
				System:          system,
				Messages:        remoteMessagesForChat(req, hintPrompt, emulateTools),
				Images:          remoteImagesFromRequest(req),
				Stream:          false,
				Temperature:     req.Temperature,
				ReasoningEffort: req.ReasoningEffort,
				Tools:           req.Tools,
				ToolChoice:      req.ToolChoice,
			}, nil)
			if err != nil {
				return "", 0, false, err
			}
			if len(retryResult.ToolCalls) > 0 {
				result.Text = retryResult.Text
				toolemulation.DecorateToolCallNamespaces(retryResult.ToolCalls, req.Tools)
				result.ToolCalls = retryResult.ToolCalls
				result.OutputTokens = retryResult.OutputTokens
				return "", 0, true, nil
			}
			return retryResult.Text, retryResult.OutputTokens, false, nil
		})
		if retryErr != nil {
			return nil, hold.streamed(), retryErr
		}
		// The contract runs before the hold resolves: a turn the serial-tooling
		// rule rewrites must not first stream the shape it no longer has.
		rewritten, cerr := acceptToolContract(req, result)
		if cerr != nil {
			return nil, hold.streamed(), cerr
		}
		if replaced || superseded {
			// A retry owns the turn: its prose must follow the superseded
			// attempt's held deltas, and also its already-flushed preamble --
			// emitFinal's no-duplicate rule would drop the retry's text there
			// (HTTP-07).
			hold.emitFinalAccepted(result.Text)
		} else if rewritten {
			// Same attempt, so only the notice acceptToolContract appended can
			// still be owed after the bound flushed the preamble.
			hold.emitFinalTail(result.Text)
		} else {
			hold.release()
		}
	} else {
		// The non-emulated channel (no tools, or tool_choice=none) never runs the
		// emulation gate above, but its native result still owes the contract: a
		// gateway that answers a none turn with a native call must be refused here.
		rewritten, cerr := acceptToolContract(req, result)
		if cerr != nil {
			return nil, hold.streamed(), cerr
		}
		if rewritten {
			hold.emitFinalTail(result.Text)
		} else {
			hold.release()
		}
	}
	return result, hold.streamed(), nil
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
		// A legal empty result is identified by its tool role and paired call ID.
		pairedToolResult := role == "tool" && strings.TrimSpace(message.ToolCallID) != ""
		if content == "" && len(message.Images) == 0 && len(message.ToolCalls) == 0 && !pairedToolResult {
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
// later request, so scanning all messages would blame the wrong turn. The
// current turn is the last user message plus the tool results that follow it:
// a tool_result attachment (HIST-05) is the turn's own screenshot, not history.
func currentTurnHasImages(req ChatRequest) bool {
	toolImages := false
	for i := len(req.Messages) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(req.Messages[i].Role))
		if role == "tool" {
			if len(remoteImagesFromChatMessage(req.Messages[i])) > 0 {
				toolImages = true
			}
			continue
		}
		if role == "user" {
			return toolImages || len(remoteImagesFromChatMessage(req.Messages[i])) > 0
		}
	}
	return toolImages
}

// requestWithCurrentTurnImagesOnly keeps the attachment on the turn being
// answered and strips the ones replayed from history. currentTurnHasImages names
// the same turn: the last user message, plus the tool results that trail it
// (their attachments are the current turn's, HIST-05), which is what the CLI
// receives.
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
		if i != keep && !(i > keep && strings.EqualFold(strings.TrimSpace(message.Role), "tool")) {
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
// is looked for in every path segment rather than only the first. The published
// `cn/` marker pins the CN site (D05): a marked request never cross-falls back
// to the other site, and the marker is consumed out of the id --model takes.
func splitCLISite(model string) (qodercli.Site, string, bool) {
	trimmed := strings.TrimSpace(model)
	segments := strings.Split(trimmed, "/")
	for i, segment := range segments {
		switch strings.ToLower(strings.TrimSpace(segment)) {
		case "cn", "china", "中国", "国内":
			rest := strings.TrimSpace(strings.Join(segments[i+1:], "/"))
			if rest != "" {
				return qodercli.SiteCN, rest, true
			}
		case "intl", "global", "国际", "国际版":
			rest := strings.TrimSpace(strings.Join(segments[i+1:], "/"))
			if rest != "" {
				return qodercli.SiteGlobal, rest, true
			}
		}
	}
	return qodercli.SiteCN, trimmed, false
}

// cliSiteEchoPrefix is the marker a named-site request's result model is echoed
// with, by the site that actually served it -- not a single global prefix.
func cliSiteEchoPrefix(site qodercli.Site) string {
	if site.Normalized() == qodercli.SiteGlobal.Normalized() {
		return cliGlobalPrefix
	}
	return "cn/"
}

// resolveCLIModel maps whatever the client asked for onto a model the signed-in
// account of that site actually exposes.
func (s *Service) resolveCLIModel(ctx context.Context, model string, site qodercli.Site) string {
	wanted := strings.TrimSpace(model)
	if alias, ok := cliModelAliases[strings.ToLower(wanted)]; ok {
		wanted = alias
	}
	known := s.cachedCLIModels(ctx, site, 0)
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
//
// Concurrency here is a gate rather than a lock, and that is deliberate. The
// comment on listCLIMergedModels rules out a mutex around the probe itself: the
// holder can sit in the child's wait for its whole budget, and every queued caller
// then times out with it. This is the narrow version of that idea -- one
// discovery at a time, no waiting -- because a miss is not cheap. Each listing is
// a CLI subprocess, measured at 24-46s of cold start, so N callers missing at the
// same instant otherwise become N runtimes competing for the same machine.
//
// The wait is a TryLock on purpose. Queuing instead would hand a client request
// the warm-up's priming budget, which is the exact coupling the note above
// exists to prevent; answering from whatever the previous run left is both faster
// and no less truthful, and the caller that skipped the probe is the one that
// already has a catalog in hand or has none either way.
func (s *Service) cachedCLIModels(ctx context.Context, site qodercli.Site, probeBudget time.Duration) []string {
	read := func() []string {
		s.mu.Lock()
		defer s.mu.Unlock()
		return append([]string(nil), s.cliModels[site.Normalized()]...)
	}
	if cached := read(); len(cached) > 0 {
		return cached
	}
	if !s.cliProbeGate.TryLock() {
		return read()
	}
	defer s.cliProbeGate.Unlock()
	// Re-read under the gate: whoever held it has very likely just filled the
	// cache, and listing again would spend a whole cold start to learn what the
	// previous run already wrote down.
	if cached := read(); len(cached) > 0 {
		return cached
	}
	if _, err := s.listCLIMergedModels(ctx, probeBudget); err != nil {
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
	if effectiveMode == SessionModeReuse && requestCarriesToolTraffic(req) {
		// E-1 (HIST-03): the reuse shortcut sends only the last converted user
		// message, which cannot carry parallel tool results or the current tool
		// constraints. Tool-carrying turns route through the existing fresh full
		// replay instead; pure-text single-message continuation keeps reuse.
		effectiveMode = SessionModeFresh
	}
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

	// Same isolation as the remote path: the emulation retry below can rewrite
	// this turn; the deferred release keeps an aborted turn's partial prose.
	hold := newFirstAttemptStream(onDelta, req)
	defer hold.release()
	runResult, err := s.runPromptLocked(requestCtx, ipcClient, sessionID, prompt, images, requestID, meta, hold.event)
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

	replaced, retryErr := s.applyToolEmulation(requestCtx, req, prompt, result, func(hintPrompt string) (string, int, bool, error) {
		retryRequestID := lingmaipc.CreateRequestID("serve-tool")
		retryMeta := lingmaipc.CreateMeta(lingmaipc.MetaOptions{
			RequestID:       retryRequestID,
			Mode:            s.cfg.Mode,
			Model:           internalModelID,
			ShellType:       s.cfg.ShellType,
			CurrentFilePath: s.cfg.CurrentFilePath,
			EnabledMCP:      []any{},
		})
		// nil onDelta: the held first attempt is still awaiting its verdict, and
		// result.Text is about to be replaced by this retry. The remote retry
		// passes nil too. The IPC peer answers in action blocks, never native
		// calls, so this callback never reports a native replacement; parsed
		// retry text still sets the caller's replaced.
		retryRunResult, retryErr := s.runPromptLocked(requestCtx, ipcClient, sessionID, hintPrompt, images, retryRequestID, retryMeta, nil)
		if retryErr != nil {
			return "", 0, false, retryErr
		}
		return retryRunResult.AssistantText, estimateTokens(retryRunResult.AssistantText), false, nil
	})
	if retryErr != nil {
		abandonTurn()
		return nil, retryErr
	}
	// Same acceptance gate the remote channel pays, before the hold resolves so
	// a rewritten turn never streams the shape it no longer has.
	rewritten, cerr := acceptToolContract(req, result)
	if cerr != nil {
		abandonTurn()
		return nil, cerr
	}
	if replaced {
		// HTTP-07 parity with the remote channel: the accepted retry's prose
		// replaces the dropped first attempt on the stream.
		hold.emitFinalAccepted(result.Text)
	} else if rewritten {
		// The same IPC attempt, rewritten by acceptToolContract: only its notice
		// tail is still owed once the bound flushed the preamble.
		hold.emitFinalTail(result.Text)
	} else {
		hold.release()
	}
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

// applyToolEmulation parses action blocks out of a finished attempt and, when
// the turn still owes a tool call, retries with a forced-tooling prompt. It
// reports whether the turn's prose ended up replaced by a retried attempt or
// consumed into inferred calls: that is the signal streaming callers use to
// drop the first attempt's held deltas, so the client only ever sees output
// that belongs to the accepted attempt. The tool_choice contract itself is
// enforced one level up, at acceptToolContract, so every channel pays the same
// acceptance gate whether or not it ran an emulation here.
func (s *Service) applyToolEmulation(
	ctx context.Context,
	req ChatRequest,
	prompt string,
	result *ChatResult,
	retry func(string) (string, int, bool, error),
) (replaced bool, err error) {
	// tool_choice:"none" is the client forbidding tool calls, so a block that
	// arrives anyway (from an echoed example, or from history) stays prose:
	// turning it into a tool call would hand back an action the client ruled out.
	if len(req.Tools) > 0 && req.ToolChoice.Mode != "none" {
		// The merge gate: a turn that already carries calls -- native ones from
		// the gateway, or dialect ones converted upstream of here -- counts them
		// against the same per-turn ceiling the parser enforces, so the residual
		// text of an already-converted attempt cannot buy a second, reset
		// budget: what the first parse suppressed stays prose (HTTP-08).
		merged := false
		if budget := turnToolCallCap() - len(result.ToolCalls); budget > 0 {
			calls, remaining, parseErr := toolemulation.ParseActionBlocks(result.Text, req.Tools, toolemulation.Config{MaxToolCalls: budget})
			if parseErr == nil && len(calls) > 0 {
				result.Text = remaining
				// The parsed calls join behind the ones already carried, so a
				// model echoing its own native call as text cannot make it
				// execute twice.
				result.ToolCalls = appendConvertedToolCalls(result.ToolCalls, calls)
				merged = true
			}
		}
		if !merged && len(result.ToolCalls) == 0 && shouldRetryTooling(req.ToolChoice, result.Text) {
			hintPrompt := prompt + "\n\n" + toolemulation.ForceToolingPrompt(req.ToolChoice)
			retryText := ""
			if retry != nil {
				// replaced means the retry already installed its own calls into
				// result (a native-tool answer needs no action-block parsing).
				text, outputTokens, replaced, retryErr := retry(hintPrompt)
				if retryErr != nil {
					return false, &turnNoReplayError{cause: fmt.Errorf("tool emulation retry failed: %w", retryErr)}
				}
				if replaced {
					return true, nil
				}
				retryText = text
				if outputTokens > 0 {
					result.OutputTokens = outputTokens
				}
			}
			if retryText != "" {
				retryCalls, retryRemaining, retryParseErr := toolemulation.ParseActionBlocks(retryText, req.Tools, toolemulation.Config{})
				if retryParseErr == nil && len(retryCalls) > 0 {
					result.Text = retryRemaining
					result.ToolCalls = retryCalls
					result.OutputTokens = estimateTokens(retryText)
					return true, nil
				} else if inferred := toolemulation.InferToolCallsFromText(retryText, req.Tools); len(inferred) > 0 {
					result.Text = ""
					result.ToolCalls = inferred
					result.OutputTokens = estimateTokens(retryText)
					return true, nil
				}
			}
			if len(result.ToolCalls) == 0 {
				if inferred := toolemulation.InferToolCallsFromText(result.Text, req.Tools); len(inferred) > 0 {
					result.Text = ""
					result.ToolCalls = inferred
					return true, nil
				}
			}
		}
	}
	return false, nil
}

// acceptToolContract is the common acceptance gate a finished turn passes
// through before Generate hands it to the API layer: the client's tool_choice
// and parallel_tool_calls selections constrain the ACCEPTED result, not only
// the prompt. A named choice must be matched by every accepted call, a
// required choice owes at least one, and none owes none -- native remote calls
// included. The existing forced-tooling retry stays the one retry layer; when
// it has not restored the contract the turn fails loudly here instead of
// silently succeeding with something the client never asked for. A turn that
// forbids parallel calls keeps only the first; the dropped ones become visible
// prose (rewritten=true) so the client can still see what was held back.
func acceptToolContract(req ChatRequest, result *ChatResult) (rewritten bool, err error) {
	if len(req.Tools) == 0 {
		return false, nil
	}
	switch req.ToolChoice.Mode {
	case "none":
		if len(result.ToolCalls) > 0 {
			return false, fmt.Errorf("tool_choice=none forbids tool calls this turn, but the backend returned %d; refusing to accept the turn", len(result.ToolCalls))
		}
	case "tool":
		if len(result.ToolCalls) == 0 {
			return false, fmt.Errorf("tool_choice requires %q but no tool call was returned after the forced-tooling retry; refusing a call-free turn", strings.TrimSpace(req.ToolChoice.Name))
		}
		if name := strings.TrimSpace(req.ToolChoice.Name); name != "" {
			for _, call := range result.ToolCalls {
				if !strings.EqualFold(toolemulation.NormalizeDeclaredName(req.Tools, call.Name), toolemulation.NormalizeDeclaredName(req.Tools, name)) {
					return false, fmt.Errorf("tool_choice requires %q but the model returned %q; refusing a different tool", name, call.Name)
				}
			}
		}
	case "any":
		if len(result.ToolCalls) == 0 {
			return false, errors.New("tool_choice=required but no tool call was returned after the forced-tooling retry; refusing a call-free turn")
		}
	}
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls && len(result.ToolCalls) > 1 {
		dropped := append([]toolemulation.ToolCall(nil), result.ToolCalls[1:]...)
		result.ToolCalls = result.ToolCalls[:1]
		note := toolemulation.SingleCallNotice(dropped)
		if strings.TrimSpace(result.Text) == "" {
			result.Text = note
		} else {
			result.Text = result.Text + "\n" + note
		}
		return true, nil
	}
	return false, nil
}

// turnToolCallCap mirrors the parser's per-turn call ceiling for the merge
// gate in applyToolEmulation: the calls a turn already carries count against
// the same budget the parser itself enforces, environment knob included.
func turnToolCallCap() int {
	if raw := strings.TrimSpace(os.Getenv("LINGMA_MAX_TOOL_CALLS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return n
		}
	}
	return 5
}

func shouldRetryTooling(choice toolemulation.ToolChoice, text string) bool {
	switch choice.Mode {
	case "any", "tool":
		return true
	case "none":
		return false
	}
	// auto, and the unset mode that means the same thing on the wire. Only an
	// explicit refusal earns the extra round trip here. LooksLikeMissedToolUse is
	// a needle list of narration words ("I will read ...", "让我尝试"), so an
	// ordinary answer that describes what the model is about to do used to buy a
	// second full upstream turn in the mode most clients send.
	return toolemulation.LooksLikeRefusal(text)
}

// appendConvertedToolCalls appends dialect-converted calls after the ones the
// turn already carries, dropping a converted call that repeats an existing one
// by name and arguments: that is the model echoing a call it already made, not
// a second invocation.
func appendConvertedToolCalls(existing, converted []toolemulation.ToolCall) []toolemulation.ToolCall {
	out := existing
	for _, call := range converted {
		duplicate := false
		for _, have := range existing {
			if have.Name == call.Name && toolArgumentsEqual(have.Arguments, call.Arguments) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			out = append(out, call)
		}
	}
	return out
}

// toolArgumentsEqual compares two argument maps by their canonical JSON:
// encoding/json writes map keys sorted, so the bytes are stable.
func toolArgumentsEqual(a, b map[string]any) bool {
	ab, errA := json.Marshal(normalizeToolArguments(a))
	bb, errB := json.Marshal(normalizeToolArguments(b))
	if errA != nil || errB != nil {
		// A map that does not marshal is not one this proxy can compare, and two
		// undecidable maps are not an echo of each other. Answering "same" here
		// would drop a call the client asked for.
		return false
	}
	return string(ab) == string(bb)
}

// normalizeToolArguments gives a nil map and an empty one the same shape. A
// native call with no parameters arrives as {}, a dialect block that omits
// "parameters" arrives as nil, and encoding/json writes those as {} and null:
// without this the same no-argument call read as two and the tool ran twice.
func normalizeToolArguments(args map[string]any) map[string]any {
	if args == nil {
		return map[string]any{}
	}
	return args
}

// convertDialectToolCalls is the native channel's dialect fallback, applied to
// one finished attempt: a native-tools request the model answered in the
// action-block dialect comes back as a legal native call, with the block cut out
// of the prose. The gate is the one applyToolEmulation uses, so anything this
// accepts is a call the emulated channel would have accepted too.
func convertDialectToolCalls(result *remote.ChatResult, req ChatRequest) {
	if result == nil || len(result.ToolCalls) > 0 {
		return
	}
	if len(req.Tools) == 0 || req.ToolChoice.Mode == "none" {
		return
	}
	calls, remaining, err := toolemulation.ParseActionBlocks(result.Text, req.Tools, toolemulation.Config{})
	if err != nil || len(calls) == 0 {
		return
	}
	result.ToolCalls = calls
	result.Text = remaining
}

// Failed retries or interrupted partial execution cannot replay the whole turn.
type turnNoReplayError struct{ cause error }

func (e *turnNoReplayError) Error() string { return e.cause.Error() }
func (e *turnNoReplayError) Unwrap() error { return e.cause }
func isTurnNoReplayError(err error) bool {
	var failure *turnNoReplayError
	return errors.As(err, &failure)
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
			return result, &turnNoReplayError{cause: fmt.Errorf("Lingma IPC response incomplete before finish: %w; partial reply: %s", ctx.Err(), truncate(result.AssistantText, 120))}
		case notification, ok := <-notifications:
			if !ok {
				result.AssistantText = builder.String()
				result.ThoughtText = thoughtBuilder.String()
				err := fmt.Errorf("Lingma IPC notification stream closed before finish; partial reply: %s", truncate(result.AssistantText, 120))
				// Partial execution cannot be replayed, including aggregate/nonstream callers.
				if builder.Len() > 0 || thoughtBuilder.Len() > 0 {
					return result, &turnNoReplayError{cause: err}
				}
				return result, err
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

// writeImageTempFile spools one attachment. It is a var so a test can reproduce
// the only way this branch can actually fail in production -- a full or
// read-only temp directory -- which no fixture can otherwise provoke.
var writeImageTempFile = os.WriteFile

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
		imageURI = spoolImageTempFile(imageScheme, mediaType, img.Data)
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

// spoolImageTempFile writes the attachment for the one host that resolves
// agent/file, and returns that host's uri. Every failure here used to be
// swallowed: the empty file CreateTemp had just made was still referenced, the
// reader came back with nothing, and the request looked like the picture had
// been ignored. So a failure leaves no uri and says so once in the log; the
// inline data field, which that host also reads, is untouched.
func spoolImageTempFile(imageScheme, mediaType, encoded string) string {
	tmpFile, err := os.CreateTemp("", "lingma-img-*"+imageExtension(mediaType))
	if err != nil {
		log.Printf("image: spooling an attachment for %s failed, sending the inline data only: %v", imageScheme, err)
		return ""
	}
	tmpPath := tmpFile.Name()
	_ = tmpFile.Close()

	data, decodeErr := base64.StdEncoding.DecodeString(encoded)
	if decodeErr != nil {
		log.Printf("image: decoding an inline attachment failed, sending it without a file reference: %v", decodeErr)
		_ = os.Remove(tmpPath)
		return ""
	}
	if len(data) == 0 {
		_ = os.Remove(tmpPath)
		return ""
	}
	if writeErr := writeImageTempFile(tmpPath, data, 0600); writeErr != nil {
		log.Printf("image: writing %s failed, sending the inline data only: %v", tmpPath, writeErr)
		_ = os.Remove(tmpPath)
		return ""
	}
	absPath, absErr := filepath.Abs(tmpPath)
	if absErr != nil {
		log.Printf("image: resolving %s failed, sending the inline data only: %v", tmpPath, absErr)
		_ = os.Remove(tmpPath)
		return ""
	}
	return fmt.Sprintf("%s:///agent/file?path=%s", imageScheme, url.QueryEscape(absPath))
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

// requestCarriesToolTraffic reports whether the turn carries tool results or
// tool constraints that the IPC reuse shortcut (single last converted user
// message) cannot express. 1005 E-1 COORD ruling: such requests always replay
// the full conversation through the fresh path, even when reuse is configured.
func requestCarriesToolTraffic(req ChatRequest) bool {
	if len(req.Tools) > 0 {
		return true
	}
	for _, message := range req.Messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role == "tool" || len(message.ToolCalls) > 0 || strings.TrimSpace(message.ToolCallID) != "" {
			return true
		}
	}
	return false
}

// extractLastUserImages returns the attachment set for the turn being answered.
// Trailing tool results participate (HIST-05): a screenshot tool's continuation
// turn carries its image on the tool message, and skipping role "tool" here
// made the IPC prompt items lose exactly those attachments.
func extractLastUserImages(messages []ChatMessage) []Image {
	for i := len(messages) - 1; i >= 0; i-- {
		role := strings.ToLower(strings.TrimSpace(messages[i].Role))
		if (role == "user" || role == "tool") && len(messages[i].Images) > 0 {
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
			parts = append(parts, fmt.Sprintf("%s: %s", role, transcriptText(message)))
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
		parts = append(parts, fmt.Sprintf("%s: %s", role, transcriptText(message)))
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
		if role == "tool" {
			// An empty tool result still belongs to the call that produced it:
			// dropping it breaks the pairing and the model re-reads the file.
			body := strings.TrimSpace(message.Text)
			if body == "" {
				body = "(empty tool output)"
			}
			text = toolemulation.ActionOutputPrompt(message.ToolCallID, body)
			role = "user"
		}
		// A completed assistant tool call usually has no Text at all, so the
		// call list alone keeps the message alive.
		if text == "" && len(message.ToolCalls) == 0 {
			continue
		}
		if role != "user" && role != "assistant" {
			continue
		}
		out = append(out, ChatMessage{Role: role, Text: text, ToolCalls: message.ToolCalls, ToolCallID: message.ToolCallID})
	}
	return out
}

// renderHistoryActionBlock replays one completed assistant tool call in the
// action-block dialect the tooling prompt itself teaches, so the model sees
// its own past turns the way it produced them. The call id and the
// already-executed marker sit outside the fenced JSON: the parser's dialect
// stays {tool, parameters} only, so an echoed block can never smuggle the id
// into tool arguments. History replay only: arguments are serialized
// verbatim, never re-derived or deduplicated -- a client's intentional repeat
// call keeps its own block and id.
func renderHistoryActionBlock(call toolemulation.ToolCall) string {
	args := call.Arguments
	if args == nil {
		args = map[string]any{}
	}
	block := map[string]any{
		"tool":       call.Name,
		"parameters": args,
	}
	b, err := json.Marshal(block)
	if err != nil {
		return ""
	}
	header := ""
	if id := strings.TrimSpace(call.ID); id != "" {
		header = "Completed tool call " + id + " (already executed, its result follows below; do not repeat or re-run it):\n"
	}
	return header + "```json action\n" + string(b) + "\n```"
}

// transcriptText renders one history message for the Lingma prompt: an
// assistant turn that made calls carries them as action blocks after any
// prose, mirroring what the model itself would have emitted.
func transcriptText(message ChatMessage) string {
	if len(message.ToolCalls) == 0 {
		return message.Text
	}
	blocks := make([]string, 0, len(message.ToolCalls))
	for _, call := range message.ToolCalls {
		if block := renderHistoryActionBlock(call); block != "" {
			blocks = append(blocks, block)
		}
	}
	joined := strings.Join(blocks, "\n")
	if message.Text == "" {
		return joined
	}
	return message.Text + "\n\n" + joined
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
