package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Position encodings are the classic source of off-by-N bugs in LSP clients:
// Go counts bytes, JavaScript counts UTF-16 units, and the protocol defaults to
// UTF-16 while some servers negotiate UTF-8.
func TestPositionEncodings(t *testing.T) {
	t.Parallel()
	// "héllo → wörld" mixes 1-, 2- and 3-byte runes.
	line := "héllo → wörld"
	cases := []struct {
		enc   string
		word  string
		wantC int
	}{
		{"utf-8", "wörld", strings.Index(line, "wörld")}, // bytes
		{"utf-16", "wörld", 8},                           // h é l l o ␠ → ␠
		{"utf-32", "wörld", 8},                           // same here: no surrogates
	}
	for _, c := range cases {
		cl := &lspClient{encoding: c.enc}
		byteCol := strings.Index(line, c.word)
		got := cl.toLSP(line, 1, byteCol)
		if got.Character != c.wantC {
			t.Errorf("%s: toLSP character = %d, want %d", c.enc, got.Character, c.wantC)
		}
		if got.Line != 0 {
			t.Errorf("%s: toLSP line = %d, want 0 (LSP is 0-based)", c.enc, got.Line)
		}
		// Round-tripping must land back on the same byte.
		gotLine, gotByte := cl.fromLSP([]string{line}, got)
		if gotLine != 1 || gotByte != byteCol {
			t.Errorf("%s: round trip = %d:%d, want 1:%d", c.enc, gotLine, gotByte, byteCol)
		}
	}
}

func TestUTF16ToByte(t *testing.T) {
	t.Parallel()
	cases := []struct {
		line string
		u16  int
		want int
	}{
		{"abc", 0, 0},
		{"abc", 2, 2},
		{"abc", 99, 3},  // past the end clamps
		{"héllo", 2, 3}, // é is two bytes
		{"→x", 1, 3},    // → is three bytes, one UTF-16 unit
		{"🎉x", 2, 4},    // emoji is a surrogate pair: two units, four bytes
		{"🎉x", 3, 5},
		{"", 5, 0},
	}
	for _, c := range cases {
		if got := utf16ToByte(c.line, c.u16); got != c.want {
			t.Errorf("utf16ToByte(%q, %d) = %d, want %d", c.line, c.u16, got, c.want)
		}
	}
}

func TestURIRoundTrip(t *testing.T) {
	t.Parallel()
	paths := []string{
		"/home/user/project/main.go",
		"/home/user/my project/a b.go", // spaces must be escaped
		"/tmp/weird#name$x.go",
	}
	if runtime.GOOS == "windows" {
		paths = []string{`C:\Users\me\main.go`}
	}
	for _, p := range paths {
		uri := pathToURI(p)
		if !strings.HasPrefix(uri, "file://") {
			t.Errorf("pathToURI(%q) = %q, want a file:// uri", p, uri)
		}
		if strings.Contains(uri, " ") {
			t.Errorf("pathToURI(%q) = %q, spaces must be percent-encoded", p, uri)
		}
		got, err := uriToPath(uri)
		if err != nil {
			t.Fatalf("uriToPath(%q): %v", uri, err)
		}
		if filepath.Clean(got) != filepath.Clean(p) {
			t.Errorf("round trip %q -> %q -> %q", p, uri, got)
		}
	}
	if _, err := uriToPath("https://example.com/x.go"); err == nil {
		t.Error("uriToPath accepted a non-file uri")
	}
}

// A disabled manager must never resolve a server, whatever is installed.
func TestManagerDisabled(t *testing.T) {
	t.Parallel()
	m := newLSPManager(t.TempDir(), false)
	if got := m.Available(); len(got) != 0 {
		t.Errorf("disabled manager offers %v", got)
	}
	if d := m.defFor("x.go"); d != nil {
		t.Errorf("disabled manager resolved %v for x.go", d.Name)
	}
	if state, _ := m.State("x.go"); state != lspOff {
		t.Errorf("disabled manager state = %v, want off", state)
	}
}

// Only paths a server actually named may be opened outside the indexed tree.
func TestExternalAllowlist(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ix := NewIndex(root)
	ix.Build()
	m := newLSPManager(root, false)
	s := NewServer(ix, m)

	if _, _, ok := s.resolvePath("/etc/passwd"); ok {
		t.Error("resolvePath allowed an arbitrary absolute path")
	}
	m.allow("/usr/lib/go/src/strings/builder.go")
	abs, _, ok := s.resolvePath("/usr/lib/go/src/strings/builder.go")
	if !ok || abs != "/usr/lib/go/src/strings/builder.go" {
		t.Errorf("resolvePath refused an allowlisted path: %q %v", abs, ok)
	}
	if _, _, ok := s.resolvePath("/usr/lib/go/src/strings/other.go"); ok {
		t.Error("allowlisting one file allowed a sibling")
	}
	if _, _, ok := s.resolvePath("../../../etc/shadow"); ok {
		t.Error("relative traversal still escapes the root")
	}
}

func TestLanguageIDMapping(t *testing.T) {
	t.Parallel()
	var ts lspServerDef
	for _, d := range lspRegistry {
		if d.Name == "typescript" {
			ts = d
		}
	}
	cases := map[string]string{
		"a.ts": "typescript", "a.tsx": "typescriptreact",
		"a.jsx": "javascriptreact", "a.js": "javascript", "a.mjs": "javascript",
	}
	for file, want := range cases {
		if got := ts.LanguageID(file); got != want {
			t.Errorf("LanguageID(%q) = %q, want %q", file, got, want)
		}
	}
}

func TestLSPCloseDoc(t *testing.T) {
	t.Parallel()
	cl := newLSPClient(lspServerDef{Name: "test", Cmd: []string{"echo"}}, t.TempDir())
	cl.opened["file:///test.go"] = 1

	// closeDoc removes from opened map
	cl.closeDoc("/test.go")
	cl.mu.Lock()
	_, exists := cl.opened["file:///test.go"]
	cl.mu.Unlock()
	if exists {
		t.Fatal("expected uri to be removed from opened map")
	}

	// Repeated closeDoc does not panic
	cl.closeDoc("/test.go")
}

// A crashed client is replaced by a fresh spawn, a bounded number of times.
func TestCrashedServerIsRespawned(t *testing.T) {
	def := lspServerDef{Name: "gone", Cmd: []string{"px0-test-no-such-language-server"}, Exts: []string{".go"}}
	dead := func() *lspClient {
		c := newLSPClient(def, t.TempDir())
		c.fail(errors.New("gone exited"))
		return c
	}
	m := &lspManager{
		root: t.TempDir(), enabled: true,
		byExt:    map[string]*lspServerDef{".go": &def},
		clients:  map[string]*lspClient{"gone": dead()},
		starting: map[string]chan struct{}{},
		failed:   map[string]string{},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The respawn is attempted; the binary does not exist, so it fails to start.
	if _, err := m.client(ctx, "x.go"); !isErrFailed(err) {
		t.Fatalf("client() = %v, want a failed respawn rather than the stale crash", err)
	}
	if m.restarts["gone"] != 1 {
		t.Errorf("restarts = %d, want 1", m.restarts["gone"])
	}

	// Out of budget: the crash is reported and nothing is spawned.
	m.mu.Lock()
	delete(m.failed, "gone")
	m.clients["gone"] = dead()
	m.restarts["gone"] = maxLSPRestarts
	m.mu.Unlock()
	if _, err := m.client(ctx, "x.go"); err == nil || isErrFailed(err) {
		t.Errorf("client() past the restart budget = %v, want the crash error", err)
	}
}

func isErrFailed(err error) bool {
	_, ok := err.(errFailed)
	return ok
}

// fakeCallServer wires an lspClient to an in-process responder over pipes, so
// the call hierarchy plumbing is exercised without a real language server.
func fakeCallServer(t *testing.T, root string, answer func(method string, params json.RawMessage) any) *lspManager {
	t.Helper()
	def := lspServerDef{Name: "fake", Cmd: []string{"fake"}, Exts: []string{".go"}}
	c := newLSPClient(def, root)
	toServerR, toServerW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	c.in, c.out = toServerW, bufio.NewReader(toClientR)
	go c.readLoop()

	var wmu sync.Mutex
	go func() {
		in := bufio.NewReader(toServerR)
		for {
			msg, err := readFrame(in)
			if err != nil {
				return
			}
			if len(msg.ID) == 0 {
				continue // notifications such as didOpen
			}
			body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": answer(msg.Method, msg.Params)})
			wmu.Lock()
			fmt.Fprintf(toClientW, "Content-Length: %d\r\n\r\n%s", len(body), body)
			wmu.Unlock()
		}
	}()
	t.Cleanup(func() { toServerW.Close(); toClientW.Close() })

	return &lspManager{
		root: root, enabled: true,
		byExt:    map[string]*lspServerDef{".go": &def},
		clients:  map[string]*lspClient{"fake": c},
		starting: map[string]chan struct{}{},
		failed:   map[string]string{},
	}
}

func callItem(name, uri string, line int) map[string]any {
	pos := map[string]any{"line": line, "character": 5}
	rng := map[string]any{"start": pos, "end": pos}
	return map[string]any{"name": name, "kind": 12, "uri": uri, "range": rng, "selectionRange": rng, "data": map[string]any{"token": name}}
}

func siteRange(line int) map[string]any {
	pos := map[string]any{"line": line, "character": 1}
	return map[string]any{"start": pos, "end": pos}
}

func TestCallTrail(t *testing.T) {
	root := t.TempDir()
	src := "package a\n\nfunc A() { C(); B() }\n\nfunc B() {}\n"
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	aURI := pathToURI(filepath.Join(root, "a.go"))
	const extPath = "/usr/lib/go/src/fmt/print.go"
	extURI := pathToURI(extPath)

	var mu sync.Mutex
	sent := map[string]string{}
	m := fakeCallServer(t, root, func(method string, params json.RawMessage) any {
		mu.Lock()
		sent[method] = string(params)
		mu.Unlock()
		switch method {
		case "textDocument/prepareCallHierarchy":
			return []any{callItem("B", aURI, 4)}
		case "callHierarchy/incomingCalls":
			return []any{
				map[string]any{"from": callItem("A", aURI, 2), "fromRanges": []any{siteRange(2), siteRange(2)}},
				map[string]any{"from": callItem("Println", extURI, 9), "fromRanges": []any{siteRange(12)}},
			}
		case "callHierarchy/outgoingCalls":
			return []any{
				map[string]any{"to": callItem("B", aURI, 4), "fromRanges": []any{siteRange(2)}},
				map[string]any{"to": callItem("C", extURI, 0), "fromRanges": []any{siteRange(1)}},
			}
		}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	roots, err := m.PrepareCalls(ctx, filepath.Join(root, "a.go"), "a.go", 5, 5)
	if err != nil || len(roots) != 1 {
		t.Fatalf("PrepareCalls = %v, %v; want one root", roots, err)
	}
	b := roots[0]
	if b.Name != "B" || b.Path != "a.go" || b.Line != 5 || b.Kind != "func" {
		t.Errorf("root = %+v, want B at a.go:5 (func)", b)
	}

	callers, err := m.Calls(ctx, "a.go", b.Item, false)
	if err != nil || len(callers) != 2 {
		t.Fatalf("incoming = %v, %v; want two callers", callers, err)
	}
	mu.Lock()
	echoed := sent["callHierarchy/incomingCalls"]
	mu.Unlock()
	if !strings.Contains(echoed, `"token":"B"`) {
		t.Errorf("item was not echoed verbatim to the server: %s", echoed)
	}
	// Sorted by path: the absolute external path sorts before "a.go".
	ext, a := callers[0], callers[1]
	if !ext.Ext || ext.Path != extPath || !m.Allowed(extPath) {
		t.Errorf("external caller = %+v, allowed=%v; want ext and allowlisted", ext, m.Allowed(extPath))
	}
	if a.Name != "A" || a.SitePath != "a.go" || fmt.Sprint(a.Sites) != "[3]" {
		t.Errorf("caller A = %+v, want call site a.go:[3] deduplicated", a)
	}

	callees, err := m.Calls(ctx, "a.go", a.Item, true)
	if err != nil || len(callees) != 2 {
		t.Fatalf("outgoing = %v, %v; want two callees", callees, err)
	}
	if callees[0].Name != "C" || callees[1].Name != "B" {
		t.Errorf("callees = %s, %s; want call order C, B", callees[0].Name, callees[1].Name)
	}
	if callees[0].SitePath != "a.go" || fmt.Sprint(callees[0].Sites) != "[2]" {
		t.Errorf("callee C sites = %s %v, want a.go [2] (inside the caller)", callees[0].SitePath, callees[0].Sites)
	}
}

// An item arrives from the browser, so its path must not open the filesystem.
func TestCallTrailForgedItem(t *testing.T) {
	root := t.TempDir()
	m := fakeCallServer(t, root, func(string, json.RawMessage) any { return []any{} })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	forged, _ := json.Marshal(callItem("x", pathToURI("/etc/passwd"), 0))
	if _, err := m.Calls(ctx, "a.go", string(forged), false); err != nil {
		t.Fatalf("Calls: %v", err)
	}
	if m.Allowed("/etc/passwd") {
		t.Error("a browser-supplied item allowlisted a path outside the tree")
	}
	if _, err := m.Calls(ctx, "a.go", `{"name":"x"}`, false); err == nil {
		t.Error("an item without a uri was accepted")
	}
}

// ---------------------------------------------------------------- setup & install tests

// Only a POST from px0's own page, addressed by IP or localhost, may install.
func TestLocalPost(t *testing.T) {
	cases := []struct {
		name, method, host, origin string
		want                       bool
	}{
		{"same origin", "POST", "127.0.0.1:7777", "http://127.0.0.1:7777", true},
		{"localhost", "POST", "localhost:7777", "http://localhost:7777", true},
		{"ipv6 loopback", "POST", "[::1]:7777", "http://[::1]:7777", true},
		{"GET", "GET", "127.0.0.1:7777", "http://127.0.0.1:7777", false},
		{"no origin", "POST", "127.0.0.1:7777", "", false},
		{"other site", "POST", "127.0.0.1:7777", "https://evil.example", false},
		{"dns rebinding", "POST", "evil.example:7777", "http://evil.example:7777", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/api/lsp/install", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if got := localPost(httptest.NewRecorder(), r); got != c.want {
			t.Errorf("%s: localPost = %v, want %v", c.name, got, c.want)
		}
	}
}

// A binary outside PATH is still found in one of the extra folders.
func TestLookPathInExtraDir(t *testing.T) {
	dir := t.TempDir()
	name := "px0-test-fake-language-server"
	file := name
	if runtime.GOOS == "windows" {
		file += ".exe"
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := lookPathIn(name, nil); ok {
		t.Fatalf("%s found without its folder; the test binary name collides with something on PATH", name)
	}
	if p, ok := lookPathIn(name, []string{t.TempDir(), dir}); !ok || filepath.Dir(p) != dir {
		t.Errorf("lookPathIn = %q, %v; want a path in %s", p, ok, dir)
	}
}

// Every recipe is well formed, and nothing px0 runs by itself asks for root.
func TestInstallRecipes(t *testing.T) {
	for _, def := range lspRegistry {
		if def.Lang == "" {
			t.Errorf("%s: no Lang", def.Name)
		}
		for _, in := range def.Install {
			if len(in.Cmd) < 2 {
				t.Errorf("%s: install command %q too short", def.Name, in.Cmd)
			}
			switch in.OS {
			case "", "darwin", "linux", "windows":
			default:
				t.Errorf("%s: unknown OS %q", def.Name, in.OS)
			}
			if in.Auto && (in.Cmd[0] == "sudo" || in.Cmd[0] == "winget") {
				t.Errorf("%s: %q needs an administrator and must not be Auto", def.Name, strings.Join(in.Cmd, " "))
			}
		}
	}
}

func TestInstallRefusals(t *testing.T) {
	off := newLSPManager(t.TempDir(), false)
	if _, err := off.Install("gopls", 0); err == nil {
		t.Error("Install with language servers disabled succeeded")
	}
	m := &lspManager{enabled: true}
	if _, err := m.Install("px0-no-such-server", 0); err == nil {
		t.Error("Install of an unknown server succeeded")
	}
	if _, err := m.Install("gopls", 99); err == nil {
		t.Error("Install with an out-of-range option succeeded")
	}
	for _, def := range lspRegistry {
		for i, in := range def.installsFor(runtime.GOOS) {
			if !in.Auto {
				if _, err := m.Install(def.Name, i); err == nil {
					t.Errorf("Install ran the manual recipe %q", strings.Join(in.Cmd, " "))
				}
			}
		}
	}
}

func TestTailBufferKeepsTheEnd(t *testing.T) {
	b := &tailBuffer{max: 8}
	b.Write([]byte("0123456789"))
	b.Write([]byte("ab"))
	if got := b.String(); got != "456789ab" {
		t.Errorf("tail = %q, want %q", got, "456789ab")
	}
}

func TestLSPServersEndpoint(t *testing.T) {
	s, root := newTestServer(t)
	s.lsp = newLSPManager(root, true)

	// Ensure we have Go and Python files
	_ = os.WriteFile(filepath.Join(root, "app.py"), []byte("print('hello')\n"), 0o644)
	s.ix.Build()

	req := httptest.NewRequest(http.MethodGet, "/api/lsp/servers", nil)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var res struct {
		Enabled    bool             `json:"enabled"`
		AnyRunning bool             `json:"anyRunning"`
		Servers    []map[string]any `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("json parse error: %v", err)
	}

	foundGo := false
	foundPy := false
	for _, srv := range res.Servers {
		name, _ := srv["name"].(string)
		if name == "gopls" {
			foundGo = true
		}
		if name == "pyright" || name == "pylsp" || name == "ruff" {
			foundPy = true
		}
	}
	if !foundGo {
		t.Errorf("expected gopls in relevant servers for Go files")
	}
	if !foundPy {
		t.Errorf("expected python language server in relevant servers for Python files")
	}
}

func TestLSPStopAndStart(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real LSP stop/start in short mode")
	}
	s, root := newTestServer(t)
	s.lsp = newLSPManager(root, true)
	_ = os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n"), 0o644)

	// Stop gopls
	stopReq := httptest.NewRequest(http.MethodPost, "/api/lsp/stop?server=gopls", nil)
	stopReq.Host = "127.0.0.1:8080"
	stopReq.Header.Set("Origin", "http://127.0.0.1:8080")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, stopReq)

	if rec.Code != http.StatusOK {
		t.Fatalf("stop returned code %d", rec.Code)
	}

	state, _ := s.lsp.State("main.go")
	if state != lspOff {
		t.Errorf("expected lspOff after stop, got %v", state)
	}

	// Verify Stop All
	stopAllReq := httptest.NewRequest(http.MethodPost, "/api/lsp/stop", nil)
	stopAllReq.Host = "127.0.0.1:8080"
	stopAllReq.Header.Set("Origin", "http://127.0.0.1:8080")
	rec = httptest.NewRecorder()
	s.ServeHTTP(rec, stopAllReq)

	if rec.Code != http.StatusOK {
		t.Fatalf("stop all returned code %d", rec.Code)
	}

	if s.lsp.AnyRunning() {
		t.Errorf("expected no servers running after stop all")
	}
}

// ---------------------------------------------------------------- concurrency & race regression tests

// lspRaceFakeServer records the notifications a client sends.
type lspRaceFakeServer struct {
	mu            sync.Mutex
	didOpen       int
	didChangeVers []int
	didChangeRaw  []json.RawMessage // raw didChange params, for shape assertions
	toCliW        *io.PipeWriter
}

func (s *lspRaceFakeServer) run(r *io.PipeReader) {
	br := bufio.NewReader(r)
	for {
		msg, err := readFrame(br)
		if err != nil {
			return
		}
		if len(msg.ID) > 0 {
			if s.toCliW != nil {
				resp := map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(msg.ID), "result": nil}
				b, _ := json.Marshal(resp)
				frame := append([]byte(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(b))), b...)
				s.toCliW.Write(frame)
			}
			continue
		}
		s.mu.Lock()
		switch msg.Method {
		case "textDocument/didOpen":
			s.didOpen++
		case "textDocument/didChange":
			var p struct {
				TextDocument struct {
					Version int `json:"version"`
				} `json:"textDocument"`
			}
			if json.Unmarshal(msg.Params, &p) == nil {
				s.didChangeVers = append(s.didChangeVers, p.TextDocument.Version)
			}
			s.didChangeRaw = append(s.didChangeRaw, msg.Params)
		}
		s.mu.Unlock()
	}
}

func lspRaceWireClient(t testing.TB) (*lspClient, *lspRaceFakeServer) {
	t.Helper()
	cl := newLSPClient(lspServerDef{Name: "fake", Cmd: []string{"fake"}}, t.TempDir())
	toSrvR, toSrvW := io.Pipe()
	toCliR, toCliW := io.Pipe()
	cl.in = toSrvW
	cl.out = bufio.NewReader(toCliR)
	srv := &lspRaceFakeServer{toCliW: toCliW}
	go srv.run(toSrvR)
	go cl.readLoop()
	cl.beginWriteLoop()
	t.Cleanup(func() {
		toSrvR.Close()
		toSrvW.Close()
		toCliR.Close()
		toCliW.Close()
	})
	return cl, srv
}

func TestEnsureOpenSendsSingleDidOpen(t *testing.T) {
	rounds := 3
	if testing.Short() {
		rounds = 1
	}
	for round := 0; round < rounds; round++ {
		cl, srv := lspRaceWireClient(t)
		path := filepath.Join(t.TempDir(), "f.go")
		if err := os.WriteFile(path, []byte("package main\n"), 0644); err != nil {
			t.Fatal(err)
		}
		const n = 32
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = cl.ensureOpen(path, "f.go")
			}()
		}
		wg.Wait()
		for start := time.Now(); time.Since(start) < 100*time.Millisecond; time.Sleep(time.Millisecond) {
			srv.mu.Lock()
			done := srv.didOpen >= 1
			srv.mu.Unlock()
			if done {
				break
			}
		}
		srv.mu.Lock()
		got := srv.didOpen
		srv.mu.Unlock()
		if got != 1 {
			t.Fatalf("round %d: server got %d textDocument/didOpen, want exactly 1", round, got)
		}
	}
}

func TestSyncDocVersionsIncreaseMonotonically(t *testing.T) {
	rounds := 3
	if testing.Short() {
		rounds = 1
	}
	for round := 0; round < rounds; round++ {
		cl, srv := lspRaceWireClient(t)
		path := filepath.Join(t.TempDir(), "f.go")
		if err := os.WriteFile(path, []byte("package main\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := cl.ensureOpen(path, "f.go"); err != nil {
			t.Fatalf("ensureOpen: %v", err)
		}
		if err := os.WriteFile(path, []byte("package main\n\n// changed\n"), 0644); err != nil {
			t.Fatal(err)
		}
		const n = 64
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = cl.syncDoc(path, "f.go")
			}()
		}
		wg.Wait()
		for start := time.Now(); time.Since(start) < 100*time.Millisecond; time.Sleep(time.Millisecond) {
			srv.mu.Lock()
			done := len(srv.didChangeVers) > 0
			srv.mu.Unlock()
			if done {
				break
			}
		}
		srv.mu.Lock()
		vers := append([]int(nil), srv.didChangeVers...)
		srv.mu.Unlock()

		seen := map[int]bool{}
		for _, v := range vers {
			if seen[v] {
				t.Fatalf("round %d: duplicate didChange version %d in %v", round, v, vers)
			}
			seen[v] = true
		}
		if len(vers) == 0 {
			t.Fatalf("round %d: no didChange sent for a real content change", round)
		}
		min, max := vers[0], vers[0]
		for _, v := range vers[1:] {
			if v < min {
				min = v
			}
			if v > max {
				max = v
			}
		}
		if min != 2 || max-min+1 != len(vers) {
			t.Fatalf("round %d: versions %v are not contiguous starting at 2", round, vers)
		}
	}
}

func TestWriteDoesNotHoldMutexDuringPipeIO(t *testing.T) {
	cl := newLSPClient(lspServerDef{Name: "fake", Cmd: []string{"fake"}}, t.TempDir())
	r, w := io.Pipe()
	cl.in = w
	cl.beginWriteLoop()
	t.Cleanup(func() { w.Close(); r.Close() })

	big := strings.Repeat("x", 1<<20)
	done := make(chan error, 1)
	go func() {
		done <- cl.notify("textDocument/didOpen", map[string]any{
			"textDocument": map[string]any{"uri": "file:///big.go", "text": big},
		})
	}()
	time.Sleep(300 * time.Millisecond)

	aliveCh := make(chan error, 1)
	go func() { aliveCh <- cl.alive() }()
	select {
	case err := <-aliveCh:
		if err != nil {
			t.Fatalf("alive() = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("alive() blocked for 2s: write() holds the client mutex during pipe I/O")
	}

	w.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("notify never returned after the pipe was closed")
	}
}

func TestCallCleansPendingOnMarshalFailure(t *testing.T) {
	cl := newLSPClient(lspServerDef{Name: "fake", Cmd: []string{"fake"}}, t.TempDir())
	err := cl.call(context.Background(), "test/method", func() {}, nil)
	if err == nil {
		t.Fatal("expected json.Marshal to fail on a func param")
	}
	cl.mu.Lock()
	n := len(cl.pending)
	cl.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending holds %d entries after marshal failure: leaked", n)
	}
}

type lspRaceFailWriter struct{}

func (lspRaceFailWriter) Write([]byte) (int, error) { return 0, errors.New("boom") }
func (lspRaceFailWriter) Close() error              { return nil }

func TestCallCleansPendingOnWriteFailure(t *testing.T) {
	cl := newLSPClient(lspServerDef{Name: "fake", Cmd: []string{"fake"}}, t.TempDir())
	cl.in = lspRaceFailWriter{}
	cl.beginWriteLoop()
	err := cl.call(context.Background(), "test/method", map[string]any{"a": 1}, nil)
	if err == nil {
		t.Fatal("expected the write to fail")
	}
	cl.mu.Lock()
	n := len(cl.pending)
	cl.mu.Unlock()
	if n != 0 {
		t.Fatalf("pending holds %d entries after write failure: leaked", n)
	}
}

func TestReplyDoesNotBlockReadLoop(t *testing.T) {
	cl := newLSPClient(lspServerDef{Name: "fake", Cmd: []string{"fake"}}, t.TempDir())
	_, toServerW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	cl.in, cl.out = toServerW, bufio.NewReader(toClientR)
	cl.beginWriteLoop()
	go cl.readLoop()

	sendFrame := func(body string) {
		t.Helper()
		if _, err := io.WriteString(toClientW, "Content-Length: "+strconv.Itoa(len(body))+"\r\n\r\n"+body); err != nil {
			t.Fatalf("send frame: %v", err)
		}
	}
	sendFrame(`{"jsonrpc":"2.0","id":1,"method":"workspace/configuration","params":{}}`)
	deadline := time.Now().Add(5 * time.Second)
	for {
		sendFrame(`{"jsonrpc":"2.0","method":"$/progress","params":{"token":"t","value":{"kind":"begin"}}}`)
		time.Sleep(50 * time.Millisecond)
		if cl.busy() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("readLoop stuck: reply() blocked on a synchronous write")
		}
	}
	toServerW.Close()
	toClientW.Close()
}

func TestSyncDocStaleReadDiscarded(t *testing.T) {
	cl, srv := lspRaceWireClient(t)
	path := filepath.Join(t.TempDir(), "f.go")
	uri := pathToURI(path)
	if err := os.WriteFile(path, []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cl.ensureOpen(path, "f.go"); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); time.Since(start) < 50*time.Millisecond; time.Sleep(time.Millisecond) {
		srv.mu.Lock()
		done := srv.didOpen >= 1
		srv.mu.Unlock()
		if done {
			break
		}
	}

	if err := os.WriteFile(path, []byte("package new\n// fresh-marker-bbb\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cl.syncDoc(path, "f.go"); err != nil {
		t.Fatal(err)
	}
	for start := time.Now(); time.Since(start) < 50*time.Millisecond; time.Sleep(time.Millisecond) {
		srv.mu.Lock()
		done := len(srv.didChangeRaw) >= 1
		srv.mu.Unlock()
		if done {
			break
		}
	}
	srv.mu.Lock()
	nBefore := len(srv.didChangeRaw)
	srv.mu.Unlock()

	cl.mu.Lock()
	cl.syncGen[uri] = cl.syncGenNext.Add(1) + 1000
	cl.mu.Unlock()

	if err := os.WriteFile(path, []byte("package old\n// stale-marker-aaa\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cl.syncDoc(path, "f.go"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.didChangeRaw) != nBefore {
		t.Fatalf("stale syncDoc sent didChange: got %d, want %d", len(srv.didChangeRaw), nBefore)
	}
	for _, raw := range srv.didChangeRaw {
		if strings.Contains(string(raw), "stale-marker-aaa") {
			t.Fatalf("server got stale didChange: %s", raw)
		}
	}
	cl.mu.RLock()
	recorded := cl.openedText[uri]
	cl.mu.RUnlock()
	if !strings.Contains(recorded, "fresh-marker-bbb") {
		t.Fatalf("client recorded stale text: %q", recorded)
	}
}

func TestCallRespectsContextDuringFlush(t *testing.T) {
	cl := newLSPClient(lspServerDef{Name: "fake", Cmd: []string{"fake"}}, t.TempDir())
	_, toServerW := io.Pipe()
	toClientR, toClientW := io.Pipe()
	cl.in, cl.out = toServerW, bufio.NewReader(toClientR)
	cl.beginWriteLoop()
	go cl.readLoop()
	t.Cleanup(func() {
		toServerW.Close()
		toClientW.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := cl.call(ctx, "test/blockedFlush", map[string]any{}, nil)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("call returned %v, want context.DeadlineExceeded", err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("call blocked for %v, want it to respect the 500ms context", elapsed)
	}
	cl.mu.RLock()
	n := len(cl.pending)
	cl.mu.RUnlock()
	if n != 0 {
		t.Fatalf("pending holds %d entries after ctx timeout: leaked", n)
	}
}

// ---------------------------------------------------------------- incremental sync tests

func applyRangedChange(t *testing.T, cl *lspClient, old string, start, end lspPosition, repl string) string {
	t.Helper()
	lines := strings.Split(old, "\n")
	lineOff := make([]int, len(lines))
	off := 0
	for i, ln := range lines {
		lineOff[i] = off
		off += len(ln) + 1
	}
	toOff := func(p lspPosition) int {
		if p.Line < 0 || p.Line >= len(lines) {
			t.Fatalf("position %+v out of range for text %q", p, old)
		}
		_, byteCol := cl.fromLSP(lines, p)
		return lineOff[p.Line] + byteCol
	}
	so, eo := toOff(start), toOff(end)
	if so < 0 || eo > len(old) || so > eo {
		t.Fatalf("bad offsets %d,%d for text %q", so, eo, old)
	}
	return old[:so] + repl + old[eo:]
}

func checkRoundTrip(t *testing.T, cl *lspClient, old, newText string) {
	t.Helper()
	dStart, dEnd, dRepl, ok := diffRange(old, newText)
	if !ok {
		t.Fatalf("diffRange refused %q -> %q", old, newText)
	}
	changes, ok := incrementalChange(cl, old, newText)
	if !ok {
		t.Fatalf("incrementalChange refused %q -> %q", old, newText)
	}
	if len(changes) != 1 {
		t.Fatalf("expected 1 change, got %d", len(changes))
	}
	rng, ok := changes[0]["range"].(map[string]any)
	if !ok {
		t.Fatalf("change has no range: %v", changes[0])
	}
	start, ok1 := rng["start"].(lspPosition)
	end, ok2 := rng["end"].(lspPosition)
	repl, ok3 := changes[0]["text"].(string)
	if !ok1 || !ok2 || !ok3 {
		t.Fatalf("malformed change: %v", changes[0])
	}
	got := applyRangedChange(t, cl, old, start, end, repl)
	if got != newText {
		t.Fatalf("round trip mismatch (encoding %s):\nold: %q\nnew: %q\ngot: %q\nrange: %+v -> %+v repl %q\ndiffRange bytes: [%d,%d) repl %q",
			cl.encoding, old, newText, got, start, end, repl, dStart, dEnd, dRepl)
	}
	lines := strings.Split(old, "\n")
	lineOff := make([]int, len(lines))
	off := 0
	for i, ln := range lines {
		lineOff[i] = off
		off += len(ln) + 1
	}
	resolve := func(p lspPosition) int {
		_, byteCol := cl.fromLSP(lines, p)
		return lineOff[p.Line] + byteCol
	}
	if so, eo := resolve(start), resolve(end); so != dStart || eo != dEnd {
		t.Fatalf("positions resolve to [%d,%d), diffRange said [%d,%d) (encoding %s, old %q)",
			so, eo, dStart, dEnd, cl.encoding, old)
	}
}

func TestIncrementalChangeRoundTrip(t *testing.T) {
	cases := []struct{ old, new string }{
		{"", "hello"},
		{"hello", ""},
		{"hello", "hello world"},
		{"hello world", "hello"},
		{"foo\nbar\nbaz\n", "foo\nBAR\nbaz\n"},
		{"line1\nline2", "line1\nline2\nline3"},
		{"line1\nline2\nline3", "line1\nline3"},
		{"a\n\nb", "a\n\n\nb"},
		{"trailing\n", "trailing"},
		{"\nleading", "leading"},
		{"héllo → wörld", "héllo → WORLD"},
		{"func 你好() {}", "func 你好世界() {}"},
		{"🎉 party", "🎊 party"},
		{"a🎉b", "a🎉🎉b"},
		{"🎉", "🎉🎉🎉"},
		{"x = \"é\" // café", "x = \"è\" // café"},
		{"aaa", "aaaaaa"},
		{"aaaaaa", "aaa"},
		{"abcdef", "abXYZef"},
		{"abcdef", "XYZabcdefXYZ"},
		{"one\ntwo\nthree", "one\n2\nthree"},
		{"x", "y"},
		{"\n\n\n", "\n\n"},
		{"no-newline-at-end", "no-newline-at-end!"},
	}
	for _, enc := range []string{"utf-8", "utf-16", "utf-32"} {
		cl := &lspClient{encoding: enc}
		for _, tc := range cases {
			checkRoundTrip(t, cl, tc.old, tc.new)
		}
	}
}

func TestIncrementalChangeRejectsCR(t *testing.T) {
	cl := &lspClient{encoding: "utf-16"}
	for _, tc := range []struct{ old, new string }{
		{"a\r\nb", "a\r\nB"},
		{"a\nb", "a\r\nb"},
		{"a\rb", "a\nb"},
	} {
		if _, ok := incrementalChange(cl, tc.old, tc.new); ok {
			t.Errorf("incrementalChange(%q, %q) = ok, want full-text fallback for CR", tc.old, tc.new)
		}
	}
}

func TestParseIncrementalSync(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{``, false},
		{`null`, false},
		{`0`, false},
		{`1`, false},
		{`2`, true},
		{`3`, false},
		{`{}`, false},
		{`{"change": 1}`, false},
		{`{"change": 2}`, true},
		{`{"openClose": true}`, false},
		{`{"change": 2, "openClose": true, "willSave": true}`, true},
		{`{"change": 0}`, false},
		{`"incremental"`, false},
		{`[2]`, false},
	}
	for _, tc := range cases {
		if got := parseIncrementalSync(json.RawMessage(tc.raw)); got != tc.want {
			t.Errorf("parseIncrementalSync(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

func randLSPText(rng *rand.Rand, tokens []string, n int) string {
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(tokens[rng.Intn(len(tokens))])
	}
	return sb.String()
}

func mutateLSPText(rng *rand.Rand, old string, tokens []string) string {
	r := []rune(old)
	for k := 0; k < 1+rng.Intn(3); k++ {
		switch rng.Intn(3) {
		case 0:
			pos := rng.Intn(len(r) + 1)
			ins := []rune(tokens[rng.Intn(len(tokens))])
			r = append(r[:pos:pos], append(ins, r[pos:]...)...)
		case 1:
			if len(r) == 0 {
				continue
			}
			pos := rng.Intn(len(r))
			n := 1 + rng.Intn(3)
			if pos+n > len(r) {
				n = len(r) - pos
			}
			r = append(r[:pos:pos], r[pos+n:]...)
		default:
			if len(r) == 0 {
				continue
			}
			pos := rng.Intn(len(r))
			n := 1 + rng.Intn(2)
			if pos+n > len(r) {
				n = len(r) - pos
			}
			rep := []rune(tokens[rng.Intn(len(tokens))])
			r = append(r[:pos:pos], append(rep, r[pos+n:]...)...)
		}
	}
	return string(r)
}

func TestIncrementalChangeFuzz(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	tokens := []string{"a", "Z", "é", "→", "ü", "你", "好", "🎉", "🚀", "\n", " ", "\t",
		"func", "main", "(", ")", "{", "}", "// c", "x=1;"}
	for i := 0; i < 3000; i++ {
		old := randLSPText(rng, tokens, 1+rng.Intn(80))
		newText := mutateLSPText(rng, old, tokens)
		if newText == old {
			continue
		}
		for _, enc := range []string{"utf-8", "utf-16", "utf-32"} {
			checkRoundTrip(t, &lspClient{encoding: enc}, old, newText)
		}
	}
}

func TestSyncDocSendsRangedChangeWhenIncremental(t *testing.T) {
	cl, srv := lspRaceWireClient(t)
	cl.syncIncremental = true
	path := filepath.Join(t.TempDir(), "f.go")
	oldText := "package main\n\nfunc main() {}\n"
	newText := "package main\n\nfunc main() { println(1) }\n"
	if err := os.WriteFile(path, []byte(oldText), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cl.ensureOpen(path, "f.go"); err != nil {
		t.Fatalf("ensureOpen: %v", err)
	}
	if err := os.WriteFile(path, []byte(newText), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cl.syncDoc(path, "f.go"); err != nil {
		t.Fatalf("syncDoc: %v", err)
	}
	for start := time.Now(); time.Since(start) < 100*time.Millisecond; time.Sleep(time.Millisecond) {
		srv.mu.Lock()
		n := len(srv.didChangeRaw)
		srv.mu.Unlock()
		if n >= 1 {
			break
		}
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.didChangeRaw) != 1 {
		t.Fatalf("got %d didChange frames, want 1", len(srv.didChangeRaw))
	}
	var p struct {
		TextDocument struct {
			Version int `json:"version"`
		} `json:"textDocument"`
		ContentChanges []struct {
			Range *struct {
				Start lspPosition `json:"start"`
				End   lspPosition `json:"end"`
			} `json:"range"`
			Text string `json:"text"`
		} `json:"contentChanges"`
	}
	if err := json.Unmarshal(srv.didChangeRaw[0], &p); err != nil {
		t.Fatalf("unmarshal didChange: %v", err)
	}
	if p.TextDocument.Version != 2 {
		t.Errorf("version = %d, want 2", p.TextDocument.Version)
	}
	if len(p.ContentChanges) != 1 || p.ContentChanges[0].Range == nil {
		t.Fatalf("didChange was not a ranged change: %s", srv.didChangeRaw[0])
	}
	r := p.ContentChanges[0].Range
	if got := applyRangedChange(t, &lspClient{encoding: "utf-16"}, oldText, r.Start, r.End, p.ContentChanges[0].Text); got != newText {
		t.Errorf("ranged change does not reconstruct file:\ngot:  %q\nwant: %q", got, newText)
	}
	if len(p.ContentChanges[0].Text) >= len(newText) {
		t.Errorf("ranged change text is %d bytes for a %d-byte file: no savings",
			len(p.ContentChanges[0].Text), len(newText))
	}
}

func TestSyncDocFallsBackToFullTextWhenNotIncremental(t *testing.T) {
	cl, srv := lspRaceWireClient(t)
	path := filepath.Join(t.TempDir(), "f.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cl.ensureOpen(path, "f.go"); err != nil {
		t.Fatalf("ensureOpen: %v", err)
	}
	newText := "package main\n\n// changed\n"
	if err := os.WriteFile(path, []byte(newText), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cl.syncDoc(path, "f.go"); err != nil {
		t.Fatalf("syncDoc: %v", err)
	}
	for start := time.Now(); time.Since(start) < 100*time.Millisecond; time.Sleep(time.Millisecond) {
		srv.mu.Lock()
		n := len(srv.didChangeRaw)
		srv.mu.Unlock()
		if n >= 1 {
			break
		}
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.didChangeRaw) != 1 {
		t.Fatalf("got %d didChange frames, want 1", len(srv.didChangeRaw))
	}
	var p struct {
		ContentChanges []map[string]any `json:"contentChanges"`
	}
	if err := json.Unmarshal(srv.didChangeRaw[0], &p); err != nil {
		t.Fatalf("unmarshal didChange: %v", err)
	}
	if len(p.ContentChanges) != 1 {
		t.Fatalf("want 1 content change, got %d", len(p.ContentChanges))
	}
	if _, hasRange := p.ContentChanges[0]["range"]; hasRange {
		t.Errorf("non-incremental server got a ranged change: %s", srv.didChangeRaw[0])
	}
	if p.ContentChanges[0]["text"] != newText {
		t.Errorf("full-text change = %q, want %q", p.ContentChanges[0]["text"], newText)
	}
}

func TestSyncDocSkipsUnchangedContent(t *testing.T) {
	cl, srv := lspRaceWireClient(t)
	path := filepath.Join(t.TempDir(), "f.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cl.ensureOpen(path, "f.go"); err != nil {
		t.Fatalf("ensureOpen: %v", err)
	}
	if err := cl.syncDoc(path, "f.go"); err != nil {
		t.Fatalf("syncDoc: %v", err)
	}
	if err := cl.syncDoc(path, "f.go"); err != nil {
		t.Fatalf("syncDoc: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.didChangeVers) != 0 {
		t.Fatalf("got %d didChange for unchanged content, want 0", len(srv.didChangeVers))
	}
}

func TestSyncDocDoesNotRecordVersionOnSendFailure(t *testing.T) {
	cl, _ := lspRaceWireClient(t)
	path := filepath.Join(t.TempDir(), "f.go")
	if err := os.WriteFile(path, []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cl.ensureOpen(path, "f.go"); err != nil {
		t.Fatalf("ensureOpen: %v", err)
	}
	cl.mu.Lock()
	cl.in = lspRaceFailWriter{}
	cl.mu.Unlock()
	if err := os.WriteFile(path, []byte("package main\n// changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := cl.syncDoc(path, "f.go"); err == nil {
		t.Fatal("expected syncDoc to fail on a broken pipe")
	}
	uri := pathToURI(path)
	cl.mu.RLock()
	v := cl.opened[uri]
	txt := cl.openedText[uri]
	cl.mu.RUnlock()
	if v != 1 {
		t.Errorf("version = %d after failed send, want 1 so the retry reuses it", v)
	}
	if txt != "package main\n" {
		t.Errorf("text recorded despite failed send: %q", txt)
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	cl, srv := lspRaceWireClient(t)
	_ = srv
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cl.shutdown()
		}()
	}
	wg.Wait()
	cl.shutdown()
}

// ---------------------------------------------------------------- benchmarks

var lspBenchLines = map[string]string{
	"ascii": "func main() { fmt.Println(\"hello, world\") } // trailing comment here",
	"latin": "héllo → wörld: naïve café résumé",
	"cjk":   "func 你好世界() { fmt.Println(\"日本語テスト\") } // コメント",
	"emoji": "🎉🚀✨ " + strings.Repeat("x🎉", 40),
}

var lspBenchLongLine = strings.Repeat("héllo → wörld 🎉 x = 42; ", 500)

func BenchmarkLSPToLSP(b *testing.B) {
	for name, line := range lspBenchLines {
		for _, enc := range []string{"utf-8", "utf-16", "utf-32"} {
			b.Run(name+"/"+enc, func(b *testing.B) {
				cl := &lspClient{encoding: enc}
				col := len(line) / 2
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_ = cl.toLSP(line, 7, col)
				}
			})
		}
	}
}

func BenchmarkLSPFromLSP(b *testing.B) {
	for name, line := range lspBenchLines {
		for _, enc := range []string{"utf-8", "utf-16", "utf-32"} {
			b.Run(name+"/"+enc, func(b *testing.B) {
				cl := &lspClient{encoding: enc}
				pos := cl.toLSP(line, 7, len(line)/2)
				lines := []string{line}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					_, _ = cl.fromLSP(lines, pos)
				}
			})
		}
	}
}

func BenchmarkLSPToLSPLongLine(b *testing.B) {
	for _, enc := range []string{"utf-8", "utf-16", "utf-32"} {
		b.Run(enc, func(b *testing.B) {
			cl := &lspClient{encoding: enc}
			col := len(lspBenchLongLine) * 3 / 4
			b.ReportAllocs()
			b.SetBytes(int64(len(lspBenchLongLine)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				_ = cl.toLSP(lspBenchLongLine, 100, col)
			}
		})
	}
}

func BenchmarkLSPReadFrame(b *testing.B) {
	for _, size := range []int{512, 8192, 65536} {
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			payload, err := json.Marshal(map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "textDocument/didOpen",
				"params": map[string]any{"pad": strings.Repeat("x", size)},
			})
			if err != nil {
				b.Fatal(err)
			}
			var frame bytes.Buffer
			fmt.Fprintf(&frame, "Content-Length: %d\r\n\r\n", len(payload))
			frame.Write(payload)
			data := frame.Bytes()
			b.ReportAllocs()
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r := bufio.NewReader(bytes.NewReader(data))
				if _, err := readFrame(r); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkLSPReadFrameManyHeaders(b *testing.B) {
	var hdr bytes.Buffer
	for i := 0; i < 25; i++ {
		fmt.Fprintf(&hdr, "X-Custom-Header-%d: some-value-%d\r\n", i, i)
	}
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "m"})
	if err != nil {
		b.Fatal(err)
	}
	fmt.Fprintf(&hdr, "Content-Length: %d\r\n\r\n", len(payload))
	hdr.Write(payload)
	data := hdr.Bytes()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r := bufio.NewReader(bytes.NewReader(data))
		if _, err := readFrame(r); err != nil {
			b.Fatal(err)
		}
	}
}

func lspBenchDrainClient(b *testing.B) *lspClient {
	b.Helper()
	cl := newLSPClient(lspServerDef{Name: "fake", Cmd: []string{"fake"}}, b.TempDir())
	srvR, srvW := io.Pipe()
	cl.in = srvW
	b.Cleanup(func() { srvR.Close(); srvW.Close() })
	go func() {
		br := bufio.NewReader(srvR)
		for {
			if _, err := readFrame(br); err != nil {
				return
			}
		}
	}()
	cl.beginWriteLoop()
	return cl
}

func BenchmarkLSPNotify(b *testing.B) {
	cl := lspBenchDrainClient(b)
	params := map[string]any{
		"textDocument": map[string]any{"uri": "file:///f.go", "version": 1},
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cl.notify("textDocument/didChange", params); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLSPNotifyParallel(b *testing.B) {
	cl := lspBenchDrainClient(b)
	params := map[string]any{
		"textDocument": map[string]any{"uri": "file:///f.go", "version": 1},
	}
	var failed atomic.Bool
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if cl.notify("textDocument/didChange", params) != nil {
				failed.Store(true)
				return
			}
		}
	})
	if failed.Load() {
		b.Fatal("notify failed under parallel load")
	}
}

func BenchmarkLSPEnsureOpen(b *testing.B) {
	cl, _ := lspRaceWireClient(b)
	dir := b.TempDir()
	content := []byte("package main\n\nfunc main() {}\n")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		path := filepath.Join(dir, fmt.Sprintf("f%d.go", i))
		if err := os.WriteFile(path, content, 0644); err != nil {
			b.Fatal(err)
		}
		if err := cl.ensureOpen(path, "f.go"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLSPEnsureOpenAlreadyOpened(b *testing.B) {
	cl, _ := lspRaceWireClient(b)
	path := filepath.Join(b.TempDir(), "f.go")
	content := []byte(strings.Repeat("package main\nfunc example() {}\n", 1024)) // ~32KB file
	if err := os.WriteFile(path, content, 0o644); err != nil {
		b.Fatal(err)
	}
	if err := cl.ensureOpen(path, "f.go"); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := cl.ensureOpen(path, "f.go"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLSPSyncDocChanged(b *testing.B) {
	cl, _ := lspRaceWireClient(b)
	path := filepath.Join(b.TempDir(), "f.go")
	content := []byte(strings.Repeat("x", 100*1024))
	if err := os.WriteFile(path, []byte("package main\n"), 0644); err != nil {
		b.Fatal(err)
	}
	if err := cl.ensureOpen(path, "f.go"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 8; j++ {
			content[len(content)-1-j] = byte('0' + (i+j)%10)
		}
		if err := os.WriteFile(path, content, 0644); err != nil {
			b.Fatal(err)
		}
		if err := cl.syncDoc(path, "f.go"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLSPSyncDocUnchanged(b *testing.B) {
	cl, _ := lspRaceWireClient(b)
	path := filepath.Join(b.TempDir(), "f.go")
	content := []byte(strings.Repeat("x", 100*1024))
	if err := os.WriteFile(path, content, 0644); err != nil {
		b.Fatal(err)
	}
	if err := cl.ensureOpen(path, "f.go"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := cl.syncDoc(path, "f.go"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLSPSyncDocIncremental(b *testing.B) {
	cl, _ := lspRaceWireClient(b)
	cl.syncIncremental = true
	path := filepath.Join(b.TempDir(), "f.go")
	content := []byte(strings.Repeat("x", 100*1024))
	if err := os.WriteFile(path, []byte("package main\n"), 0644); err != nil {
		b.Fatal(err)
	}
	if err := cl.ensureOpen(path, "f.go"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for j := 0; j < 8; j++ {
			content[len(content)-1-j] = byte('0' + (i+j)%10)
		}
		if err := os.WriteFile(path, content, 0644); err != nil {
			b.Fatal(err)
		}
		if err := cl.syncDoc(path, "f.go"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLSPIncrementalChangeScale(b *testing.B) {
	const line = "func example(arg int) int { return arg * 2 } // padding to make the lines a realistic length....\n"
	const changedLine = "func example(arg int) int { return arg * 3 } // padding to make the lines a realistic length....\n"
	cl := &lspClient{encoding: "utf-16"}
	for _, sizeKB := range []int{100, 1024, 10 * 1024} {
		nLines := sizeKB * 1024 / len(line)
		lines := make([]string, nLines)
		for i := range lines {
			lines[i] = line
		}
		oldDoc := strings.Join(lines, "")
		edited := func(lineIdx int) string {
			cp := make([]string, nLines)
			copy(cp, lines)
			cp[lineIdx] = changedLine
			return strings.Join(cp, "")
		}
		cases := map[string]string{
			"edit-begin":  edited(0),
			"edit-middle": edited(nLines / 2),
			"edit-end":    edited(nLines - 1),
			"rewrite":     strings.Repeat("x", len(oldDoc)),
		}
		for _, name := range []string{"edit-begin", "edit-middle", "edit-end", "rewrite"} {
			newDoc := cases[name]
			b.Run(fmt.Sprintf("%dKB/%s", sizeKB, name), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(oldDoc)))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					changes, ok := incrementalChange(cl, oldDoc, newDoc)
					if !ok || len(changes) != 1 {
						b.Fatalf("incrementalChange failed for %dKB/%s", sizeKB, name)
					}
				}
			})
		}
	}
}

func BenchmarkLSPBinDirs(b *testing.B) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = lspBinDirs()
	}
}

func BenchmarkLSPServerStatusBinDirs(b *testing.B) {
	m := newLSPManager(b.TempDir(), false)
	def := &lspServerDef{Name: "bench", Cmd: []string{"go"}, Exts: []string{".go"}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.ServerStatus(def)
	}
}

func BenchmarkUTF16ToByte(b *testing.B) {
	line := "func handleRequest(w http.ResponseWriter, r *http.Request) { // 🎉🚀✨ status check }"
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = utf16ToByte(line, 68)
	}
}
