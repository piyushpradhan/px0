package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"px0/internal/render"
	"px0/internal/symbols"
)

// NavHit is one navigation result, shaped exactly like a search Match so the
// UI renders LSP answers and regex answers with the same code.
type NavHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Pre  string `json:"pre"`
	Mid  string `json:"mid"`
	Post string `json:"post"`
	Def  bool   `json:"def,omitempty"`
	Ext  bool   `json:"ext,omitempty"` // outside the indexed tree, e.g. stdlib
}

// utf16ToByte converts a column measured in UTF-16 code units (what JavaScript
// string indexes count) into a byte offset into the same line.
func utf16ToByte(line string, u16 int) int {
	if u16 <= 0 {
		return 0
	}
	units, bytes := 0, 0
	for _, r := range line {
		if units >= u16 {
			break
		}
		if r >= 0x10000 {
			units += 2
		} else {
			units++
		}
		bytes += utf8.RuneLen(r)
	}
	if bytes > len(line) {
		return len(line)
	}
	return bytes
}

// resolve turns a set of LSP ranges into display-ready hits, reading each target
// file through the same cache the editor uses.
func (m *lspManager) resolve(c *lspClient, locs []lspLocation, markDef bool) []NavHit {
	type fileKey struct {
		abs, rel string
		ext      bool
	}
	byFile := map[fileKey][]lspRange{}
	var order []fileKey

	for _, l := range locs {
		abs, err := uriToPath(l.URI)
		if err != nil {
			continue
		}
		// Outside the indexed tree is still worth jumping to, so treePath
		// allows this exact file to be opened even though the tree guard
		// would refuse it.
		rel, ext := m.treePath(abs, true)
		k := fileKey{abs, rel, ext}
		if _, seen := byFile[k]; !seen {
			order = append(order, k)
		}
		byFile[k] = append(byFile[k], l.Range)
	}

	var out []NavHit
	for _, k := range order {
		d, err := render.Open(k.abs, k.rel)
		ranges := byFile[k]
		sort.Slice(ranges, func(i, j int) bool {
			if ranges[i].Start.Line != ranges[j].Start.Line {
				return ranges[i].Start.Line < ranges[j].Start.Line
			}
			return ranges[i].Start.Character < ranges[j].Start.Character
		})
		seen := map[int]bool{}
		for _, r := range ranges {
			line := r.Start.Line + 1
			if seen[line] {
				continue // one entry per line keeps the list readable
			}
			seen[line] = true

			text := ""
			from := 0
			if err == nil && d != nil && r.Start.Line >= 0 && r.Start.Line < d.Total {
				text = d.Raw(line)
				_, from = c.fromLSPLine(text, r.Start)
			}
			to := from
			if r.End.Line == r.Start.Line && err == nil && d != nil && r.End.Line >= 0 && r.End.Line < d.Total {
				_, to = c.fromLSPLine(text, r.End)
			}
			if to <= from || to > len(text) {
				to = len(text)
				if from > to {
					from = to
				}
			}
			h := snip([]byte(text), from, to)
			out = append(out, NavHit{
				Path: k.rel, Line: line, Pre: h.Pre, Mid: h.Mid, Post: h.Post,
				Def: markDef, Ext: k.ext,
			})
		}
	}
	return out
}

type lspLocationOrLink struct {
	URI                  string    `json:"uri"`
	Range                lspRange  `json:"range"`
	TargetURI            string    `json:"targetUri"`
	TargetRange          *lspRange `json:"targetRange"`
	TargetSelectionRange *lspRange `json:"targetSelectionRange"`
}

func (l lspLocationOrLink) toLocation() (lspLocation, bool) {
	if l.URI != "" {
		return lspLocation{URI: l.URI, Range: l.Range}, true
	}
	if l.TargetURI != "" {
		if l.TargetSelectionRange != nil {
			return lspLocation{URI: l.TargetURI, Range: *l.TargetSelectionRange}, true
		}
		if l.TargetRange != nil {
			return lspLocation{URI: l.TargetURI, Range: *l.TargetRange}, true
		}
		return lspLocation{URI: l.TargetURI}, true
	}
	return lspLocation{}, false
}

// locate runs one position-based request and normalises the two shapes a server
// may answer with (Location or LocationLink).
func (m *lspManager) locate(ctx context.Context, method, abs, rel string, line, u16col int, extra map[string]any) ([]NavHit, error) {
	c, err := m.client(ctx, rel)
	if err != nil {
		return nil, err
	}
	if err := c.ensureOpen(abs, rel); err != nil {
		return nil, err
	}

	d, err := render.Open(abs, rel)
	if err != nil {
		return nil, err
	}
	rawLine := d.Raw(line)
	byteCol := utf16ToByte(rawLine, u16col)

	params := map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(abs)},
		"position":     c.toLSP(rawLine, line, byteCol),
	}
	for k, v := range extra {
		params[k] = v
	}

	var rawMsg json.RawMessage
	if err := c.call(ctx, method, params, &rawMsg); err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(rawMsg)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return []NavHit{}, nil
	}
	if trimmed[0] == '{' {
		var one lspLocationOrLink
		if err := json.Unmarshal(trimmed, &one); err != nil {
			return nil, err
		}
		if loc, ok := one.toLocation(); ok {
			return m.resolve(c, []lspLocation{loc}, method != "textDocument/references"), nil
		}
		return []NavHit{}, nil
	}

	var items []lspLocationOrLink
	if err := json.Unmarshal(trimmed, &items); err != nil {
		return nil, err
	}
	locs := make([]lspLocation, 0, len(items))
	for _, item := range items {
		if loc, ok := item.toLocation(); ok {
			locs = append(locs, loc)
		}
	}
	return m.resolve(c, locs, method != "textDocument/references"), nil
}

func decodeRange(v any) lspRange {
	mv, ok := v.(map[string]any)
	if !ok {
		return lspRange{}
	}
	pos := func(key string) lspPosition {
		p, ok := mv[key].(map[string]any)
		if !ok {
			return lspPosition{}
		}
		l, _ := p["line"].(float64)
		c, _ := p["character"].(float64)
		return lspPosition{Line: int(l), Character: int(c)}
	}
	return lspRange{Start: pos("start"), End: pos("end")}
}

// Definition queries the language server for the definition site(s) of the symbol at the given position.
// Line is 1-based and col is the UTF-16 code unit offset from the frontend.
func (m *lspManager) Definition(ctx context.Context, abs, rel string, line, col int) ([]NavHit, error) {
	return m.locate(ctx, "textDocument/definition", abs, rel, line, col, nil)
}

// References queries the language server for all reference locations of the symbol at the given position,
// including its declaration site.
func (m *lspManager) References(ctx context.Context, abs, rel string, line, col int) ([]NavHit, error) {
	return m.locate(ctx, "textDocument/references", abs, rel, line, col,
		map[string]any{"context": map[string]any{"includeDeclaration": true}})
}

// Symbols returns the document outline, flattened with indentation that mirrors
// the server's nesting.
func (m *lspManager) Symbols(ctx context.Context, abs, rel string) ([]symbols.Symbol, error) {
	c, err := m.client(ctx, rel)
	if err != nil {
		return nil, err
	}
	if err := c.ensureOpen(abs, rel); err != nil {
		return nil, err
	}

	var raw []lspDocumentSymbol
	if err := c.call(ctx, "textDocument/documentSymbol", map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(abs)},
	}, &raw); err != nil {
		return nil, err
	}

	var out []symbols.Symbol
	var walk func(syms []lspDocumentSymbol, depth int)
	walk = func(syms []lspDocumentSymbol, depth int) {
		for _, s := range syms {
			rng := s.SelectionRange
			if s.Location != nil {
				rng = s.Location.Range // symbolInformation form
			}
			kind := lspSymbolKind[s.Kind]
			if kind == "" {
				kind = "sym"
			}
			out = append(out, symbols.Symbol{
				Name: s.Name, Kind: kind, Line: rng.Start.Line + 1, Indent: depth * 2,
			})
			if len(s.Children) > 0 {
				walk(s.Children, depth+1)
			}
		}
	}
	walk(raw, 0)

	// symbolInformation comes back unordered often enough to be worth fixing.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out, nil
}

// ---------------------------------------------------------------- hover

// HoverInfo is what the UI shows in the hover card: a syntax-highlighted
// signature and whatever documentation the server attached to it.
type HoverInfo struct {
	Signature string `json:"signature"` // pre-highlighted HTML, one line per entry
	Doc       string `json:"doc"`
	Empty     bool   `json:"empty"`
}

// Hover asks the server what a position is, and renders the answer.
func (m *lspManager) Hover(ctx context.Context, abs, rel string, line, col int) (*HoverInfo, error) {
	c, err := m.client(ctx, rel)
	if err != nil {
		return nil, err
	}
	if err := c.ensureOpen(abs, rel); err != nil {
		return nil, err
	}
	d, err := render.Open(abs, rel)
	if err != nil {
		return nil, err
	}
	byteCol := utf16ToByte(d.Raw(line), col)

	var res struct {
		Contents json.RawMessage `json:"contents"`
	}
	err = c.call(ctx, "textDocument/hover", map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(abs)},
		"position":     c.toLSP(d.Raw(line), line, byteCol),
	}, &res)
	if err != nil {
		return nil, err
	}
	if len(res.Contents) == 0 {
		return &HoverInfo{Empty: true}, nil
	}

	code, prose := parseHoverContents(res.Contents)
	if code == "" && prose == "" {
		return &HoverInfo{Empty: true}, nil
	}
	return &HoverInfo{Signature: highlightSnippet(code, rel), Doc: prose}, nil
}

// parseHoverContents flattens the three shapes the spec allows: a MarkupContent
// object, a MarkedString, or an array of MarkedStrings.
func parseHoverContents(raw json.RawMessage) (code, prose string) {
	var markup struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &markup); err == nil && markup.Value != "" {
		return splitMarkdown(markup.Value)
	}

	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return splitMarkdown(one)
	}

	var many []json.RawMessage
	if err := json.Unmarshal(raw, &many); err == nil {
		var codes, proses []string
		for _, item := range many {
			var s string
			if json.Unmarshal(item, &s) == nil {
				c, p := splitMarkdown(s)
				if c != "" {
					codes = append(codes, c)
				}
				if p != "" {
					proses = append(proses, p)
				}
				continue
			}
			var ms struct {
				Language string `json:"language"`
				Value    string `json:"value"`
			}
			if json.Unmarshal(item, &ms) == nil && ms.Value != "" {
				if ms.Language != "" {
					codes = append(codes, ms.Value)
				} else {
					proses = append(proses, ms.Value)
				}
			}
		}
		return strings.Join(codes, "\n"), strings.Join(proses, "\n\n")
	}
	return "", ""
}

// splitMarkdown pulls fenced code blocks out of a hover body; what is left is
// documentation prose.
func splitMarkdown(s string) (code, prose string) {
	var codes, text []string
	inFence := false
	var fence []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if inFence {
				codes = append(codes, strings.Join(fence, "\n"))
				fence = nil
			}
			inFence = !inFence
			continue
		}
		if inFence {
			fence = append(fence, line)
		} else {
			text = append(text, line)
		}
	}
	if len(fence) > 0 {
		codes = append(codes, strings.Join(fence, "\n"))
	}
	code = strings.TrimSpace(strings.Join(codes, "\n"))
	prose = strings.TrimSpace(stripMarkdown(strings.Join(text, "\n")))

	// A server that only speaks plaintext sends no fences at all. Its first
	// line is still the declaration, so promote it rather than showing nothing.
	if code == "" && prose != "" {
		first, rest, _ := strings.Cut(prose, "\n")
		if looksLikeDeclaration(first) {
			code, prose = first, strings.TrimSpace(rest)
		}
	}
	const maxProse = 900
	if len(prose) > maxProse {
		if cut := strings.LastIndex(prose[:maxProse], " "); cut > 0 {
			prose = prose[:cut]
		} else {
			prose = prose[:maxProse]
		}
		prose += "…"
	}
	return code, prose
}

// looksLikeDeclaration is a deliberately conservative test: short, no trailing
// sentence punctuation, and carrying syntax a prose line would not.
func looksLikeDeclaration(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 200 || strings.HasSuffix(s, ".") {
		return false
	}
	return strings.ContainsAny(s, "(){}[]<>:=*&") || strings.Contains(s, " ")
}

var (
	mdInline = strings.NewReplacer("**", "", "__", "", "`", "")
	mdLink   = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
	// No backreference: Go's regexp is RE2, so the three rule forms are spelled out.
	mdRule     = regexp.MustCompile(`^\s*(?:(?:-\s*){3,}|(?:\*\s*){3,}|(?:_\s*){3,})$`)
	mdOnlyLink = regexp.MustCompile(`^\s*\[[^\]]*\]\([^)]*\)\s*$`)
)

// stripMarkdown flattens a hover body into something readable in a tooltip.
// Nothing here tries to be a markdown renderer; it removes the decorations that
// would otherwise show up as literal punctuation.
func stripMarkdown(s string) string {
	var out []string
	blank := 0
	for _, line := range strings.Split(s, "\n") {
		if mdRule.MatchString(line) || mdOnlyLink.MatchString(line) {
			continue // horizontal rules and bare "see also" links are noise
		}
		line = strings.TrimRight(mdInline.Replace(mdLink.ReplaceAllString(line, "$1")), " ")
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

const maxSignatureLines = 12

// highlightSnippet runs a hover signature through the same lexer the file uses,
// so the card matches the editor.
func highlightSnippet(code, rel string) string {
	if code == "" {
		return ""
	}
	lines := strings.Split(code, "\n")
	if len(lines) > maxSignatureLines {
		lines = append(lines[:maxSignatureLines], "…")
		code = strings.Join(lines, "\n")
	}
	d := render.NewDoc(code, rel)
	out, _ := d.Lines(0, d.Total)
	return strings.Join(out, "\n")
}

// ---------------------------------------------------------------- call trails

// Call trails: who calls a function, and what it calls, one level at a time.
// The server identifies each function by an opaque CallHierarchyItem that has
// to be handed back verbatim to expand the next level. px0 keeps no state
// between requests, so every node carries its item to the browser, which sends
// it back when the reader expands that node.

// CallNode is one function in a call trail.
type CallNode struct {
	Name     string `json:"name"`
	Detail   string `json:"detail,omitempty"`
	Kind     string `json:"kind"`
	Path     string `json:"path"` // where the function is declared
	Line     int    `json:"line"`
	Ext      bool   `json:"ext,omitempty"`
	SitePath string `json:"sitePath,omitempty"` // file holding the call expressions
	Sites    []int  `json:"sites,omitempty"`    // lines of those calls, ascending
	Item     string `json:"item"`               // CallHierarchyItem, returned as-is to expand
}

// lspCallItem represents the LSP wire format for CallHierarchyItem
// defined in the Language Server Protocol specification.
type lspCallItem struct {
	Name           string   `json:"name"`
	Kind           int      `json:"kind"`
	Detail         string   `json:"detail"`
	URI            string   `json:"uri"`
	SelectionRange lspRange `json:"selectionRange"`
}

// treePath maps an absolute path to what the UI shows and opens: relative
// inside the indexed tree, absolute outside it. Only paths a language server
// named may be allowlisted for opening; paths that arrived from the browser
// must pass allow=false.
func (m *lspManager) treePath(abs string, allow bool) (rel string, ext bool) {
	rel, err := filepath.Rel(m.root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		// The standard library, or a dependency in the module cache.
		if allow {
			m.allow(abs)
		}
		return filepath.ToSlash(abs), true
	}
	return filepath.ToSlash(rel), false
}

func (m *lspManager) callNode(raw json.RawMessage, allow bool) (CallNode, lspCallItem, error) {
	var it lspCallItem
	if err := json.Unmarshal(raw, &it); err != nil || it.URI == "" {
		return CallNode{}, it, fmt.Errorf("bad call hierarchy item")
	}
	abs, err := uriToPath(it.URI)
	if err != nil {
		return CallNode{}, it, err
	}
	rel, ext := m.treePath(abs, allow)
	kind := lspSymbolKind[it.Kind]
	if kind == "" {
		kind = "sym"
	}
	return CallNode{
		Name: it.Name, Detail: it.Detail, Kind: kind,
		Path: rel, Line: it.SelectionRange.Start.Line + 1, Ext: ext,
		Item: string(raw),
	}, it, nil
}

// PrepareCalls resolves the function at a position into the root(s) of a trail.
func (m *lspManager) PrepareCalls(ctx context.Context, abs, rel string, line, u16col int) ([]CallNode, error) {
	c, err := m.client(ctx, rel)
	if err != nil {
		return nil, err
	}
	if err := c.ensureOpen(abs, rel); err != nil {
		return nil, err
	}
	d, err := render.Open(abs, rel)
	if err != nil {
		return nil, err
	}
	byteCol := utf16ToByte(d.Raw(line), u16col)

	var raw []json.RawMessage
	if err := c.call(ctx, "textDocument/prepareCallHierarchy", map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(abs)},
		"position":     c.toLSP(d.Raw(line), line, byteCol),
	}, &raw); err != nil {
		return nil, err
	}
	out := make([]CallNode, 0, len(raw))
	for _, r := range raw {
		if n, _, err := m.callNode(r, true); err == nil {
			out = append(out, n)
		}
	}
	return out, nil
}

// Calls expands one node of a trail: its callers, or with outgoing its callees.
// rel picks the language server; item is a node's Item as the browser sent it.
func (m *lspManager) Calls(ctx context.Context, rel, item string, outgoing bool) ([]CallNode, error) {
	c, err := m.client(ctx, rel)
	if err != nil {
		return nil, err
	}
	_, parent, err := m.callNode(json.RawMessage(item), false)
	if err != nil {
		return nil, err
	}
	// Some servers only answer about documents they were handed. Only files in
	// the tree are read for this: the item came from the browser.
	if pabs, err := uriToPath(parent.URI); err == nil {
		if prel, ext := m.treePath(pabs, false); !ext {
			c.ensureOpen(pabs, prel)
		}
	}

	method := "callHierarchy/incomingCalls"
	if outgoing {
		method = "callHierarchy/outgoingCalls"
	}
	var res []struct {
		From       json.RawMessage `json:"from"`
		To         json.RawMessage `json:"to"`
		FromRanges []lspRange      `json:"fromRanges"`
	}
	if err := c.call(ctx, method, map[string]any{"item": json.RawMessage(item)}, &res); err != nil {
		return nil, err
	}

	out := make([]CallNode, 0, len(res))
	for _, r := range res {
		raw := r.From
		if outgoing {
			raw = r.To
		}
		n, it, err := m.callNode(raw, true)
		if err != nil {
			continue
		}
		// fromRanges sit inside the caller: the returned function for incoming
		// calls, the function being expanded for outgoing ones.
		siteURI := it.URI
		if outgoing {
			siteURI = parent.URI
		}
		if sabs, err := uriToPath(siteURI); err == nil {
			n.SitePath, _ = m.treePath(sabs, false)
		}
		n.Sites = siteLines(r.FromRanges)
		out = append(out, n)
	}

	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if outgoing {
			return firstSite(a) < firstSite(b) // callees in the order they are called
		}
		if a.Path != b.Path {
			return a.Path < b.Path
		}
		return a.Line < b.Line
	})
	return out, nil
}

func siteLines(rs []lspRange) []int {
	seen := map[int]bool{}
	var out []int
	for _, r := range rs {
		l := r.Start.Line + 1
		if !seen[l] {
			seen[l] = true
			out = append(out, l)
		}
	}
	sort.Ints(out)
	return out
}

func firstSite(n CallNode) int {
	if len(n.Sites) == 0 {
		return 0
	}
	return n.Sites[0]
}
