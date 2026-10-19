package main

// Pull requests opened from the workbench are always checked out as a git
// worktree of a local clone of their repository, never as a fresh clone per
// PR: a worktree shares the clone's objects, so it costs a checkout and
// nothing else. The clone for each repository is found once and remembered
// in workbench.json:
//
//  1. the clone remembered for owner/repo, if it is still one;
//  2. a recorded directory (or the workbench's own directory) inside one;
//  3. a bounded scan of the usual places people keep code (~/workspace,
//     ~/code, ~/src, ...) and the folders around recorded directories.
//
// When none is found the page asks: point at a clone, or let px0 make one,
// once, under the state directory (repos/<owner>/<repo>), to be reused by
// every later PR of that repository.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	wbScanDepth   = 3                       // folder levels below each scan root
	wbScanBudget  = 20000                   // folders looked at per scan, at most
	wbScanTimeout = 1500 * time.Millisecond // wall-clock cap on one scan
)

// wbScanSkip are folder names never worth descending into for a clone.
var wbScanSkip = map[string]bool{
	"node_modules": true, "vendor": true, "target": true, "build": true, "dist": true,
	"Library": true, "Applications": true, "AppData": true, "venv": true, "__pycache__": true,
}

// wbScanRoots are where people usually keep clones, relative to home.
var wbScanRoots = []string{
	"workspace", "workspaces", "code", "src", "projects", "dev", "repos", "git",
	"github", "work", "Developer", "Documents/GitHub", "go/src/github.com", "",
}

func repoKey(repo string) string { return strings.ToLower(repo) }

func splitRepo(repo string) (owner, name string) {
	owner, name, _ = strings.Cut(repo, "/")
	return owner, name
}

// managedCloneDir is where px0 makes a clone when the user asks it to.
func managedCloneDir(repo string) string {
	owner, name := splitRepo(repo)
	return filepath.Join(workbenchStateDir(), "repos", owner, name)
}

// cloneOf returns the top level of dir's repository when one of its remotes
// is repo ("owner/name"), or "".
func cloneOf(dir, repo string) string {
	owner, name := splitRepo(repo)
	top, _ := findPRRemote(dir, owner, name)
	return top
}

// knownClone looks only where it is cheap to: the remembered clone, the
// recorded directories, and extra (the current directory).
func knownClone(f wbFile, repo string, extra ...string) string {
	if p := f.Clones[repoKey(repo)]; p != "" {
		if top := cloneOf(p, repo); top != "" {
			return top
		}
	}
	seen := map[string]bool{}
	var dirs []string
	for _, it := range f.Items {
		if it.Kind == "dir" {
			dirs = append(dirs, it.Path)
		}
	}
	for _, d := range append(dirs, extra...) {
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		if top := cloneOf(d, repo); top != "" {
			return top
		}
	}
	return ""
}

// scanForClone walks roots breadth-first, a few levels deep, for a folder
// named like the repository that is a clone of it. Bounded in folders and time.
func scanForClone(ctx context.Context, roots []string, repo string) string {
	_, name := splitRepo(repo)
	ctx, cancel := context.WithTimeout(ctx, wbScanTimeout)
	defer cancel()
	type node struct {
		path  string
		depth int
	}
	seen := map[string]bool{}
	var queue []node
	for _, r := range roots {
		if r != "" && !seen[r] {
			seen[r] = true
			queue = append(queue, node{r, 0})
		}
	}
	budget := wbScanBudget
	for len(queue) > 0 && budget > 0 && ctx.Err() == nil {
		n := queue[0]
		queue = queue[1:]
		ents, err := os.ReadDir(n.path)
		if err != nil {
			continue
		}
		for _, e := range ents {
			budget--
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || wbScanSkip[e.Name()] {
				continue
			}
			p := filepath.Join(n.path, e.Name())
			if seen[p] {
				continue
			}
			seen[p] = true
			if strings.EqualFold(e.Name(), name) {
				if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
					if top := cloneOf(p, repo); top != "" {
						return top
					}
				}
			}
			if n.depth+1 < wbScanDepth {
				queue = append(queue, node{p, n.depth + 1})
			}
		}
	}
	return ""
}

// scanRoots lists the usual code folders under home plus the parents of the
// recorded directories, which is where other clones tend to sit.
func scanRoots(f wbFile, cwd string) []string {
	var roots []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, r := range wbScanRoots {
			roots = append(roots, filepath.Join(home, r))
		}
	}
	for _, it := range f.Items {
		if it.Kind == "dir" {
			roots = append(roots, filepath.Dir(it.Path))
		}
	}
	if cwd != "" {
		roots = append(roots, cwd, filepath.Dir(cwd))
	}
	return roots
}

// findClone finds and remembers the local clone of repo, or returns "".
func (wb *workbench) findClone(ctx context.Context, repo string) string {
	f := wb.store.Load()
	top := knownClone(f, repo, wb.cwd)
	if top == "" {
		top = scanForClone(ctx, scanRoots(f, wb.cwd), repo)
	}
	if top != "" {
		wb.rememberClone(repo, top)
	}
	return top
}

func (wb *workbench) rememberClone(repo, dir string) {
	_ = wb.store.Update(func(f *wbFile) {
		if f.Clones == nil {
			f.Clones = map[string]string{}
		}
		f.Clones[repoKey(repo)] = dir
	})
}

var errNoClone = errors.New("no local clone")

// prClone returns the clone a PR's child should start in. The remembered
// clone wins; a remembered px0-made clone that is missing is made again.
func (wb *workbench) prClone(c *wbChild, it wbItem) (string, error) {
	f := wb.store.Load()
	remembered := f.Clones[repoKey(it.Repo)]
	if remembered != "" && remembered == managedCloneDir(it.Repo) {
		if top := cloneOf(remembered, it.Repo); top != "" {
			return top, nil
		}
		return wb.makeClone(c, it)
	}
	wb.setProgress(c, "Looking for a local clone of "+it.Repo)
	if top := wb.findClone(c.ctx, it.Repo); top != "" {
		return top, nil
	}
	return "", fmt.Errorf("%w of %s found; open the pull request again from the workbench to choose one", errNoClone, it.Repo)
}

// cloneLocks serializes makeClone per repository, so two PRs of one repository
// opened together do not both clone it.
var cloneLocks sync.Map

// makeClone clones the PR's repository into managedCloneDir, once. The clone
// is partial (blobs on demand) and has no checkout of its own: worktrees are
// all it is for.
func (wb *workbench) makeClone(c *wbChild, it wbItem) (string, error) {
	mu, _ := cloneLocks.LoadOrStore(repoKey(it.Repo), &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()

	dest := managedCloneDir(it.Repo)
	if top := cloneOf(dest, it.Repo); top != "" {
		return top, nil // made while this one waited
	}
	provider, pt, ok := DetectPRURL(it.URL)
	if !ok {
		return "", fmt.Errorf("not a pull request URL: %s", it.URL)
	}
	token, _ := provider.ResolveToken(readSettings(), pt)
	wb.setProgress(c, fmt.Sprintf("Cloning %s into %s (once; later pull requests reuse it)", it.Repo, tildePath(dest)))

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", err
	}
	// Clone beside dest and rename, so an interrupted clone never looks done.
	tmp, err := os.MkdirTemp(filepath.Dir(dest), "."+filepath.Base(dest)+"-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	auth := provider.GitAuth(pt, token)
	cmd := gitAuthCmd(auth, "clone", "--filter=blob:none", "--no-checkout", provider.CloneURL(pt), tmp)
	if out, err := runCtx(c.ctx, cmd); err != nil {
		return "", fmt.Errorf("git clone %s: %w: %s", it.Repo, err, auth.redact(strings.TrimSpace(string(out))))
	}
	os.RemoveAll(dest) // a broken earlier attempt
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	wb.rememberClone(it.Repo, dest)
	return dest, nil
}

// runCtx runs cmd and kills it if ctx ends first.
func runCtx(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return []byte(out.String()), err
	case <-ctx.Done():
		cmd.Process.Kill()
		<-done
		return []byte(out.String()), ctx.Err()
	}
}

func tildePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if p == home {
			return "~"
		}
		if strings.HasPrefix(p, home+string(filepath.Separator)) {
			return "~" + p[len(home):]
		}
	}
	return p
}
