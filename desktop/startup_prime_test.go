package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// desktopSource reads a file from this package and normalises its line endings.
// core.autocrlf makes the checkout CRLF, so a test that pins a literal "\n" in
// source text fails on a correct file -- which is how
// TestStartupRedirectsBeforeAnythingElse in cmd/lingma-ipc-proxy went red on
// Windows while saying nothing about the code it claimed to check.
func desktopSource(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.ReplaceAll(string(raw), "\r\n", "\n")
}

// startProxyBody returns the text of startProxy, from its signature to the start of
// the next top-level func.
func startProxyBody(t *testing.T) string {
	t.Helper()
	src := desktopSource(t, "app.go")
	from := strings.Index(src, "func (a *App) startProxy(")
	if from < 0 {
		t.Fatal("startProxy not found in app.go")
	}
	rest := src[from:]
	if next := strings.Index(rest[1:], "\nfunc "); next >= 0 {
		return rest[:next+1]
	}
	return rest
}

// TestDesktopPrimesTheBackendOnStartup pins the gap that let the desktop be the one
// entry point that never primed: Service.Warmup is the only caller of the priming
// budget, and until now only the headless main called it.
//
// The cost of the gap is specific rather than theoretical. A request-path discovery
// is capped at the catalog's own cold budget, which is below what one CLI spawn
// costs on a busy box -- so there the desktop never gets a catalog written, and
// every cold start re-runs the discovery and fails.
func TestDesktopPrimesTheBackendOnStartup(t *testing.T) {
	body := startProxyBody(t)
	if !strings.Contains(body, "svc.Warmup(") {
		t.Fatal("startProxy never calls Service.Warmup; the priming budget is unreachable from the GUI")
	}
}

// TestDesktopPrimeRunsOutsideTheAppMutex is the half that turns the fix into a
// regression if it is written the obvious way.
//
// Warmup can sit inside a child-process wait for its whole budget. Every binding
// call and every emitLog on the other side of a.mu queues behind whoever holds it,
// so a prime taken under the lock would freeze the UI for as long as the warm-up
// is allowed to run. The call has to be after the unlock.
func TestDesktopPrimeRunsOutsideTheAppMutex(t *testing.T) {
	body := startProxyBody(t)
	lock := strings.Index(body, "a.mu.Lock()")
	unlock := strings.Index(body, "a.mu.Unlock()")
	warm := strings.Index(body, "svc.Warmup(")
	if lock < 0 || unlock < 0 || warm < 0 {
		t.Fatalf("startProxy shape changed: lock=%d unlock=%d warmup=%d", lock, unlock, warm)
	}
	if warm < unlock {
		t.Fatalf("svc.Warmup is at offset %d, before the mu.Unlock at %d: it would hold a.mu for its whole budget",
			warm, unlock)
	}
}

// TestPrimeWarmupTimeoutIsBounded pins the property that lets the prime exist at
// all. It is generous because nobody waits on it, but it has to be finite: an
// unbounded one means a wedged backend holds a CLI child for the life of the
// process, and the operator is left with a machine that never goes quiet.
func TestPrimeWarmupTimeoutIsBounded(t *testing.T) {
	if primeWarmupTimeout <= 0 {
		t.Fatalf("primeWarmupTimeout = %s; a detached pass still needs a stop", primeWarmupTimeout)
	}
	if primeWarmupTimeout > 90*time.Minute {
		t.Fatalf("primeWarmupTimeout = %s, which is long enough to be indistinguishable from never", primeWarmupTimeout)
	}
	// It must also outlast the per-site budget it exists to give room to.
	if primeWarmupTimeout <= 15*time.Minute {
		t.Fatalf("primeWarmupTimeout = %s does not exceed the 15m per-site priming budget", primeWarmupTimeout)
	}
}
