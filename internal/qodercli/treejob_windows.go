//go:build windows

package qodercli

import (
	"log"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// treeGuard holds a kill-on-close job object. The CLI child is assigned to it,
// and every process the child forks inherits membership, so release() takes the
// whole tree with it. That is what bounds the stdout pipe read in run: the
// grandchild that holds the write end open dies when the deadline fires.
type treeGuard struct {
	once sync.Once
	job  windows.Handle
}

// createTreeJob is the seam the tests use to fail job creation on a machine whose
// job objects work perfectly well.
var createTreeJob = func() (windows.Handle, error) { return windows.CreateJobObject(nil, nil) }

func newTreeGuard() *treeGuard {
	job, err := createTreeJob()
	if err != nil {
		logTreeGuardFailure("create", err)
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
		logTreeGuardFailure("configure", err)
		return nil
	}
	return &treeGuard{job: job}
}

// logTreeGuardFailure is the only signal a guard that does not exist leaves
// behind. assign swallows its own errors on purpose -- failing to guard a tree
// must not fail the request -- but a guard that was never created means every
// turn from here on falls back to killing the direct child only, which is the
// wedge this job object exists to prevent, and it comes back with nothing in the
// log unless it is said here. The Windows code is in the line because a quota
// exhaustion and a permission denial are different problems on different boxes.
func logTreeGuardFailure(op string, err error) {
	const fallback = "every turn now falls back to killing the direct child only"
	if code, ok := err.(windows.Errno); ok {
		log.Printf("qodercli: CLI tree guard %s failed with windows error %d (%v); %s", op, uint32(code), err, fallback)
		return
	}
	log.Printf("qodercli: CLI tree guard %s failed (%v); %s", op, err, fallback)
}

// assign is best-effort: failing to guard the tree must not fail the request,
// it only falls back to killing the direct child.
func (g *treeGuard) assign(proc *os.Process) {
	if g == nil || proc == nil {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(proc.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.AssignProcessToJobObject(g.job, h)
}

func (g *treeGuard) release() {
	if g == nil {
		return
	}
	g.once.Do(func() { _ = windows.CloseHandle(g.job) })
}
