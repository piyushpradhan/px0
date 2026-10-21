package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestUISpinner(t *testing.T) {
	origQuiet := Quiet
	defer func() { Quiet = origQuiet }()
	Quiet = false

	var buf bytes.Buffer
	sp := NewSpinner("Initial step", &buf)
	sp.Update("Second step")
	sp.Success("All done")

	out := buf.String()
	if !strings.Contains(out, "Initial step") {
		t.Errorf("expected output to contain 'Initial step', got: %q", out)
	}
	if !strings.Contains(out, "Second step") {
		t.Errorf("expected output to contain 'Second step', got: %q", out)
	}
	if !strings.Contains(out, "All done") {
		t.Errorf("expected output to contain 'All done', got: %q", out)
	}

	// Test Fail
	buf.Reset()
	sp2 := NewSpinner("Starting task", &buf)
	sp2.Fail("Failed task")
	out2 := buf.String()
	if !strings.Contains(out2, "Failed task") {
		t.Errorf("expected output to contain 'Failed task', got: %q", out2)
	}

	// Test Quiet mode
	Quiet = true
	buf.Reset()
	sp3 := NewSpinner("Quiet task", &buf)
	sp3.Update("Quiet update")
	sp3.Success("Quiet done")
	if buf.Len() != 0 {
		t.Errorf("expected quiet mode to produce no output, got: %q", buf.String())
	}
}

func TestUIFormatting(t *testing.T) {
	// Duration formatting
	if got := FmtDuration(500 * time.Microsecond); got != "500µs" {
		t.Errorf("FmtDuration 500µs = %q", got)
	}
	if got := FmtDuration(45 * time.Millisecond); got != "45ms" {
		t.Errorf("FmtDuration 45ms = %q", got)
	}
	if got := FmtDuration(2500 * time.Millisecond); got != "2.50s" {
		t.Errorf("FmtDuration 2.50s = %q", got)
	}

	// Bytes formatting
	if got := FormatBytes(500); got != "500 B" {
		t.Errorf("FormatBytes 500 = %q", got)
	}
	if got := FormatBytes(2048); got != "2.0 KB" {
		t.Errorf("FormatBytes 2048 = %q", got)
	}
	if got := FormatBytes(10 * 1024 * 1024); got != "10.0 MB" {
		t.Errorf("FormatBytes 10MB = %q", got)
	}
}

func TestUIColorHelpers(t *testing.T) {
	var buf bytes.Buffer
	forced := true
	ForcedColor = &forced
	defer func() { ForcedColor = nil }()

	if got := Dim("text", &buf); !strings.Contains(got, "\x1b[90mtext\x1b[0m") {
		t.Errorf("Dim failed: %q", got)
	}
	if got := Faint("text", &buf); !strings.Contains(got, "\x1b[2mtext\x1b[0m") {
		t.Errorf("Faint failed: %q", got)
	}
	if got := Accent("text", &buf); !strings.Contains(got, "\x1b[35mtext\x1b[0m") {
		t.Errorf("Accent failed: %q", got)
	}
	if got := Info("text", &buf); !strings.Contains(got, "\x1b[36mtext\x1b[0m") {
		t.Errorf("Info failed: %q", got)
	}
	if got := Strong("text", &buf); !strings.Contains(got, "\x1b[1mtext\x1b[0m") {
		t.Errorf("Strong failed: %q", got)
	}
}

func TestUIOutputFunctions(t *testing.T) {
	var buf bytes.Buffer
	Quiet = false
	Verbose = true
	defer func() {
		Quiet = false
		Verbose = false
	}()

	VerbosePrompt(1, "test-harness", "Line 1\nLine 2\n", &buf)
	if !strings.Contains(buf.String(), "Line 1") {
		t.Errorf("VerbosePrompt missing line: %s", buf.String())
	}

	buf.Reset()
	Status("ok", "Operation completed", "in 12ms", 20, &buf)
	if !strings.Contains(buf.String(), "Operation completed") {
		t.Errorf("Status missing msg: %s", buf.String())
	}

	buf.Reset()
	count := 42
	Heading("Section Title", &count, &buf)
	if !strings.Contains(buf.String(), "Section Title") || !strings.Contains(buf.String(), "42") {
		t.Errorf("Heading missing title or count: %s", buf.String())
	}

	buf.Reset()
	KV("Key", "Value", 10, &buf)
	if !strings.Contains(buf.String(), "Key") || !strings.Contains(buf.String(), "Value") {
		t.Errorf("KV missing key or val: %s", buf.String())
	}

	buf.Reset()
	Hint("some helpful hint", &buf)
	if !strings.Contains(buf.String(), "hint") {
		t.Errorf("Hint missing content: %s", buf.String())
	}

	buf.Reset()
	Command("px0 --port 8080", &buf)
	if !strings.Contains(buf.String(), "px0") {
		t.Errorf("Command missing content: %s", buf.String())
	}

	buf.Reset()
	Bullet("bullet point", &buf)
	if !strings.Contains(buf.String(), "bullet") {
		t.Errorf("Bullet missing content: %s", buf.String())
	}
}

func BenchmarkFmtDuration(b *testing.B) {
	d := 123456789 * time.Nanosecond
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FmtDuration(d)
	}
}

func BenchmarkFormatBytes(b *testing.B) {
	size := int64(12345678)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FormatBytes(size)
	}
}

func BenchmarkPaint(b *testing.B) {
	var buf bytes.Buffer
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Paint("Sample Text", ColorAccent, true, &buf)
	}
}

