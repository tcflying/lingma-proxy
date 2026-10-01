package deploy

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"lingma-ipc-proxy/internal/remote"
)

func TestWriteServerBundle(t *testing.T) {
	dir := t.TempDir()
	sourceCredPath := filepath.Join(dir, "source-credentials.json")
	if err := remote.SaveCredentialFile(remote.Credential{
		CosyKey:         "cosy",
		EncryptUserInfo: "encrypted",
		UserID:          "user-123456",
		MachineID:       "machine-1234567890",
		Source:          "test",
		TokenExpireTime: 1777520000000,
	}, sourceCredPath); err != nil {
		t.Fatalf("SaveCredentialFile() error = %v", err)
	}

	outputPath := filepath.Join(dir, "bundle.zip")
	result, err := WriteServerBundle(ServerBundleOptions{
		AuthFile:   sourceCredPath,
		OutputPath: outputPath,
		Port:       18095,
		Model:      "kmodel",
	})
	if err != nil {
		t.Fatalf("WriteServerBundle() error = %v", err)
	}
	if result.Path != outputPath {
		t.Fatalf("bundle path = %q, want %q", result.Path, outputPath)
	}
	if result.CredentialSrc != "test" {
		t.Fatalf("credential source = %q, want test", result.CredentialSrc)
	}

	reader, err := zip.OpenReader(outputPath)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer reader.Close()
	names := map[string]bool{}
	for _, file := range reader.File {
		names[file.Name] = true
	}
	for _, name := range []string{"credentials.json", "lingma-proxy.json", "docker-compose.yml", "README.txt"} {
		if !names[name] {
			t.Fatalf("bundle missing %s", name)
		}
	}
}

func TestServerBundleKeepsCredentialsPrivate(t *testing.T) {
	dir := t.TempDir()
	sourceCredPath := filepath.Join(dir, "source-credentials.json")
	if err := remote.SaveCredentialFile(remote.Credential{
		CosyKey:         "cosy",
		EncryptUserInfo: "encrypted",
		UserID:          "user-123456",
		MachineID:       "machine-1234567890",
		Source:          "test",
	}, sourceCredPath); err != nil {
		t.Fatalf("SaveCredentialFile() error = %v", err)
	}
	outputPath := filepath.Join(dir, "bundle.zip")

	// On the target server the archive is the credential: anything group or
	// other readable leaks it without unpacking.
	//
	// The mode is asserted from the *request*, recorded through the seam and then
	// delegated to the real creator, so the production path is what gets checked.
	// Reading it off the finished file instead would be a check that cannot work on
	// Windows -- the OS emulates the POSIX bits there -- and an earlier version of
	// it responded to that by logging instead of failing, which is how a
	// world-readable bundle shipped while the ledger still called the item fixed.
	var requested os.FileMode
	var requests int
	realCreate := createBundleFile
	createBundleFile = func(path string, perm os.FileMode) (*os.File, error) {
		requests++
		requested = perm
		return realCreate(path, perm)
	}
	t.Cleanup(func() { createBundleFile = realCreate })

	if _, err := WriteServerBundle(ServerBundleOptions{
		AuthFile:   sourceCredPath,
		OutputPath: outputPath,
		Port:       18095,
	}); err != nil {
		t.Fatalf("WriteServerBundle() error = %v", err)
	}
	if requests != 1 {
		t.Fatalf("bundle opened through the seam %d times, want exactly 1", requests)
	}
	if requested != 0o600 {
		t.Errorf("bundle requested mode = %o, want 0600 (group and other must not read it)", requested)
	}
	if bundleFileMode != 0o600 {
		t.Errorf("bundleFileMode = %o, want 0600", bundleFileMode)
	}

	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("stat bundle: %v", err)
	}
	// Belt and braces where the OS can actually answer it: a real POSIX run also
	// proves the mode reached the filesystem, not just the call site.
	if perm := info.Mode().Perm(); goruntime.GOOS != "windows" && perm&0o077 != 0 {
		t.Errorf("bundle file mode = %o, want group/other bits cleared", perm)
	}

	reader, err := zip.OpenReader(outputPath)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	defer reader.Close()
	var compose string
	for _, file := range reader.File {
		switch file.Name {
		case "credentials.json":
			if perm := file.Mode().Perm(); perm&0o077 != 0 {
				t.Errorf("credentials.json entry mode = %o, want no group/other bits", perm)
			}
		case "docker-compose.yml":
			compose = readZipFile(t, file)
		}
	}
	// The proxy has no auth, so a bare "<port>:8095" mapping hands the recorded
	// conversations to every host that can route the server.
	if !strings.Contains(compose, `"127.0.0.1:18095:8095"`) {
		t.Errorf("compose must publish on the server loopback only, got:\n%s", compose)
	}
}

func readZipFile(t *testing.T, file *zip.File) string {
	t.Helper()
	rc, err := file.Open()
	if err != nil {
		t.Fatalf("open %s: %v", file.Name, err)
	}
	defer rc.Close()
	body, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", file.Name, err)
	}
	return string(body)
}
