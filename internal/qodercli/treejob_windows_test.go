//go:build windows

package qodercli

import (
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// pidAlive reports whether a process is still running. It asks the kernel for a
// signalled handle rather than trusting the exit code: per the TerminateProcess
// documentation a process that has been asked to die can still report
// STILL_ACTIVE for a while, and the observation this file exists to make is
// "the tree is gone", which only a signalled handle proves. The same reasoning,
// and the same API, as assertProcessTreeExited in cancel_tree_windows_test.go.
func pidAlive(pid uint32) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	// Zero timeout: the question is the state right now, not whether it changes.
	event, err := windows.WaitForSingleObject(h, 0)
	if err != nil {
		return false
	}
	return event != uint32(windows.WAIT_OBJECT_0)
}

// childPIDs lists live processes whose parent is pid, so an escaped grandchild is
// measured instead of assumed.
func childPIDs(pid uint32) []uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil
	}
	defer windows.CloseHandle(snap)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	var out []uint32
	for err := windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		if entry.ParentProcessID == pid && pidAlive(entry.ProcessID) {
			out = append(out, entry.ProcessID)
		}
	}
	return out
}

// The whole point of the guard is the level below the direct child: exec kills
// that one on its own, so a test that only checked the child would stay green
// with the job object deleted.
func TestTreeGuardReleaseKillsTheWholeTree(t *testing.T) {
	guard := newTreeGuard()
	if guard == nil {
		t.Fatal("newTreeGuard returned nil: job object unavailable")
	}
	cmd := exec.Command("cmd.exe", "/c", "start /b ping -n 20 127.0.0.1 & ping -n 20 127.0.0.1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	guard.assign(cmd.Process)
	parent := uint32(cmd.Process.Pid)

	var kids []uint32
	appear := time.Now().Add(10 * time.Second)
	for time.Now().Before(appear) {
		if kids = childPIDs(parent); len(kids) > 0 {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if len(kids) == 0 {
		guard.release()
		_ = cmd.Wait()
		t.Fatal("fixture produced no grandchild, so it cannot prove the tree dies")
	}

	guard.release()
	gone := time.Now().Add(10 * time.Second)
	for time.Now().Before(gone) {
		if !pidAlive(parent) && !anyAlive(kids) {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	_ = cmd.Wait()
	if pidAlive(parent) {
		t.Fatalf("direct child %d survived release", parent)
	}
	if anyAlive(kids) {
		t.Fatalf("grandchildren %v survived release", kids)
	}
}

func anyAlive(pids []uint32) bool {
	for _, pid := range pids {
		if pidAlive(pid) {
			return true
		}
	}
	return false
}

// TestTreeGuardCreationFailureIsReported is the only signal a guard that does not
// exist leaves behind. A job object that cannot be created means every turn from
// here on kills the direct child only -- the exact wedge this guard exists to
// prevent -- and assign swallows its errors on purpose, so nothing else says so.
// The Windows code is in the line because a quota exhaustion and a permission
// denial are different problems on different boxes.
func TestTreeGuardCreationFailureIsReported(t *testing.T) {
	prev := createTreeJob
	createTreeJob = func() (windows.Handle, error) { return 0, windows.ERROR_NOT_ENOUGH_QUOTA }
	t.Cleanup(func() { createTreeJob = prev })

	logged := &syncedLogBuffer{}
	log.SetOutput(logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	guard := newTreeGuard()
	if guard != nil {
		t.Fatal("a guard without a job handle must not be handed back")
	}
	msg := logged.String()
	if !strings.Contains(msg, "tree guard") || !strings.Contains(msg, "create") {
		t.Fatalf("the failure was not reported: %q", msg)
	}
	if !strings.Contains(msg, strconv.FormatUint(uint64(windows.ERROR_NOT_ENOUGH_QUOTA), 10)) {
		t.Fatalf("the log carries no Windows error code, so the cause is guesswork: %q", msg)
	}
	if !strings.Contains(msg, "direct child") {
		t.Fatalf("the log does not say what the degradation costs: %q", msg)
	}
}
