package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/px0-ai/harness"
)

// writeHarness creates an executable stand-in for a coding harness, outside the
// workspace so that it does not show up as a change the run made.
func writeHarness(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "harness.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// isolateSettings points the settings file at a temp dir, so a test never reads
// or overwrites the choice the developer running it has made.
func isolateSettings(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

// agentPost speaks the way the browser does: POST, with an Origin that matches
// a Host localPost will accept.
func agentPost(t *testing.T, s *Server, url string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, url, nil)
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Origin", "http://127.0.0.1:7777")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func agentPostJSON(t *testing.T, s *Server, url string, body any) (int, map[string]any) {
	t.Helper()
	data, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	req.Host = "127.0.0.1:7777"
	req.Header.Set("Origin", "http://127.0.0.1:7777")
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func agentServer(t *testing.T, root, harness string) *Server {
	t.Helper()
	isolateSettings(t)
	m, err := newAgentManager(root, harness+" {prompt}", nil)
	if err != nil {
		t.Fatal(err)
	}
	ix := NewIndex(root)
	ix.Build()
	s := NewServer(ix, nil)
	s.SetAgent(m)
	t.Cleanup(func() {
		if s.gitWatcher != nil {
			s.gitWatcher.Stop()
		}
	})
	return s
}

// waitIdle waits for the most recently started job to finish. Tests that keep
// several jobs in flight at once poll a specific id instead.
func waitIdle(t *testing.T, s *Server) *agentJob {
	t.Helper()
	return waitIdleID(t, s, 0)
}

func waitIdleID(t *testing.T, s *Server, id int64) *agentJob {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if j := s.agent.Job(id); j != nil && !j.Running {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("harness did not finish")
	return nil
}

func TestAgentSpecResolution(t *testing.T) {
	isolateSettings(t)
	root := t.TempDir()

	if _, err := newAgentManager(root, "echo hello", nil); err == nil {
		t.Fatal("a template without {prompt} should be refused")
	}
	if _, err := newAgentManager(root, "px0-not-a-real-binary {prompt}", nil); err == nil {
		t.Fatal("a missing binary should be refused at startup, not on first use")
	}

	m, err := newAgentManager(root, "echo {prompt}", nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name() != "echo" {
		t.Fatalf("name = %q, want echo", m.Name())
	}
	if !m.Pinned() {
		t.Fatal("-agent should pin the harness")
	}
	if err := m.Select("echo {prompt}"); err == nil {
		t.Fatal("a pinned harness must not be changeable from the UI")
	}

	// No flag and no saved choice: editing is available, nothing is selected.
	idle, err := newAgentManager(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if idle.Name() != "" || idle.Pinned() {
		t.Fatalf("fresh manager = %q pinned=%v, want unselected and unpinned", idle.Name(), idle.Pinned())
	}
}

func TestAgentDetectListsKnownHarnesses(t *testing.T) {
	isolateSettings(t)
	m, err := newAgentManager(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := m.Detect()
	presets := harness.Presets()
	if len(got) != len(presets) {
		t.Fatalf("detected %d rows, want one per preset (%d)", len(got), len(presets))
	}
	for i, h := range got {
		if h.Name != presets[i].Name {
			t.Fatalf("row %d = %q, want %q", i, h.Name, presets[i].Name)
		}
		if !strings.Contains(h.Cmd, "{prompt}") {
			t.Fatalf("%s cmd should show the template, got %q", h.Name, h.Cmd)
		}
		if h.Installed && h.Path == "" {
			t.Fatalf("%s reported installed with no path", h.Name)
		}
	}
}

func TestAgentDetectLazyModels(t *testing.T) {
	isolateSettings(t)
	m, err := newAgentManager(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	got := m.Detect()
	for _, h := range got {
		if len(h.Models) > 0 {
			t.Fatalf("harness %q had models eagerly discovered when unselected", h.Name)
		}
	}
}

func TestAgentSelectPersistsOutsideWorkspace(t *testing.T) {
	cfg := isolateSettings(t)
	root := t.TempDir()

	m, err := newAgentManager(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Select("px0-not-a-real-binary"); err == nil {
		t.Fatal("selecting something that is not installed should fail")
	}

	// echo stands in for a harness binary that exists on every machine.
	if err := m.Select("echo {prompt}"); err != nil {
		t.Fatal(err)
	}
	if m.Name() != "echo" {
		t.Fatalf("selected = %q, want echo", m.Name())
	}

	if _, err := os.Stat(filepath.Join(cfg, "px0", "settings.json")); err != nil {
		t.Fatalf("settings file not written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "settings.json")); !os.IsNotExist(err) {
		t.Fatal("settings must never be written into the workspace")
	}

	// A later run restores the choice, template and all.
	restored, err := newAgentManager(root, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Name() != "echo" {
		t.Fatalf("restored = %q, want echo", restored.Name())
	}

	if err := restored.Select(""); err != nil {
		t.Fatal(err)
	}
	if restored.Name() != "" {
		t.Fatalf("cleared = %q, want empty", restored.Name())
	}
	if again, _ := newAgentManager(root, "", nil); again.Name() != "" {
		t.Fatalf("clearing did not persist, got %q", again.Name())
	}
}

func TestAgentHarnessEndpointsRequireAvailability(t *testing.T) {
	s, _ := newTestServer(t) // no SetAgent: editing unavailable

	if code, _ := get(t, s, "/api/agent/job"); code != http.StatusNotFound {
		t.Fatalf("job = %d, want 404", code)
	}
	if code, _ := get(t, s, "/api/agent/harnesses"); code != http.StatusNotFound {
		t.Fatalf("harnesses = %d, want 404", code)
	}
	if code, _ := agentPost(t, s, "/api/agent/edit?path=main.go&l1=1&l2=1&instruction=hi"); code != http.StatusNotFound {
		t.Fatalf("edit = %d, want 404", code)
	}

	_, meta := get(t, s, "/api/meta")
	if meta["agent"] != "" {
		t.Fatalf("meta agent = %v, want empty", meta["agent"])
	}
	if got, ok := meta["agents"].([]any); !ok || len(got) != 0 {
		t.Fatalf("meta agents = %v, want an empty list", meta["agents"])
	}
}

func TestAgentEditRefusedUntilHarnessChosen(t *testing.T) {
	isolateSettings(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := newAgentManager(root, "", nil) // available, nothing selected
	if err != nil {
		t.Fatal(err)
	}
	ix := NewIndex(root)
	ix.Build()
	s := NewServer(ix, nil)
	s.SetAgent(m)

	code, body := agentPost(t, s, "/api/agent/edit?path=a.go&l1=1&l2=1&instruction=hi")
	if code != 400 {
		t.Fatalf("edit with nothing selected = %d, want 400", code)
	}
	if !strings.Contains(body["error"].(string), "no coding harness") {
		t.Fatalf("error = %q", body["error"])
	}

	// Picking one over HTTP is enough to make editing work.
	if code, _ = agentPost(t, s, "/api/agent/select?name="+"echo+%7Bprompt%7D"); code != 200 {
		t.Fatalf("select = %d, want 200", code)
	}
	if code, _ = agentPost(t, s, "/api/agent/edit?path=a.go&l1=1&l2=1&instruction=hi"); code != 200 {
		t.Fatalf("edit after select = %d, want 200", code)
	}
	waitIdle(t, s)
}

func TestAgentEditRunsHarnessAndReportsChange(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)
	out := t.TempDir()
	// keep.go is committed and clean, so no force is needed.
	s := agentServer(t, root, writeHarness(t,
		"printf 'touched\\n' >> keep.go\nprintf '%s' \"$1\" > "+filepath.Join(out, "prompt.txt")+"\n"))

	code, _ := agentPost(t, s, "/api/agent/edit?path=keep.go&l1=1&l2=1&instruction=add+a+line")
	if code != 200 {
		t.Fatalf("edit = %d, want 200", code)
	}

	job := waitIdle(t, s)
	if job.Error != "" {
		t.Fatalf("harness failed: %s (log: %s)", job.Error, job.Log)
	}

	body, err := os.ReadFile(filepath.Join(root, "keep.go"))
	if err != nil || !strings.Contains(string(body), "touched") {
		t.Fatalf("harness did not edit the file: %q %v", body, err)
	}
	if len(job.Changed) != 1 || job.Changed[0] != "keep.go" {
		t.Fatalf("changed = %v, want [keep.go]", job.Changed)
	}
	if !job.Tracked {
		t.Fatal("tracked should be true inside a repository")
	}

	// The prompt must carry the anchor and the instruction the user wrote.
	prompt, err := os.ReadFile(filepath.Join(out, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"@keep.go line 1", "### Instruction", "add a line"} {
		if !strings.Contains(string(prompt), want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
}

// Outside a repository px0 cannot name what a harness touched. The job must say
// so, because an empty change list would otherwise read as "nothing happened"
// and the client would skip the reload after a real edit.
func TestAgentOutsideGitReportsUnknownChanges(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := agentServer(t, root, writeHarness(t, "printf 'touched\\n' >> a.go\n"))

	// With no git there is also no uncommitted-work guard to satisfy.
	if code, _ := agentPost(t, s, "/api/agent/edit?path=a.go&l1=1&l2=1&instruction=hi"); code != 200 {
		t.Fatalf("edit = %d, want 200", code)
	}
	job := waitIdle(t, s)
	if job.Error != "" {
		t.Fatalf("run failed: %s (log: %s)", job.Error, job.Log)
	}
	if job.Tracked {
		t.Fatal("tracked should be false outside a repository")
	}
	if len(job.Changed) != 0 {
		t.Fatalf("changed = %v, want empty outside a repository", job.Changed)
	}
	body, _ := os.ReadFile(filepath.Join(root, "a.go"))
	if !strings.Contains(string(body), "touched") {
		t.Fatalf("harness did not edit the file: %q", body)
	}
}

func TestAgentRefusesSecondEditWhileRunning(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)
	s := agentServer(t, root, writeHarness(t, "sleep 2\n"))

	if code, _ := agentPost(t, s, "/api/agent/edit?path=keep.go&l1=1&l2=1&instruction=one"); code != 200 {
		t.Fatalf("first edit = %d, want 200", code)
	}
	code, body := agentPost(t, s, "/api/agent/edit?path=keep.go&l1=1&l2=1&instruction=two")
	if code != http.StatusConflict {
		t.Fatalf("second edit = %d, want 409", code)
	}
	if !strings.Contains(body["error"].(string), "already running") {
		t.Fatalf("error = %q", body["error"])
	}

	s.agent.Cancel()
	waitIdle(t, s)
}

func TestAgentAllowsEditOverUncommittedFileWithoutForce(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)
	// sub/mod.go is modified but not committed by gitRepo.
	s := agentServer(t, root, writeHarness(t, "printf 'touched\\n' >> keep.go\n"))

	code, _ := agentPost(t, s, "/api/agent/edit?path=sub/mod.go&l1=1&l2=1&instruction=hi")
	if code != http.StatusOK {
		t.Fatalf("edit over uncommitted work = %d, want 200", code)
	}
	if job := waitIdle(t, s); job.Error != "" {
		t.Fatalf("run failed: %s", job.Error)
	}
}

func TestAgentModelSelectionAndDefaults(t *testing.T) {
	isolateSettings(t)
	m, err := newAgentManager(t.TempDir(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	rows := m.Detect()
	if len(rows) == 0 {
		t.Fatal("expected detected rows")
	}
	for _, h := range rows {
		if !h.Installed {
			continue
		}
		if err := m.Select(h.Name, "custom-model"); err != nil {
			t.Fatalf("select %s with model: %v", h.Name, err)
		}
		if m.Model() != "custom-model" {
			t.Fatalf("model = %q, want custom-model", m.Model())
		}
	}
}

func TestPresetArgvOrder(t *testing.T) {
	for _, p := range harness.Presets() {
		n := len(p.Args)
		if n < 2 {
			t.Fatalf("preset %s args too short: %v", p.Name, p.Args)
		}
		if p.Args[n-1] != "{prompt}" {
			t.Fatalf("preset %s args %v: want {prompt} at the very end", p.Name, p.Args)
		}

		// When resolved, {prompt} must remain at the very end
		_, resolved, _, err := resolveAgentSpec(p.Name, "")
		if err != nil {
			continue // tool may not be installed in test env
		}
		rn := len(resolved)
		if rn < 2 || resolved[rn-1] != "{prompt}" {
			t.Fatalf("resolved %s args %v: want {prompt} at the very end", p.Name, resolved)
		}
	}
}

func TestClaudeModelDiscovery(t *testing.T) {
	dir := t.TempDir()
	fakeClaude := filepath.Join(dir, "claude")
	script := "#!/bin/sh\necho 'Current model: Sonnet 5'\necho 'Usage: /model <name>. Available: sonnet, opus, haiku, fable, best, sonnet[1m], opus[1m], fable[1m], opusplan, default, or a full model ID.'\n"
	if err := os.WriteFile(fakeClaude, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))

	models, err := harness.DiscoverModels(context.Background(), "claude")
	if err != nil {
		t.Fatalf("DiscoverModels error: %v", err)
	}

	if len(models) == 0 {
		t.Fatal("expected discovered models for claude, got none")
	}
	foundFable := false
	for _, m := range models {
		if m == "fable" {
			foundFable = true
			break
		}
	}
	if !foundFable {
		t.Fatalf("expected 'fable' in discovered models: %v", models)
	}
}

// Editing in the diff view means editing a file that is already modified. Its
// git status reads M before and after, so the change has to be seen some other way.
func TestAgentReportsEditToAlreadyModifiedFile(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)
	s := agentServer(t, root, writeHarness(t, "printf 'touched\\n' >> sub/mod.go\n"))

	if code, _ := agentPost(t, s, "/api/agent/edit?path=sub/mod.go&l1=1&l2=1&instruction=hi&force=1"); code != 200 {
		t.Fatalf("edit = %d, want 200", code)
	}
	job := waitIdle(t, s)
	if job.Error != "" {
		t.Fatalf("harness failed: %s", job.Error)
	}
	if len(job.Changed) != 1 || job.Changed[0] != "sub/mod.go" {
		t.Fatalf("changed = %v, want [sub/mod.go]", job.Changed)
	}
}

// Two edits on disjoint line ranges of the same file run at the same time;
// only an overlapping range is refused.
func TestAgentAllowsNonOverlappingEditsInParallel(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)
	s := agentServer(t, root, writeHarness(t, "sleep 0.15\n"))

	code, first := agentPost(t, s, "/api/agent/edit?path=keep.go&l1=1&l2=1&instruction=one")
	if code != 200 {
		t.Fatalf("first edit = %d, want 200", code)
	}
	code, _ = agentPost(t, s, "/api/agent/edit?path=keep.go&l1=2&l2=2&instruction=two")
	if code != 200 {
		t.Fatalf("disjoint-range edit = %d, want 200 (should run in parallel)", code)
	}
	code, _ = agentPost(t, s, "/api/agent/edit?path=keep.go&l1=1&l2=2&instruction=three")
	if code != http.StatusConflict {
		t.Fatalf("edit overlapping both = %d, want 409", code)
	}

	firstID := int64(first["id"].(float64))
	if job := waitIdleID(t, s, firstID); job.Error != "" {
		t.Fatalf("first edit failed: %s", job.Error)
	}
}

func TestAgentMutationsRejectCrossOriginPost(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)
	s := agentServer(t, root, writeHarness(t, "printf 'touched\\n' >> keep.go\n"))

	for _, path := range []string{
		"/api/agent/edit?path=keep.go&l1=1&l2=1&instruction=hi",
		"/api/agent/select?name=claude",
		"/api/agent/cancel",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Host = "127.0.0.1:7777"
		req.Header.Set("Origin", "http://evil.example.com")
		rec := httptest.NewRecorder()
		s.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("cross-origin %s = %d, want 403", path, rec.Code)
		}

		if code, _ := get(t, s, path); code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s = %d, want 405", path, code)
		}
	}
}

func TestAgentCancelJob(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)
	// Harness that sleeps to simulate a long-running edit
	s := agentServer(t, root, writeHarness(t, "sleep 5\n"))

	code, first := agentPost(t, s, "/api/agent/edit?path=keep.go&l1=1&l2=1&instruction=first")
	if code != http.StatusOK {
		t.Fatalf("edit first = %d, want 200", code)
	}
	id := int64(first["id"].(float64))

	// Cancel by id
	cancelCode, cancelBody := agentPost(t, s, fmt.Sprintf("/api/agent/cancel?id=%d", id))
	if cancelCode != http.StatusOK {
		t.Fatalf("cancel = %d, want 200", cancelCode)
	}
	if cancelled, _ := cancelBody["cancelled"].(bool); !cancelled {
		t.Fatalf("cancelled = false, want true")
	}

	// Job should be stopped
	time.Sleep(100 * time.Millisecond)
	code, job := get(t, s, fmt.Sprintf("/api/agent/job?id=%d", id))
	if code != http.StatusOK {
		t.Fatalf("job = %d, want 200", code)
	}
	if running, _ := job["running"].(bool); running {
		t.Fatalf("job still running after cancel")
	}
}

func TestReadLineRange(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(p, []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		l1, l2 int
		want   string
	}{
		{2, 3, "two\nthree"},
		{1, 1, "one"},
		{3, 99, "three\nfour\n"}, // clamped to the end, trailing blank line included
		{0, 1, "one"},            // l1 below 1 is clamped
	} {
		got, err := readLineRange(p, tc.l1, tc.l2)
		if err != nil {
			t.Fatalf("%d-%d: %v", tc.l1, tc.l2, err)
		}
		if got != tc.want {
			t.Fatalf("%d-%d = %q, want %q", tc.l1, tc.l2, got, tc.want)
		}
	}
	if _, err := readLineRange(p, 50, 60); err == nil {
		t.Fatal("a range past the end should fail")
	}
}

func TestChangedSinceReportsBothDirections(t *testing.T) {
	before := map[string]string{"stays.go": "M", "reverted.go": "M"}
	after := map[string]string{"stays.go": "M", "new.go": "U"}

	got := map[string]bool{}
	for _, p := range changedSinceMaps(before, after) {
		got[p] = true
	}
	if got["stays.go"] {
		t.Fatal("an unchanged status should not be reported")
	}
	if !got["new.go"] {
		t.Fatal("a newly dirty file should be reported")
	}
	if !got["reverted.go"] {
		t.Fatal("a file restored to its committed state should be reported")
	}
}

// The job summary and API response list changed files, so their order must not
// follow Go's randomised map iteration. Repeated to catch a lucky ordering.
func TestChangedSinceIsSorted(t *testing.T) {
	before := map[string]string{"b.go": "M", "d.go": "M"}
	after := map[string]string{"c.go": "U", "a.go": "M", "d.go": "M"}
	for i := 0; i < 20; i++ {
		if got := strings.Join(changedSinceMaps(before, after), ","); got != "a.go,b.go,c.go" {
			t.Fatalf("changed = %q, want a.go,b.go,c.go", got)
		}
	}
}

func TestLineRef(t *testing.T) {
	if lineRef(4, 4) != "4" {
		t.Fatalf("single line ref = %q", lineRef(4, 4))
	}
	if lineRef(4, 9) != "4-9" {
		t.Fatalf("range ref = %q", lineRef(4, 9))
	}
}

func TestAllPresetArgvFormatting(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	for _, p := range harness.Presets() {
		binPath := filepath.Join(dir, p.Args[0])
		if err := os.WriteFile(binPath, []byte("#!/bin/sh\nexit 0\n"), 0755); err != nil {
			t.Fatal(err)
		}
	}

	for _, p := range harness.Presets() {
		name, resolved, model, err := resolveAgentSpec(p.Name, "test-model")
		if err != nil {
			t.Fatalf("resolveAgentSpec(%q) error: %v", p.Name, err)
		}
		if name != p.Name {
			t.Errorf("name = %q, want %q", name, p.Name)
		}
		if model != "test-model" {
			t.Errorf("model = %q, want %q", model, "test-model")
		}
		if resolved[len(resolved)-1] != "{prompt}" {
			t.Errorf("%s final arg = %q, want {prompt}", p.Name, resolved[len(resolved)-1])
		}
		// Ensure model flag was inserted properly
		hasModel := false
		for i, a := range resolved {
			if a == p.ModelFlag && i+1 < len(resolved) && resolved[i+1] == "test-model" {
				hasModel = true
				break
			}
		}
		if !hasModel {
			t.Errorf("%s resolved args %v missing model flag %q %q", p.Name, resolved, p.ModelFlag, "test-model")
		}
	}
}

func TestAgentBatchEditRejectsIntraBatchOverlap(t *testing.T) {
	isolateSettings(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\nvar X = 1\nvar Y = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := newAgentManager(root, "echo {prompt}", nil)
	if err != nil {
		t.Fatal(err)
	}
	tm := newThreadManager(m, root)

	// Overlapping ranges on the same file: lines 1-2 and lines 2-3
	items := []agentBatchItem{
		{Abs: filepath.Join(root, "a.go"), Path: "a.go", L1: 1, L2: 2, Instruction: "edit 1"},
		{Abs: filepath.Join(root, "a.go"), Path: "a.go", L1: 2, L2: 3, Instruction: "edit 2"},
	}
	if _, err := tm.StartEdit(items); err == nil {
		t.Fatal("expected overlapping batch edits on same file to be rejected, got nil error")
	}

	// Disjoint ranges on the same file: lines 1-1 and lines 3-3 (should succeed)
	validItems := []agentBatchItem{
		{Abs: filepath.Join(root, "a.go"), Path: "a.go", L1: 1, L2: 1, Instruction: "edit 1"},
		{Abs: filepath.Join(root, "a.go"), Path: "a.go", L1: 3, L2: 3, Instruction: "edit 2"},
	}
	job, err := tm.StartEdit(validItems)
	if err != nil {
		t.Fatalf("expected disjoint batch edits to succeed, got %v", err)
	}
	if job.BatchCount != 2 {
		t.Errorf("job.BatchCount = %d, want 2", job.BatchCount)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if j := m.Job(job.ID); j != nil && !j.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAgentBatchEditRunsHarness(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)

	s := agentServer(t, root, writeHarness(t,
		`echo "// edited 1" >> keep.go
echo "// edited 2" >> sub/mod.go
`))

	payload := map[string]any{
		"edits": []map[string]any{
			{"path": "keep.go", "l1": 1, "l2": 1, "instruction": "edit keep"},
			{"path": "sub/mod.go", "l1": 1, "l2": 1, "instruction": "edit mod"},
		},
	}
	code, body := agentPostJSON(t, s, "/api/agent/batch", payload)
	if code != 200 {
		t.Fatalf("batch edit = %d, want 200 (error: %v)", code, body["error"])
	}
	id := int64(body["id"].(float64))
	j := waitIdleID(t, s, id)
	if j.Error != "" {
		t.Fatalf("batch job failed: %s", j.Error)
	}
	if len(j.Changed) != 2 {
		t.Fatalf("changed = %v, want 2 files", j.Changed)
	}
}

func TestAgentAllowsIndividualAndBatchEditsConcurrently(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)

	s := agentServer(t, root, writeHarness(t,
		`sleep 0.2
echo "// edited" >> keep.go
`))

	// Start individual edit on keep.go line 1
	code1, body1 := agentPost(t, s, "/api/agent/edit?path=keep.go&l1=1&l2=1&instruction=one")
	if code1 != 200 {
		t.Fatalf("edit 1 = %d, want 200 (error: %v)", code1, body1["error"])
	}
	id1 := int64(body1["id"].(float64))

	// Attempting an overlapping batch edit on keep.go:1-2 must be rejected
	overlapPayload := map[string]any{
		"edits": []map[string]any{
			{"path": "keep.go", "l1": 1, "l2": 2, "instruction": "overlap"},
		},
	}
	overlapCode, _ := agentPostJSON(t, s, "/api/agent/batch", overlapPayload)
	if overlapCode != 409 {
		t.Fatalf("overlapping batch edit = %d, want 409", overlapCode)
	}

	// Non-overlapping batch edit on keep.go:2-2 and sub/mod.go:1-1 must be accepted concurrently
	batchPayload := map[string]any{
		"edits": []map[string]any{
			{"path": "keep.go", "l1": 2, "l2": 2, "instruction": "non-overlap keep"},
			{"path": "sub/mod.go", "l1": 1, "l2": 1, "instruction": "non-overlap mod"},
		},
	}
	code2, body2 := agentPostJSON(t, s, "/api/agent/batch", batchPayload)
	if code2 != 200 {
		t.Fatalf("batch edit 2 = %d, want 200 (error: %v)", code2, body2["error"])
	}
	id2 := int64(body2["id"].(float64))

	j1 := waitIdleID(t, s, id1)
	j2 := waitIdleID(t, s, id2)
	if j1.Error != "" {
		t.Fatalf("job 1 failed: %s", j1.Error)
	}
	if j2.Error != "" {
		t.Fatalf("job 2 failed: %s", j2.Error)
	}
}

func TestAgentBatchEditPayloadVariations(t *testing.T) {
	if !gitInstalled() {
		t.Skip("git not installed")
	}
	root := gitRepo(t)
	s := agentServer(t, root, writeHarness(t, "exit 0\n"))

	// 1. Direct array JSON body
	directArray := []map[string]any{
		{"path": "keep.go", "l1": 1, "l2": 1, "instruction": "arr 1"},
	}
	c1, b1 := agentPostJSON(t, s, "/api/agent/batch", directArray)
	if c1 != 200 {
		t.Fatalf("direct array batch = %d, want 200 (err: %v)", c1, b1["error"])
	}
	waitIdleID(t, s, int64(b1["id"].(float64)))

	// 2. Query parameter edits JSON string
	c2, b2 := agentPost(t, s, "/api/agent/batch?edits=%5B%7B%22path%22%3A%22keep.go%22%2C%22l1%22%3A1%2C%22l2%22%3A1%2C%22instruction%22%3A%22query%22%7D%5D")
	if c2 != 200 {
		t.Fatalf("query param batch = %d, want 200 (err: %v)", c2, b2["error"])
	}
	waitIdleID(t, s, int64(b2["id"].(float64)))

	// 3. Single edit object JSON body
	singleObj := map[string]any{
		"path": "keep.go", "l1": 1, "l2": 1, "instruction": "single",
	}
	c3, b3 := agentPostJSON(t, s, "/api/agent/batch", singleObj)
	if c3 != 200 {
		t.Fatalf("single obj batch = %d, want 200 (err: %v)", c3, b3["error"])
	}
	waitIdleID(t, s, int64(b3["id"].(float64)))

	// 4. Empty payload -> 400
	c4, b4 := agentPostJSON(t, s, "/api/agent/batch", map[string]any{"edits": []any{}})
	if c4 != 400 {
		t.Fatalf("empty batch = %d, want 400", c4)
	}
	if b4["error"] != "invalid or empty batch edits payload" {
		t.Fatalf("empty batch error = %v", b4["error"])
	}
}

func TestCommitMessagePrompt(t *testing.T) {
	files := []string{"web/src/gitpanel.js", "server.go"}
	stat := " web/src/gitpanel.js | 15 +++\n server.go           | 40 ++-\n 2 files changed, 45 insertions(+), 10 deletions(-)"
	diff := "diff --git a/server.go b/server.go\n--- a/server.go\n+++ b/server.go\n@@ -1 +1 @@\n-old\n+new"
	instruction := "Follow conventional commits format"

	p := commitMessagePrompt(files, stat, diff, instruction)
	if !strings.Contains(p, "Changed files (2):") {
		t.Errorf("prompt missing changed files header:\n%s", p)
	}
	if !strings.Contains(p, "- web/src/gitpanel.js") || !strings.Contains(p, "- server.go") {
		t.Errorf("prompt missing changed file paths:\n%s", p)
	}
	if !strings.Contains(p, "Summary of changes (diffstat):") || !strings.Contains(p, stat) {
		t.Errorf("prompt missing diffstat summary:\n%s", p)
	}
	if !strings.Contains(p, "Staged diff:") || !strings.Contains(p, diff) {
		t.Errorf("prompt missing staged diff:\n%s", p)
	}
	if !strings.Contains(p, "Additional instructions from the user: "+instruction) {
		t.Errorf("prompt missing user instruction:\n%s", p)
	}

	// Test truncation when there are > 100 files
	manyFiles := make([]string, 125)
	for i := range manyFiles {
		manyFiles[i] = fmt.Sprintf("file%d.go", i)
	}
	p2 := commitMessagePrompt(manyFiles, "", "", "")
	if !strings.Contains(p2, "Changed files (125):") {
		t.Errorf("prompt missing total count:\n%s", p2)
	}
	if !strings.Contains(p2, "... and 25 more files") {
		t.Errorf("prompt missing truncation notice:\n%s", p2)
	}
	if !strings.Contains(p2, "- file0.go") || !strings.Contains(p2, "- file99.go") {
		t.Errorf("prompt missing head files:\n%s", p2)
	}
	if strings.Contains(p2, "- file100.go") {
		t.Errorf("prompt contains file beyond 100:\n%s", p2)
	}
}



func TestParseClaudeEvent(t *testing.T) {
	evs := parseClaudeEvent(`{"type":"assistant","session_id":"s1","message":{"content":[{"type":"text","text":"Looking."},{"type":"tool_use","name":"Edit","input":{"file_path":"a.go"}}]}}`)
	kinds := []string{}
	for _, e := range evs {
		kinds = append(kinds, e.kind+":"+e.text)
	}
	want := "session: text:Looking. tool:Edit a.go"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if evs := parseClaudeEvent(`{"type":"result","is_error":true,"result":"bad key"}`); len(evs) != 1 || evs[0].kind != "error" || evs[0].text != "bad key" {
		t.Fatalf("error result = %+v", evs)
	}
	if evs := parseClaudeEvent("not json"); len(evs) != 0 {
		t.Fatalf("non-JSON yielded %+v", evs)
	}
}

func TestParseAgyEvent(t *testing.T) {
	if evs := parseAgyEvent(`{"event":"init","conversation_id":"c123"}`); len(evs) != 1 || evs[0].kind != "session" || evs[0].text != "c123" {
		t.Fatalf("agy init = %+v", evs)
	}
	toolLine := `{"event":"step_update","step_update":{"step_type":"tool","tool_name":"view_file","state":"ACTIVE","tool_info":{"name":"view_file","parameters":{"AbsolutePath":"/path/file.go"}}}}`
	if evs := parseAgyEvent(toolLine); len(evs) != 1 || evs[0].kind != "tool" || evs[0].text != "view_file /path/file.go" {
		t.Fatalf("agy tool = %+v", evs)
	}
	deltaLine := `{"event":"step_update","step_update":{"step_type":"agent_response","text_delta":"hello world"}}`
	if evs := parseAgyEvent(deltaLine); len(evs) != 1 || evs[0].kind != "text" || evs[0].text != "hello world" {
		t.Fatalf("agy delta = %+v", evs)
	}
	resLine := `{"event":"result","result":{"status":"SUCCESS","response":"done"}}`
	if evs := parseAgyEvent(resLine); len(evs) != 1 || evs[0].kind != "result" || evs[0].text != "done" {
		t.Fatalf("agy result = %+v", evs)
	}
}

func TestParseGeminiEvent(t *testing.T) {
	if evs := parseGeminiEvent(`{"type":"init","session_id":"s456"}`); len(evs) != 1 || evs[0].kind != "session" || evs[0].text != "s456" {
		t.Fatalf("gemini init = %+v", evs)
	}
	toolLine := `{"type":"tool_use","tool_name":"read_file","parameters":{"file_path":"b.go"}}`
	if evs := parseGeminiEvent(toolLine); len(evs) != 1 || evs[0].kind != "tool" || evs[0].text != "read_file b.go" {
		t.Fatalf("gemini tool = %+v", evs)
	}
	msgLine := `{"type":"message","role":"assistant","content":"thinking...","delta":true}`
	if evs := parseGeminiEvent(msgLine); len(evs) != 1 || evs[0].kind != "text" || evs[0].text != "thinking..." {
		t.Fatalf("gemini message = %+v", evs)
	}
}

func TestThreadArgv(t *testing.T) {
	tpl := []string{"/bin/claude", "--permission-mode", "acceptEdits", "-p", "{prompt}"}
	first := strings.Join(threadArgv("claude", tpl, "U", false), " ")
	if !strings.Contains(first, "--session-id U") || strings.Contains(first, "--resume") || !strings.Contains(first, "stream-json") {
		t.Fatalf("first turn argv = %s", first)
	}
	next := strings.Join(threadArgv("claude", tpl, "U", true), " ")
	if !strings.Contains(next, "--resume U") || strings.Contains(next, "--session-id") {
		t.Fatalf("resume argv = %s", next)
	}
	if got := strings.Join(threadArgv("cursor-agent", []string{"/bin/cursor-agent", "-p", "{prompt}"}, "C", true), " "); !strings.Contains(got, "--resume C") {
		t.Fatalf("cursor argv = %s", got)
	}
	if got := threadArgv("codex", tpl, "", false); len(got) != len(tpl) {
		t.Fatalf("unsupported harness argv changed: %v", got)
	}
	if tpl[1] != "--permission-mode" {
		t.Fatal("threadArgv mutated its template")
	}
}

func TestThreadPrompt(t *testing.T) {
	th := &thread{Path: "main.go", L1: 3, L2: 4, Snippet: "func main() {}"}
	first := threadPrompt(th, nil, "why?", false, "", false)
	if !strings.Contains(first, "@main.go lines 3-4") || !strings.Contains(first, "func main() {}") || !strings.HasSuffix(first, "why?") {
		t.Fatalf("first prompt = %q", first)
	}
	prior := []*threadTurn{{Prompt: "why?", Reply: "because", Changed: []string{"a.go"}}}
	if got := threadPrompt(th, prior, "and now?", false, "", false); got != "and now?" {
		t.Fatalf("a live session should get the bare message, got %q", got)
	}
	replay := threadPrompt(th, prior, "and now?", true, "", false)
	for _, want := range []string{"@main.go", "User: why?", "Assistant: because", "a.go", "and now?"} {
		if !strings.Contains(replay, want) {
			t.Fatalf("replay prompt missing %q:\n%s", want, replay)
		}
	}
}

func waitThreadIdle(t *testing.T, s *Server, id string) *thread {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if th := s.threads.Get(id); th != nil && !th.running() {
			return th
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("thread turn did not finish")
	return nil
}

func TestThreadConversationWithPlainHarness(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	// Echoes the prompt it was given, so the test can see what a replay carries.
	h := writeHarness(t, `printf 'saw: %s\n' "$1"`)
	s := agentServer(t, root, h)

	code, m := agentPostJSON(t, s, "/api/threads/create", map[string]any{"path": "main.go", "l1": 3, "l2": 3, "message": "what does main do?"})
	if code != 200 {
		t.Fatalf("create = %d %v", code, m)
	}
	id := m["id"].(string)
	th := waitThreadIdle(t, s, id)
	if th.Error() != "" || len(th.Turns) != 1 {
		t.Fatalf("turn 1 = %+v", th.Turns)
	}
	if !strings.Contains(th.Turns[0].Reply, "saw:") || !strings.Contains(th.Turns[0].Reply, "func main() {}") {
		t.Fatalf("reply should echo an anchored prompt, got %q", th.Turns[0].Reply)
	}

	if code, _ := agentPostJSON(t, s, "/api/threads/send", map[string]any{"id": id, "message": "and its tests?"}); code != 200 {
		t.Fatalf("send = %d", code)
	}
	th = waitThreadIdle(t, s, id)
	if len(th.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(th.Turns))
	}
	second := th.Turns[1].Reply
	if !strings.Contains(second, "what does main do?") || !strings.Contains(second, "and its tests?") {
		t.Fatalf("a harness without sessions must be replayed the transcript, got %q", second)
	}

	// Threads survive a restart, with the same transcript.
	again := newThreadManager(s.agent, root)
	got := again.Get(id)
	if got == nil || len(got.Turns) != 2 || got.Title != "what does main do?" {
		t.Fatalf("reloaded thread = %+v", got)
	}

	if !s.threads.Delete(id) || s.threads.Get(id) != nil {
		t.Fatal("delete did not remove the thread")
	}
}

func (t *thread) Error() string {
	if n := len(t.Turns); n > 0 {
		return t.Turns[n-1].Error
	}
	return ""
}

func TestThreadClaudeSessionResume(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv.log")
	// A stand-in named claude: records its argv and emits stream-json.
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + log + "\n" +
		`printf '{"type":"system","subtype":"init","session_id":"x"}\n'` + "\n" +
		`printf '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"file_path":"a.go"}}]}}\n'` + "\n" +
		`printf '{"type":"assistant","message":{"content":[{"type":"text","text":"All good."}]}}\n'` + "\n" +
		`printf '{"type":"result","is_error":false,"result":"All good."}\n'` + "\n"
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	s := agentServer(t, root, bin)

	_, m := agentPostJSON(t, s, "/api/threads/create", map[string]any{"message": "look around"})
	id := m["id"].(string)
	th := waitThreadIdle(t, s, id)
	if th.Turns[0].Reply != "All good." || len(th.Turns[0].Tools) != 1 || th.Turns[0].Tools[0] != "Read a.go" {
		t.Fatalf("parsed turn = %+v", th.Turns[0])
	}
	agentPostJSON(t, s, "/api/threads/send", map[string]any{"id": id, "message": "again"})
	th = waitThreadIdle(t, s, id)
	if len(th.Turns) != 2 || th.Turns[1].Error != "" {
		t.Fatalf("turn 2 = %+v", th.Turns)
	}

	data, _ := os.ReadFile(log)
	var lines []string
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, "--output-format") { // the first prompt spans several lines
			lines = append(lines, l)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("harness ran %d times, want 2:\n%s", len(lines), data)
	}
	sid := th.SessionID
	if sid == "" || !strings.Contains(lines[0], "--session-id "+sid) {
		t.Fatalf("first run should start session %q: %s", sid, lines[0])
	}
	if !strings.Contains(lines[1], "--resume "+sid) || strings.Contains(lines[1], "Earlier in this conversation") {
		t.Fatalf("second run should resume without replay: %s", lines[1])
	}
}

func TestThreadBusyAndCancel(t *testing.T) {
	root := t.TempDir()
	h := writeHarness(t, "sleep 30")
	s := agentServer(t, root, h)
	_, m := agentPostJSON(t, s, "/api/threads/create", map[string]any{"message": "slow one"})
	id := m["id"].(string)
	if code, _ := agentPostJSON(t, s, "/api/threads/send", map[string]any{"id": id, "message": "impatient"}); code != 409 {
		t.Fatalf("send while running = %d, want 409", code)
	}
	if code, _ := agentPost(t, s, "/api/threads/cancel?id="+id); code != 200 {
		t.Fatalf("cancel = %d", code)
	}
	th := waitThreadIdle(t, s, id)
	if th.Turns[0].Error != "cancelled" {
		t.Fatalf("cancelled turn error = %q", th.Turns[0].Error)
	}
}

func TestThreadInterruptedTurnIsClosedOnLoad(t *testing.T) {
	isolateSettings(t)
	root := t.TempDir()
	m, _ := newAgentManager(root, "", nil)
	tm := newThreadManager(m, root)
	tm.mu.Lock()
	tm.threads["t1"] = &thread{ID: "t1", Title: "x", Turns: []*threadTurn{{ID: 1, Prompt: "p", Running: true}}}
	tm.saveLocked()
	tm.mu.Unlock()

	got := newThreadManager(m, root).Get("t1")
	if got == nil || got.Turns[0].Running || !strings.Contains(got.Turns[0].Error, "interrupted") {
		t.Fatalf("interrupted turn = %+v", got)
	}
}

func TestInlineEditRunsAsThread(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644)
	os.WriteFile(filepath.Join(root, "b.go"), []byte("package b\n\nfunc B() {}\n"), 0o644)
	h := writeHarness(t, `printf 'did it\n'`)
	s := agentServer(t, root, h)

	code, m := agentPostJSON(t, s, "/api/agent/batch", map[string]any{"edits": []map[string]any{
		{"path": "a.go", "l1": 3, "l2": 3, "instruction": "rename A"},
		{"path": "b.go", "l1": 3, "l2": 3, "instruction": "rename B"},
	}})
	if code != 200 {
		t.Fatalf("batch = %d %v", code, m)
	}
	id := int64(m["id"].(float64))
	job := waitIdleID(t, s, id)
	if job.Error != "" || job.BatchCount != 2 || !strings.Contains(job.Stdout, "did it") {
		t.Fatalf("job = %+v", job)
	}

	list := s.threads.List()
	if len(list) != 1 || list[0].Kind != "batch" || list[0].Title != "Batch edit: 2 changes" {
		t.Fatalf("threads = %+v", list)
	}
	th := s.threads.Get(list[0].ID)
	if p := th.Turns[0].Prompt; !strings.Contains(p, "rename A") || !strings.Contains(p, "@b.go") {
		t.Fatalf("batch prompt should carry every instruction and snippet: %q", p)
	}
	// The thread can be followed up like any conversation.
	if code, _ := agentPostJSON(t, s, "/api/threads/send", map[string]any{"id": th.ID, "message": "why?"}); code != 200 {
		t.Fatalf("follow-up = %d", code)
	}
	waitThreadIdle(t, s, th.ID)
}

func TestInlineEditOverlapStillRefused(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644)
	s := agentServer(t, root, writeHarness(t, "sleep 30"))
	q := "/api/agent/edit?path=a.go&l1=3&l2=3&instruction=one"
	if code, _ := agentPost(t, s, q); code != 200 {
		t.Fatalf("first edit = %d", code)
	}
	if code, _ := agentPost(t, s, "/api/agent/edit?path=a.go&l1=2&l2=4&instruction=two"); code != 409 {
		t.Fatalf("overlapping edit = %d, want 409", code)
	}
	// A conversation is never guarded the way an edit is.
	if code, _ := agentPostJSON(t, s, "/api/threads/create", map[string]any{"path": "a.go", "l1": 3, "l2": 3, "message": "hi"}); code != 200 {
		t.Fatalf("thread on the same lines = %d, want 200", code)
	}
	s.agent.Cancel()
	s.threads.Close()
	// Let the cancelled turns finish saving before the temp dir goes away.
	for _, sum := range s.threads.List() {
		waitThreadIdle(t, s, sum.ID)
	}
}

// In a PR review the harness is told what the thread is about: the whole PR
// (merge-base to PR head), the reviewer's own changes, or just the selection.
func TestThreadPromptCarriesPRScope(t *testing.T) {
	root := t.TempDir()
	th := &thread{Kind: "ask"}
	s := &Server{ix: NewIndex(root), pr: &prSession{
		target:   PRTarget{URL: "https://github.com/o/r/pull/7"},
		meta:     PRMeta{Number: 7, Title: "Add thing", BaseRef: "main", HeadRef: "feat", HeadSHA: "abcdef1234567890"},
		diffBase: "1234567890abcdef",
	}}
	defer s.pr.Close()

	whole := s.prThreadContext("pr")
	for _, want := range []string{"#7", "Add thing", "git diff 1234567890abcdef abcdef1234567890", "entire pull request"} {
		if !strings.Contains(whole, want) {
			t.Errorf("whole-PR context missing %q:\n%s", want, whole)
		}
	}
	if s.prThreadContext("") != whole {
		t.Error("an unset scope means the whole PR")
	}
	mine := s.prThreadContext("mine")
	for _, want := range []string{"OWN changes", "git diff abcdef1234567890", "git log --stat abcdef1234567890..HEAD", "background only"} {
		if !strings.Contains(mine, want) {
			t.Errorf("mine context missing %q:\n%s", want, mine)
		}
	}
	if strings.Contains(mine, "entire pull request") {
		t.Error("mine scope must not tell the harness to review the whole PR")
	}
	sel := s.prThreadContext("selection")
	if !strings.Contains(sel, "anchored") || strings.Contains(sel, "entire pull request") {
		t.Errorf("selection context wrong:\n%s", sel)
	}

	first := threadPrompt(th, nil, "what do you think about the PR?", false, whole, false)
	if !strings.Contains(first, "git diff 1234567890abcdef") || !strings.HasSuffix(first, "what do you think about the PR?") {
		t.Errorf("first prompt should carry the PR context before the question:\n%s", first)
	}
	prior := []*threadTurn{{Prompt: "x", Reply: "y"}}
	if got := threadPrompt(th, prior, "and now?", false, whole, false); got != "and now?" {
		t.Errorf("follow-up turns stay bare when the scope is unchanged, got %q", got)
	}
	changed := threadPrompt(th, prior, "and now?", false, mine, true)
	if !strings.Contains(changed, "changed what this conversation is about") || !strings.Contains(changed, "OWN changes") || !strings.HasSuffix(changed, "and now?") {
		t.Errorf("a scope change should be announced with the new context:\n%s", changed)
	}
	if (&Server{}).prThreadContext("pr") != "" {
		t.Error("no PR, no PR context")
	}
}

func TestThreadDeleteAPI(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	h := writeHarness(t, `printf 'ok\n'`)
	s := agentServer(t, root, h)

	// Create thread 1 and delete via query parameter
	code, m1 := agentPostJSON(t, s, "/api/threads/create", map[string]any{"path": "main.go", "l1": 1, "l2": 1, "message": "thread 1"})
	if code != 200 {
		t.Fatalf("create 1 = %d", code)
	}
	id1 := m1["id"].(string)
	waitThreadIdle(t, s, id1)

	code, delResp1 := agentPost(t, s, "/api/threads/delete?id="+id1)
	if code != 200 || delResp1["deleted"] != true {
		t.Fatalf("delete via query = %d, resp: %v", code, delResp1)
	}
	if s.threads.Get(id1) != nil {
		t.Fatalf("thread 1 should be gone after delete")
	}

	// Create thread 2 and delete via JSON body
	code, m2 := agentPostJSON(t, s, "/api/threads/create", map[string]any{"path": "main.go", "l1": 1, "l2": 1, "message": "thread 2"})
	if code != 200 {
		t.Fatalf("create 2 = %d", code)
	}
	id2 := m2["id"].(string)
	waitThreadIdle(t, s, id2)

	code, delResp2 := agentPostJSON(t, s, "/api/threads/delete", map[string]any{"id": id2})
	if code != 200 || delResp2["deleted"] != true {
		t.Fatalf("delete via json body = %d, resp: %v", code, delResp2)
	}
	if s.threads.Get(id2) != nil {
		t.Fatalf("thread 2 should be gone after delete")
	}

	// Deleting nonexistent thread returns 200 with deleted: false
	code, delResp3 := agentPostJSON(t, s, "/api/threads/delete", map[string]any{"id": "nonexistent"})
	if code != 200 || delResp3["deleted"] != false {
		t.Fatalf("delete nonexistent = %d, resp: %v", code, delResp3)
	}

	// Missing id returns 400
	code, _ = agentPost(t, s, "/api/threads/delete")
	if code != 400 {
		t.Fatalf("delete without id = %d, want 400", code)
	}
}
