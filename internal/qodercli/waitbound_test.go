package qodercli

import (
	"errors"
	"testing"
	"time"
)

// TestWaitWithTimeoutAbandonsAWaitThatNeverReturns is the shape that parked a
// finished --list-models turn forever: cmd.Wait blocked on a child that was still
// with us, and nothing bounded the call.
func TestWaitWithTimeoutAbandonsAWaitThatNeverReturns(t *testing.T) {
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })

	start := time.Now()
	err := waitWithTimeout(50*time.Millisecond, func() error {
		<-block
		return nil
	})
	if !errors.Is(err, errWaitAbandoned) {
		t.Fatalf("a wait that never returns must end as %v, got %v", errWaitAbandoned, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the abandon took %s, want it bounded by the 50ms timeout", elapsed.Round(time.Millisecond))
	}
}

// TestWaitWithTimeoutReturnsTheWaitsOwnError keeps the bound transparent: a wait
// that finishes on time must hand its own error through unchanged, exit status
// and all.
func TestWaitWithTimeoutReturnsTheWaitsOwnError(t *testing.T) {
	want := errors.New("exit status 7")
	if got := waitWithTimeout(time.Second, func() error { return want }); got != want {
		t.Fatalf("got %v, want the wait's own error unchanged", got)
	}
}
