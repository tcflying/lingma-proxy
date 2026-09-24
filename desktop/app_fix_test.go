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

// stateFlushLockBudget is how long one full-ring app-state flush may hold a.mu.
// Measured floor is ~40ms here, so this is headroom for contention, not a licence
// to grow the payload.
const stateFlushLockBudget = 250 * time.Millisecond

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

// 924 §4.3 measured, not assumed: flushAppStateLocked marshals and writes while
// holding a.mu, which every request log line also needs. The measurement says the
// marshal is the whole cost and the WriteFile is ~1ms, so moving the write off the
// lock would buy the cheap half; what has to stay true instead is the budget,
// which is what this pins. If a change pushes the payload past its designed bound
// (300 requests x two 8KB bodies, 1000 log lines), the flush goes red here rather
// than quietly stalling the request path.
func TestStateFlushHoldsTheLockForABoundedTime(t *testing.T) {
	useTempInstanceProfile(t)

	body := strings.Repeat("x", 8<<10)
	app := &App{}
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
	// Measured on this box with the ring completely full: rendering the ~5 MB is
	// the whole cost (~40ms) and the WriteFile behind it is ~1ms, which is why the
	// write was not moved off a.mu -- the restructure would only buy the 1ms half.
	// The budget below is ~5x the quiet median so contention cannot make this flap,
	// while anything that pushes the payload past its designed bound still trips it.
	t.Logf("flushed %.2f MB of app state under a.mu for %s, of which rendering took %s",
		float64(info.Size())/(1<<20), held.Round(time.Millisecond), rendered.Round(time.Millisecond))
	if held > stateFlushLockBudget {
		t.Fatalf("one flush held a.mu for %s, over the %s budget (render %s)",
			held.Round(time.Millisecond), stateFlushLockBudget, rendered.Round(time.Millisecond))
	}
}
