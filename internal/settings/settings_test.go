package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolateSettings(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return dir
}

func TestSettingsDefaults(t *testing.T) {
	isolateSettings(t)
	m := ReadMergedMap()
	if m["editor.fontSize"] != 13.5 {
		t.Errorf("expected editor.fontSize 13.5, got %v", m["editor.fontSize"])
	}
	if m["workbench.colorTheme"] != "catppuccin-mocha" {
		t.Errorf("expected workbench.colorTheme catppuccin-mocha, got %v", m["workbench.colorTheme"])
	}
	if m["editor.wordWrap"] != "on" {
		t.Errorf("expected editor.wordWrap on, got %v", m["editor.wordWrap"])
	}
	if m["editor.cursorStyle"] != "line" {
		t.Errorf("expected editor.cursorStyle line, got %v", m["editor.cursorStyle"])
	}
	if m["explorer.compactFolders"] != true {
		t.Errorf("expected explorer.compactFolders true, got %v", m["explorer.compactFolders"])
	}
	if m["explorer.autoReveal"] != true {
		t.Errorf("expected explorer.autoReveal true, got %v", m["explorer.autoReveal"])
	}
	if m["explorer.autoRelveal"] != true {
		t.Errorf("expected explorer.autoRelveal true, got %v", m["explorer.autoRelveal"])
	}
	if m["search.smartCase"] != true {
		t.Errorf("expected search.smartCase true, got %v", m["search.smartCase"])
	}
	if m["lsp.enabled"] != true {
		t.Errorf("expected lsp.enabled true, got %v", m["lsp.enabled"])
	}
	if m["agent.timeoutSeconds"] != 120.0 && m["agent.timeoutSeconds"] != 120 {
		t.Errorf("expected agent.timeoutSeconds 120, got %v", m["agent.timeoutSeconds"])
	}
	if m["server.basePath"] != "/" {
		t.Errorf("expected server.basePath /, got %v", m["server.basePath"])
	}
}

func TestSettingsAutoReveal(t *testing.T) {
	isolateSettings(t)

	// Defaults to true
	m := ReadMergedMap()
	if m["explorer.autoReveal"] != true {
		t.Fatalf("expected default explorer.autoReveal true, got %v", m["explorer.autoReveal"])
	}
	if m["explorer.autoRelveal"] != true {
		t.Fatalf("expected default explorer.autoRelveal true, got %v", m["explorer.autoRelveal"])
	}

	// Update via explorer.autoReveal = false
	err := UpdateMap(map[string]any{
		"explorer.autoReveal": false,
	})
	if err != nil {
		t.Fatalf("UpdateMap failed: %v", err)
	}

	s := Read()
	if s.ExplorerAutoReveal == nil || *s.ExplorerAutoReveal != false {
		t.Fatalf("expected ExplorerAutoReveal false, got %v", s.ExplorerAutoReveal)
	}

	m = ReadMergedMap()
	if m["explorer.autoReveal"] != false {
		t.Errorf("expected explorer.autoReveal false, got %v", m["explorer.autoReveal"])
	}
	if m["explorer.autoRelveal"] != false {
		t.Errorf("expected explorer.autoRelveal false, got %v", m["explorer.autoRelveal"])
	}

	// Update via explorer.autoRelveal = true
	err = UpdateMap(map[string]any{
		"explorer.autoRelveal": true,
	})
	if err != nil {
		t.Fatalf("UpdateMap failed: %v", err)
	}

	s = Read()
	if s.ExplorerAutoReveal == nil || *s.ExplorerAutoReveal != true {
		t.Fatalf("expected ExplorerAutoReveal true, got %v", s.ExplorerAutoReveal)
	}

	m = ReadMergedMap()
	if m["explorer.autoReveal"] != true {
		t.Errorf("expected explorer.autoReveal true, got %v", m["explorer.autoReveal"])
	}
	if m["explorer.autoRelveal"] != true {
		t.Errorf("expected explorer.autoRelveal true, got %v", m["explorer.autoRelveal"])
	}
}

func TestSettingsPreserveNonAgentValues(t *testing.T) {
	cfg := isolateSettings(t)

	// Step 1: Update editor settings
	err := UpdateMap(map[string]any{
		"editor.fontSize":      16.0,
		"workbench.colorTheme": "gruvbox-dark",
		"custom.property":      "hello",
	})
	if err != nil {
		t.Fatalf("UpdateMap failed: %v", err)
	}

	// Step 2: Agent selects a harness (which calls Write)
	err = Write(Settings{
		Agent: "claude",
		Models: map[string]string{
			"claude": "sonnet",
		},
	})
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	// Step 3: Verify editor settings were NOT wiped
	m := ReadMergedMap()
	if m["editor.fontSize"] != 16.0 {
		t.Errorf("editor.fontSize was overwritten: got %v, want 16", m["editor.fontSize"])
	}
	if m["workbench.colorTheme"] != "gruvbox-dark" {
		t.Errorf("workbench.colorTheme was overwritten: got %v, want gruvbox-dark", m["workbench.colorTheme"])
	}
	if m["custom.property"] != "hello" {
		t.Errorf("custom.property was lost: got %v, want hello", m["custom.property"])
	}
	if m["agent"] != "claude" || m["agent.harness"] != "claude" {
		t.Errorf("agent not set properly: %v", m["agent"])
	}

	// Step 4: Verify file on disk
	data, err := os.ReadFile(filepath.Join(cfg, "px0", "settings.json"))
	if err != nil {
		t.Fatalf("failed to read settings file: %v", err)
	}
	var diskMap map[string]any
	if err := json.Unmarshal(data, &diskMap); err != nil {
		t.Fatalf("corrupt settings JSON: %v", err)
	}
	if diskMap["editor.fontSize"] != 16.0 {
		t.Errorf("diskMap missing editor.fontSize: %v", diskMap)
	}
}

func TestSettingsBasePath(t *testing.T) {
	isolateSettings(t)

	// Defaults to nil in Read()
	s := Read()
	if s.ServerBasePath != nil {
		t.Errorf("expected default ServerBasePath to be nil, got %v", *s.ServerBasePath)
	}

	// Update via settings map
	err := UpdateMap(map[string]any{
		"server.basePath": "/rev-123/",
	})
	if err != nil {
		t.Fatalf("UpdateMap failed: %v", err)
	}

	s = Read()
	if s.ServerBasePath == nil || *s.ServerBasePath != "/rev-123/" {
		t.Errorf("expected ServerBasePath /rev-123/, got %v", s.ServerBasePath)
	}

	m := ReadMergedMap()
	if m["server.basePath"] != "/rev-123/" {
		t.Errorf("expected merged server.basePath /rev-123/, got %v", m["server.basePath"])
	}

	// Fallback when keyed as basePath
	err = UpdateMap(map[string]any{
		"basePath": "/rev-456/",
	})
	if err != nil {
		t.Fatalf("UpdateMap failed: %v", err)
	}

	s = Read()
	if s.ServerBasePath == nil || *s.ServerBasePath != "/rev-456/" {
		t.Errorf("expected ServerBasePath /rev-456/ from fallback, got %v", s.ServerBasePath)
	}
}

func TestSettingsRawJSON(t *testing.T) {
	isolateSettings(t)

	// Initial raw JSON should be empty object
	initial := ReadRawJSON()
	if initial == "" {
		t.Error("expected non-empty initial raw JSON")
	}

	// Save valid JSON
	raw := []byte(`{"editor.fontSize": 15.0, "agent": "claude-code"}`)
	if err := SaveRawJSON(raw); err != nil {
		t.Fatalf("SaveRawJSON failed: %v", err)
	}

	readBack := ReadRawJSON()
	if !strings.Contains(readBack, "editor.fontSize") || !strings.Contains(readBack, "15") {
		t.Errorf("expected saved settings in ReadRawJSON, got: %s", readBack)
	}

	// Invalid JSON should return error
	if err := SaveRawJSON([]byte(`{not valid json}`)); err == nil {
		t.Error("expected error saving invalid JSON")
	}
}

func BenchmarkSettingsReadMergedMap(b *testing.B) {
	isolateSettingsBenchmark(b)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = ReadMergedMap()
	}
}

func BenchmarkSettingsUpdateMap(b *testing.B) {
	isolateSettingsBenchmark(b)
	update := map[string]any{"editor.fontSize": 14.0}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = UpdateMap(update)
	}
}

func isolateSettingsBenchmark(b *testing.B) {
	b.Helper()
	dir := b.TempDir()
	b.Setenv("XDG_CONFIG_HOME", dir)
}

