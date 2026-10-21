package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/px0-ai/harness"

	"px0/internal/render"
	"px0/internal/settings"
	"px0/internal/ui"
)

// Editing through a coding harness. px0 never authors a change itself: it
// composes an instruction anchored to a line range, hands it to a harness
// already installed on this machine, and reloads whatever moved once that
// harness exits. The harness edits; px0 stays the reader that knows exactly
// when to look again.
//
// Harnesses are discovered and executed using github.com/px0-ai/harness.
// Discovery alone never enables editing: running a general-purpose agent
// over a workspace is a decision the user makes once, and it is remembered
// in the settings file rather than a flag.

const (
	agentTimeout  = harness.DefaultTimeout
	agentLogBytes = harness.DefaultLogBytes
)

// agentHarness is one row of the picker.
type agentHarness struct {
	Name      string   `json:"name"`
	Cmd       string   `json:"cmd"`
	Installed bool     `json:"installed"`
	Path      string   `json:"path,omitempty"`
	Models    []string `json:"models,omitempty"`
	Model     string   `json:"model,omitempty"`
}

// agentBatchItem represents one instruction anchored to a file range.
type agentBatchItem struct {
	Abs         string `json:"-"`
	Path        string `json:"path"`
	L1          int    `json:"l1"`
	L2          int    `json:"l2"`
	Instruction string `json:"instruction"`
}

// agentJob represents a single background editing task dispatched to an AI coding harness.
// It tracks real-time progress, log outputs, duration, affected files, and cancellation handlers.
type agentJob struct {
	ID         int64            `json:"id"`                   // Unique monotonic job identifier
	Harness    string           `json:"harness"`              // Name of the harness executing this job
	Path       string           `json:"path"`                 // Relative file path targeted for editing
	Lines      string           `json:"lines"`                // Line range formatted string (e.g. "L12-L30")
	Running    bool             `json:"running"`              // True while harness process is actively executing
	Error      string           `json:"error,omitempty"`      // Error message if the job failed or was aborted
	Log        string           `json:"log"`                  // Tail of merged stdout/stderr log output
	Stdout     string           `json:"stdout,omitempty"`     // Stdout log output tail
	Stderr     string           `json:"stderr,omitempty"`     // Stderr log output tail
	Changed    []string         `json:"changed"`              // Files detected as modified after job execution
	Ms         int64            `json:"ms"`                   // Elapsed runtime in milliseconds
	Tracked    bool             `json:"tracked"`              // Whether telemetry tracking has been recorded
	ThreadID   string           `json:"threadId,omitempty"`   // The thread this edit runs as, when it runs as one
	BatchCount int              `json:"batchCount,omitempty"` // Number of items in batch review edit
	Items      []agentBatchItem `json:"items,omitempty"`      // Detailed batch items if multi-file edit

	cancel context.CancelFunc
	out    *tailBuffer
	stderr *tailBuffer
	start  time.Time
}

var (
	// The refusals the UI reacts to rather than merely reporting.
	errAgentBusy  = errors.New("an edit is already running")
	errAgentDirty = errors.New("uncommitted")
	errAgentNone  = errors.New("no coding harness is selected")
)

// lineStreamer forwards complete lines to w with a prefix in real time,
// while also storing the raw bytes into a tailBuffer.
type lineStreamer struct {
	mu     sync.Mutex
	buf    *tailBuffer
	prefix string
	line   []byte
	w      io.Writer
}

func newLineStreamer(buf *tailBuffer, prefix string, w io.Writer) *lineStreamer {
	return &lineStreamer{buf: buf, prefix: prefix, w: w}
}

func (s *lineStreamer) Write(p []byte) (int, error) {
	if s.buf != nil {
		s.buf.Write(p)
	}
	if s.w == nil || ui.Quiet {
		return len(p), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range p {
		if b == '\n' {
			if len(s.line) > 0 {
				fmt.Fprintf(s.w, "  %s %s\n", s.prefix, string(s.line))
				s.line = s.line[:0]
			}
		} else if b != '\r' {
			s.line = append(s.line, b)
		}
	}
	return len(p), nil
}

func (s *lineStreamer) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.line) > 0 && s.w != nil && !ui.Quiet {
		fmt.Fprintf(s.w, "  %s %s\n", s.prefix, string(s.line))
		s.line = s.line[:0]
	}
}

// agentManager owns discovery, the current choice, and every edit currently in
// flight. Several harnesses can run at once as long as they touch disjoint
// line ranges: two harnesses rewriting the same lines produces a state nobody
// can review afterwards, so overlapping ranges are refused rather than queued.
type agentManager struct {
	root string
	lsp  *lspManager

	mu       sync.Mutex
	selected string            // preset name, or the template itself when pinned
	args     []string          // resolved argv, nil when nothing is selected
	pinned   bool              // -agent was given, so the UI cannot change it
	models   map[string]string // harness name -> selected model
	jobs     map[int64]*agentJob
	seq      int64
	onEdit   func()

	// Set by the thread manager: inline and batch edits run as threads.
	threadJob    func(id int64) *agentJob // id 0 means the most recent
	threadCancel func(id int64) bool      // id 0 means every one running
}

// newAgentManager wires discovery and restores the remembered choice. A flag
// value pins a harness (or an arbitrary command template) for this run and is
// the only case that can fail: a bad -agent should stop startup, whereas a
// stale settings file should just leave nothing selected.
func newAgentManager(root, flagSpec string, lsp *lspManager) (*agentManager, error) {
	m := &agentManager{
		root:   root,
		lsp:    lsp,
		models: map[string]string{},
	}
	s := settings.Read()
	if s.Models != nil {
		for k, v := range s.Models {
			m.models[k] = v
		}
	}

	if spec := strings.TrimSpace(flagSpec); spec != "" {
		name, args, chosenModel, err := resolveAgentSpec(spec, m.models[spec])
		if err != nil {
			return nil, err
		}
		m.selected, m.args, m.pinned = name, args, true
		if chosenModel != "" {
			m.models[name] = chosenModel
		}
		return m, nil
	}

	if s.Agent != "" {
		if name, args, chosenModel, err := resolveAgentSpec(s.Agent, m.models[s.Agent]); err == nil {
			m.selected, m.args = name, args
			if chosenModel != "" {
				m.models[name] = chosenModel
			}
		}
	}
	return m, nil
}

// resolveAgentSpec turns a preset name or a command template into argv, and
// verifies the binary exists now rather than at first use.
func resolveAgentSpec(spec, model string) (string, []string, string, error) {
	cmd, err := harness.Resolve(spec, model)
	if err != nil {
		return "", nil, "", err
	}
	return cmd.Name, cmd.Args, cmd.Model, nil
}

// Detect reports every harness known and whether it is installed right
// now, so a tool installed since startup shows up without a restart.
// Models are only discovered for the currently selected harness to avoid
// executing external commands (such as `agy models`) on startup for unused tools.
func (m *agentManager) Detect() []agentHarness {
	m.mu.Lock()
	savedModels := make(map[string]string, len(m.models))
	for k, v := range m.models {
		savedModels[k] = v
	}
	selected := m.selected
	m.mu.Unlock()

	detected := harness.Detect(savedModels)
	out := make([]agentHarness, 0, len(detected))
	for _, h := range detected {
		var models []string
		if h.Installed && h.Name == selected {
			if list, err := harness.DiscoverModels(context.Background(), h.Name); err == nil {
				models = list
			}
		}
		curModel := h.Model
		if curModel == "" && len(models) > 0 {
			curModel = models[0]
		}
		out = append(out, agentHarness{
			Name:      h.Name,
			Cmd:       h.Cmd,
			Installed: h.Installed,
			Path:      h.Path,
			Models:    models,
			Model:     curModel,
		})
	}
	return out
}

func (m *agentManager) Name() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.selected
}

func (m *agentManager) Model() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.selected == "" {
		return ""
	}
	return m.models[m.selected]
}

// current returns the selected harness, its headless argv and its model, read
// together so a run never mixes one harness's name with another's flags.
func (m *agentManager) current() (string, []string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.args == nil {
		return "", nil, ""
	}
	return m.selected, append([]string(nil), m.args...), m.models[m.selected]
}

func (m *agentManager) Pinned() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pinned
}

// Select remembers a harness for this workspace and every later run. Passing an
// empty name turns editing back off.
func (m *agentManager) Select(name string, modelOpt ...string) error {
	m.mu.Lock()
	if m.pinned {
		m.mu.Unlock()
		return errors.New("px0 was started with -agent, so the harness is fixed for this run")
	}
	m.mu.Unlock()

	name = strings.TrimSpace(name)
	if name == "" {
		m.mu.Lock()
		m.selected, m.args = "", nil
		m.mu.Unlock()
		return settings.Write(settings.Settings{})
	}

	reqModel := ""
	if len(modelOpt) > 0 {
		reqModel = strings.TrimSpace(modelOpt[0])
	}
	m.mu.Lock()
	if reqModel == "" {
		reqModel = m.models[name]
	}
	m.mu.Unlock()

	display, args, chosenModel, err := resolveAgentSpec(name, reqModel)
	if err != nil {
		ui.Status("err", fmt.Sprintf("agent: failed to select harness %q", name), err.Error(), 0, os.Stdout)
		return err
	}
	m.mu.Lock()
	m.selected, m.args = display, args
	if m.models == nil {
		m.models = map[string]string{}
	}
	if chosenModel != "" {
		m.models[display] = chosenModel
	}
	savedModels := make(map[string]string, len(m.models))
	for k, v := range m.models {
		savedModels[k] = v
	}
	m.mu.Unlock()

	modelNote := ""
	if chosenModel != "" {
		modelNote = fmt.Sprintf(" (%s)", chosenModel)
	}
	ui.Status("ok", "agent", fmt.Sprintf("%s%s", display, modelNote), 0, os.Stdout)
	// Persist the spec as given, not the display name: a command template
	// shortens to its binary for display and would not survive the round trip.
	return settings.Write(settings.Settings{Agent: name, Models: savedModels})
}

// Job returns a snapshot of job id, or of the most recently started job when
// id is 0, or nil when there isn't one.
func (m *agentManager) Job(id int64) *agentJob {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	j := m.jobs[id]
	if id == 0 {
		for _, cand := range m.jobs {
			if j == nil || cand.ID > j.ID {
				j = cand
			}
		}
	}
	var cp *agentJob
	if j != nil {
		c := *j
		c.Log = j.out.String()
		c.Stdout = c.Log
		if j.stderr != nil {
			c.Stderr = j.stderr.String()
		}
		if c.Running {
			c.Ms = time.Since(j.start).Milliseconds()
		}
		cp = &c
	}
	lookup := m.threadJob
	m.mu.Unlock()

	// Inline and batch edits run as threads and share this id space, so one
	// poll endpoint serves both. Asked for the latest, the newer of the two wins.
	if lookup != nil {
		if tj := lookup(id); tj != nil && (cp == nil || tj.ID > cp.ID) {
			return tj
		}
	}
	return cp
}

// nextJobID hands out an id from the sequence every job shares.
func (m *agentManager) nextJobID() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	return m.seq
}

// StartPrompt dispatches a one-shot prompt to the selected harness with no
// target file -- used for generating text (e.g. a commit message) rather
// than editing code. There is no file range to anchor an overlap check
// against, so a prompt job is never blocked by, or blocks, a file edit.
func (m *agentManager) StartPrompt(label, prompt string) (*agentJob, error) {
	m.mu.Lock()
	if m.args == nil {
		m.mu.Unlock()
		ui.Status("err", "agent", "prompt dispatch refused: no coding harness selected", 0, os.Stdout)
		return nil, errAgentNone
	}
	args := m.args
	name := m.selected
	m.seq++
	ctx, cancel := context.WithTimeout(context.Background(), agentTimeout)
	job := &agentJob{
		ID:      m.seq,
		Harness: name,
		Path:    label,
		Running: true,
		Changed: []string{},
		Tracked: gitAvailable(m.root),
		out:     &tailBuffer{max: agentLogBytes},
		stderr:  &tailBuffer{max: agentLogBytes},
		start:   time.Now(),
		cancel:  cancel,
	}
	if m.jobs == nil {
		m.jobs = map[int64]*agentJob{}
	}
	m.jobs[job.ID] = job
	modelStr := ""
	if m.models != nil && m.models[name] != "" {
		modelStr = fmt.Sprintf(" (%s)", m.models[name])
	}
	m.mu.Unlock()

	ui.Status("step", "agent", fmt.Sprintf("#%d %s%s · %s", job.ID, name, modelStr, label), 0, os.Stdout)

	go m.run(ctx, cancel, job, args, prompt)
	return m.Job(job.ID), nil
}

func (m *agentManager) run(ctx context.Context, cancel context.CancelFunc, job *agentJob, template []string, prompt string) {
	defer cancel()
	defer func() {
		m.mu.Lock()
		job.Running = false
		job.Ms = time.Since(job.start).Milliseconds()
		if job.cancel != nil {
			job.cancel = nil
		}
		m.mu.Unlock()
	}()

	if ui.Verbose {
		ui.VerbosePrompt(job.ID, job.Harness, prompt, os.Stdout)
	}

	before := worktreeSnapshot(m.root)

	stdoutStreamer := newLineStreamer(job.out, ui.Faint("│", os.Stdout), os.Stdout)
	stderrStreamer := newLineStreamer(job.stderr, ui.Dim("│", os.Stdout), os.Stdout)

	res := harness.Run(ctx, harness.Command{Name: job.Harness, Args: template}, prompt, harness.Options{
		Dir:      m.root,
		Timeout:  agentTimeout,
		LogBytes: agentLogBytes,
		Stdout:   stdoutStreamer,
		Stderr:   stderrStreamer,
	})
	stdoutStreamer.Flush()
	stderrStreamer.Flush()
	err := res.Err

	changed := changedSince(m.root, before)
	m.settle(changed)

	m.mu.Lock()
	job.Running = false
	job.Changed = changed
	job.Ms = res.Duration.Milliseconds()
	if err != nil {
		job.Error = err.Error()
	}
	stdoutOutput := job.out.String()
	stderrOutput := job.stderr.String()
	m.mu.Unlock()

	durStr := ui.FmtDuration(time.Duration(job.Ms) * time.Millisecond)
	if err != nil {
		ui.Status("err", "agent", fmt.Sprintf("#%d %s failed in %s: %s", job.ID, job.Harness, durStr, err.Error()), 0, os.Stdout)
		if trimmedErr := strings.TrimSpace(stderrOutput); trimmedErr != "" {
			ui.KV("harness stderr", trimmedErr, 0, os.Stdout)
		}
		if trimmedOut := strings.TrimSpace(stdoutOutput); trimmedOut != "" {
			ui.KV("harness stdout", trimmedOut, 0, os.Stdout)
		}
	} else {
		var summary string
		switch len(changed) {
		case 0:
			summary = "no files changed"
		case 1:
			summary = fmt.Sprintf("1 file changed: %s", changed[0])
		default:
			summary = fmt.Sprintf("%d files changed: %s", len(changed), strings.Join(changed, ", "))
		}
		ui.Status("ok", "agent", fmt.Sprintf("#%d %s · %s  (%s)", job.ID, job.Harness, durStr, summary), 0, os.Stdout)
		if len(changed) == 0 && job.Tracked {
			if trimmedErr := strings.TrimSpace(stderrOutput); trimmedErr != "" {
				ui.KV("harness stderr", trimmedErr, 0, os.Stdout)
			}
		}
	}
}

// settle drops every trace of the old bytes. Open memoises on path+mtime+size
// so a rewritten file already misses the cache, but a harness that truncates
// and writes in place can be read mid-write, and that torn copy would then sit
// under a key nothing invalidates. Evicting is cheaper than reasoning about it.
// Language servers hold their own copy of the file and never saw the write, so
// they are closed here too and reopen on the next request.
func (m *agentManager) settle(changed []string) {
	for _, rel := range changed {
		abs := filepath.Join(m.root, filepath.FromSlash(rel))
		render.Evict(abs)
		if m.lsp != nil {
			m.lsp.CloseDoc(abs, rel)
		}
	}
	if len(changed) > 0 && m.onEdit != nil {
		m.onEdit()
	}
}

// Cancel stops every harness currently running. Whatever each has already
// written stays.
func (m *agentManager) Cancel() bool {
	return m.CancelJob(0)
}

// CancelJob stops the harness run with the given id, or every running harness
// when id is 0.
func (m *agentManager) CancelJob(id int64) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	tc := m.threadCancel
	m.mu.Unlock()
	fromThreads := tc != nil && tc(id)
	if fromThreads && id != 0 {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if id != 0 {
		j := m.jobs[id]
		if j != nil && j.Running && j.cancel != nil {
			ui.Status("warn", "agent", fmt.Sprintf("cancelled in-flight run with %s (job %d)", j.Harness, j.ID), 0, os.Stdout)
			j.Running = false
			j.Error = "cancelled"
			cancel := j.cancel
			j.cancel = nil
			cancel()
			return true
		}
		return false
	}
	cancelled := false
	for _, j := range m.jobs {
		if !j.Running || j.cancel == nil {
			continue
		}
		ui.Status("warn", "agent", fmt.Sprintf("cancelled in-flight run with %s (job %d)", j.Harness, j.ID), 0, os.Stdout)
		j.Running = false
		j.Error = "cancelled"
		cancel := j.cancel
		j.cancel = nil
		cancel()
		cancelled = true
	}
	return cancelled || fromThreads
}

func (m *agentManager) Close() { m.Cancel() }

// changedSince reports the paths whose state differs from the snapshot taken
// before the run. Asking git is the only honest answer to "what did it touch":
// a harness routinely edits files nobody pointed it at.
func changedSince(root string, before map[string]string) []string {
	return changedSinceMaps(before, worktreeSnapshot(root))
}

// worktreeSnapshot is git status with each listed file's size and mtime folded
// into its entry. Status alone misses the common case of editing a file that is
// already modified: it reads "M" before and after, so the edit would go unseen.
// Files git lists as clean are left out, and those still surface through status.
func worktreeSnapshot(root string) map[string]string {
	st := gitStatus(root)
	for rel, code := range st {
		if fi, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err == nil {
			st[rel] = code + " " + strconv.FormatInt(fi.Size(), 10) + " " + strconv.FormatInt(fi.ModTime().UnixNano(), 10)
		}
	}
	return st
}

// changedSinceMaps compares two status snapshots in both directions. Outside a
// git repository both are nil and nothing is ever reported as changed, which is
// why a job carries Tracked for the client to fall back on.
func changedSinceMaps(before, after map[string]string) []string {
	out := []string{}
	for path, st := range after {
		if before[path] != st {
			out = append(out, path)
		}
	}
	// A file restored to its committed state leaves the status list entirely.
	for path := range before {
		if _, still := after[path]; !still {
			out = append(out, path)
		}
	}
	// Map iteration order is random; sort so the job's summary and API
	// response list the same files in the same order on every run.
	sort.Strings(out)
	return out
}

func lineRef(l1, l2 int) string {
	if l1 == l2 {
		return strconv.Itoa(l1)
	}
	return strconv.Itoa(l1) + "-" + strconv.Itoa(l2)
}

// readLineRange returns lines l1..l2 of a file, 1-based and inclusive.
func readLineRange(abs string, l1, l2 int) (string, error) {
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if l1 < 1 {
		l1 = 1
	}
	if l2 < l1 {
		l2 = l1
	}
	if l1 > len(lines) {
		return "", fmt.Errorf("line %d is past the end of %s", l1, filepath.Base(abs))
	}
	if l2 > len(lines) {
		l2 = len(lines)
	}
	return strings.Join(lines[l1-1:l2], "\n"), nil
}

// commitMessagePrompt asks the harness to write a commit message for the
// staged changes. instruction is the user's git.commitMessageInstruction
// setting (empty when unset), appended verbatim so it can refine or override
// the base convention below.
func commitMessagePrompt(files []string, stat, diff, instruction string) string {
	var b strings.Builder
	b.WriteString("Write a git commit message for the staged changes below.\n")
	b.WriteString("Rules: imperative mood, a concise summary line under 72 characters, a blank line before an optional body, and explain why rather than just what changed.\n")
	b.WriteString("Output ONLY the commit message text -- no markdown code fences, no preamble, no explanation afterwards, and do not edit any files.\n")
	if instruction != "" {
		fmt.Fprintf(&b, "\nAdditional instructions from the user: %s\n", instruction)
	}
	if len(files) > 0 {
		fmt.Fprintf(&b, "\nChanged files (%d):\n", len(files))
		maxFiles := 100
		for i, f := range files {
			if i >= maxFiles {
				fmt.Fprintf(&b, "... and %d more files\n", len(files)-maxFiles)
				break
			}
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if strings.TrimSpace(stat) != "" {
		b.WriteString("\nSummary of changes (diffstat):\n")
		b.WriteString(stat)
		b.WriteString("\n")
	}
	if strings.TrimSpace(diff) != "" {
		b.WriteString("\nStaged diff:\n")
		b.WriteString(diff)
		b.WriteString("\n")
	}
	return b.String()
}

// gitSyncPrompt asks the harness to rebase (or merge) the current branch
// onto its upstream after a fast-forward-only pull was refused, or to finish
// one a previous attempt left half-done. It may run git and edit conflicted
// files, but must never push: what reaches the remote stays the user's call.
func gitSyncPrompt(strategy string, st gitSyncState) string {
	// A PR's base is fetched by px0 itself, with the forge token, into a ref
	// of its own; the harness works against that ref and must not fetch.
	onto, prefetched := st.Upstream, st.ontoRef != ""
	if prefetched {
		onto = st.ontoRef
	}
	remote, _, _ := strings.Cut(st.Upstream, "/")
	var b strings.Builder
	switch {
	case st.InProgress != "":
		fmt.Fprintf(&b, "A git %s of branch %s onto %s is in progress and stopped, most likely on conflicts. Finish it.\n\n", strategy, st.Branch, onto)
	case st.PR > 0:
		fmt.Fprintf(&b, "Pull request #%d (branch %s, checked out here as a detached HEAD) is out of date with its base branch %s: %d commit(s) on the PR, %d new on %s since it branched. Bring it up to date by running %s against %s, which holds %s's latest commits.\n\n", st.PR, st.Branch, st.Upstream, st.Ahead, st.Behind, st.Upstream, strategy, onto, st.Upstream)
	default:
		fmt.Fprintf(&b, "git pull is failing on branch %s: it can't fast-forward onto %s (%d local commit(s) not upstream, %d incoming). Run %s.\n\n", st.Branch, st.Upstream, st.Ahead, st.Behind, strategy)
	}
	b.WriteString("Steps:\n")
	if strategy == "rebase" {
		switch {
		case st.InProgress != "":
			b.WriteString("1. Run git status to see where the rebase stopped.\n")
		case prefetched:
			fmt.Fprintf(&b, "1. git rebase %s (already fetched: do not run git fetch or git pull).\n", onto)
		default:
			fmt.Fprintf(&b, "1. git fetch %s, then git rebase %s.\n", remote, st.Upstream)
		}
		b.WriteString("2. For each conflict, read both sides and resolve it so the intent of both the local and the incoming change survives. Remove every conflict marker, git add the files, and run GIT_EDITOR=true git rebase --continue. Repeat until the rebase completes.\n")
		b.WriteString("3. Never use git rebase --skip: it silently drops a local commit.\n")
	} else {
		switch {
		case st.InProgress != "":
			b.WriteString("1. Run git status to see which files are still conflicted.\n")
		case prefetched:
			fmt.Fprintf(&b, "1. git merge --no-edit -m \"Merge branch '%s' into %s\" %s (already fetched: do not run git fetch or git pull).\n", st.Upstream, st.Branch, onto)
		default:
			fmt.Fprintf(&b, "1. git fetch %s, then git merge --no-edit %s.\n", remote, st.Upstream)
		}
		b.WriteString("2. For each conflict, read both sides and resolve it so the intent of both the local and the incoming change survives. Remove every conflict marker, git add the files, and run git commit --no-edit to conclude the merge.\n")
	}
	fmt.Fprintf(&b, "\nRules: do not push or force-push. Do not reset, amend, or rewrite commits beyond what the %s itself does. Do not touch files that are not part of a conflict.\n", strategy)
	fmt.Fprintf(&b, "If a conflict cannot be resolved with confidence, run git %s --abort so the branch is back where it started, and say which files and why.\n", strategy)
	b.WriteString("Finish with a short summary: the conflicts you resolved (file and how), or that there were none.\n")
	return b.String()
}

// mergeMessagePrompt asks the harness for the commit message a squash (or a
// merge commit) of a PR lands on its base branch with. Like
// commitMessagePrompt it only writes text: no files, no git. draft is the
// message as it stands in the merge dialog, rewritten rather than ignored.
// commits is newest first.
func mergeMessagePrompt(method string, meta PRMeta, commits []UnpushedCommit, stat, diff, draft string, instructions []string) string {
	var b strings.Builder
	if method == MergeMethodSquash {
		fmt.Fprintf(&b, "Write the commit message for squash-merging pull request #%d (%q) into %s: its %d commit(s) become this one commit.\n", meta.Number, meta.Title, meta.BaseRef, len(commits))
		b.WriteString("Rules: imperative mood, a concise summary line under 72 characters, a blank line, then a body explaining why the change was made and what it does overall. Describe the end result, not the PR's history: leave out fixups, review rounds and reverted attempts.\n")
	} else {
		fmt.Fprintf(&b, "Write the merge commit message for merging pull request #%d (%q) from %s into %s.\n", meta.Number, meta.Title, meta.HeadRef, meta.BaseRef)
		b.WriteString("Rules: a concise summary line under 72 characters naming what the merge brings in, a blank line, then a short body summarizing the change.\n")
	}
	b.WriteString("Output ONLY the commit message text -- no markdown code fences, no preamble, no explanation afterwards, and do not edit any files or run git.\n")
	for _, in := range instructions {
		fmt.Fprintf(&b, "\nAdditional instructions from the user: %s\n", in)
	}
	if draft != "" {
		b.WriteString("\nThe current message, which you are rewriting (keep what is accurate and useful):\n")
		b.WriteString(draft)
		b.WriteString("\n")
	}
	if len(commits) > 0 {
		fmt.Fprintf(&b, "\nCommits on the PR (%d, oldest first):\n", len(commits))
		for i := len(commits) - 1; i >= 0; i-- {
			fmt.Fprintf(&b, "- %s\n", commits[i].Subject)
		}
	}
	if strings.TrimSpace(stat) != "" {
		b.WriteString("\nSummary of changes (diffstat):\n")
		b.WriteString(stat)
		b.WriteString("\n")
	}
	if strings.TrimSpace(diff) != "" {
		b.WriteString("\nDiff:\n")
		b.WriteString(diff)
		b.WriteString("\n")
	}
	return b.String()
}

// ---------------------------------------------------------------- HTTP

func (s *Server) agentOrFail(w http.ResponseWriter) bool {
	if s.agent == nil {
		fail(w, http.StatusNotFound, "editing is not available in this session")
		return false
	}
	return true
}

// handleAgentHarnesses backs the picker. It re-scans on every call so a harness
// installed since startup appears without a restart.
func (s *Server) handleAgentHarnesses(w http.ResponseWriter, r *http.Request) {
	if !s.agentOrFail(w) {
		return
	}
	writeJSON(w, map[string]any{
		"harnesses": s.agent.Detect(),
		"selected":  s.agent.Name(),
		"model":     s.agent.Model(),
		"pinned":    s.agent.Pinned(),
		"settings":  settings.Path(),
	})
}

func (s *Server) handleAgentSelect(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	if !s.agentOrFail(w) {
		return
	}
	name := r.URL.Query().Get("name")
	model := r.URL.Query().Get("model")
	if err := s.agent.Select(name, model); err != nil {
		code := 400
		if errors.Is(err, errAgentBusy) {
			code = http.StatusConflict
		}
		fail(w, code, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"harnesses": s.agent.Detect(),
		"selected":  s.agent.Name(),
		"model":     s.agent.Model(),
		"pinned":    s.agent.Pinned(),
		"settings":  settings.Path(),
	})
}

func (s *Server) handleAgentEdit(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	if !s.agentOrFail(w) {
		return
	}
	q := r.URL.Query()
	if q.Get("path") == "" {
		s.handleAgentBatchEdit(w, r)
		return
	}
	abs, rel, ok := s.resolvePath(q.Get("path"))
	if !ok {
		fail(w, 400, "bad path")
		return
	}
	l1, _ := strconv.Atoi(q.Get("l1"))
	l2, _ := strconv.Atoi(q.Get("l2"))

	job, err := s.threads.StartEdit([]agentBatchItem{{Abs: abs, Path: rel, L1: l1, L2: l2, Instruction: q.Get("instruction")}})
	if err != nil {
		code := 400
		if errors.Is(err, errAgentBusy) || errors.Is(err, errAgentDirty) {
			code = http.StatusConflict
		}
		fail(w, code, err.Error())
		return
	}
	writeJSON(w, job)
}

func (s *Server) handleAgentBatchEdit(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	if !s.agentOrFail(w) {
		return
	}

	bodyBytes, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		fail(w, 400, "failed reading body")
		return
	}

	var req struct {
		Edits []struct {
			Path        string `json:"path"`
			L1          int    `json:"l1"`
			L2          int    `json:"l2"`
			Instruction string `json:"instruction"`
		} `json:"edits"`
		Force bool `json:"force"`
	}

	if err := json.Unmarshal(bodyBytes, &req); err != nil || len(req.Edits) == 0 {
		var list []struct {
			Path        string `json:"path"`
			L1          int    `json:"l1"`
			L2          int    `json:"l2"`
			Instruction string `json:"instruction"`
		}
		var single struct {
			Path        string `json:"path"`
			L1          int    `json:"l1"`
			L2          int    `json:"l2"`
			Instruction string `json:"instruction"`
			Force       bool   `json:"force"`
		}
		q := r.URL.Query()
		if err2 := json.Unmarshal(bodyBytes, &list); err2 == nil && len(list) > 0 {
			req.Edits = list
		} else if err3 := json.Unmarshal(bodyBytes, &single); err3 == nil && single.Path != "" {
			req.Edits = []struct {
				Path        string `json:"path"`
				L1          int    `json:"l1"`
				L2          int    `json:"l2"`
				Instruction string `json:"instruction"`
			}{{Path: single.Path, L1: single.L1, L2: single.L2, Instruction: single.Instruction}}
			if single.Force {
				req.Force = true
			}
		} else if qEdits := q.Get("edits"); qEdits != "" {
			if err4 := json.Unmarshal([]byte(qEdits), &req.Edits); err4 != nil || len(req.Edits) == 0 {
				if err5 := json.Unmarshal([]byte(qEdits), &list); err5 == nil && len(list) > 0 {
					req.Edits = list
				}
			}
		} else if q.Get("path") != "" {
			l1, _ := strconv.Atoi(q.Get("l1"))
			l2, _ := strconv.Atoi(q.Get("l2"))
			req.Edits = []struct {
				Path        string `json:"path"`
				L1          int    `json:"l1"`
				L2          int    `json:"l2"`
				Instruction string `json:"instruction"`
			}{{Path: q.Get("path"), L1: l1, L2: l2, Instruction: q.Get("instruction")}}
			if q.Get("force") == "1" {
				req.Force = true
			}
		}

		if len(req.Edits) == 0 {
			fail(w, 400, "invalid or empty batch edits payload")
			return
		}
	}

	items := make([]agentBatchItem, len(req.Edits))
	for i, e := range req.Edits {
		abs, rel, ok := s.resolvePath(e.Path)
		if !ok {
			fail(w, 400, fmt.Sprintf("bad path: %s", e.Path))
			return
		}
		items[i] = agentBatchItem{
			Abs:         abs,
			Path:        rel,
			L1:          e.L1,
			L2:          e.L2,
			Instruction: e.Instruction,
		}
	}

	job, err := s.threads.StartEdit(items)
	if err != nil {
		code := 400
		if errors.Is(err, errAgentBusy) || errors.Is(err, errAgentDirty) {
			code = http.StatusConflict
		}
		fail(w, code, err.Error())
		return
	}
	writeJSON(w, job)
}

// handleAgentJob is polled while an edit runs, once per in-flight compose box.
// px0 dispatched the harness, so it knows when the work ended without
// watching the filesystem for it. id=0 (or missing) means the most recently
// started job.
func (s *Server) handleAgentJob(w http.ResponseWriter, r *http.Request) {
	if !s.agentOrFail(w) {
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	j := s.agent.Job(id)
	if j == nil {
		writeJSON(w, map[string]any{"idle": true})
		return
	}
	writeJSON(w, j)
}

func (s *Server) handleAgentCancel(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) {
		return
	}
	if !s.agentOrFail(w) {
		return
	}
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	if id == 0 {
		var body struct {
			ID int64 `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil && body.ID != 0 {
			id = body.ID
		}
	}
	writeJSON(w, map[string]any{"cancelled": s.agent.CancelJob(id)})
}

// Threads are long-running, multi-turn conversations with a coding harness,
// anchored to a range of code but free to read and change anything in the
// workspace. Where an inline edit is a single dispatch that is forgotten once
// it exits, a thread keeps its transcript on disk and continues the same
// harness session on every new message.
//
// Continuity works two ways. A harness with a session id px0 can control
// (claude, cursor-agent) resumes its own session, so the harness keeps its own
// context. Every other harness, and any harness switched to mid-thread, is
// replayed the earlier transcript inside the prompt.
//
// There is no overlap guard between threads or against inline edits: two
// harnesses writing the same lines is last write wins.

const (
	threadTurnTimeout = 30 * time.Minute
	threadSnippetMax  = 8000
	threadReplayMax   = 16000
	threadToolsMax    = 200
	threadSaveEvery   = 2 * time.Second
)

var (
	errThreadBusy = errors.New("this thread is still working on a reply")
	errThreadGone = errors.New("no such thread")
)

// threadTurn is one user message and the harness's answer to it.
type threadTurn struct {
	ID        int      `json:"id"`
	Prompt    string   `json:"prompt"`
	Reply     string   `json:"reply"`
	Tools     []string `json:"tools,omitempty"`     // what the harness did, one line each
	Changed   []string `json:"changed,omitempty"`   // files this turn touched
	BeforeSHA string   `json:"beforeSha,omitempty"` // ref-free git snapshot commit before the turn ran
	AfterSHA  string   `json:"afterSha,omitempty"`  // ref-free git snapshot commit after the turn ran
	Reverted  bool     `json:"reverted,omitempty"`  // true if the user reverted this turn's file changes
	Tracked   bool     `json:"tracked"`             // false outside git: Changed is unknown, not empty
	Error     string   `json:"error,omitempty"`
	Running   bool     `json:"running"`
	Harness   string   `json:"harness"`
	Model     string   `json:"model,omitempty"`
	Started   int64    `json:"started"` // unix ms
	Ms        int64    `json:"ms"`
}

// thread is the persisted conversation. Path and lines are where it was
// started, which is context for the harness and a jump target for the reader,
// not a limit on what it may change.
type thread struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Kind    string `json:"kind,omitempty"` // "edit" or "batch" when started from inline edits; empty for a conversation
	Path    string `json:"path,omitempty"`
	L1      int    `json:"l1,omitempty"`
	L2      int    `json:"l2,omitempty"`
	Snippet string `json:"snippet,omitempty"`
	// Scope is what a PR-review thread is about: "pr" (the whole pull request),
	// "mine" (the reviewer's own changes on top of it) or "selection" (just the
	// anchored code). ScopeSent is the scope the harness was last told, so a
	// change of scope is announced once rather than every turn.
	Scope          string        `json:"scope,omitempty"`
	ScopeSent      string        `json:"scopeSent,omitempty"`
	Created        int64         `json:"created"`
	Updated        int64         `json:"updated"`
	SessionID      string        `json:"sessionId,omitempty"`
	SessionHarness string        `json:"sessionHarness,omitempty"` // harness that owns SessionID
	SessionLive    bool          `json:"sessionLive,omitempty"`    // that harness has really started the session
	Turns          []*threadTurn `json:"turns"`
}

type threadSummary struct {
	ID      string `json:"id"`
	Kind    string `json:"kind,omitempty"`
	Title   string `json:"title"`
	Path    string `json:"path,omitempty"`
	L1      int    `json:"l1,omitempty"`
	L2      int    `json:"l2,omitempty"`
	Updated int64  `json:"updated"`
	Turns   int    `json:"turns"`
	Running bool   `json:"running"`
	Failed  bool   `json:"failed,omitempty"`
	// Touched: the last finished turn changed files, or could not say (no git),
	// so open tabs need a reload.
	Touched bool `json:"touched,omitempty"`
}

type threadStore struct {
	Root    string    `json:"root"`
	Threads []*thread `json:"threads"`
}

type threadEvent struct {
	name string
	data []byte
}

type threadManager struct {
	agent *agentManager
	root  string
	file  string // empty when there is nowhere to persist

	mu      sync.Mutex
	threads map[string]*thread
	cancels map[string]context.CancelFunc // one running turn per thread
	// prContext, when set (PR review sessions), describes the pull request the
	// workspace is a checkout of; it goes into the first prompt of a thread.
	prContext func(scope string) string
	jobs      map[int64]*threadJobRef // inline/batch edits, polled as agent jobs
	subs      map[string]map[chan threadEvent]struct{}
	lastSave  time.Time
}

// threadStorePath keeps every workspace's threads beside settings.json, never
// inside the working tree.
func threadStorePath(root string) string {
	sp := settings.Path()
	if sp == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(root))
	return filepath.Join(filepath.Dir(sp), "threads", hex.EncodeToString(sum[:8])+".json")
}

func newThreadManager(a *agentManager, root string) *threadManager {
	tm := &threadManager{
		agent:   a,
		root:    root,
		file:    threadStorePath(root),
		threads: map[string]*thread{},
		cancels: map[string]context.CancelFunc{},
		jobs:    map[int64]*threadJobRef{},
		subs:    map[string]map[chan threadEvent]struct{}{},
	}
	tm.load()
	a.threadJob = tm.job
	a.threadCancel = tm.cancelJob
	return tm
}

// load restores saved threads. A turn that was running when px0 exited can
// never finish, so it is closed out as interrupted rather than left spinning.
func (tm *threadManager) load() {
	if tm.file == "" {
		return
	}
	data, err := os.ReadFile(tm.file)
	if err != nil {
		return
	}
	var st threadStore
	if json.Unmarshal(data, &st) != nil {
		return
	}
	for _, t := range st.Threads {
		if t == nil || t.ID == "" {
			continue
		}
		for _, tr := range t.Turns {
			if tr.Running {
				tr.Running = false
				if tr.Error == "" {
					tr.Error = "interrupted: px0 exited before this reply finished"
				}
			}
		}
		tm.threads[t.ID] = t
	}
}

// saveLocked writes the store atomically. Callers hold tm.mu.
func (tm *threadManager) saveLocked() {
	if tm.file == "" {
		return
	}
	st := threadStore{Root: tm.root}
	for _, t := range tm.threads {
		st.Threads = append(st.Threads, t)
	}
	sort.Slice(st.Threads, func(i, j int) bool { return st.Threads[i].Created < st.Threads[j].Created })
	data, err := json.MarshalIndent(st, "", " ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(tm.file), 0o755); err != nil {
		return
	}
	tmp := tm.file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, tm.file); err != nil {
		os.Remove(tmp)
		return
	}
	tm.lastSave = time.Now()
}

func newThreadID() string {
	var b [5]byte
	rand.Read(b[:])
	return "t" + hex.EncodeToString(b[:])
}

// newUUID returns a random version 4 UUID, the form claude's --session-id wants.
func newUUID() string {
	var b [16]byte
	rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func threadTitle(msg string) string {
	line := strings.TrimSpace(strings.SplitN(strings.TrimSpace(msg), "\n", 2)[0])
	r := []rune(line)
	if len(r) > 60 {
		line = string(r[:60]) + "…"
	}
	if line == "" {
		line = "Untitled thread"
	}
	return line
}

func (t *thread) running() bool {
	return len(t.Turns) > 0 && t.Turns[len(t.Turns)-1].Running
}

func (t *thread) summary() threadSummary {
	s := threadSummary{ID: t.ID, Kind: t.Kind, Title: t.Title, Path: t.Path, L1: t.L1, L2: t.L2, Updated: t.Updated, Turns: len(t.Turns), Running: t.running()}
	if n := len(t.Turns); n > 0 && !s.Running {
		last := t.Turns[n-1]
		s.Failed = last.Error != ""
		s.Touched = len(last.Changed) > 0 || !last.Tracked
	}
	return s
}

// List returns every thread, most recently active first.
func (tm *threadManager) List() []threadSummary {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	out := make([]threadSummary, 0, len(tm.threads))
	for _, t := range tm.threads {
		out = append(out, t.summary())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out
}

func (tm *threadManager) Get(id string) *thread {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	t := tm.threads[id]
	if t == nil {
		return nil
	}
	return cloneThread(t)
}

func cloneThread(t *thread) *thread {
	c := *t
	c.Turns = make([]*threadTurn, len(t.Turns))
	for i, tr := range t.Turns {
		tc := *tr
		tc.Tools = append([]string(nil), tr.Tools...)
		tc.Changed = append([]string(nil), tr.Changed...)
		c.Turns[i] = &tc
	}
	return &c
}

// Create opens a thread anchored at path:l1-l2 (path may be empty for a thread
// about the workspace as a whole) and starts its first turn.
func (tm *threadManager) Create(abs, rel string, l1, l2 int, message, scope string) (*thread, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return nil, errors.New("message is empty")
	}
	t := &thread{
		ID:      newThreadID(),
		Title:   threadTitle(message),
		Path:    rel,
		Scope:   cleanThreadScope(scope),
		Created: time.Now().UnixMilli(),
	}
	t.Updated = t.Created
	if rel != "" {
		if l1 < 1 {
			l1 = 1
		}
		if l2 < l1 {
			l2 = l1
		}
		t.L1, t.L2 = l1, l2
		t.Snippet = anchorSnippet(abs, l1, l2)
	}
	tm.mu.Lock()
	tm.threads[t.ID] = t
	tm.mu.Unlock()
	if _, err := tm.Send(t.ID, message); err != nil {
		tm.mu.Lock()
		delete(tm.threads, t.ID)
		tm.mu.Unlock()
		return nil, err
	}
	return tm.Get(t.ID), nil
}

func cleanThreadScope(s string) string {
	switch s {
	case "pr", "mine", "selection":
		return s
	}
	return ""
}

// SetScope changes what a thread is about; the harness hears of it with the
// next message.
func (tm *threadManager) SetScope(id, scope string) error {
	scope = cleanThreadScope(scope)
	tm.mu.Lock()
	defer tm.mu.Unlock()
	t := tm.threads[id]
	if t == nil {
		return errThreadGone
	}
	t.Scope = scope
	tm.saveLocked()
	tm.publishThreadLocked(t)
	return nil
}

// threadNative reports whether px0 can hand this harness its own session.
func threadNative(name string) bool {
	return name == "claude" || name == "cursor-agent" || name == "agy" || name == "gemini"
}

// threadArgv adds the session and output flags a thread turn needs to the
// harness's headless argv. Flag order does not matter to these CLIs, so the
// extras go right after the binary. Every other harness runs unchanged and is
// replayed the transcript instead.
func threadArgv(name string, template []string, sessionID string, resume bool) []string {
	var extra []string
	switch name {
	case "claude":
		extra = []string{"--output-format", "stream-json", "--verbose"}
		if resume {
			extra = append(extra, "--resume", sessionID)
		} else {
			extra = append(extra, "--session-id", sessionID)
		}
	case "cursor-agent":
		if sessionID != "" {
			extra = []string{"--resume", sessionID}
		}
	case "agy":
		extra = []string{"--output-format", "stream-json"}
		if resume && sessionID != "" {
			extra = append(extra, "--conversation", sessionID)
		}
	case "gemini":
		extra = []string{"--output-format", "stream-json"}
		if resume && sessionID != "" {
			extra = append(extra, "--resume", sessionID)
		} else if sessionID != "" {
			extra = append(extra, "--session-id", sessionID)
		}
	}
	out := make([]string, 0, len(template)+len(extra))
	out = append(out, template[0])
	out = append(out, extra...)
	out = append(out, template[1:]...)
	return out
}

// threadPrompt composes what the harness is told for one turn. The anchor and
// ground rules go in only when the harness has no memory of them: the first
// turn, or a replay after a harness switch.
func threadPrompt(t *thread, prior []*threadTurn, message string, replay bool, prCtx string, scopeChanged bool) string {
	var b strings.Builder
	if len(prior) == 0 || replay {
		if t.Kind == "edit" || t.Kind == "batch" {
			b.WriteString("Carry out the edit request below by editing files in place. ")
			b.WriteString("Change only what it asks for; you may touch other files if the change needs it. ")
			b.WriteString("When done, reply in a sentence or two saying what you changed and in which files. ")
			b.WriteString("The user may follow up in this conversation.\n\n")
		} else {
			b.WriteString("You are helping with a long-running conversation about the code in this workspace. ")
			b.WriteString("You may read any file and edit any file needed to carry out what is asked; make edits directly. ")
			b.WriteString("When you finish a request, reply with a concise summary: what you found or changed, and in which files.\n\n")
		}
		if prCtx != "" {
			b.WriteString(prCtx)
			b.WriteString("\n\n")
		}
		if t.Path != "" && t.Kind != "batch" { // a batch carries its own snippets
			ext := strings.TrimPrefix(filepath.Ext(t.Path), ".")
			lineStr := fmt.Sprintf("lines %d-%d", t.L1, t.L2)
			if t.L1 == t.L2 {
				lineStr = fmt.Sprintf("line %d", t.L1)
			}
			fmt.Fprintf(&b, "The conversation started at @%s %s (the file may have changed since):\n```%s\n%s\n```\n\n", t.Path, lineStr, ext, t.Snippet)
		}
	}
	if replay && len(prior) > 0 {
		var h strings.Builder
		for _, tr := range prior {
			fmt.Fprintf(&h, "User: %s\n\nAssistant: %s\n\n", tr.Prompt, strings.TrimSpace(tr.Reply))
			if len(tr.Changed) > 0 {
				fmt.Fprintf(&h, "(files changed in that turn: %s)\n\n", strings.Join(tr.Changed, ", "))
			}
		}
		hist := h.String()
		if len(hist) > threadReplayMax {
			hist = "…" + hist[len(hist)-threadReplayMax:]
		}
		b.WriteString("### Earlier in this conversation\n")
		b.WriteString(hist)
		b.WriteString("### Current request\n")
	}
	if scopeChanged && prCtx != "" && len(prior) > 0 && !replay {
		b.WriteString("The user has changed what this conversation is about. ")
		b.WriteString(prCtx)
		b.WriteString("\n\n")
	}
	if t.Kind == "edit" && len(prior) == 0 {
		b.WriteString("### Instruction\n")
	}
	b.WriteString(message)
	return b.String()
}

// threadRun is everything a turn's goroutine needs, decided under the lock.
type threadRun struct {
	tid     string
	turn    *threadTurn
	name    string
	model   string
	base    []string // the selected harness's headless argv
	message string
	prior   []*threadTurn
}

// Send appends a message to the thread and starts the harness on it.
func (tm *threadManager) Send(id, message string) (*thread, error) {
	message = strings.TrimSpace(message)
	if message == "" {
		return nil, errors.New("message is empty")
	}
	name, base, model := tm.agent.current()
	if base == nil {
		ui.Status("err", "thread", "dispatch refused: no coding harness selected", 0, os.Stdout)
		return nil, errAgentNone
	}

	tm.mu.Lock()
	t := tm.threads[id]
	if t == nil {
		tm.mu.Unlock()
		return nil, errThreadGone
	}
	if t.running() {
		tm.mu.Unlock()
		return nil, errThreadBusy
	}
	// A session id belongs to the harness that made it. Switching harness
	// starts a fresh session there, primed by replaying the transcript.
	if t.SessionHarness != name {
		t.SessionID, t.SessionLive, t.SessionHarness = "", false, name
	}
	prior := make([]*threadTurn, 0, len(t.Turns))
	for _, tr := range t.Turns {
		c := *tr
		prior = append(prior, &c)
	}
	turn := &threadTurn{
		ID: len(t.Turns) + 1, Prompt: message, Running: true, Tracked: gitAvailable(tm.root),
		Harness: name, Model: model, Started: time.Now().UnixMilli(),
	}
	t.Turns = append(t.Turns, turn)
	t.Updated = turn.Started
	ctx, cancel := context.WithTimeout(context.Background(), threadTurnTimeout)
	tm.cancels[id] = cancel
	tm.saveLocked()
	tm.publishThreadLocked(t)
	tm.mu.Unlock()

	modelNote := ""
	if model != "" {
		modelNote = " (" + model + ")"
	}
	ui.Status("step", "thread", fmt.Sprintf("%s turn %d · %s%s · %s", id, turn.ID, name, modelNote, t.Title), 0, os.Stdout)

	go tm.runTurn(ctx, cancel, &threadRun{tid: id, turn: turn, name: name, model: model, base: base, message: message, prior: prior})
	return tm.Get(id), nil
}

// createCursorChat asks cursor-agent for a chat id it will accept on --resume.
func (tm *threadManager) createCursorChat(ctx context.Context, bin string) string {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, bin, "create-chat")
	cmd.Dir = tm.root
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	lines := strings.Fields(strings.TrimSpace(string(out)))
	if len(lines) == 0 {
		return ""
	}
	return lines[len(lines)-1]
}

func (tm *threadManager) runTurn(ctx context.Context, cancel context.CancelFunc, r *threadRun) {
	defer cancel()

	// Settle the session before composing the prompt: whether the harness
	// remembers the conversation decides whether it must be replayed.
	tm.mu.Lock()
	t := tm.threads[r.tid]
	if t == nil {
		tm.mu.Unlock()
		return
	}
	sessionID, live := t.SessionID, t.SessionLive
	tm.mu.Unlock()

	if sessionID == "" {
		switch r.name {
		case "claude", "gemini":
			sessionID = newUUID()
		case "cursor-agent":
			sessionID = tm.createCursorChat(ctx, r.base[0])
		}
	}
	native := threadNative(r.name) && (sessionID != "" || r.name == "agy")
	// Only a session the harness really started remembers the conversation.
	// Anything else, including a session whose first turn failed, is replayed.
	replay := len(r.prior) > 0 && !(native && live)

	tm.mu.Lock()
	if t := tm.threads[r.tid]; t != nil && native && sessionID != "" {
		t.SessionID = sessionID
	}
	tm.mu.Unlock()

	prCtx, scopeChanged := "", false
	if tm.prContext != nil {
		tm.mu.Lock()
		scope := t.Scope
		scopeChanged = scope != t.ScopeSent
		tm.mu.Unlock()
		prCtx = tm.prContext(scope)
		if prCtx != "" {
			tm.mu.Lock()
			t.ScopeSent = scope
			tm.saveLocked()
			tm.mu.Unlock()
		}
	}
	prompt := threadPrompt(t, r.prior, r.message, replay, prCtx, scopeChanged)
	argv := r.base
	if native {
		argv = threadArgv(r.name, r.base, sessionID, live)
	}

	if ui.Verbose {
		ui.VerbosePrompt(0, r.name, prompt, os.Stdout)
	}

	before := worktreeSnapshot(tm.root)
	beforeSHA := gitSnapshotWorktree(tm.root)
	started := time.Now()
	stderr := &tailBuffer{max: agentLogBytes}
	sink := &threadSink{tm: tm, tid: r.tid, turn: r.turn, harness: r.name}

	res := harness.Run(ctx, harness.Command{Name: r.name, Args: argv, Model: r.model}, prompt, harness.Options{
		Dir:      tm.root,
		Timeout:  threadTurnTimeout,
		LogBytes: agentLogBytes,
		Stdout:   sink,
		Stderr:   stderr,
	})
	sink.flush()
	err := res.Err
	if err == nil && sink.failure != "" {
		err = errors.New(sink.failure)
	}

	changed := changedSince(tm.root, before)
	afterSHA := ""
	if len(changed) > 0 && beforeSHA != "" {
		afterSHA = gitSnapshotWorktree(tm.root)
	}
	tm.agent.settle(changed)

	tm.mu.Lock()
	if t := tm.threads[r.tid]; t != nil {
		if t.SessionID == "" && sink.sessionID != "" {
			t.SessionID = sink.sessionID
		}
		if (sink.started && (r.name == "claude" || r.name == "agy" || r.name == "gemini")) || (r.name == "cursor-agent" && native && err == nil) {
			t.SessionLive = true
		}
		r.turn.Running = false
		r.turn.Ms = time.Since(started).Milliseconds()
		r.turn.Changed = changed
		r.turn.BeforeSHA = beforeSHA
		r.turn.AfterSHA = afterSHA
		if err != nil {
			msg := err.Error()
			if tail := strings.TrimSpace(stderr.String()); tail != "" && strings.TrimSpace(r.turn.Reply) == "" {
				if len(tail) > 2000 {
					tail = tail[len(tail)-2000:]
				}
				msg += "\n" + tail
			}
			r.turn.Error = msg
		}
		t.Updated = time.Now().UnixMilli()
		delete(tm.cancels, r.tid)
		tm.saveLocked()
		tm.publishThreadLocked(t)
	}
	tm.mu.Unlock()

	dur := ui.FmtDuration(time.Since(started))
	if err != nil {
		ui.Status("err", "thread", fmt.Sprintf("%s turn %d failed in %s: %s", r.tid, r.turn.ID, dur, err.Error()), 0, os.Stdout)
	} else {
		ui.Status("ok", "thread", fmt.Sprintf("%s turn %d · %s  (%d files changed)", r.tid, r.turn.ID, dur, len(changed)), 0, os.Stdout)
	}
}

// Cancel stops the turn running in a thread. What it already wrote stays.
func (tm *threadManager) Cancel(id string) bool {
	tm.mu.Lock()
	cancel := tm.cancels[id]
	tm.mu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// Delete removes a thread, stopping its turn first.
func (tm *threadManager) Delete(id string) bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.threads[id] == nil {
		return false
	}
	if cancel := tm.cancels[id]; cancel != nil {
		cancel()
	}
	delete(tm.threads, id)
	tm.saveLocked()
	tm.publishLocked(id, "deleted", map[string]string{"id": id})
	tm.publishLocked("", "deleted", map[string]string{"id": id})
	return true
}

func (tm *threadManager) Close() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	for _, cancel := range tm.cancels {
		cancel()
	}
}

// RevertTurn restores the files touched by a specific turn back to their
// pre-turn snapshot (BeforeSHA).
func (tm *threadManager) RevertTurn(id string, turnID int) (*thread, []string, error) {
	tm.mu.Lock()
	t := tm.threads[id]
	if t == nil {
		tm.mu.Unlock()
		return nil, nil, errThreadGone
	}
	if t.running() {
		tm.mu.Unlock()
		return nil, nil, errThreadBusy
	}
	var target *threadTurn
	for _, tr := range t.Turns {
		if tr.ID == turnID {
			target = tr
			break
		}
	}
	if target == nil {
		tm.mu.Unlock()
		return nil, nil, errors.New("no such turn")
	}
	if target.BeforeSHA == "" || len(target.Changed) == 0 {
		tm.mu.Unlock()
		return nil, nil, errors.New("no snapshot available for this turn")
	}
	beforeSHA := target.BeforeSHA
	paths := append([]string(nil), target.Changed...)
	tm.mu.Unlock()

	if err := gitRestoreSnapshotPaths(tm.root, beforeSHA, paths); err != nil {
		return nil, nil, err
	}
	tm.agent.settle(paths)

	tm.mu.Lock()
	if t := tm.threads[id]; t != nil {
		for _, tr := range t.Turns {
			if tr.ID == turnID {
				tr.Reverted = true
				break
			}
		}
		t.Updated = time.Now().UnixMilli()
		tm.saveLocked()
		tm.publishThreadLocked(t)
	}
	tm.mu.Unlock()
	return tm.Get(id), paths, nil
}

// ---------------------------------------------------------------- output

// threadSink turns a harness's stdout into the turn's reply as it arrives.
// claude, agy, and gemini speak stream-json, one event per line, from which the
// assistant's text deltas and tool calls are pulled and streamed live to the UI.
// Anything else is treated as the reply itself, line by line.
type threadSink struct {
	tm        *threadManager
	tid       string
	turn      *threadTurn
	harness   string
	buf       []byte
	started   bool   // harness reported a session id, so --resume will work next
	sessionID string // session id discovered from stream
	failure   string // harness's own error result
}

func (s *threadSink) Write(p []byte) (int, error) {
	s.buf = append(s.buf, p...)
	for {
		i := bytes.IndexByte(s.buf, '\n')
		if i < 0 {
			break
		}
		line := s.buf[:i]
		s.buf = s.buf[i+1:]
		s.line(string(bytes.TrimRight(line, "\r")))
	}
	return len(p), nil
}

func (s *threadSink) flush() {
	if len(bytes.TrimSpace(s.buf)) > 0 {
		s.line(string(s.buf))
	}
	s.buf = nil
}

func (s *threadSink) line(l string) {
	switch s.harness {
	case "claude":
		for _, ev := range parseClaudeEvent(l) {
			s.handleEvent(ev)
		}
	case "agy":
		for _, ev := range parseAgyEvent(l) {
			s.handleEvent(ev)
		}
	case "gemini":
		for _, ev := range parseGeminiEvent(l) {
			s.handleEvent(ev)
		}
	default:
		s.text(l+"\n", false)
	}
}

func (s *threadSink) handleEvent(ev claudeEvent) {
	switch ev.kind {
	case "session":
		s.started = true
		if ev.text != "" {
			s.sessionID = ev.text
		}
	case "text":
		s.text(ev.text, ev.block)
	case "tool":
		s.tool(ev.text)
	case "error":
		s.failure = ev.text
	case "result":
		if strings.TrimSpace(s.reply()) == "" {
			s.text(ev.text, true)
		}
	}
}

func (s *threadSink) reply() string {
	s.tm.mu.Lock()
	defer s.tm.mu.Unlock()
	return s.turn.Reply
}

// text appends to the reply. block marks a whole assistant message, which is
// set off from the previous one by a blank line.
func (s *threadSink) text(text string, block bool) {
	if text == "" {
		return
	}
	s.tm.mu.Lock()
	defer s.tm.mu.Unlock()
	if block && s.turn.Reply != "" && !strings.HasSuffix(s.turn.Reply, "\n\n") {
		text = "\n\n" + text
	}
	s.turn.Reply += text
	s.tm.publishLocked(s.tid, "delta", map[string]any{"turn": s.turn.ID, "text": text})
	s.tm.maybeSaveLocked()
}

func (s *threadSink) tool(text string) {
	s.tm.mu.Lock()
	defer s.tm.mu.Unlock()
	if len(s.turn.Tools) < threadToolsMax {
		s.turn.Tools = append(s.turn.Tools, text)
	}
	s.tm.publishLocked(s.tid, "tool", map[string]any{"turn": s.turn.ID, "text": text})
}

// maybeSaveLocked checkpoints a long reply so an exit mid-turn loses seconds,
// not the whole answer.
func (tm *threadManager) maybeSaveLocked() {
	if time.Since(tm.lastSave) >= threadSaveEvery {
		tm.saveLocked()
	}
}

type claudeEvent struct {
	kind  string // session | text | tool | result | error
	text  string
	block bool
}

// parseClaudeEvent reads one line of `claude --output-format stream-json`.
// Lines that are not events px0 cares about, or not JSON at all, yield nothing.
func parseClaudeEvent(line string) []claudeEvent {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return nil
	}
	var ev struct {
		Type      string `json:"type"`
		Subtype   string `json:"subtype"`
		SessionID string `json:"session_id"`
		IsError   bool   `json:"is_error"`
		Result    string `json:"result"`
		Message   struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil {
		return nil
	}
	var out []claudeEvent
	if ev.SessionID != "" {
		out = append(out, claudeEvent{kind: "session"})
	}
	switch ev.Type {
	case "assistant":
		for _, c := range ev.Message.Content {
			switch c.Type {
			case "text":
				if strings.TrimSpace(c.Text) != "" {
					out = append(out, claudeEvent{kind: "text", text: c.Text, block: true})
				}
			case "tool_use":
				out = append(out, claudeEvent{kind: "tool", text: toolLabel(c.Name, c.Input)})
			}
		}
	case "result":
		if ev.IsError {
			msg := strings.TrimSpace(ev.Result)
			if msg == "" {
				msg = "the harness reported an error"
			}
			out = append(out, claudeEvent{kind: "error", text: msg})
		} else if strings.TrimSpace(ev.Result) != "" {
			out = append(out, claudeEvent{kind: "result", text: ev.Result})
		}
	}
	return out
}

// parseAgyEvent reads one line of `agy --output-format stream-json`.
func parseAgyEvent(line string) []claudeEvent {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return nil
	}
	var ev struct {
		Event          string `json:"event"`
		ConversationID string `json:"conversation_id"`
		StepUpdate     *struct {
			StepType string `json:"step_type"`
			State    string `json:"state"`
			ToolName string `json:"tool_name"`
			ToolInfo *struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"tool_info"`
			TextDelta string `json:"text_delta"`
		} `json:"step_update"`
		Result *struct {
			Status   string `json:"status"`
			Response string `json:"response"`
			Error    string `json:"error"`
		} `json:"result"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil {
		return nil
	}
	var out []claudeEvent
	if ev.Event == "init" && ev.ConversationID != "" {
		out = append(out, claudeEvent{kind: "session", text: ev.ConversationID})
	}
	if ev.Event == "step_update" && ev.StepUpdate != nil {
		su := ev.StepUpdate
		switch su.StepType {
		case "tool":
			if su.State == "ACTIVE" || su.State == "" {
				name := su.ToolName
				var params json.RawMessage
				if su.ToolInfo != nil {
					if su.ToolInfo.Name != "" {
						name = su.ToolInfo.Name
					}
					params = su.ToolInfo.Parameters
				}
				out = append(out, claudeEvent{kind: "tool", text: toolLabel(name, params)})
			}
		case "agent_response":
			if su.TextDelta != "" {
				out = append(out, claudeEvent{kind: "text", text: su.TextDelta, block: false})
			}
		}
	}
	if ev.Event == "result" && ev.Result != nil {
		r := ev.Result
		if r.Status != "" && r.Status != "SUCCESS" {
			errText := r.Error
			if errText == "" {
				errText = "the harness reported an error"
			}
			out = append(out, claudeEvent{kind: "error", text: errText})
		} else if strings.TrimSpace(r.Response) != "" {
			out = append(out, claudeEvent{kind: "result", text: r.Response})
		}
	}
	return out
}

// parseGeminiEvent reads one line of `gemini --output-format stream-json`.
func parseGeminiEvent(line string) []claudeEvent {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "{") {
		return nil
	}
	var ev struct {
		Type       string          `json:"type"`
		SessionID  string          `json:"session_id"`
		Role       string          `json:"role"`
		Content    string          `json:"content"`
		Delta      bool            `json:"delta"`
		ToolName   string          `json:"tool_name"`
		Parameters json.RawMessage `json:"parameters"`
		Status     string          `json:"status"`
		Error      string          `json:"error"`
	}
	if json.Unmarshal([]byte(line), &ev) != nil {
		return nil
	}
	var out []claudeEvent
	if ev.Type == "init" && ev.SessionID != "" {
		out = append(out, claudeEvent{kind: "session", text: ev.SessionID})
	}
	if ev.Type == "tool_use" {
		out = append(out, claudeEvent{kind: "tool", text: toolLabel(ev.ToolName, ev.Parameters)})
	}
	if ev.Type == "message" && ev.Role == "assistant" && ev.Content != "" {
		out = append(out, claudeEvent{kind: "text", text: ev.Content, block: false})
	}
	if ev.Type == "result" {
		if ev.Status != "" && ev.Status != "success" {
			errText := ev.Error
			if errText == "" {
				errText = "gemini reported an error"
			}
			out = append(out, claudeEvent{kind: "error", text: errText})
		}
	}
	return out
}

// toolLabel is "Edit server.go" or "Bash go test ./...": the tool and the one
// argument that says what it acted on.
func toolLabel(name string, input json.RawMessage) string {
	var in map[string]any
	json.Unmarshal(input, &in)
	for _, k := range []string{"file_path", "path", "AbsolutePath", "TargetFile", "command", "CommandLine", "pattern", "query", "url", "description"} {
		if v, ok := in[k].(string); ok && v != "" {
			v = strings.Join(strings.Fields(v), " ")
			if r := []rune(v); len(r) > 120 {
				v = string(r[:120]) + "…"
			}
			return name + " " + v
		}
	}
	return name
}

// ---------------------------------------------------------------- events

// publishLocked delivers an event to the subscribers of one thread ("" is the
// list channel). A subscriber that cannot keep up is dropped, and its client
// reconnects to a fresh snapshot, so nothing is ever delivered out of order.
func (tm *threadManager) publishLocked(id, name string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	for ch := range tm.subs[id] {
		select {
		case ch <- threadEvent{name, data}:
		default:
			delete(tm.subs[id], ch)
			close(ch)
		}
	}
}

func (tm *threadManager) publishThreadLocked(t *thread) {
	tm.publishLocked(t.ID, "thread", t)
	tm.publishLocked("", "summary", t.summary())
}

// subscribe registers for events on id ("" for the list) and returns the
// current state taken under the same lock, so nothing falls between the two.
func (tm *threadManager) subscribe(id string) (chan threadEvent, threadEvent, func(), bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	var first threadEvent
	if id == "" {
		list := make([]threadSummary, 0, len(tm.threads))
		for _, t := range tm.threads {
			list = append(list, t.summary())
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Updated > list[j].Updated })
		data, _ := json.Marshal(list)
		first = threadEvent{"list", data}
	} else {
		t := tm.threads[id]
		if t == nil {
			return nil, first, nil, false
		}
		data, _ := json.Marshal(t)
		first = threadEvent{"thread", data}
	}
	ch := make(chan threadEvent, 256)
	if tm.subs[id] == nil {
		tm.subs[id] = map[chan threadEvent]struct{}{}
	}
	tm.subs[id][ch] = struct{}{}
	return ch, first, func() {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		if _, ok := tm.subs[id][ch]; ok {
			delete(tm.subs[id], ch)
			close(ch)
		}
	}, true
}

// ---------------------------------------------------------------- HTTP

func (s *Server) threadsOrFail(w http.ResponseWriter) bool {
	if s.threads == nil {
		fail(w, http.StatusNotFound, "threads are not available in this session")
		return false
	}
	return true
}

func threadStatus(err error) int {
	switch {
	case errors.Is(err, errThreadBusy):
		return http.StatusConflict
	case errors.Is(err, errThreadGone):
		return http.StatusNotFound
	}
	return http.StatusBadRequest
}

func (s *Server) handleThreads(w http.ResponseWriter, r *http.Request) {
	if !s.threadsOrFail(w) {
		return
	}
	writeJSON(w, map[string]any{"threads": s.threads.List()})
}

func (s *Server) handleThreadGet(w http.ResponseWriter, r *http.Request) {
	if !s.threadsOrFail(w) {
		return
	}
	t := s.threads.Get(r.URL.Query().Get("id"))
	if t == nil {
		fail(w, http.StatusNotFound, errThreadGone.Error())
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleThreadCreate(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) || !s.threadsOrFail(w) {
		return
	}
	var body struct {
		Path    string `json:"path"`
		L1      int    `json:"l1"`
		L2      int    `json:"l2"`
		Message string `json:"message"`
		Scope   string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	var abs, rel string
	if body.Path != "" {
		var ok bool
		if abs, rel, ok = s.resolvePath(body.Path); !ok {
			fail(w, 400, "bad path")
			return
		}
	}
	t, err := s.threads.Create(abs, rel, body.L1, body.L2, body.Message, body.Scope)
	if err != nil {
		fail(w, threadStatus(err), err.Error())
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleThreadSend(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) || !s.threadsOrFail(w) {
		return
	}
	var body struct {
		ID      string `json:"id"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	t, err := s.threads.Send(body.ID, body.Message)
	if err != nil {
		fail(w, threadStatus(err), err.Error())
		return
	}
	writeJSON(w, t)
}

func (s *Server) handleThreadCancel(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) || !s.threadsOrFail(w) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" && r.Body != nil {
		var body struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(r.Body).Decode(&body) == nil {
			id = body.ID
		}
	}
	writeJSON(w, map[string]any{"cancelled": s.threads.Cancel(id)})
}

func (s *Server) handleThreadScope(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) || !s.threadsOrFail(w) {
		return
	}
	var body struct {
		ID    string `json:"id"`
		Scope string `json:"scope"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, 400, "bad request")
		return
	}
	if err := s.threads.SetScope(body.ID, body.Scope); err != nil {
		fail(w, threadStatus(err), err.Error())
		return
	}
	writeJSON(w, s.threads.Get(body.ID))
}

func (s *Server) handleThreadDelete(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) || !s.threadsOrFail(w) {
		return
	}
	id := r.URL.Query().Get("id")
	if id == "" && r.Body != nil {
		var body struct {
			ID string `json:"id"`
		}
		if json.NewDecoder(r.Body).Decode(&body) == nil {
			id = body.ID
		}
	}
	if id == "" {
		fail(w, 400, "missing thread id")
		return
	}
	writeJSON(w, map[string]any{"deleted": s.threads.Delete(id)})
}

func (s *Server) handleThreadRevert(w http.ResponseWriter, r *http.Request) {
	if !localPost(w, r) || !s.threadsOrFail(w) {
		return
	}
	var body struct {
		ID   string `json:"id"`
		Turn int    `json:"turn"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" || body.Turn < 1 {
		fail(w, http.StatusBadRequest, "id and turn are required")
		return
	}
	t, paths, err := s.threads.RevertTurn(body.ID, body.Turn)
	if err != nil {
		fail(w, threadStatus(err), err.Error())
		return
	}
	for _, p := range paths {
		s.ix.TouchPath(p)
	}
	if s.gitWatcher != nil {
		s.gitWatcher.Trigger()
	}
	writeJSON(w, map[string]any{"ok": true, "thread": t, "reverted": paths})
}

// handleThreadStream is the SSE feed: one thread's snapshot and live deltas
// with ?id=, or the thread list's changes without it.
func (s *Server) handleThreadStream(w http.ResponseWriter, r *http.Request) {
	if !s.threadsOrFail(w) {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch, first, unsub, ok := s.threads.subscribe(r.URL.Query().Get("id"))
	if !ok {
		fail(w, http.StatusNotFound, errThreadGone.Error())
		return
	}
	defer unsub()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	write := func(ev threadEvent) bool {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}
	if !write(first) {
		return
	}
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.streamsDone:
			return
		case ev, open := <-ch:
			if !open || !write(ev) {
				return
			}
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---------------------------------------------------------------- inline edits

// threadJobRef ties an agent-style job id, which the inline and batch edit UI
// polls, to the thread turn doing the work.
type threadJobRef struct {
	tid   string
	turn  int
	items []agentBatchItem
}

// StartEdit runs inline and batch edits as a thread, so every edit leaves a
// transcript that can be followed up. It keeps the guarantee those flows have
// always had: an edit on lines another edit is still changing is refused.
// Conversations started from the Threads pane are never guarded this way.
func (tm *threadManager) StartEdit(items []agentBatchItem) (*agentJob, error) {
	if len(items) == 0 {
		return nil, errors.New("no edits specified")
	}
	for i := range items {
		items[i].Instruction = strings.TrimSpace(items[i].Instruction)
		if items[i].Instruction == "" {
			return nil, errors.New("instruction is empty")
		}
		if items[i].L1 < 1 {
			items[i].L1 = 1
		}
		if items[i].L2 < items[i].L1 {
			items[i].L2 = items[i].L1
		}
		for j := 0; j < i; j++ {
			if items[i].Path == items[j].Path && items[i].L1 <= items[j].L2 && items[j].L1 <= items[i].L2 {
				return nil, fmt.Errorf("overlapping edits in batch on %s (%s and %s)", items[i].Path, lineRef(items[i].L1, items[i].L2), lineRef(items[j].L1, items[j].L2))
			}
		}
	}

	if busy := tm.overlappingEdit(items); busy != "" {
		ui.Status("warn", "agent", "edit dispatch refused: "+busy, 0, os.Stdout)
		return nil, fmt.Errorf("%w: %s", errAgentBusy, busy)
	}

	t := &thread{ID: newThreadID(), Created: time.Now().UnixMilli()}
	t.Updated = t.Created
	var message string
	if len(items) == 1 {
		it := items[0]
		t.Kind, t.Path, t.L1, t.L2 = "edit", it.Path, it.L1, it.L2
		t.Snippet = anchorSnippet(it.Abs, it.L1, it.L2)
		t.Title = "Edit: " + threadTitle(it.Instruction)
		message = it.Instruction
	} else {
		t.Kind, t.Path, t.L1, t.L2 = "batch", items[0].Path, items[0].L1, items[0].L2
		t.Title = fmt.Sprintf("Batch edit: %d changes", len(items))
		var b strings.Builder
		fmt.Fprintf(&b, "Apply these %d edits together.\n\n", len(items))
		for i, it := range items {
			snip, err := readLineRange(it.Abs, it.L1, it.L2)
			if err != nil {
				return nil, err
			}
			if len(snip) > threadSnippetMax {
				snip = snip[:threadSnippetMax] + "\n…"
			}
			fmt.Fprintf(&b, "### Edit %d: @%s %s\n```%s\n%s\n```\n%s\n\n", i+1, it.Path, lineRef(it.L1, it.L2),
				strings.TrimPrefix(filepath.Ext(it.Path), "."), snip, it.Instruction)
		}
		message = strings.TrimSpace(b.String())
	}

	tm.mu.Lock()
	tm.threads[t.ID] = t
	tm.mu.Unlock()
	if _, err := tm.Send(t.ID, message); err != nil {
		tm.mu.Lock()
		delete(tm.threads, t.ID)
		tm.mu.Unlock()
		return nil, err
	}
	id := tm.agent.nextJobID()
	tm.mu.Lock()
	tm.jobs[id] = &threadJobRef{tid: t.ID, turn: 1, items: items}
	tm.mu.Unlock()
	return tm.job(id), nil
}

// overlappingEdit describes an edit still running on lines these items touch,
// or returns "" when there is none.
func (tm *threadManager) overlappingEdit(items []agentBatchItem) string {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	for id, ref := range tm.jobs {
		t := tm.threads[ref.tid]
		if t == nil || !t.running() {
			continue
		}
		for _, run := range ref.items {
			for _, it := range items {
				if run.Path == it.Path && it.L1 <= run.L2 && run.L1 <= it.L2 {
					return fmt.Sprintf("an edit is already running on %s:%s (job #%d with %s)", it.Path, lineRef(it.L1, it.L2), id, t.Turns[len(t.Turns)-1].Harness)
				}
			}
		}
	}
	return ""
}

// job presents an inline or batch edit's turn in the shape the edit UI polls.
// id 0 means the most recent one.
func (tm *threadManager) job(id int64) *agentJob {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if id == 0 {
		for cand := range tm.jobs {
			if cand > id {
				id = cand
			}
		}
	}
	ref := tm.jobs[id]
	if ref == nil {
		return nil
	}
	t := tm.threads[ref.tid]
	if t == nil || ref.turn > len(t.Turns) {
		return nil
	}
	turn := t.Turns[ref.turn-1]
	j := &agentJob{
		ID: id, ThreadID: t.ID, Harness: turn.Harness, Path: ref.items[0].Path, Lines: lineRef(ref.items[0].L1, ref.items[0].L2),
		Running: turn.Running, Log: turn.Reply, Stdout: turn.Reply,
		Changed: append([]string{}, turn.Changed...), Ms: turn.Ms, Tracked: turn.Tracked,
	}
	if len(ref.items) > 1 {
		j.BatchCount, j.Items = len(ref.items), ref.items
	}
	if turn.Running {
		j.Ms = time.Now().UnixMilli() - turn.Started
	}
	if turn.Error != "" {
		first, rest, _ := strings.Cut(turn.Error, "\n")
		j.Error, j.Stderr = first, rest
	}
	return j
}

// cancelJob stops the edit with this job id, or every running edit for id 0.
func (tm *threadManager) cancelJob(id int64) bool {
	tm.mu.Lock()
	var tids []string
	for jid, ref := range tm.jobs {
		if id == 0 || jid == id {
			tids = append(tids, ref.tid)
		}
	}
	tm.mu.Unlock()
	cancelled := false
	for _, tid := range tids {
		if tm.Cancel(tid) {
			cancelled = true
		}
	}
	return cancelled
}

func anchorSnippet(abs string, l1, l2 int) string {
	snip, err := readLineRange(abs, l1, l2)
	if err != nil {
		return ""
	}
	if len(snip) > threadSnippetMax {
		snip = snip[:threadSnippetMax] + "\n…"
	}
	return snip
}
