package lingmaipc

import (
	"errors"
	"testing"
	"time"
)

// stallTransport is a framedTransport that never yields frames, so readLoop stays
// parked in ReadFrame and the test drives broadcast directly.
type stallTransport struct {
	release chan struct{}
}

func (t *stallTransport) ReadFrame() ([]byte, error) {
	<-t.release
	return nil, errors.New("transport released")
}
func (t *stallTransport) WriteFrame([]byte) error { return nil }
func (t *stallTransport) Close() error            { close(t.release); return nil }
func (t *stallTransport) Address() string         { return "stall:" }

func newStalledClient() *Client {
	return &Client{
		transport: &stallTransport{release: make(chan struct{})},
		pending:   make(map[int]chan responseEnvelope),
		subs:      make(map[int]chan Notification),
		closed:    make(chan struct{}),
	}
}

// TestBroadcastNeverBlocksOnASubscriber is the L1 regression: readLoop must keep
// draining the transport even when a subscriber is wedged, and Close must not
// queue behind the lock that wedged it. Both are asserted with a deadline so a
// re-injected blocking send fails the test instead of hanging the suite.
func TestBroadcastNeverBlocksOnASubscriber(t *testing.T) {
	c := newStalledClient()

	// One slot, nobody reading: the second frame onward has to be refused.
	stalled := make(chan Notification, 1)
	c.subsMu.Lock()
	c.subs[1] = stalled
	c.subsMu.Unlock()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			c.broadcast(Notification{Method: "session/update"})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("broadcast blocked on a full subscriber: readLoop would stop the whole transport")
	}

	if got := c.DroppedNotifications(); got != 49 {
		t.Fatalf("dropped = %d, want the 49 frames the stalled subscriber could not take", got)
	}
	if len(stalled) != 1 {
		t.Fatalf("stalled subscriber holds %d frames, want 1", len(stalled))
	}

	closeDone := make(chan struct{})
	go func() {
		defer close(closeDone)
		c.Close()
	}()
	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close deadlocked behind broadcast's read lock, so the transport never self-heals")
	}
}

// TestBroadcastStillDeliversToAFastSubscriber keeps the fix from turning into
// "drop everything": a subscriber that keeps up receives every frame.
func TestBroadcastStillDeliversToAFastSubscriber(t *testing.T) {
	c := newStalledClient()
	sub, cancel := c.Subscribe()
	defer cancel()

	go func() {
		for i := 0; i < 10; i++ {
			c.broadcast(Notification{Method: "session/update"})
		}
	}()
	for i := 0; i < 10; i++ {
		select {
		case n := <-sub:
			if n.Method != "session/update" {
				t.Fatalf("got %#v", n)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("frame %d never reached a subscriber with room", i)
		}
	}
	if got := c.DroppedNotifications(); got != 0 {
		t.Fatalf("dropped = %d, want 0 while every frame was accepted", got)
	}
}
