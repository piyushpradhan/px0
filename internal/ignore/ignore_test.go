package ignore

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIgnorePatterns(t *testing.T) {
	t.Parallel()
	ig := New([]string{"build/", "*.tmp", "!keep.tmp", "docs/**/draft.md", "/only-root.txt"})
	cases := []struct {
		path string
		dir  bool
		want bool
	}{
		{".git", true, true},
		{"node_modules", true, true},
		{"src/main.go", false, false},
		{"build", true, true},
		{"build/out.js", false, true},
		{"a/b/c.tmp", false, true},
		{"a/b/keep.tmp", false, false},
		{"docs/x/y/draft.md", false, true},
		{"docs/draft.md", false, true},
		{"only-root.txt", false, true},
		{"sub/only-root.txt", false, false},
		{"vendor", true, true},
		{"src/vendor.go", false, false},
	}
	for _, c := range cases {
		if got := ig.Match(c.path, c.dir); got != c.want {
			t.Errorf("match(%q, dir=%v) = %v, want %v", c.path, c.dir, got, c.want)
		}
	}
}

// The segment fast paths must be indistinguishable from the regexp they
// replace. This compares the two on every combination of a pattern set and a
// path set, so a shortcut that is merely "close enough" fails here.
func TestIgnoreFastPathMatchesRegex(t *testing.T) {
	t.Parallel()
	patterns := []string{
		"node_modules", "dist", ".git", "build/", "vendor/", "target",
		"*.pyc", "*.log", "*.min.js", "*", "*.", ".DS_Store",
		"a?c", "**", "x**y", "sub/dir", "/rooted.txt", "docs/**/draft.md",
		"weird name", "dash-name", "dot.name.ext", "UPPER",
		// anchored shapes, which now take a literal-prefix reject or an exact path
		"tests/baselines", "tests/baselines/reference", "src/*.ts", "src/**/gen.js",
		"/built/local", "a/b/c/d", "tests/cases/*/x?.ts", "pkg/*/vendor",
		// "**/" shapes, which have no literal head and rely on the run filter
		"**/.vscode/*", "**/node_modules/**", "**/*.generated.ts", "**/a/b",
		"x/**/y", "*/mid/*", "**/",
		"test*file", "sub/*.txt", "a/b/c",
	}
	paths := []string{
		"", "a", "node_modules", "node_modules/x.js", "a/node_modules",
		"a/node_modules/b/c.js", "src/dist", "src/dist/app.js", "dist",
		"build", "build/out.js", "a/build/b/c", "vendor", "vendor/x",
		"x.pyc", "a/b/x.pyc", "pyc", ".pyc", "a.min.js", "a/b.min.js",
		"abc", "a/abc", "axc", "sub/dir", "sub/dir/f", "a/sub/dir",
		"rooted.txt", "a/rooted.txt", "docs/x/draft.md", "docs/draft.md",
		"weird name", "weird name/f", "dash-name", "dot.name.ext",
		"UPPER", "upper", "target", "a/target/b", ".DS_Store", "a/.DS_Store",
		"x**y", "xzzy", "a/b/c/d/e/f/g",
		"tests", "tests/baselines", "tests/baselines/reference", "tests/baselines/reference/x.js",
		"tests/baselinesX", "src/a.ts", "src/a/b.ts", "src/x/gen.js", "src/gen.js",
		"built/local", "built/local/tsc.js", "builtX/local", "a/b/c/d", "a/b/c/d/e",
		"tests/cases/foo/x1.ts", "tests/cases/foo/x12.ts", "pkg/a/vendor", "pkg/vendor",
		".vscode", ".vscode/settings.json", "a/.vscode/x.json", "a/b/.vscode",
		"node_modules/x", "a/node_modules/b/c", "x.generated.ts", "a/b/x.generated.ts",
		"a/b", "q/a/b", "x/y", "x/m/y", "x/m/n/y", "p/mid/q", "mid/q", "p/mid",
		"test123file", "sub/test.txt", "other/sub/test.txt", "unrelated.go",
	}

	for _, pat := range patterns {
		for _, dirOnlySuffix := range []string{"", "/"} {
			full := pat + dirOnlySuffix
			r, ok := CompilePattern(full)
			if !ok {
				continue
			}
			// Rebuild the same pattern forced onto the regexp path.
			slow, ok := compileRegexOnly(full)
			if !ok {
				t.Fatalf("%q: regex form failed to compile", full)
			}
			if r.Kind != RKRegex {
				if slow.Re == nil || slow.Sub == nil {
					t.Fatalf("%q: reference rule has no regexps", full)
				}
			}
			for _, p := range paths {
				for _, isDir := range []bool{false, true} {
					got := r.Hit(p, isDir)
					want := slow.Hit(p, isDir)
					if got != want {
						t.Errorf("pattern %q path %q dir=%v: fast=%v regex=%v (kind=%d lit=%q)",
							full, p, isDir, got, want, r.Kind, r.Lit)
					}
				}
			}
		}
	}
}

// compileRegexOnly builds the same rule with the segment shortcuts disabled.
func compileRegexOnly(p string) (Rule, bool) {
	r, ok := CompilePattern(p)
	if !ok {
		return r, false
	}
	if r.Kind == RKRegex {
		return r, true
	}
	forced, ok := compile(p, false)
	if !ok {
		return r, false
	}
	return forced, true
}

func TestIgnoreSetBasics(t *testing.T) {
	t.Parallel()
	set := New([]string{"custom_ignored/", "*.tmp"})
	if !set.Match("node_modules/foo.js", false) {
		t.Error("expected node_modules to be ignored")
	}
	if !set.Match("custom_ignored", true) {
		t.Error("expected custom_ignored dir to be ignored")
	}
	if !set.Match("a/b/c.tmp", false) {
		t.Error("expected *.tmp to be ignored")
	}
	if set.Match("main.go", false) {
		t.Error("main.go should not be ignored")
	}

	// Test Child empty and with patterns
	sameChild := set.Child(nil)
	if sameChild != set {
		t.Error("expected same set when patterns empty")
	}
	subChild := set.Child([]string{"*.subtmp"})
	if !subChild.Match("test.subtmp", false) {
		t.Error("expected subChild to match *.subtmp")
	}

	// Test literalHead fallback
	if got := literalHead("foo/bar"); got != "foo/bar" {
		t.Errorf("expected 'foo/bar', got %q", got)
	}

	// Test CompilePattern edge cases
	if _, ok := CompilePattern(""); ok {
		t.Error("expected empty pattern to not compile")
	}
	if _, ok := CompilePattern("   "); ok {
		t.Error("expected whitespace pattern to not compile")
	}
	if _, ok := CompilePattern("# a comment"); ok {
		t.Error("expected comment pattern to not compile")
	}

	// Test ReadGitignore nonexistent
	if res := ReadGitignore("/nonexistent-path-px0", ""); res != nil {
		t.Errorf("expected nil for nonexistent gitignore, got %v", res)
	}

	// Test ReadGitignore with valid file
	tempDir := t.TempDir()
	content := []byte("\n# comment\nfoo\n!bar\n/baz\n!/qux\n")
	if err := os.WriteFile(filepath.Join(tempDir, ".gitignore"), content, 0644); err != nil {
		t.Fatal(err)
	}
	rootPats := ReadGitignore(tempDir, "")
	if len(rootPats) != 4 || rootPats[0] != "foo" || rootPats[1] != "!bar" || rootPats[2] != "/baz" || rootPats[3] != "!/qux" {
		t.Errorf("unexpected root patterns: %v", rootPats)
	}
	scopedPats := ReadGitignore(tempDir, "sub/dir")
	if len(scopedPats) != 4 || scopedPats[0] != "sub/dir/foo" || scopedPats[1] != "!sub/dir/bar" || scopedPats[2] != "sub/dir/baz" || scopedPats[3] != "!sub/dir/qux" {
		t.Errorf("unexpected scoped patterns: %v", scopedPats)
	}
}

func BenchmarkIgnoreMatch(b *testing.B) {
	ig := New([]string{"build/", "*.tmp", "!keep.tmp", "docs/**/draft.md", "/only-root.txt", "node_modules/", "vendor/"})
	paths := []string{
		"src/main.go",
		"build/out.js",
		"a/b/keep.tmp",
		"docs/x/y/draft.md",
		"sub/only-root.txt",
		"vendor/pkg/foo.go",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := paths[i%len(paths)]
		_ = ig.Match(p, false)
	}
}

func BenchmarkCompilePattern(b *testing.B) {
	patterns := []string{"build/", "*.tmp", "docs/**/draft.md", "/only-root.txt", "node_modules/"}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p := patterns[i%len(patterns)]
		_, _ = CompilePattern(p)
	}
}

