package ui

import (
	"bytes"
	"strings"
	"testing"
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
