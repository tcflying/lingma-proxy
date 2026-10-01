package qodercli

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// waitForPidFile blocks until the fake CLI host has recorded its own pid and the
// grandchild's, which is the earliest moment the whole tree exists. Used by the
// Windows-only tree test; kept here so the helper itself stays cross-platform.
func waitForPidFile(t *testing.T, path string, budget time.Duration) []int {
	t.Helper()
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil {
			var pids []int
			for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
				pid, err := strconv.Atoi(strings.TrimSpace(line))
				if err != nil {
					t.Fatalf("pid file %q holds a non-numeric line %q", path, line)
				}
				pids = append(pids, pid)
			}
			if len(pids) == 2 {
				return pids
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("fake CLI host never recorded its tree at %s within %s", path, budget)
	return nil
}

// TestClientCancelReportsCancellationNotTimeout pins the error wording for a
// turn whose caller hung up while a timeout was also configured: the run
// context dies with context.Canceled, not DeadlineExceeded, and reporting it as
// "timed out after 180s" sends an operator chasing a slow CLI when the actual
// event was the client disconnecting.
func TestClientCancelReportsCancellationNotTimeout(t *testing.T) {
	// A real timeout is configured, so the misleading branch is armed; the fake
	// child hangs long past the test, so only the cancel can end it.
	c := fakeCLIClient(t, fakeShortWriteThenHang, 10*time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type outcome struct {
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		_, err := c.run(ctx)
		done <- outcome{err: err}
	}()
	time.Sleep(300 * time.Millisecond)
	cancel()

	select {
	case out := <-done:
		if out.err == nil {
			t.Fatal("run succeeded against a cancelled context")
		}
		if !strings.Contains(out.err.Error(), "cancel") {
			t.Errorf("a caller cancel must be reported as a cancellation, got: %v", out.err)
		}
		if strings.Contains(out.err.Error(), "timed out") {
			t.Errorf("a caller cancel is not a timeout; the deadline never fired, got: %v", out.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return within 10s of the cancel")
	}
}
