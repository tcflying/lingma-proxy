package service

import (
	"strings"
	"testing"

	"lingma-ipc-proxy/internal/qodercli"
)

// 1005 E-4 (evidence/D05): the docs promise a `cn/` prefix pins the CN site,
// but splitCLISite recognized only the intl markers, so a cn/-marked request
// was treated as site-unspecified: it could cross-fall back to the
// international site and the literal "cn/" stayed glued to the model id.

// e4BurnSiteDetection pins cliSites to a synthetic list so resolveCLISite is
// testable without touching real installs or login state.
func e4BurnSiteDetection(t *testing.T, s *Service, sites []qodercli.Site) {
	t.Helper()
	s.cliSitesOnce.Do(func() {})
	s.mu.Lock()
	s.detectedCLISites = sites
	s.mu.Unlock()
}

func TestE4Site1005CNMarkerPinsTheSite(t *testing.T) {
	for _, in := range []string{"cn/Qwen3.8-Flash", "CN/Auto", "lingma-proxy/cn/Qwen3.8-Flash-xhigh"} {
		site, model, named := splitCLISite(in)
		if site != qodercli.SiteCN || !named {
			t.Errorf("splitCLISite(%q) = %v,%q,%v want CN site with named=true (D05: cn/ must pin CN)", in, site, model, named)
		}
		if strings.Contains(model, "/") {
			t.Errorf("splitCLISite(%q) left the marker glued to the model id: %q", in, model)
		}
	}
	// The bare marker names no model, mirroring the intl rule.
	if site, _, named := splitCLISite("cn"); site != qodercli.SiteCN || named {
		t.Errorf("bare cn marker must stay an unnamed CN request, got %v,%v", site, named)
	}
}

func TestE4Site1005CNMarkerRefusesCrossSiteFallback(t *testing.T) {
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	// Only the international site is usable on this synthetic machine.
	e4BurnSiteDetection(t, s, []qodercli.Site{qodercli.SiteGlobal})

	site, model, named := splitCLISite("cn/Qwen3.8-Flash")
	if !named {
		t.Fatalf("cn/ marker not recognized before fallback check: %v,%q,%v", site, model, named)
	}
	resolved, err := s.resolveCLISite(site, named)
	if err == nil {
		t.Fatalf("D05: cn/-marked request silently crossed to the %s site (resolved=%v)", resolved.Label(), resolved)
	}
	if resolved.Normalized() != qodercli.SiteCN.Normalized() {
		t.Fatalf("fallback returned %v, want the requested CN site on the error path", resolved)
	}
}

func TestE4Site1005UnmarkedAndIntlBehaviorUnchanged(t *testing.T) {
	// Counter-cases: no marker keeps the site-unspecified default; intl keeps
	// its existing marker rule.
	if site, model, named := splitCLISite("Qwen3.8-Flash"); site != qodercli.SiteCN || named || model != "Qwen3.8-Flash" {
		t.Errorf("unmarked request changed: %v,%q,%v", site, model, named)
	}
	if site, model, named := splitCLISite("intl/Qwen3.8-Flash"); site != qodercli.SiteGlobal || !named || model != "Qwen3.8-Flash" {
		t.Errorf("intl marker changed: %v,%q,%v", site, model, named)
	}

	// Unmarked requests may still fall back to the machine's other site.
	s := &Service{cfg: Config{Backend: BackendQoderCLI}}
	e4BurnSiteDetection(t, s, []qodercli.Site{qodercli.SiteGlobal})
	resolved, err := s.resolveCLISite(qodercli.SiteCN, false)
	if err != nil || resolved.Normalized() != qodercli.SiteGlobal.Normalized() {
		t.Errorf("unmarked default fallback changed: resolved=%v err=%v", resolved, err)
	}
}

// TestE4Site1005EchoPrefixByActualSite pins the result-model echo: the prefix
// follows the site that served the turn, so a cn/-marked request echoes cn/,
// not the single global prefix (D05).
func TestE4Site1005EchoPrefixByActualSite(t *testing.T) {
	if got := cliSiteEchoPrefix(qodercli.SiteCN); got != "cn/" {
		t.Errorf("cliSiteEchoPrefix(CN) = %q, want cn/", got)
	}
	if got := cliSiteEchoPrefix(qodercli.SiteGlobal); got != cliGlobalPrefix {
		t.Errorf("cliSiteEchoPrefix(Global) = %q, want %q", got, cliGlobalPrefix)
	}
}
