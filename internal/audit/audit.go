// Package audit is the operator audit log of an App Destination: an
// append-only JSON-lines file on the Server, <root>/audit/<app>/<dest>.log
// (outside the App directory, so it survives `yoho remove`), recording who ran which mutating command, with what, and how it ended.
//
// Every command writes a "started" record before it changes anything and a
// "finished" record after, paired by Run, so a killed command still leaves a
// trace. Writes are best-effort: the CLI warns and carries on.
//
// Members of the Gate (ADR 0011) have their own log, access/audit.log, in
// the gate.AuditEntry format (member, verb, exit, seconds). This format keeps
// the same field names where the meaning is the same (time, app,
// destination, args, exit, error, seconds) and adds performer, command,
// version, run, phase, yoho and via.
package audit

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/safepath"
)

// Phases of a Record.
const (
	Started  = "started"
	Finished = "finished"
)

// Via values: where the command came from.
const (
	ViaCLI  = "cli"
	ViaGate = "gate"
)

// Record is one line of the audit log.
type Record struct {
	Time        time.Time `json:"time"` // UTC
	Run         string    `json:"run"`  // pairs the started and finished records of one command
	Phase       string    `json:"phase"`
	Performer   string    `json:"performer"` // user@host, as in Release.Performer
	Command     string    `json:"command"`   // e.g. "deploy", "app exec", "lock acquire"
	App         string    `json:"app"`
	Destination string    `json:"destination"`
	Version     string    `json:"version,omitempty"`
	// Args summarizes the invocation: flags that were set, positional
	// arguments and, for app exec/run, the command argv. Redacted: never a
	// secret value (secrets rotate lists key names only).
	Args    map[string]any `json:"args,omitempty"`
	Exit    *int           `json:"exit,omitempty"` // finished only
	Error   string         `json:"error,omitempty"`
	Seconds float64        `json:"seconds,omitempty"` // finished only
	Yoho    string         `json:"yoho,omitempty"`    // Yoho CLI version
	Via     string         `json:"via"`
}

// NewRun returns a random run id.
func NewRun() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const (
	// maxArgLen caps one recorded argument, maxArgs the recorded argv, and
	// maxLine the whole record, so a line stays one small atomic append.
	maxArgLen = 512
	maxArgs   = 64
	maxLine   = 8192
	maxError  = 300
)

// Line encodes r as one JSON line without the newline, dropping Args when
// the record would exceed maxLine.
func (r Record) Line() ([]byte, error) {
	r.Error = Truncate(firstLine(r.Error), maxError)
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(b) > maxLine {
		r.Args = map[string]any{"truncated": true}
		if b, err = json.Marshal(r); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// Truncate shortens s to n bytes (cut at a rune boundary).
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n] + "…"
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}

// ArgvArgs records an exec/run command: the Service and the argv, each
// argument cut to maxArgLen and the list to maxArgs. Redact first.
func ArgvArgs(service string, argv []string) map[string]any {
	out := make([]string, 0, min(len(argv), maxArgs+1))
	for i, a := range argv {
		if i == maxArgs {
			out = append(out, fmt.Sprintf("… (%d more)", len(argv)-maxArgs))
			break
		}
		out = append(out, Truncate(a, maxArgLen))
	}
	return map[string]any{"service": service, "argv": out}
}

// Dir is the audit directory of an App: <root>/audit/<app>. It lives
// outside the App directory so `yoho remove` keeps the log, and the first
// deploy can write before the App directory exists.
func Dir(app string) string { return path.Join(release.Root, "audit", app) }

// Path is the audit log of an App Destination: <root>/audit/<app>/<dest>.log.
func Path(app, destination string) string { return path.Join(Dir(app), destination+".log") }

// legacyDir is where earlier versions kept audit.log: the App directory.
func legacyDir(app, destination string) string { return release.AppDir(app, destination) }

// MaxBytes is the size at which the log is rotated to <dest>.log.1.
var MaxBytes int64 = 5 << 20

// Keep is how many rotated files are kept (<dest>.log.1, <dest>.log.2).
const Keep = 2

// AppendScript returns the remote script that appends the record read from
// stdin (one line) to the audit log of app/destination. Nothing but paths
// and numbers is in the script; the record travels on stdin.
//
// The audit directories (<root>/audit, <root>/audit/<app>) are created
// 0700 on the first write, one component at a time and checked with the
// same trust functions as the App directory, so a first deploy gets its
// "started" record; the log is created 0600 and never followed through a
// link. At MaxBytes it rotates before the append. The append is one
// printf >> write, so concurrent commands do not interleave lines.
//
// A legacy <app dir>/audit.log (and .1, .2) is appended into the new log
// once and removed. A legacy file that fails the trust checks is left alone.
func AppendScript(app, destination string) string {
	log := remote.Quote(Path(app, destination))
	old := legacyDir(app, destination)
	var b strings.Builder
	b.WriteString(safepath.AppLink(app, destination) + safepath.Script("audit log") + safepath.Creation)
	b.WriteString("yoho_make " + remote.Quote(Dir(app)) + "\n")
	b.WriteString("yoho_log=" + log + "\n")
	b.WriteString("yoho_line=$(cat)\n")
	b.WriteString("if [ -e \"$yoho_log\" ] || [ -L \"$yoho_log\" ]; then yoho_file \"$yoho_log\"; else : > \"$yoho_log\"; yoho_file \"$yoho_log\"; fi\n")
	// Migration of the legacy log, in a subshell: any refusal skips it.
	b.WriteString("yoho_old=" + remote.Quote(old) + "\n")
	b.WriteString("(\n for yoho_f in audit.log.2 audit.log.1 audit.log; do\n")
	b.WriteString("  [ -e \"$yoho_old/$yoho_f\" ] || [ -L \"$yoho_old/$yoho_f\" ] || continue\n")
	b.WriteString("  yoho_file \"$yoho_old/$yoho_f\"\n  cat \"$yoho_old/$yoho_f\" >> \"$yoho_log\"\n  rm -f \"$yoho_old/$yoho_f\"\n")
	b.WriteString(" done\n) >/dev/null 2>&1 || true\n")
	b.WriteString("yoho_size=$(wc -c < \"$yoho_log\" | tr -d ' ')\n")
	b.WriteString("if [ \"$yoho_size\" -ge " + strconv.FormatInt(MaxBytes, 10) + " ]; then\n")
	b.WriteString(" rm -f \"$yoho_log." + strconv.Itoa(Keep) + "\"\n")
	for i := Keep - 1; i >= 1; i-- {
		from := "\"$yoho_log." + strconv.Itoa(i) + "\""
		b.WriteString(" [ ! -e " + from + " ] || mv -f " + from + " \"$yoho_log." + strconv.Itoa(i+1) + "\"\n")
	}
	b.WriteString(" mv -f \"$yoho_log\" \"$yoho_log.1\"\n")
	b.WriteString("fi\n")
	b.WriteString("printf '%s\\n' \"$yoho_line\" >> \"$yoho_log\"\n")
	return b.String()
}

// Append writes r to the audit log on h.
func Append(ctx context.Context, h remote.Host, r Record) error {
	line, err := r.Line()
	if err != nil {
		return err
	}
	if err := h.Run(ctx, remote.Cmd{Script: AppendScript(r.App, r.Destination), Stdin: bytes.NewReader(line)}); err != nil {
		return fmt.Errorf("audit log on %s: %w", h.Name(), err)
	}
	return nil
}

// ReadScript prints the last lines of the log files that exist and are
// trusted, oldest first: an unmigrated legacy <app dir>/audit.log{.2,.1,}
// (skipped when it fails the trust checks), then <dest>.log.2, .1 and
// <dest>.log. Read-only: it does not migrate.
func ReadScript(app, destination string, lines int) string {
	var b strings.Builder
	n := strconv.Itoa(lines)
	b.WriteString(safepath.AppLink(app, destination) + safepath.Script("audit log"))
	b.WriteString("yoho_old=" + remote.Quote(legacyDir(app, destination)) + "\n")
	b.WriteString("(\n for yoho_f in audit.log.2 audit.log.1 audit.log; do\n")
	b.WriteString("  [ -e \"$yoho_old/$yoho_f\" ] || continue\n")
	b.WriteString("  yoho_file \"$yoho_old/$yoho_f\"\n  tail -n " + n + " \"$yoho_old/$yoho_f\"\n done\n) 2>/dev/null || true\n")
	log := Path(app, destination)
	for _, f := range []string{log + ".2", log + ".1", log} {
		p := remote.Quote(f)
		b.WriteString("if [ -e " + p + " ]; then yoho_file " + p + "; tail -n " + n + " " + p + "; fi\n")
	}
	return b.String()
}

// Entry is one parsed audit line and its raw text.
type Entry struct {
	Record
	Raw string
}

// Parse decodes audit lines, skipping lines that are not records.
func Parse(data string) []Entry {
	var out []Entry
	for _, l := range strings.Split(data, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		var r Record
		if json.Unmarshal([]byte(l), &r) != nil || r.Command == "" || r.Time.IsZero() {
			continue
		}
		out = append(out, Entry{Record: r, Raw: l})
	}
	return out
}

// Read returns the entries on h, oldest first.
func Read(ctx context.Context, h remote.Host, app, destination string, lines int) ([]Entry, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: ReadScript(app, destination, lines)})
	if err != nil {
		return nil, fmt.Errorf("read audit log on %s: %w", h.Name(), err)
	}
	return Parse(out), nil
}

// Run is a command with its started and finished records.
type Run struct {
	Started  *Entry
	Finished *Entry
}

// Start is when the command began.
func (r Run) Start() time.Time { return r.First().Time }

// First is the started record, or the finished one when the started line
// was rotated away.
func (r Run) First() Entry {
	if r.Started != nil {
		return *r.Started
	}
	return *r.Finished
}

// Result is "ok", "exit N: error", or "unfinished" (killed or still running).
func (r Run) Result() string {
	if r.Finished == nil {
		return "unfinished"
	}
	f := r.Finished
	code := 0
	if f.Exit != nil {
		code = *f.Exit
	}
	if code == 0 {
		return "ok"
	}
	s := "exit " + strconv.Itoa(code)
	if f.Error != "" {
		s += ": " + f.Error
	}
	return s
}

// Duration is the finished record's seconds.
func (r Run) Duration() (time.Duration, bool) {
	if r.Finished == nil {
		return 0, false
	}
	return time.Duration(r.Finished.Seconds * float64(time.Second)), true
}

// Pair groups entries into runs by Run id, ordered by start time. Entries
// without a run id are runs of their own.
func Pair(entries []Entry) []Run {
	byID := map[string]*Run{}
	var runs []*Run
	for i := range entries {
		e := &entries[i]
		var r *Run
		if e.Run != "" {
			r = byID[e.Run]
		}
		if r == nil {
			r = &Run{}
			runs = append(runs, r)
			if e.Run != "" {
				byID[e.Run] = r
			}
		}
		if e.Phase == Finished {
			r.Finished = e
		} else {
			r.Started = e
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].Start().Before(runs[j].Start()) })
	out := make([]Run, len(runs))
	for i, r := range runs {
		out[i] = *r
	}
	return out
}

// Select keeps the last n runs that started at or after since (zero: all).
func Select(runs []Run, since time.Time, n int) []Run {
	var out []Run
	for _, r := range runs {
		if !since.IsZero() && r.Start().Before(since) {
			continue
		}
		out = append(out, r)
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}
