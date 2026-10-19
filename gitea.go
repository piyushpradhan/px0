package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// gitea.go implements GitProvider for Gitea through its REST API (v1), which
// Forgejo and Codeberg serve unchanged. Gitea is nearly always self-hosted,
// so every target carries its own host.
//
// Where Gitea differs from GitHub:
//   - Review comments carry no reply link. Comments on the same line form one
//     conversation in Gitea's own UI, so px0 threads them the same way, and a
//     "reply" is a new comment on that line.
//   - There is no detailed mergeability: the PR is mergeable or it isn't.
//     Branch protection shows up as the merge being refused, with Gitea's
//     reason.
//   - Draft PRs are marked by a title prefix ("WIP:", "Draft:", ...).

// https://codeberg.org/owner/repo/pulls/12. GitHub's path is "pull",
// singular, so the two never overlap.
var giteaPRURLRe = regexp.MustCompile(`^(?:https?://)?([^/\s]+)/([^/\s]+)/([^/\s]+?)/pulls/(\d+)`)

// giteaAPIBase is the API root for a host; a variable so tests can point it
// at a fake server.
var giteaAPIBase = func(host string) string { return "https://" + host + "/api/v1" }

// giteaWIPPrefixes are Gitea's default work-in-progress title prefixes.
var giteaWIPPrefixes = []string{"wip:", "[wip]", "draft:", "[draft]"}

// GiteaProvider implements GitProvider for Gitea and Forgejo.
type GiteaProvider struct{}

func (g *GiteaProvider) Name() string { return "gitea" }

func (g *GiteaProvider) Info() ForgeInfo {
	return ForgeInfo{
		Name: "gitea", Label: "Gitea", TokenKey: "gitea.token", TokenCategory: "Gitea",
		TokenHint: "set GITEA_TOKEN (and GITEA_HOST for an instance other than codeberg.org)",
	}
}

func (g *GiteaProvider) TokenSetup(host string) TokenSetup {
	if host == "" {
		host = "codeberg.org"
	}
	return TokenSetup{
		URL: "https://" + host + "/user/settings/applications",
		Steps: []string{
			"Under Generate new token, name it (e.g. px0).",
			"Under Select permissions, set repository and issue to Read and write.",
			"Generate the token and copy it.",
		},
		Env:      "GITEA_TOKEN",
		HostKey:  "gitea.host",
		Readonly: "Public repositories open and diff without a token; posting comments, reviewing, pushing and merging need one.",
	}
}

func (g *GiteaProvider) MatchURL(rawURL string) bool {
	m := giteaPRURLRe.FindStringSubmatch(strings.TrimSpace(rawURL))
	return m != nil && !strings.EqualFold(m[1], "github.com")
}

func (g *GiteaProvider) ParseURL(rawURL string) (PRTarget, error) {
	if !g.MatchURL(rawURL) {
		return PRTarget{}, fmt.Errorf("invalid Gitea pull request URL: %q (expected format https://codeberg.org/owner/repo/pulls/12)", rawURL)
	}
	m := giteaPRURLRe.FindStringSubmatch(strings.TrimSpace(rawURL))
	n, _ := strconv.Atoi(m[4])
	return PRTarget{
		Provider: "gitea",
		Host:     strings.ToLower(m[1]),
		Owner:    m[2],
		Repo:     strings.TrimSuffix(m[3], ".git"),
		Number:   n,
		URL:      rawURL,
	}, nil
}

func (g *GiteaProvider) MatchRemote(remoteURL string) bool {
	host := remoteHost(remoteURL)
	return host != "" && (host == giteaTokenHost(readSettings()) || host == "codeberg.org" ||
		strings.Contains(host, "gitea") || strings.Contains(host, "forgejo"))
}

func (g *GiteaProvider) CloneURL(t PRTarget) string {
	return fmt.Sprintf("https://%s/%s/%s.git", t.Host, t.Owner, t.Repo)
}

func (g *GiteaProvider) PRHeadRef(t PRTarget) string {
	return fmt.Sprintf("refs/pull/%d/head", t.Number)
}

// Gitea reads the password as the token; the username is not checked.
func (g *GiteaProvider) GitAuth(t PRTarget, token string) gitAuth {
	return basicGitAuth(t.Host, "px0", token)
}

// giteaTokenHost is the one host the gitea.token setting and GITEA_TOKEN are
// sent to: the gitea.host setting, else GITEA_HOST, else codeberg.org. A PR
// URL can name any host, and a token must never reach one it wasn't issued by.
func giteaTokenHost(cfg settings) string {
	h := ""
	if cfg.GiteaHost != nil {
		h = strings.TrimSpace(*cfg.GiteaHost)
	}
	if h == "" {
		h = strings.TrimSpace(os.Getenv("GITEA_HOST"))
	}
	return normalizeHost(h, "codeberg.org")
}

func (g *GiteaProvider) ResolveToken(cfg settings, t PRTarget) (token, source string) {
	if t.Host != "" && t.Host != giteaTokenHost(cfg) {
		return "", ""
	}
	if cfg.GiteaToken != nil {
		if tok := strings.TrimSpace(*cfg.GiteaToken); tok != "" {
			return tok, "settings"
		}
	}
	if tok := strings.TrimSpace(os.Getenv("GITEA_TOKEN")); tok != "" {
		return tok, "env"
	}
	return "", ""
}

func giteaAPI(t PRTarget, token string) forgeAPI {
	a := forgeAPI{name: "gitea", base: giteaAPIBase(t.Host), message: giteaMessage}
	if token != "" {
		a.auth = "token " + token
	}
	return a
}

func giteaMessage(b []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil {
		return e.Message
	}
	return ""
}

func giteaRepoPath(t PRTarget) string {
	return "/repos/" + url.PathEscape(t.Owner) + "/" + url.PathEscape(t.Repo)
}

func giteaPRPath(t PRTarget) string { return fmt.Sprintf("%s/pulls/%d", giteaRepoPath(t), t.Number) }

type giteaUser struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

type giteaPR struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"` // open, closed
	Merged    bool      `json:"merged"`
	MergedAt  string    `json:"merged_at"`
	Draft     bool      `json:"draft"`
	Mergeable bool      `json:"mergeable"`
	User      giteaUser `json:"user"`
	Base      struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			CloneURL string `json:"clone_url"`
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

func (p giteaPR) draft() bool {
	if p.Draft {
		return true
	}
	title := strings.ToLower(strings.TrimSpace(p.Title))
	for _, pre := range giteaWIPPrefixes {
		if strings.HasPrefix(title, pre) {
			return true
		}
	}
	return false
}

func (g *GiteaProvider) fetchPR(ctx context.Context, t PRTarget, token string) (giteaPR, error) {
	var pr giteaPR
	code, err := giteaAPI(t, token).do(ctx, http.MethodGet, giteaPRPath(t), nil, &pr)
	if err != nil && token == "" && (code == http.StatusNotFound || code == http.StatusUnauthorized || code == http.StatusForbidden) {
		return pr, fmt.Errorf("%w (no Gitea token found for %s; for private repositories, %s)", err, t.Host, g.Info().TokenHint)
	}
	return pr, err
}

func (g *GiteaProvider) FetchPR(ctx context.Context, t PRTarget, token string) (PRMeta, error) {
	pr, err := g.fetchPR(ctx, t, token)
	if err != nil {
		return PRMeta{}, err
	}
	m := PRMeta{
		Number: pr.Number, Title: pr.Title, Author: pr.User.Login,
		State: pr.State, Merged: pr.Merged, MergedAt: pr.MergedAt, Draft: pr.draft(),
		BaseRef: pr.Base.Ref, HeadRef: pr.Head.Ref, HeadSHA: pr.Head.SHA,
	}
	if r := pr.Head.Repo; r != nil {
		m.HeadRepoCloneURL = r.CloneURL
		m.HeadIsFork = r.FullName != "" && !strings.EqualFold(r.FullName, t.Owner+"/"+t.Repo)
	}
	return m, nil
}

type giteaRepo struct {
	Permissions struct {
		Push bool `json:"push"`
	} `json:"permissions"`
	AllowMerge       *bool `json:"allow_merge_commits"`
	AllowRebase      *bool `json:"allow_rebase"`
	AllowSquash      *bool `json:"allow_squash_merge"`
	AllowFastForward *bool `json:"allow_fast_forward_only_merge"`
}

func (g *GiteaProvider) repo(ctx context.Context, t PRTarget, token string) (giteaRepo, error) {
	var r giteaRepo
	_, err := giteaAPI(t, token).do(ctx, http.MethodGet, giteaRepoPath(t), nil, &r)
	return r, err
}

func (g *GiteaProvider) CheckPushAccess(ctx context.Context, t PRTarget, token string) bool {
	if token == "" {
		return false
	}
	r, err := g.repo(ctx, t, token)
	return err == nil && r.Permissions.Push
}

// giteaEvents maps px0's review verdicts onto Gitea's review states.
var giteaEvents = map[string]string{"APPROVE": "APPROVED", "REQUEST_CHANGES": "REQUEST_CHANGES", "COMMENT": "COMMENT"}

// SubmitReview posts one review with every draft, like GitHub. Gitea
// addresses a line by its number on the new side (new_position) or the old
// one (old_position).
func (g *GiteaProvider) SubmitReview(ctx context.Context, t PRTarget, token, headSHA string, comments []prComment, event, body string) error {
	type reviewComment struct {
		Path        string `json:"path"`
		Body        string `json:"body"`
		NewPosition int    `json:"new_position,omitempty"`
		OldPosition int    `json:"old_position,omitempty"`
	}
	payload := struct {
		Body     string          `json:"body,omitempty"`
		Event    string          `json:"event"`
		CommitID string          `json:"commit_id,omitempty"`
		Comments []reviewComment `json:"comments,omitempty"`
	}{Body: body, Event: giteaEvents[event], CommitID: headSHA}
	for _, c := range comments {
		rc := reviewComment{Path: c.Path, Body: c.Body}
		if c.Side == "LEFT" {
			rc.OldPosition = c.Line
		} else {
			rc.NewPosition = c.Line
		}
		payload.Comments = append(payload.Comments, rc)
	}
	_, err := giteaAPI(t, token).do(ctx, http.MethodPost, giteaPRPath(t)+"/reviews", payload, nil)
	return err
}

// giteaPages fetches every page of a list endpoint into out, one decoded
// page at a time.
func giteaPages[T any](ctx context.Context, a forgeAPI, path string) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	const limit = 50
	var all []T
	for page := 1; page <= 100; page++ {
		var items []T
		if _, err := a.do(ctx, http.MethodGet, fmt.Sprintf("%s%slimit=%d&page=%d", path, sep, limit, page), nil, &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < limit {
			break
		}
	}
	return all, nil
}

type giteaComment struct {
	ID               int64     `json:"id"`
	Body             string    `json:"body"`
	User             giteaUser `json:"user"`
	CreatedAt        string    `json:"created_at"`
	HTMLURL          string    `json:"html_url"`
	Path             string    `json:"path"`
	Position         int       `json:"position"`
	OriginalPosition int       `json:"original_position"`
}

func (c giteaComment) toPRComment(kind string) PRComment {
	return PRComment{
		ID: c.ID, Kind: kind, Author: c.User.Login, AvatarURL: c.User.AvatarURL,
		Body: c.Body, CreatedAt: c.CreatedAt, URL: c.HTMLURL,
	}
}

// FetchComments reads top-level comments from the issue API and inline ones
// review by review. Inline comments on the same path and line become one
// thread rooted at the earliest, matching Gitea's own conversations. Gitea
// doesn't report which side a comment is on, so all are taken as the new side.
func (g *GiteaProvider) FetchComments(ctx context.Context, t PRTarget, token string) (issue, review []PRComment, err error) {
	a := giteaAPI(t, token)
	ic, err := giteaPages[giteaComment](ctx, a, fmt.Sprintf("%s/issues/%d/comments", giteaRepoPath(t), t.Number))
	if err != nil {
		return nil, nil, err
	}
	issue = make([]PRComment, 0, len(ic))
	for _, c := range ic {
		issue = append(issue, c.toPRComment("issue"))
	}
	reviews, err := giteaPages[struct {
		ID int64 `json:"id"`
	}](ctx, a, giteaPRPath(t)+"/reviews")
	if err != nil {
		return nil, nil, err
	}
	review = []PRComment{}
	roots := map[string]int64{}
	for _, r := range reviews {
		var cs []giteaComment
		if _, err := a.do(ctx, http.MethodGet, fmt.Sprintf("%s/reviews/%d/comments", giteaPRPath(t), r.ID), nil, &cs); err != nil {
			return nil, nil, err
		}
		for _, c := range cs {
			pc := c.toPRComment("review")
			pc.Path, pc.Line, pc.Side = c.Path, c.Position, "RIGHT"
			if pc.Line == 0 {
				pc.Line = c.OriginalPosition
			}
			review = append(review, pc)
		}
	}
	// Reviews arrive in order, but comments across them interleave: thread
	// by line after sorting oldest first.
	sortComments(review)
	for i := range review {
		key := fmt.Sprintf("%s:%d", review[i].Path, review[i].Line)
		if root, ok := roots[key]; ok {
			review[i].InReplyTo = root
		} else {
			roots[key] = review[i].ID
		}
	}
	return issue, review, nil
}

func (g *GiteaProvider) PostIssueComment(ctx context.Context, t PRTarget, token, body string) (PRComment, error) {
	var c giteaComment
	if _, err := giteaAPI(t, token).do(ctx, http.MethodPost, fmt.Sprintf("%s/issues/%d/comments", giteaRepoPath(t), t.Number), map[string]string{"body": body}, &c); err != nil {
		return PRComment{}, err
	}
	return c.toPRComment("issue"), nil
}

// ReplyToReviewComment posts a one-comment review on the same line as the
// comment being answered, which Gitea shows as a reply in that conversation.
func (g *GiteaProvider) ReplyToReviewComment(ctx context.Context, t PRTarget, token string, commentID int64, body string) (PRComment, error) {
	_, review, err := g.FetchComments(ctx, t, token)
	if err != nil {
		return PRComment{}, err
	}
	for _, c := range review {
		if c.ID != commentID {
			continue
		}
		root := c.ID
		if c.InReplyTo != 0 {
			root = c.InReplyTo
		}
		draft := []prComment{{Path: c.Path, Line: c.Line, Side: c.Side, Body: body}}
		if err := g.SubmitReview(ctx, t, token, "", draft, "COMMENT", ""); err != nil {
			return PRComment{}, err
		}
		return PRComment{Kind: "review", Path: c.Path, Line: c.Line, Side: c.Side, InReplyTo: root, Body: body, Author: "you"}, nil
	}
	return PRComment{}, fmt.Errorf("gitea: comment %d not found on pull request #%d", commentID, t.Number)
}

func giteaMergeState(pr giteaPR) (string, string) {
	switch {
	case pr.Merged:
		return MergeStateMerged, ""
	case pr.State == "closed":
		return MergeStateClosed, ""
	case pr.draft():
		return MergeStateDraft, "this pull request is marked work in progress"
	case pr.Mergeable:
		return MergeStateClean, ""
	}
	return MergeStateConflicts, "this branch has conflicts with " + pr.Base.Ref
}

// giteaMergeMethods lists the methods the repository allows. Absent fields
// (an older Gitea) mean Gitea's own default: allowed. Fast-forward-only is
// offered as MergeMethodRebase when plain rebase isn't allowed.
func giteaMergeMethods(r giteaRepo) []string {
	on := func(b *bool) bool { return b == nil || *b }
	methods := []string{}
	if on(r.AllowMerge) {
		methods = append(methods, MergeMethodMerge)
	}
	if on(r.AllowSquash) {
		methods = append(methods, MergeMethodSquash)
	}
	if on(r.AllowRebase) || (r.AllowFastForward != nil && *r.AllowFastForward) {
		methods = append(methods, MergeMethodRebase)
	}
	return methods
}

func (g *GiteaProvider) MergeStatus(ctx context.Context, t PRTarget, token string) (PRMergeStatus, error) {
	pr, err := g.fetchPR(ctx, t, token)
	if err != nil {
		return PRMergeStatus{}, err
	}
	st := PRMergeStatus{HeadSHA: pr.Head.SHA, Methods: []string{}}
	st.State, st.Reason = giteaMergeState(pr)
	if token == "" {
		return st, nil
	}
	if r, err := g.repo(ctx, t, token); err == nil {
		st.CanMerge = r.Permissions.Push
		st.Methods = giteaMergeMethods(r)
	}
	return st, nil
}

func (g *GiteaProvider) Merge(ctx context.Context, t PRTarget, token string, req PRMergeRequest) (PRMergeResult, error) {
	do := req.Method
	if req.Method == MergeMethodRebase {
		if r, err := g.repo(ctx, t, token); err == nil && r.AllowRebase != nil && !*r.AllowRebase {
			do = "fast-forward-only"
		}
	}
	payload := map[string]any{"Do": do}
	if req.HeadSHA != "" {
		payload["head_commit_id"] = req.HeadSHA
	}
	if req.Method != MergeMethodRebase {
		if req.Title != "" {
			payload["MergeTitleField"] = req.Title
		}
		if req.Message != "" {
			payload["MergeMessageField"] = req.Message
		}
	}
	code, err := giteaAPI(t, token).do(ctx, http.MethodPost, giteaPRPath(t)+"/merge", payload, nil)
	if err != nil {
		low := strings.ToLower(err.Error())
		switch {
		case code == http.StatusConflict && (strings.Contains(low, "out of date") || strings.Contains(low, "head")):
			return PRMergeResult{}, fmt.Errorf("%w: %v", ErrPRHeadMoved, err)
		case code == http.StatusConflict, code == http.StatusMethodNotAllowed, code == http.StatusUnprocessableEntity:
			return PRMergeResult{}, fmt.Errorf("%w: %v", ErrPRNotMergeable, err)
		}
		return PRMergeResult{}, err
	}
	// Gitea answers an empty 200; the merge commit is on the PR now.
	var pr struct {
		MergeCommitSHA string `json:"merge_commit_sha"`
	}
	giteaAPI(t, token).do(ctx, http.MethodGet, giteaPRPath(t), nil, &pr)
	return PRMergeResult{SHA: pr.MergeCommitSHA}, nil
}
