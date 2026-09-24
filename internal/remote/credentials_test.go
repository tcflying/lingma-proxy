package remote

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSaveCredentialFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials.json")
	cred := Credential{
		CosyKey:         "cosy",
		EncryptUserInfo: "encrypted",
		UserID:          "user-123456",
		MachineID:       "machine-1234567890",
		Source:          "test",
		TokenExpireTime: 1777520000000,
	}

	if err := SaveCredentialFile(cred, path); err != nil {
		t.Fatalf("SaveCredentialFile() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat credentials file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0600 {
		if runtime.GOOS == "windows" {
			// NTFS has no POSIX permission bits: os.WriteFile's 0600 comes back
			// as 0666, so only the round trip is meaningful here.
			t.Logf("windows reports mode %o for a 0600 write", got)
		} else {
			t.Fatalf("credentials file mode = %o, want 0600", got)
		}
	}

	loaded, err := LoadCredential(path)
	if err != nil {
		t.Fatalf("LoadCredential() error = %v", err)
	}
	if loaded.CosyKey != cred.CosyKey || loaded.EncryptUserInfo != cred.EncryptUserInfo ||
		loaded.UserID != cred.UserID || loaded.MachineID != cred.MachineID ||
		loaded.TokenExpireTime != cred.TokenExpireTime {
		t.Fatalf("loaded credential mismatch: %#v", loaded)
	}
}

func writeMachineIDLog(t *testing.T, dir, machineID string) {
	t.Helper()
	logs := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logs, 0755); err != nil {
		t.Fatal(err)
	}
	body := "2026-09-24 INFO shared client started\n2026-09-24 INFO machine id: " + machineID + "\n"
	if err := os.WriteFile(filepath.Join(logs, "lingma.log"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func machineIDMemoLen() int {
	machineIDLogFallbackMu.Lock()
	defer machineIDLogFallbackMu.Unlock()
	return len(machineIDLogFallbackCache)
}

func expireMachineIDMemo() {
	machineIDLogFallbackMu.Lock()
	defer machineIDLogFallbackMu.Unlock()
	for key, entry := range machineIDLogFallbackCache {
		entry.resolved = time.Now().Add(-2 * machineIDLogFallbackTTL)
		machineIDLogFallbackCache[key] = entry
	}
}

// R3: a machine that has cache/user but neither cache/id nor cli/.auth/id reaches
// the log walk on every Chat and ListModels, because both load the credential
// first. That walk opens thousands of rotated IDE log files, so the answer is reused.
func TestMachineIDLogFallbackIsMemoizedOnTheRequestPath(t *testing.T) {
	sandboxCandidateEnv(t)
	resetMachineIDMemo()
	t.Cleanup(resetMachineIDMemo)

	dir := t.TempDir()
	writeMachineIDLog(t, dir, "machine-from-log-one-aaaa")
	if got, err := loadMachineID(dir); err != nil || got != "machine-from-log-one-aaaa" {
		t.Fatalf("first loadMachineID() = (%q, %v)", got, err)
	}
	if machineIDMemoLen() == 0 {
		t.Fatal("log fallback left no memo, so every request re-walks the log tree")
	}

	// A re-created IDE profile writes a different id into the same log.
	writeMachineIDLog(t, dir, "machine-from-log-two-bbbb")
	if got, err := loadMachineID(dir); err != nil || got != "machine-from-log-one-aaaa" {
		t.Fatalf("second loadMachineID() = (%q, %v), want the memoized id", got, err)
	}

	// The memo expires: the id is not pinned for the life of the process.
	expireMachineIDMemo()
	if got, err := loadMachineID(dir); err != nil || got != "machine-from-log-two-bbbb" {
		t.Fatalf("after the TTL loadMachineID() = (%q, %v), want the new id", got, err)
	}
}

// R3 counterpart: the explicit inspect/policy entry points keep the unbounded scan,
// so a diagnostic never reads a cached answer.
func TestMachineIDLogScanStaysUncachedForInspect(t *testing.T) {
	sandboxCandidateEnv(t)
	resetMachineIDMemo()
	t.Cleanup(resetMachineIDMemo)

	dir := t.TempDir()
	writeMachineIDLog(t, dir, "machine-from-log-one-aaaa")
	if got, err := loadMachineIDFromLogs(dir); err != nil || got != "machine-from-log-one-aaaa" {
		t.Fatalf("loadMachineIDFromLogs() = (%q, %v)", got, err)
	}
	writeMachineIDLog(t, dir, "machine-from-log-two-bbbb")
	if got, err := loadMachineIDFromLogs(dir); err != nil || got != "machine-from-log-two-bbbb" {
		t.Fatalf("uncached scan = (%q, %v), want the fresh id", got, err)
	}
	if n := machineIDMemoLen(); n != 0 {
		t.Fatalf("uncached scan populated the request-path memo (%d entries)", n)
	}
}

// The cheap direct files stay outside the memo, so a cache/id that appears later is
// picked up without waiting for the TTL.
func TestMachineIDPrefersDirectFileOverMemo(t *testing.T) {
	sandboxCandidateEnv(t)
	resetMachineIDMemo()
	t.Cleanup(resetMachineIDMemo)

	dir := t.TempDir()
	writeMachineIDLog(t, dir, "machine-from-log-one-aaaa")
	if got, err := loadMachineID(dir); err != nil || got != "machine-from-log-one-aaaa" {
		t.Fatalf("loadMachineID() = (%q, %v)", got, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "cache"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache", "id"), []byte("cache-id-file-value-1234"), 0600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadMachineID(dir); err != nil || got != "cache-id-file-value-1234" {
		t.Fatalf("loadMachineID() = (%q, %v), want the cache/id file to win immediately", got, err)
	}
}
