package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"px0/internal/settings"
)

// gitlab.go talks to the GitLab REST API (v4) for merge request review via
// the GitLabProvider implementation of GitProvider, on gitlab.com or any
// self-hosted instance. Same rules as github.go: net/http only, plus a
// shell-out to the glab CLI as one token source.
//
// GitLab's model differs from GitHub's in a few places this file absorbs:
//   - There is no single "submit review" call: draft comments become diff
//     discussions, an approval is its own endpoint, and "request changes" is
//     a note plus withdrawing your approval.
//   - Projects nest in groups, so PRTarget.Owner may be "group/sub", and API
//     calls address the project by its URL-encoded full path.
//   - Merge methods are project settings (merge commit, semi-linear, or
//     fast-forward) with squashing as a per-merge option on top.

var gitlabHTTPClient = &http.Client{Timeout: 15 * time.Second}

// gitlabAPIBase is the API root for a host; a variable so tests can point it
// at a fake server.
var gitlabAPIBase = func(host string) string { return "https://" + host + "/api/v4" }

// https://gitlab.com/group/sub/project/-/merge_requests/12: the /-/ path
// segment is GitLab's own, so any host using it is taken to be GitLab.
var gitlabMRURLRe = regexp.MustCompile(`^(?:https?://)?([^/\s]+)/(.+)/([^/\s]+?)/-/merge_requests/(\d+)`)

// GitLabProvider implements GitProvider for GitLab.
type GitLabProvider struct{}

func (g *GitLabProvider) Name() string { return "gitlab" }

func (g *GitLabProvider) Info() ForgeInfo {
	return ForgeInfo{
		Name: "gitlab", Label: "GitLab", TokenKey: "gitlab.token", TokenCategory: "GitLab",
		TokenHint: "set GITLAB_TOKEN or run `glab auth login`",
	}
}

func (g *GitLabProvider) TokenSetup(host string) TokenSetup {
	if host == "" {
		host = "gitlab.com"
	}
	return TokenSetup{
		URL: "https://" + host + "/-/user_settings/personal_access_tokens",
		Steps: []string{
			"Select Add new token, name it (e.g. px0) and pick an expiration date.",
			"Under Select scopes, check api.",
			"Create the token and copy it.",
		},
		CLI:      "glab auth login",
		Env:      "GITLAB_TOKEN",
		HostKey:  "gitlab.host",
		Readonly: "Public projects open and diff without a token; posting comments, approving, pushing and merging need one.",
	}
}

func (g *GitLabProvider) MatchURL(rawURL string) bool {
	return gitlabMRURLRe.MatchString(strings.TrimSpace(rawURL))
}

func (g *GitLabProvider) ParseURL(rawURL string) (PRTarget, error) {
	m := gitlabMRURLRe.FindStringSubmatch(strings.TrimSpace(rawURL))
	if m == nil {
		return PRTarget{}, fmt.Errorf("invalid GitLab merge request URL: %q (expected format https://gitlab.com/group/project/-/merge_requests/12)", rawURL)
	}
	n, _ := strconv.Atoi(m[4])
	return PRTarget{
		Provider: "gitlab",
		Host:     strings.ToLower(m[1]),
		Owner:    m[2],
		Repo:     strings.TrimSuffix(m[3], ".git"),
		Number:   n,
		URL:      rawURL,
	}, nil
}

func (g *GitLabProvider) MatchRemote(remoteURL string) bool {
	host := remoteHost(remoteURL)
	return host != "" && (host == gitlabTokenHost() || strings.Contains(host, "gitlab"))
}

func (g *GitLabProvider) CloneURL(target PRTarget) string {
	return fmt.Sprintf("https://%s/%s/%s.git", target.Host, target.Owner, target.Repo)
}

func (g *GitLabProvider) PRHeadRef(target PRTarget) string {
	return fmt.Sprintf("refs/merge-requests/%d/head", target.Number)
}

// GitLab takes any username with a token as the password; "oauth2" is the
// one it documents, and it works for personal access tokens too.
func (g *GitLabProvider) GitAuth(target PRTarget, token string) gitAuth {
	return basicGitAuth(target.Host, "oauth2", token)
}

// gitlabTokenHost is the one host the gitlab.token setting and the
// GITLAB_TOKEN variable are sent to: the gitlab.host setting, else
// GITLAB_HOST (glab's own convention), else gitlab.com. A merge request URL
// can name any host, and a token must never reach one it wasn't issued by.
func gitlabTokenHost() string {
	cfg := settings.Read()
	h := ""
	if cfg.GitLabHost != nil {
		h = strings.TrimSpace(*cfg.GitLabHost)
	}
	if h == "" {
		h = strings.TrimSpace(os.Getenv("GITLAB_HOST"))
	}
	return normalizeHost(h, "gitlab.com")
}

// ResolveToken looks, in order, for the gitlab.token setting and then
// GITLAB_TOKEN (both only for gitlabTokenHost), then glab's stored token for
// the target's own host. Empty means the review stays read-only.
func (g *GitLabProvider) ResolveToken(cfg settings.Settings, target PRTarget) (token, source string) {
	host := target.Host
	if host == "" {
		host = "gitlab.com"
	}
	if host == gitlabTokenHost() {
		if cfg.GitLabToken != nil {
			if t := strings.TrimSpace(*cfg.GitLabToken); t != "" {
				return t, "settings"
			}
		}
		if t := strings.TrimSpace(os.Getenv("GITLAB_TOKEN")); t != "" {
			return t, "env"
		}
	}
	if out, err := exec.Command("glab", "config", "get", "token", "--host", host).Output(); err == nil {
		if t := strings.TrimSpace(string(out)); t != "" {
			return t, "glab"
		}
	}
	return "", ""
}

// gitlabProject is the project path as the API addresses it.
func gitlabProject(t PRTarget) string { return url.PathEscape(t.Owner + "/" + t.Repo) }

func gitlabMRPath(t PRTarget) string {
	return fmt.Sprintf("/projects/%s/merge_requests/%d", gitlabProject(t), t.Number)
}

// gitlabRequest issues an authenticated (if token != "") GitLab REST call.
// path is relative to the host's API root, or an absolute URL (a paginated
// Link header's "next").
func gitlabRequest(ctx context.Context, method, host, path, token string, body any) (*http.Response, error) {
	u := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		u = gitlabAPIBase(host) + path
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		// Bearer covers both personal access tokens and glab's OAuth tokens.
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return gitlabHTTPClient.Do(req)
}

// gitlabDo runs one request and decodes a 2xx reply into out (if non-nil).
// Anything else becomes an error carrying GitLab's message.
func gitlabDo(ctx context.Context, method string, t PRTarget, path, token string, body, out any) (int, error) {
	resp, err := gitlabRequest(ctx, method, t.Host, path, token, body)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return resp.StatusCode, fmt.Errorf("gitlab: %s %s: %s: %s", method, path, resp.Status, gitlabMessage(b))
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// gitlabMessage pulls the human-readable part out of an error body, whose
// "message" may be a string, a list, or a map of field errors.
func gitlabMessage(b []byte) string {
	var e struct {
		Message any    `json:"message"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil {
		switch m := e.Message.(type) {
		case string:
			return m
		case nil:
			if e.Error != "" {
				return e.Error
			}
		default:
			if j, err := json.Marshal(m); err == nil {
				return string(j)
			}
		}
	}
	return strings.TrimSpace(string(b))
}

func gitlabGetAllPages(ctx context.Context, t PRTarget, path, token string) ([]json.RawMessage, error) {
	var all []json.RawMessage
	next := path
	for next != "" {
		resp, err := gitlabRequest(ctx, http.MethodGet, t.Host, next, token, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			return nil, fmt.Errorf("gitlab: get %s: %s: %s", path, resp.Status, gitlabMessage(b))
		}
		var page []json.RawMessage
		err = json.NewDecoder(resp.Body).Decode(&page)
		next = ""
		if m := githubLinkNextRe.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			next = m[1] // same RFC 8288 Link format GitHub uses
		}
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
	}
	return all, nil
}

// glMR is the subset of a merge request px0 reads.
type glMR struct {
	IID                 int    `json:"iid"`
	Title               string `json:"title"`
	State               string `json:"state"` // opened, closed, locked, merged
	MergedAt            string `json:"merged_at"`
	Draft               bool   `json:"draft"`
	WorkInProgress      bool   `json:"work_in_progress"` // pre-15.x name for draft
	SHA                 string `json:"sha"`
	SourceBranch        string `json:"source_branch"`
	TargetBranch        string `json:"target_branch"`
	SourceProjectID     int64  `json:"source_project_id"`
	TargetProjectID     int64  `json:"target_project_id"`
	DetailedMergeStatus string `json:"detailed_merge_status"`
	MergeStatus         string `json:"merge_status"`
	HasConflicts        bool   `json:"has_conflicts"`
	WebURL              string `json:"web_url"`
	Author              struct {
		Username string `json:"username"`
	} `json:"author"`
	User struct {
		CanMerge bool `json:"can_merge"`
	} `json:"user"`
	DiffRefs struct {
		BaseSHA  string `json:"base_sha"`
		HeadSHA  string `json:"head_sha"`
		StartSHA string `json:"start_sha"`
	} `json:"diff_refs"`
}

func (g *GitLabProvider) fetchMR(ctx context.Context, t PRTarget, token string) (glMR, error) {
	var mr glMR
	code, err := gitlabDo(ctx, http.MethodGet, t, gitlabMRPath(t), token, nil, &mr)
	if err != nil && token == "" && (code == http.StatusNotFound || code == http.StatusUnauthorized || code == http.StatusForbidden) {
		return mr, fmt.Errorf("%w (no GitLab token found; for private projects, %s)", err, g.Info().TokenHint)
	}
	return mr, err
}

func (g *GitLabProvider) FetchPR(ctx context.Context, t PRTarget, token string) (PRMeta, error) {
	mr, err := g.fetchMR(ctx, t, token)
	if err != nil {
		return PRMeta{}, err
	}
	m := PRMeta{
		Number:   mr.IID,
		Title:    mr.Title,
		Author:   mr.Author.Username,
		State:    "open",
		Merged:   mr.State == "merged",
		MergedAt: mr.MergedAt,
		Draft:    mr.Draft || mr.WorkInProgress,
		BaseRef:  mr.TargetBranch,
		HeadRef:  mr.SourceBranch,
		HeadSHA:  mr.SHA,
	}
	if mr.State != "opened" {
		m.State = "closed"
	}
	// A fork's commits live in its own project; px0 pushes there.
	if mr.SourceProjectID != 0 && mr.SourceProjectID != mr.TargetProjectID {
		m.HeadIsFork = true
		var src struct {
			HTTPURL string `json:"http_url_to_repo"`
		}
		if _, err := gitlabDo(ctx, http.MethodGet, t, fmt.Sprintf("/projects/%d", mr.SourceProjectID), token, nil, &src); err == nil {
			m.HeadRepoCloneURL = src.HTTPURL
		}
	}
	if m.HeadRepoCloneURL == "" {
		m.HeadRepoCloneURL = g.CloneURL(t)
	}
	return m, nil
}

type glProject struct {
	MergeMethod  string `json:"merge_method"`  // merge, rebase_merge, ff
	SquashOption string `json:"squash_option"` // never, always, default_on, default_off
	Permissions  struct {
		ProjectAccess *struct {
			AccessLevel int `json:"access_level"`
		} `json:"project_access"`
		GroupAccess *struct {
			AccessLevel int `json:"access_level"`
		} `json:"group_access"`
	} `json:"permissions"`
}

// gitlabDeveloper is GitLab's Developer access level: enough to push to the
// project, approve, and (branch protection permitting) merge.
const gitlabDeveloper = 30

// CheckPushAccess fails closed, like checkPushAccess for GitHub.
func (g *GitLabProvider) CheckPushAccess(ctx context.Context, t PRTarget, token string) bool {
	if token == "" {
		return false
	}
	var p glProject
	if _, err := gitlabDo(ctx, http.MethodGet, t, "/projects/"+gitlabProject(t), token, nil, &p); err != nil {
		return false
	}
	level := 0
	if a := p.Permissions.ProjectAccess; a != nil && a.AccessLevel > level {
		level = a.AccessLevel
	}
	if a := p.Permissions.GroupAccess; a != nil && a.AccessLevel > level {
		level = a.AccessLevel
	}
	return level >= gitlabDeveloper
}

// SubmitReview maps px0's one-shot review onto GitLab's separate calls: each
// draft becomes a diff discussion (or, where GitLab can't anchor it -- an
// unchanged context line needs both old and new line numbers, which px0's
// drafts don't carry -- a note naming path:line), the body a note, and the
// verdict an approval or, for REQUEST_CHANGES, a withdrawn approval.
func (g *GitLabProvider) SubmitReview(ctx context.Context, t PRTarget, token, headSHA string, comments []prComment, event, body string) error {
	if len(comments) > 0 {
		mr, err := g.fetchMR(ctx, t, token)
		if err != nil {
			return err
		}
		for _, c := range comments {
			pos := map[string]any{
				"position_type": "text",
				"base_sha":      mr.DiffRefs.BaseSHA,
				"start_sha":     mr.DiffRefs.StartSHA,
				"head_sha":      mr.DiffRefs.HeadSHA,
				"old_path":      c.Path,
				"new_path":      c.Path,
			}
			if c.Side == "LEFT" {
				pos["old_line"] = c.Line
			} else {
				pos["new_line"] = c.Line
			}
			_, err := gitlabDo(ctx, http.MethodPost, t, gitlabMRPath(t)+"/discussions", token, map[string]any{"body": c.Body, "position": pos}, nil)
			if err != nil {
				loc := fmt.Sprintf("`%s:%d`", c.Path, c.Line)
				if c.Side == "LEFT" {
					loc += " (base)"
				}
				if _, err := gitlabDo(ctx, http.MethodPost, t, gitlabMRPath(t)+"/notes", token, map[string]string{"body": loc + "\n\n" + c.Body}, nil); err != nil {
					return err
				}
			}
		}
	}
	note := strings.TrimSpace(body)
	if event == "REQUEST_CHANGES" {
		note = strings.TrimSpace("**Changes requested**\n\n" + note)
	}
	if note != "" {
		if _, err := gitlabDo(ctx, http.MethodPost, t, gitlabMRPath(t)+"/notes", token, map[string]string{"body": note}, nil); err != nil {
			return err
		}
	}
	switch event {
	case "APPROVE":
		payload := map[string]string{}
		if headSHA != "" {
			payload["sha"] = headSHA // GitLab refuses if the MR moved past what was reviewed
		}
		if code, err := gitlabDo(ctx, http.MethodPost, t, gitlabMRPath(t)+"/approve", token, payload, nil); err != nil {
			if code == http.StatusUnauthorized {
				return fmt.Errorf("gitlab: you can't approve this merge request (already approved, or not an eligible approver): %w", err)
			}
			return err
		}
	case "REQUEST_CHANGES":
		// Withdraw an earlier approval so the verdict isn't contradicted.
		// Not having approved is the common case and not an error.
		gitlabDo(ctx, http.MethodPost, t, gitlabMRPath(t)+"/unapprove", token, nil, nil)
	}
	return nil
}

type glNote struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	System    bool   `json:"system"`
	CreatedAt string `json:"created_at"`
	Author    struct {
		Username  string `json:"username"`
		AvatarURL string `json:"avatar_url"`
	} `json:"author"`
	Position *struct {
		PositionType string `json:"position_type"`
		NewPath      string `json:"new_path"`
		OldPath      string `json:"old_path"`
		NewLine      int    `json:"new_line"`
		OldLine      int    `json:"old_line"`
	} `json:"position"`
}

type glDiscussion struct {
	ID    string   `json:"id"`
	Notes []glNote `json:"notes"`
}

func (n glNote) toPRComment(kind, mrURL string) PRComment {
	c := PRComment{
		ID: n.ID, Kind: kind, Author: n.Author.Username, AvatarURL: n.Author.AvatarURL,
		Body: n.Body, CreatedAt: n.CreatedAt,
	}
	if mrURL != "" {
		c.URL = fmt.Sprintf("%s#note_%d", mrURL, n.ID)
	}
	if p := n.Position; p != nil {
		c.Path, c.Line, c.Side = p.NewPath, p.NewLine, "RIGHT"
		if p.NewLine == 0 {
			c.Path, c.Line, c.Side = p.OldPath, p.OldLine, "LEFT"
		}
	}
	return c
}

func (g *GitLabProvider) discussions(ctx context.Context, t PRTarget, token string) ([]glDiscussion, error) {
	raw, err := gitlabGetAllPages(ctx, t, gitlabMRPath(t)+"/discussions?per_page=100", token)
	if err != nil {
		return nil, err
	}
	out := make([]glDiscussion, 0, len(raw))
	for _, r := range raw {
		var d glDiscussion
		if json.Unmarshal(r, &d) == nil {
			out = append(out, d)
		}
	}
	return out, nil
}

// FetchComments flattens discussions: one anchored to a diff line is a
// review thread (its replies point at the first note), anything else --
// including a threaded general discussion, which px0 shows flat -- is a list
// of top-level comments. GitLab's system notes ("added 2 commits") are skipped.
func (g *GitLabProvider) FetchComments(ctx context.Context, t PRTarget, token string) (issue, review []PRComment, err error) {
	ds, err := g.discussions(ctx, t, token)
	if err != nil {
		return nil, nil, err
	}
	mrURL := strings.TrimSuffix(t.URL, "/")
	if i := strings.Index(mrURL, "/-/merge_requests/"); i >= 0 {
		mrURL = fmt.Sprintf("%s/-/merge_requests/%d", mrURL[:i], t.Number)
	}
	issue, review = []PRComment{}, []PRComment{}
	for _, d := range ds {
		var notes []glNote
		for _, n := range d.Notes {
			if !n.System {
				notes = append(notes, n)
			}
		}
		if len(notes) == 0 {
			continue
		}
		if notes[0].Position == nil || notes[0].Position.PositionType != "text" {
			for _, n := range notes {
				issue = append(issue, n.toPRComment("issue", mrURL))
			}
			continue
		}
		root := notes[0].toPRComment("review", mrURL)
		review = append(review, root)
		for _, n := range notes[1:] {
			c := n.toPRComment("review", mrURL)
			c.Path, c.Line, c.Side, c.InReplyTo = root.Path, root.Line, root.Side, root.ID
			review = append(review, c)
		}
	}
	return issue, review, nil
}

func (g *GitLabProvider) PostIssueComment(ctx context.Context, t PRTarget, token, body string) (PRComment, error) {
	var n glNote
	if _, err := gitlabDo(ctx, http.MethodPost, t, gitlabMRPath(t)+"/notes", token, map[string]string{"body": body}, &n); err != nil {
		return PRComment{}, err
	}
	return n.toPRComment("issue", ""), nil
}

// ReplyToReviewComment replies within the discussion the note belongs to.
// GitLab addresses replies by discussion, not note, so the discussion is
// looked up first.
func (g *GitLabProvider) ReplyToReviewComment(ctx context.Context, t PRTarget, token string, commentID int64, body string) (PRComment, error) {
	ds, err := g.discussions(ctx, t, token)
	if err != nil {
		return PRComment{}, err
	}
	for _, d := range ds {
		for _, n := range d.Notes {
			if n.ID != commentID {
				continue
			}
			root := d.Notes[0].toPRComment("review", "")
			var reply glNote
			if _, err := gitlabDo(ctx, http.MethodPost, t, gitlabMRPath(t)+"/discussions/"+url.PathEscape(d.ID)+"/notes", token, map[string]string{"body": body}, &reply); err != nil {
				return PRComment{}, err
			}
			c := reply.toPRComment("review", "")
			c.Path, c.Line, c.Side, c.InReplyTo = root.Path, root.Line, root.Side, root.ID
			return c, nil
		}
	}
	return PRComment{}, fmt.Errorf("gitlab: comment %d not found on merge request !%d", commentID, t.Number)
}

// gitlabMergeState maps GitLab's detailed_merge_status (15.6+), or the older
// merge_status/has_conflicts pair, onto the forge-neutral MergeState* values.
func gitlabMergeState(mr glMR) (string, string) {
	switch {
	case mr.State == "merged":
		return MergeStateMerged, ""
	case mr.State != "opened":
		return MergeStateClosed, ""
	case mr.Draft || mr.WorkInProgress:
		return MergeStateDraft, "this merge request is still a draft"
	}
	base := mr.TargetBranch
	switch mr.DetailedMergeStatus {
	case "mergeable":
		return MergeStateClean, ""
	case "conflict":
		return MergeStateConflicts, "this branch has conflicts with " + base
	case "need_rebase":
		return MergeStateBehind, "the project requires this branch to be rebased onto " + base
	case "draft_status":
		return MergeStateDraft, "this merge request is still a draft"
	case "checking", "unchecked", "preparing", "approvals_syncing":
		return MergeStateUnknown, ""
	case "not_open":
		return MergeStateClosed, ""
	case "ci_must_pass":
		return MergeStateBlocked, "the pipeline must succeed first"
	case "ci_still_running":
		return MergeStateBlocked, "the pipeline is still running"
	case "not_approved":
		return MergeStateBlocked, "required approvals are missing"
	case "discussions_not_resolved":
		return MergeStateBlocked, "all threads must be resolved first"
	case "requested_changes":
		return MergeStateBlocked, "a reviewer requested changes"
	case "blocked_status":
		return MergeStateBlocked, "blocked by another merge request"
	case "":
		// Older GitLab: fall back to merge_status below.
	default:
		return MergeStateBlocked, strings.ReplaceAll(mr.DetailedMergeStatus, "_", " ")
	}
	switch {
	case mr.HasConflicts, mr.MergeStatus == "cannot_be_merged", mr.MergeStatus == "cannot_be_merged_recheck":
		return MergeStateConflicts, "this branch has conflicts with " + base
	case mr.MergeStatus == "can_be_merged":
		return MergeStateClean, ""
	}
	return MergeStateUnknown, ""
}

// gitlabMergeMethods derives the methods a merge can use from the project's
// merge method (merge commit, semi-linear merge commit, or fast-forward)
// and squash setting. Fast-forward is offered as MergeMethodRebase: the
// branch's commits land as they are, with no merge commit -- though unlike
// GitHub, GitLab won't rebase them itself (that is "need_rebase", which the
// UI turns into Update branch).
func gitlabMergeMethods(p glProject) []string {
	plain := MergeMethodMerge
	if p.MergeMethod == "ff" {
		plain = MergeMethodRebase
	}
	methods := []string{}
	if p.SquashOption != "always" && plain == MergeMethodMerge {
		methods = append(methods, MergeMethodMerge)
	}
	if p.SquashOption != "never" {
		methods = append(methods, MergeMethodSquash)
	}
	if p.SquashOption != "always" && plain == MergeMethodRebase {
		methods = append(methods, MergeMethodRebase)
	}
	return methods
}

func (g *GitLabProvider) MergeStatus(ctx context.Context, t PRTarget, token string) (PRMergeStatus, error) {
	mr, err := g.fetchMR(ctx, t, token)
	if err != nil {
		return PRMergeStatus{}, err
	}
	st := PRMergeStatus{HeadSHA: mr.SHA, Methods: []string{}, CanMerge: mr.User.CanMerge}
	st.State, st.Reason = gitlabMergeState(mr)
	if token == "" {
		return st, nil
	}
	var p glProject
	if _, err := gitlabDo(ctx, http.MethodGet, t, "/projects/"+gitlabProject(t), token, nil, &p); err == nil {
		st.Methods = gitlabMergeMethods(p)
	}
	return st, nil
}

// Merge accepts the merge request. GitLab takes one commit message, so the
// title and body are joined; with squash, the merge commit (if the project
// makes one) keeps GitLab's own default message.
func (g *GitLabProvider) Merge(ctx context.Context, t PRTarget, token string, req PRMergeRequest) (PRMergeResult, error) {
	payload := map[string]any{"squash": req.Method == MergeMethodSquash}
	if req.HeadSHA != "" {
		payload["sha"] = req.HeadSHA
	}
	msg := strings.TrimSpace(req.Title)
	if b := strings.TrimSpace(req.Message); b != "" && msg != "" {
		msg += "\n\n" + b
	}
	if msg != "" {
		switch req.Method {
		case MergeMethodSquash:
			payload["squash_commit_message"] = msg
		case MergeMethodMerge:
			payload["merge_commit_message"] = msg
		}
	}
	var out struct {
		MergeCommitSHA  string `json:"merge_commit_sha"`
		SquashCommitSHA string `json:"squash_commit_sha"`
		SHA             string `json:"sha"`
	}
	code, err := gitlabDo(ctx, http.MethodPut, t, gitlabMRPath(t)+"/merge", token, payload, &out)
	if err != nil {
		switch code {
		case http.StatusConflict:
			return PRMergeResult{}, fmt.Errorf("%w: %v", ErrPRHeadMoved, err)
		case http.StatusMethodNotAllowed, http.StatusNotAcceptable, http.StatusUnprocessableEntity:
			return PRMergeResult{}, fmt.Errorf("%w: %v", ErrPRNotMergeable, err)
		}
		return PRMergeResult{}, err
	}
	sha := out.MergeCommitSHA
	if sha == "" {
		sha = out.SquashCommitSHA
	}
	if sha == "" {
		sha = out.SHA // fast-forward: the head itself
	}
	return PRMergeResult{SHA: sha}, nil
}
