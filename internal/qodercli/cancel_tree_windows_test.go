//go:build windows

package qodercli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"lingma-ipc-proxy/internal/remote"
)

// TestCancelledChatKillsTheWholeTree is Windows-only: the tree guard behind it
// is a kill-on-close job object, and off Windows the guard is a no-op (see
// treejob_other.go), so a grandchild holding the stdout write end would
// legitimately survive the direct-child kill there.
func TestCancelledChatKillsTheWholeTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	t.Setenv(fakeCLIPidFileEnv, pidFile)
	// timeout 0 keeps the run context equal to the caller's context, so the only
	// thing that can end this child is the cancel under test.
	c := fakeCLIClient(t, fakeSpawnsTreeThenHang, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		_, err := c.Chat(ctx, remote.ChatRequest{Prompt: "hang as a tree", Model: "Qwen3.8-Flash"}, nil)
		done <- outcome{err: err}
	}()

	pids := waitForPidFile(t, pidFile, 20*time.Second)
	cancel()

	const returnBudget = 8 * time.Second
	select {
	case out := <-done:
		if out.err == nil {
			t.Fatal("Chat succeeded against a cancelled turn")
		}
		if !strings.Contains(out.err.Error(), "cancel") {
			t.Errorf("error does not name the cancellation: %v", out.err)
		}
	case <-time.After(returnBudget):
		t.Fatalf("Chat did not return within %s of the cancel; the descendant holding stdout is still alive", returnBudget)
	}

	assertProcessTreeExited(t, pids, 8*time.Second)
}

// assertProcessTreeExited waits until the OS reports each member of the
// cancelled tree terminated. TerminateProcess is asynchronous and its exit code
// can read as final long before the process is gone (kernel observation
// 2026-09-29: code 0 at 0.2s while the handle stayed un-signalled to 30s), so
// per the TerminateProcess documentation only a signalled handle -- or a pid
// that no longer exists -- proves termination. Anything else fails the test.
func assertProcessTreeExited(t *testing.T, pids []int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for _, pid := range pids {
		handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				continue // the pid itself no longer exists
			}
			t.Fatalf("pid %d of the cancelled tree could not be opened (%v); its termination is unproven", pid, err)
		}
		terminated := false
		for !terminated {
			event, err := windows.WaitForSingleObject(handle, 100)
			switch {
			case err != nil:
				windows.CloseHandle(handle)
				t.Fatalf("wait on pid %d failed: %v", pid, err)
			case event == windows.WAIT_OBJECT_0:
				terminated = true
			case event == uint32(windows.WAIT_TIMEOUT):
				// Not terminated yet; keep waiting against the budget.
			default:
				windows.CloseHandle(handle)
				t.Fatalf("wait on pid %d returned unexpected event %d", pid, event)
			}
			if !terminated && time.Now().After(deadline) {
				windows.CloseHandle(handle)
				t.Fatalf("pid %d from the cancelled tree did not terminate within %s", pid, budget)
			}
		}
		windows.CloseHandle(handle)
	}
}
