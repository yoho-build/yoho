// Package ui renders CLI progress like Kamal (host-prefixed command output,
// a final "Finished all in") with structure on top: timed steps, hints on
// failure, and an NDJSON event stream (--json) for agents and scripts.
package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

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
	w          io.Writer // all output; clears and redraws the live line around writes
	out        io.Writer // the underlying writer
	mode       Mode
	color      bool
	tty        bool       // human output to a terminal (independent of NO_COLOR)
	cols       func() int // terminal width
	live       string     // live line text; "" when none is shown
	Verbose    bool
	warnings   int
	start      time.Time
	progressAt time.Time // last human line; shared by every Progress writer
	depth      int
}

// heartbeatInterval is how long a step may stay silent before a "still
// running" line is printed. Tests shorten it.
var heartbeatInterval = 30 * time.Second

// nowFn is the clock for progress deltas. Tests replace it.
var nowFn = time.Now

// New returns a UI writing to w. Color is enabled when w is a terminal and
// NO_COLOR is unset.
func New(w io.Writer, mode Mode, verbose bool) *UI {
	color, tty := false, false
	cols := func() int { return 100 }
	if f, ok := w.(*os.File); ok && term.IsTerminal(int(f.Fd())) && mode == Human {
		tty = true
		color = os.Getenv("NO_COLOR") == ""
		cols = func() int {
			if c, _, err := term.GetSize(int(f.Fd())); err == nil && c > 0 {
				return c
			}
			return 100
		}
	}
	now := nowFn()
	u := &UI{out: w, mode: mode, color: color, tty: tty, cols: cols, Verbose: verbose, start: now, progressAt: now}
	u.w = liveWriter{u}
	return u
}

// liveWriter keeps the live line on the last row: it is erased before any
// other output and redrawn after. Callers hold u.mu.
type liveWriter struct{ u *UI }

func (l liveWriter) Write(p []byte) (int, error) {
	u := l.u
	if u.live == "" {
		return u.out.Write(p)
	}
	io.WriteString(u.out, "\r\x1b[K")
	n, err := u.out.Write(p)
	u.drawLive()
	return n, err
}

// drawLive rewrites the live line in place, cut to the terminal width.
func (u *UI) drawLive() {
	t := []rune(u.live)
	if w := u.cols(); len(t) > w-1 && w > 1 {
		t = append(t[:w-2], '…')
	}
	io.WriteString(u.out, "\r\x1b[K"+u.paint(dim, string(t)))
}

// LiveStart begins a Service's live line (rewritten in place by LiveUpdate
// and frozen by LiveFinish). Without a terminal, or with -v, it prints text
// as a normal line instead.
func (u *UI) LiveStart(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.mode == JSON {
		return
	}
	if !u.tty || u.Verbose {
		fmt.Fprintln(u.w, u.indent()+text)
		u.stamp()
		return
	}
	u.live = u.indent() + text
	u.drawLive()
}

// LiveUpdate replaces the live line text. It prints nothing without one.
func (u *UI) LiveUpdate(text string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.live == "" {
		return
	}
	u.live = u.indent() + text
	u.drawLive()
}

// LiveFinish freezes the live line as a normal line showing text (or prints
// text when there is no live line). ok colors the leading mark.
func (u *UI) LiveFinish(text string, ok bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.mode == JSON {
		return
	}
	if u.live != "" {
		io.WriteString(u.out, "\r\x1b[K")
		u.live = ""
	}
	mark := u.paint(green, "✓")
	if !ok {
		mark = u.paint(red, "✗")
	}
	fmt.Fprintln(u.w, u.indent()+"  "+mark+" "+text)
	u.stamp()
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

// Event emits a named event with fields in JSON mode and the human line
// otherwise. Use it for results that scripts consume (one event per item).
func (u *UI) Event(kind, human string, fields map[string]any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.mode == JSON {
		f := map[string]any{}
		for k, v := range fields {
			f[k] = v
		}
		u.event(kind, f)
		return
	}
	if human != "" {
		fmt.Fprintln(u.w, u.indent()+human)
		u.stamp()
	}
}

// Warn prints a warning.
func (u *UI) Warn(format string, args ...any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.warnings++
	msg := fmt.Sprintf(format, args...)
	if u.mode == JSON {
		u.event("warning", map[string]any{"message": msg})
		return
	}
	fmt.Fprintln(u.w, u.indent()+u.paint(yellow, "! "+msg))
	u.stamp()
}

// Error reports a failure outside any step (e.g. bad arguments found before
// connecting), with a hint for what to do next.
func (u *UI) Error(err error, hint string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.mode == JSON {
		u.event("error", map[string]any{"error": err.Error(), "hint": hint})
		return
	}
	fmt.Fprintln(u.w, u.indent()+u.paint(red, "✗ "+err.Error()))
	if hint != "" {
		fmt.Fprintln(u.w, u.indent()+"  "+u.paint(yellow, "hint: ")+hint)
	}
	u.stamp()
}

// Finding prints a check result (level error or warning).
func (u *UI) Finding(level, subject, msg string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if level != "error" {
		u.warnings++
	}
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

// Check emits the closing result of `config check` as one JSON event, with
// the warnings (Warn and warning Findings) this UI printed; human
// output already ended with the step line.
func (u *UI) Check(ok bool, errors int, offline bool, services, secretKeys int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.mode == JSON {
		u.event("check", map[string]any{"ok": ok, "errors": errors, "warnings": u.warnings, "offline": offline, "services": services, "secret_keys": secretKeys})
	}
}

// Step is a timed unit of work.
type Step struct {
	u     *UI
	name  string
	host  string
	start time.Time
	tail  *tailBuffer
	done  bool
	stop  chan struct{} // closes when the step finishes (heartbeat)
	// fields are extra step_done fields set by DoneWith.
	fields map[string]any
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
	if !u.Verbose {
		s.stop = make(chan struct{})
		go s.heartbeat(heartbeatInterval, s.stop)
	}
	return s
}

// heartbeat prints a dim "still running" line whenever the UI (any writer)
// has been silent for a full interval while the step runs.
func (s *Step) heartbeat(every time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		u := s.u
		u.mu.Lock()
		if s.done {
			u.mu.Unlock()
			return
		}
		if u.live != "" || nowFn().Sub(u.progressAt) < every {
			u.mu.Unlock()
			continue
		}
		msg := "… still running"
		if l := lastNonEmpty(s.tail.lines()); l != "" {
			r := []rune(l)
			if len(r) > 80 {
				l = string(r[:79]) + "…"
			}
			msg += " (last: " + l + ")"
		}
		fmt.Fprintln(u.w, u.indent()+"    "+u.paint(dim, msg+" "+fmtDur(time.Since(s.start))))
		u.stamp()
		u.mu.Unlock()
	}
}

func lastNonEmpty(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
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

// DoneWith is Done plus extra fields on the JSON step_done event (for
// example "transfers"). Reserved keys (event, time, step, host, duration_ms,
// detail) keep their usual values.
func (s *Step) DoneWith(detail string, fields map[string]any) {
	s.fields = fields
	s.finish(nil, detail)
}

// Fail marks the step failed; hint tells the user what to do next.
func (s *Step) Fail(err error, hint string) {
	s.finish(err, hint)
}

// Waiting reports that the step is blocked on something outside Yoho, such as
// a password manager approval prompt. fields (names only, never values) join
// the JSON step_wait event.
func (s *Step) Waiting(msg string, fields map[string]any) {
	s.u.waiting(s, msg, fields)
}

// Waiting is Step.Waiting for work that has no step of its own.
func (u *UI) Waiting(msg string, fields map[string]any) { u.waiting(nil, msg, fields) }

func (u *UI) waiting(s *Step, msg string, fields map[string]any) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if s != nil && s.done {
		return
	}
	if u.mode == JSON {
		f := map[string]any{"message": msg}
		if s != nil {
			f["step"], f["host"] = s.name, s.host
		}
		for k, v := range fields {
			f[k] = v
		}
		u.event("step_wait", f)
		return
	}
	fmt.Fprintln(u.w, u.indent()+"  "+u.paint(yellow, "… "+msg))
	u.stamp()
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
	s.stopHeartbeat()
	if u.mode == JSON {
		u.event("step_skip", map[string]any{"step": s.name, "host": s.host, "reason": reason})
		return
	}
	fmt.Fprintln(u.w, u.indent()+u.paint(dim, "- "+s.name+" (skipped: "+reason+")"))
	u.stamp()
}

// stopHeartbeat ends the heartbeat goroutine. Caller holds u.mu.
func (s *Step) stopHeartbeat() {
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
}

func (s *Step) finish(err error, extra string) {
	u := s.u
	u.mu.Lock()
	defer u.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	s.stopHeartbeat()
	d := time.Since(s.start)
	if u.mode == JSON {
		f := map[string]any{"step": s.name, "host": s.host, "duration_ms": d.Milliseconds()}
		if err != nil {
			f["error"] = err.Error()
			var facts interface{ FailureFacts() any }
			if errors.As(err, &facts) {
				f["failure_facts"] = facts.FailureFacts()
			}
			var coded interface{ ErrorCode() string }
			if errors.As(err, &coded) {
				if code := coded.ErrorCode(); code != "" {
					f["code"] = code
				}
			}
			f["hint"] = extra
			f["output_tail"] = s.tail.lines()
			u.event("step_fail", f)
		} else {
			for k, v := range s.fields {
				if _, taken := f[k]; !taken {
					f[k] = v
				}
			}
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

// sanitize removes terminal control from one line of command output (Hook
// output, a drill's verify output from restored data): C0 controls except
// tab, DEL, C1 controls and escape sequences (CSI, OSC, DCS, SOS, PM, APC
// and two-byte ESC sequences), so output cannot recolor, move the cursor,
// retitle or rewrite the terminal. Colors the UI adds are painted after.
func sanitize(l string) string {
	clean := true
	for i := 0; i < len(l); i++ {
		if c := l[i]; (c < 0x20 && c != '\t') || c == 0x7f || c == 0xc2 || (c >= 0x80 && c <= 0x9f) {
			clean = false
			break
		}
	}
	if clean {
		return l
	}
	var b strings.Builder
	b.Grow(len(l))
	// skipString skips a control string body (OSC, DCS, SOS, PM, APC) up to
	// and including its terminator: BEL (OSC), ESC \ or C1 ST.
	skipString := func(i int) int {
		for i < len(l) {
			r, n := utf8.DecodeRuneInString(l[i:])
			switch {
			case r == 0x07 || r == 0x9c:
				return i + n
			case r == 0x1b && i+1 < len(l) && l[i+1] == '\\':
				return i + 2
			case r == 0x1b:
				return i // an unterminated string ends at the next ESC
			}
			i += n
		}
		return i
	}
	// skipCSI skips parameter and intermediate bytes and the final byte.
	skipCSI := func(i int) int {
		for i < len(l) {
			c := l[i]
			i++
			if c >= 0x40 && c <= 0x7e {
				return i
			}
			if c < 0x20 || c >= 0x7f {
				return i - 1 // malformed: drop the introducer only
			}
		}
		return i
	}
	for i := 0; i < len(l); {
		r, n := utf8.DecodeRuneInString(l[i:])
		if r == utf8.RuneError && n == 1 {
			if c := l[i]; c >= 0x80 && c <= 0x9f {
				i++ // raw 8-bit C1 byte
				continue
			}
			b.WriteByte(l[i])
			i++
			continue
		}
		switch {
		case r == 0x1b:
			i += n
			if i >= len(l) {
				continue
			}
			switch c := l[i]; {
			case c == '[':
				i = skipCSI(i + 1)
			case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
				i = skipString(i + 1)
			case c >= 0x20 && c <= 0x2f: // nF: intermediates, then a final byte
				for i < len(l) && l[i] >= 0x20 && l[i] <= 0x2f {
					i++
				}
				if i < len(l) && l[i] >= 0x30 && l[i] <= 0x7e {
					i++
				}
			case c >= 0x30 && c <= 0x7e:
				i++
			}
		case r == 0x9b:
			i = skipCSI(i + n)
		case r == 0x9d || r == 0x90 || r == 0x98 || r == 0x9e || r == 0x9f:
			i = skipString(i + n)
		case (r < 0x20 && r != '\t') || (r >= 0x7f && r <= 0x9f):
			i += n
		default:
			b.WriteString(l[i : i+n])
			i += n
		}
	}
	return b.String()
}

func (w *stepWriter) line(l string) {
	l = sanitize(l)
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
	l = sanitize(l)
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
