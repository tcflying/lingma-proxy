package remote

import (
	"net/http"
	"testing"
)

// 933 P3-R2 (remote half): imageFetchClient in internal/httpapi builds a bare
// &http.Transport, so it misses the two things every other egress in this process
// gets for free. This pins the reference side of that comparison — remote.Client
// must keep inheriting the proxy environment and the idle-connection reaper, so
// the image path has something to converge on instead of the other way round.
func TestClientTransportInheritsProxyEnvAndReapsIdleConnections(t *testing.T) {
	def, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("http.DefaultTransport is %T, want *http.Transport", http.DefaultTransport)
	}

	t.Run("no explicit proxy falls back to the shared transport", func(t *testing.T) {
		c := New(Config{BaseURL: "http://fixed.invalid", AuthFile: "unused"})
		if c.client.Transport != nil {
			t.Fatalf("Transport = %T, want nil so http.DefaultTransport applies", c.client.Transport)
		}
		if def.Proxy == nil {
			t.Fatal("http.DefaultTransport has no ProxyFromEnvironment to inherit")
		}
		if def.IdleConnTimeout <= 0 {
			t.Fatalf("http.DefaultTransport IdleConnTimeout = %v, want > 0 so idle connections are reaped", def.IdleConnTimeout)
		}
	})

	t.Run("explicit proxy clones the shared transport instead of replacing it", func(t *testing.T) {
		c := New(Config{BaseURL: "http://fixed.invalid", AuthFile: "unused", ProxyURL: "http://127.0.0.1:9"})
		tr, ok := c.client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("Transport = %T, want *http.Transport", c.client.Transport)
		}
		if tr == http.DefaultTransport.(*http.Transport) {
			t.Fatal("the shared transport was mutated in place instead of cloned")
		}
		if tr.IdleConnTimeout != def.IdleConnTimeout {
			t.Fatalf("IdleConnTimeout = %v, want the inherited %v", tr.IdleConnTimeout, def.IdleConnTimeout)
		}
		if tr.MaxIdleConns != def.MaxIdleConns {
			t.Fatalf("MaxIdleConns = %d, want the inherited %d", tr.MaxIdleConns, def.MaxIdleConns)
		}
		if tr.IdleConnTimeout <= 0 || tr.MaxIdleConns <= 0 {
			t.Fatal("a zero IdleConnTimeout or MaxIdleConns keeps connections resident forever")
		}
	})
}
