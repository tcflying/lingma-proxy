package qodercli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Location points at a usable Qoder CN CLI host. Either the standalone
// qoderclicn binary, or the desktop app's Electron binary plus the bundled
// agent SDK worker runtime that ships inside its resources directory.
type Location struct {
	// HostExe is launched directly for StandaloneCLI, or with
	// ELECTRON_RUN_AS_NODE=1 for the bundled worker runtime.
	HostExe   string
	RuntimeJS string
	// EnvPrefix is what the bundled CLI expects for its own environment
	// variables, e.g. "QODERCN_" for the CN build.
	EnvPrefix  string
	ProfileDir string
}

const workerGlob = "resources/app.asar.unpacked/node_modules/@qoder-ai/*-agent-sdk/dist/_worker/qoder-worker-runtime.obf.mjs"

func (l Location) useWorker() bool { return l.RuntimeJS != "" }

// Detect finds an installed Qoder CN CLI host together with its app profile dir.
func Detect() (Location, bool) {
	if exe := strings.TrimSpace(os.Getenv("LINGMA_QODERCLI_BIN")); exe != "" && fileExists(exe) {
		return finish(Location{HostExe: exe, EnvPrefix: envPrefixFor(exe)}), true
	}
	if exe := strings.TrimSpace(os.Getenv("LINGMA_QODERCLI_RUNTIME")); exe != "" && fileExists(exe) {
		if host := strings.TrimSpace(os.Getenv("LINGMA_QODERCLI_HOST")); host != "" && fileExists(host) {
			return finish(Location{HostExe: host, RuntimeJS: exe, EnvPrefix: envPrefixFor(exe)}), true
		}
	}

	for _, root := range installRoots() {
		if standalone := standaloneCLI(root); standalone != "" {
			return finish(Location{HostExe: standalone, EnvPrefix: "QODERCN_"}), true
		}
		runtimeJS := firstMatch(filepath.Join(root, workerGlob))
		if runtimeJS == "" {
			continue
		}
		host := hostExecutable(root)
		if host == "" {
			continue
		}
		return finish(Location{HostExe: host, RuntimeJS: runtimeJS, EnvPrefix: envPrefixFor(runtimeJS)}), true
	}
	return Location{}, false
}

// Available reports whether both a CLI host and usable login state exist.
func Available() bool {
	loc, ok := Detect()
	return ok && loc.ProfileDir != "" && hasAppCredential(loc.ProfileDir)
}

func finish(loc Location) Location {
	if loc.ProfileDir == "" {
		loc.ProfileDir = detectProfileDir()
	}
	return loc
}

func standaloneCLI(root string) string {
	name := "qoderclicn"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	candidate := filepath.Join(root, "bin", name)
	if fileExists(candidate) {
		return candidate
	}
	return ""
}

func installRoots() []string {
	var roots []string
	if runtime.GOOS == "windows" {
		roots = append(roots, registryInstallRoots()...)
	}
	if home, err := os.UserHomeDir(); err == nil {
		roots = append(roots,
			filepath.Join(home, ".qoder-cn", "bin", "qoderclicn"),
			filepath.Join(home, "AppData", "Local", "Programs", "Qoder CN"),
			filepath.Join(home, "AppData", "Local", "Programs", "Qoder"),
			"/Applications/Qoder CN.app",
			"/Applications/Qoder.app",
		)
	}
	for _, envName := range []string{"ProgramFiles", "ProgramFiles(x86)", "LOCALAPPDATA"} {
		if base := strings.TrimSpace(os.Getenv(envName)); base != "" {
			roots = append(roots, filepath.Join(base, "Qoder CN"), filepath.Join(base, "Qoder"))
		}
	}
	for _, path := range filepath.SplitList(os.Getenv("PATH")) {
		trimmed := strings.TrimSpace(path)
		if trimmed == "" {
			continue
		}
		if base := appRootFromPath(trimmed); base != "" {
			roots = append(roots, base)
		}
	}
	if runtime.GOOS == "windows" {
		roots = append(roots, `C:\Qoder CN`)
	}
	return uniqueNonEmpty(roots)
}

// appRootFromPath walks up from a PATH entry such as
// "C:\Qoder CN\resources\bin" looking for an Electron installation.
func appRootFromPath(entry string) string {
	current := filepath.Clean(entry)
	for i := 0; i < 5; i++ {
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		if hostExecutable(parent) != "" {
			return parent
		}
		current = parent
	}
	return ""
}

// hostExecutable returns the binary to launch with ELECTRON_RUN_AS_NODE=1.
func hostExecutable(root string) string {
	var names []string
	if runtime.GOOS == "darwin" {
		names = []string{
			filepath.Join("Contents", "MacOS", "Qoder CN"),
			filepath.Join("Contents", "MacOS", "Qoder"),
		}
	} else {
		names = []string{"Qoder CN.exe", "Qoder.exe"}
	}
	for _, name := range names {
		if candidate := filepath.Join(root, name); fileExists(candidate) {
			return candidate
		}
	}
	return ""
}

func envPrefixFor(path string) string {
	if strings.Contains(strings.ToLower(path), "qoder-cn") {
		return "QODERCN_"
	}
	return "QODER_"
}

func firstMatch(pattern string) string {
	matches, err := filepath.Glob(pattern)
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
