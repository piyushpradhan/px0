package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// pr.go handles git forge pull/merge request reviews: checking out a PR's
// source tree into a throwaway git worktree, diffing it against the merge-base
// with its target branch instead of HEAD, and letting the reviewer leave draft
// comments and submit reviews or batch apply them with AI agents.
//
// Nothing persists past the process. The worktree lives in a system temp dir
// and is removed in prSession.Close.

// prSession is one checked-out PR review. Draft comments live only in
// memory (mu-guarded), same lifetime as an agentJob -- never written to
// disk, never surviving a restart.
type prSession struct {
	mu sync.Mutex

	provider        GitProvider
	target          PRTarget
	meta            PRMeta
	token           string
	tokenSource     string // where token came from: settings, env, gh, glab, session ("" with no token)
	writeAccess     bool
	diffBase        string // merge-base(head, base branch), or "HEAD" if the base couldn't be resolved
	diffBaseWarning string // set when diffBase fell back to "HEAD"; surfaced in the UI so an empty diff doesn't read as "no changes"

	worktree  string // temp checkout, removed in Close
	srcRepo   string // the repo the worktree was registered against ("" for a plain clone)
	srcRemote string // srcRepo's remote that points at the PR's repository

	// pushedSHA is what the PR's branch holds on the remote after this session
	// pushed to it ("" until the first push: the head the PR was checked out
	// at). It bounds "unpushed", and nothing else. meta.HeadSHA deliberately
	// stays at the head the review started from, so a push moves your commits
	// out of "Unpushed" without folding them into "PR changes".
	pushedSHA string

	scopeDir string // temp dir holding the diff files handed to threads; removed in Close

	comments []prComment
	nextID   int64
}

// remoteHead returns the commit the PR's branch is at on the remote, as far as
// this session knows. Callers hold p.mu.
func (p *prSession) remoteHead() string {
	if p.pushedSHA != "" {
		return p.pushedSHA
	}
	return p.meta.HeadSHA
}

// ErrPRMergedCancelled is returned when opening an already-merged PR is cancelled.
var ErrPRMergedCancelled = errors.New("PR is already merged; opening cancelled")

// computeDiffBase fetches the PR's base branch into worktree and returns a
// merge-base with HEAD to diff against, so review diffs show exactly what
// the PR changes rather than the head's full HEAD diff. Best-effort: if it
// can't be resolved (e.g. the base branch was force-pushed away, or the
// fetch itself failed), diffBase falls back to "HEAD" -- which diffs the
// checkout against its own HEAD and looks empty -- with a warning explaining
// why, so callers can surface it and a blank diff never reads as "no
// changes". Shared by checkoutPR (initial checkout) and prSession.Pull
// (re-sync after new commits land on the PR).
func computeDiffBase(worktree, srcRepo, srcRemote string, fg forgeGit, baseRef string, num int, onProgress func(string)) (diffBase, diffBaseWarning string) {
	if onProgress != nil {
		onProgress(fmt.Sprintf("Computing merge base with %s...", baseRef))
	}
	diffBase = "HEAD"
	if srcRemote == "" {
		srcRemote = "origin"
	}
	fetchErr := fetchPRBase(worktree, srcRepo, srcRemote, fg, baseRef, num)
	if fetchErr == "" {
		if mb := gitMergeBase(worktree, "HEAD", prBaseRef(num)); mb != "" {
			diffBase = mb
		}
	}
	if diffBase == "HEAD" && srcRepo != "" {
		if mb := gitMergeBase(worktree, "HEAD", srcRemote+"/"+baseRef); mb != "" {
			diffBase = mb
			fetchErr = "" // recovered via the local clone's own remote-tracking ref
		}
	}
	if diffBase == "HEAD" {
		diffBaseWarning = fmt.Sprintf("could not resolve a merge-base with %s; diff will show no changes", baseRef)
		if fetchErr != "" {
			diffBaseWarning = fmt.Sprintf("%s (%s)", diffBaseWarning, fetchErr)
		}
		fmt.Fprintln(os.Stderr, "px0: warning:", diffBaseWarning)
		if onProgress != nil {
			onProgress("Warning: " + diffBaseWarning)
		}
	}
	return diffBase, diffBaseWarning
}

// forgeGit is how plain git reaches the PR's repository on its forge.
type forgeGit struct {
	CloneURL string  // https clone URL of the base repository
	HeadRef  string  // the forge's ref for the PR head, "" if it has none
	Auth     gitAuth // empty without a token
}

func newForgeGit(provider GitProvider, target PRTarget, token string) forgeGit {
	if provider == nil {
		provider = &GitHubProvider{}
	}
	return forgeGit{CloneURL: provider.CloneURL(target), HeadRef: provider.PRHeadRef(target), Auth: provider.GitAuth(target, token)}
}

// git is newForgeGit for this session. Callers hold no lock.
func (p *prSession) git() forgeGit {
	p.mu.Lock()
	defer p.mu.Unlock()
	t := p.target
	if t.Number == 0 {
		t.Number = p.meta.Number
	}
	return newForgeGit(p.provider, t, p.token)
}

// prBaseRef is where a session keeps its fetched copy of the PR's base
// branch: the ref diffs are measured from and an agent rebase targets.
func prBaseRef(num int) string { return fmt.Sprintf("refs/px0/base/%d", num) }

// fetchPRBase force-updates prBaseRef(num) from the PR's base branch,
// returning git's error text ("" on success). Forced, so a base branch that
// was itself rewritten is followed rather than refused.
func fetchPRBase(worktree, srcRepo, srcRemote string, fg forgeGit, baseRef string, num int) string {
	baseRemote := srcRemote
	if srcRepo == "" {
		baseRemote = fg.CloneURL
	}
	refspec := fmt.Sprintf("+refs/heads/%s:%s", baseRef, prBaseRef(num))
	out, err := gitAuthCmd(fg.Auth, "-C", worktree, "fetch", "--no-tags", baseRemote, refspec).CombinedOutput()
	if err == nil {
		return ""
	}
	if msg := fg.Auth.redact(strings.TrimSpace(string(out))); msg != "" {
		return msg
	}
	return err.Error()
}

// checkoutPR fetches a PR's head ref and checks it out into a system temp
// directory: a git worktree of cwd's origin when cwd is already a clone of
// the same repo (the common case -- opened inside the repo), or a shallow
// single-branch clone of the PR head otherwise (a bare URL opened from an
// unrelated directory).
func checkoutPR(ctx context.Context, provider GitProvider, target PRTarget, cwd string, onProgress func(string)) (*prSession, error) {
	cfg := readSettings()
	token, tokenSource := provider.ResolveToken(cfg, target)

	if onProgress != nil {
		onProgress(fmt.Sprintf("Fetching PR #%d metadata from %s...", target.Number, provider.Name()))
	}
	meta, err := provider.FetchPR(ctx, target, token)
	if err != nil {
		return nil, err
	}

	tmp, err := os.MkdirTemp("", "px0-pr-*")
	if err != nil {
		return nil, err
	}
	// macOS TempDir lives under /var -> /private/var; git rev-parse
	// --show-toplevel reports the resolved path, so leaving tmp unresolved
	// makes gitStatusAgainst's toplevel-relative prefix check fail for every
	// file, silently emptying the PR's diff/status view.
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	cleanup := func() { os.RemoveAll(tmp) }

	srcRepo, srcRemote := findPRRemote(cwd, target.Owner, target.Repo)
	fg := newForgeGit(provider, target, token)

	num := target.Number
	if srcRepo != "" && fg.HeadRef != "" {
		if onProgress != nil {
			onProgress(fmt.Sprintf("Fetching PR #%d head and preparing worktree...", num))
		}
		headRefspec := fmt.Sprintf("%s:refs/px0/pr/%d", fg.HeadRef, num)
		if out, err := gitLocalThenToken(fg.Auth, "-C", srcRepo, "fetch", "--no-tags", srcRemote, headRefspec); err != nil {
			cleanup()
			return nil, fmt.Errorf("git fetch PR head: %w: %s", err, fg.Auth.redact(strings.TrimSpace(string(out))))
		}
		// A partial clone (the workbench's own) fetches file contents here.
		if out, err := gitLocalThenToken(fg.Auth, "-C", srcRepo, "worktree", "add", "--detach", tmp, fmt.Sprintf("refs/px0/pr/%d", num)); err != nil {
			cleanup()
			return nil, fmt.Errorf("git worktree add: %w: %s", err, fg.Auth.redact(strings.TrimSpace(string(out))))
		}
	} else {
		// No local clone, or a forge with no PR head ref: clone the head
		// branch itself, from wherever it lives (a fork included).
		srcRepo, srcRemote = "", ""
		if onProgress != nil {
			onProgress(fmt.Sprintf("Cloning PR #%d (%s)...", num, meta.HeadRef))
		}
		cloneURL := meta.HeadRepoCloneURL
		if cloneURL == "" {
			cloneURL = fg.CloneURL
		}
		if out, err := gitAuthCmd(fg.Auth, "clone", "--filter=blob:none", "--branch", meta.HeadRef, "--single-branch", cloneURL, tmp).CombinedOutput(); err != nil {
			cleanup()
			return nil, fmt.Errorf("git clone PR head: %w: %s", err, fg.Auth.redact(strings.TrimSpace(string(out))))
		}
	}

	diffBase, diffBaseWarning := computeDiffBase(tmp, srcRepo, srcRemote, fg, meta.BaseRef, num, onProgress)

	writeAccess := provider.CheckPushAccess(ctx, target, token)

	return &prSession{
		provider:        provider,
		target:          target,
		meta:            meta,
		token:           token,
		tokenSource:     tokenSource,
		writeAccess:     writeAccess,
		diffBase:        diffBase,
		diffBaseWarning: diffBaseWarning,
		worktree:        tmp,
		srcRepo:         srcRepo,
		srcRemote:       srcRemote,
	}, nil
}

func (p *prSession) Root() string { return p.worktree }

// findPRRemote reports the git repository containing dir and the name of its
// remote that points at owner/repo, preferring origin, then upstream (a fork
// checkout), then any other. Both are empty when dir is not such a clone.
func findPRRemote(dir, owner, repo string) (toplevel, remote string) {
	if owner == "" || repo == "" {
		return "", ""
	}
	info := gitProbe(dir)
	if !info.ok || info.toplevel == "" {
		return "", ""
	}
	out, err := exec.Command("git", "-C", info.toplevel, "remote").Output()
	if err != nil {
		return "", ""
	}
	names := strings.Fields(string(out))
	rank := func(n string) int {
		switch n {
		case "origin":
			return 0
		case "upstream":
			return 1
		}
		return 2
	}
	sort.SliceStable(names, func(i, j int) bool { return rank(names[i]) < rank(names[j]) })
	for _, name := range names {
		u, err := exec.Command("git", "-C", info.toplevel, "remote", "get-url", name).Output()
		if err == nil && remoteMatchesRepo(string(u), owner, repo) {
			return info.toplevel, name
		}
	}
	return "", ""
}

// remoteMatchesRepo reports whether a remote URL (https, ssh or scp-style)
// names owner/repo, ignoring case. owner may be a nested GitLab group path
// ("group/sub"), so the whole repository path is compared, not just its last
// two segments; only a local-path remote matches by suffix.
func remoteMatchesRepo(remoteURL, owner, repo string) bool {
	want := strings.ToLower(owner + "/" + repo)
	u := strings.ToLower(strings.TrimSpace(remoteURL))
	local := false
	switch {
	case strings.Contains(u, "://"):
		parsed, err := url.Parse(u)
		if err != nil {
			return false
		}
		local = parsed.Scheme == "file"
		u = parsed.Path
	case remoteHost(u) != "":
		_, u, _ = strings.Cut(u, ":")
	default:
		local = true
	}
	u = strings.TrimSuffix(strings.Trim(u, "/"), ".git")
	return u == want || (local && strings.HasSuffix(u, "/"+want))
}

// Close removes the worktree registration (if any), cleans up temporary
// references, and deletes the temp checkout. Safe on a nil receiver.
func (p *prSession) Close() {
	if p == nil {
		return
	}
	if p.scopeDir != "" {
		os.RemoveAll(p.scopeDir)
	}
	if p.srcRepo != "" {
		exec.Command("git", "-C", p.srcRepo, "worktree", "remove", "--force", p.worktree).Run()
		exec.Command("git", "-C", p.srcRepo, "update-ref", "-d", fmt.Sprintf("refs/px0/pr/%d", p.meta.Number)).Run()
		exec.Command("git", "-C", p.srcRepo, "update-ref", "-d", fmt.Sprintf("refs/px0/base/%d", p.meta.Number)).Run()
	}
	os.RemoveAll(p.worktree)
}

// httpsRemoteURL rewrites an ssh-style remote (git@github.com:o/r.git,
// ssh://git@github.com/o/r.git) to its https form; anything else is returned as is.
func httpsRemoteURL(raw string) string {
	switch {
	case strings.HasPrefix(raw, "ssh://"):
		rest := strings.TrimPrefix(raw, "ssh://")
		if i := strings.IndexByte(rest, '@'); i >= 0 {
			rest = rest[i+1:]
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			host := rest[:i]
			if j := strings.IndexByte(host, ':'); j >= 0 { // ssh port means nothing to https
				host = host[:j]
			}
			return "https://" + host + rest[i:]
		}
	case strings.Contains(raw, "@") && !strings.Contains(raw, "://"):
		if at := strings.IndexByte(raw, '@'); at >= 0 {
			if host, path, ok := strings.Cut(raw[at+1:], ":"); ok {
				return "https://" + host + "/" + strings.TrimPrefix(path, "/")
			}
		}
	}
	return raw
}

// gitAuthCmd builds a git command that authenticates to the forge over
// https with its token -- what a token-in-the-URL does, minus the token in the
// URL (and so in argv, in git's remote config, and in error text). The header
// travels in the environment, scoped to the forge's URL prefix, and terminal
// prompts are off so a bad or missing token fails fast instead of hanging on a
// password prompt. With no auth it is plain git.
func gitAuthCmd(auth gitAuth, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	env := append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if auth.Header != "" {
		env = append(env,
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http."+auth.Prefix+".extraheader",
			"GIT_CONFIG_VALUE_0="+auth.Header,
		)
	}
	cmd.Env = env
	return cmd
}

// gitLocalThenToken runs git as the local clone is set up to, and only if
// that fails and there is a token, again with the token: a clone's own
// credentials may well reach more than the token does.
func gitLocalThenToken(auth gitAuth, args ...string) ([]byte, error) {
	out, err := gitAuthCmd(gitAuth{}, args...).CombinedOutput()
	if err != nil && auth.Header != "" {
		out, err = gitAuthCmd(auth, args...).CombinedOutput()
	}
	return out, err
}

func redactToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// writeScopeFile puts content in a per-session temp dir (outside the worktree, so
// it never shows up as an untracked change) and returns its path, or "" if it
// could not be written.
func (p *prSession) writeScopeFile(name, content string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.scopeDir == "" {
		dir, err := os.MkdirTemp("", "px0-prdiff-*")
		if err != nil {
			return ""
		}
		p.scopeDir = dir
	}
	path := filepath.Join(p.scopeDir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return ""
	}
	return path
}

// errPRDiverged is returned by Pull when the checkout can't be fast-forwarded
// onto the PR's current head -- local commits, or a force-pushed head, that
// don't share a straight-line history with what was fetched. Resolving that
// is not supported: Pull never invokes git's merge machinery, only a reset
// --hard onto an ancestor-verified fast-forward.
var errPRDiverged = errors.New("local checkout has diverged from the PR head; resolve manually")

// Pull re-fetches the PR's current head and, if it's a clean fast-forward,
// resets the worktree onto it and refreshes meta.HeadSHA/diffBase to match.
// Refuses outright when the worktree has uncommitted changes (nothing here
// stashes) or when the fast-forward check fails, in which case the caller
// should surface errPRDiverged as "not supported, resolve manually".
func (p *prSession) Pull() (info string, err error) {
	p.mu.Lock()
	worktree, srcRepo, srcRemote, num := p.worktree, p.srcRepo, p.srcRemote, p.meta.Number
	if srcRemote == "" {
		srcRemote = "origin"
	}
	target, baseRef := p.target, p.meta.BaseRef
	headRepoCloneURL, headRef := p.meta.HeadRepoCloneURL, p.meta.HeadRef
	token := p.token
	p.mu.Unlock()
	fg := p.git()

	if gitHasUncommittedChanges(worktree) {
		return "", errors.New("commit or discard your local changes before pulling")
	}

	// Fetch the PR branch itself, forced: it is the source of truth for the
	// PR's head (the forge's PR head ref can lag behind a fresh push), and a
	// force-pushed branch must not be refused as a non-fast-forward ref update.
	// A local clone with no token keeps using its own remote instead, which is
	// the remote that clone can already authenticate to.
	newRef := "FETCH_HEAD"
	if srcRepo != "" && token == "" && fg.HeadRef != "" {
		newRef = fmt.Sprintf("refs/px0/pr/%d", num)
		headRefspec := fmt.Sprintf("+%s:%s", fg.HeadRef, newRef)
		if out, err := exec.Command("git", "-C", srcRepo, "fetch", "--no-tags", srcRemote, headRefspec).CombinedOutput(); err != nil {
			return "", fmt.Errorf("git fetch PR head: %w: %s", err, strings.TrimSpace(string(out)))
		}
	} else {
		cloneURL := headRepoCloneURL
		if cloneURL == "" {
			cloneURL = fg.CloneURL
		}
		if out, err := gitAuthCmd(fg.Auth, "-C", worktree, "fetch", "--no-tags", cloneURL, "+refs/heads/"+headRef).CombinedOutput(); err != nil {
			return "", fmt.Errorf("git fetch PR head: %w: %s", err, fg.Auth.redact(strings.TrimSpace(string(out))))
		}
	}

	headOut, err := exec.Command("git", "-C", worktree, "rev-parse", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	head := strings.TrimSpace(string(headOut))
	p.mu.Lock()
	knownHead := p.meta.HeadSHA
	p.mu.Unlock()
	nothingOfMine := head == knownHead // no local commits past the PR head we checked out

	info = "pulled the latest changes"
	upToDate := exec.Command("git", "-C", worktree, "merge-base", "--is-ancestor", newRef, "HEAD").Run() == nil
	switch {
	case upToDate:
		info = "already up to date"
	case exec.Command("git", "-C", worktree, "merge-base", "--is-ancestor", "HEAD", newRef).Run() == nil:
		// plain fast-forward
	case nothingOfMine:
		// The PR branch was rewritten (force-push or rebase) and there is
		// nothing of yours to lose: follow it.
		info = "pulled the latest changes (the PR branch was force-pushed)"
	default:
		return "", errPRDiverged
	}
	if !upToDate {
		if out, err := exec.Command("git", "-C", worktree, "reset", "--hard", newRef).CombinedOutput(); err != nil {
			return "", fmt.Errorf("git reset: %w: %s", err, strings.TrimSpace(string(out)))
		}
		shaOut, err := exec.Command("git", "-C", worktree, "rev-parse", "HEAD").Output()
		if err != nil {
			return "", fmt.Errorf("git rev-parse HEAD: %w", err)
		}
		p.mu.Lock()
		p.meta.HeadSHA = strings.TrimSpace(string(shaOut))
		p.pushedSHA = ""
		p.mu.Unlock()
	}

	// Title, state, merged, base branch: refresh what the forge says about the PR
	// itself, so the bar doesn't keep showing the state from session start.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if p.provider == nil {
		// nothing to ask
	} else if fresh, err := p.provider.FetchPR(ctx, target, token); err == nil {
		p.mu.Lock()
		p.meta.Title, p.meta.State, p.meta.Merged, p.meta.MergedAt = fresh.Title, fresh.State, fresh.Merged, fresh.MergedAt
		p.meta.Draft, p.meta.BaseRef = fresh.Draft, fresh.BaseRef
		baseRef = p.meta.BaseRef
		p.mu.Unlock()
	}
	diffBase, diffBaseWarning := computeDiffBase(worktree, srcRepo, srcRemote, fg, baseRef, num, nil)
	p.mu.Lock()
	p.diffBase = diffBase
	p.diffBaseWarning = diffBaseWarning
	p.mu.Unlock()

	return info, nil
}

// FetchBase refreshes the session's copy of the PR's base branch and
// returns its ref -- what an agent rebase or merge of the base targets. px0
// fetches it itself, with the forge token, so the harness never needs
// credentials (and never fetches at all).
func (p *prSession) FetchBase() (string, error) {
	p.mu.Lock()
	worktree, srcRepo, srcRemote := p.worktree, p.srcRepo, p.srcRemote
	baseRef, num := p.meta.BaseRef, p.meta.Number
	p.mu.Unlock()
	if srcRemote == "" {
		srcRemote = "origin"
	}
	if msg := fetchPRBase(worktree, srcRepo, srcRemote, p.git(), baseRef, num); msg != "" {
		return "", fmt.Errorf("git fetch %s: %s", baseRef, msg)
	}
	return prBaseRef(num), nil
}

// refreshDiffBase re-derives diffBase from the base ref already fetched. A
// pushed rebase onto (or merge of) a newer base moves the merge-base, and
// the old one would fold the base's own changes into "PR changes".
func (p *prSession) refreshDiffBase() {
	p.mu.Lock()
	worktree, srcRepo, srcRemote, baseRef, num := p.worktree, p.srcRepo, p.srcRemote, p.meta.BaseRef, p.meta.Number
	p.mu.Unlock()
	mb := gitMergeBase(worktree, "HEAD", prBaseRef(num))
	if mb == "" && srcRepo != "" && srcRemote != "" {
		mb = gitMergeBase(worktree, "HEAD", srcRemote+"/"+baseRef)
	}
	if mb == "" {
		return
	}
	p.mu.Lock()
	p.diffBase = mb
	p.diffBaseWarning = ""
	p.mu.Unlock()
}

// Rewritten reports whether the checkout's history no longer contains the
// PR head on the remote -- a rebase that hasn't been (force-)pushed yet.
// Until it is, the forge still holds the old commits: approving or merging
// would act on those, not on what is checked out.
func (p *prSession) Rewritten() bool {
	p.mu.Lock()
	worktree, head := p.worktree, p.remoteHead()
	p.mu.Unlock()
	return validSHA(head) && !gitIsAncestor(worktree, head, "HEAD")
}

// Push pushes the worktree's current commit to the PR's actual head branch
// on its head repo (which may be a fork). Without force, a rejection means
// the head moved since this checkout or since the last Pull, and the caller
// should Pull before trying again. With force -- only ever after the
// reviewer confirmed pushing a rebase -- it is --force-with-lease against the
// head this session last saw, so commits someone else pushed in the meantime
// are refused rather than silently discarded.
func (p *prSession) Push(force bool) error {
	p.mu.Lock()
	worktree := p.worktree
	cloneURL, headRef := p.meta.HeadRepoCloneURL, p.meta.HeadRef
	token := p.token
	lease := p.remoteHead()
	p.mu.Unlock()
	if force && !validSHA(lease) {
		return errors.New("can't force-push: the PR head this checkout started from is unknown")
	}
	fg := p.git()

	if cloneURL == "" {
		cloneURL = fg.CloneURL
	}
	// However this checkout was cloned (https or ssh), the push goes over https
	// with the forge token, so it never stops to ask for a password.
	pushURL := cloneURL
	if token != "" {
		pushURL = httpsRemoteURL(cloneURL)
	}
	refspec := fmt.Sprintf("HEAD:refs/heads/%s", headRef)
	args := []string{"-C", worktree, "push"}
	if force {
		args = append(args, fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", headRef, lease))
	}
	out, err := gitAuthCmd(fg.Auth, append(args, pushURL, refspec)...).CombinedOutput()
	if err != nil {
		msg := fg.Auth.redact(strings.TrimSpace(string(out)))
		if force && strings.Contains(msg, "stale info") {
			return fmt.Errorf("git push: someone else pushed to %s since this checkout; pull their commits and rebase again: %s", headRef, msg)
		}
		return fmt.Errorf("git push: %w: %s", err, msg)
	}
	// What you pushed is now part of the PR: the PR head moves up to it, so
	// those commits leave "Your changes" and join "PR changes".
	if sha, err := exec.Command("git", "-C", worktree, "rev-parse", "HEAD").Output(); err == nil {
		p.mu.Lock()
		p.meta.HeadSHA = strings.TrimSpace(string(sha))
		p.pushedSHA = ""
		p.mu.Unlock()
	}
	p.refreshDiffBase()
	return nil
}

// ---------------------------------------------------------------- HTTP

func (s *Server) prOrFail(w http.ResponseWriter) bool {
	if s.pr == nil {
		fail(w, http.StatusNotFound, "not a PR review session")
		return false
	}
	return true
}

func (s *Server) handlePRMeta(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	p := s.pr
	p.mu.Lock()
	defer p.mu.Unlock()
	writeJSON(w, map[string]any{
		"number":          p.meta.Number,
		"title":           p.meta.Title,
		"author":          p.meta.Author,
		"base":            p.meta.BaseRef,
		"head":            p.meta.HeadRef,
		"state":           p.meta.State,
		"merged":          p.meta.Merged,
		"mergedAt":        p.meta.MergedAt,
		"writeAccess":     p.writeAccess,
		"readOnly":        p.token == "",
		"draftCount":      len(p.comments),
		"diffBaseWarning": p.diffBaseWarning,
		"headSHA":         p.meta.HeadSHA,
		"url":             p.target.URL,
		"files":           s.ix.PRFiles(),
	})
}

// handlePRExistingComments fetches every comment already posted on the PR
// (top-level and inline) directly from the forge -- always live, never
// cached, since another reviewer may have commented since the page loaded.
func (s *Server) handlePRExistingComments(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	p := s.pr
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	issue, review, err := p.provider.FetchComments(ctx, p.target, p.token)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	if issue == nil {
		issue = []PRComment{}
	}
	if review == nil {
		review = []PRComment{}
	}
	writeJSON(w, map[string]any{"issueComments": issue, "reviewComments": review})
}

// handlePRIssueCommentPost posts a new top-level PR comment immediately (not
// part of the draft-then-submit review flow below, since GitHub's issue
// comments aren't tied to a review). Used both for starting a new top-level
// comment and for "replying" to one, since GitHub doesn't thread these.
func (s *Server) handlePRIssueCommentPost(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	p := s.pr
	if p.token == "" {
		fail(w, http.StatusForbidden, "no auth token configured; posting comments requires a "+p.provider.Info().Label+" token")
		return
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || strings.TrimSpace(body.Body) == "" {
		fail(w, http.StatusBadRequest, "body is required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c, err := p.provider.PostIssueComment(ctx, p.target, p.token, strings.TrimSpace(body.Body))
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, c)
}

// handlePRReviewCommentReply posts an immediate, threaded reply to an
// existing inline review comment (not a new draft: this bypasses the
// draft-then-submit review flow, matching GitHub's own dedicated reply
// endpoint, which posts right away).
func (s *Server) handlePRReviewCommentReply(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	p := s.pr
	if p.token == "" {
		fail(w, http.StatusForbidden, "no auth token configured; posting comments requires a "+p.provider.Info().Label+" token")
		return
	}
	var body struct {
		CommentID int64  `json:"commentId"`
		Body      string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || body.CommentID <= 0 || strings.TrimSpace(body.Body) == "" {
		fail(w, http.StatusBadRequest, "commentId and body are required")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	c, err := p.provider.ReplyToReviewComment(ctx, p.target, p.token, body.CommentID, strings.TrimSpace(body.Body))
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, c)
}

func (s *Server) handlePRComments(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	p := s.pr
	switch r.Method {
	case http.MethodGet:
		p.mu.Lock()
		defer p.mu.Unlock()
		comments := p.comments
		if comments == nil {
			comments = []prComment{}
		}
		writeJSON(w, map[string]any{"comments": comments})
	case http.MethodPost:
		if !localPost(w, r) {
			return
		}
		var body struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Side string `json:"side"`
			Body string `json:"body"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil ||
			body.Path == "" || body.Line <= 0 || strings.TrimSpace(body.Body) == "" {
			fail(w, http.StatusBadRequest, "path, line, and body are required")
			return
		}
		side := strings.ToUpper(body.Side)
		if side != "LEFT" {
			side = "RIGHT"
		}
		p.mu.Lock()
		p.nextID++
		c := prComment{ID: p.nextID, Path: body.Path, Line: body.Line, Side: side, Body: strings.TrimSpace(body.Body)}
		p.comments = append(p.comments, c)
		if s.session != nil {
			s.session.Update(func(ws *WorkspaceSession) { ws.Drafts = p.comments })
		}
		p.mu.Unlock()
		writeJSON(w, c)
	default:
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handlePRCommentDelete(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	p := s.pr
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, c := range p.comments {
		if c.ID == id {
			p.comments = append(p.comments[:i], p.comments[i+1:]...)
			break
		}
	}
	if s.session != nil {
		s.session.Update(func(ws *WorkspaceSession) { ws.Drafts = p.comments })
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (s *Server) handlePRSubmit(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	p := s.pr
	if p.token == "" {
		fail(w, http.StatusForbidden, "no auth token configured; review submission is read-only")
		return
	}
	var body struct {
		Event string `json:"event"`
		Body  string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		fail(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	event := strings.ToUpper(body.Event)
	if event != "APPROVE" && event != "REQUEST_CHANGES" && event != "COMMENT" {
		fail(w, http.StatusBadRequest, "event must be APPROVE, REQUEST_CHANGES, or COMMENT")
		return
	}
	if (event == "APPROVE" || event == "REQUEST_CHANGES") && !p.writeAccess {
		fail(w, http.StatusForbidden, "no push access on this repository; only Comment reviews are allowed")
		return
	}
	p.mu.Lock()
	comments := append([]prComment(nil), p.comments...)
	p.mu.Unlock()

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := p.provider.SubmitReview(ctx, p.target, p.token, p.meta.HeadSHA, comments, event, body.Body); err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	p.mu.Lock()
	p.comments = nil
	if s.session != nil {
		s.session.Update(func(ws *WorkspaceSession) { ws.Drafts = nil })
	}
	p.mu.Unlock()
	writeJSON(w, map[string]any{"ok": true})
}

// prMergeMessage is a commit title and body, as prefilled in the merge dialog.
type prMergeMessage struct {
	Title   string `json:"title"`
	Message string `json:"message"`
}

// prMergeDefaults are the forge-neutral starting points the merge dialog
// prefills for each method that writes a commit: the conventions GitHub,
// GitLab and Gitea all default to. Rebase writes no commit of its own.
// commits is newest first, as git log lists them.
func prMergeDefaults(meta PRMeta, commits []UnpushedCommit) map[string]prMergeMessage {
	var body strings.Builder
	for i := len(commits) - 1; i >= 0; i-- {
		fmt.Fprintf(&body, "* %s\n", commits[i].Subject)
	}
	return map[string]prMergeMessage{
		MergeMethodMerge:  {Title: fmt.Sprintf("Merge pull request #%d from %s", meta.Number, meta.HeadRef), Message: meta.Title},
		MergeMethodSquash: {Title: fmt.Sprintf("%s (#%d)", meta.Title, meta.Number), Message: strings.TrimSpace(body.String())},
	}
}

// prCommits lists the PR's own commits (merge-base..PR head), newest first.
func (s *Server) prCommits() []UnpushedCommit {
	p := s.pr
	p.mu.Lock()
	from, to := p.diffBase, p.meta.HeadSHA
	p.mu.Unlock()
	if !validSHA(from) || !validSHA(to) {
		return nil
	}
	return gitCommitsInRange(s.ix.Root(), from+".."+to, 200)
}

func validMergeMethod(m string) bool {
	return m == MergeMethodMerge || m == MergeMethodSquash || m == MergeMethodRebase
}

// handlePRMergeStatus reports whether the PR can merge and how, plus what the
// merge dialog needs to prefill: per-method default messages and the PR's
// commits. Always live from the forge; mergeability changes under you.
func (s *Server) handlePRMergeStatus(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	p := s.pr
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	st, err := p.provider.MergeStatus(ctx, p.target, p.token)
	if err != nil {
		fail(w, http.StatusBadGateway, err.Error())
		return
	}
	commits := s.prCommits()
	if commits == nil {
		commits = []UnpushedCommit{}
	}
	p.mu.Lock()
	meta, writeAccess := p.meta, p.writeAccess
	p.mu.Unlock()
	root := s.ix.Root()
	hasInstruction := false
	if cfg := readSettings(); cfg.GitCommitMessageInstruction != nil {
		hasInstruction = strings.TrimSpace(*cfg.GitCommitMessageInstruction) != ""
	}
	writeJSON(w, map[string]any{
		"status":   st,
		"canMerge": st.CanMerge && writeAccess && p.token != "",
		// The forge holds commits this checkout hasn't seen: merging now
		// would land code nobody here reviewed. Pull first.
		"stale":          st.HeadSHA != "" && st.HeadSHA != meta.HeadSHA,
		"rewritten":      p.Rewritten(),
		"inProgress":     gitOpInProgress(root),
		"unpushed":       gitCountSince(root, meta.HeadSHA),
		"defaults":       prMergeDefaults(meta, commits),
		"commits":        commits,
		"base":           meta.BaseRef,
		"hasInstruction": hasInstruction,
	})
}

// handlePRMerge merges the PR on the forge, exactly as confirmed in the
// dialog. The forge is told the head the reviewer saw, so a PR that moved
// since is refused there rather than merged unseen.
func (s *Server) handlePRMerge(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	p := s.pr
	if p.token == "" {
		fail(w, http.StatusForbidden, "no auth token configured; merging requires a token")
		return
	}
	if !p.writeAccess {
		fail(w, http.StatusForbidden, "no push access on this repository; merging is not allowed")
		return
	}
	var body struct {
		Method  string `json:"method"`
		Title   string `json:"title"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || !validMergeMethod(body.Method) {
		fail(w, http.StatusBadRequest, "method must be merge, squash, or rebase")
		return
	}
	if op := gitOpInProgress(s.ix.Root()); op != "" {
		fail(w, http.StatusConflict, "a "+op+" is in progress in this checkout; finish or abort it first")
		return
	}
	if p.Rewritten() {
		fail(w, http.StatusConflict, "this checkout was rebased but not pushed; force-push it first, or the forge merges the old commits")
		return
	}
	p.mu.Lock()
	head := p.meta.HeadSHA
	p.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	res, err := p.provider.Merge(ctx, p.target, p.token, PRMergeRequest{
		Method:  body.Method,
		Title:   strings.TrimSpace(body.Title),
		Message: strings.TrimSpace(body.Message),
		HeadSHA: head,
	})
	if err != nil {
		switch {
		case errors.Is(err, ErrPRHeadMoved):
			fail(w, http.StatusConflict, "the PR has new commits since this checkout; pull and review them, then merge")
		case errors.Is(err, ErrPRNotMergeable):
			fail(w, http.StatusConflict, err.Error())
		default:
			fail(w, http.StatusBadGateway, err.Error())
		}
		return
	}
	// The bar flips to Merged from the forge's own account of it.
	if fresh, err := p.provider.FetchPR(ctx, p.target, p.token); err == nil {
		p.mu.Lock()
		p.meta.State, p.meta.Merged, p.meta.MergedAt = fresh.State, fresh.Merged, fresh.MergedAt
		p.mu.Unlock()
	} else {
		p.mu.Lock()
		p.meta.Merged = true
		p.mu.Unlock()
	}
	writeJSON(w, map[string]any{"ok": true, "sha": res.SHA, "message": res.Message})
}

// handlePRMergeMessage has the harness write (or rewrite) the commit message
// a squash or merge commit lands with, from the PR's commits and diff, the
// user's commit message instructions in Settings, and any one-off
// instruction typed in the dialog. It only writes text: px0 shows it in the
// dialog, where it can still be edited before anything is merged.
func (s *Server) handlePRMergeMessage(w http.ResponseWriter, r *http.Request) {
	if !s.prOrFail(w) {
		return
	}
	if !localPost(w, r) {
		return
	}
	if !s.agentOrFail(w) {
		return
	}
	var body struct {
		Method      string `json:"method"`
		Instruction string `json:"instruction"`
		Title       string `json:"title"`
		Message     string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil || (body.Method != MergeMethodSquash && body.Method != MergeMethodMerge) {
		fail(w, http.StatusBadRequest, "method must be squash or merge")
		return
	}
	p := s.pr
	p.mu.Lock()
	meta, from := p.meta, p.diffBase
	p.mu.Unlock()
	if !validSHA(from) {
		fail(w, http.StatusConflict, "the PR's merge base is unresolved, so there is no diff to describe")
		return
	}
	root := s.ix.Root()
	instructions := []string{}
	if cfg := readSettings(); cfg.GitCommitMessageInstruction != nil {
		if t := strings.TrimSpace(*cfg.GitCommitMessageInstruction); t != "" {
			instructions = append(instructions, t)
		}
	}
	if t := strings.TrimSpace(body.Instruction); t != "" {
		instructions = append(instructions, t)
	}
	draft := strings.TrimSpace(strings.TrimSpace(body.Title) + "\n\n" + strings.TrimSpace(body.Message))
	prompt := mergeMessagePrompt(body.Method, meta, s.prCommits(),
		gitRangeStat(root, from, meta.HeadSHA), gitRangeDiff(root, from, meta.HeadSHA), draft, instructions)
	job, err := s.agent.StartPrompt(body.Method+" message", prompt)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, errAgentNone) {
			code = http.StatusBadRequest
		}
		fail(w, code, err.Error())
		return
	}
	writeJSON(w, job)
}

// handleLaunchPR lets an already-running px0 open another PR without
// disturbing its own session: it re-execs itself as a brand new process on
// a new port with "px0 -y <url>". The call returns as soon as the child starts.
func (s *Server) handleLaunchPR(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	var body struct {
		Target string `json:"target"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body); err != nil || strings.TrimSpace(body.Target) == "" {
		fail(w, http.StatusBadRequest, "target is required")
		return
	}
	targetURL := strings.TrimSpace(body.Target)
	if _, _, ok := DetectPRURL(targetURL); !ok {
		fail(w, http.StatusBadRequest, "target must be a valid pull or merge request URL (e.g. "+prURLExamples+")")
		return
	}
	exe, err := os.Executable()
	if err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	cmd := exec.Command(exe, "-y", targetURL)
	cmd.Dir = s.ix.Root()
	if err := cmd.Start(); err != nil {
		fail(w, http.StatusInternalServerError, err.Error())
		return
	}
	go cmd.Wait() // reap without blocking the handler
	writeJSON(w, map[string]any{"ok": true})
}
