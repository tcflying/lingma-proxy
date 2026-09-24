//go:build windows

package qodercli

import (
	"os/exec"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// stillActive is Win32 STILL_ACTIVE, what GetExitCodeProcess reports for a
// process that has not exited; x/sys/windows does not export it.
const stillActive = 259

func pidAlive(pid uint32) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
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
