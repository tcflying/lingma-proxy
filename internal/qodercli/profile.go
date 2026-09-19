package qodercli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const authFile = "auth.v1.dat"

type appUser struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// appCredential is the desktop app's persisted OAuth device credential.
type appCredential struct {
	Token                 string    `json:"token"`
	RefreshToken          string    `json:"refreshToken"`
	ExpiresAt             time.Time `json:"expiresAt"`
	RefreshTokenExpiresAt time.Time `json:"refreshTokenExpiresAt"`
	User                  appUser   `json:"user"`
}

func (c appCredential) valid() bool { return strings.TrimSpace(c.Token) != "" }

// profileCandidates lists Electron userData directories that may hold the login.
func profileCandidates(site Site) []string {
	if explicit := explicitProfileDir(site); explicit != "" {
		return []string{explicit}
	}
	var roots []string
	switch runtime.GOOS {
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			roots = append(roots, filepath.Join(home, "Library", "Application Support"))
		}
	case "windows":
		roots = append(roots, appDataRoaming())
	default:
		if base := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); base != "" {
			roots = append(roots, base)
		}
		if home, err := os.UserHomeDir(); err == nil {
			roots = append(roots, filepath.Join(home, ".config"))
		}
	}
	names := site.profile().profileNames
	out := make([]string, 0, len(names)*len(roots))
	for _, root := range roots {
		if root == "" {
			continue
		}
		for _, name := range names {
			out = append(out, filepath.Join(root, name))
		}
	}
	return out
}

// explicitProfileDir applies LINGMA_QODERCLI_PROFILE only where it names the
// site being resolved, so one override cannot serve both credential chains.
func explicitProfileDir(site Site) string {
	explicit := strings.TrimSpace(os.Getenv("LINGMA_QODERCLI_PROFILE"))
	if explicit == "" {
		return ""
	}
	dir := expandHome(explicit)
	if inferred := siteOfProfileDir(dir); inferred != "" && inferred != site.Normalized() {
		return ""
	}
	return dir
}

func siteOfProfileDir(dir string) Site {
	base := strings.ToLower(filepath.Base(dir))
	switch {
	case strings.Contains(base, "qodercn"):
		return SiteCN
	case strings.Contains(base, "qoder.app"):
		return SiteGlobal
	}
	return ""
}

func detectProfileDir(site Site) string {
	for _, dir := range profileCandidates(site) {
		if hasAppCredential(dir) {
			return dir
		}
	}
	return ""
}

func hasAppCredential(dir string) bool {
	return dir != "" && fileExists(filepath.Join(dir, authFile))
}

func appDataRoaming() string {
	if value := strings.TrimSpace(os.Getenv("APPDATA")); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(home, "AppData", "Roaming")
	}
	return filepath.Join(home, ".config")
}

func expandHome(path string) string {
	if path != "~" && !strings.HasPrefix(path, "~"+string(filepath.Separator)) && !strings.HasPrefix(path, "~/") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, filepath.ToSlash(strings.TrimPrefix(path[1:], "/")))
}
