package remote

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeRemoteEndpointConfig drops a plain-text line into a file the candidate scan
// always reads, so a test can feed the scanner without touching the real home.
func writeRemoteEndpointConfig(t *testing.T, home, rawURL string) {
	t.Helper()
	path := filepath.Join(home, ".config", "lingma-proxy", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	body := "2026-09-24 INFO endpoint config: " + rawURL + "/algo/api/v2/model/list\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func writeBaseURLCacheFile(t *testing.T, rawURL string, updatedAt time.Time) {
	t.Helper()
	path, err := baseURLCachePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(baseURLCacheFile{URL: rawURL, UpdatedAt: updatedAt.Format(time.RFC3339)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func hintURLs(hints []BaseURLHint) []string {
	out := make([]string, 0, len(hints))
	for _, hint := range hints {
		out = append(out, hint.URL)
	}
	return out
}

// R4: a host that is not on the enterprise allowlist is only noise from a log line.
// Scoring it above the official endpoint handed every signed request to it.
func TestBaseURLHintScoreIgnoresUnknownHosts(t *testing.T) {
	if got := baseURLHintScore("https://ci.example-corp.internal"); got != 0 {
		t.Fatalf("unknown host score = %d, want 0", got)
	}
	if got := baseURLHintScore(DefaultBaseURL); baseURLHintScore("https://ci.example-corp.internal") >= got {
		t.Fatalf("unknown host scores %d, official endpoint scores %d", baseURLHintScore("https://ci.example-corp.internal"), got)
	}
	// The allowlist still has to carry enterprise hosts; the score comes from there.
	if got := baseURLHintScore("https://lingma.asiainfo.com"); got <= baseURLHintScore(DefaultBaseURL) {
		t.Fatalf("enterprise allowlist host scores %d, want above the official %d", got, baseURLHintScore(DefaultBaseURL))
	}
	if !isKnownEnterpriseRemoteHost("qoder.example.com") {
		t.Fatal("expected the shared allowlist to keep recognising these hosts")
	}
	if got := baseURLHintScore("https://qoder.example.com"); got != 90 {
		t.Fatalf("allowlist host via qoder score = %d, want 90", got)
	}
}

// R4 end to end: one CI link in a scanned log must not become candidates[0].
func TestResolveBaseURLCandidatesKeepsUnknownHostBehindOfficial(t *testing.T) {
	home := sandboxCandidateEnv(t)
	writeRemoteEndpointConfig(t, home, "https://ci.example-corp.internal")

	hints := ResolveBaseURLCandidates()
	if len(hints) < 2 {
		t.Fatalf("hints = %#v", hints)
	}
	if got := hints[0].URL; got != DefaultBaseURL {
		t.Fatalf("first hint = %q, want the official endpoint ahead of a log-scanned unknown host (%#v)", got, hintURLs(hints))
	}
}

// R5: the cached domain is a hint with a leash, not a permanent owner of first place.
func TestStaleCachedBaseURLHintLosesFirstPlace(t *testing.T) {
	sandboxCandidateEnv(t)
	writeBaseURLCacheFile(t, "https://lingma.asiainfo.com", time.Now().Add(-cachedBaseURLHintMaxAge-time.Hour))

	hints := ResolveBaseURLCandidates()
	if len(hints) == 0 {
		t.Fatal("expected candidates")
	}
	if got := hints[0].URL; got != DefaultBaseURL {
		t.Fatalf("first hint = %q, want a stale cached domain unfavoured behind the official endpoint (%#v)", got, hintURLs(hints))
	}
	found := false
	for _, hint := range hints[1:] {
		if hint.URL == "https://lingma.asiainfo.com" {
			found = true
		}
	}
	if !found {
		t.Fatalf("stale cached domain dropped from the fallback list entirely: %#v", hintURLs(hints))
	}
}

// R5 counterpart: a recently confirmed domain still outranks the official endpoint.
func TestFreshCachedBaseURLHintKeepsPriority(t *testing.T) {
	sandboxCandidateEnv(t)
	writeBaseURLCacheFile(t, "https://lingma.asiainfo.com", time.Now().Add(-time.Minute))

	hints := ResolveBaseURLCandidates()
	if len(hints) == 0 {
		t.Fatal("expected candidates")
	}
	if got := hints[0]; got.URL != "https://lingma.asiainfo.com" || got.Source != "last successful remote domain" {
		t.Fatalf("first hint = %+v, want the freshly confirmed domain", got)
	}
}

// R5: the cached hint is ranked, not pinned: a better-scoring configured domain
// takes the first request.
func TestCachedBaseURLHintRanksBehindBetterScoredConfig(t *testing.T) {
	home := sandboxCandidateEnv(t)
	writeBaseURLCacheFile(t, "https://lingma.asiainfo.com", time.Now())
	writeRemoteEndpointConfig(t, home, "https://ai-example-cn-beijing.rdc.aliyuncs.com")

	hints := ResolveBaseURLCandidates()
	if len(hints) < 2 {
		t.Fatalf("hints = %#v", hints)
	}
	if got := hints[0].URL; got != "https://ai-example-cn-beijing.rdc.aliyuncs.com" {
		t.Fatalf("first hint = %q, want the higher-scoring configured domain (%#v)", got, hintURLs(hints))
	}
	if got := hints[1]; got.URL != "https://lingma.asiainfo.com" || got.Source != "last successful remote domain" {
		t.Fatalf("second hint = %+v, want the cached domain to keep second place", got)
	}
}
