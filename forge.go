package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"px0/internal/settings"
)

// PRTarget identifies a pull request or merge request across any git forge.
type PRTarget struct {
	Provider string // "github", "gitlab", etc.
	Host     string // the forge's web host, e.g. "github.com" or a self-hosted "gitlab.example.com"
	Owner    string // namespace/owner; GitLab groups nest, so this may contain "/"
	Repo     string // project/repo name
	Number   int    // PR/MR number
	URL      string // original URL
}

// PRMeta holds the normalized metadata px0 needs to check out a PR,
// compute diffs, and label the review UI.
type PRMeta struct {
	Number           int
	Title            string
	Author           string
	State            string
	Merged           bool
	MergedAt         string
	Draft            bool
	BaseRef          string
	HeadRef          string
	HeadSHA          string
	HeadRepoCloneURL string
	HeadIsFork       bool
}

// PRComment is a comment already posted on the pull request, fetched
// read-only from the forge -- distinct from pr.go's prComment, which is a
// draft held in memory until a review is submitted. Kind is "issue" (a
// top-level PR conversation comment) or "review" (anchored to a diff line).
type PRComment struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Path      string `json:"path,omitempty"`
	Line      int    `json:"line,omitempty"`
	Side      string `json:"side,omitempty"`
	InReplyTo int64  `json:"inReplyTo,omitempty"`
	Author    string `json:"author"`
	AvatarURL string `json:"avatarUrl,omitempty"`
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
	URL       string `json:"url"`
}

// Merge methods, named for what lands on the base branch rather than for any
// one forge's API. Each provider maps them onto its own vocabulary (GitHub's
// merge/squash/rebase, GitLab's squash flag plus merge-method setting,
// Bitbucket's merge_commit/squash/fast_forward, Gitea's Do field).
const (
	MergeMethodMerge  = "merge"  // a merge commit joining the head into the base
	MergeMethodSquash = "squash" // the PR's commits squashed into one commit
	MergeMethodRebase = "rebase" // each commit replayed onto the base, no merge commit
)

// Merge states, normalized across forges. PRMergeStatus.State is one of these.
const (
	MergeStateClean     = "clean"     // mergeable, nothing in the way
	MergeStateUnstable  = "unstable"  // mergeable, but some non-required checks are failing
	MergeStateConflicts = "conflicts" // the head conflicts with the base branch
	MergeStateBehind    = "behind"    // no conflicts, but the base requires the head to be up to date
	MergeStateBlocked   = "blocked"   // branch protection (required reviews or checks) refuses it
	MergeStateDraft     = "draft"     // still a draft
	MergeStateUnknown   = "unknown"   // the forge hasn't computed it yet; ask again shortly
	MergeStateClosed    = "closed"
	MergeStateMerged    = "merged"
)

// PRMergeStatus is whether, and how, the PR can be merged right now.
type PRMergeStatus struct {
	State    string   `json:"state"`
	Reason   string   `json:"reason,omitempty"` // the forge's own detail, when it gives one
	Methods  []string `json:"methods"`          // allowed on this repository, in the forge's display order
	CanMerge bool     `json:"canMerge"`         // the token has the rights to merge
	HeadSHA  string   `json:"headSha"`          // the head the status was computed for
}

// PRMergeRequest is one merge, as the reviewer confirmed it.
type PRMergeRequest struct {
	Method  string
	Title   string // commit title; ignored for MergeMethodRebase
	Message string // commit body; ignored for MergeMethodRebase
	HeadSHA string // the head the reviewer saw; a PR that has moved past it must not be merged
}

// PRMergeResult is what the forge reports back about a completed merge.
type PRMergeResult struct {
	SHA     string `json:"sha"`
	Message string `json:"message,omitempty"`
}

// Errors a provider's Merge wraps, so callers can tell a stale head or a PR
// that can't merge from a transport failure without parsing forge text.
var (
	ErrPRHeadMoved    = errors.New("the PR head moved since it was loaded")
	ErrPRNotMergeable = errors.New("the PR cannot be merged")
)

// gitAuth is how plain git authenticates to a forge over https: one extra
// header, scoped to the forge's URL prefix and passed in the environment
// (gitAuthCmd), so the token never appears in argv, remote config or URLs.
// The zero value is no auth.
type gitAuth struct {
	Prefix string // URL prefix the header applies to, e.g. "https://gitlab.com/"
	Header string // e.g. "Authorization: Basic ..."
	Token  string // the raw token, only to redact it from git's error text
}

// basicGitAuth is the https Basic auth most forges accept for git with a
// token as the password; user is whatever name the forge expects with it.
func basicGitAuth(host, user, token string) gitAuth {
	if token == "" {
		return gitAuth{}
	}
	return gitAuth{
		Prefix: "https://" + host + "/",
		Header: "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"+token)),
		Token:  token,
	}
}

// normalizeHost turns a configured host ("https://git.example.com/" or
// "git.example.com") into a bare lowercase host, or def when it is empty.
func normalizeHost(h, def string) string {
	h = strings.TrimSpace(h)
	if h == "" {
		return def
	}
	if u, err := url.Parse(h); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return strings.ToLower(strings.TrimSuffix(h, "/"))
}

// redact strips the token from text git printed.
func (a gitAuth) redact(s string) string { return redactToken(s, a.Token) }

// ForgeInfo is what the UI needs to name the forge and send the user to the
// right token setting.
type ForgeInfo struct {
	Name          string `json:"name"`          // provider name, e.g. "gitlab"
	Label         string `json:"label"`         // display name, e.g. "GitLab"
	TokenKey      string `json:"tokenKey"`      // the px0 setting holding its token
	TokenCategory string `json:"tokenCategory"` // that setting's Settings category
	TokenHint     string `json:"tokenHint"`     // where else a token is picked up from
}

// TokenSetup walks someone through getting a token for one forge host: the
// page that creates it, what to pick there, and the settings px0 stores it
// under. It drives the Connect dialog, so it must name exactly what the
// forge's own page calls things.
type TokenSetup struct {
	URL      string      `json:"url"`                // the forge page that creates a token
	Steps    []string    `json:"steps"`              // what to choose on that page, in order
	CLI      string      `json:"cli,omitempty"`      // a CLI login px0 also reads, e.g. "gh auth login"
	Env      string      `json:"env"`                // the environment variable px0 also reads
	HostKey  string      `json:"hostKey,omitempty"`  // setting naming the host the token belongs to, for forges that have many
	Extra    *TokenField `json:"extra,omitempty"`    // a second value some tokens need (Bitbucket's account email)
	Readonly string      `json:"readonly,omitempty"` // what works without any token
}

// TokenField is one more input the Connect dialog asks for.
type TokenField struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Placeholder string `json:"placeholder"`
	Hint        string `json:"hint"`
}

// GitProvider abstracts forge-specific operations (GitHub, GitLab, etc.)
// for pull/merge request reviews.
type GitProvider interface {
	// Name returns the provider name (e.g. "github", "gitlab").
	Name() string

	// Info names the forge and its token setting for the UI.
	Info() ForgeInfo

	// TokenSetup says how to create a token for host (the forge's own host
	// for a single-host forge).
	TokenSetup(host string) TokenSetup

	// MatchRemote reports whether a git remote URL points at this forge, so
	// a plain workspace knows which token it would need.
	MatchRemote(remoteURL string) bool

	// CloneURL is the https clone URL of the target's repository.
	CloneURL(target PRTarget) string

	// PRHeadRef is the ref the forge publishes the PR head under
	// (refs/pull/N/head, refs/merge-requests/N/head), or "" when it has none
	// and the head branch itself must be fetched.
	PRHeadRef(target PRTarget) string

	// GitAuth is how git authenticates to the forge with token.
	GitAuth(target PRTarget, token string) gitAuth

	// MatchURL reports whether this provider recognizes and handles the given URL.
	MatchURL(rawURL string) bool

	// ParseURL extracts the PR target from the URL.
	ParseURL(rawURL string) (PRTarget, error)

	// ResolveToken looks for an auth token across settings, env vars, and CLI tools.
	// An empty return means the session stays read-only.
	ResolveToken(cfg settings.Settings, target PRTarget) (token, source string)

	// FetchPR fetches pull/merge request metadata from the forge API.
	FetchPR(ctx context.Context, target PRTarget, token string) (PRMeta, error)

	// CheckPushAccess reports whether the authenticated user has push access to the repository.
	CheckPushAccess(ctx context.Context, target PRTarget, token string) bool

	// SubmitReview posts draft comments and the overall review verdict back to the forge.
	SubmitReview(ctx context.Context, target PRTarget, token, headSHA string, comments []prComment, event, body string) error

	// FetchComments returns every comment already posted on the PR: top-level
	// ("issue") comments and inline ("review") comments anchored to a diff line.
	FetchComments(ctx context.Context, target PRTarget, token string) (issue, review []PRComment, err error)

	// PostIssueComment posts a new top-level PR comment immediately. GitHub has
	// no threading for these, so "replying" to one is just posting a new one.
	PostIssueComment(ctx context.Context, target PRTarget, token, body string) (PRComment, error)

	// ReplyToReviewComment posts an immediate, threaded reply to an existing
	// inline review comment.
	ReplyToReviewComment(ctx context.Context, target PRTarget, token string, commentID int64, body string) (PRComment, error)

	// MergeStatus reports whether the PR can merge right now and which merge
	// methods the repository allows. A forge that computes mergeability
	// lazily reports MergeStateUnknown until it has.
	MergeStatus(ctx context.Context, target PRTarget, token string) (PRMergeStatus, error)

	// Merge merges the PR on the forge. It must refuse, wrapping
	// ErrPRHeadMoved, when the PR head is no longer req.HeadSHA, and wrap
	// ErrPRNotMergeable when the forge refuses the merge itself.
	Merge(ctx context.Context, target PRTarget, token string, req PRMergeRequest) (PRMergeResult, error)
}

var defaultProviders = []GitProvider{
	&GitHubProvider{},
	&GitLabProvider{},
	&BitbucketProvider{},
	// Last: its URL shape (host/owner/repo/pulls/N) is the loosest.
	&GiteaProvider{},
}

// sortComments orders comments oldest first (ID breaking ties), the order
// threads are built in.
func sortComments(cs []PRComment) {
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].CreatedAt != cs[j].CreatedAt {
			return cs[i].CreatedAt < cs[j].CreatedAt
		}
		return cs[i].ID < cs[j].ID
	})
}

// ForgeForRemote returns the provider a git remote URL belongs to, or nil.
func ForgeForRemote(remoteURL string) GitProvider {
	for _, p := range defaultProviders {
		if p.MatchRemote(remoteURL) {
			return p
		}
	}
	return nil
}

// remoteHost is the host of an https, ssh or scp-style remote URL, lowercased.
func remoteHost(remoteURL string) string {
	u := strings.TrimSpace(remoteURL)
	if strings.Contains(u, "://") {
		if parsed, err := url.Parse(u); err == nil {
			return strings.ToLower(parsed.Hostname())
		}
		return ""
	}
	if at := strings.IndexByte(u, '@'); at >= 0 {
		if host, _, ok := strings.Cut(u[at+1:], ":"); ok {
			return strings.ToLower(host)
		}
	}
	return ""
}

// DetectPRURL checks if rawURL is a recognized pull request URL for any supported provider.
// If matched and valid, returns the matching provider, parsed target, and true.
func DetectPRURL(rawURL string) (GitProvider, PRTarget, bool) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, PRTarget{}, false
	}
	for _, p := range defaultProviders {
		if p.MatchURL(rawURL) {
			target, err := p.ParseURL(rawURL)
			if err == nil {
				return p, target, true
			}
		}
	}
	return nil, PRTarget{}, false
}

// prURLExamples is the URL shapes px0 opens, for error messages.
const prURLExamples = "https://github.com/owner/repo/pull/123, https://gitlab.com/group/project/-/merge_requests/45, https://codeberg.org/owner/repo/pulls/7, or https://bitbucket.org/workspace/repo/pull-requests/9"

// ---------------------------------------------------------------- HTTP plumbing

var githubHTTPClient = &http.Client{Timeout: 15 * time.Second}

var githubLinkNextRe = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

// forgeAPI is one forge's API root, auth header and error-body reader.
type forgeAPI struct {
	name    string            // for error text: "github", "gitlab", "gitea", "bitbucket"
	base    string            // e.g. "https://codeberg.org/api/v1"
	auth    string            // full Authorization header value, "" for anonymous
	accept  string            // Accept header; defaults to "application/json"
	headers map[string]string // extra request headers
	message func(body []byte) string
}

func (a forgeAPI) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	u := path
	if !strings.HasPrefix(path, "http://") && !strings.HasPrefix(path, "https://") {
		u = a.base + path
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
	accept := a.accept
	if accept == "" {
		accept = "application/json"
	}
	req.Header.Set("Accept", accept)
	for k, v := range a.headers {
		req.Header.Set(k, v)
	}
	if a.auth != "" {
		req.Header.Set("Authorization", a.auth)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return githubHTTPClient.Do(req)
}

// do issues method on path (relative to base, or an absolute URL) and
// decodes a 2xx reply into out when it is non-nil. The status is returned
// on error too, so callers can tell "not mergeable" from "head moved".
func (a forgeAPI) do(ctx context.Context, method, path string, body, out any) (int, error) {
	resp, err := a.request(ctx, method, path, body)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(string(b))
		if a.message != nil {
			if m := a.message(b); m != "" {
				msg = m
			}
		}
		return resp.StatusCode, fmt.Errorf("%s: %s %s: %s: %s", a.name, method, path, resp.Status, msg)
	}
	if out != nil && len(bytes.TrimSpace(b)) > 0 {
		if err := json.Unmarshal(b, out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// linkPages follows an RFC 8288 Link header's rel="next" until exhausted,
// decoding each page as a raw JSON array so callers can unmarshal elements
// into their own shape.
func (a forgeAPI) linkPages(ctx context.Context, path string) ([]json.RawMessage, error) {
	var all []json.RawMessage
	next := path
	for next != "" {
		resp, err := a.request(ctx, http.MethodGet, next, nil)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			msg := strings.TrimSpace(string(b))
			if a.message != nil {
				if m := a.message(b); m != "" {
					msg = m
				}
			}
			return nil, fmt.Errorf("%s: get %s: %s: %s", a.name, path, resp.Status, msg)
		}
		var page []json.RawMessage
		err = json.NewDecoder(resp.Body).Decode(&page)
		next = ""
		if m := githubLinkNextRe.FindStringSubmatch(resp.Header.Get("Link")); m != nil {
			next = m[1]
		}
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		all = append(all, page...)
	}
	return all, nil
}

// ---------------------------------------------------------------- GitHub

const githubAPIBase = "https://api.github.com"

var githubPRURLRe = regexp.MustCompile(`^(?:https?://)?github\.com/([^/]+)/([^/]+)/pull/(\d+)`)

// GitHubProvider implements GitProvider for GitHub.
type GitHubProvider struct{}

func (g *GitHubProvider) Name() string { return "github" }

func (g *GitHubProvider) MatchURL(rawURL string) bool {
	return githubPRURLRe.MatchString(strings.TrimSpace(rawURL))
}

func (g *GitHubProvider) ParseURL(rawURL string) (PRTarget, error) {
	m := githubPRURLRe.FindStringSubmatch(strings.TrimSpace(rawURL))
	if m == nil {
		return PRTarget{}, fmt.Errorf("invalid GitHub pull request URL: %q (expected format https://github.com/owner/repo/pull/123)", rawURL)
	}
	n, _ := strconv.Atoi(m[3])
	return PRTarget{
		Provider: "github",
		Host:     "github.com",
		Owner:    m[1],
		Repo:     strings.TrimSuffix(m[2], ".git"),
		Number:   n,
		URL:      rawURL,
	}, nil
}

func (g *GitHubProvider) Info() ForgeInfo {
	return ForgeInfo{
		Name: "github", Label: "GitHub", TokenKey: "github.token", TokenCategory: "GitHub",
		TokenHint: "set GITHUB_TOKEN, set GH_TOKEN, or run `gh auth login`",
	}
}

func (g *GitHubProvider) TokenSetup(host string) TokenSetup {
	return TokenSetup{
		URL: "https://github.com/settings/personal-access-tokens/new",
		Steps: []string{
			"Name the token (e.g. px0) and pick an expiration.",
			"Under Repository access, choose the repositories you review (or All repositories).",
			"Under Repository permissions, set Contents and Pull requests to Read and write.",
			"Generate the token and copy it.",
		},
		CLI:      "gh auth login",
		Env:      "GITHUB_TOKEN",
		Readonly: "Public repositories open and diff without a token; posting comments, reviewing, pushing and merging need one.",
	}
}

func (g *GitHubProvider) MatchRemote(remoteURL string) bool {
	return remoteHost(remoteURL) == "github.com"
}

func (g *GitHubProvider) CloneURL(target PRTarget) string {
	return fmt.Sprintf("https://github.com/%s/%s.git", target.Owner, target.Repo)
}

func (g *GitHubProvider) PRHeadRef(target PRTarget) string {
	return fmt.Sprintf("refs/pull/%d/head", target.Number)
}

func (g *GitHubProvider) GitAuth(target PRTarget, token string) gitAuth {
	return basicGitAuth("github.com", "x-access-token", token)
}

func (g *GitHubProvider) ResolveToken(cfg settings.Settings, target PRTarget) (token, source string) {
	return resolveGitHubToken(cfg)
}

func (g *GitHubProvider) FetchPR(ctx context.Context, target PRTarget, token string) (PRMeta, error) {
	return fetchPRMeta(ctx, target.Owner, target.Repo, target.Number, token)
}

func (g *GitHubProvider) CheckPushAccess(ctx context.Context, target PRTarget, token string) bool {
	return checkPushAccess(ctx, target.Owner, target.Repo, token)
}

func (g *GitHubProvider) SubmitReview(ctx context.Context, target PRTarget, token, headSHA string, comments []prComment, event, body string) error {
	return submitReview(ctx, target.Owner, target.Repo, target.Number, token, headSHA, comments, event, body)
}

func (g *GitHubProvider) FetchComments(ctx context.Context, target PRTarget, token string) ([]PRComment, []PRComment, error) {
	return fetchComments(ctx, target.Owner, target.Repo, target.Number, token)
}

func (g *GitHubProvider) PostIssueComment(ctx context.Context, target PRTarget, token, body string) (PRComment, error) {
	return postIssueComment(ctx, target.Owner, target.Repo, target.Number, token, body)
}

func (g *GitHubProvider) ReplyToReviewComment(ctx context.Context, target PRTarget, token string, commentID int64, body string) (PRComment, error) {
	return replyToReviewComment(ctx, target.Owner, target.Repo, target.Number, token, commentID, body)
}

func (g *GitHubProvider) MergeStatus(ctx context.Context, target PRTarget, token string) (PRMergeStatus, error) {
	return fetchMergeStatus(ctx, target.Owner, target.Repo, target.Number, token)
}

func (g *GitHubProvider) Merge(ctx context.Context, target PRTarget, token string, req PRMergeRequest) (PRMergeResult, error) {
	return mergePR(ctx, target.Owner, target.Repo, target.Number, token, req)
}

// resolveGitHubToken looks for a token in order: the explicit px0 setting
// (github.token), the GITHUB_TOKEN environment variable, GH_TOKEN, then the gh CLI
// if installed and logged in. An empty return means PR review stays read-only.
func resolveGitHubToken(cfg settings.Settings) (token, source string) {
	if cfg.GitHubToken != nil {
		if t := strings.TrimSpace(*cfg.GitHubToken); t != "" {
			return t, "settings"
		}
	}
	if t := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); t != "" {
		return t, "env"
	}
	if t := strings.TrimSpace(os.Getenv("GH_TOKEN")); t != "" {
		return t, "env"
	}
	if out, err := exec.Command("gh", "auth", "token").Output(); err == nil {
		if t := strings.TrimSpace(string(out)); t != "" {
			return t, "gh"
		}
	}
	return "", ""
}

func githubAPI(token string) forgeAPI {
	a := forgeAPI{
		name:    "github",
		base:    githubAPIBase,
		accept:  "application/vnd.github+json",
		headers: map[string]string{"X-GitHub-Api-Version": "2022-11-28"},
	}
	if token != "" {
		a.auth = "Bearer " + token
	}
	return a
}

// githubRequest issues an authenticated (if token != "") GitHub REST call.
func githubRequest(ctx context.Context, method, path, token string, body any) (*http.Response, error) {
	return githubAPI(token).request(ctx, method, path, body)
}

// checkPushAccess reports whether token has push access to owner/repo. Fails
// closed: any transport error, non-200, or missing field yields false, never
// true, so a broken check can only ever hide the write UI, not expose it.
func checkPushAccess(ctx context.Context, owner, repo, token string) bool {
	if token == "" {
		return false
	}
	resp, err := githubRequest(ctx, http.MethodGet, "/repos/"+owner+"/"+repo, token, nil)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var out struct {
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return false
	}
	return out.Permissions.Push
}

func fetchPRMeta(ctx context.Context, owner, repo string, num int, token string) (PRMeta, error) {
	resp, err := githubRequest(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, num), token, nil)
	if err != nil {
		return PRMeta{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		bodyMsg := strings.TrimSpace(string(b))
		if token == "" && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized) {
			return PRMeta{}, fmt.Errorf("github: fetch PR #%d: %s (no GitHub token found; for private repos or rate limits, set GITHUB_TOKEN or run 'gh auth login'): %s", num, resp.Status, bodyMsg)
		}
		return PRMeta{}, fmt.Errorf("github: fetch PR #%d: %s: %s", num, resp.Status, bodyMsg)
	}
	var out struct {
		Number   int    `json:"number"`
		Title    string `json:"title"`
		State    string `json:"state"`
		Merged   bool   `json:"merged"`
		MergedAt string `json:"merged_at"`
		Draft    bool   `json:"draft"`
		User     struct {
			Login string `json:"login"`
		} `json:"user"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				CloneURL string `json:"clone_url"`
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return PRMeta{}, err
	}
	m := PRMeta{
		Number:           out.Number,
		Title:            out.Title,
		Author:           out.User.Login,
		State:            out.State,
		Merged:           out.Merged || out.MergedAt != "",
		MergedAt:         out.MergedAt,
		Draft:            out.Draft,
		BaseRef:          out.Base.Ref,
		HeadRef:          out.Head.Ref,
		HeadSHA:          out.Head.SHA,
		HeadRepoCloneURL: out.Head.Repo.CloneURL,
	}
	m.HeadIsFork = out.Head.Repo.FullName != "" && !strings.EqualFold(out.Head.Repo.FullName, owner+"/"+repo)
	return m, nil
}

// prComment is a review comment held in memory only for the life of the
// process (pr.go's prSession) until submitReview posts it. Side matches
// GitHub's review-comment API: "LEFT" (the base) or "RIGHT" (the PR head).
type prComment struct {
	ID   int64  `json:"id"`
	Path string `json:"path"`
	Line int    `json:"line"`
	Side string `json:"side"`
	Body string `json:"body"`
}

// submitReview posts one review carrying every draft comment plus an overall
// verdict in a single call, mirroring GitHub's own draft-then-submit model
// so px0 never needs a per-comment endpoint.
func submitReview(ctx context.Context, owner, repo string, num int, token, commitID string, comments []prComment, event, body string) error {
	type reviewComment struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Side string `json:"side"`
		Body string `json:"body"`
	}
	payload := struct {
		CommitID string          `json:"commit_id,omitempty"`
		Body     string          `json:"body,omitempty"`
		Event    string          `json:"event"`
		Comments []reviewComment `json:"comments,omitempty"`
	}{CommitID: commitID, Body: body, Event: event}
	for _, c := range comments {
		payload.Comments = append(payload.Comments, reviewComment{Path: c.Path, Line: c.Line, Side: c.Side, Body: c.Body})
	}
	resp, err := githubRequest(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls/%d/reviews", owner, repo, num), token, payload)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("github: submit review: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

type ghUser struct {
	Login     string `json:"login"`
	AvatarURL string `json:"avatar_url"`
}

type ghIssueComment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
	HTMLURL   string `json:"html_url"`
	User      ghUser `json:"user"`
}

func (c ghIssueComment) toPRComment() PRComment {
	return PRComment{
		ID: c.ID, Kind: "issue", Author: c.User.Login, AvatarURL: c.User.AvatarURL,
		Body: c.Body, CreatedAt: c.CreatedAt, URL: c.HTMLURL,
	}
}

type ghReviewComment struct {
	ID           int64  `json:"id"`
	Body         string `json:"body"`
	Path         string `json:"path"`
	Line         int    `json:"line"`
	OriginalLine int    `json:"original_line"`
	Side         string `json:"side"`
	InReplyToID  int64  `json:"in_reply_to_id"`
	CreatedAt    string `json:"created_at"`
	HTMLURL      string `json:"html_url"`
	User         ghUser `json:"user"`
}

func (c ghReviewComment) toPRComment() PRComment {
	// An outdated review comment (its line since edited elsewhere in the
	// diff) has line == null; original_line still says where it was.
	line := c.Line
	if line == 0 {
		line = c.OriginalLine
	}
	return PRComment{
		ID: c.ID, Kind: "review", Path: c.Path, Line: line, Side: c.Side, InReplyTo: c.InReplyToID,
		Author: c.User.Login, AvatarURL: c.User.AvatarURL, Body: c.Body, CreatedAt: c.CreatedAt, URL: c.HTMLURL,
	}
}

// fetchComments returns every comment already posted on the PR: top-level
// ("issue") comments from the issues API, and inline ("review") comments
// (which may be replies, linked via InReplyTo) from the pulls API.
func fetchComments(ctx context.Context, owner, repo string, num int, token string) (issue, review []PRComment, err error) {
	a := githubAPI(token)
	issueRaw, err := a.linkPages(ctx, fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100", owner, repo, num))
	if err != nil {
		return nil, nil, err
	}
	reviewRaw, err := a.linkPages(ctx, fmt.Sprintf("/repos/%s/%s/pulls/%d/comments?per_page=100", owner, repo, num))
	if err != nil {
		return nil, nil, err
	}
	issue = make([]PRComment, 0, len(issueRaw))
	for _, raw := range issueRaw {
		var c ghIssueComment
		if err := json.Unmarshal(raw, &c); err == nil {
			issue = append(issue, c.toPRComment())
		}
	}
	review = make([]PRComment, 0, len(reviewRaw))
	for _, raw := range reviewRaw {
		var c ghReviewComment
		if err := json.Unmarshal(raw, &c); err == nil {
			review = append(review, c.toPRComment())
		}
	}
	return issue, review, nil
}

// postIssueComment posts a new top-level PR conversation comment. GitHub's
// issue comments have no reply/threading concept, so this is also how a
// "reply" to one is implemented: just a new comment.
func postIssueComment(ctx context.Context, owner, repo string, num int, token, body string) (PRComment, error) {
	resp, err := githubRequest(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, repo, num), token, map[string]string{"body": body})
	if err != nil {
		return PRComment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return PRComment{}, fmt.Errorf("github: post comment: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var c ghIssueComment
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return PRComment{}, err
	}
	return c.toPRComment(), nil
}

// replyToReviewComment posts an immediate, properly threaded reply to an
// existing inline review comment via GitHub's dedicated replies endpoint.
func replyToReviewComment(ctx context.Context, owner, repo string, num int, token string, commentID int64, body string) (PRComment, error) {
	resp, err := githubRequest(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls/%d/comments/%d/replies", owner, repo, num, commentID), token, map[string]string{"body": body})
	if err != nil {
		return PRComment{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return PRComment{}, fmt.Errorf("github: reply to review comment: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var c ghReviewComment
	if err := json.NewDecoder(resp.Body).Decode(&c); err != nil {
		return PRComment{}, err
	}
	return c.toPRComment(), nil
}

// fetchMergeStatus combines the PR's mergeability (computed lazily by GitHub:
// the first read after a push often says null/"unknown") with the
// repository's allowed merge methods and the token's push rights.
func fetchMergeStatus(ctx context.Context, owner, repo string, num int, token string) (PRMergeStatus, error) {
	resp, err := githubRequest(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d", owner, repo, num), token, nil)
	if err != nil {
		return PRMergeStatus{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return PRMergeStatus{}, fmt.Errorf("github: fetch PR #%d: %s: %s", num, resp.Status, strings.TrimSpace(string(b)))
	}
	var pr struct {
		State          string `json:"state"`
		Merged         bool   `json:"merged"`
		Draft          bool   `json:"draft"`
		Mergeable      *bool  `json:"mergeable"`
		MergeableState string `json:"mergeable_state"`
		Base           struct {
			Ref string `json:"ref"`
		} `json:"base"`
		Head struct {
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return PRMergeStatus{}, err
	}
	st := PRMergeStatus{HeadSHA: pr.Head.SHA, Methods: []string{}}
	st.State, st.Reason = githubMergeState(pr.Merged, pr.State, pr.Draft, pr.Mergeable, pr.MergeableState, pr.Base.Ref)

	if token == "" {
		return st, nil
	}
	rresp, err := githubRequest(ctx, http.MethodGet, "/repos/"+owner+"/"+repo, token, nil)
	if err != nil {
		return st, nil // the state alone is still worth showing
	}
	defer rresp.Body.Close()
	if rresp.StatusCode != http.StatusOK {
		return st, nil
	}
	var r struct {
		AllowMergeCommit *bool `json:"allow_merge_commit"`
		AllowSquashMerge *bool `json:"allow_squash_merge"`
		AllowRebaseMerge *bool `json:"allow_rebase_merge"`
		Permissions      struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err := json.NewDecoder(rresp.Body).Decode(&r); err != nil {
		return st, nil
	}
	// The allow_* fields are only returned to tokens with admin or push
	// rights; absent means "not ours to know", so nothing is offered.
	for _, m := range []struct {
		allowed *bool
		method  string
	}{{r.AllowMergeCommit, MergeMethodMerge}, {r.AllowSquashMerge, MergeMethodSquash}, {r.AllowRebaseMerge, MergeMethodRebase}} {
		if m.allowed != nil && *m.allowed {
			st.Methods = append(st.Methods, m.method)
		}
	}
	st.CanMerge = r.Permissions.Push
	return st, nil
}

// githubMergeState maps GitHub's mergeable/mergeable_state pair onto the
// forge-neutral MergeState* values.
func githubMergeState(merged bool, state string, draft bool, mergeable *bool, mergeableState, base string) (string, string) {
	switch {
	case merged:
		return MergeStateMerged, ""
	case state == "closed":
		return MergeStateClosed, ""
	case draft || mergeableState == "draft":
		return MergeStateDraft, "this PR is still a draft"
	}
	switch mergeableState {
	case "dirty":
		return MergeStateConflicts, "this branch has conflicts with " + base
	case "behind":
		return MergeStateBehind, "this branch is out of date with " + base
	case "blocked":
		return MergeStateBlocked, "branch protection requires approving reviews or passing checks"
	case "unstable":
		return MergeStateUnstable, "some checks are failing or still running"
	case "clean", "has_hooks":
		return MergeStateClean, ""
	}
	if mergeable != nil && !*mergeable {
		return MergeStateConflicts, "this branch has conflicts with " + base
	}
	return MergeStateUnknown, ""
}

// mergePR merges via PUT /pulls/{n}/merge. The sha guard makes GitHub itself
// refuse (409) a head that moved after the reviewer looked at it.
func mergePR(ctx context.Context, owner, repo string, num int, token string, req PRMergeRequest) (PRMergeResult, error) {
	payload := map[string]string{"merge_method": req.Method}
	if req.HeadSHA != "" {
		payload["sha"] = req.HeadSHA
	}
	if req.Method != MergeMethodRebase {
		if req.Title != "" {
			payload["commit_title"] = req.Title
		}
		if req.Message != "" {
			payload["commit_message"] = req.Message
		}
	}
	resp, err := githubRequest(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", owner, repo, num), token, payload)
	if err != nil {
		return PRMergeResult{}, err
	}
	defer resp.Body.Close()
	var out struct {
		SHA     string `json:"sha"`
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}
	b, _ := io.ReadAll(resp.Body)
	_ = json.Unmarshal(b, &out)
	switch resp.StatusCode {
	case http.StatusOK:
		return PRMergeResult{SHA: out.SHA, Message: out.Message}, nil
	case http.StatusConflict:
		return PRMergeResult{}, fmt.Errorf("github: %w: %s", ErrPRHeadMoved, out.Message)
	case http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
		msg := out.Message
		if msg == "" {
			msg = strings.TrimSpace(string(b))
		}
		return PRMergeResult{}, fmt.Errorf("github: %w: %s", ErrPRNotMergeable, msg)
	}
	return PRMergeResult{}, fmt.Errorf("github: merge PR #%d: %s: %s", num, resp.Status, strings.TrimSpace(string(b)))
}

// ---------------------------------------------------------------- GitLab

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

func gitlabAPI(t PRTarget, token string) forgeAPI {
	a := forgeAPI{name: "gitlab", base: gitlabAPIBase(t.Host), message: gitlabMessage}
	if token != "" {
		// Bearer covers both personal access tokens and glab's OAuth tokens.
		a.auth = "Bearer " + token
	}
	return a
}

// gitlabDo runs one request and decodes a 2xx reply into out (if non-nil).
// Anything else becomes an error carrying GitLab's message.
func gitlabDo(ctx context.Context, method string, t PRTarget, path, token string, body, out any) (int, error) {
	return gitlabAPI(t, token).do(ctx, method, path, body, out)
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
	raw, err := gitlabAPI(t, token).linkPages(ctx, gitlabMRPath(t)+"/discussions?per_page=100")
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

// ---------------------------------------------------------------- Gitea / Forgejo / Codeberg

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
	return host != "" && (host == giteaTokenHost(settings.Read()) || host == "codeberg.org" ||
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
func giteaTokenHost(cfg settings.Settings) string {
	h := ""
	if cfg.GiteaHost != nil {
		h = strings.TrimSpace(*cfg.GiteaHost)
	}
	if h == "" {
		h = strings.TrimSpace(os.Getenv("GITEA_HOST"))
	}
	return normalizeHost(h, "codeberg.org")
}

func (g *GiteaProvider) ResolveToken(cfg settings.Settings, t PRTarget) (token, source string) {
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

// ---------------------------------------------------------------- Bitbucket Cloud

var bitbucketAPIBase = "https://api.bitbucket.org/2.0"

var bitbucketPRURLRe = regexp.MustCompile(`^(?:https?://)?(?:www\.)?bitbucket\.org/([^/\s]+)/([^/\s]+?)/pull-requests/(\d+)`)

// BitbucketProvider implements GitProvider for Bitbucket Cloud.
type BitbucketProvider struct{}

func (b *BitbucketProvider) Name() string { return "bitbucket" }

func (b *BitbucketProvider) Info() ForgeInfo {
	return ForgeInfo{
		Name: "bitbucket", Label: "Bitbucket", TokenKey: "bitbucket.token", TokenCategory: "Bitbucket",
		TokenHint: "set BITBUCKET_TOKEN (an access token, or an API token with BITBUCKET_EMAIL)",
	}
}

func (b *BitbucketProvider) TokenSetup(host string) TokenSetup {
	return TokenSetup{
		URL: "https://id.atlassian.com/manage-profile/security/api-tokens",
		Steps: []string{
			"Select Create API token with scopes, name it (e.g. px0) and pick an expiry.",
			"Choose Bitbucket as the app.",
			"Grant read and write access to repositories and pull requests, and read access to your account (px0 uses it to check push access).",
			"Create the token, copy it, and enter your Atlassian account email below.",
		},
		Env: "BITBUCKET_TOKEN",
		Extra: &TokenField{
			Key: "bitbucket.email", Label: "Atlassian account email", Placeholder: "you@example.com",
			Hint: "Leave empty for a repository, project or workspace access token. Approve and Merge need an API token with an email.",
		},
		Readonly: "Public repositories open and diff without a token; posting comments, reviewing, pushing and merging need one.",
	}
}

func (b *BitbucketProvider) MatchURL(rawURL string) bool {
	return bitbucketPRURLRe.MatchString(strings.TrimSpace(rawURL))
}

func (b *BitbucketProvider) ParseURL(rawURL string) (PRTarget, error) {
	m := bitbucketPRURLRe.FindStringSubmatch(strings.TrimSpace(rawURL))
	if m == nil {
		return PRTarget{}, fmt.Errorf("invalid Bitbucket pull request URL: %q (expected format https://bitbucket.org/workspace/repo/pull-requests/12)", rawURL)
	}
	n, _ := strconv.Atoi(m[3])
	return PRTarget{
		Provider: "bitbucket",
		Host:     "bitbucket.org",
		Owner:    m[1],
		Repo:     strings.TrimSuffix(m[2], ".git"),
		Number:   n,
		URL:      rawURL,
	}, nil
}

func (b *BitbucketProvider) MatchRemote(remoteURL string) bool {
	return remoteHost(remoteURL) == "bitbucket.org"
}

func (b *BitbucketProvider) CloneURL(t PRTarget) string {
	return fmt.Sprintf("https://bitbucket.org/%s/%s.git", t.Owner, t.Repo)
}

// PRHeadRef is empty: Bitbucket publishes no ref for a PR head.
func (b *BitbucketProvider) PRHeadRef(t PRTarget) string { return "" }

// Access tokens authenticate git as "x-token-auth"; API tokens as the fixed
// name Atlassian documents for them.
func (b *BitbucketProvider) GitAuth(t PRTarget, token string) gitAuth {
	if bitbucketEmail(settings.Read()) != "" {
		return basicGitAuth("bitbucket.org", "x-bitbucket-api-token-auth", token)
	}
	return basicGitAuth("bitbucket.org", "x-token-auth", token)
}

// bitbucketEmail is the Atlassian account email an API token belongs to, or
// "" when the token is an access token.
func bitbucketEmail(cfg settings.Settings) string {
	if cfg.BitbucketEmail != nil {
		if e := strings.TrimSpace(*cfg.BitbucketEmail); e != "" {
			return e
		}
	}
	return strings.TrimSpace(os.Getenv("BITBUCKET_EMAIL"))
}

// Bitbucket Cloud has one host, so there is no cross-host risk to guard.
func (b *BitbucketProvider) ResolveToken(cfg settings.Settings, t PRTarget) (token, source string) {
	if cfg.BitbucketToken != nil {
		if tok := strings.TrimSpace(*cfg.BitbucketToken); tok != "" {
			return tok, "settings"
		}
	}
	if tok := strings.TrimSpace(os.Getenv("BITBUCKET_TOKEN")); tok != "" {
		return tok, "env"
	}
	return "", ""
}

func bitbucketAPI(token string) forgeAPI {
	a := forgeAPI{name: "bitbucket", base: bitbucketAPIBase, message: bitbucketMessage}
	if token != "" {
		if email := bitbucketEmail(settings.Read()); email != "" {
			a.auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(email+":"+token))
		} else {
			a.auth = "Bearer " + token
		}
	}
	return a
}

func bitbucketMessage(body []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Detail  any    `json:"detail"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil || e.Error.Message == "" {
		return ""
	}
	if d, ok := e.Error.Detail.(string); ok && d != "" {
		return e.Error.Message + ": " + d
	}
	return e.Error.Message
}

func bitbucketRepoPath(fullName string) string {
	owner, repo, _ := strings.Cut(fullName, "/")
	return "/repositories/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

func bitbucketPRPath(t PRTarget) string {
	return fmt.Sprintf("%s/pullrequests/%d", bitbucketRepoPath(t.Owner+"/"+t.Repo), t.Number)
}

type bbUser struct {
	DisplayName string `json:"display_name"`
	Nickname    string `json:"nickname"`
	Links       struct {
		Avatar struct {
			Href string `json:"href"`
		} `json:"avatar"`
	} `json:"links"`
}

func (u bbUser) name() string {
	if u.Nickname != "" {
		return u.Nickname
	}
	return u.DisplayName
}

type bbEndpoint struct {
	Branch struct {
		Name string `json:"name"`
	} `json:"branch"`
	Commit *struct {
		Hash string `json:"hash"`
	} `json:"commit"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

type bbPR struct {
	ID          int        `json:"id"`
	Title       string     `json:"title"`
	State       string     `json:"state"` // OPEN, MERGED, DECLINED, SUPERSEDED
	Draft       bool       `json:"draft"`
	Author      bbUser     `json:"author"`
	UpdatedOn   string     `json:"updated_on"`
	Source      bbEndpoint `json:"source"`
	Destination bbEndpoint `json:"destination"`
	MergeCommit *struct {
		Hash string `json:"hash"`
	} `json:"merge_commit"`
}

// sourceRepo is the repository the PR's commits live in: the fork, or the
// target itself.
func (p bbPR) sourceRepo(t PRTarget) string {
	if p.Source.Repository != nil && p.Source.Repository.FullName != "" {
		return p.Source.Repository.FullName
	}
	return t.Owner + "/" + t.Repo
}

func (b *BitbucketProvider) fetchPR(ctx context.Context, t PRTarget, token string) (bbPR, error) {
	var pr bbPR
	code, err := bitbucketAPI(token).do(ctx, http.MethodGet, bitbucketPRPath(t), nil, &pr)
	if err != nil && token == "" && (code == http.StatusNotFound || code == http.StatusUnauthorized || code == http.StatusForbidden) {
		return pr, fmt.Errorf("%w (no Bitbucket token found; for private repositories, %s)", err, b.Info().TokenHint)
	}
	return pr, err
}

// headSHA is the PR head as a full hash. Bitbucket reports 12 characters;
// the commit endpoint gives the rest. The short hash is kept if that fails.
func (b *BitbucketProvider) headSHA(ctx context.Context, t PRTarget, token string, pr bbPR) string {
	if pr.Source.Commit == nil {
		return ""
	}
	short := pr.Source.Commit.Hash
	if len(short) == 40 {
		return short
	}
	var c struct {
		Hash string `json:"hash"`
	}
	if _, err := bitbucketAPI(token).do(ctx, http.MethodGet, bitbucketRepoPath(pr.sourceRepo(t))+"/commit/"+url.PathEscape(short), nil, &c); err == nil && strings.HasPrefix(c.Hash, short) {
		return c.Hash
	}
	return short
}

func (b *BitbucketProvider) FetchPR(ctx context.Context, t PRTarget, token string) (PRMeta, error) {
	pr, err := b.fetchPR(ctx, t, token)
	if err != nil {
		return PRMeta{}, err
	}
	m := PRMeta{
		Number: pr.ID, Title: pr.Title, Author: pr.Author.name(),
		State: "open", Merged: pr.State == "MERGED", Draft: pr.Draft,
		BaseRef: pr.Destination.Branch.Name, HeadRef: pr.Source.Branch.Name,
		HeadSHA: b.headSHA(ctx, t, token, pr),
	}
	if pr.State != "OPEN" {
		m.State = "closed"
	}
	if m.Merged {
		m.MergedAt = pr.UpdatedOn
	}
	src := pr.sourceRepo(t)
	m.HeadIsFork = !strings.EqualFold(src, t.Owner+"/"+t.Repo)
	m.HeadRepoCloneURL = "https://bitbucket.org/" + src + ".git"
	return m, nil
}

// CheckPushAccess asks for the user's permission on the repository. That
// endpoint needs a user (API token); an access token can't ask, and this
// fails closed like every provider's check.
func (b *BitbucketProvider) CheckPushAccess(ctx context.Context, t PRTarget, token string) bool {
	if token == "" {
		return false
	}
	q := url.QueryEscape(fmt.Sprintf(`repository.full_name="%s/%s"`, t.Owner, t.Repo))
	var out struct {
		Values []struct {
			Permission string `json:"permission"`
		} `json:"values"`
	}
	if _, err := bitbucketAPI(token).do(ctx, http.MethodGet, "/user/permissions/repositories?q="+q, nil, &out); err != nil {
		return false
	}
	for _, v := range out.Values {
		if v.Permission == "write" || v.Permission == "admin" {
			return true
		}
	}
	return false
}

type bbComment struct {
	ID      int64 `json:"id"`
	Deleted bool  `json:"deleted"`
	Content struct {
		Raw string `json:"raw"`
	} `json:"content"`
	User      bbUser `json:"user"`
	CreatedOn string `json:"created_on"`
	Inline    *struct {
		Path string `json:"path"`
		From *int   `json:"from"`
		To   *int   `json:"to"`
	} `json:"inline"`
	Parent *struct {
		ID int64 `json:"id"`
	} `json:"parent"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

func (c bbComment) toPRComment() PRComment {
	pc := PRComment{
		ID: c.ID, Kind: "issue", Author: c.User.name(), AvatarURL: c.User.Links.Avatar.Href,
		Body: c.Content.Raw, CreatedAt: c.CreatedOn, URL: c.Links.HTML.Href,
	}
	if in := c.Inline; in != nil {
		pc.Kind, pc.Path = "review", in.Path
		switch {
		case in.To != nil:
			pc.Line, pc.Side = *in.To, "RIGHT"
		case in.From != nil:
			pc.Line, pc.Side = *in.From, "LEFT"
		}
	}
	return pc
}

func bbInline(path string, line int, side string) map[string]any {
	in := map[string]any{"path": path}
	if side == "LEFT" {
		in["from"] = line
	} else {
		in["to"] = line
	}
	return in
}

func (b *BitbucketProvider) postComment(ctx context.Context, t PRTarget, token string, payload map[string]any) (bbComment, error) {
	var c bbComment
	_, err := bitbucketAPI(token).do(ctx, http.MethodPost, bitbucketPRPath(t)+"/comments", payload, &c)
	return c, err
}

// SubmitReview posts each draft as an inline comment and the body as a
// general one, then records the verdict: Bitbucket has both an approval and
// a "request changes" state of its own.
func (b *BitbucketProvider) SubmitReview(ctx context.Context, t PRTarget, token, headSHA string, comments []prComment, event, body string) error {
	for _, c := range comments {
		if _, err := b.postComment(ctx, t, token, map[string]any{"content": map[string]string{"raw": c.Body}, "inline": bbInline(c.Path, c.Line, c.Side)}); err != nil {
			return err
		}
	}
	if strings.TrimSpace(body) != "" {
		if _, err := b.postComment(ctx, t, token, map[string]any{"content": map[string]string{"raw": body}}); err != nil {
			return err
		}
	}
	verdict := map[string]string{"APPROVE": "/approve", "REQUEST_CHANGES": "/request-changes"}[event]
	if verdict == "" {
		return nil
	}
	_, err := bitbucketAPI(token).do(ctx, http.MethodPost, bitbucketPRPath(t)+verdict, nil, nil)
	return err
}

// bitbucketPages follows a paginated list's "next" links.
func bitbucketPages[T any](ctx context.Context, a forgeAPI, path string) ([]T, error) {
	var all []T
	for next, n := path, 0; next != "" && n < 100; n++ {
		var page struct {
			Values []T    `json:"values"`
			Next   string `json:"next"`
		}
		if _, err := a.do(ctx, http.MethodGet, next, nil, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Values...)
		next = page.Next
	}
	return all, nil
}

// FetchComments splits inline comments (review threads, replies pointed at
// the thread's first comment) from general ones. Bitbucket threads general
// comments too; px0 shows those flat.
func (b *BitbucketProvider) FetchComments(ctx context.Context, t PRTarget, token string) (issue, review []PRComment, err error) {
	cs, err := bitbucketPages[bbComment](ctx, bitbucketAPI(token), bitbucketPRPath(t)+"/comments?pagelen=100")
	if err != nil {
		return nil, nil, err
	}
	parent := map[int64]int64{}
	byID := map[int64]bbComment{}
	for _, c := range cs {
		byID[c.ID] = c
		if c.Parent != nil {
			parent[c.ID] = c.Parent.ID
		}
	}
	root := func(id int64) int64 {
		for i := 0; i < 1000; i++ { // a cycle can't happen, but never spin on one
			p, ok := parent[id]
			if !ok {
				break
			}
			id = p
		}
		return id
	}
	issue, review = []PRComment{}, []PRComment{}
	for _, c := range cs {
		if c.Deleted {
			continue
		}
		r := byID[root(c.ID)]
		pc := c.toPRComment()
		if r.Inline == nil {
			pc.Kind = "issue"
			issue = append(issue, pc)
			continue
		}
		rc := r.toPRComment()
		pc.Kind, pc.Path, pc.Line, pc.Side = "review", rc.Path, rc.Line, rc.Side
		if r.ID != c.ID {
			pc.InReplyTo = r.ID
		}
		review = append(review, pc)
	}
	return issue, review, nil
}

func (b *BitbucketProvider) PostIssueComment(ctx context.Context, t PRTarget, token, body string) (PRComment, error) {
	c, err := b.postComment(ctx, t, token, map[string]any{"content": map[string]string{"raw": body}})
	if err != nil {
		return PRComment{}, err
	}
	pc := c.toPRComment()
	pc.Kind = "issue"
	return pc, nil
}

// ReplyToReviewComment uses Bitbucket's own threading: a comment with a parent.
func (b *BitbucketProvider) ReplyToReviewComment(ctx context.Context, t PRTarget, token string, commentID int64, body string) (PRComment, error) {
	_, review, err := b.FetchComments(ctx, t, token)
	if err != nil {
		return PRComment{}, err
	}
	for _, rc := range review {
		if rc.ID != commentID {
			continue
		}
		c, err := b.postComment(ctx, t, token, map[string]any{"content": map[string]string{"raw": body}, "parent": map[string]int64{"id": commentID}})
		if err != nil {
			return PRComment{}, err
		}
		pc := c.toPRComment()
		root := rc.ID
		if rc.InReplyTo != 0 {
			root = rc.InReplyTo
		}
		pc.Kind, pc.Path, pc.Line, pc.Side, pc.InReplyTo = "review", rc.Path, rc.Line, rc.Side, root
		return pc, nil
	}
	return PRComment{}, fmt.Errorf("bitbucket: comment %d not found on pull request #%d", commentID, t.Number)
}

// bitbucketStrategies maps px0's merge methods onto Bitbucket's strategies,
// most faithful first. A rebase merge is "rebase_fast_forward" where the
// branch allows it, else a plain fast-forward.
var bitbucketStrategies = map[string][]string{
	MergeMethodMerge:  {"merge_commit", "rebase_merge"},
	MergeMethodSquash: {"squash", "squash_fast_forward"},
	MergeMethodRebase: {"rebase_fast_forward", "fast_forward"},
}

// bitbucketDefaultStrategies are what a branch allows when Bitbucket doesn't
// say: the three it has always offered.
var bitbucketDefaultStrategies = []string{"merge_commit", "squash", "fast_forward"}

func bitbucketStrategy(method string, allowed []string) string {
	for _, s := range bitbucketStrategies[method] {
		for _, a := range allowed {
			if a == s {
				return s
			}
		}
	}
	return ""
}

// allowedStrategies reads the merge strategies the destination branch allows.
func (b *BitbucketProvider) allowedStrategies(ctx context.Context, t PRTarget, token, branch string) []string {
	var br struct {
		MergeStrategies []string `json:"merge_strategies"`
	}
	path := bitbucketRepoPath(t.Owner+"/"+t.Repo) + "/refs/branches/" + url.PathEscape(branch)
	if _, err := bitbucketAPI(token).do(ctx, http.MethodGet, path, nil, &br); err != nil || len(br.MergeStrategies) == 0 {
		return bitbucketDefaultStrategies
	}
	return br.MergeStrategies
}

// hasConflicts reports whether the PR's diffstat lists any file in conflict.
func (b *BitbucketProvider) hasConflicts(ctx context.Context, t PRTarget, token string) (bool, error) {
	entries, err := bitbucketPages[struct {
		Status string `json:"status"`
	}](ctx, bitbucketAPI(token), bitbucketPRPath(t)+"/diffstat?pagelen=500")
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if strings.Contains(e.Status, "conflict") {
			return true, nil
		}
	}
	return false, nil
}

func (b *BitbucketProvider) MergeStatus(ctx context.Context, t PRTarget, token string) (PRMergeStatus, error) {
	pr, err := b.fetchPR(ctx, t, token)
	if err != nil {
		return PRMergeStatus{}, err
	}
	st := PRMergeStatus{HeadSHA: b.headSHA(ctx, t, token, pr), Methods: []string{}}
	switch {
	case pr.State == "MERGED":
		st.State = MergeStateMerged
	case pr.State != "OPEN":
		st.State = MergeStateClosed
	case pr.Draft:
		st.State, st.Reason = MergeStateDraft, "this pull request is still a draft"
	default:
		conflicts, err := b.hasConflicts(ctx, t, token)
		switch {
		case err != nil:
			st.State = MergeStateUnknown
		case conflicts:
			st.State, st.Reason = MergeStateConflicts, "this branch has conflicts with "+pr.Destination.Branch.Name
		default:
			st.State = MergeStateClean
		}
	}
	if token == "" {
		return st, nil
	}
	allowed := b.allowedStrategies(ctx, t, token, pr.Destination.Branch.Name)
	for _, m := range []string{MergeMethodMerge, MergeMethodSquash, MergeMethodRebase} {
		if bitbucketStrategy(m, allowed) != "" {
			st.Methods = append(st.Methods, m)
		}
	}
	st.CanMerge = b.CheckPushAccess(ctx, t, token)
	return st, nil
}

// Merge merges with the strategy the branch allows for req.Method. Bitbucket
// takes no head-commit guard, so the head is checked here first; the window
// between the two calls is the only one left.
func (b *BitbucketProvider) Merge(ctx context.Context, t PRTarget, token string, req PRMergeRequest) (PRMergeResult, error) {
	pr, err := b.fetchPR(ctx, t, token)
	if err != nil {
		return PRMergeResult{}, err
	}
	if req.HeadSHA != "" {
		if head := b.headSHA(ctx, t, token, pr); head != "" && !strings.HasPrefix(req.HeadSHA, head) && !strings.HasPrefix(head, req.HeadSHA) {
			return PRMergeResult{}, fmt.Errorf("bitbucket: %w: head is %s, reviewed %s", ErrPRHeadMoved, head, req.HeadSHA)
		}
	}
	strategy := bitbucketStrategy(req.Method, b.allowedStrategies(ctx, t, token, pr.Destination.Branch.Name))
	if strategy == "" {
		return PRMergeResult{}, fmt.Errorf("bitbucket: %w: %s is not allowed on %s", ErrPRNotMergeable, req.Method, pr.Destination.Branch.Name)
	}
	payload := map[string]any{"merge_strategy": strategy, "close_source_branch": false}
	msg := strings.TrimSpace(req.Title)
	if body := strings.TrimSpace(req.Message); body != "" && msg != "" {
		msg += "\n\n" + body
	}
	if msg != "" && req.Method != MergeMethodRebase {
		payload["message"] = msg
	}
	var out bbPR
	code, err := bitbucketAPI(token).do(ctx, http.MethodPost, bitbucketPRPath(t)+"/merge", payload, &out)
	if err != nil {
		if code == http.StatusBadRequest || code == http.StatusConflict {
			return PRMergeResult{}, fmt.Errorf("%w: %v", ErrPRNotMergeable, err)
		}
		return PRMergeResult{}, err
	}
	// 202: Bitbucket is still merging in the background; the bar's next
	// refresh shows the result.
	res := PRMergeResult{}
	if code == http.StatusAccepted {
		res.Message = "Bitbucket is completing the merge"
	}
	if out.MergeCommit != nil {
		res.SHA = out.MergeCommit.Hash
	}
	return res, nil
}
