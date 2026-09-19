package qodercli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Location points at a usable Qoder CLI host for one site. Either the standalone
// qodercli/qoderclicn binary, or a desktop app's Electron binary plus the agent
// SDK worker runtime that ships inside its resources directory.
type Location struct {
	// HostExe is launched directly for StandaloneCLI, or with
	// ELECTRON_RUN_AS_NODE=1 for the bundled worker runtime.
	HostExe   string
	RuntimeJS string
	Site      Site
	// EnvPrefix is what the bundled CLI expects for its own environment
	// variables, e.g. "QODERCN_" for the CN build.
	EnvPrefix  string
	ProfileDir string
	// ConfigDir is passed as --config-dir when the site must stay out of the
	// desktop app's own config root.
	ConfigDir string
}

// DetectSite finds the CLI host installed for one site.
func DetectSite(site Site) (Location, bool) {
	if loc, ok := explicitLocation(); ok {
		if loc.Site.Normalized() != site.Normalized() {
			return Location{}, false
		}
		return finish(loc), true
	}
	for _, root := range installRoots(site) {
		if standalone := standaloneCLI(root, site); standalone != "" {
			return finish(newLocation(site, standalone, "")), true
		}
		runtimeJS := firstMatch(filepath.Join(root, workerGlob(site)))
		if runtimeJS == "" {
			continue
		}
		host := hostExecutable(root, site)
		if host == "" {
			continue
		}
		return finish(newLocation(site, host, runtimeJS)), true
	}
	return Location{}, false
}

// Available reports whether any enabled site has both a CLI host and a login.
func Available() bool { return len(UsableSites()) > 0 }

// AvailableSite reports whether both a CLI host and usable login state exist.
func AvailableSite(site Site) bool {
	loc, ok := DetectSite(site)
	return ok && loc.ProfileDir != "" && hasAppCredential(loc.ProfileDir)
}

// explicitLocation honours the host overrides an operator sets to pin a build.
func explicitLocation() (Location, bool) {
	if exe := strings.TrimSpace(os.Getenv("LINGMA_QODERCLI_BIN")); exe != "" && fileExists(exe) {
		return newLocation(siteForPath(exe), exe, ""), true
	}
	if rt := strings.TrimSpace(os.Getenv("LINGMA_QODERCLI_RUNTIME")); rt != "" && fileExists(rt) {
		host := strings.TrimSpace(os.Getenv("LINGMA_QODERCLI_HOST"))
		if host != "" && fileExists(host) {
			return newLocation(siteForPath(rt), host, rt), true
		}
	}
	return Location{}, false
}

func newLocation(site Site, host, runtimeJS string) Location {
	site = site.Normalized()
	return Location{
		HostExe:   host,
		RuntimeJS: runtimeJS,
		Site:      site,
		EnvPrefix: site.profile().envPrefix,
	}
}

func finish(loc Location) Location {
	if loc.Site == "" {
		loc.Site = SiteCN
	}
	if loc.EnvPrefix == "" {
		loc.EnvPrefix = loc.Site.profile().envPrefix
	}
	if loc.ProfileDir == "" {
		loc.ProfileDir = detectProfileDir(loc.Site)
	}
	if loc.ConfigDir == "" {
		loc.ConfigDir = loc.Site.ownConfigDir()
	}
	return loc
}

func (l Location) useWorker() bool { return l.RuntimeJS != "" }

func standaloneCLI(root string, site Site) string {
	name := site.profile().standaloneBin
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidate := filepath.Join(root, "bin", name)
	if fileExists(candidate) {
		return candidate
	}
	return ""
}

func installRoots(site Site) []string {
	profile := site.profile()
	var roots []string
	if runtime.GOOS == "windows" {
		roots = append(roots, registryInstallRoots()...)
	}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots,
			filepath.Join(home, profile.homeDirName, "bin", profile.standaloneBin),
			filepath.Join(home, "AppData", "Local", "Programs", profile.appName),
			filepath.Join("/Applications", profile.appName+".app"),
		)
	}
	for _, envName := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
		if base := strings.TrimSpace(os.Getenv(envName)); base != "" {
			roots = append(roots, filepath.Join(base, profile.appName))
		}
	}
	for _, path := range filepath.SplitList(os.Getenv("PATH")) {
		trimmed := strings.TrimSpace(path)
		if trimmed == "" {
			continue
		}
		if base := appRootFromPath(trimmed, site); base != "" {
			roots = append(roots, base)
		}
	}
	if runtime.GOOS == "windows" {
		roots = append(roots, filepath.Join(`C:\`, profile.appName))
	}
	return uniqueNonEmpty(roots)
}

// appRootFromPath walks up from a PATH entry such as
// "C:\Qoder CN\resources\bin" looking for an Electron installation.
func appRootFromPath(entry string, site Site) string {
	current := filepath.Clean(entry)
	for i := 0; i < 5; i++ {
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		if hostExecutable(parent, site) != "" {
			return parent
		}
		current = parent
	}
	return ""
}

// hostExecutable returns the binary to launch with ELECTRON_RUN_AS_NODE=1.
func hostExecutable(root string, site Site) string {
	app := site.profile().appName
	var names []string
	if runtime.GOOS == "darwin" {
		names = []string{
			filepath.Join("Contents", "MacOS", app),
			filepath.Join("Contents", "MacOS", "Electron"),
		}
	} else {
		names = []string{app + ".exe", app}
	}
	for _, name := range names {
		if candidate := filepath.Join(root, name); fileExists(candidate) {
			return candidate
		}
	}
	return ""
}

func workerGlob(site Site) string {
	return filepath.Join("resources", "app.asar.unpacked", "node_modules", "@qoder-ai",
		site.profile().workerPkg, "dist", "_worker", "qoder-worker-runtime.obf.mjs")
}

func firstMatch(pattern string) string {
	matches, err := filepath.Glob(filepath.FromSlash(pattern))
	if err != nil {
		return ""
	}
	for _, match := range matches {
		if fileExists(match) {
			return match
		}
	}
	return ""
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func uniqueNonEmpty(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := value
		if runtime.GOOS == "windows" {
			key = strings.ToLower(filepath.Clean(key))
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, value)
	}
	return out
}
