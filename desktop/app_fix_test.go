package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/service"
)

// stateFlushPayloadBudget is how big the persisted app state may get. The flush
// renders all of it while holding a.mu, which every request log line also needs,
// so the payload size -- not a wall-clock number, which moves 10x between a plain
// and a -race build -- is what has to stay bounded. The fixture below is the
// designed maximum ring; this is that with ~20% headroom for indentation.
const stateFlushPayloadBudget = 6 << 20

// useTempInstanceProfile points both the per-install settings file and the app
// state file at a temporary home, so no test can touch the real
// ~/.config/lingma-ipc-proxy of the machine running it.
func useTempInstanceProfile(t *testing.T) (home, configFile string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("ProgramData", filepath.Join(home, "ProgramData"))
	configFile = filepath.Join(home, "config.json")

	instanceMu.Lock()
	saved := instanceCached
	instanceCached = &instanceProfile{
		title:      defaultWindowTitle,
		singleID:   defaultSingleInstanceID,
		configPath: configFile,
	}
	instanceMu.Unlock()
	t.Cleanup(func() {
		instanceMu.Lock()
		instanceCached = saved
		instanceMu.Unlock()
	})
	return home, configFile
}

// setStateFlushKnobs shrinks the debounce window and its ceiling so a test can
// drive a whole burst of writes inside one interval.
func setStateFlushKnobs(t *testing.T, interval, maxDelay time.Duration) {
	t.Helper()
	oldInterval, oldMax := appStateFlushInterval, appStateFlushMaxDelay
	t.Cleanup(func() {
		appStateFlushInterval, appStateFlushMaxDelay = oldInterval, oldMax
	})
	appStateFlushInterval, appStateFlushMaxDelay = interval, maxDelay
}

// D1: the flush is a debounce, so a session that writes faster than the interval
// used to postpone it forever and nothing reached disk before a crash.
func TestStateFlushPersistsContinuousWrites(t *testing.T) {
	home, _ := useTempInstanceProfile(t)
	setStateFlushKnobs(t, 60*time.Millisecond, 120*time.Millisecond)

	statePath, err := appStatePath()
	if err != nil {
		t.Fatalf("appStatePath() error = %v", err)
	}
	if !strings.HasPrefix(statePath, home) {
		t.Fatalf("state path %q escaped the temporary home %q", statePath, home)
	}

	app := &App{}
	const writes = 24 // ~480ms at 20ms apart, four times the ceiling
	sawAdvanceDuringWrites := false
	for i := 0; i < writes; i++ {
		app.mu.Lock()
		app.requests = append(app.requests, RequestRecord{
			ID:        "req-" + strconv.Itoa(i),
			CreatedAt: time.Now().Format(time.RFC3339),
			Path:      "/v1/messages",
		})
		app.saveAppStateLocked()
		app.mu.Unlock()
		time.Sleep(20 * time.Millisecond)

		// Half the writes back, so this cannot be satisfied by a flush that
		// only ran after the burst stopped.
		if data, readErr := os.ReadFile(statePath); readErr == nil && strings.Contains(string(data), "req-"+strconv.Itoa(i/2)) {
			sawAdvanceDuringWrites = true
		}
	}
	if !sawAdvanceDuringWrites {
		t.Fatalf("state never reached disk while writes kept coming: a pure debounce that keeps resetting is back")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		data, readErr := os.ReadFile(statePath)
		if readErr == nil && strings.Contains(string(data), "req-"+strconv.Itoa(writes-1)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the last write never persisted: %v", readErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// D5: saving merges into the existing file to keep keys this app does not own,
// so omitting an empty site set let the old one come back after a restart.
func TestSaveConfigClearsQoderCLISites(t *testing.T) {
	_, configFile := useTempInstanceProfile(t)
	seed := `{"instance_name":"keep me","qodercli_sites":["cn","intl"]}`
	if err := os.WriteFile(configFile, []byte(seed), 0644); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	app := &App{}
	if err := app.saveConfig(service.Config{Host: "127.0.0.1", Port: 8095}); err != nil {
		t.Fatalf("saveConfig() error = %v", err)
	}

	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse config: %v", err)
	}
	if doc["instance_name"] != "keep me" {
		t.Fatalf("saving stripped a key this app does not own: %s", raw)
	}
	value, ok := doc["qodercli_sites"]
	if !ok {
		t.Fatalf("qodercli_sites is absent, so the previous value survives a restart: %s", raw)
	}
	list, isList := value.([]any)
	if !isList || len(list) != 0 {
		t.Fatalf("qodercli_sites = %v, want an empty array", value)
	}
}

// D6: "no detected list yet" is not "any string is a valid model".
func TestSelectModelRefusesWithoutADetectedList(t *testing.T) {
	app := &App{cfg: service.Config{Model: "kmodel"}}
	if _, err := app.SelectModel("not-a-model"); err == nil {
		t.Fatal("SelectModel accepted an arbitrary id before detection finished")
	}
	app.mu.RLock()
	model := app.cfg.Model
	app.mu.RUnlock()
	if model != "kmodel" {
		t.Fatalf("the rejected id was written into the config: %q", model)
	}
}

// D7: stats.ByModel is keyed by a client-controlled string and cloned on every
// read, so the tracked key set has to stay bounded.
func TestTokenStatsCapTrackedModelKeys(t *testing.T) {
	app := &App{}
	// Up to the cap, every model keeps its own bucket: the real detected list is
	// far below it, and folding those together would break the usage view.
	for i := 0; i < statsModelKeyLimit; i++ {
		app.mu.Lock()
		app.accumulateTokenStatsLocked(RequestRecord{
			Path:        "/v1/messages",
			Model:       fmt.Sprintf("model-%03d", i),
			TotalTokens: 10,
		})
		app.mu.Unlock()
	}
	app.mu.RLock()
	atLimit := len(app.stats.ByModel)
	app.mu.RUnlock()
	if atLimit != statsModelKeyLimit {
		t.Fatalf("tracked models = %d, want %d", atLimit, statsModelKeyLimit)
	}

	for _, overflow := range []string{"overflow-a", "overflow-b"} {
		app.mu.Lock()
		app.accumulateTokenStatsLocked(RequestRecord{Path: "/v1/messages", Model: overflow, TotalTokens: 5})
		app.mu.Unlock()
	}

	stats := app.GetTokenStats()
	if len(stats.ByModel) != statsModelKeyLimit {
		t.Fatalf("tracked models = %d, want the cap held at %d", len(stats.ByModel), statsModelKeyLimit)
	}
	if _, leaked := stats.ByModel["overflow-a"]; leaked {
		t.Fatal("a model past the cap got its own bucket")
	}
	// Folding moves buckets around, it must never drop tokens: the cap exists to
	// bound how many keys every read has to clone, not how many tokens we admit.
	// (The exact split between "-" and an evicted small bucket is the fold's
	// business; the total and the cap are the invariants.)
	total := 0
	for _, tokens := range stats.ByModel {
		total += tokens
	}
	if want := 200*10 + 2*5; total != want {
		t.Fatalf("folded total = %d, want every request counted exactly once (%d)", total, want)
	}
	if stats.ByModel[statsOverflowModel] == 0 {
		t.Fatal("nothing was folded into the overflow bucket")
	}
	if stats.LastModel == statsOverflowModel {
		t.Fatal("the overflow bucket must not become the last used model")
	}
}

// D7: a state file written before the cap existed has to be folded on load, or
// the unbounded map keeps being cloned per request forever.
func TestFoldModelKeysCapsLoadedStats(t *testing.T) {
	stats := TokenStats{ByModel: map[string]int{}}
	for i := 0; i < 500; i++ {
		stats.ByModel[fmt.Sprintf("model-%03d", i)] = i + 1
	}
	stats.ByModel[statsOverflowModel] = 7

	foldModelKeysLocked(&stats, statsModelKeyLimit)

	if len(stats.ByModel) > statsModelKeyLimit {
		t.Fatalf("folded size = %d, want at most %d", len(stats.ByModel), statsModelKeyLimit)
	}
	// The biggest buckets survive, and everything else is counted exactly once.
	total := 0
	for i := 0; i < 500; i++ {
		total += i + 1
	}
	folded := 0
	for _, value := range stats.ByModel {
		folded += value
	}
	if folded != total+7 {
		t.Fatalf("folded tokens = %d, want %d", folded, total+7)
	}
	if _, kept := stats.ByModel["model-499"]; !kept {
		t.Fatal("the largest bucket should survive the fold")
	}
	if _, kept := stats.ByModel["model-000"]; kept {
		t.Fatal("the smallest bucket should be folded away")
	}

	// Deterministic: Go map order is not, so the same input must fold the same.
	again := TokenStats{ByModel: map[string]int{}}
	for i := 0; i < 500; i++ {
		again.ByModel[fmt.Sprintf("model-%03d", i)] = i + 1
	}
	again.ByModel[statsOverflowModel] = 7
	foldModelKeysLocked(&again, statsModelKeyLimit)
	if len(again.ByModel) != len(stats.ByModel) {
		t.Fatalf("fold is not stable: %d vs %d", len(again.ByModel), len(stats.ByModel))
	}
	for model := range stats.ByModel {
		if _, ok := again.ByModel[model]; !ok {
			t.Fatalf("fold is not stable: %q present only once", model)
		}
	}
}

// D8: an empty host formats as ":8095", which is every interface.
func TestListenAddrRefusesBlankHost(t *testing.T) {
	for _, host := range []string{"", "   ", "\t"} {
		if got, err := listenAddr(host, 8095); err == nil {
			t.Fatalf("listenAddr(%q, 8095) = %q, want an error: a blank host binds every interface", host, got)
		}
	}
	if got, err := listenAddr("0.0.0.0", 8095); err != nil || got != "0.0.0.0:8095" {
		t.Fatalf("explicit public bind = %q, %v", got, err)
	}
	if got, err := listenAddr("127.0.0.1", 8095); err != nil || got != "127.0.0.1:8095" {
		t.Fatalf("loopback bind = %q, %v", got, err)
	}
	if got, err := listenAddr("::", 8095); err != nil || got != "[::]:8095" {
		t.Fatalf("ipv6 bind = %q, %v", got, err)
	}
	for _, port := range []int{0, -1, 70000} {
		if _, err := listenAddr("127.0.0.1", port); err == nil {
			t.Fatalf("listenAddr accepted port %d", port)
		}
	}
}

// D8: the console config route must not be able to persist a blank host. The
// StartProxy side is not asserted here on purpose: proving it would mean
// letting a re-injected build bind every interface, which a test must not do.
func TestUpdateConfigRejectsBlankHost(t *testing.T) {
	app := &App{cfg: service.Config{Host: "127.0.0.1", Port: 10095}}
	for _, host := range []string{"", "  "} {
		if err := app.UpdateConfig(service.Config{Host: host, Port: 10095}); err == nil {
			t.Fatalf("UpdateConfig accepted listen host %q", host)
		}
	}
	app.mu.RLock()
	persisted := app.cfg.Host
	app.mu.RUnlock()
	if persisted != "127.0.0.1" {
		t.Fatalf("the rejected host was written into memory: %q", persisted)
	}
}

// D2: startup and DOM ready used to publish the Wails context from two threads.
// Wails hands both hooks the same front-end context, so onDomReady must not
// write it again.
func TestOnDomReadyDoesNotReplaceTheContext(t *testing.T) {
	sentinel := context.WithValue(context.Background(), "startup", true)
	app := &App{}
	app.setWailsCtx(sentinel)
	app.onDomReady(context.WithValue(context.Background(), "domready", true))
	if app.wailsCtx() != sentinel {
		t.Fatal("onDomReady re-published the lifecycle context; that is the second unsynchronised writer")
	}
	if (&App{}).wailsCtx() != nil {
		t.Fatal("wailsCtx must be nil before startup publishes it")
	}
}

// D4: a serve failure may only retire the generation that is still in charge.
func TestReleaseProxyOnlyClearsItsOwnGeneration(t *testing.T) {
	current := &service.Service{}
	stale := &service.Service{}
	app := &App{running: true, svc: current, addr: "127.0.0.1:10095", startedAt: time.Now()}

	app.releaseProxy(stale)
	app.mu.RLock()
	running, addr := app.running, app.addr
	app.mu.RUnlock()
	if !running || addr != "127.0.0.1:10095" {
		t.Fatal("a late failure from an old server retired the running generation")
	}

	app.releaseProxy(current)
	app.mu.RLock()
	running, server, svc := app.running, app.server, app.svc
	app.mu.RUnlock()
	if running || server != nil || svc != nil {
		t.Fatal("the owning generation did not clear its claim")
	}
}

// 924 §4.3 originally measured the flush as marshal-plus-write under a.mu and
// bounded the payload instead of moving the cost. The 930 re-audit (O7) split
// it for real: the lock now pays only the snapshot (fresh backing arrays plus
// one map clone, microseconds with the ring full), and marshal plus the
// temp-file write happen in the single writer goroutine. This test still pins
// the payload bound -- anything that grows the persisted state past its
// designed size (300 requests x two 8KB bodies, 1000 log lines) goes red here
// rather than quietly growing what the writer marshals -- and now also pins
// that the lock-held half stays cheap even with the ring full.
func TestStateFlushPayloadStaysBounded(t *testing.T) {
	useTempInstanceProfile(t)

	body := strings.Repeat("x", 8<<10)
	app := &App{}
	// Zero value keeps the write inline, so the file this test stats exists the
	// moment the flush returns; the async writer's lock duty is pinned by its
	// own test below.
	app.mu.Lock()
	for i := 0; i < appStatePersistRequestMax; i++ {
		app.requests = append(app.requests, RequestRecord{
			ID:        "req-" + strconv.Itoa(i),
			CreatedAt: time.Now().Format(time.RFC3339),
			Path:      "/v1/messages",
			Model:     "kmodel",
			ReqBody:   body,
			RespBody:  body,
		})
	}
	for i := 0; i < appStatePersistLogMax; i++ {
		app.logs = append(app.logs, AppLog{
			ID:        "log-" + strconv.Itoa(i),
			CreatedAt: time.Now().Format(time.RFC3339),
			Level:     "info",
			Message:   "request recorded for the console ring buffer, padded to a typical length",
		})
	}
	renderStarted := time.Now()
	app.renderAppStateLocked()
	rendered := time.Since(renderStarted)
	started := time.Now()
	app.flushAppStateLocked()
	held := time.Since(started)
	app.mu.Unlock()

	statePath, err := appStatePath()
	if err != nil {
		t.Fatalf("appStatePath() error = %v", err)
	}
	info, err := os.Stat(statePath)
	if err != nil {
		t.Fatalf("the flush wrote nothing: %v", err)
	}
	// The payload budget is the invariant that survived both designs: bounding
	// what one flush marshals is what bounds the writer's duty cycle and the
	// on-disk size alike. The inline-mode ceiling stays loose on purpose --
	// it holds marshal plus write, which is race-build sensitive.
	t.Logf("flushed %.2f MB of app state inline for %s, of which rendering took %s",
		float64(info.Size())/(1<<20), held.Round(time.Millisecond), rendered.Round(time.Millisecond))
	if info.Size() > stateFlushPayloadBudget {
		t.Fatalf("app state file is %.2f MB, over the %.1f MB budget: the payload bound is what keeps the writer's marshal cheap",
			float64(info.Size())/(1<<20), float64(stateFlushPayloadBudget)/(1<<20))
	}
	if held > appStateFlushInterval {
		t.Fatalf("one inline flush took %s, longer than the %s debounce interval it is supposed to fit inside",
			held.Round(time.Millisecond), appStateFlushInterval)
	}
}

// TestStateWriterPersistsAsyncWithoutBlockingTheLock pins the O7 restructure
// itself: in async mode (the production default) a flush returns while holding
// a.mu for microseconds, the file appears shortly after via the writer, and no
// temp file is left behind -- rename either succeeded or the direct-write
// fallback replaced the target.
func TestStateWriterPersistsAsyncWithoutBlockingTheLock(t *testing.T) {
	useTempInstanceProfile(t)
	app := &App{}
	// Production shape: async writer, so the flush under a.mu pays only the
	// snapshot and the marshal lands in the writer goroutine.
	app.stateWriteAsync = true
	// Fill the ring the way the payload test does: with ~5 MB of state, an
	// inline marshal inside the flush would take the ~30ms+ measured in 924,
	// which is exactly the regression this bound exists to catch.
	body := strings.Repeat("x", 8<<10)
	app.mu.Lock()
	for i := 0; i < appStatePersistRequestMax; i++ {
		app.requests = append(app.requests, RequestRecord{ID: "r" + strconv.Itoa(i), ReqBody: body, RespBody: body})
	}
	started := time.Now()
	app.flushAppStateLocked()
	held := time.Since(started)
	app.mu.Unlock()
	if held > 25*time.Millisecond {
		t.Fatalf("flushAppStateLocked held a.mu for %s; the async hand-off must be immediate", held.Round(time.Millisecond))
	}

	statePath, err := appStatePath()
	if err != nil {
		t.Fatalf("appStatePath() error = %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var info os.FileInfo
	for time.Now().Before(deadline) {
		info, err = os.Stat(statePath)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("the async writer never wrote the state file: %v", err)
	}
	if info.Size() == 0 {
		t.Fatal("the async writer wrote an empty state file")
	}
	if _, err := os.Stat(statePath + ".tmp"); err == nil {
		t.Fatal("a temp file survived the flush; rename must consume it")
	}
}

// TestUsageUpdatedIsThrottledWithTrailingEmit pins O9: the first event goes out
// immediately, events inside the interval are coalesced into one trailing
// emission, and the timer arms once rather than per call. State is inspected
// directly because both real sinks (Wails runtime, console publish) are no-ops
// without a live frontend, which is exactly why the throttle must not depend
// on them.
func TestUsageUpdatedIsThrottledWithTrailingEmit(t *testing.T) {
	app := &App{}
	app.emitUsageUpdated()
	if app.lastUsageEmit.IsZero() {
		t.Fatal("the first emit did not record its time")
	}
	if app.usageEmitTimer != nil {
		t.Fatal("the first emit armed a trailing timer; only a suppressed emit may")
	}
	app.emitUsageUpdated()
	app.emitUsageUpdated()
	if app.usageEmitTimer == nil {
		t.Fatal("a suppressed emit did not arm the trailing timer")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		app.usageEmitMu.Lock()
		fired := app.usageEmitTimer == nil
		app.usageEmitMu.Unlock()
		if fired {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	app.usageEmitMu.Lock()
	fired := app.usageEmitTimer == nil
	app.usageEmitMu.Unlock()
	if !fired {
		t.Fatal("the trailing timer never fired within its interval")
	}
}

// TestFetchModelsPrefersTheInProcessService pins O6: with a claimed proxy the
// desktop must ask its own service object for the model list instead of making
// a loopback HTTP request to itself (which allocated a client per probe and
// landed every refresh in the user's request history). The discriminator is the
// error source: the service is configured with a remote base URL on a closed
// local port, so the direct path fails with that address in the error within
// milliseconds, while the loopback path on an empty address fails with a
// malformed-URL error that names neither the port nor the service.
func TestFetchModelsPrefersTheInProcessService(t *testing.T) {
	useTempInstanceProfile(t)

	svc := service.New(service.Config{
		Backend:       service.BackendRemote,
		RemoteBaseURL: "http://127.0.0.1:1",
	})
	app := &App{}
	app.mu.Lock()
	app.svc = svc
	app.mu.Unlock()

	started := time.Now()
	_, err := app.fetchModels("", 2*time.Second)
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("fetchModels against a closed port succeeded; the discriminator is broken")
	}
	// Either marker proves the in-process service was consulted: a box with a
	// real login cache dials and fails naming 127.0.0.1:1, while a box without
	// one fails earlier in credential resolution. Neither error can come from
	// the loopback path, whose empty address fails in URL handling.
	if !strings.Contains(err.Error(), "127.0.0.1:1") && !strings.Contains(err.Error(), "登录缓存") {
		t.Fatalf("fetchModels error = %v; it must come from the in-process service, not the loopback fallback", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("the probe took %s; a refused connection or a cache miss should fail fast", elapsed.Round(time.Millisecond))
	}
}

// TestFetchModelsFallsBackToLoopbackWithoutService pins the other half of O6:
// before a proxy is claimed there is no service to ask, and the loopback HTTP
// path remains for exactly that case.
func TestFetchModelsFallsBackToLoopbackWithoutService(t *testing.T) {
	useTempInstanceProfile(t)
	app := &App{}
	_, err := app.fetchModels("", 2*time.Second)
	if err == nil {
		t.Fatal("a loopback fetch on an empty address should not succeed")
	}
	if strings.Contains(err.Error(), "127.0.0.1:1") || strings.Contains(err.Error(), "登录缓存") {
		t.Fatalf("fetchModels error = %v; with no service claimed the loopback path must be the one that answers", err)
	}
}

// D14: the desktop's cold model probe must not give up before the backend it is
// calling is allowed to finish. On this box a cold CLI catalog takes 44-130s while
// the startup probe was capped at 12s, so the GUI opened on an empty model list
// until somebody pressed refresh by hand.
func TestStartupColdProbeCoversTheBackendsOwnBudget(t *testing.T) {
	cold := startupModelProbeTimeout(service.Config{WarmupTimeout: 30 * time.Second}, false)
	if cold < service.CLIColdProbeTimeout() {
		t.Fatalf("cold probe = %v, want >= the service's own budget %v", cold, service.CLIColdProbeTimeout())
	}
	if got := startupModelProbeTimeout(service.Config{WarmupTimeout: 300 * time.Second}, false); got != 300*time.Second {
		t.Fatalf("a longer configured budget must survive the floor, got %v", got)
	}
	if got := startupModelProbeTimeout(service.Config{WarmupTimeout: 30 * time.Second}, true); got > 5*time.Second {
		t.Fatalf("the cached path stays fast, got %v", got)
	}
}
