package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

func FmtDuration(d time.Duration) string {
	if d < time.Millisecond {
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.2fs", d.Seconds())
}

// Terminal UI helpers inspired by modern CLI tools:
// - Minimal, high-signal, scannable
// - Honors NO_COLOR (https://no-color.org/), TERM=dumb, and TTY detection
// - Quiet mode suppresses non-essential output
const (
	ColorAccent = "35" // magenta
	ColorOk     = "32" // green
	ColorErr    = "31" // red
	ColorWarn   = "33" // yellow
	ColorInfo   = "36" // cyan
	ColorDim    = "90" // bright black / gray
	ColorFaint  = "2"  // faint
)

var (
	ForcedColor *bool
	Quiet       = false
	Verbose     = false
)

func FormatBytes(b int64) string {
	if b < 1024 {
		return fmt.Sprintf("%d B", b)
	}
	if b < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(b)/(1024*1024))
}

// VerbosePrompt prints the prompt lines in verbose mode.
func VerbosePrompt(jobID int64, harness, prompt string, w io.Writer) {
	if !Verbose || Quiet {
		return
	}
	lines := strings.Split(strings.TrimRight(prompt, "\n"), "\n")
	fmt.Fprintf(w, "  %s %s\n", Dim("prompt:", w), Faint(fmt.Sprintf("(%d lines)", len(lines)), w))
	for _, l := range lines {
		fmt.Fprintf(w, "  %s %s\n", Faint("│", w), Dim(l, w))
	}
}

func useColor(w io.Writer) bool {
	if ForcedColor != nil {
		return *ForcedColor
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" {
		return true
	}
	return isTTY(w)
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	if (fi.Mode() & os.ModeCharDevice) == 0 {
		return false
	}
	return checkTerminalFD(f.Fd())
}

func Paint(text, code string, bold bool, w io.Writer) string {
	if !useColor(w) || text == "" {
		return text
	}
	prefix := code
	if bold {
		prefix = "1;" + code
	}
	return fmt.Sprintf("\x1b[%sm%s\x1b[0m", prefix, text)
}

func Dim(t string, w io.Writer) string    { return Paint(t, ColorDim, false, w) }
func Faint(t string, w io.Writer) string  { return Paint(t, ColorFaint, false, w) }
func Accent(t string, w io.Writer) string { return Paint(t, ColorAccent, false, w) }
func Info(t string, w io.Writer) string   { return Paint(t, ColorInfo, false, w) }
func Strong(t string, w io.Writer) string {
	if !useColor(w) || t == "" {
		return t
	}
	return fmt.Sprintf("\x1b[1m%s\x1b[0m", t)
}

func glyphFor(role string, w io.Writer) string {
	switch role {
	case "ok":
		return Paint("✓", ColorOk, false, w)
	case "err":
		return Paint("✗", ColorErr, true, w)
	case "warn":
		return Paint("!", ColorWarn, true, w)
	case "info":
		return Paint("i", ColorInfo, false, w)
	case "step":
		return Paint("›", ColorAccent, true, w)
	default:
		return Faint("·", w)
	}
}

// Status prints a structured status line:
//
//	✓ message  detail
func Status(role, msg, detail string, width int, w io.Writer) {
	if Quiet {
		return
	}
	if width > 0 && len(msg) < width {
		msg = msg + strings.Repeat(" ", width-len(msg))
	}
	g := glyphFor(role, w)
	line := fmt.Sprintf("%s %s", g, msg)
	if detail != "" {
		line += fmt.Sprintf("  %s", Dim(detail, w))
	}
	fmt.Fprintln(w, line)
}

func Heading(title string, count *int, w io.Writer) {
	if Quiet {
		return
	}
	fmt.Fprintln(w)
	line := Strong(title, w)
	if count != nil {
		line += fmt.Sprintf(" %s", Dim(fmt.Sprintf("(%d)", *count), w))
	}
	fmt.Fprintln(w, line)
}

func KV(label, value string, width int, w io.Writer) {
	if Quiet {
		return
	}
	if width <= 0 {
		width = 11
	}
	labelText := fmt.Sprintf("%-*s", width, label)
	fmt.Fprintf(w, "  %s %s\n", Dim(labelText, w), value)
}

func Hint(text string, w io.Writer) {
	if Quiet {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, Dim(text, w))
}

func Command(text string, w io.Writer) {
	if Quiet {
		return
	}
	fmt.Fprintf(w, "  %s\n", Accent(text, w))
}

func Bullet(text string, w io.Writer) {
	if Quiet {
		return
	}
	fmt.Fprintf(w, "  %s %s\n", Faint("·", w), text)
}

// Spinner displays an animated spinner on TTY terminals, or clean step messages on non-TTYs.
type Spinner struct {
	mu      sync.Mutex
	msg     string
	w       io.Writer
	stopCh  chan struct{}
	doneCh  chan struct{}
	stopped bool
	tty     bool
	start   time.Time
}

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

func NewSpinner(msg string, w io.Writer) *Spinner {
	s := &Spinner{
		msg:    msg,
		w:      w,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
		tty:    isTTY(w) && !Quiet,
		start:  time.Now(),
	}
	if Quiet {
		close(s.doneCh)
		return s
	}
	if !s.tty {
		Status("step", msg, "", 0, w)
		close(s.doneCh)
		return s
	}
	go s.run()
	return s
}

func (s *Spinner) run() {
	defer close(s.doneCh)
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	i := 0
	for {
		s.mu.Lock()
		msg := s.msg
		s.mu.Unlock()
		frame := Paint(spinnerFrames[i%len(spinnerFrames)], ColorAccent, true, s.w)
		elapsed := FmtDuration(time.Since(s.start))
		fmt.Fprintf(s.w, "\r\x1b[K%s %s  %s", frame, msg, Dim(elapsed, s.w))
		i++
		select {
		case <-s.stopCh:
			fmt.Fprintf(s.w, "\r\x1b[K")
			return
		case <-ticker.C:
		}
	}
}

func (s *Spinner) Update(msg string) {
	if Quiet {
		return
	}
	s.mu.Lock()
	s.msg = msg
	tty := s.tty
	s.mu.Unlock()
	if !tty {
		Status("step", msg, "", 0, s.w)
	}
}

func (s *Spinner) Stop() {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	tty := s.tty
	s.mu.Unlock()
	if tty {
		close(s.stopCh)
		<-s.doneCh
	}
}

func (s *Spinner) Success(msg string) {
	s.Stop()
	if Quiet {
		return
	}
	elapsed := FmtDuration(time.Since(s.start))
	Status("ok", msg, elapsed, 0, s.w)
}

func (s *Spinner) Fail(msg string) {
	s.Stop()
	if Quiet {
		return
	}
	Status("err", msg, "", 0, s.w)
}
