// Package operation stores recoverable mutation outcomes on a Server.
package operation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/safepath"
)

type Result struct {
	Outcome   string   `json:"outcome,omitempty"`
	Changed   []string `json:"changed,omitempty"`
	Unchanged []string `json:"unchanged,omitempty"`
}
type Record struct {
	ID       string            `json:"id"`
	Command  string            `json:"command"`
	Started  time.Time         `json:"started_at"`
	State    string            `json:"state"`
	LockHeld bool              `json:"lock_held,omitempty"`
	Services map[string]string `json:"services"`
	Result
}

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func ValidID(id string) bool      { return validID.MatchString(id) }
func Dir(app, dest string) string { return path.Join(release.AppDir(app, dest), "operations") }
func Save(ctx context.Context, h remote.Host, app, dest string, r Record) error {
	if !ValidID(r.ID) {
		return fmt.Errorf("invalid operation ID")
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	dir := Dir(app, dest)
	// Exclusive temporary file; publish atomically. Never follow an existing record link.
	script := safepath.AppLink(app, dest) + safepath.Script("operation record") + safepath.Creation +
		"if [ ! -e " + remote.Quote(release.AppDir(app, dest)) + " ] && [ ! -L " + remote.Quote(release.AppDir(app, dest)) + " ]; then\n" +
		"yoho_make " + remote.Quote(release.AppDir(app, dest)) + "\n(set -C; printf '%s' " + remote.Quote(safepath.Marker(app, dest)) + " > " + remote.Quote(path.Join(release.AppDir(app, dest), safepath.MarkerName)) + ")\nfi\n" +
		"yoho_make " + remote.Quote(dir) + "\n" +
		"yoho_tmp=$(mktemp " + remote.Quote(path.Join(dir, ".operation-XXXXXX")) + ")\ntrap 'rm -f \"$yoho_tmp\"' EXIT HUP INT TERM\ncat > \"$yoho_tmp\"\nchmod 600 \"$yoho_tmp\"\nmv -f \"$yoho_tmp\" " + remote.Quote(path.Join(dir, r.ID+".json")) + "\n"
	return h.Run(ctx, remote.Cmd{Script: script, Stdin: bytes.NewReader(b)})
}
func Read(ctx context.Context, h remote.Host, app, dest, id string) (Record, error) {
	var r Record
	if !ValidID(id) {
		return r, fmt.Errorf("invalid operation ID")
	}
	p := path.Join(Dir(app, dest), id+".json")
	out, err := h.Output(ctx, remote.Cmd{Script: safepath.AppLink(app, dest) + safepath.Script("operation record") + "yoho_file " + remote.Quote(p) + "\ncat " + remote.Quote(p)})
	if err != nil {
		return r, err
	}
	if err = json.Unmarshal([]byte(out), &r); err != nil {
		return r, err
	}
	if r.State == "running" {
		held, err := h.Output(ctx, remote.Cmd{Script: "set -eu\nif [ -d " + remote.Quote(path.Join(release.AppDir(app, dest), "lock")) + " ]; then printf held; fi"})
		if err != nil {
			return r, err
		}
		r.Outcome = "unknown"
		r.LockHeld = held != ""
	}
	return r, nil
}

// Prune retains the newest 50 records, always retaining the current operation.
func Prune(ctx context.Context, h remote.Host, app, dest, current string) error {
	if !ValidID(current) {
		return fmt.Errorf("invalid operation ID")
	}
	dir := remote.Quote(Dir(app, dest))
	// mtime tracks atomic updates. Paths are generated IDs, never shell input.
	script := safepath.AppLink(app, dest) + safepath.Script("operation pruning") + "yoho_path " + dir + "\ncd " + dir + "\n" +
		"ls -1t ./*.json 2>/dev/null | { n=0; while IFS= read -r f; do\n" +
		"case \"$f\" in ./" + current + ".json) continue;; esac\n" +
		"n=$((n + 1)); [ \"$n\" -lt 50 ] || rm -f -- \"$f\"\ndone; }\n"
	return h.Run(ctx, remote.Cmd{Script: script})
}
