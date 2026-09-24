package remote

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// sandboxCandidateEnv points every directory these helpers scan at a temp home and
// drops the package-global candidate memo, so a test cannot inherit a cached scan
// from whichever test ran before it.
func sandboxCandidateEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("ProgramData", filepath.Join(home, "ProgramData"))
	t.Setenv("LINGMA_REMOTE_BASE_URL", "")
	t.Setenv("LINGMA_CACHE_DIR", "")
	t.Cleanup(resetBaseURLCandidateMemo)
	resetBaseURLCandidateMemo()
	return home
}

func resetBaseURLCandidateMemo() {
	baseURLHintsMu.Lock()
	baseURLHintsValue = nil
	baseURLHintsAt = time.Time{}
	baseURLHintsMu.Unlock()
}

func resetMachineIDMemo() {
	machineIDLogFallbackMu.Lock()
	machineIDLogFallbackCache = map[string]machineIDLogFallbackEntry{}
	machineIDLogFallbackMu.Unlock()
}

// chatTestClient is a Client that reads its credential from a file, so Chat never
// touches the real login cache or the candidate scan.
func chatTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	path := filepath.Join(t.TempDir(), "credentials.json")
	cred := Credential{
		CosyKey:         "cosy-key-value",
		EncryptUserInfo: "encrypted-user-info",
		UserID:          "user-123456",
		MachineID:       "machine-1234567890ab",
		Source:          "test",
	}
	if err := SaveCredentialFile(cred, path); err != nil {
		t.Fatal(err)
	}
	return New(Config{BaseURL: baseURL, AuthFile: path})
}

// sseFrame wraps an inner OpenAI-style chunk the way the gateway does: the frame is
// an outer envelope whose body is the JSON-encoded chunk.
func sseFrame(t *testing.T, inner string) string {
	t.Helper()
	outer, err := json.Marshal(outerSSE{Body: inner, StatusCode: 200})
	if err != nil {
		t.Fatal(err)
	}
	return "data: " + string(outer) + "\n\n"
}

func contentChunk(content string) string {
	return `{"choices":[{"delta":{"content":"` + content + `"},"finish_reason":null}]}`
}

func doneFrame() string { return "data: [DONE]\n\n" }
