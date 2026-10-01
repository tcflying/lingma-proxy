package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"lingma-ipc-proxy/internal/service"
)

// TestHeadlessLogLeavesTheConsoleAlone pins the thing that kept 192.168.50.239
// unreachable: the startup logs must not go through the console the Task
// Scheduler owns, because a console that stops servicing writes parks the
// process before it reaches net.Listen.
func TestHeadlessLogLeavesTheConsoleAlone(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("headlessLog is Windows-only by design")
	}
	dir := t.TempDir()
	t.Setenv("APPDATA", dir)
	previous := log.Writer()
	t.Cleanup(func() { log.SetOutput(previous) })
	// The production path keeps this handle for the lifetime of the process, which
	// is the point; only the test has to let go of it before TempDir cleans up.
	t.Cleanup(func() {
		if f, ok := log.Writer().(*os.File); ok {
			f.Close()
		}
	})

	headlessLog()
	log.Printf("marker-line-for-the-console")

	data, err := os.ReadFile(filepath.Join(dir, "lingma-proxy", "headless.log"))
	if err != nil {
		t.Fatalf("headlessLog did not create a log file: %v", err)
	}
	if !strings.Contains(string(data), "marker-line-for-the-console") {
		t.Fatalf("marker missing from %q", data)
	}
	if previous == log.Writer() {
		t.Fatal("log output is still the inherited writer")
	}
}

// TestConfigArgsThePreScanMustSee is the regression for the pre-scan that
// resolveConfigPath runs before flag.Parse. Go's flag package accepts both
// "-config v" and "--config v"; a pre-scan that only looks for the double-dash
// spelling reads the default file instead of the one the operator named, and
// the startup log then prints the named path as if it had been loaded.
func TestConfigArgsThePreScanMustSee(t *testing.T) {
	for _, spelling := range []string{"-config", "--config"} {
		for _, form := range []struct {
			name string
			args []string
		}{
			{"separate value", []string{spelling, "named.json"}},
			{"equals value", []string{spelling + "=named.json"}},
		} {
			t.Run(spelling+"/"+form.name, func(t *testing.T) {
				fs := flag.NewFlagSet("lingma-proxy", flag.ContinueOnError)
				fs.SetOutput(io.Discard)
				parsed := fs.String("config", "default.json", "")
				// FlagSet.Parse takes the arguments without the command name,
				// which is what the top-level flag.Parse feeds it as os.Args[1:].
				if err := fs.Parse(form.args); err != nil {
					t.Fatalf("flag.Parse rejected %v: %v", form.args, err)
				}
				if *parsed != "named.json" {
					t.Fatalf("test premise broken: flag.Parse resolved -config to %q", *parsed)
				}
				// The pre-scan has to agree with the parser that runs after it;
				// when it does not, the file the flag names is never read.
				got := ""
				withArgs(t, form.args, func() { got = lookupArgValue("config") })
				if got != *parsed {
					t.Fatalf("pre-scan of %v returned %q, flag.Parse resolved %q", form.args, got, *parsed)
				}
			})
		}
	}
}

// TestResolveConfigPathLoadsTheNamedFileNotTheDefault is the user-visible half
// of the same defect: with a default lingma-proxy.json sitting in the working
// directory, "-config other.json" used to load the default and report
// configLoaded=true for the default path, so the mistake survived all the way
// into the startup log.
func TestResolveConfigPathLoadsTheNamedFileNotTheDefault(t *testing.T) {
	dir := chdirTemp(t)
	writeConfigFile(t, filepath.Join(dir, "lingma-proxy.json"), `{"port":10095}`)
	named := filepath.Join(dir, "other.json")
	writeConfigFile(t, named, `{"port":20095}`)

	for _, arg := range [][]string{{"-config", named}, {"--config", named}} {
		t.Run(strings.TrimLeft(arg[0], "-")+" separated", func(t *testing.T) {
			withArgs(t, arg, func() {
				got, loaded := resolveConfigPath()
				if !loaded {
					t.Fatalf("%v was ignored: no config file was selected", arg)
				}
				if got != named {
					t.Fatalf("%v resolved to %q, want the named file %q", arg, got, named)
				}
			})
		})
	}

	// And the file the resolver points at has to be the one whose bytes win.
	cfg, err := readFileConfig(named)
	if err != nil {
		t.Fatalf("read named config: %v", err)
	}
	if cfg.Port != 20095 {
		t.Fatalf("named config carried port %d, want 20095", cfg.Port)
	}
}

// TestResolveConfigPathFallsBackWithoutTheFlag keeps the other direction honest:
// no -config, no env var, no file on disk must still resolve to the default path
// with configLoaded=false, which is what stops loadConfig from trying to read
// a file that is not there.
func TestResolveConfigPathFallsBackWithoutTheFlag(t *testing.T) {
	dir := chdirTemp(t)
	t.Setenv("LINGMA_PROXY_CONFIG", "")
	withArgs(t, []string{"lingma-proxy", "-port", "8095"}, func() {
		got, loaded := resolveConfigPath()
		if loaded {
			t.Fatalf("an empty working directory reported a loaded config: %q", got)
		}
		if want := filepath.Join(dir, "lingma-proxy.json"); got != want {
			t.Fatalf("fallback path is %q, want %q", got, want)
		}
	})
}

// TestReadFileConfigNamesTheUnknownKey is the operational half of the loader:
// a misspelled key used to be dropped with no trace, so the operator saw a
// setting that silently never took effect.
func TestReadFileConfigNamesTheUnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lingma-proxy.json")
	writeConfigFile(t, path, `{"port":20095,"remoate_base_url":"http://127.0.0.1:1","typo":true}`)

	var cfg fileConfig
	var err error
	logged := captureLog(t, func() { cfg, err = readFileConfig(path) })
	if err != nil {
		t.Fatalf("a misspelled key must not stop the file from loading: %v", err)
	}
	if cfg.Port != 20095 {
		t.Fatalf("the known key beside the typo was lost: port=%d", cfg.Port)
	}
	for _, key := range []string{"remoate_base_url", "typo"} {
		if !strings.Contains(logged, key) {
			t.Fatalf("unknown key %q went unreported; log was:\n%s", key, logged)
		}
	}
}

// TestReadFileConfigAcceptsEveryDeclaredKey is the other direction: the warning
// is only useful if it stays quiet for keys the struct really does declare.
// The JSON is generated from the struct itself, so adding a field to fileConfig
// without teaching the key set about it fails here instead of in production.
func TestReadFileConfigAcceptsEveryDeclaredKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lingma-proxy.json")
	writeConfigFile(t, path, everyDeclaredKeyJSON(t))

	var err error
	logged := captureLog(t, func() { _, err = readFileConfig(path) })
	if err != nil {
		t.Fatalf("a config using every declared key failed to load: %v", err)
	}
	if strings.TrimSpace(logged) != "" {
		t.Fatalf("declared keys were reported as unknown:\n%s", logged)
	}
}

// TestEnvFallbacksAreLoud covers the environment half. A value that does not
// parse used to fall back to the default in silence, which is the same
// "setting does nothing" failure as a misspelled key, only harder to notice
// because nothing was written down at all.
func TestEnvFallbacksAreLoud(t *testing.T) {
	t.Run("int", func(t *testing.T) {
		t.Setenv("LINGMA_PROXY_PORT", "80x95")
		var cfg service.Config
		cfg.Port = 8095
		logged := captureLog(t, func() { overlayEnvConfig(&cfg) })
		if cfg.Port != 8095 {
			t.Fatalf("a malformed port changed the config to %d", cfg.Port)
		}
		if !strings.Contains(logged, "LINGMA_PROXY_PORT") {
			t.Fatalf("the ignored LINGMA_PROXY_PORT went unreported; log was:\n%s", logged)
		}
	})
	t.Run("int timeout", func(t *testing.T) {
		t.Setenv("LINGMA_PROXY_TIMEOUT_SECONDS", "soon")
		var cfg service.Config
		logged := captureLog(t, func() { overlayEnvConfig(&cfg) })
		if !strings.Contains(logged, "LINGMA_PROXY_TIMEOUT_SECONDS") {
			t.Fatalf("the ignored LINGMA_PROXY_TIMEOUT_SECONDS went unreported; log was:\n%s", logged)
		}
	})
	t.Run("bool", func(t *testing.T) {
		t.Setenv("LINGMA_REMOTE_FALLBACK_ENABLED", "maybe")
		var cfg service.Config
		cfg.RemoteFallbackEnabled = true
		logged := captureLog(t, func() { overlayEnvConfig(&cfg) })
		if !cfg.RemoteFallbackEnabled {
			t.Fatal("a malformed bool flipped the setting it failed to parse")
		}
		if !strings.Contains(logged, "LINGMA_REMOTE_FALLBACK_ENABLED") {
			t.Fatalf("the ignored LINGMA_REMOTE_FALLBACK_ENABLED went unreported; log was:\n%s", logged)
		}
	})
}

// TestEnvFallbacksStayQuietWhenValid keeps the warning from turning into noise:
// an unset variable and a well-formed one are not misconfigurations.
func TestEnvFallbacksStayQuietWhenValid(t *testing.T) {
	t.Setenv("LINGMA_PROXY_PORT", "10095")
	t.Setenv("LINGMA_PROXY_TIMEOUT_SECONDS", "30")
	t.Setenv("LINGMA_REMOTE_FALLBACK_ENABLED", "off")
	var cfg service.Config
	cfg.Port = 8095
	cfg.Timeout = 7 * time.Second
	cfg.RemoteFallbackEnabled = true
	logged := captureLog(t, func() { overlayEnvConfig(&cfg) })
	if strings.TrimSpace(logged) != "" {
		t.Fatalf("valid environment values produced warnings:\n%s", logged)
	}
	if cfg.Port != 10095 || cfg.Timeout != 30*time.Second || cfg.RemoteFallbackEnabled {
		t.Fatalf("valid environment values were not applied: port=%d timeout=%s fallback=%v", cfg.Port, cfg.Timeout, cfg.RemoteFallbackEnabled)
	}
}

// everyDeclaredKeyJSON marshals the zero value of every fileConfig field under
// its own JSON key, so the unknown-key check is exercised against the real
// struct rather than a hand-written list that can drift from it.
func everyDeclaredKeyJSON(t *testing.T) string {
	t.Helper()
	typ := reflect.TypeOf(fileConfig{})
	fields := make(map[string]json.RawMessage, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		encoded, err := json.Marshal(reflect.Zero(typ.Field(i).Type).Interface())
		if err != nil {
			t.Fatalf("marshal zero %s: %v", name, err)
		}
		fields[name] = encoded
	}
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal declared keys: %v", err)
	}
	return string(body)
}

// TestGoSourcesAreCheckedOutAsLF pins the repository line-ending policy. The
// working tree is 45 CRLF files against 8 LF ones, and that split is invisible
// right up until someone clones with a different core.autocrlf -- at which point
// the 8 LF files show as whole-file rewrites and blame points at the wrong
// commits. The policy is one line of .gitattributes, and the cheapest guard
// against losing it is a test that fails the day the file goes away.
func TestGoSourcesAreCheckedOutAsLF(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", ".gitattributes"))
	if err != nil {
		t.Fatalf("the repository declares no line-ending policy: %v", err)
	}
	var text, eol string
	for _, line := range strings.Split(strings.ReplaceAll(string(body), "\r\n", "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || strings.HasPrefix(fields[0], "#") || fields[0] != "*.go" {
			continue
		}
		for _, field := range fields[1:] {
			name, value, hasValue := strings.Cut(field, "=")
			switch {
			case name == "text":
				// "text" is legal bare (meaning true); "text=auto" sets a mode.
				text = "set"
				if hasValue {
					text = value
				}
			case name == "eol" && hasValue:
				eol = value
			}
		}
	}
	if text == "" || eol == "" {
		t.Fatalf("no *.go rule normalises line endings; .gitattributes says:\n%s", body)
	}
	// "text" alone still normalises in the index but leaves the working tree to
	// core.autocrlf, which is the half of the problem this file exists to remove.
	if eol != "lf" && eol != "crlf" {
		t.Fatalf("the *.go rule names no fixed working-tree ending (eol=%q)", eol)
	}
}

// TestQoderCLITurnCeilingReachesTheCLIBackend covers the entry's half of the
// turn backstop. internal/qodercli owns the knob and reads it from the
// environment on every turn, so the flag has to be handed over there -- but
// only when the operator actually passed it, because a process started without
// the flag has to behave exactly as it did before the flag existed.
func TestQoderCLITurnCeilingReachesTheCLIBackend(t *testing.T) {
	t.Setenv(qodercliTurnCeilingEnv, "from-the-environment")

	applyQoderCLITurnCeiling("25m", false)
	if got := os.Getenv(qodercliTurnCeilingEnv); got != "from-the-environment" {
		t.Fatalf("an unset flag overwrote the environment: %q", got)
	}

	applyQoderCLITurnCeiling("  25m  ", true)
	if got := os.Getenv(qodercliTurnCeilingEnv); got != "25m" {
		t.Fatalf("the explicit flag did not reach the CLI backend, env holds %q", got)
	}

	// 0 is the documented way to switch the backstop off, so it has to survive
	// the trip rather than be mistaken for "not set".
	applyQoderCLITurnCeiling("0", true)
	if got := os.Getenv(qodercliTurnCeilingEnv); got != "0" {
		t.Fatalf("the value that disables the ceiling was lost, env holds %q", got)
	}
}

// TestQoderCLITurnCeilingIsDocumentedAtTheEntry is the finding itself: an
// operator reads -h and the env var, and neither mentioned the knob. The help
// text has to say which variable the CLI backend reads, and -- because the
// whole point of the knob is that -timeout 0 still means "no proxy deadline" --
// that this is a separate setting rather than a shorter -timeout.
func TestQoderCLITurnCeilingIsDocumentedAtTheEntry(t *testing.T) {
	if !strings.Contains(qodercliTurnCeilingUsage, qodercliTurnCeilingEnv) {
		t.Fatalf("the flag help never names %s:\n%s", qodercliTurnCeilingEnv, qodercliTurnCeilingUsage)
	}
	if !strings.Contains(qodercliTurnCeilingUsage, "-timeout") {
		t.Fatalf("the flag help does not say how it relates to -timeout:\n%s", qodercliTurnCeilingUsage)
	}
	if !strings.Contains(qodercliTurnCeilingUsage, "0") {
		t.Fatalf("the flag help does not say how to switch the backstop off:\n%s", qodercliTurnCeilingUsage)
	}
}

// TestProxyDeadlineDefaultIsUntouched pins the one thing this entry must not
// change while documenting the ceiling: -timeout keeps meaning "0 = no proxy
// deadline". A shape check, because the default lives in a struct literal no
// unit test can reach without running the whole flag set.
func TestProxyDeadlineDefaultIsUntouched(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	normalised := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !strings.Contains(normalised, "\t\tTimeout:               0,\n") {
		t.Fatal("the entry no longer defaults the proxy deadline to 0")
	}
	if !strings.Contains(normalised, `flag.Int("timeout"`) {
		t.Fatal("the -timeout flag is gone")
	}
}

// TestStartupRedirectsBeforeAnythingElse guards the other half of the invariant:
// main must call headlessLog before it can log anything at all. It is a shape
// check on purpose -- "the first statement of main" is exactly what the blocked
// console write breaks, and no unit test can observe a real console selection.
func TestStartupRedirectsBeforeAnythingElse(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	// Compare against a newline-normalised copy. The shape being checked is "the
	// first statement of main", and pinning the literal LF sequence tested the
	// working tree's line endings instead: on a CRLF checkout (core.autocrlf) this
	// failed on a main() that is perfectly correct, and said nothing about the
	// invariant it names.
	normalised := strings.ReplaceAll(string(src), "\r\n", "\n")
	if !strings.Contains(normalised, "func main() {\n\theadlessLog()\n") {
		t.Fatal("main() no longer redirects the log as its first act")
	}
}

// chdirTemp moves the process into an empty directory for the duration of the
// test. resolveConfigPath probes the working directory for a default config, so
// a test about explicit paths has to stand somewhere that has none -- and the
// package directory is not it, it just happens to be clean today.
func chdirTemp(t *testing.T) string {
	t.Helper()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	return dir
}

// withArgs replaces the command line the pre-scan reads and restores it after.
func withArgs(t *testing.T, args []string, fn func()) {
	t.Helper()
	previous := os.Args
	os.Args = append([]string{"lingma-proxy"}, args...)
	t.Cleanup(func() { os.Args = previous })
	fn()
}

func writeConfigFile(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// captureLog redirects the standard logger for the duration of fn and returns
// everything it wrote. Config complaints are only observable through the log --
// the loader has no channel to report them on.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	previousOut := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousOut)
		log.SetFlags(previousFlags)
	})
	fn()
	return buf.String()
}
