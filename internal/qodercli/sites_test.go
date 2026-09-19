package qodercli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnabledSitesDefaultsToBothSites(t *testing.T) {
	t.Setenv("LINGMA_QODERCLI_SITES", "")
	got := EnabledSites()
	if len(got) != 2 || got[0] != SiteCN || got[1] != SiteGlobal {
		t.Fatalf("EnabledSites() = %v, want [cn global]", got)
	}
}

func TestEnabledSitesHonoursNarrowing(t *testing.T) {
	t.Setenv("LINGMA_QODERCLI_SITES", " global , cn , banana ,, global ")
	got := EnabledSites()
	if len(got) != 2 || got[0] != SiteGlobal || got[1] != SiteCN {
		t.Fatalf("EnabledSites() = %v, want [global cn] with the bogus entry dropped", got)
	}

	t.Setenv("LINGMA_QODERCLI_SITES", "cn")
	if got := EnabledSites(); len(got) != 1 || got[0] != SiteCN {
		t.Fatalf("EnabledSites() = %v, want [cn]", got)
	}
}

func TestSiteBranding(t *testing.T) {
	cases := []struct {
		site     Site
		label    string
		jobToken string
	}{
		{SiteCN, "Qoder CN", "QODERCN_JOB_TOKEN"},
		{SiteGlobal, "Qoder", "QODER_JOB_TOKEN"},
		// Locations built before sites existed referred to the CN build.
		{Site(""), "Qoder CN", "QODERCN_JOB_TOKEN"},
	}
	for _, tc := range cases {
		if got := tc.site.Label(); got != tc.label {
			t.Fatalf("Site(%q).Label() = %q, want %q", tc.site, got, tc.label)
		}
		if got := tc.site.jobTokenEnvName(); got != tc.jobToken {
			t.Fatalf("Site(%q).jobTokenEnvName() = %q, want %q", tc.site, got, tc.jobToken)
		}
		if tc.site.Normalized() != tc.site && tc.site != "" {
			t.Fatalf("Site(%q).Normalized() = %q", tc.site, tc.site.Normalized())
		}
	}
}

func TestSiteForPathAttributesTheInstallToItsOwnSite(t *testing.T) {
	windowsCN := filepath.Join("C:" + string(os.PathSeparator) + "Qoder CN" + string(os.PathSeparator) +
		"resources" + string(os.PathSeparator) + "app.asar.unpacked" + string(os.PathSeparator) +
		"node_modules" + string(os.PathSeparator) + "@qoder-ai" + string(os.PathSeparator) +
		"qoder-cn-agent-sdk" + string(os.PathSeparator) + "dist" + string(os.PathSeparator) +
		"_worker" + string(os.PathSeparator) + "qoder-worker-runtime.obf.mjs")
	windowsGlobal := filepath.Join("C:" + string(os.PathSeparator) + "Program Files" + string(os.PathSeparator) +
		"Qoder" + string(os.PathSeparator) + "resources" + string(os.PathSeparator) + "app.asar.unpacked" +
		string(os.PathSeparator) + "node_modules" + string(os.PathSeparator) + "@qoder-ai" +
		string(os.PathSeparator) + "qoder-agent-sdk" + string(os.PathSeparator) + "dist" +
		string(os.PathSeparator) + "_worker" + string(os.PathSeparator) + "qoder-worker-runtime.obf.mjs")

	cases := map[string]Site{
		windowsCN:     SiteCN,
		windowsGlobal: SiteGlobal,
		"/home/u/.qoder-cn/bin/qoderclicn/qoderclicn":              SiteCN,
		"/home/u/.qoder/bin/qodercli/qodercli":                     SiteGlobal,
		"C:\\Qoder CN\\Qoder CN.exe":                               SiteCN,
		"C:\\Users\\u\\AppData\\Local\\Programs\\Qoder\\Qoder.exe": SiteGlobal,
	}
	for path, want := range cases {
		if got := siteForPath(path); got != want {
			t.Fatalf("siteForPath(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestGlobalSiteNeedsItsOwnConfigDir pins the reason the international build is
// launched with --config-dir: sharing ~/.qoder makes its CLI reject every job
// token as inactive.
func TestGlobalSiteNeedsItsOwnConfigDir(t *testing.T) {
	if SiteCN.ownConfigDir() != "" {
		t.Fatalf("the CN site must keep using its default config root")
	}
	dir := SiteGlobal.ownConfigDir()
	if dir == "" {
		t.Fatal("global site has no isolated config dir")
	}
	if filepath.Base(dir) != "qoder-cli-global" {
		t.Fatalf("ownConfigDir() = %q", dir)
	}
	if !siteProfiles[SiteGlobal].needsOwnConfigDir {
		t.Fatal("global site must be marked as needing its own config dir")
	}
}
