package lingmaipc

import (
	"testing"
	"time"
)

type a1BlockedClose struct {
	errorChainTransport
	entered, release chan struct{}
}

func (t *a1BlockedClose) Close() error { close(t.entered); <-t.release; return nil }
func TestA1Candidate1005SubscribeDuringClose(t *testing.T) {
	tr := &a1BlockedClose{entered: make(chan struct{}), release: make(chan struct{})}
	c := newErrorChainClient(tr)
	buffered, cancel := c.Subscribe()
	defer cancel()
	c.broadcast(Notification{Method: "queued-finish"})
	done := make(chan struct{})
	go func() { _ = c.Close(); close(done) }()
	<-tr.entered
	// Close has signalled closed, but has not yet acquired subsMu for closing old channels.
	fresh, stop := c.Subscribe()
	stop()
	select {
	case _, ok := <-fresh:
		if ok {
			t.Error("unexpected post-close notification")
		}
	case <-time.After(time.Second):
		t.Error("registered into closing client")
	}
	close(tr.release)
	<-done
	if n, ok := <-buffered; !ok || n.Method != "queued-finish" {
		t.Fatal("buffered frame lost")
	}
	if _, ok := <-buffered; ok {
		t.Fatal("old subscription not closed")
	}
}
