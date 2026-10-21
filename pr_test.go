package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"px0/internal/settings"
)

func TestDetectPRURL(t *testing.T) {
	cases := []struct {
		arg         string
		wantOK      bool
		owner, repo string
		num         int
	}{
		{arg: "https://github.com/px0-ai/px0/pull/42", wantOK: true, owner: "px0-ai", repo: "px0", num: 42},
		{arg: "http://github.com/px0-ai/px0/pull/7", wantOK: true, owner: "px0-ai", repo: "px0", num: 7},
		{arg: "github.com/px0-ai/px0/pull/1", wantOK: true, owner: "px0-ai", repo: "px0", num: 1},
		{arg: "https://github.com/px0-ai/px0.git/pull/99", wantOK: true, owner: "px0-ai", repo: "px0", num: 99},
		// Bare numbers must NOT be accepted as PR targets
		{arg: "42", wantOK: false},
		{arg: "123", wantOK: false},
		// File paths must NOT be accepted
		{arg: "src/main.go", wantOK: false},
		{arg: ".", wantOK: false},
		// Non-PR GitHub URLs must NOT be accepted
		{arg: "https://github.com/px0-ai/px0", wantOK: false},
		{arg: "https://github.com/px0-ai/px0/issues/42", wantOK: false},
		// Invalid / empty
		{arg: "not a url", wantOK: false},
		{arg: "", wantOK: false},
	}
	for _, c := range cases {
		p, target, ok := DetectPRURL(c.arg)
		if ok != c.wantOK {
			t.Errorf("DetectPRURL(%q) ok = %v, want %v", c.arg, ok, c.wantOK)
			continue
		}
		if !ok {
			continue
		}
		if p.Name() != "github" {
			t.Errorf("DetectPRURL(%q) provider = %q, want github", c.arg, p.Name())
		}
		if target.Owner != c.owner || target.Repo != c.repo || target.Number != c.num {
			t.Errorf("DetectPRURL(%q) = (%q, %q, %d), want (%q, %q, %d)", c.arg, target.Owner, target.Repo, target.Number, c.owner, c.repo, c.num)
		}
	}
}

func TestParsePRURLUnsupported(t *testing.T) {
	if _, _, ok := DetectPRURL("42"); ok {
		t.Error("expected false parsing bare number as PR URL")
	}
	if _, _, ok := DetectPRURL("https://example.com/foo/bar"); ok {
		t.Error("expected false for unsupported forge URL")
	}
}

func TestResolveGitHubTokenPrecedence(t *testing.T) {
	// settings.json wins over everything, including the environment.
	t.Setenv("GITHUB_TOKEN", "env-token")
	settingsToken := "settings-token"
	token, source := resolveGitHubToken(settings.Settings{GitHubToken: &settingsToken})
	if token != "settings-token" || source != "settings" {
		t.Errorf("got (%q, %q), want (settings-token, settings)", token, source)
	}

	// With no settings token, the environment variable wins.
	token, source = resolveGitHubToken(settings.Settings{})
	if token != "env-token" || source != "env" {
		t.Errorf("got (%q, %q), want (env-token, env)", token, source)
	}
}

func TestResolveGitHubTokenNoneAvailable(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	// Force `gh` to be unresolvable so the outcome is deterministic
	// regardless of whether the test machine happens to have it installed.
	empty := t.TempDir()
	t.Setenv("PATH", empty)

	token, source := resolveGitHubToken(settings.Settings{})
	if token != "" || source != "" {
		t.Errorf("got (%q, %q), want (\"\", \"\")", token, source)
	}
}

func TestResolveGitHubTokenBlankSettingsFallsThrough(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "env-token")
	blank := "   "
	token, source := resolveGitHubToken(settings.Settings{GitHubToken: &blank})
	if token != "env-token" || source != "env" {
		t.Errorf("a blank settings token must fall through to the env var; got (%q, %q)", token, source)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestSubmitReviewPayload(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()

	var capturedBody []byte
	var capturedPath string
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		capturedPath = req.URL.Path
		capturedBody, _ = io.ReadAll(req.Body)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("{}")),
			Header:     make(http.Header),
		}, nil
	})

	ctx := context.Background()
	comments := []prComment{
		{ID: 1, Path: "main.go", Line: 10, Side: "RIGHT", Body: "looks good"},
		{ID: 2, Path: "old.go", Line: 5, Side: "LEFT", Body: "deleted line comment"},
	}
	err := submitReview(ctx, "px0-ai", "px0", 42, "dummy-token", "abc123sha", comments, "APPROVE", "Overall LGTM")
	if err != nil {
		t.Fatalf("submitReview failed: %v", err)
	}
	if capturedPath != "/repos/px0-ai/px0/pulls/42/reviews" {
		t.Errorf("unexpected path: %q", capturedPath)
	}
	var payload struct {
		CommitID string `json:"commit_id"`
		Body     string `json:"body"`
		Event    string `json:"event"`
		Comments []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Side string `json:"side"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(capturedBody, &payload); err != nil {
		t.Fatalf("failed to parse captured payload: %v", err)
	}
	if payload.CommitID != "abc123sha" {
		t.Errorf("payload.CommitID = %q, want abc123sha", payload.CommitID)
	}
	if payload.Event != "APPROVE" || payload.Body != "Overall LGTM" {
		t.Errorf("unexpected event or body: %+v", payload)
	}
	if len(payload.Comments) != 2 || payload.Comments[1].Side != "LEFT" {
		t.Errorf("unexpected comments in payload: %+v", payload.Comments)
	}
}

func TestPRSessionCloseRefCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.name", "test")
	run("config", "user.email", "test@test.local")
	run("commit", "--allow-empty", "-m", "initial")

	run("update-ref", "refs/px0/pr/99", "HEAD")
	run("update-ref", "refs/px0/base/99", "HEAD")

	tmpWT := t.TempDir()
	p := &prSession{
		srcRepo:  root,
		worktree: tmpWT,
		meta:     PRMeta{Number: 99},
	}
	p.Close()

	checkRef := exec.Command("git", "-C", root, "rev-parse", "--verify", "refs/px0/pr/99")
	if err := checkRef.Run(); err == nil {
		t.Errorf("refs/px0/pr/99 was not deleted on Close")
	}
	checkBase := exec.Command("git", "-C", root, "rev-parse", "--verify", "refs/px0/base/99")
	if err := checkBase.Run(); err == nil {
		t.Errorf("refs/px0/base/99 was not deleted on Close")
	}
}

// TestPRSessionPullFastForwardAndDiverge exercises prSession.Pull against a
// fake "upstream" (a bare repo standing in for GitHub) with a srcRepo/
// worktree pair set up exactly like checkoutPR's worktree case. A clean
// fast-forward onto a new PR commit must succeed and update meta.HeadSHA; a
// local commit in the worktree that then diverges from a further PR push
// must be refused with errPRDiverged, leaving the worktree untouched.
func TestPRSessionPullFastForwardAndDiverge(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	upstream := filepath.Join(base, "upstream.git")
	srcRepo := filepath.Join(base, "src")

	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, upstream, "init", "--bare", "-b", "main")

	gitTestRun(t, base, "clone", upstream, "src")
	for _, cfg := range [][2]string{{"user.email", "t@example.com"}, {"user.name", "T"}, {"commit.gpgsign", "false"}} {
		gitTestRun(t, srcRepo, "config", cfg[0], cfg[1])
	}
	if err := os.WriteFile(filepath.Join(srcRepo, "base.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, srcRepo, "add", "base.txt")
	gitTestRun(t, srcRepo, "commit", "-qm", "base commit")
	gitTestRun(t, srcRepo, "push", "origin", "main")

	// A "PR branch" pushed to upstream as refs/pull/99/head, the way GitHub does.
	gitTestRun(t, srcRepo, "checkout", "-qb", "feature")
	if err := os.WriteFile(filepath.Join(srcRepo, "feature.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, srcRepo, "add", "feature.txt")
	gitTestRun(t, srcRepo, "commit", "-qm", "pr commit 1")
	gitTestRun(t, srcRepo, "push", "origin", "feature:refs/pull/99/head")
	gitTestRun(t, srcRepo, "checkout", "-q", "main")

	// Check out the PR into a worktree, same as checkoutPR does.
	gitTestRun(t, srcRepo, "fetch", "--no-tags", "origin", "refs/pull/99/head:refs/px0/pr/99")
	worktree := filepath.Join(base, "wt")
	gitTestRun(t, srcRepo, "worktree", "add", "--detach", worktree, "refs/px0/pr/99")

	p := &prSession{
		worktree: worktree,
		srcRepo:  srcRepo,
		target:   PRTarget{Owner: "o", Repo: "r"},
		meta:     PRMeta{Number: 99, BaseRef: "main", HeadRef: "feature"},
	}

	// Someone pushes a second commit to the PR head -- Pull should fast-forward cleanly.
	gitTestRun(t, srcRepo, "checkout", "-q", "feature")
	if err := os.WriteFile(filepath.Join(srcRepo, "feature.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, srcRepo, "commit", "-aqm", "pr commit 2")
	gitTestRun(t, srcRepo, "push", "origin", "feature:refs/pull/99/head")
	gitTestRun(t, srcRepo, "checkout", "-q", "main")

	info, err := p.Pull()
	if err != nil {
		t.Fatalf("expected a clean fast-forward Pull, got %v", err)
	}
	if info == "" {
		t.Error("expected a non-empty info message")
	}
	if got, err := os.ReadFile(filepath.Join(worktree, "feature.txt")); err != nil || string(got) != "two\n" {
		t.Fatalf("expected worktree to fast-forward to %q, got %q, err=%v", "two\n", got, err)
	}
	if p.meta.HeadSHA == "" {
		t.Error("expected Pull to record the new HeadSHA")
	}

	// A local commit in the worktree (as if the reviewer committed a fix)
	// that then diverges from a further PR push must be refused, not merged.
	if err := os.WriteFile(filepath.Join(worktree, "feature.txt"), []byte("local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, worktree, "commit", "-aqm", "reviewer's local commit")

	gitTestRun(t, srcRepo, "checkout", "-q", "feature")
	if err := os.WriteFile(filepath.Join(srcRepo, "feature.txt"), []byte("three\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, srcRepo, "commit", "-aqm", "pr commit 3")
	gitTestRun(t, srcRepo, "push", "origin", "feature:refs/pull/99/head")
	gitTestRun(t, srcRepo, "checkout", "-q", "main")

	if _, err := p.Pull(); !errors.Is(err, errPRDiverged) {
		t.Fatalf("expected errPRDiverged, got %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(worktree, "feature.txt")); err != nil || string(got) != "local edit\n" {
		t.Fatalf("expected worktree untouched by the refused pull, got %q, err=%v", got, err)
	}
}

// TestPRSessionPush confirms Push sends the worktree's HEAD to the PR's
// actual head branch (meta.HeadRepoCloneURL/HeadRef), not wherever the
// worktree happens to be checked out from.
func TestPRSessionPush(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	upstream := filepath.Join(base, "upstream.git")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, upstream, "init", "--bare", "-b", "main")

	worktree := filepath.Join(base, "wt")
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, worktree, "init", "-q", "-b", "feature")
	for _, cfg := range [][2]string{{"user.email", "t@example.com"}, {"user.name", "T"}, {"commit.gpgsign", "false"}} {
		gitTestRun(t, worktree, "config", cfg[0], cfg[1])
	}
	if err := os.WriteFile(filepath.Join(worktree, "f.txt"), []byte("pushed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitTestRun(t, worktree, "add", "f.txt")
	gitTestRun(t, worktree, "commit", "-qm", "reviewer commit")

	p := &prSession{
		worktree: worktree,
		target:   PRTarget{Owner: "o", Repo: "r"},
		meta:     PRMeta{Number: 1, HeadRef: "feature", HeadRepoCloneURL: upstream},
	}
	if err := p.Push(false); err != nil {
		t.Fatalf("Push failed: %v", err)
	}

	out := gitTestRun(t, upstream, "log", "--oneline", "-1", "refs/heads/feature")
	if !strings.Contains(out, "reviewer commit") {
		t.Fatalf("expected upstream's refs/heads/feature to carry the pushed commit, got %q", out)
	}
	pushed := strings.TrimSpace(gitTestRun(t, worktree, "rev-parse", "HEAD"))
	if p.meta.HeadSHA != pushed {
		t.Fatalf("Push should move the PR head (meta.HeadSHA) to %s, got %q", pushed, p.meta.HeadSHA)
	}
	if p.remoteHead() != pushed {
		t.Fatalf("remoteHead = %q, want %s", p.remoteHead(), pushed)
	}
}

// TestPRAheadCountsFromPRHead covers the detached-HEAD case: a PR checkout has
// no upstream, so commits made in the IDE must still count as unpushed
// (measured from the PR head), and stop counting once that head moves.
func TestPRAheadCountsFromPRHead(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	gitTestRun(t, root, "init", "-q", "-b", "main")
	for _, cfg := range [][2]string{{"user.email", "t@example.com"}, {"user.name", "T"}, {"commit.gpgsign", "false"}} {
		gitTestRun(t, root, "config", cfg[0], cfg[1])
	}
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("1\n"), 0o644)
	gitTestRun(t, root, "add", ".")
	gitTestRun(t, root, "commit", "-qm", "pr commit")
	head := strings.TrimSpace(gitTestRun(t, root, "rev-parse", "HEAD"))
	gitTestRun(t, root, "checkout", "-q", "--detach")

	if n := gitCountSince(root, head); n != 0 {
		t.Fatalf("fresh checkout: ahead = %d, want 0", n)
	}
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("2\n"), 0o644)
	gitTestRun(t, root, "commit", "-qam", "my change")
	if n := gitCountSince(root, head); n != 1 {
		t.Fatalf("after IDE commit: ahead = %d, want 1", n)
	}
	cs := gitCommitsSince(root, head, 0)
	if len(cs) != 1 || cs[0].Subject != "my change" {
		t.Fatalf("unpushed = %+v", cs)
	}
	head = strings.TrimSpace(gitTestRun(t, root, "rev-parse", "HEAD"))
	if n := gitCountSince(root, head); n != 0 {
		t.Fatalf("after push moved the head: ahead = %d, want 0", n)
	}
}

func TestFetchPRMetaMerged(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()

	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body := `{
			"number": 55,
			"title": "Fix login race condition",
			"state": "closed",
			"merged": true,
			"merged_at": "2026-09-20T10:00:00Z",
			"user": {"login": "alice"},
			"base": {"ref": "main"},
			"head": {
				"ref": "fix-race",
				"sha": "fedcba987654",
				"repo": {"clone_url": "https://github.com/alice/px0.git", "full_name": "alice/px0"}
			}
		}`
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})

	meta, err := fetchPRMeta(context.Background(), "px0-ai", "px0", 55, "")
	if err != nil {
		t.Fatalf("fetchPRMeta failed: %v", err)
	}
	if !meta.Merged {
		t.Errorf("meta.Merged = false, want true")
	}
	if meta.State != "closed" {
		t.Errorf("meta.State = %q, want closed", meta.State)
	}
	if meta.MergedAt != "2026-09-20T10:00:00Z" {
		t.Errorf("meta.MergedAt = %q, want timestamp", meta.MergedAt)
	}
}

func TestCheckoutPRMergedAlwaysProceeds(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()

	localRepo := t.TempDir()
	gitTestRun(t, localRepo, "init")
	gitTestRun(t, localRepo, "config", "user.name", "Tester")
	gitTestRun(t, localRepo, "config", "user.email", "test@example.com")
	gitTestRun(t, localRepo, "checkout", "-b", "feature-x")
	os.WriteFile(filepath.Join(localRepo, "f.txt"), []byte("hi"), 0644)
	gitTestRun(t, localRepo, "add", "f.txt")
	gitTestRun(t, localRepo, "commit", "-m", "init")

	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body := fmt.Sprintf(`{
			"number": 77,
			"title": "Already merged PR",
			"state": "closed",
			"merged": true,
			"merged_at": "2026-09-21T08:00:00Z",
			"user": {"login": "bob"},
			"base": {"ref": "main"},
			"head": {
				"ref": "feature-x",
				"sha": "1234567890ab",
				"repo": {"clone_url": %q, "full_name": "px0-ai/px0"}
			}
		}`, localRepo)
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	})

	target := PRTarget{Provider: "github", Owner: "px0-ai", Repo: "px0", Number: 77}
	_, err := checkoutPR(context.Background(), &GitHubProvider{}, target, t.TempDir(), nil)
	// Must not be ErrPRMergedCancelled; merged PRs are always opened without blocking.
	if errors.Is(err, ErrPRMergedCancelled) {
		t.Errorf("err = %v, did not want ErrPRMergedCancelled", err)
	}
}

func TestGitHubProviderInterface(t *testing.T) {
	var gp GitProvider = &GitHubProvider{}
	if gp.Name() != "github" {
		t.Errorf("gp.Name() = %q, want github", gp.Name())
	}
	if !gp.MatchURL("https://github.com/foo/bar/pull/12") {
		t.Error("MatchURL should be true for valid github PR URL")
	}
	if gp.MatchURL("https://gitlab.com/foo/bar/-/merge_requests/12") {
		t.Error("MatchURL should be false for gitlab URL")
	}
	tgt, err := gp.ParseURL("https://github.com/foo/bar.git/pull/99")
	if err != nil {
		t.Fatalf("ParseURL failed: %v", err)
	}
	if tgt.Owner != "foo" || tgt.Repo != "bar" || tgt.Number != 99 || tgt.Provider != "github" {
		t.Errorf("unexpected target: %+v", tgt)
	}
}

// TestGitFilesBetween is the PR's own file set: what changed between the
// merge-base and the PR head, never what the reviewer committed afterwards.
func TestGitFilesBetween(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	gitTestRun(t, root, "init", "-q", "-b", "main")
	for _, cfg := range [][2]string{{"user.email", "t@example.com"}, {"user.name", "T"}, {"commit.gpgsign", "false"}} {
		gitTestRun(t, root, "config", cfg[0], cfg[1])
	}
	write := func(name string) { os.WriteFile(filepath.Join(root, name), []byte(name+"\n"), 0o644) }
	write("base.txt")
	gitTestRun(t, root, "add", ".")
	gitTestRun(t, root, "commit", "-qm", "base")
	base := strings.TrimSpace(gitTestRun(t, root, "rev-parse", "HEAD"))
	write("pr.txt")
	gitTestRun(t, root, "add", ".")
	gitTestRun(t, root, "commit", "-qm", "pr")
	head := strings.TrimSpace(gitTestRun(t, root, "rev-parse", "HEAD"))
	write("mine.txt")
	gitTestRun(t, root, "add", ".")
	gitTestRun(t, root, "commit", "-qm", "mine")

	got := gitFilesBetween(root, base, head)
	if len(got) != 1 || got["pr.txt"] != "A" {
		t.Fatalf("PR files = %v, want map[pr.txt:A]", got)
	}
}

func TestHTTPSRemoteURL(t *testing.T) {
	for in, want := range map[string]string{
		"git@github.com:o/r.git":          "https://github.com/o/r.git",
		"ssh://git@github.com/o/r.git":    "https://github.com/o/r.git",
		"ssh://git@github.com:22/o/r.git": "https://github.com/o/r.git",
		"https://github.com/o/r.git":      "https://github.com/o/r.git",
		"/tmp/some/local/upstream.git":    "/tmp/some/local/upstream.git",
	} {
		if got := httpsRemoteURL(in); got != want {
			t.Errorf("httpsRemoteURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPRSessionPullFollowsForcePush: a PR branch rewritten upstream is not a
// fast-forward, but with nothing of the reviewer's in the checkout Pull follows
// it instead of leaving stale content behind; with local commits it still refuses.
func TestPRSessionPullFollowsForcePush(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	upstream := filepath.Join(base, "upstream.git")
	os.MkdirAll(upstream, 0o755)
	gitTestRun(t, upstream, "init", "--bare", "-b", "feature")

	work := filepath.Join(base, "work")
	os.MkdirAll(work, 0o755)
	gitTestRun(t, work, "init", "-q", "-b", "feature")
	for _, cfg := range [][2]string{{"user.email", "t@example.com"}, {"user.name", "T"}, {"commit.gpgsign", "false"}} {
		gitTestRun(t, work, "config", cfg[0], cfg[1])
	}
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("one\n"), 0o644)
	gitTestRun(t, work, "add", ".")
	gitTestRun(t, work, "commit", "-qm", "one")
	gitTestRun(t, work, "push", "-q", upstream, "feature")

	wt := filepath.Join(base, "wt")
	gitTestRun(t, base, "clone", "-q", upstream, "wt")
	for _, cfg := range [][2]string{{"user.email", "t@example.com"}, {"user.name", "T"}, {"commit.gpgsign", "false"}} {
		gitTestRun(t, wt, "config", cfg[0], cfg[1])
	}
	head := strings.TrimSpace(gitTestRun(t, wt, "rev-parse", "HEAD"))
	p := &prSession{
		worktree: wt,
		target:   PRTarget{Owner: "o", Repo: "r"},
		meta:     PRMeta{Number: 1, BaseRef: "main", HeadRef: "feature", HeadRepoCloneURL: upstream, HeadSHA: head},
	}

	// Rewrite the PR branch: amend and force-push.
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("rewritten\n"), 0o644)
	gitTestRun(t, work, "commit", "-aqm", "one (amended)", "--amend")
	gitTestRun(t, work, "push", "-qf", upstream, "feature")

	if _, err := p.Pull(); err != nil {
		t.Fatalf("Pull should follow a force-push when nothing local is at stake: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(wt, "f.txt")); string(got) != "rewritten\n" {
		t.Fatalf("worktree still has stale content: %q", got)
	}
	if p.meta.HeadSHA == head {
		t.Fatal("HeadSHA should have moved to the rewritten head")
	}

	// With a local commit in the way, the same situation is refused.
	os.WriteFile(filepath.Join(wt, "f.txt"), []byte("mine\n"), 0o644)
	gitTestRun(t, wt, "commit", "-aqm", "mine")
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("rewritten again\n"), 0o644)
	gitTestRun(t, work, "commit", "-aqm", "again", "--amend")
	gitTestRun(t, work, "push", "-qf", upstream, "feature")
	if _, err := p.Pull(); !errors.Is(err, errPRDiverged) {
		t.Fatalf("expected errPRDiverged with a local commit, got %v", err)
	}
}

// connectProvider is a GitLab provider whose API answers are canned: one
// token is accepted and has push access, everything else is rejected.
type connectProvider struct {
	GitLabProvider
	good string
}

func (c *connectProvider) FetchPR(ctx context.Context, t PRTarget, token string) (PRMeta, error) {
	if token != c.good {
		return PRMeta{}, errors.New("gitlab: 401 Unauthorized (token " + token + ")")
	}
	return PRMeta{Number: t.Number}, nil
}

func (c *connectProvider) CheckPushAccess(ctx context.Context, t PRTarget, token string) bool {
	return token == c.good
}

func connectServer(t *testing.T, host string) *Server {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("GITLAB_HOST", "")
	t.Setenv("GITLAB_TOKEN", "")
	t.Setenv("PATH", t.TempDir()) // no glab
	s, _ := newTestServer(t)
	s.pr = &prSession{
		provider: &connectProvider{good: "good-token"},
		target:   PRTarget{Provider: "gitlab", Host: host, Owner: "g", Repo: "r", Number: 4},
		meta:     PRMeta{Number: 4},
	}
	return s
}

func TestForgeConnectAppliesAndSaves(t *testing.T) {
	s := connectServer(t, "gitlab.example.com")
	w := postJSON(t, s, "/api/forge/connect", `{"token":"good-token"}`)
	if w.Code != 200 {
		t.Fatalf("connect: %d %s", w.Code, w.Body)
	}
	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)
	if res["saved"] != true || res["validated"] != true || res["writeAccess"] != true {
		t.Errorf("result = %v", res)
	}
	// Live: the running review uses it now, with no restart.
	if s.pr.token != "good-token" || !s.pr.writeAccess {
		t.Errorf("session token=%q writeAccess=%v", s.pr.token, s.pr.writeAccess)
	}
	// Saved, along with the host it belongs to, so the next session finds it.
	cfg := settings.Read()
	if cfg.GitLabToken == nil || *cfg.GitLabToken != "good-token" || cfg.GitLabHost == nil || *cfg.GitLabHost != "gitlab.example.com" {
		t.Errorf("settings = token %v host %v", cfg.GitLabToken, cfg.GitLabHost)
	}
	if tok, src := (&GitLabProvider{}).ResolveToken(cfg, PRTarget{Host: "gitlab.example.com"}); tok != "good-token" || src != "settings" {
		t.Errorf("resolve after connect = %q/%q", tok, src)
	}
	meta := s.forgeMeta()
	if meta["hasToken"] != true || meta["source"] != "settings" {
		t.Errorf("forgeMeta = %v", meta)
	}
}

func TestForgeConnectRejectsBadToken(t *testing.T) {
	s := connectServer(t, "gitlab.com")
	w := postJSON(t, s, "/api/forge/connect", `{"token":"bad-token"}`)
	if w.Code != 400 {
		t.Fatalf("bad token accepted: %d %s", w.Code, w.Body)
	}
	if body := w.Body.String(); strings.Contains(body, "bad-token") {
		t.Errorf("error echoes the token: %s", body)
	}
	if cfg := settings.Read(); cfg.GitLabToken != nil || cfg.GitLabHost != nil {
		t.Errorf("rejected token was kept: %v %v", cfg.GitLabToken, cfg.GitLabHost)
	}
	if s.pr.token != "" {
		t.Errorf("session took a rejected token")
	}
	if w := postJSON(t, s, "/api/forge/connect", `{"token":"  "}`); w.Code != 400 {
		t.Errorf("empty token: %d", w.Code)
	}
}

// A token already saved for one GitLab host is not replaced by connecting to
// another: the new one serves this session only.
func TestForgeConnectKeepsOtherHostsToken(t *testing.T) {
	s := connectServer(t, "gitlab.other.org")
	if err := settings.UpdateMap(map[string]any{"gitlab.token": "corp-token", "gitlab.host": "gitlab.corp.io"}); err != nil {
		t.Fatal(err)
	}
	w := postJSON(t, s, "/api/forge/connect", `{"token":"good-token"}`)
	var res map[string]any
	json.Unmarshal(w.Body.Bytes(), &res)
	if w.Code != 200 || res["saved"] != false {
		t.Fatalf("connect: %d %v", w.Code, res)
	}
	cfg := settings.Read()
	if *cfg.GitLabToken != "corp-token" || *cfg.GitLabHost != "gitlab.corp.io" {
		t.Errorf("other host's token replaced: %v %v", *cfg.GitLabToken, *cfg.GitLabHost)
	}
	if s.pr.token != "good-token" || s.forgeMeta()["source"] != "session" {
		t.Errorf("session token = %q, source %v", s.pr.token, s.forgeMeta()["source"])
	}
}

func TestTokenSetupPerForge(t *testing.T) {
	cases := []struct {
		p       GitProvider
		host    string
		url     string
		hostKey string
	}{
		{&GitHubProvider{}, "github.com", "https://github.com/settings/personal-access-tokens/new", ""},
		{&GitLabProvider{}, "gitlab.corp.io", "https://gitlab.corp.io/-/user_settings/personal_access_tokens", "gitlab.host"},
		{&GiteaProvider{}, "git.example.org", "https://git.example.org/user/settings/applications", "gitea.host"},
		{&BitbucketProvider{}, "bitbucket.org", "https://id.atlassian.com/manage-profile/security/api-tokens", ""},
	}
	for _, c := range cases {
		st := c.p.TokenSetup(c.host)
		if st.URL != c.url || st.HostKey != c.hostKey || len(st.Steps) == 0 || st.Env == "" || st.Readonly == "" {
			t.Errorf("%s: %+v", c.p.Name(), st)
		}
		// Every setting the dialog writes must exist in the Settings schema.
		for _, key := range []string{c.p.Info().TokenKey, st.HostKey} {
			if key != "" && !schemaHas(key) {
				t.Errorf("%s: setting %q missing from the schema", c.p.Name(), key)
			}
		}
		if st.Extra != nil && !schemaHas(st.Extra.Key) {
			t.Errorf("%s: setting %q missing from the schema", c.p.Name(), st.Extra.Key)
		}
	}
}

func schemaHas(key string) bool {
	for _, it := range settings.Schema {
		if it.Key == key {
			return true
		}
	}
	return false
}

func TestGitHubMergeState(t *testing.T) {
	f, tr := false, true
	cases := []struct {
		merged    bool
		state     string
		draft     bool
		mergeable *bool
		ms        string
		want      string
	}{
		{merged: true, state: "closed", want: MergeStateMerged},
		{state: "closed", want: MergeStateClosed},
		{state: "open", draft: true, ms: "draft", want: MergeStateDraft},
		{state: "open", mergeable: &f, ms: "dirty", want: MergeStateConflicts},
		{state: "open", mergeable: &f, want: MergeStateConflicts},
		{state: "open", mergeable: &tr, ms: "behind", want: MergeStateBehind},
		{state: "open", mergeable: &tr, ms: "blocked", want: MergeStateBlocked},
		{state: "open", mergeable: &tr, ms: "unstable", want: MergeStateUnstable},
		{state: "open", mergeable: &tr, ms: "clean", want: MergeStateClean},
		{state: "open", mergeable: &tr, ms: "has_hooks", want: MergeStateClean},
		{state: "open", ms: "unknown", want: MergeStateUnknown},
		{state: "open", want: MergeStateUnknown},
	}
	for _, c := range cases {
		if got, _ := githubMergeState(c.merged, c.state, c.draft, c.mergeable, c.ms, "main"); got != c.want {
			t.Errorf("githubMergeState(%+v) = %q, want %q", c, got, c.want)
		}
	}
}

func TestFetchMergeStatusMethods(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		body := `{"state":"open","mergeable":true,"mergeable_state":"clean","base":{"ref":"main"},"head":{"sha":"abc123"}}`
		if req.URL.Path == "/repos/o/r" {
			body = `{"allow_merge_commit":true,"allow_squash_merge":true,"allow_rebase_merge":false,"permissions":{"push":true}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	st, err := fetchMergeStatus(context.Background(), "o", "r", 7, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if st.State != MergeStateClean || !st.CanMerge || st.HeadSHA != "abc123" {
		t.Fatalf("status = %+v", st)
	}
	if strings.Join(st.Methods, ",") != "merge,squash" {
		t.Fatalf("methods = %v, want merge,squash (rebase disallowed on the repo)", st.Methods)
	}
}

func TestMergePRRequestAndErrors(t *testing.T) {
	orig := githubHTTPClient.Transport
	defer func() { githubHTTPClient.Transport = orig }()
	var got map[string]string
	status := http.StatusOK
	githubHTTPClient.Transport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPut || req.URL.Path != "/repos/o/r/pulls/7/merge" {
			t.Errorf("unexpected request %s %s", req.Method, req.URL.Path)
		}
		got = nil
		json.NewDecoder(req.Body).Decode(&got)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"sha":"m1","merged":true,"message":"Head branch was modified"}`)), Header: make(http.Header)}, nil
	})

	res, err := mergePR(context.Background(), "o", "r", 7, "tok", PRMergeRequest{Method: MergeMethodSquash, Title: "Add x (#7)", Message: "body", HeadSHA: "abc"})
	if err != nil || res.SHA != "m1" {
		t.Fatalf("merge = %+v, %v", res, err)
	}
	if got["merge_method"] != "squash" || got["sha"] != "abc" || got["commit_title"] != "Add x (#7)" || got["commit_message"] != "body" {
		t.Fatalf("payload = %v", got)
	}

	// A rebase writes no commit of its own: no title or message is sent.
	if _, err := mergePR(context.Background(), "o", "r", 7, "tok", PRMergeRequest{Method: MergeMethodRebase, Title: "ignored", HeadSHA: "abc"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["commit_title"]; ok {
		t.Fatalf("rebase payload carries a title: %v", got)
	}

	status = http.StatusConflict
	if _, err := mergePR(context.Background(), "o", "r", 7, "tok", PRMergeRequest{Method: MergeMethodMerge, HeadSHA: "abc"}); !errors.Is(err, ErrPRHeadMoved) {
		t.Fatalf("409: err = %v, want ErrPRHeadMoved", err)
	}
	status = http.StatusMethodNotAllowed
	if _, err := mergePR(context.Background(), "o", "r", 7, "tok", PRMergeRequest{Method: MergeMethodMerge}); !errors.Is(err, ErrPRNotMergeable) {
		t.Fatalf("405: err = %v, want ErrPRNotMergeable", err)
	}
}

func TestPRMergeDefaults(t *testing.T) {
	meta := PRMeta{Number: 12, Title: "Add widgets", HeadRef: "widgets"}
	d := prMergeDefaults(meta, []UnpushedCommit{{Subject: "second"}, {Subject: "first"}})
	if d[MergeMethodSquash].Title != "Add widgets (#12)" || d[MergeMethodSquash].Message != "* first\n* second" {
		t.Fatalf("squash default = %+v", d[MergeMethodSquash])
	}
	if d[MergeMethodMerge].Title != "Merge pull request #12 from widgets" || d[MergeMethodMerge].Message != "Add widgets" {
		t.Fatalf("merge default = %+v", d[MergeMethodMerge])
	}
}

func TestMergeMessagePrompt(t *testing.T) {
	meta := PRMeta{Number: 3, Title: "Fix race", BaseRef: "main", HeadRef: "fix"}
	p := mergeMessagePrompt(MergeMethodSquash, meta, []UnpushedCommit{{Subject: "fixup"}, {Subject: "fix the race"}}, "a.go | 2 +-", "diff --git", "Fix race (#3)", []string{"Follow Conventional Commits", "Mention ticket PX-9"})
	for _, want := range []string{"squash-merging pull request #3", "Follow Conventional Commits", "Mention ticket PX-9", "rewriting", "Fix race (#3)", "- fix the race\n- fixup", "Output ONLY"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q:\n%s", want, p)
		}
	}
}

func TestGitSyncPromptPR(t *testing.T) {
	st := gitSyncState{PR: 9, Branch: "feat", Upstream: "main", Ahead: 2, Behind: 5, ontoRef: "refs/px0/base/9"}
	p := gitSyncPrompt("rebase", st)
	for _, want := range []string{"Pull request #9", "git rebase refs/px0/base/9", "do not run git fetch", "do not push"} {
		if !strings.Contains(p, want) {
			t.Errorf("rebase prompt missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "git fetch refs") {
		t.Errorf("prompt asks the harness to fetch:\n%s", p)
	}
	if p := gitSyncPrompt("merge", st); !strings.Contains(p, `Merge branch 'main' into feat`) || !strings.Contains(p, "refs/px0/base/9") {
		t.Errorf("merge prompt:\n%s", p)
	}
}

// TestPRForcePushAfterRebase covers the PR rebase flow's last step: the
// rebased checkout is detected as rewritten, a plain push is refused, and a
// force-push with lease replaces the PR branch -- unless someone else pushed
// to it meanwhile, in which case the lease refuses and their commit survives.
func TestPRForcePushAfterRebase(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	base := t.TempDir()
	if r, err := filepath.EvalSymlinks(base); err == nil {
		base = r
	}
	cfg := func(dir string) {
		for _, c := range [][2]string{{"user.email", "t@example.com"}, {"user.name", "T"}, {"commit.gpgsign", "false"}} {
			gitTestRun(t, dir, "config", c[0], c[1])
		}
	}
	upstream := filepath.Join(base, "upstream.git")
	os.MkdirAll(upstream, 0o755)
	gitTestRun(t, upstream, "init", "-q", "--bare", "-b", "main")

	work := filepath.Join(base, "work")
	os.MkdirAll(work, 0o755)
	gitTestRun(t, work, "init", "-q", "-b", "main")
	cfg(work)
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("a\n"), 0o644)
	gitTestRun(t, work, "add", ".")
	gitTestRun(t, work, "commit", "-qm", "base")
	gitTestRun(t, work, "push", "-q", upstream, "main")
	gitTestRun(t, work, "checkout", "-qb", "feature")
	os.WriteFile(filepath.Join(work, "f.txt"), []byte("f\n"), 0o644)
	gitTestRun(t, work, "add", ".")
	gitTestRun(t, work, "commit", "-qm", "feature")
	gitTestRun(t, work, "push", "-q", upstream, "feature")
	head := strings.TrimSpace(gitTestRun(t, work, "rev-parse", "HEAD"))
	gitTestRun(t, work, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(work, "b.txt"), []byte("b\n"), 0o644)
	gitTestRun(t, work, "add", ".")
	gitTestRun(t, work, "commit", "-qm", "main moves on")
	gitTestRun(t, work, "push", "-q", upstream, "main")

	wt := filepath.Join(base, "wt")
	gitTestRun(t, base, "clone", "-q", "-b", "feature", upstream, "wt")
	cfg(wt)
	gitTestRun(t, wt, "fetch", "-q", upstream, "+refs/heads/main:"+prBaseRef(1))
	p := &prSession{
		worktree: wt,
		target:   PRTarget{Owner: "o", Repo: "r"},
		meta:     PRMeta{Number: 1, BaseRef: "main", HeadRef: "feature", HeadRepoCloneURL: upstream, HeadSHA: head},
	}
	if p.Rewritten() {
		t.Fatal("fresh checkout reported as rewritten")
	}
	if a, b := gitAheadBehindRef(wt, prBaseRef(1)); a != 1 || b != 1 {
		t.Fatalf("ahead/behind base = %d/%d, want 1/1", a, b)
	}

	gitTestRun(t, wt, "rebase", "-q", prBaseRef(1))
	if !p.Rewritten() {
		t.Fatal("rebased checkout not reported as rewritten")
	}
	if err := p.Push(false); err == nil {
		t.Fatal("a plain push of a rebased branch should be refused")
	}

	// Someone else pushes to the PR branch: the lease must refuse.
	other := filepath.Join(base, "other")
	gitTestRun(t, base, "clone", "-q", "-b", "feature", upstream, "other")
	cfg(other)
	os.WriteFile(filepath.Join(other, "o.txt"), []byte("o\n"), 0o644)
	gitTestRun(t, other, "add", ".")
	gitTestRun(t, other, "commit", "-qm", "theirs")
	gitTestRun(t, other, "push", "-q", "origin", "feature")
	theirs := strings.TrimSpace(gitTestRun(t, other, "rev-parse", "HEAD"))
	if err := p.Push(true); err == nil {
		t.Fatal("force-push should be refused by the lease after someone else pushed")
	}
	if got := strings.TrimSpace(gitTestRun(t, upstream, "rev-parse", "refs/heads/feature")); got != theirs {
		t.Fatalf("their commit was overwritten: feature = %s, want %s", got, theirs)
	}

	// Lease on what is actually there (as a pull would): the force-push lands.
	gitTestRun(t, upstream, "update-ref", "refs/heads/feature", head)
	if err := p.Push(true); err != nil {
		t.Fatalf("force-push with a valid lease: %v", err)
	}
	rebased := strings.TrimSpace(gitTestRun(t, wt, "rev-parse", "HEAD"))
	if got := strings.TrimSpace(gitTestRun(t, upstream, "rev-parse", "refs/heads/feature")); got != rebased {
		t.Fatalf("feature = %s, want the rebased head %s", got, rebased)
	}
	if p.Rewritten() || p.meta.HeadSHA != rebased {
		t.Fatalf("after push: rewritten=%v head=%s", p.Rewritten(), p.meta.HeadSHA)
	}
	mainTip := strings.TrimSpace(gitTestRun(t, wt, "rev-parse", prBaseRef(1)))
	if p.diffBase != mainTip {
		t.Fatalf("diffBase = %q, want the new merge-base %s", p.diffBase, mainTip)
	}
}

// ---------------------------------------------------------------- Forges (GitLab, Gitea, Bitbucket)

// Every URL shape routes to exactly its own provider.
func TestForgeURLRouting(t *testing.T) {
	cases := map[string]string{
		"https://github.com/o/r/pull/1":                          "github",
		"https://gitlab.com/g/sub/r/-/merge_requests/2":          "gitlab",
		"https://codeberg.org/o/r/pulls/3":                       "gitea",
		"https://git.example.com/o/r/pulls/4/files":              "gitea",
		"https://bitbucket.org/ws/r/pull-requests/5":             "bitbucket",
		"https://bitbucket.org/ws/r/pull-requests/5/diff":        "bitbucket",
		"https://gitea.example.com/o/r/pulls/6":                  "gitea",
		"https://gitlab.example.com/a/b/c/d/-/merge_requests/77": "gitlab",
	}
	for u, want := range cases {
		p, tg, ok := DetectPRURL(u)
		if !ok || p.Name() != want || tg.Provider != want {
			t.Errorf("%s: routed to %v (ok=%v), want %s", u, p, ok, want)
		}
	}
	for _, bad := range []string{"https://github.com/o/r/pulls/1", "https://bitbucket.org/ws/r", "https://example.com/o/r/pull/1"} {
		if p, _, ok := DetectPRURL(bad); ok {
			t.Errorf("%s: unexpectedly routed to %s", bad, p.Name())
		}
	}
	remotes := map[string]string{
		"git@bitbucket.org:ws/r.git":     "bitbucket",
		"https://codeberg.org/o/r.git":   "gitea",
		"git@gitea.corp.example:o/r.git": "gitea",
		"ssh://git@gitlab.corp.io/o/r":   "gitlab",
		"https://github.com/o/r":         "github",
	}
	for r, want := range remotes {
		if p := ForgeForRemote(r); p == nil || p.Name() != want {
			t.Errorf("ForgeForRemote(%s) = %v, want %s", r, p, want)
		}
	}
}

func TestGiteaTokenScopedToHost(t *testing.T) {
	t.Setenv("GITEA_HOST", "")
	t.Setenv("GITEA_TOKEN", "envtok")
	g := &GiteaProvider{}
	if tok, _ := g.ResolveToken(settings.Settings{}, PRTarget{Host: "codeberg.org"}); tok != "envtok" {
		t.Errorf("codeberg default: %q", tok)
	}
	if tok, _ := g.ResolveToken(settings.Settings{}, PRTarget{Host: "evil.example"}); tok != "" {
		t.Errorf("token leaked to another host: %q", tok)
	}
	host, tok := "https://git.corp.example/", "settok"
	cfg := settings.Settings{GiteaHost: &host, GiteaToken: &tok}
	if got, src := g.ResolveToken(cfg, PRTarget{Host: "git.corp.example"}); got != "settok" || src != "settings" {
		t.Errorf("configured host: %q/%q", got, src)
	}
	if got, _ := g.ResolveToken(cfg, PRTarget{Host: "codeberg.org"}); got != "" {
		t.Errorf("configured host set, codeberg got %q", got)
	}
}

func TestGiteaMergeMethodsAndState(t *testing.T) {
	f, tr := false, true
	cases := []struct {
		r    giteaRepo
		want string
	}{
		{giteaRepo{}, "merge,squash,rebase"},
		{giteaRepo{AllowMerge: &f, AllowRebase: &f, AllowSquash: &tr}, "squash"},
		{giteaRepo{AllowMerge: &tr, AllowRebase: &f, AllowSquash: &f, AllowFastForward: &tr}, "merge,rebase"},
	}
	for _, c := range cases {
		if got := strings.Join(giteaMergeMethods(c.r), ","); got != c.want {
			t.Errorf("%+v: %q, want %q", c.r, got, c.want)
		}
	}
	states := []struct {
		pr   giteaPR
		want string
	}{
		{giteaPR{Merged: true, State: "closed"}, MergeStateMerged},
		{giteaPR{State: "closed"}, MergeStateClosed},
		{giteaPR{State: "open", Title: "WIP: thing", Mergeable: true}, MergeStateDraft},
		{giteaPR{State: "open", Title: "[Draft] thing"}, MergeStateDraft},
		{giteaPR{State: "open", Mergeable: true}, MergeStateClean},
		{giteaPR{State: "open"}, MergeStateConflicts},
	}
	for _, c := range states {
		if got, _ := giteaMergeState(c.pr); got != c.want {
			t.Errorf("%+v: %q, want %q", c.pr, got, c.want)
		}
	}
}

// recorder is a fake forge API: handlers keyed by "METHOD path", writes logged.
type recorder struct {
	mu     sync.Mutex
	routes map[string]func(w http.ResponseWriter, r *http.Request)
	writes []string
	bodies []map[string]any
}

func (rc *recorder) serve(t *testing.T, wantAuth string) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Errorf("%s %s: Authorization = %q, want %q", r.Method, r.URL.Path, got, wantAuth)
		}
		key := r.Method + " " + r.URL.EscapedPath()
		if r.Method != http.MethodGet {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			rc.mu.Lock()
			rc.writes = append(rc.writes, key)
			rc.bodies = append(rc.bodies, body)
			rc.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		if h, ok := rc.routes[key]; ok {
			h(w, r)
			return
		}
		t.Errorf("unexpected %s (query %s)", key, r.URL.RawQuery)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func reply(status int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

func fakeGitea(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request)) (*recorder, PRTarget) {
	rc := &recorder{routes: routes}
	srv := rc.serve(t, "token tok")
	orig := giteaAPIBase
	giteaAPIBase = func(string) string { return srv.URL + "/api/v1" }
	t.Cleanup(func() { giteaAPIBase = orig })
	return rc, PRTarget{Provider: "gitea", Host: "codeberg.org", Owner: "o", Repo: "r", Number: 3}
}

const giteaPRJSON = `{"number":3,"title":"Add x","state":"open","mergeable":true,"user":{"login":"ana"},
	"base":{"ref":"main"},"head":{"ref":"feat","sha":"h1","repo":{"clone_url":"https://codeberg.org/ana/r.git","full_name":"ana/r"}}}`

func TestGiteaProvider(t *testing.T) {
	rc, tg := fakeGitea(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET /api/v1/repos/o/r/pulls/3":                    reply(200, giteaPRJSON),
		"GET /api/v1/repos/o/r":                            reply(200, `{"permissions":{"push":true},"allow_merge_commits":true,"allow_rebase":false,"allow_squash_merge":true,"allow_fast_forward_only_merge":true}`),
		"POST /api/v1/repos/o/r/pulls/3/reviews":           reply(200, `{"id":5}`),
		"GET /api/v1/repos/o/r/issues/3/comments":          reply(200, `[{"id":1,"body":"hi","user":{"login":"bo"},"created_at":"2026-01-01T00:00:00Z"}]`),
		"GET /api/v1/repos/o/r/pulls/3/reviews":            reply(200, `[{"id":7},{"id":8}]`),
		"GET /api/v1/repos/o/r/pulls/3/reviews/7/comments": reply(200, `[{"id":70,"body":"first","path":"a.go","position":4,"user":{"login":"bo"},"created_at":"2026-01-02T00:00:00Z"}]`),
		"GET /api/v1/repos/o/r/pulls/3/reviews/8/comments": reply(200, `[{"id":80,"body":"answer","path":"a.go","position":4,"user":{"login":"ana"},"created_at":"2026-01-03T00:00:00Z"},
			{"id":81,"body":"elsewhere","path":"b.go","position":1,"user":{"login":"ana"},"created_at":"2026-01-03T00:00:01Z"}]`),
		"POST /api/v1/repos/o/r/pulls/3/merge": reply(200, ``),
	})
	g := &GiteaProvider{}
	ctx := context.Background()
	m, err := g.FetchPR(ctx, tg, "tok")
	if err != nil || m.HeadSHA != "h1" || !m.HeadIsFork || m.HeadRepoCloneURL != "https://codeberg.org/ana/r.git" || m.BaseRef != "main" {
		t.Fatalf("FetchPR = %+v, %v", m, err)
	}
	st, err := g.MergeStatus(ctx, tg, "tok")
	if err != nil || st.State != MergeStateClean || !st.CanMerge || strings.Join(st.Methods, ",") != "merge,squash,rebase" {
		t.Errorf("MergeStatus = %+v, %v", st, err)
	}

	err = g.SubmitReview(ctx, tg, "tok", "h1", []prComment{{Path: "a.go", Line: 4, Side: "RIGHT", Body: "nit"}, {Path: "b.go", Line: 2, Side: "LEFT", Body: "old"}}, "APPROVE", "ok")
	if err != nil {
		t.Fatal(err)
	}
	b := rc.bodies[0]
	cs := b["comments"].([]any)
	if b["event"] != "APPROVED" || b["commit_id"] != "h1" || cs[0].(map[string]any)["new_position"] != float64(4) || cs[1].(map[string]any)["old_position"] != float64(2) {
		t.Errorf("review payload = %v", b)
	}

	issue, review, err := g.FetchComments(ctx, tg, "tok")
	if err != nil || len(issue) != 1 || len(review) != 3 {
		t.Fatalf("comments: %v %v %v", issue, review, err)
	}
	if review[1].ID != 80 || review[1].InReplyTo != 70 || review[2].InReplyTo != 0 {
		t.Errorf("threading = %+v", review)
	}

	if _, err := g.ReplyToReviewComment(ctx, tg, "tok", 80, "ack"); err != nil {
		t.Fatal(err)
	}
	last := rc.bodies[len(rc.bodies)-1]
	if c := last["comments"].([]any)[0].(map[string]any); c["path"] != "a.go" || c["new_position"] != float64(4) || last["event"] != "COMMENT" {
		t.Errorf("reply payload = %v", last)
	}

	// Rebase is disallowed but fast-forward-only isn't: that's what's used.
	if _, err := g.Merge(ctx, tg, "tok", PRMergeRequest{Method: MergeMethodRebase, Title: "ignored", HeadSHA: "h1"}); err != nil {
		t.Fatal(err)
	}
	if last := rc.bodies[len(rc.bodies)-1]; last["Do"] != "fast-forward-only" || last["head_commit_id"] != "h1" || last["MergeTitleField"] != nil {
		t.Errorf("merge payload = %v", last)
	}
	g.Merge(ctx, tg, "tok", PRMergeRequest{Method: MergeMethodSquash, Title: "T", Message: "B"})
	if last := rc.bodies[len(rc.bodies)-1]; last["Do"] != "squash" || last["MergeTitleField"] != "T" || last["MergeMessageField"] != "B" {
		t.Errorf("squash payload = %v", last)
	}
}

func TestGiteaMergeErrors(t *testing.T) {
	status, msg := 409, `{"message":"head out of date"}`
	_, tg := fakeGitea(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST /api/v1/repos/o/r/pulls/3/merge": func(w http.ResponseWriter, r *http.Request) { reply(status, msg)(w, r) },
	})
	g := &GiteaProvider{}
	if _, err := g.Merge(context.Background(), tg, "tok", PRMergeRequest{Method: MergeMethodMerge}); !errors.Is(err, ErrPRHeadMoved) {
		t.Errorf("409 head: %v", err)
	}
	msg = `{"message":"merge conflict"}`
	if _, err := g.Merge(context.Background(), tg, "tok", PRMergeRequest{Method: MergeMethodMerge}); !errors.Is(err, ErrPRNotMergeable) {
		t.Errorf("409 conflict: %v", err)
	}
	status, msg = 405, `{"message":"not allowed"}`
	if _, err := g.Merge(context.Background(), tg, "tok", PRMergeRequest{Method: MergeMethodMerge}); !errors.Is(err, ErrPRNotMergeable) {
		t.Errorf("405: %v", err)
	}
}

func fakeBitbucket(t *testing.T, routes map[string]func(http.ResponseWriter, *http.Request)) (*recorder, PRTarget) {
	t.Setenv("BITBUCKET_EMAIL", "")
	rc := &recorder{routes: routes}
	srv := rc.serve(t, "Bearer tok")
	orig := bitbucketAPIBase
	bitbucketAPIBase = srv.URL + "/2.0"
	t.Cleanup(func() { bitbucketAPIBase = orig })
	return rc, PRTarget{Provider: "bitbucket", Host: "bitbucket.org", Owner: "ws", Repo: "r", Number: 9}
}

const bbFull = "abcdef0123456789abcdef0123456789abcdef01"

var bbPRJSON = `{"id":9,"title":"Add y","state":"OPEN","author":{"nickname":"ana"},
	"source":{"branch":{"name":"feat"},"commit":{"hash":"abcdef012345"},"repository":{"full_name":"ana/r"}},
	"destination":{"branch":{"name":"main"},"commit":{"hash":"000000000000"},"repository":{"full_name":"ws/r"}}}`

func bbRoutes(extra map[string]func(http.ResponseWriter, *http.Request)) map[string]func(http.ResponseWriter, *http.Request) {
	routes := map[string]func(http.ResponseWriter, *http.Request){
		"GET /2.0/repositories/ws/r/pullrequests/9":          reply(200, bbPRJSON),
		"GET /2.0/repositories/ana/r/commit/abcdef012345":    reply(200, `{"hash":"`+bbFull+`"}`),
		"GET /2.0/repositories/ws/r/pullrequests/9/diffstat": reply(200, `{"values":[{"status":"modified"}]}`),
		"GET /2.0/repositories/ws/r/refs/branches/main":      reply(200, `{"merge_strategies":["merge_commit","squash","rebase_fast_forward","fast_forward"]}`),
		"GET /2.0/user/permissions/repositories":             reply(200, `{"values":[{"permission":"write"}]}`),
	}
	for k, v := range extra {
		routes[k] = v
	}
	return routes
}

func TestBitbucketProvider(t *testing.T) {
	comments := `{"values":[
		{"id":1,"content":{"raw":"general"},"user":{"nickname":"bo"},"created_on":"2026-01-01"},
		{"id":2,"content":{"raw":"on a line"},"user":{"nickname":"bo"},"inline":{"path":"a.go","to":7,"from":null}},
		{"id":3,"content":{"raw":"reply"},"user":{"nickname":"ana"},"parent":{"id":2},"inline":{"path":"a.go","to":7}},
		{"id":4,"content":{"raw":"reply to reply"},"user":{"nickname":"bo"},"parent":{"id":3}},
		{"id":5,"content":{"raw":""},"deleted":true}
	],"next":""}`
	rc, tg := fakeBitbucket(t, bbRoutes(map[string]func(http.ResponseWriter, *http.Request){
		"GET /2.0/repositories/ws/r/pullrequests/9/comments":         reply(200, comments),
		"POST /2.0/repositories/ws/r/pullrequests/9/comments":        reply(201, `{"id":50,"content":{"raw":"x"}}`),
		"POST /2.0/repositories/ws/r/pullrequests/9/approve":         reply(200, `{}`),
		"POST /2.0/repositories/ws/r/pullrequests/9/request-changes": reply(200, `{}`),
		"POST /2.0/repositories/ws/r/pullrequests/9/merge":           reply(200, `{"merge_commit":{"hash":"m1"}}`),
	}))
	b := &BitbucketProvider{}
	ctx := context.Background()
	m, err := b.FetchPR(ctx, tg, "tok")
	if err != nil || m.HeadSHA != bbFull || !m.HeadIsFork || m.HeadRepoCloneURL != "https://bitbucket.org/ana/r.git" || m.BaseRef != "main" || m.State != "open" {
		t.Fatalf("FetchPR = %+v, %v", m, err)
	}
	if b.PRHeadRef(tg) != "" {
		t.Error("Bitbucket has no PR head ref")
	}
	st, err := b.MergeStatus(ctx, tg, "tok")
	if err != nil || st.State != MergeStateClean || !st.CanMerge || st.HeadSHA != bbFull || strings.Join(st.Methods, ",") != "merge,squash,rebase" {
		t.Errorf("MergeStatus = %+v, %v", st, err)
	}

	issue, review, err := b.FetchComments(ctx, tg, "tok")
	if err != nil || len(issue) != 1 || len(review) != 3 {
		t.Fatalf("comments: %+v %+v %v", issue, review, err)
	}
	if review[0].Line != 7 || review[0].Side != "RIGHT" || review[1].InReplyTo != 2 || review[2].InReplyTo != 2 || review[2].Path != "a.go" {
		t.Errorf("threads = %+v", review)
	}

	if err := b.SubmitReview(ctx, tg, "tok", bbFull, []prComment{{Path: "a.go", Line: 3, Side: "LEFT", Body: "old"}}, "REQUEST_CHANGES", "fix"); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /2.0/repositories/ws/r/pullrequests/9/comments", "POST /2.0/repositories/ws/r/pullrequests/9/comments", "POST /2.0/repositories/ws/r/pullrequests/9/request-changes"}
	if strings.Join(rc.writes, "|") != strings.Join(want, "|") {
		t.Fatalf("writes = %v", rc.writes)
	}
	if in := rc.bodies[0]["inline"].(map[string]any); in["from"] != float64(3) || in["to"] != nil {
		t.Errorf("inline = %v", in)
	}

	if c, err := b.ReplyToReviewComment(ctx, tg, "tok", 3, "ok"); err != nil || c.InReplyTo != 2 || c.Path != "a.go" {
		t.Errorf("reply = %+v, %v", c, err)
	}
	if p := rc.bodies[len(rc.bodies)-1]["parent"].(map[string]any); p["id"] != float64(3) {
		t.Errorf("reply parent = %v", p)
	}

	res, err := b.Merge(ctx, tg, "tok", PRMergeRequest{Method: MergeMethodRebase, HeadSHA: bbFull, Title: "ignored"})
	if err != nil || res.SHA != "m1" {
		t.Fatalf("merge = %+v, %v", res, err)
	}
	if last := rc.bodies[len(rc.bodies)-1]; last["merge_strategy"] != "rebase_fast_forward" || last["message"] != nil || last["close_source_branch"] != false {
		t.Errorf("merge payload = %v", last)
	}
	b.Merge(ctx, tg, "tok", PRMergeRequest{Method: MergeMethodSquash, HeadSHA: bbFull, Title: "T", Message: "B"})
	if last := rc.bodies[len(rc.bodies)-1]; last["merge_strategy"] != "squash" || last["message"] != "T\n\nB" {
		t.Errorf("squash payload = %v", last)
	}

	// Bitbucket has no head guard: px0 refuses itself when the head moved.
	n := len(rc.writes)
	if _, err := b.Merge(ctx, tg, "tok", PRMergeRequest{Method: MergeMethodMerge, HeadSHA: "1111111111111111111111111111111111111111"}); !errors.Is(err, ErrPRHeadMoved) {
		t.Errorf("moved head: %v", err)
	}
	if len(rc.writes) != n {
		t.Error("a merge was sent despite the moved head")
	}
}

func TestBitbucketConflictsAndRefusal(t *testing.T) {
	_, tg := fakeBitbucket(t, bbRoutes(map[string]func(http.ResponseWriter, *http.Request){
		"GET /2.0/repositories/ws/r/pullrequests/9/diffstat": reply(200, `{"values":[{"status":"modified"},{"status":"merge conflict"}]}`),
		"GET /2.0/repositories/ws/r/refs/branches/main":      reply(200, `{"merge_strategies":["squash"]}`),
		"POST /2.0/repositories/ws/r/pullrequests/9/merge":   reply(400, `{"type":"error","error":{"message":"You can't merge until you resolve all merge conflicts."}}`),
	}))
	b := &BitbucketProvider{}
	st, _ := b.MergeStatus(context.Background(), tg, "tok")
	if st.State != MergeStateConflicts || strings.Join(st.Methods, ",") != "squash" {
		t.Errorf("status = %+v", st)
	}
	if _, err := b.Merge(context.Background(), tg, "tok", PRMergeRequest{Method: MergeMethodMerge, HeadSHA: bbFull}); !errors.Is(err, ErrPRNotMergeable) {
		t.Errorf("disallowed strategy: %v", err)
	}
	_, err := b.Merge(context.Background(), tg, "tok", PRMergeRequest{Method: MergeMethodSquash, HeadSHA: bbFull})
	if !errors.Is(err, ErrPRNotMergeable) || !strings.Contains(err.Error(), "resolve all merge conflicts") {
		t.Errorf("400: %v", err)
	}
}

func TestBitbucketAuthKinds(t *testing.T) {
	b := &BitbucketProvider{}
	t.Setenv("BITBUCKET_EMAIL", "")
	if a := bitbucketAPI("tok"); a.auth != "Bearer tok" {
		t.Errorf("access token API auth = %q", a.auth)
	}
	if a := b.GitAuth(PRTarget{}, "tok"); a.Prefix != "https://bitbucket.org/" || a.Header != basicGitAuth("bitbucket.org", "x-token-auth", "tok").Header {
		t.Errorf("access token git auth = %+v", a)
	}
	t.Setenv("BITBUCKET_EMAIL", "me@example.com")
	if a := bitbucketAPI("tok"); !strings.HasPrefix(a.auth, "Basic ") {
		t.Errorf("API token auth = %q", a.auth)
	}
	if a := b.GitAuth(PRTarget{}, "tok"); a.Header != basicGitAuth("bitbucket.org", "x-bitbucket-api-token-auth", "tok").Header {
		t.Errorf("API token git auth = %+v", a)
	}
}

func TestGitLabParseURL(t *testing.T) {
	g := &GitLabProvider{}
	cases := []struct {
		url               string
		host, owner, repo string
		num               int
	}{
		{"https://gitlab.com/group/project/-/merge_requests/12", "gitlab.com", "group", "project", 12},
		{"https://gitlab.com/group/sub/deeper/project/-/merge_requests/3/diffs", "gitlab.com", "group/sub/deeper", "project", 3},
		{"https://git.example.org/team/app/-/merge_requests/7", "git.example.org", "team", "app", 7},
	}
	for _, c := range cases {
		if !g.MatchURL(c.url) {
			t.Errorf("MatchURL(%q) = false", c.url)
			continue
		}
		tg, err := g.ParseURL(c.url)
		if err != nil {
			t.Fatal(err)
		}
		if tg.Host != c.host || tg.Owner != c.owner || tg.Repo != c.repo || tg.Number != c.num || tg.Provider != "gitlab" {
			t.Errorf("ParseURL(%q) = %+v", c.url, tg)
		}
	}
	for _, bad := range []string{"https://github.com/o/r/pull/1", "https://gitlab.com/group/project/merge_requests/1", "https://gitlab.com/group/project"} {
		if g.MatchURL(bad) {
			t.Errorf("MatchURL(%q) = true", bad)
		}
	}
	// Routing: each URL lands on its own provider.
	if p, _, ok := DetectPRURL("https://gitlab.com/a/b/-/merge_requests/1"); !ok || p.Name() != "gitlab" {
		t.Errorf("gitlab URL routed to %v", p)
	}
	if p, _, ok := DetectPRURL("https://github.com/a/b/pull/1"); !ok || p.Name() != "github" {
		t.Errorf("github URL routed to %v", p)
	}
}

func TestGitLabGitTransport(t *testing.T) {
	g := &GitLabProvider{}
	tg := PRTarget{Host: "git.example.org", Owner: "team/sub", Repo: "app", Number: 5}
	if got := g.CloneURL(tg); got != "https://git.example.org/team/sub/app.git" {
		t.Errorf("CloneURL = %q", got)
	}
	if got := g.PRHeadRef(tg); got != "refs/merge-requests/5/head" {
		t.Errorf("PRHeadRef = %q", got)
	}
	a := g.GitAuth(tg, "tok")
	if a.Prefix != "https://git.example.org/" || !strings.HasPrefix(a.Header, "Authorization: Basic ") || a.redact("x tok y") != "x *** y" {
		t.Errorf("GitAuth = %+v", a)
	}
	if (g.GitAuth(tg, "") != gitAuth{}) {
		t.Error("no token should mean no auth")
	}
	if gh := (&GitHubProvider{}).GitAuth(PRTarget{}, "t"); gh.Prefix != "https://github.com/" {
		t.Errorf("github auth prefix = %q", gh.Prefix)
	}
}

// A token configured for gitlab.com must never be sent to whatever host a
// merge request URL names.
func TestGitLabTokenScopedToHost(t *testing.T) {
	t.Setenv("GITLAB_HOST", "")
	t.Setenv("GITLAB_TOKEN", "envtok")
	t.Setenv("PATH", t.TempDir()) // no glab
	tok := "settok"
	cfg := settings.Settings{GitLabToken: &tok}
	g := &GitLabProvider{}
	if got, src := g.ResolveToken(cfg, PRTarget{Host: "gitlab.com"}); got != "settok" || src != "settings" {
		t.Errorf("gitlab.com: %q/%q", got, src)
	}
	if got, _ := g.ResolveToken(settings.Settings{}, PRTarget{Host: "gitlab.com"}); got != "envtok" {
		t.Errorf("gitlab.com env: %q", got)
	}
	if got, _ := g.ResolveToken(cfg, PRTarget{Host: "evil.example.com"}); got != "" {
		t.Errorf("token leaked to another host: %q", got)
	}
	t.Setenv("GITLAB_HOST", "https://git.corp.example/")
	if got, _ := g.ResolveToken(cfg, PRTarget{Host: "git.corp.example"}); got != "settok" {
		t.Errorf("GITLAB_HOST instance: %q", got)
	}
	if got, _ := g.ResolveToken(cfg, PRTarget{Host: "gitlab.com"}); got != "" {
		t.Errorf("with GITLAB_HOST set, gitlab.com got %q", got)
	}
}

func TestRemoteMatchesNestedGroups(t *testing.T) {
	cases := []struct {
		remote string
		want   bool
	}{
		{"git@gitlab.com:group/sub/app.git", true},
		{"https://gitlab.com/group/sub/app", true},
		{"ssh://git@gitlab.com:2222/group/sub/app.git", true},
		{"https://gitlab.com/sub/app.git", false},
		{"https://gitlab.com/other/group/sub/app.git", false},
		{"/srv/mirrors/group/sub/app.git", true}, // a local path matches by suffix
	}
	for _, c := range cases {
		if got := remoteMatchesRepo(c.remote, "group/sub", "app"); got != c.want {
			t.Errorf("%q: got %v, want %v", c.remote, got, c.want)
		}
	}
	if p := ForgeForRemote("git@gitlab.com:a/b.git"); p == nil || p.Name() != "gitlab" {
		t.Errorf("ForgeForRemote(gitlab) = %v", p)
	}
	if p := ForgeForRemote("https://github.com/a/b"); p == nil || p.Name() != "github" {
		t.Errorf("ForgeForRemote(github) = %v", p)
	}
	if p := ForgeForRemote("https://example.com/a/b.git"); p != nil {
		t.Errorf("ForgeForRemote(unknown) = %v", p.Name())
	}
}

func TestGitLabMergeState(t *testing.T) {
	cases := []struct {
		mr   glMR
		want string
	}{
		{glMR{State: "merged"}, MergeStateMerged},
		{glMR{State: "closed"}, MergeStateClosed},
		{glMR{State: "opened", Draft: true}, MergeStateDraft},
		{glMR{State: "opened", DetailedMergeStatus: "mergeable"}, MergeStateClean},
		{glMR{State: "opened", DetailedMergeStatus: "conflict"}, MergeStateConflicts},
		{glMR{State: "opened", DetailedMergeStatus: "need_rebase"}, MergeStateBehind},
		{glMR{State: "opened", DetailedMergeStatus: "not_approved"}, MergeStateBlocked},
		{glMR{State: "opened", DetailedMergeStatus: "ci_still_running"}, MergeStateBlocked},
		{glMR{State: "opened", DetailedMergeStatus: "checking"}, MergeStateUnknown},
		{glMR{State: "opened", DetailedMergeStatus: "something_new"}, MergeStateBlocked},
		{glMR{State: "opened", MergeStatus: "can_be_merged"}, MergeStateClean},
		{glMR{State: "opened", HasConflicts: true, MergeStatus: "cannot_be_merged"}, MergeStateConflicts},
		{glMR{State: "opened", MergeStatus: "unchecked"}, MergeStateUnknown},
	}
	for _, c := range cases {
		if got, _ := gitlabMergeState(c.mr); got != c.want {
			t.Errorf("%+v: got %q, want %q", c.mr, got, c.want)
		}
	}
}

func TestGitLabMergeMethods(t *testing.T) {
	cases := []struct {
		method, squash, want string
	}{
		{"merge", "default_off", "merge,squash"},
		{"rebase_merge", "default_on", "merge,squash"},
		{"merge", "never", "merge"},
		{"merge", "always", "squash"},
		{"ff", "default_off", "squash,rebase"},
		{"ff", "never", "rebase"},
		{"", "", "merge,squash"}, // fields absent: GitLab's defaults
	}
	for _, c := range cases {
		if got := strings.Join(gitlabMergeMethods(glProject{MergeMethod: c.method, SquashOption: c.squash}), ","); got != c.want {
			t.Errorf("%s/%s: got %q, want %q", c.method, c.squash, got, c.want)
		}
	}
}

// fakeGitLab serves a small GitLab API for one project, recording writes.
type fakeGitLab struct {
	mu       sync.Mutex
	writes   []string         // "METHOD path"
	bodies   []map[string]any // matching request bodies
	mergeErr int              // status to answer the merge call with, 0 = success
	failPos  bool             // reject positioned discussions (an unanchorable line)
}

func (f *fakeGitLab) handler(t *testing.T) http.Handler {
	const mr = "/api/v4/projects/group%2Fsub%2Fapp/merge_requests/4"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("%s %s: Authorization = %q", r.Method, r.URL.EscapedPath(), got)
		}
		path := r.URL.EscapedPath()
		if r.Method != http.MethodGet {
			var body map[string]any
			json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.writes = append(f.writes, r.Method+" "+strings.TrimPrefix(path, mr))
			f.bodies = append(f.bodies, body)
			f.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && path == mr:
			io.WriteString(w, `{"iid":4,"title":"Add app","state":"opened","sha":"head1","source_branch":"feat","target_branch":"main",
				"source_project_id":9,"target_project_id":1,"detailed_merge_status":"mergeable","author":{"username":"ana"},
				"user":{"can_merge":true},"diff_refs":{"base_sha":"b","head_sha":"head1","start_sha":"s"}}`)
		case r.Method == http.MethodGet && path == "/api/v4/projects/9":
			io.WriteString(w, `{"http_url_to_repo":"https://gitlab.test/ana/app.git"}`)
		case r.Method == http.MethodGet && path == "/api/v4/projects/group%2Fsub%2Fapp":
			io.WriteString(w, `{"merge_method":"merge","squash_option":"default_off","permissions":{"project_access":null,"group_access":{"access_level":40}}}`)
		case r.Method == http.MethodGet && path == mr+"/discussions":
			io.WriteString(w, `[
				{"id":"d1","notes":[{"id":11,"body":"general","author":{"username":"bo"}}]},
				{"id":"d2","notes":[{"id":21,"body":"on a line","author":{"username":"bo"},"position":{"position_type":"text","new_path":"a.go","new_line":7}},
				                    {"id":22,"body":"reply","author":{"username":"ana"}}]},
				{"id":"d3","notes":[{"id":31,"body":"added 2 commits","system":true}]}]`)
		case r.Method == http.MethodPost && path == mr+"/discussions":
			if f.failPos {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"message":"400 Bad request - Note {:line_code=>[\"can't be blank\"]}"}`)
				return
			}
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":"dn","notes":[{"id":99}]}`)
		case r.Method == http.MethodPost && (path == mr+"/notes" || path == mr+"/discussions/d2/notes"):
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{"id":77,"body":"posted","author":{"username":"me"}}`)
		case r.Method == http.MethodPost && (path == mr+"/approve" || path == mr+"/unapprove"):
			w.WriteHeader(http.StatusCreated)
			io.WriteString(w, `{}`)
		case r.Method == http.MethodPut && path == mr+"/merge":
			if f.mergeErr != 0 {
				w.WriteHeader(f.mergeErr)
				io.WriteString(w, `{"message":"SHA does not match HEAD of source branch"}`)
				return
			}
			io.WriteString(w, `{"merge_commit_sha":"m1","squash_commit_sha":"sq1"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func withFakeGitLab(t *testing.T, f *fakeGitLab) PRTarget {
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	orig := gitlabAPIBase
	gitlabAPIBase = func(string) string { return srv.URL + "/api/v4" }
	t.Cleanup(func() { gitlabAPIBase = orig })
	return PRTarget{Provider: "gitlab", Host: "gitlab.test", Owner: "group/sub", Repo: "app", Number: 4,
		URL: "https://gitlab.test/group/sub/app/-/merge_requests/4"}
}

func TestGitLabFetchPRAndAccess(t *testing.T) {
	f := &fakeGitLab{}
	tg := withFakeGitLab(t, f)
	g := &GitLabProvider{}
	m, err := g.FetchPR(context.Background(), tg, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if m.Number != 4 || m.BaseRef != "main" || m.HeadRef != "feat" || m.HeadSHA != "head1" || m.State != "open" || m.Author != "ana" {
		t.Errorf("meta = %+v", m)
	}
	if !m.HeadIsFork || m.HeadRepoCloneURL != "https://gitlab.test/ana/app.git" {
		t.Errorf("fork = %v, clone = %q", m.HeadIsFork, m.HeadRepoCloneURL)
	}
	if !g.CheckPushAccess(context.Background(), tg, "tok") {
		t.Error("Maintainer via group should have push access")
	}
	st, err := g.MergeStatus(context.Background(), tg, "tok")
	if err != nil || st.State != MergeStateClean || !st.CanMerge || strings.Join(st.Methods, ",") != "merge,squash" {
		t.Errorf("status = %+v, %v", st, err)
	}
}

func TestGitLabComments(t *testing.T) {
	f := &fakeGitLab{}
	tg := withFakeGitLab(t, f)
	g := &GitLabProvider{}
	issue, review, err := g.FetchComments(context.Background(), tg, "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(issue) != 1 || issue[0].Body != "general" || !strings.HasSuffix(issue[0].URL, "/-/merge_requests/4#note_11") {
		t.Errorf("issue = %+v", issue)
	}
	if len(review) != 2 || review[0].Path != "a.go" || review[0].Line != 7 || review[0].Side != "RIGHT" ||
		review[1].InReplyTo != 21 || review[1].Path != "a.go" || review[1].Line != 7 {
		t.Errorf("review = %+v", review)
	}
	c, err := g.ReplyToReviewComment(context.Background(), tg, "tok", 22, "thanks")
	if err != nil || c.InReplyTo != 21 || c.Path != "a.go" {
		t.Errorf("reply = %+v, %v", c, err)
	}
	if f.writes[len(f.writes)-1] != "POST /discussions/d2/notes" {
		t.Errorf("reply went to %v", f.writes)
	}
	if _, err := g.ReplyToReviewComment(context.Background(), tg, "tok", 12345, "x"); err == nil {
		t.Error("reply to an unknown note should fail")
	}
}

func TestGitLabSubmitReview(t *testing.T) {
	f := &fakeGitLab{}
	tg := withFakeGitLab(t, f)
	g := &GitLabProvider{}
	comments := []prComment{{Path: "a.go", Line: 3, Side: "RIGHT", Body: "nit"}, {Path: "b.go", Line: 9, Side: "LEFT", Body: "old"}}
	if err := g.SubmitReview(context.Background(), tg, "tok", "head1", comments, "APPROVE", "LGTM"); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /discussions", "POST /discussions", "POST /notes", "POST /approve"}
	if strings.Join(f.writes, "|") != strings.Join(want, "|") {
		t.Fatalf("writes = %v, want %v", f.writes, want)
	}
	pos := f.bodies[0]["position"].(map[string]any)
	if pos["new_line"] != float64(3) || pos["head_sha"] != "head1" || pos["base_sha"] != "b" {
		t.Errorf("RIGHT position = %v", pos)
	}
	if pos := f.bodies[1]["position"].(map[string]any); pos["old_line"] != float64(9) || pos["new_line"] != nil {
		t.Errorf("LEFT position = %v", pos)
	}
	if f.bodies[3]["sha"] != "head1" {
		t.Errorf("approve body = %v", f.bodies[3])
	}

	// A line GitLab can't anchor falls back to a note naming it; changes
	// requested is a note plus a withdrawn approval.
	f.writes, f.bodies, f.failPos = nil, nil, true
	if err := g.SubmitReview(context.Background(), tg, "tok", "head1", comments[:1], "REQUEST_CHANGES", "please fix"); err != nil {
		t.Fatal(err)
	}
	want = []string{"POST /discussions", "POST /notes", "POST /notes", "POST /unapprove"}
	if strings.Join(f.writes, "|") != strings.Join(want, "|") {
		t.Fatalf("writes = %v, want %v", f.writes, want)
	}
	if b := f.bodies[1]["body"].(string); !strings.Contains(b, "`a.go:3`") || !strings.Contains(b, "nit") {
		t.Errorf("fallback note = %q", b)
	}
	if b := f.bodies[2]["body"].(string); !strings.HasPrefix(b, "**Changes requested**") {
		t.Errorf("verdict note = %q", b)
	}
}

func TestGitLabMerge(t *testing.T) {
	f := &fakeGitLab{}
	tg := withFakeGitLab(t, f)
	g := &GitLabProvider{}
	res, err := g.Merge(context.Background(), tg, "tok", PRMergeRequest{Method: MergeMethodSquash, Title: "Add app (!4)", Message: "body", HeadSHA: "head1"})
	if err != nil || res.SHA != "m1" {
		t.Fatalf("merge = %+v, %v", res, err)
	}
	b := f.bodies[0]
	if b["squash"] != true || b["sha"] != "head1" || b["squash_commit_message"] != "Add app (!4)\n\nbody" || b["merge_commit_message"] != nil {
		t.Errorf("squash payload = %v", b)
	}
	f.bodies = nil
	g.Merge(context.Background(), tg, "tok", PRMergeRequest{Method: MergeMethodMerge, Title: "Merge", HeadSHA: "head1"})
	if b := f.bodies[0]; b["squash"] != false || b["merge_commit_message"] != "Merge" {
		t.Errorf("merge payload = %v", b)
	}
	f.mergeErr = http.StatusConflict
	if _, err := g.Merge(context.Background(), tg, "tok", PRMergeRequest{Method: MergeMethodMerge, HeadSHA: "old"}); !errors.Is(err, ErrPRHeadMoved) {
		t.Errorf("409: %v", err)
	}
	f.mergeErr = http.StatusMethodNotAllowed
	if _, err := g.Merge(context.Background(), tg, "tok", PRMergeRequest{Method: MergeMethodMerge}); !errors.Is(err, ErrPRNotMergeable) {
		t.Errorf("405: %v", err)
	}
}
