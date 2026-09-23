package main

import (
	"bufio"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/service"
)

func testConsole() (*console, *httptest.Server) {
	app := &App{
		cfg: service.Config{Host: "127.0.0.1", Port: 10095, Backend: service.BackendQoderCLI},
	}
	app.running = true
	app.addr = "127.0.0.1:10095"
	c := &console{app: app, token: "tok-" + strings.Repeat("x", 12), statics: http.NotFoundHandler(), addr: "127.0.0.1:10096", url: "http://127.0.0.1:10096/"}
	return c, httptest.NewServer(c)
}

func TestConsoleRefusesRequestsWithoutTheToken(t *testing.T) {
	c, srv := testConsole()
	defer srv.Close()

	for _, tc := range []struct {
		name   string
		path   string
		header string
	}{
		{"no header", "/api/admin/status", ""},
		{"wrong token", "/api/admin/status", "Bearer nope"},
		{"prefix only", "/api/admin/status", "Bearer "},
		{"events with wrong query token", "/api/admin/events?token=nope", ""},
	} {
		req, err := http.NewRequest(http.MethodGet, srv.URL+tc.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if tc.header != "" {
			req.Header.Set("Authorization", tc.header)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s: got %d, want 401", tc.name, resp.StatusCode)
		}
	}
	if c.token == "" {
		t.Fatal("test console must carry a token")
	}
}

func TestConsoleReadsStateAndRejectsUnknownEndpoints(t *testing.T) {
	c, srv := testConsole()
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/status", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var status ProxyStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if !status.Running || status.Addr != "127.0.0.1:10095" {
		t.Fatalf("got %#v", status)
	}

	bad, err := http.Get(srv.URL + "/api/admin/nope")
	if err != nil {
		t.Fatal(err)
	}
	bad.Body.Close()
	// Unauthenticated first, so this proves the gate ordering: unknown routes
	// must not be enumerable without a token.
	if bad.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unknown endpoint without token: %d", bad.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodGet, srv.URL+"/api/admin/nope", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	gated, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	gated.Body.Close()
	if gated.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown endpoint with token: %d", gated.StatusCode)
	}
}

func TestConsoleInfoReportsTheBoundAddress(t *testing.T) {
	c, srv := testConsole()
	defer srv.Close()
	c.app.console = c

	got := c.app.ConsoleInfo()
	if !got.Serving || got.Addr != c.addr || got.Token != c.token {
		t.Fatalf("got %#v", got)
	}
	if got.URL != c.url {
		t.Fatalf("url %q", got.URL)
	}
	if empty := (&App{}).ConsoleInfo(); empty.Serving {
		t.Fatal("a disabled console must not report serving")
	}
}

func TestConsoleStreamDeliversPublishedEvents(t *testing.T) {
	c, srv := testConsole()
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/admin/events?token="+c.token, nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ctype := resp.Header.Get("Content-Type"); !strings.HasPrefix(ctype, "text/event-stream") {
		t.Fatalf("content type %q", ctype)
	}

	go func() {
		for i := 0; i < 30; i++ {
			c.publish("log", AppLog{Message: "hello console"})
			time.Sleep(50 * time.Millisecond)
		}
	}()

	line := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			if text := scanner.Text(); strings.HasPrefix(text, "data:") {
				line <- text
				return
			}
		}
		line <- ""
	}()
	select {
	case text := <-line:
		if !strings.Contains(text, "hello console") || !strings.Contains(text, `"log"`) {
			t.Fatalf("frame %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived on the console stream")
	}
}

func TestConsoleStaysOnLoopbackUnlessToldOtherwise(t *testing.T) {
	if got := consoleBindHost(); got != "127.0.0.1" {
		t.Fatalf("default bind %q, want 127.0.0.1 even when the proxy is public", got)
	}
	t.Setenv("LINGMA_CONSOLE_HOST", "0.0.0.0")
	if got := consoleBindHost(); got != "0.0.0.0" {
		t.Fatalf("opt-in wildcard must actually bind the wildcard, got %q", got)
	}
	if got := consoleDisplayHost("0.0.0.0"); got != "127.0.0.1" {
		t.Fatalf("wildcard should display as loopback, got %q", got)
	}
	t.Setenv("LINGMA_CONSOLE_HOST", "192.168.50.9")
	if got := consoleBindHost(); got != "192.168.50.9" {
		t.Fatalf("opt-in bind %q", got)
	}
}
