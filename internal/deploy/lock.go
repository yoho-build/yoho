package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/yoho-dev/yoho/internal/release"
	"github.com/yoho-dev/yoho/internal/remote"
)

// LockInfo describes who holds the deploy/backup lock of an App Destination.
type LockInfo struct {
	Performer string    `json:"performer"`
	Version   string    `json:"version,omitempty"`
	Command   string    `json:"command,omitempty"` // e.g. "deploy", "rollback", "backup"
	Time      time.Time `json:"time"`
}

// LockedError reports a lock held by someone else.
type LockedError struct {
	Dir    string
	Holder LockInfo
}

func (e *LockedError) Error() string {
	h := e.Holder
	who := h.Performer
	if who == "" {
		who = "unknown"
	}
	msg := "locked by " + who
	if h.Command != "" {
		msg += " (" + h.Command
		if h.Version != "" {
			msg += " " + h.Version
		}
		msg += ")"
	}
	if !h.Time.IsZero() {
		msg += " since " + h.Time.UTC().Format(time.RFC3339)
	}
	return msg + "; if the lock is stale, remove " + e.Dir + " on the Server"
}

// LockDir is the mkdir-based lock directory of an App Destination.
func LockDir(app, destination string) string {
	return path.Join(release.AppDir(app, destination), "lock")
}

// AcquireLock takes the App Destination lock (atomic mkdir) and records info
// in lock/info.json. It fails with *LockedError when the lock is held. The
// returned unlock removes the lock; call it on every exit path.
func AcquireLock(ctx context.Context, host remote.Host, app, destination string, info LockInfo) (unlock func(context.Context) error, err error) {
	if info.Time.IsZero() {
		info.Time = time.Now().UTC()
	}
	data, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	dir := LockDir(app, destination)
	q := remote.Quote(dir)
	script := "set -eu\numask 077\nmkdir -p " + remote.Quote(path.Dir(dir)) + "\n" +
		"if mkdir " + q + " 2>/dev/null; then cat > " + q + "/info.json; echo acquired; else echo held; cat " + q + "/info.json 2>/dev/null || true; fi"
	out, err := host.Output(ctx, remote.Cmd{Script: script, Stdin: bytes.NewReader(data)})
	if err != nil {
		return nil, fmt.Errorf("acquire lock on %s: %w", host.Name(), err)
	}
	status, rest, _ := strings.Cut(out, "\n")
	if strings.TrimSpace(status) != "acquired" {
		le := &LockedError{Dir: dir}
		_ = json.Unmarshal([]byte(strings.TrimSpace(rest)), &le.Holder)
		return nil, le
	}
	return func(ctx context.Context) error {
		if err := host.Run(context.WithoutCancel(ctx), remote.Cmd{Script: "rm -rf " + q}); err != nil {
			return fmt.Errorf("release lock on %s: %w", host.Name(), err)
		}
		return nil
	}, nil
}
