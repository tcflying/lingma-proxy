package remote

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
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
