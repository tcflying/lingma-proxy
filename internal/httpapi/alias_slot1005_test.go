package httpapi

import (
	"context"
	"fmt"
	"io"
	"lingma-ipc-proxy/internal/service"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These router regressions stop before normalization or generation: svc is nil.
// No listener, provider, credentials, CLI, or service construction is involved.
func TestAlias1005SingleSlotRouting(t *testing.T) {
	t.Setenv("LINGMA_PROXY_MAX_CONCURRENT", "1")
	old := slotQueueWait
	slotQueueWait = 5 * time.Millisecond
	t.Cleanup(func() { slotQueueWait = old })
	for _, path := range []string{"/v1/messages", "/anthropic/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			s := NewServer("", nil)
			for i := 0; i < 3; i++ {
				w := httptest.NewRecorder()
				s.http.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{")))
				if w.Code != http.StatusBadRequest {
					t.Fatalf("request %d status = %d, want 400; body=%s", i, w.Code, w.Body.String())
				}
				if len(s.sem) != 0 {
					t.Fatalf("request %d leaked %d slots", i, len(s.sem))
				}
			}
		})
	}
}

type alias1005ReadProbe struct{ reads int }

func (b *alias1005ReadProbe) Read([]byte) (int, error) { b.reads++; return 0, io.EOF }
func (b *alias1005ReadProbe) Close() error             { return nil }

func TestAlias1005BusyAndCancelled(t *testing.T) {
	t.Setenv("LINGMA_PROXY_MAX_CONCURRENT", "1")
	old := slotQueueWait
	slotQueueWait = 5 * time.Millisecond
	t.Cleanup(func() { slotQueueWait = old })
	for _, path := range []string{"/v1/messages", "/anthropic/v1/messages"} {
		for _, cancelled := range []bool{false, true} {
			s := NewServer("", nil)
			s.sem <- struct{}{}
			ctx, cancel := context.WithCancel(context.Background())
			if cancelled {
				cancel()
			}
			body := &alias1005ReadProbe{}
			r := httptest.NewRequest(http.MethodPost, path, body).WithContext(ctx)
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, r)
			cancel()
			want := http.StatusTooManyRequests
			if cancelled {
				want = http.StatusRequestTimeout
			}
			if w.Code != want {
				t.Fatalf("path=%s cancelled=%v status=%d want=%d", path, cancelled, w.Code, want)
			}
			if body.reads != 0 {
				t.Fatalf("busy request body read %d times", body.reads)
			}
			if len(s.sem) != 1 {
				t.Fatalf("busy request released another request's slot")
			}
			<-s.sem
			w = httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader("{")))
			if w.Code != http.StatusBadRequest || len(s.sem) != 0 {
				t.Fatalf("retry after release status=%d slots=%d", w.Code, len(s.sem))
			}
		}
	}
}

func TestAlias1005LocalAndNonPostRoutes(t *testing.T) {
	t.Setenv("LINGMA_PROXY_MAX_CONCURRENT", "1")
	for _, prefix := range []string{"", "/anthropic"} {
		s := NewServer("", nil)
		s.sem <- struct{}{}
		for _, tc := range []struct {
			method, path, body string
			status             int
		}{
			{http.MethodPost, prefix + "/v1/messages/count_tokens", `{"messages":[{"role":"user","content":"synthetic"}]}`, http.StatusOK},
			{http.MethodGet, prefix + "/v1/messages", "", http.StatusMethodNotAllowed},
			{http.MethodOptions, prefix + "/v1/messages", "", http.StatusNoContent},
		} {
			w := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body)))
			if w.Code != tc.status {
				t.Fatalf("%s %s status=%d want=%d", tc.method, tc.path, w.Code, tc.status)
			}
			if len(s.sem) != 1 {
				t.Fatalf("local route changed occupied slot")
			}
		}
	}
}

type alias1005Transport func(*http.Request) (*http.Response, error)

func (f alias1005Transport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Exercise the real Service.Generate path with a synthetic credential file and
// an in-memory transport. Nothing can dial or read an installed IDE profile.
func TestAlias1005SuccessfulGenerationAndConcurrentLimit(t *testing.T) {
	t.Setenv("LINGMA_PROXY_MAX_CONCURRENT", "1")
	oldWait := slotQueueWait
	slotQueueWait = 5 * time.Millisecond
	t.Cleanup(func() { slotQueueWait = oldWait })
	authFile := filepath.Join(t.TempDir(), "synthetic-auth.json")
	fixture := `{"source":"synthetic-alias1005","auth":{"cosy_key":"synthetic-key","encrypt_user_info":"synthetic-info","user_id":"synthetic-user","machine_id":"synthetic-machine"}}`
	if err := os.WriteFile(authFile, []byte(fixture), 0600); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	var calls atomic.Int32
	frames := hybrid931ContentFrame(t, "synthetic-alias-success") + hybrid931FinishFrame(t) + "data: [DONE]\n\n"
	oldTransport := http.DefaultTransport
	http.DefaultTransport = alias1005Transport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost || r.URL.Scheme != "http" || r.URL.Host != "alias1005.invalid" || r.URL.Path != "/algo/api/v2/service/pro/sse/agent_chat_generation" || r.URL.RawQuery != "FetchKeys=llm_model_result&AgentId=agent_common" {
			return nil, fmt.Errorf("unexpected synthetic request target")
		}
		if calls.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-r.Context().Done():
				return nil, r.Context().Err()
			}
		}

		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(frames)), Request: r}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	svc := service.New(service.Config{Backend: service.BackendRemote, RemoteBaseURL: "http://alias1005.invalid", RemoteAuthFile: authFile, Model: "kmodel", Timeout: time.Second})
	t.Cleanup(func() { _ = svc.Close() })
	s := NewServer("", svc)
	body := `{"model":"kmodel","max_tokens":8,"messages":[{"role":"user","content":"synthetic prompt"}]}`
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.http.Handler.ServeHTTP(first, httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body)))
	}()
	// Always join the worker before restoring shared transport or removing fixtures.
	defer func() { unblock(); <-done }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first request never reached synthetic transport")
	}
	if len(s.sem) != 1 {
		t.Fatalf("in-flight generation holds %d slots, want 1", len(s.sem))
	}
	second := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(second, httptest.NewRequest(http.MethodPost, "/anthropic/v1/messages", strings.NewReader(body)))
	if second.Code != http.StatusTooManyRequests || calls.Load() != 1 {
		t.Fatalf("concurrent request status=%d synthetic calls=%d; want 429 and 1", second.Code, calls.Load())
	}
	unblock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("synthetic generation did not finish")
	}
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), "synthetic-alias-success") || len(s.sem) != 0 {
		t.Fatalf("first response status=%d slots=%d body=%s", first.Code, len(s.sem), first.Body.String())
	}
	for _, path := range []string{"/anthropic/v1/messages", "/v1/messages"} {
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "synthetic-alias-success") || len(s.sem) != 0 {
			t.Fatalf("repeat %s status=%d slots=%d", path, w.Code, len(s.sem))
		}
	}
	if calls.Load() != 3 {
		t.Fatalf("synthetic calls=%d, want 3 successful generations", calls.Load())
	}
}
