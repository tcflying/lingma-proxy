package main

import (
	"testing"
	"time"

	"lingma-ipc-proxy/internal/service"
)

// 933 P3-R3 verification. The finding claimed that because the front end's 0 is
// kept verbatim, the CLI-turn fallback ceiling would not take effect on the
// desktop. Reading the whole path says the opposite, and this pins it:
//
//	defaultConfig (app.go)        -> Timeout: 0
//	UpdateConfig  (app.go)        -> 0 untouched; only sub-second non-zero values
//	                                 are scaled
//	startProxy   (app.go)         -> service.New(cfg), no local normalisation
//	service.Config.Timeout        -> contextWithOptionalTimeout / qodercli.NewClient
//
// That last hop is the same one the headless entry takes (cmd/.../main.go builds
// the same service.Config), and a ceiling applied there is exactly the branch that
// 0 selects. So 0 is the value that *triggers* the ceiling, never the value that
// suppresses it, and there is no desktop-side bypass to close. Substituting a
// number here instead would give the `timeout` setting a second meaning and leave
// two ceilings that can drift apart.
func TestTimeoutZeroStaysZeroThroughTheDesktopHop(t *testing.T) {
	useTempInstanceProfile(t)

	if got := defaultConfig().Timeout; got != 0 {
		t.Fatalf("defaultConfig().Timeout = %v, want 0: the desktop default is deliberately the same as the headless entry", got)
	}

	app := &App{cfg: defaultConfig()}
	// The front end's own zero, on the request that reaches the service.
	if err := app.UpdateConfig(service.Config{Host: "127.0.0.1", Port: 10095, Timeout: 0}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	app.mu.RLock()
	afterZero := app.cfg.Timeout
	app.mu.RUnlock()
	if afterZero != 0 {
		t.Fatalf("Timeout = %v after the front end sent 0; the '0 disables the proxy deadline' contract is broken", afterZero)
	}

	// The one conversion the front end does depend on: a bare number is seconds.
	const wantSeconds = 90
	if err := app.UpdateConfig(service.Config{Host: "127.0.0.1", Port: 10095, Timeout: wantSeconds}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	app.mu.RLock()
	afterSeconds := app.cfg.Timeout
	app.mu.RUnlock()
	if afterSeconds != wantSeconds*time.Second {
		t.Fatalf("Timeout = %v, want %v: a bare number from the front end is seconds", afterSeconds, wantSeconds*time.Second)
	}
}
