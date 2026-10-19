package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
