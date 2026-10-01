package lingmaipc

import (
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// errCloseSentinel is what the fake transport's Close reports, wrapped into a
// *net.OpError so it carries a concrete type the read/decode chains never produce.
var errCloseSentinel = errors.New("close frame write failed")

// errorChainTransport drives readLoop into one of its two error exits and then
// fails Close with an error of a different concrete type. That is the shape the
// 933 review found on a live connection: a ws that dies mid-turn makes readLoop
// record the read error, and the very next statement is the deferred Close,
// which records the transport's own close error.
type errorChainTransport struct {
	frame    []byte // delivered once, before readErr
	readErr  error  // returned once frame is exhausted
	closeErr error  // returned by Close

	mu     sync.Mutex
	served bool
}

func (t *errorChainTransport) ReadFrame() ([]byte, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.frame != nil && !t.served {
		t.served = true
		return t.frame, nil
	}
	return nil, t.readErr
}

func (t *errorChainTransport) WriteFrame([]byte) error { return nil }

func (t *errorChainTransport) Close() error { return t.closeErr }

func (t *errorChainTransport) Address() string { return "errorchain:" }

func newErrorChainClient(tr framedTransport) *Client {
	return &Client{
		transport: tr,
		pending:   make(map[int]chan responseEnvelope),
		subs:      make(map[int]chan Notification),
		closed:    make(chan struct{}),
	}
}

// TestCloseErrAcceptsHeterogeneousErrorTypes is the P2-L1 regression. closeErr
// is an atomic.Value, and atomic.Value panics when a later Store carries a
// different concrete type than the first one. The three write paths produce
// unrelated types -- a transport read error (*errors.errorString / *net.OpError),
// a decode wrapper (*fmt.wrapError) and a close error -- so a single dropped
// connection could panic the readLoop goroutine and take the whole proxy down.
// On the unfixed code this test never reaches its assertions: the second Store
// panics inside readLoop and aborts the test binary.
func TestCloseErrAcceptsHeterogeneousErrorTypes(t *testing.T) {
	tests := []struct {
		name string
		// firstErr is what the read/decode chain records first.
		firstErr error
		frame    []byte
	}{
		{
			// readLoop's transport read error: io.ErrUnexpectedEOF is an
			// *errors.errorString, which the close path never stores.
			name:     "read error then close error",
			firstErr: io.ErrUnexpectedEOF,
		},
		{
			// readLoop's decode failure: fmt.Errorf("...%w") is a *fmt.wrapError.
			name:     "decode error then close error",
			firstErr: io.EOF,
			frame:    []byte("{not json"),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tr := &errorChainTransport{
				frame:    tc.frame,
				readErr:  tc.firstErr,
				closeErr: &net.OpError{Op: "close", Net: "pipe", Err: errCloseSentinel},
			}
			c := newErrorChainClient(tr)

			go c.readLoop()

			select {
			case <-c.closed:
			case <-time.After(5 * time.Second):
				t.Fatal("readLoop never reached Close after its error exit")
			}

			// The second store already happened inside that Close. Reaching here
			// without a process-level panic is the point of the regression.
			got := c.Close()
			if got == nil {
				t.Fatal("Close returned nil, want the error the transport recorded")
			}
			if !errors.Is(got, errCloseSentinel) {
				t.Fatalf("Close returned %v, want the most recently recorded error to survive", got)
			}
			if fromLoop := c.closeError(); !errors.Is(fromLoop, errCloseSentinel) {
				t.Fatalf("closeError() returned %v, want the same recorded error Close returns", fromLoop)
			}
		})
	}
}

// TestCloseErrFallsBackToEOFWithoutARecordedError pins the read side of the
// same change: a transport that closes cleanly records nothing, so callers
// waiting on a closed client must still see io.EOF rather than a boxed nil.
func TestCloseErrFallsBackToEOFWithoutARecordedError(t *testing.T) {
	tr := &errorChainTransport{readErr: io.EOF}
	c := newErrorChainClient(tr)

	go c.readLoop()

	select {
	case <-c.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("readLoop never reached Close after a clean EOF")
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close returned %v, want nil when no error was recorded", err)
	}
	if err := c.closeError(); !errors.Is(err, io.EOF) {
		t.Fatalf("closeError() returned %v, want io.EOF", err)
	}
}
