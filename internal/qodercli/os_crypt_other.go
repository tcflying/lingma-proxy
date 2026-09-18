//go:build !windows

package qodercli

import "fmt"

// registryInstallRoots has no equivalent outside Windows; discovery falls back
// to the standard install locations in detect.go.
func registryInstallRoots() []string { return nil }

// loadAppCredential is only implemented on Windows, where the desktop app keeps
// its login state in a DPAPI-wrapped Chromium OSCrypt envelope. On other
// platforms set LINGMA_QODERCLI_JOB_TOKEN to the JSON job credential instead.
func loadAppCredential(dir string) (appCredential, error) {
	return appCredential{}, fmt.Errorf("reading the Qoder CN app login state is only supported on Windows (profile %q)", dir)
}
