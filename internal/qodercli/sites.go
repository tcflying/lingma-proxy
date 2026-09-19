package qodercli

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Site names one of the two Qoder deployments. Both ship the same worker
// runtime and share an OAuth client id, but they mint job tokens from different
// gateways and brand their environment variables.
type Site string

const (
	SiteCN     Site = "cn"
	SiteGlobal Site = "global"
)

type siteProfile struct {
	label         string
	openAPIBase   string
	envPrefix     string
	standaloneBin string
	workerPkg     string
	appName       string
	homeDirName   string
	profileNames  []string
	// needsOwnConfigDir marks the sites that must not read the desktop app's own
	// config root. The global build's ~/.qoder holds the desktop login, and a
	// CLI that finds it answers every request with "auth.getUserInfo failed:
	// token is not active" even though the same job token is accepted when the
	// gateway is queried directly. A private config root avoids the stale state.
	needsOwnConfigDir bool
}

var siteProfiles = map[Site]siteProfile{
	SiteCN: {
		label:         "Qoder CN",
		openAPIBase:   "https://openapi.qoder.com.cn",
		envPrefix:     "QODERCN_",
		standaloneBin: "qoderclicn",
		workerPkg:     "qoder-cn-agent-sdk",
		appName:       "Qoder CN",
		homeDirName:   ".qoder-cn",
		profileNames:  []string{"com.qodercn.app.stable", "com.qodercn.app.canary"},
	},
	SiteGlobal: {
		label:             "Qoder",
		openAPIBase:       "https://openapi.qoder.sh",
		envPrefix:         "QODER_",
		standaloneBin:     "qodercli",
		workerPkg:         "qoder-agent-sdk",
		appName:           "Qoder",
		homeDirName:       ".qoder",
		profileNames:      []string{"com.qoder.app.stable", "com.qoder.app.canary"},
		needsOwnConfigDir: true,
	},
}

// Normalized maps the zero Site onto the CN build, which is what every location
// created before sites existed referred to.
func (s Site) Normalized() Site {
	if s == "" {
		return SiteCN
	}
	return s
}

func (s Site) profile() siteProfile {
	return siteProfiles[s.Normalized()]
}

// Label is how the site appears in user-facing errors: "Qoder CN" or "Qoder".
func (s Site) Label() string { return s.profile().label }

func (s Site) jobTokenEnvName() string { return s.profile().envPrefix + "JOB_TOKEN" }

func (s Site) valid() bool {
	if s == "" {
		return false
	}
	switch s.Normalized() {
	case SiteCN, SiteGlobal:
		return true
	}
	return false
}

// ownConfigDir is the proxy-owned CLI config root for sites that cannot share
// the desktop app's directory. It returns "" for sites that need no isolation.
func (s Site) ownConfigDir() string {
	if !s.profile().needsOwnConfigDir {
		return ""
	}
	base, err := os.UserConfigDir()
	if err != nil || strings.TrimSpace(base) == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "lingma-proxy", "qoder-cli-"+string(s.Normalized()))
}

// EnabledSites lists the sites this process may serve, in priority order.
// A configured override wins, then LINGMA_QODERCLI_SITES narrows it to e.g. "cn";
// unset means both.
func EnabledSites() []Site {
	sitesMu.Lock()
	defer sitesMu.Unlock()
	if len(sitesOverride) > 0 {
		return append([]Site(nil), sitesOverride...)
	}
	spec := strings.ToLower(os.Getenv("LINGMA_QODERCLI_SITES"))
	var out []Site
	for _, name := range strings.FieldsFunc(spec, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '|'
	}) {
		site := Site(name)
		if !site.valid() {
			continue
		}
		if !containsSite(out, site) {
			out = append(out, site)
		}
	}
	if len(out) == 0 {
		return []Site{SiteCN, SiteGlobal}
	}
	return out
}

// sitesOverride lets a config file pin the site set, which a second desktop
// instance needs to stay international-only without an environment to inherit.
var (
	sitesMu       sync.Mutex
	sitesOverride []Site
)

// SetEnabledSites pins the sites this process serves. An empty list restores the
// environment-driven default.
func SetEnabledSites(sites []string) {
	var parsed []Site
	for _, name := range sites {
		site := Site(strings.ToLower(strings.TrimSpace(name)))
		if site.valid() && !containsSite(parsed, site) {
			parsed = append(parsed, site)
		}
	}
	sitesMu.Lock()
	defer sitesMu.Unlock()
	sitesOverride = parsed
}

// UsableSites returns the enabled sites that have both a CLI host and a login.
func UsableSites() []Site {
	var out []Site
	for _, site := range EnabledSites() {
		if AvailableSite(site) {
			out = append(out, site)
		}
	}
	return out
}

func containsSite(sites []Site, want Site) bool {
	for _, site := range sites {
		if site.Normalized() == want.Normalized() {
			return true
		}
	}
	return false
}

// siteForPath attributes a CLI path to the site whose branding it carries, so
// an explicit LINGMA_QODERCLI_BIN picks its own endpoints.
func siteForPath(path string) Site {
	lower := strings.ToLower(filepath.ToSlash(path))
	for _, site := range []Site{SiteCN, SiteGlobal} {
		profile := site.profile()
		for _, token := range []string{profile.workerPkg, profile.standaloneBin, profile.homeDirName, profile.appName} {
			if strings.Contains(lower, strings.ToLower(token)) {
				return site
			}
		}
	}
	return SiteCN
}
