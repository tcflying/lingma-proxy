//go:build windows

package qodercli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestKillProcessTreeKillsAnUnassignedGrandchild proves the pid walk reaches what
// the job object cannot. The fixture is started directly, with no guard and no
// assign call, so nothing in this tree was ever in a job: the grandchild is only
// reachable through its parent link.
func TestKillProcessTreeKillsAnUnassignedGrandchild(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		fakeCLIModeEnv+"="+fakeSpawnsTreeThenHang,
		fakeCLIPidFileEnv+"="+pidFile,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the fake tree: %v", err)
	}
	t.Cleanup(func() {
		killProcessTree(uint32(cmd.Process.Pid))
		_ = cmd.Wait()
	})

	pids := waitForPidFile(t, pidFile, 20*time.Second)
	killProcessTree(uint32(cmd.Process.Pid))
	assertProcessTreeExited(t, pids, 8*time.Second)
}

// TestListModelsReturnsWhenTheListingGoesIdle is the measured --list-models
// shape: the CLI prints its list within seconds, logs a normal exit, and then
// hangs in shutdown, leaving the request parked in cmd.Wait forever.
func TestListModelsReturnsWhenTheListingGoesIdle(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	t.Setenv(fakeCLIArgvEnv, "")
	t.Setenv(fakeCLIPidFileEnv, pidFile)
	c := fakeCLIClient(t, fakeListModelsThenHang, 0)

	// The caller's budget is far above the assertion below, so only the idle grace
	// can explain a return inside it.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	models, err := c.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if took := time.Since(start); took >= 6*time.Second {
		t.Fatalf("ListModels took %s against a CLI that stopped printing", took.Round(time.Millisecond))
	}
	var keys []string
	for _, model := range models {
		keys = append(keys, model.Key)
	}
	if strings.Join(keys, ",") != "Auto,Qwen3.8-Flash" {
		t.Fatalf("models = %v, want exactly the printed list", keys)
	}
	// The pid file is written before the first line, so it is readable after the
	// turn; the process behind the listing must have died with it.
	assertProcessTreeExited(t, []int{readPIDFile(t, pidFile)}, 8*time.Second)
}

// TestTreeGuardReleasedBeforeAssignKillsTheTree pins the cancel window between
// Start and assign: cmd.Cancel can release the guard first, which kills nothing
// because root is still 0, and closes the job. assign must see that and take the
// tree down by pid instead of assigning through the dead handle.
func TestTreeGuardReleasedBeforeAssignKillsTheTree(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pids")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		fakeCLIModeEnv+"="+fakeSpawnsTreeThenHang,
		fakeCLIPidFileEnv+"="+pidFile,
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the fake tree: %v", err)
	}
	t.Cleanup(func() {
		killProcessTree(uint32(cmd.Process.Pid))
		_ = cmd.Wait()
	})
	guard := newTreeGuard()
	if guard == nil {
		t.Fatal("newTreeGuard returned nil: job object unavailable")
	}

	// The tree exists before the release, so a release that loses the race for
	// the pid has a live tree to leak. Hold a handle to each member now: the
	// full suite recycles pids fast enough that reopening a pid after the kill
	// can land on an unrelated live process.
	pids := waitForPidFile(t, pidFile, 20*time.Second)
	handles := make([]windows.Handle, 0, len(pids))
	for _, pid := range pids {
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			t.Fatalf("open pid %d before the kill: %v", pid, err)
		}
		handles = append(handles, h)
	}
	defer func() {
		for _, h := range handles {
			windows.CloseHandle(h)
		}
	}()

	guard.release()
	guard.assign(cmd.Process)

	for i, h := range handles {
		if event, err := windows.WaitForSingleObject(h, 8000); err != nil || event != windows.WAIT_OBJECT_0 {
			t.Fatalf("pid %d from the released-before-assign tree did not terminate: event=%d err=%v", pids[i], event, err)
		}
	}
}

func readPIDFile(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fake CLI pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("fake CLI pid file %q holds %q", path, raw)
	}
	return pid
}
