package qodercli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// useFakeHome points os.UserHomeDir at an empty directory. Install roots and
// profile candidates are all home-relative, so the machine running the test must
// not get to decide the outcome.
func useFakeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	// Windows reads USERPROFILE, everything else reads HOME; only one of the two is
	// consulted per platform, so setting both keeps the test portable.
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	got, err := os.UserHomeDir()
	if err != nil || filepath.Clean(got) != filepath.Clean(home) {
		t.Fatalf("os.UserHomeDir() = %q (%v), this test needs it to follow %q", got, err, home)
	}
	return filepath.Clean(got)
}

func standaloneBinName(site Site) string {
	name := site.profile().standaloneBin
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	return name
}

func offersPath(paths []string, want string) bool {
	for _, path := range paths {
		if filepath.Clean(path) == filepath.Clean(want) {
			return true
		}
	}
	return false
}

// TestInstallRootsOfferTheProfileHome is Q4: the home-relative root named the CLI
// binary itself, and standaloneCLI appends bin/<name> on top of whatever root it is
// handed, so the probe looked for .../bin/qoderclicn/bin/qoderclicn.exe and a
// machine with only the CLI installed reported "not installed".
func TestInstallRootsOfferTheProfileHome(t *testing.T) {
	home := useFakeHome(t)
	profileHome := filepath.Join(home, SiteCN.profile().homeDirName)
	bin := filepath.Join(profileHome, "bin", standaloneBinName(SiteCN))
	if err := os.MkdirAll(filepath.Dir(bin), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("fake cli"), 0o700); err != nil {
		t.Fatal(err)
	}

	roots := installRoots(SiteCN)
	if !offersPath(roots, profileHome) {
		t.Fatalf("installRoots(SiteCN) = %v, want the profile home %q among them", roots, profileHome)
	}
	if got := standaloneCLI(profileHome, SiteCN); got != bin {
		t.Fatalf("standaloneCLI(%q) = %q, want %q", profileHome, got, bin)
	}
}

// TestAvailableSiteRequiresADecodableLogin is C4. Existence of auth.v1.dat proved
// nothing: the envelope only opens for the interactive Windows user that wrote it,
// so the site was chosen as a backend and then every request failed with "login
// state unavailable" for the rest of the process.
func TestAvailableSiteRequiresADecodableLogin(t *testing.T) {
	home := useFakeHome(t)
	cli := filepath.Join(home, "qoderclicn.exe")
	if err := os.WriteFile(cli, []byte("fake cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINGMA_QODERCLI_BIN", cli)
	t.Setenv("LINGMA_QODERCLI_JOB_TOKEN", "")

	var probes int
	original := probeLogin
	t.Cleanup(func() { probeLogin = original })
	probeLogin = func(string) (appCredential, error) {
		probes++
		return appCredential{}, errors.New("simulated: this process cannot open the DPAPI envelope")
	}

	// A login file that never decodes must not count as available, however often
	// the site list is rebuilt -- and the decode is only worth attempting once.
	profile := filepath.Join(home, "com.qodercn.app.stable")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, authFile), []byte("not an envelope"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINGMA_QODERCLI_PROFILE", profile)
	if AvailableSite(SiteCN) {
		t.Fatal("an undecodable login state must not make the site available")
	}
	if AvailableSite(SiteCN) {
		t.Fatal("an undecodable login state must not make the site available")
	}
	if probes != 1 {
		t.Fatalf("the login was decoded %d times, want it memoized once per process", probes)
	}

	// The same site does come back available when the state really opens.
	unreadable := probes
	probeLogin = func(string) (appCredential, error) {
		probes++
		return appCredential{Token: "device-token"}, nil
	}
	other := filepath.Join(home, "com.qodercn.app.canary")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, authFile), []byte("v10 something"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINGMA_QODERCLI_PROFILE", other)
	if !AvailableSite(SiteCN) {
		t.Fatal("a login state that decodes must keep the site available")
	}
	if probes != unreadable+1 {
		t.Fatalf("decoded %d extra times, want one probe for the new profile dir", probes-unreadable)
	}

	// The other half: an operator-supplied job token is the whole credential, so a
	// host with no profile directory at all is still usable, and the pointless
	// decode must not even be attempted.
	t.Setenv("LINGMA_QODERCLI_PROFILE", filepath.Join(home, "com.qodercn.app.nologin"))
	t.Setenv("LINGMA_QODERCLI_JOB_TOKEN", `{"token":"operator-token"}`)
	before := probes
	if !AvailableSite(SiteCN) {
		t.Fatal("LINGMA_QODERCLI_JOB_TOKEN has to stand in for the desktop login")
	}
	if probes != before {
		t.Fatalf("the env job token still paid for %d login decodes", probes-before)
	}
}

// TestUsableEnvJobTokenIsTheOneJobTokenAccepts applies C4's lesson to the other
// credential: presence of LINGMA_QODERCLI_JOB_TOKEN proved nothing, so a value that
// never decodes marked the site available and then failed every request for the rest
// of the process.
func TestUsableEnvJobTokenIsTheOneJobTokenAccepts(t *testing.T) {
	home := useFakeHome(t)
	cli := filepath.Join(home, "qoderclicn.exe")
	if err := os.WriteFile(cli, []byte("fake cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LINGMA_QODERCLI_BIN", cli)
	t.Setenv("LINGMA_QODERCLI_PROFILE", filepath.Join(home, "com.qodercn.app.nologin"))

	source := NewTokenSource("", SiteCN)
	for _, tc := range []struct {
		value string
		ok    bool
	}{
		{value: `{"token":"operator-token"}`, ok: true},
		{value: `0`, ok: false},
		{value: `notjson`, ok: false},
		{value: `{"expires_at":3600}`, ok: false},
		{value: `{"token":"   "}`, ok: false},
	} {
		t.Setenv("LINGMA_QODERCLI_JOB_TOKEN", tc.value)
		if got := AvailableSite(SiteCN); got != tc.ok {
			t.Fatalf("AvailableSite with %q = %v, want %v", tc.value, got, tc.ok)
		}
		if _, err := source.JobToken(context.Background()); (err == nil) != tc.ok {
			t.Fatalf("JobToken with %q disagreed with the probe: err=%v", tc.value, err)
		}
	}
}
