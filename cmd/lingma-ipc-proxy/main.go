package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lingma-ipc-proxy/internal/deploy"
	"lingma-ipc-proxy/internal/httpapi"
	"lingma-ipc-proxy/internal/lingmaipc"
	"lingma-ipc-proxy/internal/remote"
	"lingma-ipc-proxy/internal/service"
)

var utilityOptions struct {
	exportRemoteAuth   string
	exportServerBundle string
	remoteAuthPick     string
}

type fileConfig struct {
	Host                  string   `json:"host"`
	Port                  int      `json:"port"`
	Backend               string   `json:"backend"`
	Transport             string   `json:"transport"`
	Pipe                  string   `json:"pipe"`
	WebSocketURL          string   `json:"websocket_url"`
	RemoteBaseURL         string   `json:"remote_base_url"`
	RemoteAuthFile        string   `json:"remote_auth_file"`
	RemoteProxyURL        string   `json:"remote_proxy_url"`
	RemoteVersion         string   `json:"remote_version"`
	Cwd                   string   `json:"cwd"`
	CurrentFilePath       string   `json:"current_file_path"`
	Mode                  string   `json:"mode"`
	Model                 string   `json:"model"`
	ShellType             string   `json:"shell_type"`
	SessionMode           string   `json:"session_mode"`
	TimeoutSeconds        int      `json:"timeout"`
	RemoteFallbackEnabled *bool    `json:"remote_fallback_enabled"`
	RemoteFallbackModels  []string `json:"remote_fallback_models"`
	QoderCLISites         []string `json:"qodercli_sites"`
}

// qodercliTurnCeilingEnv is where internal/qodercli reads the CLI turn backstop
// from. The name is repeated here rather than imported because that constant is
// unexported and belongs to the package that applies the deadline; this entry
// only names it so the flag has something to hand over and -h has something to
// point at.
const qodercliTurnCeilingEnv = "LINGMA_QODERCLI_TURN_CEILING"

// qodercliTurnCeilingUsage documents the backstop at the place an operator
// actually looks. The line that matters is the last one: with -timeout 0 there
// is no proxy deadline at all, and the ceiling is what stops a wedged CLI child
// from holding one of the backend's few concurrency slots forever. It stays a
// separate setting rather than a shorter -timeout, so an operator who asked for
// an hour of budget is not cut off at half an hour by a safety net.
const qodercliTurnCeilingUsage = "Backstop ceiling for one Qoder CLI turn, as a Go duration (e.g. 20m; 0 switches the backstop off). " +
	"It only applies when -timeout is 0, and then replaces nothing: an operator who asked for a deadline still gets exactly the one they asked for. " +
	"The CLI backend reads it from " + qodercliTurnCeilingEnv + ", which this flag sets."

// headlessLog takes the log off the console. Task Scheduler starts this build
// inside an interactive console, and a console that stops servicing writes (a
// quick-edit selection, a session whose window station went away) makes
// WriteFile block rather than fail: measured on 192.168.50.239, the exe started
// by the task sat at 0.12 s of total CPU and never bound its port, while the same
// bytes started with stderr pointed at a file bound within 10 s and had already
// logged five lines. The startup logs run before net.Listen, so a blocked write
// there produces exactly the failure this box kept showing -- process alive, no
// listener, nothing to read afterwards. So the log goes to
// <UserConfigDir>\lingma-proxy\headless.log, and anyone watching it uses
// `Get-Content -Wait`.
func headlessLog() {
	if runtime.GOOS != "windows" {
		return
	}
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = os.TempDir()
	}
	dir := filepath.Join(base, "lingma-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	path := filepath.Join(dir, "headless.log")
	// One generation of history is enough to answer "what did it do on boot", and
	// a service nobody restarts must not grow without bound.
	if fi, err := os.Stat(path); err == nil && fi.Size() > 5<<20 {
		_ = os.Remove(path + ".old")
		_ = os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	log.SetOutput(f)
	log.Printf("log file: %s", path)
}

func main() {
	headlessLog()
	cfg, configPath := loadConfig()
	if handleUtilityCommands(cfg) {
		return
	}
	addr := fmt.Sprintf("%s:%d", cfg.Host, cfg.Port)

	service.SweepImageTemps()
	svc := service.New(cfg)
	server := httpapi.NewServer(addr, svc)

	log.Printf("lingma-proxy listening on http://%s", addr)
	log.Printf("session mode: %s", cfg.SessionMode)
	log.Printf("transport: %s", cfg.Transport)
	log.Printf("mode: %s", cfg.Mode)
	if configPath != "" {
		log.Printf("config file: %s", configPath)
	}
	// The backstop's default lives inside internal/qodercli, so this reports the
	// override in force rather than a number copied here that could drift out of
	// step with it. Silence means the CLI backend's own default is in effect.
	if ceiling := strings.TrimSpace(os.Getenv(qodercliTurnCeilingEnv)); ceiling != "" {
		log.Printf("qodercli turn ceiling: %s", ceiling)
	}

	errCh := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	// Warm up only once the port is open, and never inside a budget a client could
	// be waiting on. Minting a CLI job token spawns the desktop runtime, and os/exec
	// can go on waiting for a grandchild that inherited the output pipe after the
	// context has killed the direct child -- warming before the listener meant such
	// a wedged child silently left the proxy with no port at all.
	//
	// The goroutine keeps the port open to clients either way: the point of this
	// pass is to land the per-site catalog on disk (internal/service's
	// cliPrimeProbeTimeout bounds each probe), and a request that arrives while it
	// runs is served from the persisted catalog or fails fast, never queued here.
	// The 10 s this used to carry was measured to lose on 192.168.50.239, where one
	// discovery costs 36-47s at best.
	go func() {
		warmupCtx, warmupCancel := context.WithTimeout(context.Background(), 45*time.Minute)
		defer warmupCancel()
		if err := svc.Warmup(warmupCtx); err != nil {
			log.Printf("warmup failed: %v", err)
		} else {
			log.Printf("Lingma IPC warmup completed")
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		log.Fatal(err)
	case sig := <-sigCh:
		log.Printf("received %s, shutting down", sig.String())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatal(err)
	}
}

func loadConfig() (service.Config, string) {
	cfg := service.Config{
		Host:                  "127.0.0.1",
		Port:                  8095,
		Backend:               service.BackendRemote,
		Transport:             lingmaipc.TransportAuto,
		Cwd:                   currentDir(),
		Mode:                  "agent",
		Model:                 "kmodel",
		ShellType:             defaultShellType(),
		SessionMode:           service.SessionModeAuto,
		Timeout:               0,
		RemoteFallbackEnabled: true,
		RemoteFallbackModels:  service.DefaultRemoteFallbackModels(),
	}

	configPath, configLoaded := resolveConfigPath()
	if configLoaded {
		fileCfg, err := readFileConfig(configPath)
		if err != nil {
			log.Fatalf("load config file %q: %v", configPath, err)
		}
		overlayFileConfig(&cfg, fileCfg)
	}

	overlayEnvConfig(&cfg)

	host := flag.String("host", cfg.Host, "Listen host")
	port := flag.Int("port", cfg.Port, "Listen port")
	transport := flag.String("transport", string(cfg.Transport), "Lingma/QoderCN transport: auto, pipe, websocket")
	backend := flag.String("backend", string(cfg.Backend), "Backend mode: ipc, remote or qodercli")
	pipe := flag.String("pipe", cfg.Pipe, "Explicit Lingma/QoderCN IPC socket or named pipe path")
	wsURL := flag.String("ws-url", cfg.WebSocketURL, "Explicit Lingma/QoderCN local websocket URL")
	remoteBaseURL := flag.String("remote-base-url", cfg.RemoteBaseURL, "Remote Lingma/QoderCN API base URL")
	remoteAuthFile := flag.String("remote-auth-file", cfg.RemoteAuthFile, "Remote Lingma/QoderCN credentials.json path; empty reads local login cache")
	remoteProxyURL := flag.String("remote-proxy-url", cfg.RemoteProxyURL, "Explicit proxy URL for Remote API requests, e.g. http://127.0.0.1:7890")
	remoteVersion := flag.String("remote-version", cfg.RemoteVersion, "Remote Lingma/QoderCN cosy version")
	cwd := flag.String("cwd", cfg.Cwd, "Working directory used when creating Lingma/QoderCN sessions")
	currentFilePath := flag.String("current-file-path", cfg.CurrentFilePath, "Current file path sent through ACP meta")
	mode := flag.String("mode", cfg.Mode, "Lingma/QoderCN ACP mode value")
	model := flag.String("model", cfg.Model, "Default Lingma/QoderCN model when API request omits model")
	shellType := flag.String("shell-type", cfg.ShellType, "Shell type sent through ACP meta")
	timeoutSeconds := flag.Int("timeout", int(cfg.Timeout/time.Second), "Per-request timeout in seconds; 0 disables the proxy deadline (the Qoder CLI backend then bounds a turn by -qodercli-turn-ceiling instead)")
	remoteFallbackEnabled := flag.Bool("remote-fallback", cfg.RemoteFallbackEnabled, "Enable remote timeout/5xx fallback to the next available model")
	remoteFallbackModels := flag.String("remote-fallback-models", strings.Join(cfg.RemoteFallbackModels, ","), "Comma-separated remote fallback model IDs")
	qodercliSites := flag.String("qodercli-sites", strings.Join(cfg.QoderCLISites, ","), "Qoder sites the CLI backend serves: cn, global (empty serves both)")
	exportRemoteAuth := flag.String("export-remote-auth", "", "Export portable Remote API credentials.json to the given path and exit")
	exportServerBundle := flag.String("export-server-bundle", "", "Export a server deployment zip containing credentials.json, config, and docker-compose.yml, then exit")
	remoteAuthPick := flag.String("remote-auth-pick", "auto", "Remote login cache pick policy for export: auto, newest, or longest")
	sessionMode := flag.String("session-mode", string(cfg.SessionMode), "Session mode: auto, fresh, reuse")
	config := flag.String("config", valueOr(configPath, filepath.Join(currentDir(), "lingma-proxy.json")), "Path to JSON config file")
	qodercliTurnCeiling := flag.String("qodercli-turn-ceiling", os.Getenv(qodercliTurnCeilingEnv), qodercliTurnCeilingUsage)
	flag.Parse()

	// The turn backstop is the one setting this entry does not carry in
	// service.Config: it bounds a Qoder CLI subprocess turn rather than a proxy
	// request, and the package that owns the subprocess is the one that applies
	// it. It is read from the environment on every turn, so handing the value
	// over here is enough -- and only an operator who actually passed the flag
	// writes anything, leaving every other startup byte-for-byte unchanged.
	applyQoderCLITurnCeiling(*qodercliTurnCeiling, flagWasSet("qodercli-turn-ceiling"))

	parsedSessionMode := parseSessionMode(*sessionMode)
	parsedTransport := parseTransport(*transport)
	finalConfigPath := strings.TrimSpace(*config)

	cfg.Host = strings.TrimSpace(*host)
	cfg.Port = *port
	cfg.Backend = parseBackend(*backend)
	cfg.Transport = parsedTransport
	cfg.Pipe = strings.TrimSpace(*pipe)
	cfg.WebSocketURL = strings.TrimSpace(*wsURL)
	cfg.RemoteBaseURL = strings.TrimSpace(*remoteBaseURL)
	cfg.RemoteAuthFile = strings.TrimSpace(*remoteAuthFile)
	cfg.RemoteProxyURL = strings.TrimSpace(*remoteProxyURL)
	cfg.RemoteVersion = strings.TrimSpace(*remoteVersion)
	cfg.Cwd = strings.TrimSpace(*cwd)
	cfg.CurrentFilePath = strings.TrimSpace(*currentFilePath)
	cfg.Mode = strings.TrimSpace(*mode)
	cfg.Model = strings.TrimSpace(*model)
	cfg.ShellType = strings.TrimSpace(*shellType)
	cfg.SessionMode = parsedSessionMode
	cfg.Timeout = time.Duration(*timeoutSeconds) * time.Second
	cfg.RemoteFallbackEnabled = *remoteFallbackEnabled
	cfg.RemoteFallbackModels = splitCSV(*remoteFallbackModels)
	cfg.QoderCLISites = splitCSV(*qodercliSites)
	utilityOptions.exportRemoteAuth = strings.TrimSpace(*exportRemoteAuth)
	utilityOptions.exportServerBundle = strings.TrimSpace(*exportServerBundle)
	utilityOptions.remoteAuthPick = strings.TrimSpace(*remoteAuthPick)
	if err := remote.ValidateProxyURL(cfg.RemoteProxyURL); err != nil {
		log.Fatal(err)
	}

	if configLoaded {
		configPath = finalConfigPath
	} else {
		configPath = ""
	}

	// The remote/CLI choice is deliberately not made here: it opens the login cache
	// and globs PATH, and doing it in the loader put it before the listener for no
	// reason the request path needed. The service resolves it once, lazily, after
	// the port is open.
	return cfg, configPath
}

func handleUtilityCommands(cfg service.Config) bool {
	if utilityOptions.exportRemoteAuth == "" && utilityOptions.exportServerBundle == "" {
		return false
	}
	policy := remote.CredentialPickPolicy(strings.ToLower(valueOr(utilityOptions.remoteAuthPick, string(remote.CredentialPickAuto))))
	switch policy {
	case remote.CredentialPickAuto, remote.CredentialPickNewest, remote.CredentialPickLongest:
	default:
		log.Fatalf("invalid remote auth pick policy %q; expected auto, newest, or longest", utilityOptions.remoteAuthPick)
	}
	if utilityOptions.exportRemoteAuth != "" {
		result, err := deploy.WriteCredentialFile(cfg.RemoteAuthFile, utilityOptions.exportRemoteAuth, policy)
		if err != nil {
			log.Fatalf("export remote auth: %v", err)
		}
		printExportResult("credentials", result)
	}
	if utilityOptions.exportServerBundle != "" {
		host := cfg.Host
		if strings.TrimSpace(host) == "" || host == "127.0.0.1" || host == "localhost" {
			host = "0.0.0.0"
		}
		result, err := deploy.WriteServerBundle(deploy.ServerBundleOptions{
			AuthFile:      cfg.RemoteAuthFile,
			OutputPath:    utilityOptions.exportServerBundle,
			PickPolicy:    policy,
			BaseURL:       cfg.RemoteBaseURL,
			ProxyURL:      cfg.RemoteProxyURL,
			RemoteVersion: cfg.RemoteVersion,
			Host:          host,
			Port:          cfg.Port,
			Model:         cfg.Model,
		})
		if err != nil {
			log.Fatalf("export server bundle: %v", err)
		}
		printExportResult("server bundle", result)
	}
	return true
}

func printExportResult(kind string, result deploy.ServerBundleResult) {
	fmt.Printf("exported %s: %s\n", kind, result.Path)
	fmt.Printf("credential source: %s\n", result.CredentialSrc)
	if result.TokenExpireAt != "" {
		fmt.Printf("token expire at: %s\n", result.TokenExpireAt)
	}
	if result.TokenExpired {
		fmt.Println("warning: exported credential is already expired")
	}
	if result.UserID != "" || result.MachineID != "" {
		fmt.Printf("account: %s / %s\n", result.UserID, result.MachineID)
	}
	fmt.Println("keep the exported file private; it contains login secrets")
}

func resolveConfigPath() (string, bool) {
	// The pre-scan runs before flag.Parse, so it has to accept every spelling
	// flag.Parse would accept: "-config v" is the same flag as "--config v", and
	// a pre-scan that only knows the double-dash form silently loads the default
	// file while the startup log prints the path the operator actually named.
	if path := strings.TrimSpace(lookupArgValue("config")); path != "" {
		return path, true
	}
	if path := strings.TrimSpace(os.Getenv("LINGMA_PROXY_CONFIG")); path != "" {
		return path, true
	}
	defaultPath := filepath.Join(currentDir(), "lingma-proxy.json")
	for _, candidate := range []string{defaultPath, filepath.Join(currentDir(), "lingma-ipc-proxy.json")} {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
	}
	return defaultPath, false
}

func readFileConfig(path string) (fileConfig, error) {
	var cfg fileConfig
	body, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return cfg, err
	}
	// Unknown keys are reported, not rejected. The file is hand-written and also
	// shipped by -export-server-bundle, and its only error path is log.Fatalf in
	// loadConfig: turning one stale key nobody reads into a refusal to start is a
	// far worse outage than the typo it would catch. The console write API can
	// reject unknown fields because it owns the payload it is handed; this file
	// belongs to whoever edited it last, so the complaint goes to the log.
	for _, key := range unknownFileConfigKeys(body) {
		log.Printf("config file %s: ignoring unknown key %q", path, key)
	}
	return cfg, nil
}

// unknownFileConfigKeys lists the top-level keys no field of fileConfig claims.
// A misspelling such as "remoate_base_url" used to be dropped without a word, so
// the operator saw a setting that silently never took effect.
func unknownFileConfigKeys(body []byte) []string {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil
	}
	known := knownFileConfigKeys()
	unknown := make([]string, 0, len(raw))
	for key := range raw {
		if _, ok := known[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// knownFileConfigKeys derives the accepted set from the struct tags instead of a
// hand-maintained list, so a field added to fileConfig is accepted without a
// second edit here that can be forgotten.
func knownFileConfigKeys() map[string]struct{} {
	typ := reflect.TypeOf(fileConfig{})
	keys := make(map[string]struct{}, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		keys[name] = struct{}{}
	}
	return keys
}

func overlayFileConfig(dst *service.Config, src fileConfig) {
	if strings.TrimSpace(src.Host) != "" {
		dst.Host = strings.TrimSpace(src.Host)
	}
	if src.Port > 0 {
		dst.Port = src.Port
	}
	if strings.TrimSpace(src.Transport) != "" {
		dst.Transport = parseTransport(src.Transport)
	}
	if strings.TrimSpace(src.Backend) != "" {
		dst.Backend = parseBackend(src.Backend)
	}
	if strings.TrimSpace(src.Pipe) != "" {
		dst.Pipe = strings.TrimSpace(src.Pipe)
	}
	if strings.TrimSpace(src.WebSocketURL) != "" {
		dst.WebSocketURL = strings.TrimSpace(src.WebSocketURL)
	}
	if strings.TrimSpace(src.RemoteBaseURL) != "" {
		dst.RemoteBaseURL = strings.TrimSpace(src.RemoteBaseURL)
	}
	if strings.TrimSpace(src.RemoteAuthFile) != "" {
		dst.RemoteAuthFile = strings.TrimSpace(src.RemoteAuthFile)
	}
	if strings.TrimSpace(src.RemoteProxyURL) != "" {
		dst.RemoteProxyURL = strings.TrimSpace(src.RemoteProxyURL)
	}
	if strings.TrimSpace(src.RemoteVersion) != "" {
		dst.RemoteVersion = strings.TrimSpace(src.RemoteVersion)
	}
	if strings.TrimSpace(src.Cwd) != "" {
		dst.Cwd = strings.TrimSpace(src.Cwd)
	}
	if strings.TrimSpace(src.CurrentFilePath) != "" {
		dst.CurrentFilePath = strings.TrimSpace(src.CurrentFilePath)
	}
	if strings.TrimSpace(src.Mode) != "" {
		dst.Mode = strings.TrimSpace(src.Mode)
	}
	if strings.TrimSpace(src.Model) != "" {
		dst.Model = strings.TrimSpace(src.Model)
	}
	if strings.TrimSpace(src.ShellType) != "" {
		dst.ShellType = strings.TrimSpace(src.ShellType)
	}
	if strings.TrimSpace(src.SessionMode) != "" {
		dst.SessionMode = parseSessionMode(src.SessionMode)
	}
	if src.TimeoutSeconds >= 0 {
		dst.Timeout = time.Duration(src.TimeoutSeconds) * time.Second
	}
	if src.RemoteFallbackEnabled != nil {
		dst.RemoteFallbackEnabled = *src.RemoteFallbackEnabled
	}
	if len(src.RemoteFallbackModels) > 0 {
		dst.RemoteFallbackModels = cleanStringSlice(src.RemoteFallbackModels)
	}
	if len(src.QoderCLISites) > 0 {
		dst.QoderCLISites = cleanStringSlice(src.QoderCLISites)
	}
}

func overlayEnvConfig(dst *service.Config) {
	if value := strings.TrimSpace(os.Getenv("LINGMA_PROXY_HOST")); value != "" {
		dst.Host = value
	}
	if value := envInt("LINGMA_PROXY_PORT", 0); value > 0 {
		dst.Port = value
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_PROXY_TRANSPORT")); value != "" {
		dst.Transport = parseTransport(value)
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_PROXY_BACKEND")); value != "" {
		dst.Backend = parseBackend(value)
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_IPC_PIPE")); value != "" {
		dst.Pipe = value
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_PROXY_WS_URL")); value != "" {
		dst.WebSocketURL = value
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_REMOTE_BASE_URL")); value != "" {
		dst.RemoteBaseURL = value
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_REMOTE_AUTH_FILE")); value != "" {
		dst.RemoteAuthFile = value
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_REMOTE_PROXY_URL")); value != "" {
		dst.RemoteProxyURL = value
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_REMOTE_VERSION")); value != "" {
		dst.RemoteVersion = value
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_PROXY_CWD")); value != "" {
		dst.Cwd = value
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_PROXY_CURRENT_FILE_PATH")); value != "" {
		dst.CurrentFilePath = value
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_PROXY_MODE")); value != "" {
		dst.Mode = value
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_PROXY_MODEL")); value != "" {
		dst.Model = value
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_PROXY_SHELL_TYPE")); value != "" {
		dst.ShellType = value
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_PROXY_SESSION_MODE")); value != "" {
		dst.SessionMode = parseSessionMode(value)
	}
	if value := envInt("LINGMA_PROXY_TIMEOUT_SECONDS", -1); value >= 0 {
		dst.Timeout = time.Duration(value) * time.Second
	}
	if value, ok := envBool("LINGMA_REMOTE_FALLBACK_ENABLED"); ok {
		dst.RemoteFallbackEnabled = value
	}
	if value := strings.TrimSpace(os.Getenv("LINGMA_REMOTE_FALLBACK_MODELS")); value != "" {
		dst.RemoteFallbackModels = splitCSV(value)
	}
}

func parseSessionMode(value string) service.SessionMode {
	mode := service.SessionMode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case service.SessionModeAuto, service.SessionModeFresh, service.SessionModeReuse:
		return mode
	default:
		log.Fatalf("invalid session mode %q; expected auto, fresh, or reuse", value)
		return service.SessionModeAuto
	}
}

func parseBackend(value string) service.BackendMode {
	mode := service.BackendMode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case "":
		return service.BackendRemote
	case service.BackendIPC:
		return service.BackendIPC
	case service.BackendRemote:
		return service.BackendRemote
	case service.BackendQoderCLI:
		return service.BackendQoderCLI
	default:
		log.Fatalf("invalid backend %q; expected ipc, remote or qodercli", value)
		return service.BackendIPC
	}
}

func parseTransport(value string) lingmaipc.Transport {
	transport, err := lingmaipc.ParseTransport(value)
	if err != nil {
		log.Fatal(err)
	}
	return transport
}

// lookupArgValue reads one flag out of the raw command line, before flag.Parse
// has had a chance to. It matches both the single-dash and the double-dash
// spelling, in the separated and the "=" form, because the flag package treats
// "-config", "--config" and "-config=v" as the same flag and a pre-scan that
// disagrees with the parser hands the caller the wrong file.
func lookupArgValue(flagName string) string {
	names := []string{"-" + flagName, "--" + flagName}
	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		for _, name := range names {
			if arg == name {
				if i+1 < len(os.Args) {
					return os.Args[i+1]
				}
				return ""
			}
			prefix := name + "="
			if strings.HasPrefix(arg, prefix) {
				return strings.TrimPrefix(arg, prefix)
			}
		}
	}
	return ""
}

// envInt reads an integer override, and says so when the value it was handed is
// not one. A silent fallback makes a typo'd LINGMA_PROXY_PORT look exactly like
// an unset one, and "my setting does nothing" is the most expensive kind of
// misconfiguration to diagnose.
func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		log.Printf("%s=%q is not an integer; keeping %d", key, value, fallback)
		return fallback
	}
	return n
}

func envBool(key string) (bool, bool) {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	switch value {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	case "":
		return false, false
	default:
		log.Printf("%s=%q is not a boolean (want 1/0, true/false, yes/no or on/off); ignoring it", key, value)
		return false, false
	}
}

// applyQoderCLITurnCeiling hands the flag's value to the CLI backend. Only an
// explicit flag writes it, so an unset flag leaves the environment exactly as
// the operator handed it to us, including "unset", which is the CLI backend's
// own signal to use its default backstop.
func applyQoderCLITurnCeiling(value string, explicit bool) {
	if !explicit {
		return
	}
	if err := os.Setenv(qodercliTurnCeilingEnv, strings.TrimSpace(value)); err != nil {
		log.Printf("set %s: %v", qodercliTurnCeilingEnv, err)
	}
}

// flagWasSet reports whether the operator passed the flag, as opposed to it
// merely carrying a default. Every other flag in this file layers its default
// from the file config and lets the command line win; this one layers from the
// environment, and only a real override may be pushed back into it.
func flagWasSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func splitCSV(value string) []string {
	return cleanStringSlice(strings.Split(value, ","))
}

func cleanStringSlice(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		item := strings.TrimSpace(value)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func currentDir() string {
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func valueOr(value string, fallback string) string {
	if strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

func defaultShellType() string {
	if runtime.GOOS == "windows" {
		return "powershell"
	}
	if runtime.GOOS == "darwin" {
		return "zsh"
	}
	return "bash"
}
