//go:build !windows

package qodercli

import "os"

// treeGuard is a no-op off Windows, so the CLI tree is only bounded by the
// direct-child kill there.
// ponytail: upgrade with SysProcAttr{Setpgid: true} plus kill(-pgid, SIGKILL)
// in release() if a unix grandchild is ever observed holding the stdout pipe.
type treeGuard struct{}

func newTreeGuard() *treeGuard          { return nil }
func (g *treeGuard) assign(*os.Process) {}
func (g *treeGuard) release()           {}
