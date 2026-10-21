package telemetry

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func TestIsOptedOut(t *testing.T) {
	if !isOptedOut(true) {
		t.Fatal("expected isOptedOut(true) to be true")
	}

	os.Setenv("DO_NOT_TRACK", "1")
	if !isOptedOut(false) {
		t.Fatal("expected isOptedOut to respect DO_NOT_TRACK=1")
	}
	os.Unsetenv("DO_NOT_TRACK")

	for _, val := range []string{"0", "false", "off", "no"} {
		os.Setenv("PX0_TELEMETRY", val)
		if !isOptedOut(false) {
			t.Fatalf("expected isOptedOut to respect PX0_TELEMETRY=%s", val)
		}
	}
	os.Unsetenv("PX0_TELEMETRY")

	if isOptedOut(false) {
		t.Fatal("expected isOptedOut to be false when no opt-out is set")
	}
}

func TestFilesBucket(t *testing.T) {
	tests := []struct {
		files    int
		expected string
	}{
		{0, "<100"},
		{99, "<100"},
		{100, "100-500"},
		{499, "100-500"},
		{500, "500-2.5k"},
		{2499, "500-2.5k"},
		{2500, "2.5k-10k"},
		{9999, "2.5k-10k"},
		{10000, "10k-50k"},
		{49999, "10k-50k"},
		{50000, "50k-100k"},
		{99999, "50k-100k"},
		{100000, ">100k"},
		{500000, ">100k"},
	}

	for _, tt := range tests {
		actual := FilesBucket(tt.files)
		if actual != tt.expected {
			t.Errorf("FilesBucket(%d) = %q, want %q", tt.files, actual, tt.expected)
		}
	}
}

func TestTelemetrySessionLifecycle(t *testing.T) {
	var mu sync.Mutex
	var received []map[string]any

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/capture/" {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		mu.Lock()
		received = append(received, payload)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	os.Setenv("PX0_POSTHOG_KEY", "phc_test_key_xyz")
	os.Setenv("PX0_POSTHOG_HOST", server.URL)
	defer os.Unsetenv("PX0_POSTHOG_KEY")
	defer os.Unsetenv("PX0_POSTHOG_HOST")

	tel := New("", "0.1.0", false)
	if !tel.enabled {
		t.Fatal("expected telemetry to be enabled with key set")
	}

	// 1. Session start
	tel.Track("session_started", map[string]any{
		"files_bucket": "100-1k",
		"has_git":      true,
		"has_lsp":      false,
	})

	// Brief pause to ensure duration >= 0
	time.Sleep(10 * time.Millisecond)

	// 2. Session stop
	tel.Close("normal")

	mu.Lock()
	defer mu.Unlock()

	if len(received) != 3 {
		t.Fatalf("expected 3 events (session_started, session_ended, session_stopped), got %d", len(received))
	}

	// Check session_started
	evtStart := received[0]
	if evtStart["event"] != "session_started" {
		t.Errorf("expected event session_started, got %v", evtStart["event"])
	}
	propsStart := evtStart["properties"].(map[string]any)
	if propsStart["files_bucket"] != "100-1k" {
		t.Errorf("unexpected files_bucket: %v", propsStart["files_bucket"])
	}
	if propsStart["distinct_id"] == "" {
		t.Error("expected non-empty distinct_id")
	}

	// Check session_ended
	evtEnded := received[1]
	if evtEnded["event"] != "session_ended" {
		t.Errorf("expected session_ended, got %v", evtEnded["event"])
	}
	propsEnded := evtEnded["properties"].(map[string]any)
	if propsEnded["exit_reason"] != "normal" {
		t.Errorf("expected exit_reason=normal, got %v", propsEnded["exit_reason"])
	}
	if _, ok := propsEnded["duration_seconds"]; !ok {
		t.Errorf("expected duration_seconds in session_ended properties")
	}

	// Check session_stopped
	evtStop := received[2]
	if evtStop["event"] != "session_stopped" {
		t.Errorf("expected session_stopped, got %v", evtStop["event"])
	}
	propsStop := evtStop["properties"].(map[string]any)
	if propsStop["exit_reason"] != "normal" {
		t.Errorf("expected exit_reason=normal, got %v", propsStop["exit_reason"])
	}
	if _, ok := propsStop["duration_seconds"]; !ok {
		t.Errorf("expected duration_seconds in session_stopped properties")
	}

	// Verify PostHog built-in $session_id groups the events together
	startSessID, _ := propsStart["$session_id"].(string)
	endedSessID, _ := propsEnded["$session_id"].(string)
	stopSessID, _ := propsStop["$session_id"].(string)
	if startSessID == "" {
		t.Error("expected non-empty $session_id")
	}
	if startSessID != endedSessID || startSessID != stopSessID {
		t.Errorf("expected matching $session_id across session events: start=%q ended=%q stop=%q",
			startSessID, endedSessID, stopSessID)
	}
}

func TestTelemetryDisabledWithoutKey(t *testing.T) {
	os.Unsetenv("PX0_POSTHOG_KEY")
	tel := New("", "0.1.0", false)
	if tel.enabled {
		t.Fatal("expected telemetry disabled without key")
	}
	tel.Track("session_started", nil)
	tel.Close("normal")
	// Double close is safe
	tel.Close("normal")
}

func TestGetOrGenerateDistinctID(t *testing.T) {
	tempHome := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tempHome)
	defer os.Setenv("HOME", origHome)

	// First call generates new ID and saves to tempHome/.px0/anonymous_id
	id1 := getOrGenerateDistinctID()
	if len(id1) < 16 {
		t.Fatalf("expected >= 16 byte id, got %q", id1)
	}

	// Second call reads it back
	id2 := getOrGenerateDistinctID()
	if id1 != id2 {
		t.Errorf("expected cached id %q, got %q", id1, id2)
	}

	// Corrupt file (too short) -> regenerates
	idPath := tempHome + "/.px0/anonymous_id"
	_ = os.WriteFile(idPath, []byte("short"), 0644)
	id3 := getOrGenerateDistinctID()
	if id3 == "short" || len(id3) < 16 {
		t.Errorf("expected regenerated id, got %q", id3)
	}

	// UUID generator
	u := generateUUID()
	if len(u) != 36 {
		t.Errorf("expected 36 char UUID, got %q", u)
	}
}

func TestTelemetryServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	os.Setenv("PX0_POSTHOG_KEY", "phc_test_err_key")
	os.Setenv("PX0_POSTHOG_HOST", server.URL)
	defer os.Unsetenv("PX0_POSTHOG_KEY")
	defer os.Unsetenv("PX0_POSTHOG_HOST")

	tel := New("", "0.1.0", false)
	tel.Track("test_event", nil)
	tel.Close("error_test")
}

func BenchmarkFilesBucket(b *testing.B) {
	counts := []int{10, 250, 1500, 5000, 25000, 75000, 200000}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FilesBucket(counts[i%len(counts)])
	}
}

func BenchmarkTelemetryTrackDisabled(b *testing.B) {
	tel := New("", "0.1.0", false)
	props := map[string]any{"count": 42}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		tel.Track("action", props)
	}
}

