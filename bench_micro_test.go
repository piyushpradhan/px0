package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"px0/internal/fuzzy"
	"px0/internal/ignore"
	"px0/internal/render"
)

func generateFileEntries(n int) []FileEntry {
	dirs := []string{"src", "pkg", "internal", "cmd", "pkg/util", "web/src/components", "api/v1", "services"}
	names := []string{"server.go", "client.go", "handler.go", "types.go", "utils.go", "index.ts", "app.py", "main.rs"}
	entries := make([]FileEntry, n)
	for i := 0; i < n; i++ {
		d := dirs[i%len(dirs)]
		name := fmt.Sprintf("%d_%s", i, names[i%len(names)])
		p := fmt.Sprintf("%s/%s", d, name)
		entries[i] = FileEntry{
			Path:      p,
			Name:      name,
			Lower:     strings.ToLower(p),
			NameStart: len(p) - len(name),
		}
	}
	return entries
}

func BenchmarkFuzzyFind_1K(b *testing.B) {
	files := generateFileEntries(1000)
	query := "handler"
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = fuzzy.Find(files, query, 50)
	}
}

func BenchmarkFuzzyFind_10K(b *testing.B) {
	files := generateFileEntries(10000)
	query := "handler"
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = fuzzy.Find(files, query, 50)
	}
}

func BenchmarkFuzzyFind_50K(b *testing.B) {
	files := generateFileEntries(50000)
	query := "srvutil"
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = fuzzy.Find(files, query, 50)
	}
}

func BenchmarkFuzzyScore(b *testing.B) {
	e := &FileEntry{
		Path:      "internal/server/http_server.go",
		Name:      "http_server.go",
		Lower:     "internal/server/http_server.go",
		NameStart: 16,
	}
	q := "httpserver"
	origQ := "httpserver"
	pos := make([]int, 0, len(e.Path))

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = fuzzy.Score(q, origQ, e.Path, e.Lower, e.NameStart, pos)
	}
}

func BenchmarkIgnoreSetMatch(b *testing.B) {
	patterns := []string{
		"*.tmp",
		"build/",
		"dist/",
		"node_modules/",
		".git/",
		"docs/**/draft.md",
		"!build/keep.txt",
		"vendor/",
	}
	ig := ignore.New(patterns)
	testPaths := []string{
		"src/server/handler.go",
		"build/output.js",
		"node_modules/react/index.js",
		"docs/internal/draft.md",
		"pkg/vendor.go",
		"tmp/test.tmp",
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		p := testPaths[i%len(testPaths)]
		_ = ig.Match(p, false)
	}
}

func BenchmarkSnip(b *testing.B) {
	rawLine := []byte("    func handleSearchRequest(w http.ResponseWriter, r *http.Request, query string) (SearchResult, error) {")
	from := 9
	to := 28

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = snip(rawLine, from, to)
	}
}

func BenchmarkHighlightLines_Go(b *testing.B) {
	src := `package main

import (
	"fmt"
	"sync"
	"time"
)

type WorkerPool struct {
	tasks chan func()
	wg    sync.WaitGroup
}

func NewWorkerPool(workers int) *WorkerPool {
	p := &WorkerPool{tasks: make(chan func(), 128)}
	for i := 0; i < workers; i++ {
		p.wg.Add(1)
		go func() {
			defer p.wg.Done()
			for t := range p.tasks {
				t()
			}
		}()
	}
	return p
}
`
	d := render.NewDoc(src, "main.go")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = d.Lines(0, d.Total)
	}
}

func BenchmarkHighlightLines_Python(b *testing.B) {
	src := `import os
import sys
from dataclasses import dataclass

@dataclass
class Config:
    host: str = "127.0.0.1"
    port: int = 8080
    debug: bool = False

def run_server(cfg: Config):
    print(f"Starting server on {cfg.host}:{cfg.port}")
    if cfg.debug:
        print("Debug mode enabled")
`
	d := render.NewDoc(src, "app.py")
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = d.Lines(0, d.Total)
	}
}

func BenchmarkDocCreation(b *testing.B) {
	src := strings.Repeat("func exampleLine() { return 42 }\n", 200)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = render.NewDoc(src, "example.go")
	}
}

func BenchmarkSearchLiteralInMemory(b *testing.B) {
	root := b.TempDir()
	content := "package main\n\n// TargetSearchToken is here\nfunc TargetSearchToken() string { return \"ok\" }\n"
	for i := 0; i < 20; i++ {
		_ = os.WriteFile(filepath.Join(root, fmt.Sprintf("file_%d.go", i)), []byte(content), 0o644)
	}
	ix := NewIndex(root)
	ix.Build()

	opts := SearchOpts{
		Query:    "TargetSearchToken",
		MaxFiles: 50,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = SearchContext(context.Background(), ix, opts)
	}
}

func BenchmarkSearchDefWord(b *testing.B) {
	root := b.TempDir()
	missContent := strings.Repeat("package main\nfunc helperFunc(x int) int { return x + 1 }\n", 50)
	for i := 0; i < 40; i++ {
		_ = os.WriteFile(filepath.Join(root, fmt.Sprintf("miss_%d.go", i)), []byte(missContent), 0o644)
	}
	hitContent := "package main\nfunc TargetSymbolDef() string { return \"ok\" }\n"
	_ = os.WriteFile(filepath.Join(root, "hit.go"), []byte(hitContent), 0o644)

	ix := NewIndex(root)
	ix.Build()

	opts := SearchOpts{
		Query:        "TargetSymbolDef",
		Word:         true,
		Case:         true,
		classifyDefs: true,
		MaxFiles:     50,
	}

	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _, _ = SearchContext(context.Background(), ix, opts)
	}
}

func BenchmarkFuzzyFindShortQuery_50K(b *testing.B) {
	files := generateFileEntries(50000)
	query := "in"
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = fuzzy.Find(files, query, 50)
	}
}

func BenchmarkColdHighlight_Go(b *testing.B) {
	src := strings.Repeat("func exampleLine(x int) int { return x * 2 }\n", 200)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		d := render.NewDoc(src, "example.go")
		_, _ = d.Lines(0, 300)
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

func BenchmarkSortNodes(b *testing.B) {
	base := make([]Node, 64)
	for i := range base {
		base[i] = Node{
			Name: fmt.Sprintf("File_%02d_Component.go", 64-i),
			Path: fmt.Sprintf("pkg/File_%02d_Component.go", 64-i),
			Dir:  i%5 == 0,
		}
	}
	work := make([]Node, len(base))
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		copy(work, base)
		sortNodes(work)
	}
}

func BenchmarkHandleFileScrollChunk(b *testing.B) {
	wd, err := os.Getwd()
	if err != nil {
		b.Fatal(err)
	}
	ix := NewIndex(wd)
	ix.Build()
	srv := &Server{ix: ix, lsp: newLSPManager(wd, false), diffBase: "HEAD"}
	req := httptest.NewRequest(http.MethodGet, "/api/file?path=server.go&start=300&count=300", nil)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		srv.handleFile(w, req)
	}
}

