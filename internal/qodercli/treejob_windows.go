//go:build windows

package qodercli

import (
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

func newTreeGuard() *treeGuard {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
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
		return nil
	}
	return &treeGuard{job: job}
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
