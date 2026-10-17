package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/safepath"
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

// MigrationPendingError reports an app_dir migration journal: a move of the
// App directory is in progress or was interrupted. Lock takers refuse until
// the next deploy or apply resumes it.
type MigrationPendingError struct {
	Journal string
	Content string
}

func (e *MigrationPendingError) Error() string {
	msg := "App directory migration in progress or interrupted (" + e.Journal
	if c := strings.TrimSpace(e.Content); c != "" {
		msg += ": " + c
	}
	return msg + "); rerun yoho deploy or yoho apply to finish it"
}

// AcquireLock takes the App Destination lock (atomic mkdir) and records info
// in lock/info.json. It fails with *LockedError when the lock is held. The
// returned unlock removes the lock; call it on every exit path.
//
// It passes through the Destination gate (<root>/appdirs/<app>/d/<dest>/gate,
// held only for this script) so an app_dir migration never races it: with
// the gate held it refuses while a migration journal exists
// (*MigrationPendingError), creates a missing canonical App directory as a
// real directory (today's layout; deploy and apply ensure app_dir before
// they lock), uses a real directory, and uses a symlink only when it is the
// trusted App link (owned by the deploy user, marked target). Anything else
// is refused. A held gate is a *LockedError naming the gate.
func AcquireLock(ctx context.Context, host remote.Host, app, destination string, info LockInfo) (unlock func(context.Context) error, err error) {
	if info.Time.IsZero() {
		info.Time = time.Now().UTC()
	}
	data, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	dir := LockDir(app, destination)
	canon := release.AppDir(app, destination)
	q, qc := remote.Quote(dir), remote.Quote(canon)
	script := safepath.AppLink(app, destination) + safepath.Script("App directory") +
		safepath.LockTaker(app, destination, string(data), true) +
		"if [ -L " + qc + " ]; then yoho_private " + qc + "\n" +
		"elif [ -e " + qc + " ]; then [ -d " + qc + " ] || yoho_bad " + qc + "\n" +
		"else mkdir -p " + qc + "; fi\n" +
		"if mkdir " + q + " 2>/dev/null; then cat > " + q + "/info.json; echo acquired; else echo held; cat " + q + "/info.json 2>/dev/null || true; fi"
	out, err := host.Output(ctx, remote.Cmd{Script: script, Stdin: bytes.NewReader(data)})
	if err != nil {
		return nil, fmt.Errorf("acquire lock on %s: %w", host.Name(), err)
	}
	status, rest, _ := strings.Cut(out, "\n")
	status = strings.TrimSpace(status)
	switch {
	case status == "acquired":
	case strings.HasPrefix(status, safepath.HeldLine+"\t"):
		le := &LockedError{Dir: strings.TrimPrefix(status, safepath.HeldLine+"\t")}
		_ = json.Unmarshal([]byte(strings.TrimSpace(rest)), &le.Holder)
		return nil, le
	case strings.HasPrefix(status, safepath.JournalLine+"\t"):
		return nil, &MigrationPendingError{Journal: strings.TrimPrefix(status, safepath.JournalLine+"\t"), Content: rest}
	default:
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
