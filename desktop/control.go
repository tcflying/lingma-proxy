package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"lingma-ipc-proxy/internal/service"
)

// ConsoleInfo tells the desktop Settings page how to reach the browser UI.
type ConsoleInfo struct {
	URL     string `json:"url"`
	Token   string `json:"token"`
	Addr    string `json:"addr"`
	Serving bool   `json:"serving"`
}

// console is the browser control plane: the same App methods the Wails window
// calls, over token-gated HTTP. It deliberately does not share the proxy
// listener, because UpdateConfig restarts the proxy and would take down the
// very request that asked for it.
//
// ponytail: port is the proxy port +1; give it its own config field when
// someone needs to move it off that offset.
type console struct {
	app     *App
	token   string
	statics http.Handler
	addr    string
	url     string

	mu   sync.Mutex
	subs map[chan consoleEvent]struct{}
	srv  *http.Server
}

type consoleEvent struct {
	Name string `json:"name"`
	Data any    `json:"data"`
}

func (c *console) listen(addr string) (net.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	c.srv = &http.Server{
		Handler:           c,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := c.srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			c.app.emitLog("error", fmt.Sprintf("console server error: %v", err))
		}
	}()
	return ln, nil
}

func (c *console) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/admin/") {
		c.statics.ServeHTTP(w, r)
		return
	}
	if !c.authorized(r) {
		writeConsoleError(w, http.StatusUnauthorized, "missing or incorrect console token")
		return
	}
	a := c.app
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/admin/events":
		c.stream(w, r)
		return
	case r.Method == http.MethodGet:
		var payload any
		var err error
		switch r.URL.Path {
		case "/api/admin/version":
			payload = a.GetAppVersion()
		case "/api/admin/status":
			payload = a.GetStatus()
		case "/api/admin/config":
			payload = a.GetConfig()
		case "/api/admin/detection":
			payload = a.GetDetectionInfo()
		case "/api/admin/models":
			payload = a.GetModels()
		case "/api/admin/stats":
			payload = a.GetTokenStats()
		case "/api/admin/console":
			payload = a.ConsoleInfo()
		case "/api/admin/requests":
			payload = a.GetRequestSummaries()
		case "/api/admin/requests/detail":
			payload, err = a.GetRequestDetail(r.URL.Query().Get("id"))
		case "/api/admin/logs":
			payload = a.GetLogSummaries()
		case "/api/admin/logs/detail":
			payload, err = a.GetLogDetail(r.URL.Query().Get("id"))
		default:
			writeConsoleError(w, http.StatusNotFound, "unknown console endpoint")
			return
		}
		writeConsoleJSON(w, payload, err)
		return
	case r.Method == http.MethodPost:
		c.handleWrite(w, r)
		return
	default:
		writeConsoleError(w, http.StatusMethodNotAllowed, "use GET to read and POST to write")
	}
}

func (c *console) handleWrite(w http.ResponseWriter, r *http.Request) {
	a := c.app
	switch r.URL.Path {
	case "/api/admin/config":
		var cfg service.Config
		if err := decodeBody(r, &cfg); err != nil {
			writeConsoleError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeConsoleJSON(w, nil, a.UpdateConfig(cfg))
	case "/api/admin/models/refresh":
		models, err := a.RefreshModels()
		writeConsoleJSON(w, models, err)
	case "/api/admin/models/select":
		var body struct {
			Model string `json:"model"`
		}
		if err := decodeBody(r, &body); err != nil {
			writeConsoleError(w, http.StatusBadRequest, err.Error())
			return
		}
		status, err := a.SelectModel(body.Model)
		writeConsoleJSON(w, status, err)
	case "/api/admin/requests/clear":
		a.ClearRequests()
		writeConsoleJSON(w, nil, nil)
	case "/api/admin/logs/clear":
		a.ClearLogs()
		writeConsoleJSON(w, nil, nil)
	case "/api/admin/proxy/start":
		writeConsoleJSON(w, nil, a.StartProxy())
	case "/api/admin/proxy/stop":
		writeConsoleJSON(w, nil, a.StopProxy())
	case "/api/admin/proxy/restart":
		err := a.StopProxy()
		if err == nil {
			err = a.StartProxy()
		}
		writeConsoleJSON(w, nil, err)
	default:
		writeConsoleError(w, http.StatusNotFound, "unknown console endpoint")
	}
}

// authorized accepts the token as a bearer header; the event stream is also
// allowed through ?token= because EventSource cannot set headers.
func (c *console) authorized(r *http.Request) bool {
	if c.token == "" {
		return false
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	if subtle.ConstantTimeCompare([]byte(got), []byte(c.token)) == 1 {
		return true
	}
	if r.URL.Path != "/api/admin/events" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(c.token)) == 1
}

func decodeBody(r *http.Request, target any) error {
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}
	return nil
}

func writeConsoleJSON(w http.ResponseWriter, payload any, err error) {
	if err != nil {
		writeConsoleError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(payload)
}

func writeConsoleError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (c *console) publish(name string, data any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for sub := range c.subs {
		select {
		case sub <- consoleEvent{Name: name, Data: data}:
		default: // a stalled viewer drops frames rather than blocking the app
		}
	}
}

func (c *console) stream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeConsoleError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	events := make(chan consoleEvent, 64)
	c.mu.Lock()
	if c.subs == nil {
		c.subs = map[chan consoleEvent]struct{}{}
	}
	c.subs[events] = struct{}{}
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.subs, events)
		c.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case event := <-events:
			body, err := json.Marshal(event)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", body); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// newConsoleToken issues the once-per-install bearer token. It is stored in the
// per-instance app state so the desktop and the browser agree on one value.
func newConsoleToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// startConsole binds the browser control plane one port above the proxy.
func (a *App) startConsole() {
	token, err := a.ensureConsoleToken()
	if err != nil {
		a.emitLog("warn", "web console disabled: "+err.Error())
		return
	}
	statics, err := fs.Sub(assets, "frontend/dist")
	if err != nil {
		a.emitLog("warn", "web console disabled: "+err.Error())
		return
	}
	a.mu.RLock()
	port := a.cfg.Port
	a.mu.RUnlock()

	portPart := strconv.Itoa(port + 1)
	bind := consoleBindHost()
	c := &console{app: a, token: token, statics: http.FileServer(http.FS(statics))}
	ln, err := c.listen(net.JoinHostPort(bind, portPart))
	if err != nil {
		a.emitLog("warn", "web console could not bind "+bind+":"+portPart+": "+err.Error())
		return
	}
	c.addr = ln.Addr().String()
	c.url = "http://" + net.JoinHostPort(consoleDisplayHost(bind), portPart) + "/"
	a.mu.Lock()
	a.console = c
	a.mu.Unlock()
	if bind != "127.0.0.1" {
		a.emitLog("warn", "web console is reachable from the network at "+c.addr+" over plain HTTP; the bearer token is sent in the clear")
	}
	// The token stays out of the persisted log ring: stdout carries it for a
	// terminal start, the Settings page for a double-click start.
	runtime.LogInfof(a.wailsCtx(), "Web console: %s/#token=%s", c.url, token)
	a.emitLog("info", "Web 控制台："+c.url+"（令牌见设置页或 app-state 的 admin_token）")
}

// ConsoleInfo backs the Settings page entry that shows the URL and token.
func (a *App) ConsoleInfo() ConsoleInfo {
	a.mu.RLock()
	c := a.console
	a.mu.RUnlock()
	if c == nil {
		return ConsoleInfo{}
	}
	return ConsoleInfo{URL: c.url, Token: c.token, Addr: c.addr, Serving: true}
}

// publishEvent mirrors a Wails broadcast to browsers watching the event stream.
func (a *App) publishEvent(name string, data any) {
	a.mu.RLock()
	c := a.console
	a.mu.RUnlock()
	if c != nil {
		c.publish(name, data)
	}
}

func (a *App) ensureConsoleToken() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.consoleToken != "" {
		return a.consoleToken, nil
	}
	token, err := newConsoleToken()
	if err != nil {
		return "", err
	}
	a.consoleToken = token
	a.flushAppStateLocked()
	return token, nil
}

// consoleBindHost is where the control plane listens. It stays on loopback even
// when the proxy serves the network: the console can rewrite config, restart the
// proxy and read request bodies, and a bearer token over plain HTTP is not
// something to hand to a LAN by accident. Set LINGMA_CONSOLE_HOST to opt into
// another address.
func consoleBindHost() string {
	if value := strings.TrimSpace(os.Getenv("LINGMA_CONSOLE_HOST")); value != "" {
		return value
	}
	return "127.0.0.1"
}

// consoleDisplayHost turns a wildcard bind into an address one can actually open.
func consoleDisplayHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" || host == "0.0.0.0" || host == "::" || host == "[::]" {
		return "127.0.0.1"
	}
	return host
}

// The console page and its API share one origin, so no CORS is needed: a foreign
// localhost port is a different app and must not get cross-origin write access to
// config, restart and recorded request bodies.
