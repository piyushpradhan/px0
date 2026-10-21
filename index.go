package main

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"px0/internal/fuzzy"
	"px0/internal/ignore"
)

// FileEntry represents a single indexed file in the workspace,
// storing its relative path, name, size, and pre-computed lowercase representation
// for fast fuzzy searching.
type FileEntry = fuzzy.Item

// Node represents a file or directory entry in the hierarchical file tree view.
type Node struct {
	Name       string `json:"name"`                 // File or directory name
	Path       string `json:"path"`                 // Slash-separated path relative to workspace root
	Dir        bool   `json:"dir"`                  // True if this node is a directory
	Size       int64  `json:"size"`                 // File size in bytes (0 for directories)
	Ignored    bool   `json:"ignored,omitempty"`    // Matched by .gitignore: listed in tree dimmed, never indexed
	Status     string `json:"status,omitempty"`     // Git working-tree status code: M (modified), A (added), D (deleted), U (untracked), etc.
	Staged     bool   `json:"staged,omitempty"`     // True if the file has staged changes in the git index
	Dirty      bool   `json:"dirty,omitempty"`      // For folders: true if any descendant has git changes
	YourStatus string `json:"yourStatus,omitempty"` // PR review mode: reviewer's local changes since checkout
	YourDirty  bool   `json:"yourDirty,omitempty"`  // PR review mode for folders: contains reviewer-changed descendant
}

// vcsDirs are version control internals. Unlike other ignored entries they are
// not even listed: nobody reads them, and .git is present in nearly every repo.
var vcsDirs = map[string]bool{".git": true, ".hg": true, ".svn": true}

// Index maintains in-memory representation of the workspace filesystem:
// a flat list of all unignored files for fast searching, a directory tree map
// for lazy sidebar rendering, and live git status tracking.
type Index struct {
	root string

	mu               sync.RWMutex
	files            []FileEntry
	children         map[string][]Node
	builtAt          time.Time
	buildMS          int64
	gitChanges       int
	gitFiles         []string
	gitStatusMap     map[string]string
	gitStagedMap     map[string]bool
	gitYourStatusMap map[string]string
	diffBase         string
	prHead           string
	prFiles          map[string]string
	pushedHead       string // PR mode: remote branch tip as of the last push; bounds "unpushed"
	readyCh          chan struct{}
}

// NewIndex constructs a new unpopulated workspace index for the given root directory.
func NewIndex(root string) *Index {
	return &Index{root: root, children: map[string][]Node{}, readyCh: make(chan struct{})}
}

// Root returns the absolute path to the workspace root directory.
func (ix *Index) Root() string { return ix.root }

// SetDiffBase sets the git ref or commit against which working tree diffs and statuses are computed.
func (ix *Index) SetDiffBase(base string) {
	ix.mu.Lock()
	ix.diffBase = base
	ix.mu.Unlock()
}

// DiffBase returns the current git diff base ref (e.g. "HEAD" or PR merge-base).
func (ix *Index) DiffBase() string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.diffBase
}

// SetPRHead stores the checked-out PR head commit SHA in PR review mode.
func (ix *Index) SetPRHead(head string) {
	ix.mu.Lock()
	base := ix.diffBase
	ix.mu.Unlock()
	// The PR's own file set only moves with the head or the base, so it is
	// worked out here once rather than on every git-status tick.
	files := gitFilesBetween(ix.root, base, head)
	ix.mu.Lock()
	ix.prHead = head
	ix.prFiles = files
	ix.mu.Unlock()
}

// PRFiles returns the paths the PR itself changes (diffBase..PR head) with
// their git status letters, as opposed to files only the reviewer has touched.
// Empty outside PR review.
func (ix *Index) PRFiles() map[string]string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make(map[string]string, len(ix.prFiles))
	for k, v := range ix.prFiles {
		out[k] = v
	}
	return out
}

// SetPushedHead records the PR branch's tip after a push (or pull), which is
// the boundary "ahead"/"unpushed" are counted from. Unlike prHead it moves on push.
func (ix *Index) SetPushedHead(head string) {
	ix.mu.Lock()
	ix.pushedHead = head
	ix.mu.Unlock()
}

// PushedHead returns the unpushed boundary, or empty outside PR review mode.
func (ix *Index) PushedHead() string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.pushedHead
}

// PRHead returns the checked-out PR head commit SHA, or empty if not in PR review mode.
func (ix *Index) PRHead() string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.prHead
}

// Ready reports whether the initial index build has finished.
func (ix *Index) Ready() bool {
	select {
	case <-ix.readyCh:
		return true
	default:
		return false
	}
}

// WaitReady waits up to timeout for the index build to complete.
func (ix *Index) WaitReady(timeout time.Duration) bool {
	select {
	case <-ix.readyCh:
		return true
	case <-time.After(timeout):
		return ix.Ready()
	}
}

// Stats returns the total indexed file count, the timestamp of the build, and duration in milliseconds.
func (ix *Index) Stats() (files int, builtAt time.Time, ms int64) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.files), ix.builtAt, ix.buildMS
}

// Files returns a copy or view of all indexed file entries.
func (ix *Index) Files() []FileEntry {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return ix.files
}

// GitChanges returns the number of modified files and their workspace-relative paths.
func (ix *Index) GitChanges() (int, []string) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	res := make([]string, len(ix.gitFiles))
	copy(res, ix.gitFiles)
	return ix.gitChanges, res
}

// GitStatusMap returns a copy of the mapping from relative path to git status letter.
func (ix *Index) GitStatusMap() map[string]string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	if ix.gitStatusMap == nil {
		return map[string]string{}
	}
	res := make(map[string]string, len(ix.gitStatusMap))
	for k, v := range ix.gitStatusMap {
		res[k] = v
	}
	return res
}

// Children lists a directory for the tree. Ignored directories are never walked,
// so their contents are read from disk on demand, all marked ignored: git cannot
// re-include anything beneath an excluded directory either.
func (ix *Index) Children(dir string) ([]Node, bool) {
	ix.mu.RLock()
	c, ok := ix.children[dir]
	under := !ok && ix.underIgnoredLocked(dir)
	var res []Node
	if ok {
		res = make([]Node, len(c))
		copy(res, c)
	}
	ix.mu.RUnlock()
	if ok || !under {
		return res, ok
	}
	return ix.listIgnored(dir)
}

// underIgnoredLocked reports whether dir sits inside a directory the walk
// listed as ignored. The caller holds ix.mu.
func (ix *Index) underIgnoredLocked(dir string) bool {
	if dir == "" || strings.ContainsRune(dir, '\\') {
		return false
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false // never let a crafted path climb out of the root
		}
	}
	// The nearest ancestor the walk visited decides: its entry for the next
	// segment down must be an ignored directory.
	for p := dir; p != ""; {
		parent := ""
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			parent = p[:i]
		}
		if kids, ok := ix.children[parent]; ok {
			name := strings.TrimPrefix(p[len(parent):], "/")
			for _, k := range kids {
				if k.Name == name {
					return k.Dir && k.Ignored
				}
			}
			return false
		}
		p = parent
	}
	return false
}

// listIgnored reads the immediate entries of an ignored directory directly from disk on demand,
// marking all child nodes as Ignored. This allows the file tree UI to expand ignored directories
// without needing them to be walked or kept in the search index.
func (ix *Index) listIgnored(dir string) ([]Node, bool) {
	ents, err := os.ReadDir(filepath.Join(ix.root, filepath.FromSlash(dir)))
	if err != nil {
		return nil, false
	}
	kids := make([]Node, 0, len(ents))
	for _, e := range ents {
		if e.Type()&os.ModeSymlink != 0 {
			continue
		}
		kids = append(kids, Node{Name: e.Name(), Path: dir + "/" + e.Name(), Dir: e.IsDir(), Ignored: true})
	}
	sortNodes(kids)
	return kids, true
}

// HasDiffAvailable reports in O(1) whether rel has a tracked git diff according
// to the in-memory index maps (excluding untracked "U" files), or whether the
// caller should fall back when diffBase differs from the index's diffBase.
func (ix *Index) HasDiffAvailable(rel, serverDiffBase string, isPR bool) (avail, authoritative bool) {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	ixBase := ix.diffBase
	if ixBase == "" {
		ixBase = "HEAD"
	}
	srvBase := serverDiffBase
	if srvBase == "" {
		srvBase = "HEAD"
	}
	if ixBase != srvBase || (ix.gitStatusMap == nil && ix.builtAt.IsZero()) {
		return false, false
	}
	if st := ix.gitStatusMap[rel]; st != "" && st != "U" {
		return true, true
	}
	if isPR {
		if st := ix.prFiles[rel]; st != "" && st != "U" {
			return true, true
		}
		if st := ix.gitYourStatusMap[rel]; st != "" && st != "U" {
			return true, true
		}
	}
	return false, true
}

func toLowerIfNeeded(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'A' && c <= 'Z') || c >= 0x80 {
			return strings.ToLower(s)
		}
	}
	return s
}

func lessFoldASCII(a, b string) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		ca, cb := a[i], b[i]
		if ca >= 0x80 || cb >= 0x80 {
			return strings.ToLower(a) < strings.ToLower(b)
		}
		if ca >= 'A' && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if cb >= 'A' && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return ca < cb
		}
	}
	return len(a) < len(b)
}

// sortNodes orders a listing directories first, then case-insensitively by name.
func sortNodes(kids []Node) {
	sort.Slice(kids, func(i, j int) bool {
		if kids[i].Dir != kids[j].Dir {
			return kids[i].Dir
		}
		return lessFoldASCII(kids[i].Name, kids[j].Name)
	})
}

func computeDirtyState(gs, ys map[string]string) (dirtyDirs, yourDirtyDirs map[string]bool, gitFiles []string) {
	dirtyDirs = map[string]bool{}
	for p := range gs {
		gitFiles = append(gitFiles, p)
		for i := strings.LastIndexByte(p, '/'); i >= 0; i = strings.LastIndexByte(p, '/') {
			p = p[:i]
			dirtyDirs[p] = true
		}
	}
	sort.Slice(gitFiles, func(i, j int) bool {
		si, sj := gs[gitFiles[i]], gs[gitFiles[j]]
		if (si != "U") != (sj != "U") {
			return si != "U"
		}
		return gitFiles[i] < gitFiles[j]
	})
	yourDirtyDirs = map[string]bool{}
	for p := range ys {
		for i := strings.LastIndexByte(p, '/'); i >= 0; i = strings.LastIndexByte(p, '/') {
			p = p[:i]
			yourDirtyDirs[p] = true
		}
	}
	return dirtyDirs, yourDirtyDirs, gitFiles
}

// Build walks the tree once, honouring .gitignore at every level, and
// materialises both the flat file list (for fuzzy find and search) and the
// directory map (for the tree view). Root entries are published immediately so
// the frontend can display the file tree without waiting for the full repo scan.
func (ix *Index) Build() {
	start := time.Now()
	root := ignore.New(nil)
	root = root.Child(ignore.ReadGitignore(ix.root, ""))

	// Run git status concurrently with the directory walk so the 80-300ms git
	// subprocess overlaps the filesystem scan, and root directory entries are
	// published in under a millisecond on cold startup. Once both the walk and
	// git status complete, statuses are overlaid across all nodes before readyCh
	// closes.
	base := ix.DiffBase()
	head := ix.PRHead()
	type gitRes struct {
		gs           map[string]string
		staged       map[string]bool
		yourStatuses map[string]string
		dirtyDirs    map[string]bool
		yourDirty    map[string]bool
		gitFiles     []string
	}
	gsCh := make(chan gitRes, 1)
	go func() {
		gs, staged, yourStatuses := gitStatusAndStagedAgainst(ix.root, base, head)
		dirtyDirs, yourDirtyDirs, gitFiles := computeDirtyState(gs, yourStatuses)
		gsCh <- gitRes{
			gs:           gs,
			staged:       staged,
			yourStatuses: yourStatuses,
			dirtyDirs:    dirtyDirs,
			yourDirty:    yourDirtyDirs,
			gitFiles:     gitFiles,
		}
	}()

	var (
		mu       sync.Mutex
		files    []FileEntry
		children = map[string][]Node{}
		wg       sync.WaitGroup
		sem      = make(chan struct{}, runtime.NumCPU()*4)
	)

	var walk func(abs, rel string, ig *ignore.IgnoreSet)
	walk = func(abs, rel string, ig *ignore.IgnoreSet) {
		defer wg.Done()
		ents, err := os.ReadDir(abs)
		if err != nil {
			return
		}
		if rel != "" {
			if extra := ignore.ReadGitignore(abs, rel); len(extra) > 0 {
				ig = ig.Child(extra)
			}
		}
		kids := make([]Node, 0, len(ents))
		localFiles := make([]FileEntry, 0, len(ents))
		var subdirs []struct {
			abs, rel string
		}
		for _, e := range ents {
			name := e.Name()
			childRel := name
			if rel != "" {
				childRel = rel + "/" + name
			}
			isDir := e.IsDir()
			// Follow nothing through symlinks; cycles are not worth the risk.
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			if ig.Match(childRel, isDir) {
				// Listed so the tree can show it dimmed, but never walked or
				// indexed, so search and quick open stay out of it.
				if !(isDir && vcsDirs[name]) {
					kids = append(kids, Node{Name: name, Path: childRel, Dir: isDir, Ignored: true})
				}
				continue
			}
			if isDir {
				kids = append(kids, Node{Name: name, Path: childRel, Dir: true})
				subdirs = append(subdirs, struct{ abs, rel string }{filepath.Join(abs, name), childRel})
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			kids = append(kids, Node{Name: name, Path: childRel, Size: info.Size()})
			localFiles = append(localFiles, FileEntry{
				Path: childRel, Name: name, Size: info.Size(),
				Lower: toLowerIfNeeded(childRel), NameStart: len(childRel) - len(name),
			})
		}
		sortNodes(kids)
		mu.Lock()
		children[rel] = kids
		files = append(files, localFiles...)
		mu.Unlock()

		// If this is the root directory, make it available to ix.Children("")
		// immediately so the browser UI can render the sidebar tree without delay.
		if rel == "" {
			ix.mu.Lock()
			if ix.children == nil {
				ix.children = map[string][]Node{}
			}
			rootKids := make([]Node, len(kids))
			copy(rootKids, kids)
			ix.children[""] = rootKids
			ix.mu.Unlock()
		}

		for _, sd := range subdirs {
			wg.Add(1)
			select {
			case sem <- struct{}{}:
				go func(a, r string, g *ignore.IgnoreSet) {
					defer func() { <-sem }()
					walk(a, r, g)
				}(sd.abs, sd.rel, ig)
			default:
				walk(sd.abs, sd.rel, ig) // pool saturated: recurse inline
			}
		}
	}

	wg.Add(1)
	walk(ix.root, "", root)
	wg.Wait()

	gr := <-gsCh
	gs, staged, yourStatuses := gr.gs, gr.staged, gr.yourStatuses
	dirtyDirs, yourDirtyDirs, gitFiles := gr.dirtyDirs, gr.yourDirty, gr.gitFiles

	if len(gs) > 0 || len(staged) > 0 || len(yourStatuses) > 0 {
		for dir, kids := range children {
			for i := range kids {
				if kids[i].Ignored {
					continue
				}
				p := kids[i].Path
				if kids[i].Dir {
					kids[i].Dirty = dirtyDirs[p]
					kids[i].YourDirty = yourDirtyDirs[p]
				} else {
					kids[i].Status = gs[p]
					kids[i].Staged = staged[p]
					if yourStatuses != nil {
						kids[i].YourStatus = yourStatuses[p]
					}
				}
			}
			children[dir] = kids
		}
	}

	injectDeletedNodes(children, gs, staged, yourStatuses, dirtyDirs, yourDirtyDirs)

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	ix.mu.Lock()
	ix.gitChanges = len(gitFiles)
	ix.gitFiles = gitFiles
	ix.gitStatusMap = gs
	ix.gitStagedMap = staged
	ix.gitYourStatusMap = yourStatuses
	ix.files, ix.children = files, children
	ix.builtAt, ix.buildMS = time.Now(), time.Since(start).Milliseconds()
	select {
	case <-ix.readyCh:
	default:
		close(ix.readyCh)
	}
	ix.mu.Unlock()
}

// injectDeletedNodes ensures that files marked deleted in git status (which do not
// exist on disk and were therefore omitted by the filesystem walk) are added to children
// under their respective parent directories, and that any missing intermediate directory
// nodes are created and marked dirty.
func injectDeletedNodes(children map[string][]Node, gs map[string]string, staged map[string]bool, ys map[string]string, dirtyDirs map[string]bool, yourDirtyDirs map[string]bool) {
	apply := func(p string, st string, isStaged bool, yst string) {
		parts := strings.Split(p, "/")
		for i := 0; i < len(parts)-1; i++ {
			parentDir := ""
			if i > 0 {
				parentDir = strings.Join(parts[:i], "/")
			}
			subDir := strings.Join(parts[:i+1], "/")
			name := parts[i]

			kids := children[parentDir]
			found := false
			for _, k := range kids {
				if k.Name == name && k.Dir {
					found = true
					break
				}
			}
			if !found {
				children[parentDir] = append(children[parentDir], Node{
					Name:      name,
					Path:      subDir,
					Dir:       true,
					Dirty:     dirtyDirs[subDir],
					YourDirty: yourDirtyDirs != nil && yourDirtyDirs[subDir],
				})
				sortNodes(children[parentDir])
			}
			if _, ok := children[subDir]; !ok {
				children[subDir] = []Node{}
			}
		}

		dir := ""
		name := p
		if lastSlash := strings.LastIndexByte(p, '/'); lastSlash >= 0 {
			dir = p[:lastSlash]
			name = p[lastSlash+1:]
		}

		kids := children[dir]
		found := false
		for i := range kids {
			if kids[i].Path == p && !kids[i].Dir {
				kids[i].Status = st
				kids[i].Staged = isStaged
				kids[i].YourStatus = yst
				found = true
				break
			}
		}
		if !found {
			children[dir] = append(children[dir], Node{
				Name:       name,
				Path:       p,
				Dir:        false,
				Status:     st,
				Staged:     isStaged,
				YourStatus: yst,
			})
			sortNodes(children[dir])
		}
	}

	for p, st := range gs {
		yst := ""
		if ys != nil {
			yst = ys[p]
		}
		apply(p, st, staged != nil && staged[p], yst)
	}
	if ys != nil {
		for p, st := range ys {
			if gs == nil || gs[p] == "" {
				apply(p, "", staged != nil && staged[p], st)
			}
		}
	}
}

func cleanupEmptyDirs(children map[string][]Node, root string) {
	changed := true
	for changed {
		changed = false
		for dir, kids := range children {
			if dir == "" || len(kids) > 0 {
				continue
			}
			absDir := filepath.Join(root, filepath.FromSlash(dir))
			if _, err := os.Stat(absDir); err != nil {
				delete(children, dir)
				parent := ""
				if idx := strings.LastIndexByte(dir, '/'); idx >= 0 {
					parent = dir[:idx]
				}
				pkids := children[parent]
				pFiltered := pkids[:0]
				for _, k := range pkids {
					if k.Path != dir {
						pFiltered = append(pFiltered, k)
					}
				}
				children[parent] = pFiltered
				changed = true
				break
			}
		}
	}
}

// UpdateGitStatus re-runs git status, updates in-memory status codes, staged
// flags, and dirty directory markers across ix.children without re-walking
// the filesystem tree. Reports gitChanges count, gitFiles list, whether
// anything changed (status, staged, or yourStatus), and the raw status/staged/dirty-dir
// maps.
func (ix *Index) UpdateGitStatus() (count int, files []string, changed bool, statuses map[string]string, dirtyDirs map[string]bool, staged map[string]bool, yourStatuses map[string]string, yourDirtyDirs map[string]bool) {
	if !ix.Ready() || gitDisabled || !gitAvailable(ix.root) {
		return 0, nil, false, nil, nil, nil, nil, nil
	}

	base := ix.DiffBase()
	head := ix.PRHead()
	gs, sg, ys := gitStatusAndStagedAgainst(ix.root, base, head)
	if gs == nil {
		gs = map[string]string{}
	}
	if sg == nil {
		sg = map[string]bool{}
	}
	if head != "" && ys == nil {
		ys = map[string]string{}
	}
	newDirtyDirs, newYourDirtyDirs, newGitFiles := computeDirtyState(gs, ys)

	ix.mu.Lock()
	defer ix.mu.Unlock()

	// Check if status, staged, and yourStatuses maps are all unchanged
	same := len(gs) == len(ix.gitStatusMap) && len(sg) == len(ix.gitStagedMap) && len(ys) == len(ix.gitYourStatusMap)
	if same {
		for k, v := range gs {
			if ix.gitStatusMap[k] != v {
				same = false
				break
			}
		}
	}
	if same {
		for k, v := range sg {
			if ix.gitStagedMap[k] != v {
				same = false
				break
			}
		}
	}
	if same {
		for k, v := range ys {
			if ix.gitYourStatusMap[k] != v {
				same = false
				break
			}
		}
	}
	if same {
		resFiles := make([]string, len(ix.gitFiles))
		copy(resFiles, ix.gitFiles)
		return ix.gitChanges, resFiles, false, gs, newDirtyDirs, sg, ys, newYourDirtyDirs
	}

	// Update nodes in-place across ix.children
	for dir, kids := range ix.children {
		filtered := kids[:0]
		for _, node := range kids {
			if node.Dir {
				node.Dirty = newDirtyDirs[node.Path]
				node.YourDirty = newYourDirtyDirs[node.Path]
				filtered = append(filtered, node)
			} else {
				newStatus := gs[node.Path]
				newStaged := sg[node.Path]
				newYourStatus := ys[node.Path]

				if (node.Status == "D" || node.YourStatus == "D") && newStatus != "D" && newYourStatus != "D" {
					absPath := filepath.Join(ix.root, filepath.FromSlash(node.Path))
					if _, err := os.Stat(absPath); err != nil {
						continue // committed deletion: remove from tree
					}
				}

				node.Status = newStatus
				node.Staged = newStaged
				node.YourStatus = newYourStatus
				filtered = append(filtered, node)
			}
		}
		ix.children[dir] = filtered
	}

	injectDeletedNodes(ix.children, gs, sg, ys, newDirtyDirs, newYourDirtyDirs)
	cleanupEmptyDirs(ix.children, ix.root)

	ix.gitChanges = len(newGitFiles)
	ix.gitFiles = newGitFiles
	ix.gitStatusMap = gs
	ix.gitStagedMap = sg
	ix.gitYourStatusMap = ys

	resFiles := make([]string, len(ix.gitFiles))
	copy(resFiles, ix.gitFiles)
	return ix.gitChanges, resFiles, true, gs, newDirtyDirs, sg, ys, newYourDirtyDirs
}

// TouchPath incrementally inserts or updates a single relative file or directory
// path in ix.children and ix.files without re-walking the workspace tree.
func (ix *Index) TouchPath(rel string) {
	if !ix.Ready() {
		ix.Build()
		return
	}
	rel = strings.Trim(filepath.ToSlash(rel), "/")
	if rel == "" || strings.Contains(rel, "..") {
		return
	}
	abs := filepath.Join(ix.root, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			ix.mu.Lock()
			defer ix.mu.Unlock()
			idx := sort.Search(len(ix.files), func(i int) bool { return ix.files[i].Path >= rel })
			if idx < len(ix.files) && ix.files[idx].Path == rel {
				ix.files = append(ix.files[:idx], ix.files[idx+1:]...)
			}
			parentDir := ""
			if lastSlash := strings.LastIndexByte(rel, '/'); lastSlash >= 0 {
				parentDir = rel[:lastSlash]
			}
			if kids, ok := ix.children[parentDir]; ok {
				filtered := kids[:0]
				for _, k := range kids {
					if k.Path != rel {
						filtered = append(filtered, k)
					}
				}
				ix.children[parentDir] = filtered
			}
			delete(ix.children, rel)
		}
		return
	}

	ig := ignore.New(nil).Child(ignore.ReadGitignore(ix.root, ""))
	parts := strings.Split(rel, "/")
	for i := 0; i < len(parts)-1; i++ {
		if vcsDirs[parts[i]] {
			return
		}
		subDir := strings.Join(parts[:i+1], "/")
		if ig.Match(subDir, true) {
			return
		}
		absSub := filepath.Join(ix.root, filepath.FromSlash(subDir))
		if extra := ignore.ReadGitignore(absSub, subDir); len(extra) > 0 {
			ig = ig.Child(extra)
		}
	}

	isDir := info.IsDir()
	name := parts[len(parts)-1]
	if isDir && vcsDirs[name] {
		return
	}
	ignored := ig.Match(rel, isDir)
	var size int64
	if !isDir {
		size = info.Size()
	}

	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.children == nil {
		ix.children = map[string][]Node{}
	}

	for i := 0; i < len(parts)-1; i++ {
		parentDir := ""
		if i > 0 {
			parentDir = strings.Join(parts[:i], "/")
		}
		subDir := strings.Join(parts[:i+1], "/")
		segName := parts[i]
		kids := ix.children[parentDir]
		found := false
		for _, k := range kids {
			if k.Name == segName && k.Dir {
				found = true
				break
			}
		}
		if !found {
			ix.children[parentDir] = append(ix.children[parentDir], Node{
				Name: segName,
				Path: subDir,
				Dir:  true,
			})
			sortNodes(ix.children[parentDir])
		}
		if _, ok := ix.children[subDir]; !ok {
			ix.children[subDir] = []Node{}
		}
	}

	parentDir := ""
	if len(parts) > 1 {
		parentDir = strings.Join(parts[:len(parts)-1], "/")
	}
	kids := ix.children[parentDir]
	foundNode := false
	for i := range kids {
		if kids[i].Path == rel {
			kids[i].Size = size
			kids[i].Dir = isDir
			kids[i].Ignored = ignored
			foundNode = true
			break
		}
	}
	if !foundNode {
		node := Node{
			Name:    name,
			Path:    rel,
			Dir:     isDir,
			Size:    size,
			Ignored: ignored,
		}
		if !isDir && !ignored {
			if ix.gitStatusMap != nil {
				node.Status = ix.gitStatusMap[rel]
			}
			if ix.gitStagedMap != nil {
				node.Staged = ix.gitStagedMap[rel]
			}
			if ix.gitYourStatusMap != nil {
				node.YourStatus = ix.gitYourStatusMap[rel]
			}
		}
		ix.children[parentDir] = append(ix.children[parentDir], node)
		sortNodes(ix.children[parentDir])
	}
	if isDir && !ignored {
		if _, ok := ix.children[rel]; !ok {
			ix.children[rel] = []Node{}
		}
		return
	}
	if !isDir && !ignored {
		idx := sort.Search(len(ix.files), func(i int) bool { return ix.files[i].Path >= rel })
		entry := FileEntry{
			Path:      rel,
			Name:      name,
			Size:      size,
			Lower:     toLowerIfNeeded(rel),
			NameStart: len(rel) - len(name),
		}
		if idx < len(ix.files) && ix.files[idx].Path == rel {
			ix.files[idx] = entry
		} else {
			ix.files = append(ix.files, FileEntry{})
			copy(ix.files[idx+1:], ix.files[idx:])
			ix.files[idx] = entry
		}
	}
}

