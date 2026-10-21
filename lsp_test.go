package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
