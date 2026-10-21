package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if _, _, err := ParsePRURL("42"); err == nil {
		t.Error("expected error parsing bare number as PR URL")
	}
	if _, _, err := ParsePRURL("https://example.com/foo/bar"); err == nil {
		t.Error("expected error for unsupported forge URL")
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
