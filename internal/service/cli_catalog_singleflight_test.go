package service

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/qodercli"
	"lingma-ipc-proxy/internal/remote"
)

// stubCLICatalogProbe replaces the CLI probe with one test's fake, and hands
// back a per-site call counter. Fake clients are injected so no detection or
// CLI subprocess runs.
func stubCLICatalogProbe(t *testing.T, s *Service, probe func(site qodercli.Site, ctx context.Context) ([]remote.Model, error)) map[qodercli.Site]*atomic.Int32 {
	t.Helper()
	sites := s.cliSites()
	if len(sites) == 0 {
		t.Skip("no CLI site is configured")
	}
	byClient := make(map[*qodercli.Client]qodercli.Site, len(sites))
	counts := make(map[qodercli.Site]*atomic.Int32, len(sites))
	s.mu.Lock()
	s.cliClients = make(map[qodercli.Site]*qodercli.Client, len(sites))
	for _, site := range sites {
		client := &qodercli.Client{}
		s.cliClients[site] = client
		byClient[client] = site
		counts[site] = new(atomic.Int32)
	}
	s.mu.Unlock()

	previous := cliListModels
	cliListModels = func(client *qodercli.Client, ctx context.Context) ([]remote.Model, error) {
		site, ok := byClient[client]
		if !ok {
			// Nothing in this test may talk to a real CLI.
			return nil, context.Canceled
		}
		counts[site].Add(1)
		return probe(site, ctx)
	}
	t.Cleanup(func() {
		cliListModels = previous
		s.mu.Lock()
		s.cliClients = nil
		s.mu.Unlock()
	})
	return counts
}

// expectedCLIModelIDs folds the fake name the way mergeCLICatalogs would.
func expectedCLIModelIDs(sites []qodercli.Site, name string) map[string]bool {
	want := make(map[string]bool, len(sites))
	for _, site := range sites {
		id := name
		if len(sites) > 1 && site.Normalized() == qodercli.SiteGlobal {
			id = cliGlobalPrefix + name
		}
		want[id] = true
	}
	return want
}

func modelIDSet(models []Model) map[string]bool {
	ids := make(map[string]bool, len(models))
	for _, model := range models {
		ids[model.ID] = true
	}
	return ids
}

func assertModelIDs(t *testing.T, label string, models []Model, want map[string]bool) {
	t.Helper()
	got := modelIDSet(models)
	for id := range want {
		if !got[id] {
			t.Fatalf("%s returned %v, want the seam catalog %v", label, models, want)
		}
	}
}

// TestCatalogDiscoveryIsSingleFlightPerSite pins the stampede fix: concurrent
// /v1/models calls on a cold catalog used to start one CLI probe per site per
// caller (observed as leaked --list-models children), and must now share one
// probe per site. The seam blocks until both callers have asked, so a second
// probe would be counted before the owner is released.
func TestCatalogDiscoveryIsSingleFlightPerSite(t *testing.T) {
	useCLICatalogDir(t)
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	sites := s.cliSites()
	if len(sites) == 0 {
		t.Skip("no CLI site is configured")
	}

	release := make(chan struct{})
	entered := make(chan qodercli.Site, len(sites)+4)
	counts := stubCLICatalogProbe(t, s, func(site qodercli.Site, ctx context.Context) ([]remote.Model, error) {
		entered <- site
		<-release
		return []remote.Model{{Key: "seam-model"}}, nil
	})

	type answer struct {
		models []Model
		err    error
	}
	answers := make(chan answer, 2)
	for i := 0; i < 2; i++ {
		go func() {
			models, err := s.ListModels(context.Background())
			answers <- answer{models: models, err: err}
		}()
	}
	for range sites {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the first caller never started its probe")
		}
	}
	// Every site's probe is now blocked. A caller that spawned its own probe
	// instead of joining would reach the seam inside this window.
	time.Sleep(200 * time.Millisecond)
	for _, site := range sites {
		if got := counts[site].Load(); got != 1 {
			t.Fatalf("site %v ran %d probes while one was already in flight, want 1", site.Normalized(), got)
		}
	}
	close(release)

	want := expectedCLIModelIDs(sites, "seam-model")
	for i := 0; i < 2; i++ {
		select {
		case got := <-answers:
			if got.err != nil {
				t.Fatalf("concurrent ListModels failed: %v", got.err)
			}
			assertModelIDs(t, "concurrent ListModels", got.models, want)
		case <-time.After(5 * time.Second):
			t.Fatal("a concurrent ListModels did not return")
		}
	}
	for _, site := range sites {
		if got := counts[site].Load(); got != 1 {
			t.Fatalf("site %v ran %d probes for two callers, want exactly 1", site.Normalized(), got)
		}
	}
}

// TestCatalogWaiterIsBoundedByItsOwnContext pins the half a mutex would get
// wrong: a waiter must not inherit the owner's (possibly 15-minute) budget. Its
// own 100ms context has to end the wait, while the owner's probe, detached from
// the caller context, still finishes and lands in the catalog.
func TestCatalogWaiterIsBoundedByItsOwnContext(t *testing.T) {
	useCLICatalogDir(t)
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	sites := s.cliSites()
	if len(sites) == 0 {
		t.Skip("no CLI site is configured")
	}

	release := make(chan struct{})
	entered := make(chan qodercli.Site, len(sites)+4)
	counts := stubCLICatalogProbe(t, s, func(site qodercli.Site, ctx context.Context) ([]remote.Model, error) {
		entered <- site
		<-release
		return []remote.Model{{Key: "late-model"}}, nil
	})

	ownerDone := make(chan error, 1)
	go func() {
		_, err := s.ListModels(context.Background())
		ownerDone <- err
	}()
	// Wait until the owner holds every site's flight, so the waiter below joins
	// instead of racing to own one.
	for range sites {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the owner never started its probes")
		}
	}

	waitCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	waiterDone := make(chan error, 1)
	go func() {
		_, err := s.ListModels(waitCtx)
		waiterDone <- err
	}()
	select {
	case err := <-waiterDone:
		if err == nil {
			t.Fatal("the waiter answered from a catalog the probe had not published yet")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the waiter blocked on another caller's probe instead of its own deadline")
	}
	for _, site := range sites {
		if got := counts[site].Load(); got != 1 {
			t.Fatalf("site %v ran %d probes, want only the owner's one", site.Normalized(), got)
		}
	}

	// The waiter's context died; the owner's probe must not have.
	close(release)
	select {
	case err := <-ownerDone:
		if err != nil {
			t.Fatalf("the owner's probe failed after the waiter's context expired: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the owner's probe never finished")
	}
	for _, site := range sites {
		entry := s.cliCatalogEntry(site)
		if len(entry.names) != 1 || entry.names[0] != "late-model" {
			t.Fatalf("site %v catalog = %v, want the owner's late result", site.Normalized(), entry.names)
		}
	}
}

// TestCatalogOwnerFailureReachesTheMerge pins the owner's own listing: its
// error went to the shared flight but never to listings[i], so with no stale
// catalog mergeCLICatalogs read a failed site as one that legitimately listed
// nothing and silently shrank the merged answer.
func TestCatalogOwnerFailureReachesTheMerge(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stale bool
	}{
		{name: "cold"},
		{name: "stale", stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useCLICatalogDir(t)
			s := &Service{cfg: Config{Backend: BackendQoderCLI}}
			sites := s.cliSites()
			if len(sites) == 0 {
				t.Skip("no CLI site is configured")
			}
			if tc.stale {
				for _, site := range sites {
					s.setCLICatalog(site, []string{"stale-model"}, time.Now().Add(-time.Hour))
				}
			}
			probeErr := errors.New("probe boom")
			stubCLICatalogProbe(t, s, func(qodercli.Site, context.Context) ([]remote.Model, error) {
				return nil, probeErr
			})

			models, err := s.ListModels(context.Background())
			if tc.stale {
				// A failed owner still has a fallback: the stale catalog must be
				// served instead of the failure taking the whole answer down.
				if err != nil {
					t.Fatalf("a failed probe with a stale catalog returned %v", err)
				}
				if len(models) != len(sites) {
					t.Fatalf("merged %d models, want the %d stale ones: %v", len(models), len(sites), models)
				}
				for _, model := range models {
					if !strings.Contains(model.ID, "stale-model") {
						t.Fatalf("model %q is not the stale catalog: %v", model.ID, models)
					}
				}
				return
			}
			if err == nil {
				t.Fatalf("a cold failed probe returned %v, want the probe error", models)
			}
			if !strings.Contains(err.Error(), "probe boom") {
				t.Fatalf("the merged answer lost the failed probe: %v", err)
			}
		})
	}
}

// TestCatalogWaiterPrefersAnAnswerThatLandsAtItsDeadline pins the select race:
// when the owner publishes exactly as the waiter's deadline fires, the waiter
// reported a stale error over an answer that was already available.
func TestCatalogWaiterPrefersAnAnswerThatLandsAtItsDeadline(t *testing.T) {
	useCLICatalogDir(t)
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	sites := s.cliSites()
	if len(sites) == 0 {
		t.Skip("no CLI site is configured")
	}

	release := make(chan struct{})
	entered := make(chan qodercli.Site, len(sites)+4)
	stubCLICatalogProbe(t, s, func(site qodercli.Site, ctx context.Context) ([]remote.Model, error) {
		entered <- site
		<-release
		return []remote.Model{{Key: "owner-model"}}, nil
	})

	ownerDone := make(chan error, 1)
	go func() {
		_, err := s.ListModels(context.Background())
		ownerDone <- err
	}()
	for range sites {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the owner never started its probes")
		}
	}

	// Widen the re-check window so the test, not the scheduler, decides when the
	// owner's answer lands after the waiter's deadline.
	previous := cliCatalogWaiterGrace
	cliCatalogWaiterGrace = 10 * time.Second
	t.Cleanup(func() { cliCatalogWaiterGrace = previous })

	// The waiter's deadline has already fired when it joins the flight, so its
	// timeout branch is the only branch it can take while the owner probes.
	waiterCtx, cancel := context.WithCancel(context.Background())
	cancel()
	type answer struct {
		models []Model
		err    error
	}
	waiterDone := make(chan answer, 1)
	go func() {
		models, err := s.ListModels(waiterCtx)
		waiterDone <- answer{models: models, err: err}
	}()

	// Let the waiter reach its timeout branch with the owner still probing,
	// then publish the owner's answer inside the grace.
	time.Sleep(200 * time.Millisecond)
	close(release)

	select {
	case got := <-waiterDone:
		if got.err != nil {
			t.Fatalf("the waiter reported %v, want the owner's answer", got.err)
		}
		if len(got.models) != len(sites) {
			t.Fatalf("waiter models = %v, want the owner's catalog", got.models)
		}
		assertModelIDs(t, "waiter", got.models, expectedCLIModelIDs(sites, "owner-model"))
	case <-time.After(10 * time.Second):
		t.Fatal("the waiter never returned after the owner published")
	}
	if err := <-ownerDone; err != nil {
		t.Fatalf("the owner's probe failed: %v", err)
	}
}
