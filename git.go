package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// gitDisabled turns off all git awareness (the -no-git flag). Like uiQuiet, a
// process-wide switch set once in main before anything reads it.
var gitDisabled bool

type gitInfo struct {
	ok       bool
	toplevel string // repo root as git reports it (symlinks resolved)
	gitdir   string // absolute path to .git directory or file
}

var (
	gitMu    sync.Mutex
	gitCache = map[string]gitInfo{}
)

// gitAvailable reports whether the git binary is on PATH and root sits inside a
// working tree. Memoized per root: detection shells out once. Fails quiet -- no
// git, no repo, or -no-git all yield false, never an error.
func gitAvailable(root string) bool { return gitProbe(root).ok }

// gitDir returns the absolute path to the repository's .git directory.
func gitDir(root string) string { return gitProbe(root).gitdir }

func gitProbe(root string) gitInfo {
	if gitDisabled {
		return gitInfo{}
	}
	gitMu.Lock()
	defer gitMu.Unlock()
	if info, ok := gitCache[root]; ok {
		if info.ok && info.gitdir != "" {
			if _, err := os.Lstat(info.gitdir); err == nil {
				return info
			}
		}
		delete(gitCache, root)
	}
	var info gitInfo
	if _, err := exec.LookPath("git"); err == nil {
		out, err := exec.Command("git", "-C", root, "rev-parse", "--show-toplevel").Output()
		safeDir := false
		if err != nil {
			if out2, err2 := exec.Command("git", "-c", "safe.directory=*", "-C", root, "rev-parse", "--show-toplevel").Output(); err2 == nil {
				out, err = out2, nil
				safeDir = true
			}
		}
		if err == nil {
			top := strings.TrimSpace(string(out))
			gd := filepath.Join(top, ".git")
			var gdCmd *exec.Cmd
			if safeDir {
				gdCmd = exec.Command("git", "-c", "safe.directory=*", "-C", root, "rev-parse", "--git-dir")
			} else {
				gdCmd = exec.Command("git", "-C", root, "rev-parse", "--git-dir")
			}
			if gdOut, err := gdCmd.Output(); err == nil {
				rawGd := strings.TrimSpace(string(gdOut))
				if filepath.IsAbs(rawGd) {
					gd = rawGd
				} else {
					gd = filepath.Join(top, rawGd)
				}
			}
			info = gitInfo{ok: true, toplevel: top, gitdir: gd}
			gitCache[root] = info
		}
	}
	return info
}

// gitStatus maps repo-relative-to-served-root path -> single-letter status for
// every file git considers changed. Uses porcelain v2 -z, the stable
// null-delimited format. Fails quiet: nil on any error, no repo, or disabled.
func gitStatus(root string) map[string]string {
	return gitStatusAgainst(root, "HEAD")
}

// repoRelKey returns a function mapping a git-porcelain path (always relative
// to the repo toplevel) to a path relative to root, or false when it falls
// outside root's subtree (root may be a subdirectory of the repo).
func repoRelKey(info gitInfo, root string) func(string) (string, bool) {
	prefix := ""
	if rel, err := filepath.Rel(info.toplevel, root); err == nil && rel != "." {
		prefix = filepath.ToSlash(rel) + "/"
	}
	return func(p string) (string, bool) {
		if prefix == "" {
			return p, true
		}
		if !strings.HasPrefix(p, prefix) {
			return "", false // outside the served subtree
		}
		return p[len(prefix):], true
	}
}

// gitStatusAndStagedAgainst runs a single `git status --porcelain=v2 -z -uall`
// command to extract both working-tree status codes and staged index paths,
// and optionally overlays diffs against base and head (for PR review mode)
// without spawning duplicate git status processes.
func gitStatusAndStagedAgainst(root, base, head string) (status map[string]string, staged map[string]bool, yourStatus map[string]string) {
	info := gitProbe(root)
	if !info.ok {
		return nil, nil, nil
	}
	// Background status must not refresh the index and race explicit writes
	// (stage/commit) for index.lock.
	out, err := exec.Command("git", "--no-optional-locks", "-C", root, "status", "--porcelain=v2", "-z", "-uall").Output()
	if err != nil {
		return nil, nil, nil
	}
	key := repoRelKey(info, root)

	wtStatus := map[string]string{}
	staged = map[string]bool{}
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "" {
			continue
		}
		switch f[0] {
		case '?': // "? <path>"
			if k, ok := key(f[2:]); ok {
				wtStatus[k] = "U" // untracked
			}
		case '1': // "1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>"
			p := strings.SplitN(f, " ", 9)
			if len(p) == 9 {
				if k, ok := key(p[8]); ok {
					wtStatus[k] = mapXY(p[1])
					if len(p[1]) > 0 && p[1][0] != '.' {
						staged[k] = true
					}
				}
			}
		case '2': // "2 <XY> ... <Rscore> <path>", then original path in the next field
			p := strings.SplitN(f, " ", 10)
			if len(p) == 10 {
				if k, ok := key(p[9]); ok {
					wtStatus[k] = mapXY(p[1])
					if len(p[1]) > 0 && p[1][0] != '.' {
						staged[k] = true
					}
				}
			}
			i++ // the original path follows as its own NUL-terminated field
		case 'u': // "u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>"
			p := strings.SplitN(f, " ", 11)
			if len(p) == 11 {
				if k, ok := key(p[10]); ok {
					wtStatus[k] = "!" // unmerged / conflict
					staged[k] = true
				}
			}
		}
	}

	overlayDiff := func(ref string) map[string]string {
		m := make(map[string]string, len(wtStatus))
		for k, v := range wtStatus {
			m[k] = v
		}
		if ref != "" && ref != "HEAD" {
			if diffOut, err := exec.Command("git", "-C", root, "diff", "--name-status", "-z", ref).Output(); err == nil {
				parseNameStatusZ(diffOut, key, m, false)
			}
		}
		if len(m) == 0 {
			return nil
		}
		return m
	}

	if head != "" {
		yourStatus = overlayDiff(head)
	}
	if base != "" && base != "HEAD" {
		status = overlayDiff(base)
	} else if len(wtStatus) > 0 {
		status = wtStatus
	}
	if len(staged) == 0 {
		staged = nil
	}
	return status, staged, yourStatus
}

// gitStatusAgainst maps changed files relative to root against base.
// When base is "HEAD" or empty, it returns working-tree changes only.
// When base is an arbitrary commit or ref (such as a PR merge-base),
// it includes both files changed against base and working-tree changes.
func gitStatusAgainst(root, base string) map[string]string {
	st, _, _ := gitStatusAndStagedAgainst(root, base, "")
	return st
}

// mapXY collapses a porcelain v2 two-letter XY code (X=index, Y=worktree) into
// a single status letter, preferring the staged side when both are set.
func mapXY(xy string) string {
	if len(xy) < 2 {
		return "M"
	}
	c := xy[0]
	if c == '.' {
		c = xy[1]
	}
	switch c {
	case 'A':
		return "A"
	case 'D':
		return "D"
	case 'R':
		return "R"
	case 'C':
		return "C"
	case 'U':
		return "!" // unmerged / conflict
	default: // M (modified), T (typechange) and anything else read as modified
		return "M"
	}
}

// parseNameStatusZ parses `git diff --name-status -z` output into dst.
func parseNameStatusZ(out []byte, key func(string) (string, bool), dst map[string]string, overwrite bool) {
	parts := strings.Split(string(out), "\x00")
	for i := 0; i < len(parts); i++ {
		st := parts[i]
		if st == "" {
			continue
		}
		code := st[:1]
		if code == "R" || code == "C" {
			i += 2 // R<score> \0 <src> \0 <dst>
		} else {
			i++
		}
		if i < len(parts) {
			if k, ok := key(parts[i]); ok {
				if overwrite {
					dst[k] = code
				} else if _, exists := dst[k]; !exists {
					dst[k] = code
				}
			}
		}
	}
}

// gitStagedPaths maps repo-relative-to-served-root path -> true for every
// file with staged (index) changes. Used to drive the stage tick in the file
// tree and to gate gitCommit. Fails quiet: nil on any error or no repo.
func gitStagedPaths(root string) map[string]bool {
	files := gitStagedFiles(root)
	if len(files) == 0 {
		return nil
	}
	staged := make(map[string]bool, len(files))
	for _, k := range files {
		staged[k] = true
	}
	return staged
}

// gitStagedFiles returns a list of repo-relative paths staged in the index.
func gitStagedFiles(root string) []string {
	info := gitProbe(root)
	if !info.ok {
		return nil
	}
	out, err := exec.Command("git", "-C", root, "diff", "--name-only", "--cached", "-z").Output()
	if err != nil {
		return nil
	}
	key := repoRelKey(info, root)
	var files []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p == "" {
			continue
		}
		if k, ok := key(p); ok {
			files = append(files, k)
		}
	}
	return files
}

// gitStagedStat returns the diffstat summary of staged changes against HEAD.
// Fails quiet -> "".
func gitStagedStat(root string) string {
	if !gitAvailable(root) {
		return ""
	}
	out, err := exec.Command("git", "-C", root, "diff", "--stat", "--cached").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

const (
	maxStagedDiffBytes = 32 * 1024 // 32 KB limit on staged diff to stay safely under ARG_MAX / MAX_ARG_STRLEN
)

var lockfileExclusions = []string{
	":(exclude)*package-lock.json",
	":(exclude)*yarn.lock",
	":(exclude)*pnpm-lock.yaml",
	":(exclude)*go.sum",
	":(exclude)*Cargo.lock",
	":(exclude)*composer.lock",
	":(exclude)*poetry.lock",
	":(exclude)*Gemfile.lock",
	":(exclude)*.min.js",
	":(exclude)*.min.css",
	":(exclude)*.map",
}

// gitStagedDiff returns the unified diff of the index (staged changes)
// against HEAD, for handing to a coding harness asked to write a commit
// message. Large diffs are capped to maxStagedDiffBytes and noise files
// (lockfiles, minified assets) are excluded when other changes exist.
// Fails quiet -> "".
func gitStagedDiff(root string) string {
	return gitPromptDiff(root, "--cached")
}

// gitRangeDiff is gitStagedDiff for the commits between from and to: what a
// squash of a PR would land, trimmed the same way for a harness prompt.
func gitRangeDiff(root, from, to string) string {
	return gitPromptDiff(root, from, to)
}

// gitRangeStat is the diffstat between from and to.
func gitRangeStat(root, from, to string) string {
	if !gitAvailable(root) {
		return ""
	}
	out, err := exec.Command("git", "-C", root, "diff", "--stat", from, to).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func gitPromptDiff(root string, diffArgs ...string) string {
	if !gitAvailable(root) {
		return ""
	}
	base := append([]string{"-C", root, "diff", "--no-color"}, diffArgs...)
	// Try fetching the diff excluding high-noise files (lockfiles, minified bundles)
	cmdArgs := append(append(append([]string(nil), base...), "--", "."), lockfileExclusions...)
	out, err := exec.Command("git", cmdArgs...).Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		// Fallback to unfiltered diff if excluded diff was empty or failed (e.g. only lockfiles were modified)
		out, err = exec.Command("git", base...).Output()
		if err != nil {
			return ""
		}
	}
	if len(out) > maxStagedDiffBytes {
		cut := maxStagedDiffBytes
		if idx := bytes.LastIndexByte(out[:cut], '\n'); idx > 0 {
			cut = idx
		}
		return string(out[:cut]) + "\n\n[Diff truncated: showing first 32KB. See changed files and summary above for full list of changes]"
	}
	return string(out)
}

// gitHasUncommittedChanges reports whether the working tree or index has any
// changes at all (staged, unstaged, or untracked). Used to gate commit and
// pull -- a pull is refused outright when there's anything uncommitted,
// rather than risking it colliding with incoming changes.
func gitHasUncommittedChanges(root string) bool {
	if !gitAvailable(root) {
		return false
	}
	out, err := exec.Command("git", "--no-optional-locks", "-C", root, "status", "--porcelain", "-uall").Output()
	if err != nil {
		return false
	}
	return len(strings.TrimSpace(string(out))) > 0
}

// gitStage adds relpath to the index. An empty relpath (the served root
// itself) means "stage everything", so a bare "Stage All" action can reuse
// this instead of a separate endpoint.
func gitStage(root, relpath string) error {
	if relpath == "" {
		relpath = "."
	}
	out, err := exec.Command("git", "-C", root, "add", "-A", "--", relpath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitUnstage removes relpath from the index without touching the working tree.
func gitUnstage(root, relpath string) error {
	if relpath == "" {
		relpath = "."
	}
	out, err := exec.Command("git", "-C", root, "reset", "HEAD", "--", relpath).CombinedOutput()
	if err != nil {
		// On an unborn HEAD (a repository with no commits yet), `git reset`
		// fails because HEAD does not resolve; `git rm --cached` removes the
		// path from the index without touching the working tree.
		if _, rmErr := exec.Command("git", "-C", root, "rm", "-r", "--cached", "--", relpath).CombinedOutput(); rmErr == nil {
			return nil
		}
		return fmt.Errorf("git reset: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitApplyHunk applies a single unified diff hunk for relpath to the index
// ("stage" / "unstage") or working tree ("discard" reverses the hunk in place).
func gitApplyHunk(root, relpath, hunk, action string) error {
	info := gitProbe(root)
	if !info.ok {
		return errors.New("git repository not available")
	}
	hunk = strings.TrimRight(hunk, "\r\n")
	if !strings.HasPrefix(hunk, "@@") {
		return errors.New("hunk must start with @@ header")
	}
	absTarget := filepath.Join(root, filepath.FromSlash(relpath))
	repoRel, err := filepath.Rel(info.toplevel, absTarget)
	if err != nil {
		return err
	}
	repoRel = filepath.ToSlash(repoRel)

	var patch strings.Builder
	fmt.Fprintf(&patch, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n%s\n", repoRel, repoRel, repoRel, repoRel, hunk)
	patchBytes := []byte(patch.String())

	runApply := func(flags ...string) ([]byte, error) {
		args := append([]string{"-C", info.toplevel, "apply", "--unidiff-zero", "--whitespace=nowarn"}, flags...)
		args = append(args, "-")
		cmd := exec.Command("git", args...)
		cmd.Stdin = bytes.NewReader(patchBytes)
		return cmd.CombinedOutput()
	}

	switch action {
	case "stage":
		if out, err := runApply("--cached"); err != nil {
			return fmt.Errorf("git apply --cached: %w: %s", err, strings.TrimSpace(string(out)))
		}
	case "unstage":
		if out, err := runApply("--cached", "-R"); err != nil {
			return fmt.Errorf("git apply --cached -R: %w: %s", err, strings.TrimSpace(string(out)))
		}
	case "discard":
		// Try reversing against both index and worktree first (in case the hunk was
		// staged), falling back to worktree-only when the hunk is unstaged.
		if _, err := runApply("--index", "-R"); err != nil {
			if out, err2 := runApply("-R"); err2 != nil {
				return fmt.Errorf("git apply -R: %w: %s", err2, strings.TrimSpace(string(out)))
			}
		}
	default:
		return fmt.Errorf("unknown hunk action %q", action)
	}
	return nil
}

// gitSnapshotWorktree captures the entire working tree (including unstaged and
// untracked files) as a ref-free commit SHA in the repository's object database
// using a temporary index in os.TempDir(). Neither HEAD nor the real git index
// is modified. Returns "" if git is unavailable or the snapshot fails.
func gitSnapshotWorktree(root string) string {
	info := gitProbe(root)
	if !info.ok {
		return ""
	}
	tmpFile, err := os.CreateTemp("", "px0-idx-*")
	if err != nil {
		return ""
	}
	tmpIdx := tmpFile.Name()
	tmpFile.Close()
	defer os.Remove(tmpIdx)

	// Seed the temp index from the real index so git add only hashes modified/untracked files.
	realIdx := filepath.Join(info.gitdir, "index")
	if data, err := os.ReadFile(realIdx); err == nil {
		_ = os.WriteFile(tmpIdx, data, 0o600)
	} else {
		_ = os.Remove(tmpIdx)
	}

	env := append(os.Environ(),
		"GIT_INDEX_FILE="+tmpIdx,
		"GIT_AUTHOR_NAME=px0",
		"GIT_AUTHOR_EMAIL=px0@localhost",
		"GIT_COMMITTER_NAME=px0",
		"GIT_COMMITTER_EMAIL=px0@localhost",
	)

	addCmd := exec.Command("git", "-C", root, "add", "-A")
	addCmd.Env = env
	if addCmd.Run() != nil {
		return ""
	}

	wtCmd := exec.Command("git", "-C", root, "write-tree")
	wtCmd.Env = env
	treeOut, err := wtCmd.Output()
	if err != nil {
		return ""
	}
	treeSHA := strings.TrimSpace(string(treeOut))
	if !validSHA(treeSHA) {
		return ""
	}

	ctArgs := []string{"-C", root, "commit-tree", treeSHA, "-m", "px0 turn snapshot"}
	if headOut, err := exec.Command("git", "-C", root, "rev-parse", "--verify", "HEAD").Output(); err == nil {
		if headSHA := strings.TrimSpace(string(headOut)); validSHA(headSHA) {
			ctArgs = []string{"-C", root, "commit-tree", treeSHA, "-p", headSHA, "-m", "px0 turn snapshot"}
		}
	}
	ctCmd := exec.Command("git", ctArgs...)
	ctCmd.Env = env
	commitOut, err := ctCmd.Output()
	if err != nil {
		return ""
	}
	sha := strings.TrimSpace(string(commitOut))
	if !validSHA(sha) {
		return ""
	}
	return sha
}

// gitRestoreSnapshotPaths restores the specified root-relative paths to their
// state in snapshotSHA, removing any path that did not exist in snapshotSHA.
func gitRestoreSnapshotPaths(root, snapshotSHA string, paths []string) error {
	info := gitProbe(root)
	if !info.ok {
		return errors.New("git repository not available")
	}
	if !validSHA(snapshotSHA) {
		return errors.New("invalid snapshot SHA")
	}
	for _, rel := range paths {
		clean := filepath.Clean(filepath.FromSlash(rel))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
			continue
		}
		abs := filepath.Join(root, clean)
		repoRel, err := filepath.Rel(info.toplevel, abs)
		if err != nil {
			continue
		}
		repoRel = filepath.ToSlash(repoRel)

		// Check if the path existed in snapshotSHA.
		if exec.Command("git", "-C", info.toplevel, "cat-file", "-e", snapshotSHA+":"+repoRel).Run() == nil {
			if out, err := exec.Command("git", "-C", info.toplevel, "checkout", snapshotSHA, "--", repoRel).CombinedOutput(); err != nil {
				return fmt.Errorf("git checkout %s: %w: %s", rel, err, strings.TrimSpace(string(out)))
			}
			// Reset index entry to HEAD if HEAD has it, or unstage if it was newly added.
			_ = exec.Command("git", "-C", info.toplevel, "reset", "-q", "HEAD", "--", repoRel).Run()
		} else {
			_ = exec.Command("git", "-C", info.toplevel, "rm", "-f", "--ignore-unmatch", "--cached", "--", repoRel).Run()
			_ = os.Remove(abs)
		}
	}
	return nil
}

// gitCommit commits whatever is currently staged. Refuses up front when
// nothing is staged so the caller gets a clear message instead of git's own
// "nothing to commit" noise.
func gitCommit(root, message string) error {
	if len(gitStagedPaths(root)) == 0 {
		return errors.New("nothing staged to commit")
	}
	out, err := exec.Command("git", "-C", root, "commit", "-m", message).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitCurrentBranch returns the checked-out branch name, or "HEAD" when
// detached (e.g. inside a PR review worktree).
func gitCurrentBranch(root string) string {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		if sym, err2 := exec.Command("git", "-C", root, "symbolic-ref", "--quiet", "--short", "HEAD").Output(); err2 == nil {
			return strings.TrimSpace(string(sym))
		}
		return ""
	}
	return strings.TrimSpace(string(out))
}

// errNotFastForward is returned by gitFFOnlyPull when fetching ref would not
// fast-forward the current branch; resolving that is not supported.
var errNotFastForward = errors.New("not a fast-forward")

// gitFFOnlyPull fetches ref from remote and fast-forwards the current branch
// onto it. It never touches history any other way: if the merge would not be
// a clean fast-forward, it returns errNotFastForward without modifying
// anything, leaving conflict resolution to the user in a terminal.
func gitFFOnlyPull(root, remote, ref string) error {
	if out, err := exec.Command("git", "-C", root, "fetch", "--no-tags", remote, ref).CombinedOutput(); err != nil {
		return fmt.Errorf("git fetch: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if _, err := exec.Command("git", "-C", root, "merge", "--ff-only", "FETCH_HEAD").CombinedOutput(); err != nil {
		return errNotFastForward
	}
	return nil
}

// gitOpInProgress names the history-rewriting operation left half-done in
// the repository -- "rebase" or "merge" -- or "" when there is none. A
// conflicted rebase or merge (from a harness that gave up, or a terminal)
// would otherwise just look like a dirty working tree.
func gitOpInProgress(root string) string {
	gd := gitDir(root)
	if gd == "" {
		return ""
	}
	exists := func(name string) bool {
		_, err := os.Stat(filepath.Join(gd, name))
		return err == nil
	}
	if exists("rebase-merge") || exists("rebase-apply") {
		return "rebase"
	}
	if exists("MERGE_HEAD") {
		return "merge"
	}
	return ""
}

// gitRebasingBranch returns the branch a rebase in progress is rewriting
// (HEAD itself is detached until it finishes), or "" when there is none.
func gitRebasingBranch(root string) string {
	gd := gitDir(root)
	if gd == "" {
		return ""
	}
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		if b, err := os.ReadFile(filepath.Join(gd, dir, "head-name")); err == nil {
			return strings.TrimPrefix(strings.TrimSpace(string(b)), "refs/heads/")
		}
	}
	return ""
}

// gitIsAncestor reports whether commit a is an ancestor of (or equal to) b.
func gitIsAncestor(root, a, b string) bool {
	return exec.Command("git", "-C", root, "merge-base", "--is-ancestor", a, b).Run() == nil
}

// gitAheadBehindRef counts the commits HEAD has that ref does not, and the
// other way round. Both are 0 when ref doesn't resolve.
func gitAheadBehindRef(root, ref string) (ahead, behind int) {
	out, err := exec.Command("git", "-C", root, "rev-list", "--left-right", "--count", "HEAD..."+ref).Output()
	if err != nil {
		return 0, 0
	}
	parts := strings.Fields(string(out))
	if len(parts) >= 2 {
		ahead, _ = strconv.Atoi(parts[0])
		behind, _ = strconv.Atoi(parts[1])
	}
	return ahead, behind
}

// gitAbortOp aborts the rebase or merge in progress, restoring the branch to
// where it was before that operation started.
func gitAbortOp(root string) error {
	op := gitOpInProgress(root)
	if op == "" {
		return errors.New("no rebase or merge in progress")
	}
	out, err := exec.Command("git", "-C", root, op, "--abort").CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %s --abort: %w: %s", op, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// gitPush pushes the current branch to its configured remote. Returns
// trimmed combined output so the caller can recognize specific failures
// (e.g. no upstream configured) in the error text.
func gitPush(root string) (string, error) {
	out, err := exec.Command("git", "-C", root, "push").CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// gitPushSetUpstream pushes branch to remote and records it as the
// upstream, for a first push when the branch has none configured yet.
func gitPushSetUpstream(root, remote, branch string) (string, error) {
	out, err := exec.Command("git", "-C", root, "push", "-u", remote, branch).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// gitUpstream returns the remote and remote-branch name of branch's
// upstream (`@{u}`), or ok=false if none is configured.
func gitUpstream(root, branch string) (remote, remoteBranch string, ok bool) {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--abbrev-ref", branch+"@{u}").Output()
	if err != nil {
		return "", "", false
	}
	remote, remoteBranch, found := strings.Cut(strings.TrimSpace(string(out)), "/")
	return remote, remoteBranch, found
}

// gitAheadBehind returns the number of commits HEAD is ahead and behind its
// upstream tracking branch (@{u}). If no upstream tracking branch is configured,
// it checks against origin/<branch> if available, or counts local commits if a
// remote is configured so initial publish pushes are permitted.
func gitAheadBehind(root string) (ahead, behind int, hasUpstream bool) {
	if !gitAvailable(root) {
		return 0, 0, false
	}
	out, err := exec.Command("git", "-C", root, "rev-list", "--left-right", "--count", "HEAD...@{u}").Output()
	if err == nil {
		parts := strings.Fields(string(out))
		if len(parts) >= 2 {
			a, _ := strconv.Atoi(parts[0])
			b, _ := strconv.Atoi(parts[1])
			return a, b, true
		}
	}

	branch := gitCurrentBranch(root)
	if branch != "" && branch != "HEAD" {
		if out, err := exec.Command("git", "-C", root, "rev-list", "--left-right", "--count", "HEAD...origin/"+branch).Output(); err == nil {
			parts := strings.Fields(string(out))
			if len(parts) >= 2 {
				a, _ := strconv.Atoi(parts[0])
				b, _ := strconv.Atoi(parts[1])
				return a, b, true
			}
		}

		if gitRemoteURL(root, "") != "" {
			if out, err := exec.Command("git", "-C", root, "rev-list", "--count", "HEAD").Output(); err == nil {
				c, _ := strconv.Atoi(strings.TrimSpace(string(out)))
				if c > 0 {
					return c, 0, false
				}
			}
		}
	}

	return 0, 0, false
}

// gitDiffFull returns the whole unified diff for `git diff <refs...>`, capped at
// max bytes (with a marker line when cut). Untracked files are not part of a git
// diff; callers that care list them separately (gitUntracked).
func gitDiffFull(root string, max int, refs ...string) string {
	if !gitAvailable(root) {
		return ""
	}
	args := append([]string{"-C", root, "diff", "--no-color", "--no-ext-diff"}, refs...)
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}
	if max > 0 && len(out) > max {
		return string(out[:max]) + "\n… diff truncated; run git diff for the rest …\n"
	}
	return string(out)
}

// gitUntracked lists untracked, non-ignored files relative to the served root.
func gitUntracked(root string) []string {
	info := gitProbe(root)
	if !info.ok {
		return nil
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "--others", "--exclude-standard", "-z").Output()
	if err != nil {
		return nil
	}
	var files []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	return files
}

// gitFilesBetween maps each path that differs between two commits to its
// name-status letter (A, M, D, R...), relative to the served root (paths outside
// it are dropped). PR review uses it for the PR's own file set and statuses:
// diffBase..head, fixed until the next Pull.
func gitFilesBetween(root, from, to string) map[string]string {
	info := gitProbe(root)
	if !info.ok || from == "" || to == "" {
		return nil
	}
	out, err := exec.Command("git", "-C", root, "diff", "--name-status", "-z", from, to).Output()
	if err != nil {
		return nil
	}
	files := map[string]string{}
	parseNameStatusZ(out, repoRelKey(info, root), files, true)
	return files
}

// gitDiffAgainst is gitDiff generalized to an arbitrary base ref, so a PR
// review session (pr.go) can diff a file against the merge-base with the
// PR's target branch instead of the working tree's HEAD.
func gitDiffAgainst(root, relpath, base string) string {
	if !gitAvailable(root) {
		return ""
	}
	out, err := exec.Command("git", "-C", root, "diff", "--no-color", base, "--", relpath).Output()
	if err != nil {
		if base == "HEAD" {
			// Unborn HEAD (newly initialized repo with no commits yet): diff against the empty tree.
			if out2, err2 := exec.Command("git", "-C", root, "diff", "--no-color", "4b825dc642cb6eb9a060e54bf8d69288fbee4904", "--", relpath).Output(); err2 == nil {
				return string(out2)
			}
		}
		return ""
	}
	return string(out)
}

// gitDiffBetween is gitDiffAgainst generalized to a two-dot diff between two
// commits, rather than a commit against the working tree. A PR review
// session uses it to render the PR's own diff (merge-base..head) separately
// from the reviewer's local changes since checkout (head..working tree),
// which gitDiffAgainst covers.
func gitDiffBetween(root, relpath, from, to string) string {
	if !gitAvailable(root) {
		return ""
	}
	out, err := exec.Command("git", "-C", root, "diff", "--no-color", from, to, "--", relpath).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// gitMergeBase returns the merge-base commit of a and b, or "" if it cannot
// be determined (e.g. b was never fetched locally).
func gitMergeBase(root, a, b string) string {
	if !gitAvailable(root) {
		return ""
	}
	out, err := exec.Command("git", "-C", root, "merge-base", a, b).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitHunksAgainst parses the unified diff of relpath against base into
// 1-based NEW-FILE line numbers for a change gutter: added lines, modified
// (replaced) lines, and one marker per pure-deletion run (the new-file line
// immediately preceding the removed run; 0 means "before the first line").
// Fails quiet: empty when git is off/unavailable or the file has no diff
// against base (clean/untracked). base is "HEAD" for the working-tree
// gutter, or a PR's merge-base in review mode (server.go's diffBase).
func gitHunksAgainst(root, relpath, base string) (added, modified, deleted []int) {
	return parseDiffHunks(gitDiffAgainst(root, relpath, base))
}

// parseDiffHunks is gitHunksAgainst's body, split out so a commit's own diff
// (gitDiffCommit) can feed the same gutter parser as a diff against a ref.
func parseDiffHunks(diff string) (added, modified, deleted []int) {
	if diff == "" {
		return nil, nil, nil
	}
	newLine := 0
	inHunk := false
	// Current block: a maximal run of consecutive '+'/'-' lines.
	dels := 0
	var adds []int
	blockStart := 0 // newLine when the block began (for deletion markers)
	flush := func() {
		switch {
		case dels > 0 && len(adds) > 0:
			modified = append(modified, adds...) // replacement
		case len(adds) > 0:
			added = append(added, adds...) // pure insertion
		case dels > 0:
			deleted = append(deleted, blockStart-1) // pure deletion
		}
		dels, adds = 0, nil
	}
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "@@"):
			flush()
			inHunk = true
			newLine = parseNewStart(line)
		case !inHunk, strings.HasPrefix(line, "\\"): // pre-hunk header / "\ No newline"
			// skip: neither +/- nor a new-file line
		case strings.HasPrefix(line, "+"):
			if dels == 0 && len(adds) == 0 {
				blockStart = newLine
			}
			adds = append(adds, newLine)
			newLine++
		case strings.HasPrefix(line, "-"):
			if dels == 0 && len(adds) == 0 {
				blockStart = newLine
			}
			dels++
		default: // context line (" ...", or the trailing empty split element)
			flush()
			newLine++
		}
	}
	flush()
	return added, modified, deleted
}

// parseNewStart pulls newStart out of a hunk header "@@ -a,b +c,d @@".
func parseNewStart(hdr string) int {
	i := strings.IndexByte(hdr, '+')
	if i < 0 {
		return 1
	}
	rest := hdr[i+1:]
	if end := strings.IndexAny(rest, ", "); end >= 0 {
		rest = rest[:end]
	}
	if n, err := strconv.Atoi(rest); err == nil {
		return n
	}
	return 1
}

// GitCommit represents a single commit in git log.
type GitCommit struct {
	Hash    string `json:"hash"`
	Full    string `json:"full"` // full SHA, what a pinned tab's ref is
	Subject string `json:"subject"`
	Author  string `json:"author"`
	Date    string `json:"date"`
}

// gitRecentCommits returns up to count recent commits from HEAD.
func gitRecentCommits(root string, count int) []GitCommit {
	if count <= 0 {
		count = 5
	}
	out, err := exec.Command("git", "-C", root, "log", fmt.Sprintf("-n%d", count), "--format=%h%x1f%s%x1f%an%x1f%cr%x1f%H").Output()
	if err != nil {
		return []GitCommit{}
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	commits := make([]GitCommit, 0, len(lines))
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\x1f")
		c := GitCommit{
			Hash: parts[0],
		}
		if len(parts) > 1 {
			c.Subject = parts[1]
		}
		if len(parts) > 2 {
			c.Author = parts[2]
		}
		if len(parts) > 3 {
			c.Date = parts[3]
		}
		if len(parts) > 4 {
			c.Full = parts[4]
		}
		commits = append(commits, c)
	}
	return commits
}

/* ---------- unpushed commits (@{u}..HEAD) ----------

   Everything below reads commits that exist locally but not on the tracking
   branch, so they can be reviewed in px0 before they are pushed. All of it is
   strictly read-only and fails quiet: no upstream, no git, or a bad SHA all
   yield an empty result rather than an error, and the sidebar section simply
   doesn't appear. */

// UnpushedCommit is one commit in @{u}..HEAD, newest first.
type UnpushedCommit struct {
	Hash    string `json:"hash"`    // full SHA, what every follow-up call passes back
	Short   string `json:"short"`   // abbreviated SHA for display
	Subject string `json:"subject"` // first line of the message
	Author  string `json:"author"`
	Date    string `json:"date"` // relative ("2 hours ago")
}

// CommitFile is one path a commit touched, with git's name-status letter
// (M/A/D/R/C/T) -- the same alphabet the file tree badges working-tree changes
// with, so the commit's file list reads identically.
type CommitFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
	From   string `json:"from,omitempty"` // previous path, for R/C only
	Add    int    `json:"add"`
	Del    int    `json:"del"`
	Binary bool   `json:"binary,omitempty"` // numstat reports "-" for its line counts
}

// CommitDetail is everything the commit hover card shows: who wrote it, the
// full message, and its diffstat.
type CommitDetail struct {
	Hash       string `json:"hash"`
	Short      string `json:"short"`
	Subject    string `json:"subject"`
	Message    string `json:"message"` // full message, subject line included
	Author     string `json:"author"`
	Email      string `json:"email"`
	Date       string `json:"date"`    // relative
	DateISO    string `json:"dateIso"` // exact, for the title attribute
	Files      int    `json:"files"`
	Insertions int    `json:"insertions"`
	Deletions  int    `json:"deletions"`
	AuthorURL  string `json:"authorUrl,omitempty"` // best-effort GitHub profile/search link
	CommitURL  string `json:"commitUrl,omitempty"`
}

// shaRe bounds what reaches git as a revision. Every SHA the frontend sends
// came from a list px0 itself produced, so anything else is a bug or a probe;
// refusing it keeps a crafted "--upload-pack=..." out of an exec argument.
var shaRe = regexp.MustCompile(`^[0-9a-fA-F]{4,40}$`)

func validSHA(sha string) bool { return shaRe.MatchString(sha) }

// gitUpstreamRef returns the tracking branch HEAD is configured to push to, as
// git names it ("origin/master"), or "" when none is configured. Unlike
// gitAheadBehind there is no origin/<branch> fallback: an unpushed list is
// only meaningful against a branch git itself considers upstream, and guessing
// one would quietly measure against the wrong ref.
func gitUpstreamRef(root string) string {
	if !gitAvailable(root) {
		return ""
	}
	out, err := exec.Command("git", "-C", root, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitUnpushedCommits lists the commits in @{u}..HEAD, newest first, along with
// the upstream ref they are measured against. Both are empty when no upstream
// is configured or nothing is ahead.
func gitUnpushedCommits(root string, limit int) (upstream string, commits []UnpushedCommit) {
	upstream = gitUpstreamRef(root)
	if upstream == "" {
		return "", nil
	}
	return upstream, gitCommitsInRange(root, "@{u}..HEAD", limit)
}

// gitCommitsSince lists the commits in base..HEAD, newest first. PR review
// checkouts sit on a detached HEAD with no upstream, so the PR head they were
// checked out at (or last pushed to) stands in as the boundary.
func gitCommitsSince(root, base string, limit int) []UnpushedCommit {
	if !validSHA(base) {
		return nil
	}
	return gitCommitsInRange(root, base+"..HEAD", limit)
}

// gitCountSince returns how many commits HEAD has that base does not.
func gitCountSince(root, base string) int {
	if !validSHA(base) {
		return 0
	}
	out, err := exec.Command("git", "-C", root, "rev-list", "--count", base+"..HEAD").Output()
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return n
}

func gitCommitsInRange(root, rng string, limit int) (commits []UnpushedCommit) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	out, err := exec.Command("git", "-C", root, "log", fmt.Sprintf("-n%d", limit),
		"--format=%H%x1f%h%x1f%s%x1f%an%x1f%cr", rng).Output()
	if err != nil {
		return nil
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\x1f")
		c := UnpushedCommit{Hash: f[0]}
		if len(f) > 1 {
			c.Short = f[1]
		}
		if len(f) > 2 {
			c.Subject = f[2]
		}
		if len(f) > 3 {
			c.Author = f[3]
		}
		if len(f) > 4 {
			c.Date = f[4]
		}
		commits = append(commits, c)
	}
	return commits
}

// commitDiffArgs are the diff-tree flags every per-commit read shares: the
// commit's own change against its first parent (--root so the very first
// commit still has one to show), one entry per path rather than per tree.
var commitDiffArgs = []string{"--no-commit-id", "-r", "-m", "--first-parent", "--root"}

// gitCommitFiles lists the paths a commit touched, relative to the served
// root. Paths outside root's subtree are dropped, the same way gitStatusAgainst
// drops them, so a repo served from a subdirectory lists only what it can open.
func gitCommitFiles(root, sha string) []CommitFile {
	info := gitProbe(root)
	if !info.ok || !validSHA(sha) {
		return nil
	}
	args := append([]string{"-C", root, "diff-tree", "--name-status", "-z"}, commitDiffArgs...)
	out, err := exec.Command("git", append(args, sha)...).Output()
	if err != nil {
		return nil
	}
	stats := gitCommitNumstat(root, sha)
	key := repoRelKey(info, root)
	var files []CommitFile
	// -z name-status emits "<status>\0<path>\0", or "<status>\0<src>\0<dst>\0"
	// for a rename or copy, so the field count per record varies.
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		st := fields[i]
		if st == "" {
			continue
		}
		letter := st[:1]
		rename := letter == "R" || letter == "C"
		if i+1 >= len(fields) {
			break
		}
		from, to := "", fields[i+1]
		i++
		if rename {
			if i+1 >= len(fields) {
				break
			}
			from, to = to, fields[i+1]
			i++
		}
		path, ok := key(to)
		if !ok {
			continue
		}
		ns := stats[to]
		f := CommitFile{Path: path, Status: letter, Add: ns.add, Del: ns.del, Binary: ns.binary}
		if from != "" {
			if fp, ok := key(from); ok {
				f.From = fp
			}
		}
		files = append(files, f)
	}
	return files
}

type numstat struct {
	add, del int
	binary   bool
}

// gitCommitNumstat returns each path's line counts in a commit, keyed by the
// repo-relative path git reports (the new path for a rename). In -z form a
// rename is "add\tdel\t\0old\0new\0"; anything else is "add\tdel\tpath\0".
//
// git show rather than diff-tree: on a 20,000-file commit diff-tree --numstat
// takes ~1.6 s against show's ~90 ms, and on a merge diff-tree -m lists every
// path once per parent, where show -m --first-parent gives the first parent only.
func gitCommitNumstat(root, sha string) map[string]numstat {
	out, err := exec.Command("git", "-C", root, "show", "--numstat", "-z", "--format=",
		"-m", "--first-parent", "--no-color", sha).Output()
	if err != nil {
		return nil
	}
	stats := map[string]numstat{}
	toks := strings.Split(string(out), "\x00")
	for i := 0; i < len(toks); {
		f := strings.SplitN(toks[i], "\t", 3)
		i++
		if len(f) < 3 {
			continue
		}
		path := f[2]
		if path == "" { // rename or copy: old and new follow as their own tokens
			if i+1 >= len(toks) {
				break
			}
			path = toks[i+1]
			i += 2
		}
		st := numstat{binary: f[0] == "-" && f[1] == "-"}
		st.add, _ = strconv.Atoi(f[0])
		st.del, _ = strconv.Atoi(f[1])
		stats[path] = st
	}
	return stats
}

// gitCommitDetail reads a commit's metadata and diffstat for the hover card.
// ok is false for an unknown or malformed SHA.
func gitCommitDetail(root, sha string) (CommitDetail, bool) {
	if !gitAvailable(root) || !validSHA(sha) {
		return CommitDetail{}, false
	}
	// %B is last: it spans lines, so everything past the final separator is it.
	out, err := exec.Command("git", "-C", root, "show", "-s",
		"--format=%H%x1f%h%x1f%an%x1f%ae%x1f%cr%x1f%cI%x1f%s%x1f%B", sha).Output()
	if err != nil {
		return CommitDetail{}, false
	}
	f := strings.SplitN(string(out), "\x1f", 8)
	if len(f) < 8 {
		return CommitDetail{}, false
	}
	d := CommitDetail{
		Hash: f[0], Short: f[1], Author: f[2], Email: f[3],
		Date: f[4], DateISO: f[5], Subject: f[6],
		Message: strings.TrimRight(f[7], "\n"),
	}
	d.Files, d.Insertions, d.Deletions = gitCommitStat(root, sha)
	d.AuthorURL = githubAuthorURL(root, d.Email, d.Author)
	d.CommitURL = gitCommitWebURL(root, d.Hash)
	return d, true
}

// gitCommitStat sums a commit's numstat into files/insertions/deletions.
// Binary files count toward files but contribute no line counts, which is what
// git's own "--shortstat" does.
func gitCommitStat(root, sha string) (files, insertions, deletions int) {
	if !validSHA(sha) {
		return 0, 0, 0
	}
	args := append([]string{"-C", root, "diff-tree", "--numstat"}, commitDiffArgs...)
	out, err := exec.Command("git", append(args, sha)...).Output()
	if err != nil {
		return 0, 0, 0
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 3 {
			continue
		}
		files++
		if add, err := strconv.Atoi(parts[0]); err == nil { // "-" for binary
			insertions += add
		}
		if del, err := strconv.Atoi(parts[1]); err == nil {
			deletions += del
		}
	}
	return files, insertions, deletions
}

// gitDiffCommit returns the unified diff a single commit made to relpath --
// the commit against its first parent, or against the empty tree for a root
// commit. This is the ref form of gitDiffAgainst: the working tree isn't
// involved at all, so the result is frozen no matter what is edited since.
func gitDiffCommit(root, relpath, sha string) string {
	if !gitAvailable(root) || !validSHA(sha) {
		return ""
	}
	args := append([]string{"-C", root, "diff-tree", "-p", "--no-color"}, commitDiffArgs...)
	args = append(args, sha, "--", relpath)
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		return ""
	}
	return string(out)
}

// gitHunksCommit is gitHunksAgainst for a single commit's own diff.
func gitHunksCommit(root, relpath, sha string) (added, modified, deleted []int) {
	return parseDiffHunks(gitDiffCommit(root, relpath, sha))
}

// remoteHostPath splits a remote URL -- in any of git's spellings -- into its
// host and "owner/repo" path. ok is false for a local path or anything it
// can't recognise.
func remoteHostPath(raw string) (host, path string, ok bool) {
	raw = strings.TrimSuffix(raw, ".git")
	switch {
	case strings.HasPrefix(raw, "git@"): // git@host:owner/repo
		h, p, found := strings.Cut(raw[4:], ":")
		if !found {
			return "", "", false
		}
		return h, strings.TrimPrefix(p, "/"), true
	case strings.HasPrefix(raw, "ssh://"): // ssh://git@host/owner/repo
		clean := strings.TrimPrefix(raw, "ssh://")
		if i := strings.Index(clean, "@"); i >= 0 {
			clean = clean[i+1:]
		}
		h, p, found := strings.Cut(clean, "/")
		if !found {
			return "", "", false
		}
		return h, p, true
	case strings.HasPrefix(raw, "http://"), strings.HasPrefix(raw, "https://"):
		u, err := url.Parse(raw)
		if err != nil {
			return "", "", false
		}
		u.User = nil // strip user:token if any
		return u.Host, strings.TrimPrefix(u.Path, "/"), true
	}
	return "", "", false
}

// gitCommitWebURL returns a web URL for one commit on GitHub/GitLab/Bitbucket,
// or "" when the remote isn't one of those.
func gitCommitWebURL(root, sha string) string {
	host, path, ok := remoteHostPath(gitRemoteURL(root, gitCurrentBranch(root)))
	if !ok || path == "" {
		return ""
	}
	if strings.Contains(host, "gitlab") {
		return fmt.Sprintf("https://%s/%s/-/commit/%s", host, path, sha)
	}
	return fmt.Sprintf("https://%s/%s/commit/%s", host, path, sha)
}

// githubAuthorURL guesses a GitHub page for a commit author. A
// users.noreply.github.com address carries the login outright, so that becomes
// a profile link; otherwise the best that can be said from a commit alone is
// "this repo's commits by this address", which is at least always right about
// who it means. Non-GitHub remotes get nothing.
func githubAuthorURL(root, email, name string) string {
	host, path, ok := remoteHostPath(gitRemoteURL(root, gitCurrentBranch(root)))
	if !ok || path == "" || !strings.Contains(host, "github") {
		return ""
	}
	if login, _, found := strings.Cut(email, "@"); found && strings.HasSuffix(email, "users.noreply.github.com") {
		// "12345+octocat@users.noreply.github.com" and the older "octocat@..."
		if _, after, hasID := strings.Cut(login, "+"); hasID {
			login = after
		}
		if login != "" {
			return "https://" + host + "/" + url.PathEscape(login)
		}
	}
	q := email
	if q == "" {
		q = name
	}
	if q == "" {
		return ""
	}
	return fmt.Sprintf("https://%s/%s/commits?author=%s", host, path, url.QueryEscape(q))
}

// gitHeadCommit returns the abbreviated or full HEAD commit hash.
func gitHeadCommit(root string) string {
	out, err := exec.Command("git", "-C", root, "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitRemoteURL returns the fetch URL for the repo's upstream or origin remote.
func gitRemoteURL(root, branch string) string {
	if branch != "" {
		if remote, _, ok := gitUpstream(root, branch); ok {
			if out, err := exec.Command("git", "-C", root, "config", "--get", fmt.Sprintf("remote.%s.url", remote)).Output(); err == nil {
				if u := strings.TrimSpace(string(out)); u != "" {
					return u
				}
			}
		}
	}
	out, err := exec.Command("git", "-C", root, "config", "--get", "remote.origin.url").Output()
	if err == nil {
		return strings.TrimSpace(string(out))
	}
	return ""
}

// gitCommitsWebURL returns a web URL to view commits on GitHub/GitLab/Bitbucket if configured.
func gitCommitsWebURL(root, branch string) string {
	host, path, ok := remoteHostPath(gitRemoteURL(root, branch))
	if !ok || path == "" {
		return ""
	}
	seg := "commits"
	if strings.Contains(host, "gitlab") {
		seg = "-/commits"
	}
	if branch != "" && branch != "HEAD" {
		return fmt.Sprintf("https://%s/%s/%s/%s", host, path, seg, branch)
	}
	return fmt.Sprintf("https://%s/%s/%s", host, path, seg)
}
// GitStatusPayload contains the complete serialized git status event sent to the browser
// over Server-Sent Events (SSE). It includes working tree status, staged files, dirty directories,
// current branch, commit ahead/behind counts, and recent commits.
type GitStatusPayload struct {
	Git           bool              `json:"git"`                     // True if root is inside a git repository
	GitChanges    int               `json:"gitChanges"`              // Total count of modified, added, deleted, or untracked files
	GitFiles      []string          `json:"gitFiles"`                // List of modified file paths relative to root
	Statuses      map[string]string `json:"statuses"`                // Relative path -> status code (e.g. "M", "A", "D", "U")
	DirtyDirs     map[string]bool   `json:"dirtyDirs,omitempty"`     // Relative directory path -> true if any descendant is modified
	Staged        map[string]bool   `json:"staged,omitempty"`        // Relative path -> true if file has staged changes
	YourStatuses  map[string]string `json:"yourStatuses,omitempty"`  // PR mode: relative path -> reviewer modification status
	YourDirtyDirs map[string]bool   `json:"yourDirtyDirs,omitempty"` // PR mode: directory path -> contains reviewer changes
	Branch        string            `json:"branch,omitempty"`        // Active branch name
	RecentCommits []GitCommit       `json:"recentCommits,omitempty"` // Recent git commits for commit list UI
	CommitsURL    string            `json:"commitsUrl,omitempty"`    // Web URL to view commits on GitHub/forge
	Ahead         int               `json:"ahead"`                   // Commits ahead of upstream tracking branch
	Behind        int               `json:"behind"`                  // Commits behind upstream tracking branch
	Rewritten     bool              `json:"rewritten,omitempty"`     // PR mode: HEAD no longer contains the PR head (an unpushed rebase); only a force-push updates the PR
}

// GitWatcher monitors git state changes in the background and broadcasts updates
// to connected browser clients over SSE (/api/stream). It uses a hybrid detection strategy:
//  1. Fast path: 1-second polling of .git metadata files (HEAD, index, packed-refs) to instantly
//     detect CLI operations (git commit, git checkout, git add).
//  2. Worktree path: adaptive polling of working tree file modifications.
//  3. Manual triggers: immediate re-evaluations triggered on user UI actions.
type GitWatcher struct {
	ix   *Index
	root string

	mu      sync.Mutex
	clients map[chan []byte]struct{}
	active  atomic.Int32

	triggerCh chan struct{}
	stopCh    chan struct{}

	lastIndexMod   time.Time
	lastIndexSize  int64
	lastHeadMod    time.Time
	lastLogsMod    time.Time
	lastFetchMod   time.Time
	lastPackedMod  time.Time
	lastHeadCommit string
	lastAhead      int
	lastBehind     int
	lastRewritten  bool
	lastPushedHead string
	lastBranch     string
	lastCommits    []GitCommit
	lastCommitsURL string
	hasMetaCache   bool
	lastReady      bool
	lastInGit      bool
}

// NewGitWatcher constructs a new GitWatcher attached to the given workspace index.
func NewGitWatcher(ix *Index) *GitWatcher {
	gw := &GitWatcher{
		ix:             ix,
		root:           ix.Root(),
		clients:        make(map[chan []byte]struct{}),
		triggerCh:      make(chan struct{}, 1),
		stopCh:         make(chan struct{}),
		lastHeadCommit: gitHeadCommit(ix.Root()),
		lastReady:      ix.Ready(),
		lastInGit:      gitAvailable(ix.Root()),
	}
	return gw
}

// Start begins the background monitoring goroutine.
func (gw *GitWatcher) Start(ctx context.Context) {
	if gitDisabled {
		return
	}

	var gitdir string
	if gitAvailable(gw.root) {
		gitdir = gitDir(gw.root)
		gw.recordGitMeta(gitdir)
	}

	go gw.loop(ctx, gitdir)
}

func (gw *GitWatcher) Stop() {
	select {
	case <-gw.stopCh:
	default:
		close(gw.stopCh)
	}
}

// Trigger requests an immediate status check and broadcast.
func (gw *GitWatcher) Trigger() {
	select {
	case gw.triggerCh <- struct{}{}:
	default:
	}
}

func (gw *GitWatcher) recordGitMeta(gitdir string) bool {
	if gitdir == "" {
		return false
	}
	changed := false

	indexPath := filepath.Join(gitdir, "index")
	if fi, err := os.Stat(indexPath); err == nil {
		if fi.ModTime() != gw.lastIndexMod || fi.Size() != gw.lastIndexSize {
			gw.lastIndexMod = fi.ModTime()
			gw.lastIndexSize = fi.Size()
			changed = true
		}
	}

	headPath := filepath.Join(gitdir, "HEAD")
	if fi, err := os.Stat(headPath); err == nil {
		if fi.ModTime() != gw.lastHeadMod {
			gw.lastHeadMod = fi.ModTime()
			changed = true
		}
	}

	packedPath := filepath.Join(gitdir, "packed-refs")
	if fi, err := os.Stat(packedPath); err == nil {
		if fi.ModTime() != gw.lastPackedMod {
			gw.lastPackedMod = fi.ModTime()
			changed = true
		}
	}

	logsHeadPath := filepath.Join(gitdir, "logs", "HEAD")
	if fi, err := os.Stat(logsHeadPath); err == nil {
		if fi.ModTime() != gw.lastLogsMod {
			gw.lastLogsMod = fi.ModTime()
			changed = true
		}
	}

	fetchHeadPath := filepath.Join(gitdir, "FETCH_HEAD")
	if fi, err := os.Stat(fetchHeadPath); err == nil {
		if fi.ModTime() != gw.lastFetchMod {
			gw.lastFetchMod = fi.ModTime()
			changed = true
		}
	}

	return changed
}

func (gw *GitWatcher) loop(ctx context.Context, gitdir string) {
	metaTicker := time.NewTicker(1 * time.Second)
	defer metaTicker.Stop()

	adaptiveInterval := 1 * time.Second
	worktreeTicker := time.NewTicker(adaptiveInterval)
	defer worktreeTicker.Stop()
	unchangedStreak := 0

	heartbeatTicker := time.NewTicker(15 * time.Second)
	defer heartbeatTicker.Stop()

	resetInterval := func(d time.Duration) {
		if d != adaptiveInterval {
			adaptiveInterval = d
			worktreeTicker.Reset(adaptiveInterval)
		}
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-gw.stopCh:
			return

		case <-metaTicker.C:
			// Fast path: check .git metadata (HEAD, index, packed-refs).
			// If touched by git CLI (commit, checkout, add, etc.), refresh immediately.
			if gw.active.Load() > 0 {
				if gitdir == "" && gitAvailable(gw.root) {
					gitdir = gitDir(gw.root)
				}
				if gw.recordGitMeta(gitdir) {
					unchangedStreak = 0
					resetInterval(1 * time.Second)
					gw.checkAndBroadcast()
				}
			}

		case <-worktreeTicker.C:
			// Worktree path: check for modifications in files outside git CLI.
			// Only run when at least one client is actively listening.
			if gw.active.Load() > 0 {
				if gitdir == "" && gitAvailable(gw.root) {
					gitdir = gitDir(gw.root)
				}
				metaDirty := gw.recordGitMeta(gitdir)
				start := time.Now()
				_, changed := gw.refreshInternal(!metaDirty)
				dur := time.Since(start)
				if changed || metaDirty {
					unchangedStreak = 0
				} else if unchangedStreak < 4 {
					unchangedStreak++
				}
				// Adapt interval based on execution speed and idle streak: min 1s, max 10s.
				newInterval := dur * 5
				backoff := time.Duration(1<<unchangedStreak) * time.Second
				if backoff > newInterval {
					newInterval = backoff
				}
				if newInterval < 1*time.Second {
					newInterval = 1 * time.Second
				} else if newInterval > 10*time.Second {
					newInterval = 10 * time.Second
				}
				resetInterval(newInterval)
			}

		case <-gw.triggerCh:
			// Debounce rapid triggers
			time.Sleep(50 * time.Millisecond)
			// Drain any pending triggers queued during debounce
			select {
			case <-gw.triggerCh:
			default:
			}
			unchangedStreak = 0
			resetInterval(1 * time.Second)
			gw.checkAndBroadcast()

		case <-heartbeatTicker.C:
			if gw.active.Load() > 0 {
				gw.broadcast([]byte(": ping\n\n"))
			}
		}
	}
}

// Refresh runs an immediate UpdateGitStatus, broadcasts to subscribers if changed,
// and returns the latest git status payload.
func (gw *GitWatcher) Refresh() GitStatusPayload {
	p, _ := gw.refreshInternal(false)
	return p
}

func (gw *GitWatcher) refreshInternal(worktreeOnly bool) (GitStatusPayload, bool) {
	ready := gw.ix.Ready()
	inGit := gitAvailable(gw.root)
	count, files, changed, statuses, dirtyDirs, staged, yourStatuses, yourDirtyDirs := gw.ix.UpdateGitStatus()

	gw.mu.Lock()
	if ready != gw.lastReady || inGit != gw.lastInGit {
		gw.lastReady = ready
		gw.lastInGit = inGit
		gw.hasMetaCache = false
		changed = true
	}
	gw.mu.Unlock()

	var recentCommits []GitCommit
	var ahead, behind int
	var rewritten bool
	var branch, commitsURL string
	if inGit {
		pushedHead := gw.ix.PushedHead()
		gw.mu.Lock()
		canReuseMeta := worktreeOnly && gw.hasMetaCache && gw.lastPushedHead == pushedHead
		if canReuseMeta {
			recentCommits = gw.lastCommits
			ahead = gw.lastAhead
			behind = gw.lastBehind
			rewritten = gw.lastRewritten
			branch = gw.lastBranch
			commitsURL = gw.lastCommitsURL
		}
		gw.mu.Unlock()

		if !canReuseMeta {
			recentCommits = gitRecentCommits(gw.root, 5)
			headCommit := ""
			if len(recentCommits) > 0 {
				headCommit = recentCommits[0].Hash
			}
			ahead, behind = gw.aheadBehind()
			if validSHA(pushedHead) && ahead > 0 {
				rewritten = !gitIsAncestor(gw.root, pushedHead, "HEAD")
			}
			branch = gitCurrentBranch(gw.root)
			commitsURL = gitCommitsWebURL(gw.root, branch)

			gw.mu.Lock()
			if headCommit != gw.lastHeadCommit || branch != gw.lastBranch || ahead != gw.lastAhead || behind != gw.lastBehind || rewritten != gw.lastRewritten {
				gw.lastHeadCommit = headCommit
				gw.lastAhead = ahead
				gw.lastBehind = behind
				gw.lastRewritten = rewritten
				changed = true
			}
			gw.lastPushedHead = pushedHead
			gw.lastBranch = branch
			gw.lastCommits = recentCommits
			gw.lastCommitsURL = commitsURL
			if ready {
				gw.hasMetaCache = true
			}
			gw.mu.Unlock()
		}
	}

	payload := GitStatusPayload{
		Git:           inGit,
		GitChanges:    count,
		GitFiles:      files,
		Statuses:      statuses,
		DirtyDirs:     dirtyDirs,
		Staged:        staged,
		YourStatuses:  yourStatuses,
		YourDirtyDirs: yourDirtyDirs,
		Branch:        branch,
		RecentCommits: recentCommits,
		CommitsURL:    commitsURL,
		Ahead:         ahead,
		Behind:        behind,
		Rewritten:     rewritten,
	}
	if changed {
		data, err := json.Marshal(payload)
		if err == nil {
			msg := []byte(fmt.Sprintf("event: git-status\ndata: %s\n\n", data))
			gw.broadcast(msg)
		}
	}
	return payload, changed
}

// aheadBehind counts local commits not yet on the remote. In a PR review the
// checkout is a detached HEAD with no upstream, so "ahead" is measured from the
// PR head it was checked out at (or last pushed to) -- exactly the reviewer's
// own commits. Behind stays 0 there: knowing it needs a fetch, which Pull does.
func (gw *GitWatcher) aheadBehind() (ahead, behind int) {
	if head := gw.ix.PushedHead(); head != "" {
		return gitCountSince(gw.root, head), 0
	}
	ahead, behind, _ = gitAheadBehind(gw.root)
	return ahead, behind
}

// checkAndBroadcast runs UpdateGitStatus and broadcasts to subscribers if changed.
// Returns duration of the status check.
func (gw *GitWatcher) checkAndBroadcast() time.Duration {
	start := time.Now()
	gw.Refresh()
	return time.Since(start)
}

func (gw *GitWatcher) broadcast(msg []byte) {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	for ch := range gw.clients {
		select {
		case ch <- msg:
		default:
		}
	}
}

// Subscribe registers a new SSE listener channel and returns an unregister cancel function.
func (gw *GitWatcher) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 16)

	// Immediately send current state to newly connected client
	inGit := gitAvailable(gw.root)
	var initialMsg []byte
	if inGit {
		payload := gw.Refresh()
		if data, err := json.Marshal(payload); err == nil {
			initialMsg = []byte(fmt.Sprintf("event: git-status\ndata: %s\n\n", data))
		}
	} else {
		payload := GitStatusPayload{
			Git:        false,
			GitChanges: 0,
			GitFiles:   []string{},
			Statuses:   map[string]string{},
		}
		if data, err := json.Marshal(payload); err == nil {
			initialMsg = []byte(fmt.Sprintf("event: git-status\ndata: %s\n\n", data))
		}
	}

	if len(initialMsg) > 0 {
		ch <- initialMsg
	}

	gw.mu.Lock()
	gw.clients[ch] = struct{}{}
	gw.active.Add(1)
	gw.mu.Unlock()

	cancel := func() {
		gw.mu.Lock()
		if _, ok := gw.clients[ch]; ok {
			delete(gw.clients, ch)
			gw.active.Add(-1)
			close(ch)
		}
		gw.mu.Unlock()
	}

	return ch, cancel
}
