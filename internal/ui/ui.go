// Package ui renders CLI progress like Kamal (host-prefixed command output,
// a final "Finished all in") with structure on top: timed steps, hints on
// failure, and an NDJSON event stream (--json) for agents and scripts.
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"
)

// Mode selects the output format.
type Mode int

const (
	Human Mode = iota
	JSON
)

// UI writes progress. Safe for concurrent use.
type UI struct {
	mu         sync.Mutex
	w          io.Writer
	mode       Mode
	color      bool
	Verbose    bool
	start      time.Time
	progressAt time.Time // last human line; shared by every Progress writer
	depth      int
}

// nowFn is the clock for progress deltas. Tests replace it.
var nowFn = time.Now

// New returns a UI writing to w. Color is enabled when w is a terminal and
// NO_COLOR is unset.
func New(w io.Writer, mode Mode, verbose bool) *UI {
	color := false
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) && os.Getenv("NO_COLOR") == "" && mode == Human {
		color = true
	}
	now := nowFn()
	return &UI{w: w, mode: mode, color: color, Verbose: verbose, start: now, progressAt: now}
}

// stamp marks a human line so the next progress delta starts here.
// Caller holds u.mu.
func (u *UI) stamp() { u.progressAt = nowFn() }

// Discard is a UI that prints nothing (tests).
func Discard() *UI { return New(io.Discard, Human, false) }

const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	blue   = "\033[34m"
	cyan   = "\033[36m"
)

func (u *UI) paint(code, s string) string {
	if !u.color {
		return s
	}
	return code + s + reset
}

func (u *UI) event(kind string, fields map[string]any) {
	fields["event"] = kind
	fields["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, _ := json.Marshal(fields)
	fmt.Fprintln(u.w, string(b))
}

func (u *UI) indent() string { return strings.Repeat("  ", u.depth) }

// Title prints a command header, e.g. "Deploying qa-notes to production".
func (u *UI) Title(format string, args ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	if u.mode == JSON {
		u.event("title", map[string]any{"message": msg})
		return
	}
	fmt.Fprintln(u.w, u.paint(bold, msg))
	u.stamp()
}

// Info prints a plain line.
func (u *UI) Info(format string, args ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	if u.mode == JSON {
		u.event("info", map[string]any{"message": msg})
		return
	}
	fmt.Fprintln(u.w, u.indent()+msg)
	u.stamp()
}

// Warn prints a warning.
func (u *UI) Warn(format string, args ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	msg := fmt.Sprintf(format, args...)
	if u.mode == JSON {
		u.event("warning", map[string]any{"message": msg})
		return
	}
	fmt.Fprintln(u.w, u.indent()+u.paint(yellow, "! "+msg))
	u.stamp()
}

// Finding prints a check result (level error or warning).
func (u *UI) Finding(level, subject, msg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.mode == JSON {
		u.event("finding", map[string]any{"level": level, "subject": subject, "message": msg})
		return
	}
	where := ""
	if subject != "" {
		where = u.paint(cyan, "["+subject+"]") + " "
	}
	if level == "error" {
		fmt.Fprintln(u.w, u.indent()+"  "+u.paint(red, "✗ ")+where+msg)
	} else {
		fmt.Fprintln(u.w, u.indent()+"  "+u.paint(yellow, "! ")+where+msg)
	}
	u.stamp()
}

// Step is a timed unit of work.
type Step struct {
	u     *UI
	name  string
	host  string
	start time.Time
	tail  *tailBuffer
	done  bool
}

// Step starts a step. host may be empty for local work.
func (u *UI) Step(host, format string, args ...any) *Step {
	u.mu.Lock()
	defer u.mu.Unlock()
	s := &Step{u: u, name: fmt.Sprintf(format, args...), host: host, start: time.Now(), tail: &tailBuffer{max: 8}}
	if u.mode == JSON {
		u.event("step_start", map[string]any{"step": s.name, "host": host})
		return s
	}
	fmt.Fprintln(u.w, u.indent()+u.paint(blue, "▸ ")+s.label())
	u.stamp()
	return s
}

func (s *Step) label() string {
	if s.host == "" {
		return s.name
	}
	return s.u.paint(cyan, "["+s.host+"]") + " " + s.name
}

// Output returns a writer for command output belonging to this step. In
// verbose mode lines stream live with the host prefix; otherwise the last
// lines are kept and shown only if the step fails.
func (s *Step) Output() io.Writer { return &stepWriter{s: s} }

// Done marks the step successful, with an optional detail ("3 layers").
func (s *Step) Done(detail ...string) {
	s.finish(nil, strings.Join(detail, " "))
}

// Fail marks the step failed; hint tells the user what to do next.
func (s *Step) Fail(err error, hint string) {
	s.finish(err, hint)
}

// Skip marks the step as skipped with a reason.
func (s *Step) Skip(reason string) {
	u := s.u
	u.mu.Lock()
	defer u.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	if u.mode == JSON {
		u.event("step_skip", map[string]any{"step": s.name, "host": s.host, "reason": reason})
		return
	}
	fmt.Fprintln(u.w, u.indent()+u.paint(dim, "- "+s.name+" (skipped: "+reason+")"))
	u.stamp()
}

func (s *Step) finish(err error, extra string) {
	u := s.u
	u.mu.Lock()
	defer u.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	d := time.Since(s.start)
	if u.mode == JSON {
		f := map[string]any{"step": s.name, "host": s.host, "duration_ms": d.Milliseconds()}
		if err != nil {
			f["error"] = err.Error()
			f["hint"] = extra
			f["output_tail"] = s.tail.lines()
			u.event("step_fail", f)
		} else {
			f["detail"] = extra
			u.event("step_done", f)
		}
		return
	}
	if err != nil {
		fmt.Fprintln(u.w, u.indent()+u.paint(red, "✗ ")+s.label()+" "+u.paint(dim, fmtDur(d)))
		if !u.Verbose {
			for _, l := range s.tail.lines() {
				fmt.Fprintln(u.w, u.indent()+"    "+u.paint(dim, l))
			}
		}
		fmt.Fprintln(u.w, u.indent()+"  "+u.paint(red, err.Error()))
		if extra != "" {
			fmt.Fprintln(u.w, u.indent()+"  "+u.paint(yellow, "hint: ")+extra)
		}
		u.stamp()
		return
	}
	line := u.indent() + u.paint(green, "✓ ") + s.label()
	if extra != "" {
		line += " " + u.paint(dim, "· "+extra)
	}
	fmt.Fprintln(u.w, line+" "+u.paint(dim, fmtDur(d)))
	u.stamp()
}

// Finished prints Kamal's closing line with total time, or the failure.
func (u *UI) Finished(err error, summary string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	d := time.Since(u.start)
	if u.mode == JSON {
		f := map[string]any{"duration_ms": d.Milliseconds(), "summary": summary}
		if err != nil {
			f["error"] = err.Error()
		}
		u.event("finished", f)
		return
	}
	if err != nil {
		fmt.Fprintln(u.w, u.paint(red+bold, "Failed after "+fmtDur(d)))
		u.stamp()
		return
	}
	if summary != "" {
		fmt.Fprintln(u.w, u.paint(bold, summary))
	}
	fmt.Fprintln(u.w, u.paint(dim, "Finished all in "+fmtDur(d)))
	u.stamp()
}

// Table prints aligned rows (human) or one event per row (JSON).
func (u *UI) Table(header []string, rows [][]string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.mode == JSON {
		for _, r := range rows {
			f := map[string]any{}
			for i, h := range header {
				if i < len(r) {
					f[strings.ToLower(strings.ReplaceAll(h, " ", "_"))] = r[i]
				}
			}
			u.event("row", f)
		}
		return
	}
	widths := make([]int, len(header))
	for i, h := range header {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) && len([]rune(c)) > widths[i] {
				widths[i] = len([]rune(c))
			}
		}
	}
	pr := func(cells []string, style string) {
		var b strings.Builder
		for i, c := range cells {
			if i >= len(widths) {
				break
			}
			b.WriteString(c)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-len([]rune(c))+2))
			}
		}
		fmt.Fprintln(u.w, u.paint(style, b.String()))
	}
	pr(header, bold)
	for _, r := range rows {
		pr(r, "")
	}
	u.stamp()
}

func fmtDur(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

type stepWriter struct {
	s   *Step
	buf []byte
}

func (w *stepWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := strings.IndexByte(string(w.buf), '\n')
		if i < 0 {
			break
		}
		w.line(strings.TrimRight(string(w.buf[:i]), "\r"))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

func (w *stepWriter) line(l string) {
	s := w.s
	s.tail.add(l)
	u := s.u
	if !u.Verbose {
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.mode == JSON {
		u.event("output", map[string]any{"step": s.name, "host": s.host, "line": l})
		return
	}
	prefix := "    "
	if s.host != "" {
		prefix += u.paint(cyan, s.host) + " "
	}
	fmt.Fprintln(u.w, u.indent()+prefix+u.paint(dim, l))
	u.stamp()
}

type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []string
}

func (t *tailBuffer) add(l string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, l)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
}

func (t *tailBuffer) lines() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.buf...)
}

// Progress returns a writer that renders free-form progress lines from
// runtimes ("[server] message") as sub-steps with elapsed time.
//
// The elapsed time is since the previous human line on this UI, shared by
// every writer. A hook writer is created before the build, so a private
// clock would print the whole build on the post-deploy line.
func (u *UI) Progress() io.Writer { return &progressWriter{u: u} }

type progressWriter struct {
	u   *UI
	buf []byte
}

func (p *progressWriter) Write(b []byte) (int, error) {
	p.buf = append(p.buf, b...)
	for {
		i := strings.IndexByte(string(p.buf), '\n')
		if i < 0 {
			break
		}
		p.line(strings.TrimRight(string(p.buf[:i]), "\r"))
		p.buf = p.buf[i+1:]
	}
	return len(b), nil
}

func (p *progressWriter) line(l string) {
	if strings.TrimSpace(l) == "" {
		return
	}
	u := p.u
	u.mu.Lock()
	defer u.mu.Unlock()
	now := nowFn()
	if u.progressAt.IsZero() {
		u.progressAt = u.start
	}
	d := now.Sub(u.progressAt)
	u.progressAt = now
	host, msg := "", l
	if strings.HasPrefix(l, "[") {
		if j := strings.Index(l, "] "); j > 0 {
			host, msg = l[1:j], l[j+2:]
		}
	}
	if u.mode == JSON {
		u.event("progress", map[string]any{"host": host, "message": msg})
		return
	}
	prefix := ""
	if host != "" {
		prefix = u.paint(cyan, "["+host+"]") + " "
	}
	fmt.Fprintln(u.w, u.indent()+"  "+u.paint(dim, "·")+" "+prefix+msg+" "+u.paint(dim, "+"+fmtDur(d)))
}
