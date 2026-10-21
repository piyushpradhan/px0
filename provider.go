package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

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

// RegisterProvider registers a custom or additional GitProvider.
func RegisterProvider(p GitProvider) {
	defaultProviders = append(defaultProviders, p)
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

// ParsePRURL attempts to parse a PR URL, returning an error if no provider matches
// or if the URL format is invalid.
func ParsePRURL(rawURL string) (GitProvider, PRTarget, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, PRTarget{}, fmt.Errorf("empty PR URL")
	}
	for _, p := range defaultProviders {
		if p.MatchURL(rawURL) {
			target, err := p.ParseURL(rawURL)
			if err != nil {
				return nil, PRTarget{}, err
			}
			return p, target, nil
		}
	}
	return nil, PRTarget{}, fmt.Errorf("unsupported or unrecognized PR URL: %q (expected e.g. %s)", rawURL, prURLExamples)
}

// prURLExamples is the URL shapes px0 opens, for error messages.
const prURLExamples = "https://github.com/owner/repo/pull/123, https://gitlab.com/group/project/-/merge_requests/45, https://codeberg.org/owner/repo/pulls/7, or https://bitbucket.org/workspace/repo/pull-requests/9"
