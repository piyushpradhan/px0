package fuzzy

import (
	"runtime"
	"sort"
	"strings"
	"sync"
)

// Item represents a searchable item with precomputed lowercasing and basename offset.
type Item struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Lower     string `json:"-"`
	NameStart int    `json:"-"`
}

// NewItem creates an Item from a relative path, precomputing basename, lowercasing, and nameStart.
func NewItem(path string) Item {
	name := path
	idx := strings.LastIndex(path, "/")
	nameStart := 0
	if idx >= 0 {
		name = path[idx+1:]
		nameStart = idx + 1
	}
	return Item{
		Path:      path,
		Name:      name,
		Lower:     strings.ToLower(path),
		NameStart: nameStart,
	}
}

// Result represents a matched file path ranked by the fuzzy search engine.
type Result struct {
	Path  string `json:"path"` // Workspace-relative path to the matched file
	Name  string `json:"name"` // Basename of the file
	Pos   []int  `json:"pos"`  // Byte offsets in Path that matched, used by frontend for highlight badges
	Score int    `json:"-"`    // Computed match quality score (higher is better)
}

// IsBoundary reports whether a byte acts as a word or segment boundary
// in file paths (e.g. slashes, underscores, dashes, dots, spaces, or at-symbols).
func IsBoundary(b byte) bool {
	switch b {
	case '/', '_', '-', '.', ' ', '@':
		return true
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Score does a two-pass match: forward to prove every query rune is
// present, then backward from that endpoint to pull the matched positions as
// tightly together as possible. Tight matches score higher, which is what makes
// "fzf feel" work without an O(n*m) dynamic program.
func Score(q, origQ, path, lowerPath string, nameStart int, pos []int) (int, []int, bool) {
	p, lp := path, lowerPath
	qi, end := 0, -1
	for i := 0; i < len(lp) && qi < len(q); i++ {
		if lp[i] == q[qi] {
			qi++
			end = i
		}
	}
	if qi < len(q) {
		return 0, nil, false
	}

	pos = pos[:0]
	qi = len(q) - 1
	for i := end; i >= 0 && qi >= 0; i-- {
		if lp[i] == q[qi] {
			pos = append(pos, i)
			qi--
		}
	}
	// Collected right-to-left; flip in place.
	for i, j := 0, len(pos)-1; i < j; i, j = i+1, j-1 {
		pos[i], pos[j] = pos[j], pos[i]
	}

	score, prev := 0, -2
	for k, i := range pos {
		if i == prev+1 {
			score += 12 // consecutive run
		} else if k > 0 {
			score -= min(i-prev, 12) // gap penalty, bounded
		}
		if i >= nameStart {
			score += 14 // basename beats directory noise
		}
		if i == 0 || IsBoundary(p[i-1]) {
			score += 16 // start of a path or word segment
		} else if p[i] >= 'A' && p[i] <= 'Z' && p[i-1] >= 'a' && p[i-1] <= 'z' {
			score += 14 // camelCase hump
		}
		if k < len(origQ) && p[i] == origQ[k] {
			score += 4 // exact case
		}
		prev = i
	}
	// Prefer the shallower, shorter of two otherwise-equal paths.
	score -= len(p) / 8
	score -= strings.Count(p, "/") * 2
	if nameStart <= len(lp) {
		if idx := strings.Index(lp[nameStart:], q); idx >= 0 {
			score += 40 // whole query appears verbatim in the basename
			if idx == 0 {
				score += 20
			}
		}
	}
	return score, pos, true
}

// worseResult reports whether a is strictly worse than b (lower score, or same score and lexicographically greater path).
func worseResult(a, b Result) bool {
	if a.Score != b.Score {
		return a.Score < b.Score
	}
	return a.Path > b.Path
}

// siftDownResult maintains a min-heap of Result ordered by worseResult (root h[0] is the worst of the top-K).
func siftDownResult(h []Result, i int) {
	n := len(h)
	for {
		left := 2*i + 1
		if left >= n {
			break
		}
		smallest := left
		if right := left + 1; right < n && worseResult(h[right], h[left]) {
			smallest = right
		}
		if !worseResult(h[smallest], h[i]) {
			break
		}
		h[i], h[smallest] = h[smallest], h[i]
		i = smallest
	}
}

// Find ranks items against query and returns the best limit results.
func Find(items []Item, query string, limit int) []Result {
	origQ := strings.ReplaceAll(strings.TrimSpace(query), " ", "")
	q := strings.ToLower(origQ)

	if q == "" {
		out := make([]Result, 0, limit)
		for i := range items {
			if i == limit {
				break
			}
			out = append(out, Result{Path: items[i].Path, Name: items[i].Name})
		}
		return out
	}

	workers := runtime.NumCPU()
	if len(items) < 1024 {
		workers = 1
	}
	chunk := (len(items) + workers - 1) / workers
	if chunk == 0 {
		chunk = 1
	}
	parts := make([][]Result, workers)
	qLen := len(q)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		lo := w * chunk
		if lo >= len(items) {
			break
		}
		hi := min(lo+chunk, len(items))
		wg.Add(1)
		go func(w, lo, hi int) {
			defer wg.Done()
			capLimit := limit
			if capLimit <= 0 || capLimit > hi-lo {
				capLimit = min(64, hi-lo)
			}
			local := make([]Result, 0, capLimit)
			var posSlab []int
			if limit > 0 {
				posSlab = make([]int, limit*qLen)
			}
			scratch := make([]int, 0, max(64, qLen))
			for i := lo; i < hi; i++ {
				s, pos, ok := Score(q, origQ, items[i].Path, items[i].Lower, items[i].NameStart, scratch)
				if !ok {
					continue
				}
				if limit > 0 {
					if len(local) < limit {
						slot := len(local)
						cp := posSlab[slot*qLen : (slot+1)*qLen]
						copy(cp, pos)
						local = append(local, Result{Path: items[i].Path, Name: items[i].Name, Pos: cp, Score: s})
						if len(local) == limit {
							for k := limit/2 - 1; k >= 0; k-- {
								siftDownResult(local, k)
							}
						}
					} else if s > local[0].Score || (s == local[0].Score && items[i].Path < local[0].Path) {
						cp := local[0].Pos
						copy(cp, pos)
						local[0] = Result{Path: items[i].Path, Name: items[i].Name, Pos: cp, Score: s}
						siftDownResult(local, 0)
					}
					continue
				}
				cp := make([]int, len(pos))
				copy(cp, pos)
				local = append(local, Result{Path: items[i].Path, Name: items[i].Name, Pos: cp, Score: s})
			}
			parts[w] = local
		}(w, lo, hi)
	}
	wg.Wait()

	total := 0
	for _, p := range parts {
		total += len(p)
	}
	all := make([]Result, 0, total)
	for _, p := range parts {
		all = append(all, p...)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Score != all[j].Score {
			return all[i].Score > all[j].Score
		}
		return all[i].Path < all[j].Path
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
	}
	return all
}
