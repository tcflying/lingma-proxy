package main

import (
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestHeadlessLogLeavesTheConsoleAlone pins the thing that kept 192.168.50.239
// unreachable: the startup logs must not go through the console the Task
// Scheduler owns, because a console that stops servicing writes parks the
// process before it reaches net.Listen.
func TestHeadlessLogLeavesTheConsoleAlone(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("headlessLog is Windows-only by design")
	}
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	previous := log.Writer()
	t.Cleanup(func() { log.SetOutput(previous) })
	// The production path keeps this handle for the lifetime of the process, which
	// is the point; only the test has to let go of it before TempDir cleans up.
	t.Cleanup(func() {
		if f, ok := log.Writer().(*os.File); ok {
			f.Close()
		}
	})

	headlessLog()
	log.Printf("marker-line-for-the-console")

	data, err := os.ReadFile(filepath.Join(dir, "lingma-proxy", "headless.log"))
	if err != nil {
		t.Fatalf("headlessLog did not create a log file: %v", err)
	}
	if !strings.Contains(string(data), "marker-line-for-the-console") {
		t.Fatalf("marker missing from %q", data)
	}
	if previous == log.Writer() {
		t.Fatal("log output is still the inherited writer")
	}
}

// TestStartupRedirectsBeforeAnythingElse guards the other half of the invariant:
// main must call headlessLog before it can log anything at all. It is a shape
// check on purpose -- "the first statement of main" is exactly what the blocked
// console write breaks, and no unit test can observe a real console selection.
func TestStartupRedirectsBeforeAnythingElse(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(src), "func main() {\n\theadlessLog()\n") {
		t.Fatal("main() no longer redirects the log as its first act")
	}
}
