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
	if tok, _ := g.ResolveToken(settings{}, PRTarget{Host: "codeberg.org"}); tok != "envtok" {
		t.Errorf("codeberg default: %q", tok)
	}
	if tok, _ := g.ResolveToken(settings{}, PRTarget{Host: "evil.example"}); tok != "" {
		t.Errorf("token leaked to another host: %q", tok)
	}
	host, tok := "https://git.corp.example/", "settok"
	cfg := settings{GiteaHost: &host, GiteaToken: &tok}
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
