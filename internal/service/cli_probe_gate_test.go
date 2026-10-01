package service

import (
	"context"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/qodercli"
)

// seedCLIModels puts names straight into the in-memory tier, which is the state a
// completed discovery leaves behind. setCLICatalog fills the on-disk tier and does
// not touch this one, so seeding the wrong map makes a test look like it exercises
// the warm path when it is actually re-discovering.
func seedCLIModels(s *Service, site qodercli.Site, names []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cliModels == nil {
		s.cliModels = make(map[qodercli.Site][]string)
	}
	s.cliModels[site.Normalized()] = names
}

// TestCachedCLIModelsDoesNotQueueBehindAnotherProbe pins the half of the guard that
// can actually be got wrong.
//
// A mutex here would be the obvious implementation and the wrong one: the holder
// can be the startup warm-up, which is allowed the priming budget rather than the
// request-path one, so a queued client would inherit fifteen minutes and time out
// with it. That is the coupling the note above listCLIMergedModels exists to
// prevent. So the guard is a gate, and a caller that finds it held answers from
// what is already in hand instead of waiting.
//
// Deterministic on purpose: the gate is taken by this test, so the assertion needs
// no timing and cannot flake. The bound is generous -- a real discovery here costs a
// CLI cold start, measured at 24-46s -- so failing still fails loudly.
func TestCachedCLIModelsDoesNotQueueBehindAnotherProbe(t *testing.T) {
	useCLICatalogDir(t)
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	sites := s.cliSites()
	if len(sites) == 0 {
		t.Skip("no CLI site is configured")
	}
	site := sites[0]

	// Stand in for a discovery already in flight.
	s.cliProbeGate.Lock()
	defer s.cliProbeGate.Unlock()

	done := make(chan []string, 1)
	go func() { done <- s.cachedCLIModels(context.Background(), site, 0) }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Fatalf("returned %v from an empty cache; it must not have discovered anything", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cachedCLIModels waited on the probe gate; a client must answer, not queue")
	}
}

// TestCachedCLIModelsStillAnswersTheCallerItSkippedFor covers the case that makes
// skipping safe rather than lossy: the in-flight discovery has already written the
// catalog, so the caller that did not wait gets a real answer anyway.
func TestCachedCLIModelsStillAnswersTheCallerItSkippedFor(t *testing.T) {
	useCLICatalogDir(t)
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	sites := s.cliSites()
	if len(sites) == 0 {
		t.Skip("no CLI site is configured")
	}
	site := sites[0]
	seedCLIModels(s, site, []string{"holder-a", "holder-b"})

	// The gate is held by the run that produced those names.
	s.cliProbeGate.Lock()
	defer s.cliProbeGate.Unlock()

	got := s.cachedCLIModels(context.Background(), site, 0)
	if len(got) != 2 || got[0] != "holder-a" {
		t.Fatalf("cachedCLIModels = %v, want the names already in hand", got)
	}
}

// TestCachedCLIModelsDoesNotProbeWhileAnotherDoesItElseWhere is the plain warm-cache
// case, and the one the guard must not regress: a populated in-memory catalog is
// answered from memory, with the gate held or not.
func TestCachedCLIModelsDoesNotProbeWhileAnotherDoesItElseWhere(t *testing.T) {
	useCLICatalogDir(t)
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	sites := s.cliSites()
	if len(sites) == 0 {
		t.Skip("no CLI site is configured")
	}
	// Names no installed CLI would return, so a re-discovery cannot pass by
	// coincidence.
	seedCLIModels(s, sites[0], []string{"only-from-memory"})

	start := time.Now()
	got := s.cachedCLIModels(context.Background(), sites[0], 0)
	if len(got) != 1 || got[0] != "only-from-memory" {
		t.Fatalf("cachedCLIModels = %v, want the in-memory name", got)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("a warm cache took %s; it should be a map read", elapsed)
	}
}

// TestCLIProbeGateIsPerServiceNotGlobal guards against the obvious "fix" for the
// original stampede: making the gate a package variable. Two Service values in one
// process would then serialise against each other, and one wedged backend would
// stall the other's catalog.
func TestCLIProbeGateIsPerServiceNotGlobal(t *testing.T) {
	first := &Service{}
	second := &Service{}
	first.cliProbeGate.Lock()
	defer first.cliProbeGate.Unlock()
	done := make(chan struct{})
	go func() {
		second.cliProbeGate.Lock()
		second.cliProbeGate.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a second Service is blocked by the first one's gate; it must be per-instance")
	}
}

// TestCLIProbeGateIsReleasedAfterAFailedProbe pins that the guard is a gate and not
// a latch: whatever listCLIMergedModels does here, the gate must not stay taken. A
// leaked gate would be a silent, permanent outage of the catalog for this process --
// every later read would come back empty and nothing would say why.
func TestCLIProbeGateIsReleasedAfterAFailedProbe(t *testing.T) {
	useCLICatalogDir(t)
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	sites := s.cliSites()
	if len(sites) == 0 {
		t.Skip("no CLI site is configured")
	}
	s.cachedCLIModels(context.Background(), sites[0], 0)
	if !s.cliProbeGate.TryLock() {
		t.Fatal("the probe gate is still held after cachedCLIModels returned")
	}
	s.cliProbeGate.Unlock()
}

// TestFailedProbeServesStaleWithinFailureTTL pins the negative cache: a site whose
// probe failed less than cliProbeFailureTTL ago answers from its stale names
// without spawning another probe. Measured live before the fix: with CN
// unreachable at the TLS layer, three consecutive /v1/models took 8046/8028/
// 8030ms each -- a full cliProbeTimeout tax on every call, all serving the same
// stale disk list anyway. The bound is 2s because a real probe cannot finish
// faster than that (measured 3-17s warm, 8s timeout), so a green run here means
// no probe ran at all.
func TestFailedProbeServesStaleWithinFailureTTL(t *testing.T) {
	useCLICatalogDir(t)
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	sites := s.cliSites()
	if len(sites) == 0 {
		t.Skip("no CLI site is configured")
	}
	// Every served site gets a stale-but-usable catalog and a fresh failure mark,
	// so none of them may probe regardless of which sites exist on this box.
	for _, site := range sites {
		s.setCLICatalog(site, []string{"stale-" + string(site.Normalized())}, time.Now().Add(-time.Hour))
		s.markCLIProbeFailed(site)
	}

	start := time.Now()
	models, err := s.ListModels(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the negative-cache path errored: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("the negative-cache path returned no models")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("a recently failed site probed again: %s; it must serve stale instantly", elapsed)
	}
}

// TestSuccessClearsFailureMark pins that a successful probe supersedes the
// failure mark: the entry is replaced whole, so a site that recovers is probed
// on the normal TTL schedule from then on rather than lingering in the failure
// window until it expires.
func TestSuccessClearsFailureMark(t *testing.T) {
	useCLICatalogDir(t)
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	sites := s.cliSites()
	if len(sites) == 0 {
		t.Skip("no CLI site is configured")
	}
	site := sites[0]
	s.setCLICatalog(site, []string{"a"}, time.Now().Add(time.Hour))
	s.markCLIProbeFailed(site)
	if s.cliCatalogEntry(site).failedAt.IsZero() {
		t.Fatal("markCLIProbeFailed did not record the failure")
	}
	s.setCLICatalog(site, []string{"b"}, time.Now().Add(time.Hour))
	if !s.cliCatalogEntry(site).failedAt.IsZero() {
		t.Fatal("a successful probe must clear the failure mark with the entry it replaces")
	}
}

// TestFailureMarkAloneIsNotACatalog pins the guard on markCLIProbeFailed: a site
// with no names to serve must not gain a catalog entry from its failure, or the
// negative cache would turn "never answered" into "recently answered, skip the
// probe" and a first-ever discovery would be suppressed for a whole TTL window.
func TestFailureMarkAloneIsNotACatalog(t *testing.T) {
	useCLICatalogDir(t)
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	sites := s.cliSites()
	if len(sites) == 0 {
		t.Skip("no CLI site is configured")
	}
	s.markCLIProbeFailed(sites[0])
	if entry := s.cliCatalogEntry(sites[0]); entry.names != nil {
		t.Fatal("markCLIProbeFailed created a catalog entry; it must only annotate an existing one")
	}
}

// TestFailedProbeServesDiskTierWithinFailureTTL is the restart shape the
// in-memory test cannot catch: the process is new, so the only record of the
// site's names is the on-disk catalog, and the in-memory map is empty. Before
// the seeding in markCLIProbeFailed, the failure mark silently no-opped for
// exactly this case (names==nil guard hit the empty map, never the disk tier)
// and a TLS-dead site re-probed on every /v1/models -- measured live at 8.0s
// per call, three in a row, against a production deployment.
func TestFailedProbeServesDiskTierWithinFailureTTL(t *testing.T) {
	useCLICatalogDir(t)
	// Seed only the disk tier, the way a previous process leaves it behind.
	for _, site := range (&Service{cfg: Config{Backend: BackendQoderCLI}}).cliSites() {
		writeCLICatalog(site, []string{"disk-" + string(site.Normalized())}, time.Now().Add(-time.Hour))
	}
	// Fresh process: nothing in memory.
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	sites := s.cliSites()
	if len(sites) == 0 {
		t.Skip("no CLI site is configured")
	}
	for _, site := range sites {
		s.markCLIProbeFailed(site)
	}

	start := time.Now()
	models, err := s.ListModels(context.Background())
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("the disk-tier negative-cache path errored: %v", err)
	}
	if len(models) == 0 {
		t.Fatal("the disk-tier negative-cache path returned no models")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("a disk-seeded site probed again after a recent failure: %s; it must serve stale instantly", elapsed)
	}
}
