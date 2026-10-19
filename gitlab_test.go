package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

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
	cfg := settings{GitLabToken: &tok}
	g := &GitLabProvider{}
	if got, src := g.ResolveToken(cfg, PRTarget{Host: "gitlab.com"}); got != "settok" || src != "settings" {
		t.Errorf("gitlab.com: %q/%q", got, src)
	}
	if got, _ := g.ResolveToken(settings{}, PRTarget{Host: "gitlab.com"}); got != "envtok" {
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
