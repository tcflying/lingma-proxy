//go:build windows

package qodercli

import (
	"errors"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// treeGuard holds a kill-on-close job object. The CLI child is assigned to it,
// and every process the child forks inherits membership, so release() takes the
// whole tree with it. That is what bounds the stdout pipe read in run: the
// grandchild that holds the write end open dies when the deadline fires.
//
// The job is not enough on its own: a worker forked before assign() ran was
// never a member, so release() also walks the tree by pid.
type treeGuard struct {
	once sync.Once
	job  windows.Handle
	// root is the direct child's pid, recorded before anything else in assign so
	// release can reach the tree even when the job handle was never created.
	root atomic.Uint32
	// closed is set inside release's once before the job handle is closed: a
	// cancel can win the race against assign, and assign must then take the tree
	// down by pid instead of assigning through a dead handle.
	closed atomic.Bool
}

// createTreeJob is the seam the tests use to fail job creation on a machine whose
// job objects work perfectly well.
var createTreeJob = func() (windows.Handle, error) { return windows.CreateJobObject(nil, nil) }

func newTreeGuard() *treeGuard {
	job, err := createTreeJob()
	if err != nil {
		logTreeGuardFailure("create", 0, err)
		return nil
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		logTreeGuardFailure("configure", 0, err)
		return nil
	}
	return &treeGuard{job: job}
}

// logTreeGuardFailure is the only signal a guard failure leaves behind. assign
// swallows its own errors on purpose -- failing to guard a tree must not fail
// the request -- but a turn that runs without the job falls back to killing the
// direct child only, which is the wedge this job object exists to prevent, and
// it comes back with nothing in the log unless it is said here. The Windows code
// is in the line because a quota exhaustion and a permission denial are
// different problems on different boxes. pid is 0 for guard-level operations.
func logTreeGuardFailure(op string, pid uint32, err error) {
	const fallback = "every turn now falls back to killing the direct child only"
	if code, ok := err.(windows.Errno); ok {
		if pid != 0 {
			log.Printf("qodercli: CLI tree guard %s for pid %d failed with windows error %d (%v); %s", op, pid, uint32(code), err, fallback)
			return
		}
		log.Printf("qodercli: CLI tree guard %s failed with windows error %d (%v); %s", op, uint32(code), err, fallback)
		return
	}
	if pid != 0 {
		log.Printf("qodercli: CLI tree guard %s for pid %d failed (%v); %s", op, pid, err, fallback)
		return
	}
	log.Printf("qodercli: CLI tree guard %s failed (%v); %s", op, err, fallback)
}

// assign is best-effort: failing to guard the tree must not fail the request,
// it only falls back to killing the direct child. The guard may already be
// released when this runs -- a cancel between Start and here takes that race --
// so the closed flag decides: a released guard's job handle must not be touched,
// and the root walk is the only thing left that can take the tree down.
func (g *treeGuard) assign(proc *os.Process) {
	if g == nil || proc == nil {
		return
	}
	g.root.Store(uint32(proc.Pid))
	if g.closed.Load() {
		killProcessTree(g.root.Load())
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(proc.Pid))
	if err != nil {
		// A child that exited before this ran has no tree left to guard; anything
		// else would leave descendants outside the job and must be visible.
		if !errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			logTreeGuardFailure("open", uint32(proc.Pid), err)
		}
		return
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(g.job, h); err != nil {
		logTreeGuardFailure("assign", uint32(proc.Pid), err)
	}
}

func (g *treeGuard) release() {
	if g == nil {
		return
	}
	g.once.Do(func() {
		g.closed.Store(true)
		_ = windows.CloseHandle(g.job)
		// Closing the job kills what was assigned to it. The pid walk catches what
		// was not: a worker forked before assign ran, or every descendant when the
		// job handle could not be created at all.
		if root := g.root.Load(); root != 0 {
			killProcessTree(root)
		}
	})
}

// Two rounds catch a child forked while the tree is being torn down. The pause
// is short because release() sits on request paths (cancel, teardown, defer).
const (
	treeKillRounds     = 2
	treeKillRoundDelay = 150 * time.Millisecond
)

// killProcessTree terminates root and every live descendant found through the
// process list's parent links. It is the fallback for a kill-on-close job that
// does not hold the whole tree; exec only reaps the direct child, so an unassigned
// grandchild is exactly what keeps a stdout pipe open after the CLI is done.
//
// shortcut: pids can be reused between the snapshot and the kill, so this may
// terminate an unrelated process only if the pid was recycled in that window;
// upgrade to a creation-time check if that is ever observed.
func killProcessTree(root uint32) {
	killed := false
	for round := 0; round < treeKillRounds; round++ {
		if round > 0 {
			if !killed {
				// Nothing was alive in the previous round, so nothing could have
				// forked a new child for this one to find.
				return
			}
			time.Sleep(treeKillRoundDelay)
		}
		killed = false
		for _, pid := range processTree(root) {
			if terminateProcess(pid) {
				killed = true
			}
		}
	}
}

// processTree returns root followed by every descendant reachable through
// ParentProcessID in one snapshot. A failed snapshot still returns root: killing
// the direct child is always worth attempting.
func processTree(root uint32) []uint32 {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		logTreeKillFailure("snapshot", root, err)
		return []uint32{root}
	}
	defer windows.CloseHandle(snap)

	type parentLink struct{ pid, parent uint32 }
	var all []parentLink
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err := windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		all = append(all, parentLink{pid: entry.ProcessID, parent: entry.ParentProcessID})
	}

	tree := []uint32{root}
	seen := map[uint32]bool{root: true}
	for i := 0; i < len(tree); i++ {
		for _, link := range all {
			if link.parent == tree[i] && !seen[link.pid] {
				seen[link.pid] = true
				tree = append(tree, link.pid)
			}
		}
	}
	return tree
}

// terminateProcess reports whether a process was still there to kill. A pid that
// no longer exists is not a failure; every other error is logged with its pid and
// Windows code, because a silent failure to kill is what kept a finished CLI alive
// for 150 seconds and more.
func terminateProcess(pid uint32) bool {
	h, err := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		if !errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			logTreeKillFailure("open", pid, err)
		}
		return false
	}
	defer windows.CloseHandle(h)
	// A child that has exited but has not been reaped yet still resolves by pid,
	// and TerminateProcess then fails with access denied: that is the normal end
	// of a turn, not a kill that failed.
	const stillActive = 259 // STILL_ACTIVE from GetExitCodeProcess
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		logTreeKillFailure("query", pid, err)
	} else if code != stillActive {
		return false
	}
	if err := windows.TerminateProcess(h, 1); err != nil && !errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		logTreeKillFailure("terminate", pid, err)
	}
	return true
}

func logTreeKillFailure(op string, pid uint32, err error) {
	if code, ok := err.(windows.Errno); ok {
		log.Printf("qodercli: CLI tree kill %s for pid %d failed with windows error %d (%v)", op, pid, uint32(code), err)
		return
	}
	log.Printf("qodercli: CLI tree kill %s for pid %d failed (%v)", op, pid, err)
}
