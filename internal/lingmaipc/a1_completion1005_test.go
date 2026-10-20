package lingmaipc

import (
	"testing"
	"time"
)

func TestA1Completion1005ClosedSubscribe(t *testing.T) {
	c := newErrorChainClient(&errorChainTransport{})
	_ = c.Close()
	ch, cancel := c.Subscribe()
	defer cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("closed client delivered unexpected notification")
		}
	case <-time.After(50 * time.Millisecond):
		t.Error("subscription registered after Close never becomes terminal")
	}
}
func TestA1Completion1005BufferedFinishControl(t *testing.T) {
	c := newErrorChainClient(&errorChainTransport{})
	ch, cancel := c.Subscribe()
	defer cancel()
	c.broadcast(Notification{Method: "session/update"})
	_ = c.Close()
	n, ok := <-ch
	if !ok || n.Method != "session/update" {
		t.Fatal("buffered notification lost")
	}
	if _, ok := <-ch; ok {
		t.Fatal("channel not terminal")
	}
}
