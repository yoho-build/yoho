package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/yoho-build/yoho/internal/plan"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/safepath"
)

// LockInfo describes who holds the deploy/backup lock of an App Destination.
type LockInfo struct {
	Performer string `json:"performer"`
	Version   string `json:"version,omitempty"`
	Command   string `json:"command,omitempty"` // e.g. "deploy", "rollback", "backup", "lock" (yoho lock acquire)
	// Message is the reason given to `yoho lock acquire -m`.
	Message string    `json:"message,omitempty"`
	Time    time.Time `json:"time"`
	// PID and Boot (kernel boot id) of a Gate holder, so a later Gate run
	// can tell that the holder died (see gate.ClearStaleLock).
	PID  int    `json:"pid,omitempty"`
	Boot string `json:"boot,omitempty"`
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
	if h.Command == ManualLockCommand {
		msg += " (yoho lock acquire)"
	} else if h.Command != "" {
		msg += " (" + h.Command
		if h.Version != "" {
			msg += " " + h.Version
		}
		msg += ")"
	}
	if !h.Time.IsZero() {
		msg += " since " + h.Time.UTC().Format(time.RFC3339)
	}
	if h.Message != "" {
		msg += ": " + strconv.Quote(h.Message)
	}
	switch {
	case e.Gate():
		return msg + "; if the lock is stale, remove " + e.Dir + " on the Server"
	case h.Command == ManualLockCommand:
		return msg + "; `yoho lock release` releases it"
	}
	return msg + "; if the lock is stale (its run was killed), run `yoho lock release --force`"
}

// Gate reports a held Destination gate (held only briefly by lock takers
// and app_dir migrations), not the lock itself: `yoho lock release` does
// not remove it.
func (e *LockedError) Gate() bool { return e.Dir != "" && path.Base(e.Dir) != "lock" }

// ManualLockCommand is the LockInfo.Command of `yoho lock acquire`.
const ManualLockCommand = "lock"

// Manual reports a lock taken by `yoho lock acquire`, not by a running
// command.
func (e *LockedError) Manual() bool { return e.Holder.Command == ManualLockCommand }

// ReadLock returns the holder of the App Destination lock; held is false
// when nobody holds it.
func ReadLock(ctx context.Context, host remote.Host, app, destination string) (info LockInfo, held bool, err error) {
	q := remote.Quote(LockDir(app, destination))
	out, err := host.Output(ctx, remote.Cmd{Script: "if [ -d " + q + " ]; then echo held; cat " + q + "/info.json 2>/dev/null || true; else echo free; fi"})
	if err != nil {
		return info, false, fmt.Errorf("read lock on %s: %w", host.Name(), err)
	}
	status, rest, _ := strings.Cut(out, "\n")
	if strings.TrimSpace(status) != "held" {
		return info, false, nil
	}
	_ = json.Unmarshal([]byte(strings.TrimSpace(rest)), &info)
	return info, true, nil
}

// ReleaseLock removes the App Destination lock. Unless force, it removes
// only a lock taken by `yoho lock acquire` and returns *LockedError for one
// a running (or killed) command holds. held reports whether there was one.
func ReleaseLock(ctx context.Context, host remote.Host, app, destination string, force bool) (holder LockInfo, held bool, err error) {
	holder, held, err = ReadLock(ctx, host, app, destination)
	if err != nil || !held {
		return holder, held, err
	}
	dir := LockDir(app, destination)
	if !force && holder.Command != ManualLockCommand {
		return holder, true, &LockedError{Dir: dir, Holder: holder}
	}
	if err := host.Run(ctx, remote.Cmd{Script: "rm -rf " + remote.Quote(dir)}); err != nil {
		return holder, true, fmt.Errorf("release lock on %s: %w", host.Name(), err)
	}
	return holder, true, nil
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
		lockDirectoryScript(q)
	return acquireLockScript(ctx, host, dir, script, data)
}

// AcquirePreviewLock serializes preview admission across all Destinations of an
// App. It is held only while counting previews and writing their markers.
func AcquirePreviewLock(ctx context.Context, host remote.Host, app string, info LockInfo) (func(context.Context) error, error) {
	if info.Time.IsZero() {
		info.Time = time.Now().UTC()
	}
	data, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	dir := path.Join(release.AppDir(app, ""), "preview.lock")
	q := remote.Quote(dir)
	script := safepath.Script("preview lock") + safepath.Creation +
		"yoho_make " + remote.Quote(path.Dir(dir)) + "\n" +
		"yoho_path " + q + "\n" +
		lockDirectoryScript(q)
	return acquireLockScript(ctx, host, dir, script, data)
}

// lockDirectoryScript distinguishes a held lock from a filesystem failure.
// If recording a newly acquired lock fails (for example ENOSPC), remove only
// the directory this invocation created so a retry can acquire it.
// Use mkdir's C-locale error to distinguish contention: a later directory
// inspection would race an unlock and misclassify a vanished lock as I/O.
func lockDirectoryScript(q string) string {
	return "yoho_lock_write() {\n" +
		"if cat > \"$1\"; then return 0; fi\n" +
		"rm -f " + q + "/info.json; rmdir " + q + "\n" +
		"echo 'cannot record Destination lock; check free disk space and permissions' >&2; return 1\n}\n" +
		"if yoho_lock_error=$(LC_ALL=C mkdir " + q + " 2>&1); then yoho_lock_write " + q + "/info.json; echo acquired; else echo held; " +
		"case \"$yoho_lock_error\" in *\": File exists\") ;; *) printf '%s\\n' \"$yoho_lock_error\" >&2; echo 'cannot create Destination lock; check free disk space and permissions' >&2; exit 1 ;; esac; " +
		"cat " + q + "/info.json 2>/dev/null || true; fi"
}

func acquireLockScript(ctx context.Context, host remote.Host, dir, script string, data []byte) (func(context.Context) error, error) {
	q := remote.Quote(dir)
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

// AcquireLocks is the single multi-Server lock coordinator. On refusal it
// releases every acquired lock, and cleanup uses an uncancelled context.
// take permits the CLI's existing --lock-wait policy without duplicating order.
func AcquireLocks(ctx context.Context, hosts []plan.NamedHost, app, dest string, info LockInfo, take func(context.Context, plan.NamedHost, LockInfo) (func(context.Context) error, error)) (func(context.Context) error, error) {
	hosts = slices.Clone(hosts)
	slices.SortFunc(hosts, func(a, b plan.NamedHost) int { return strings.Compare(a.Name, b.Name) })
	var held []func(context.Context) error
	unlock := func(ctx context.Context) error {
		var errs []error
		for i := len(held) - 1; i >= 0; i-- {
			errs = append(errs, held[i](context.WithoutCancel(ctx)))
		}
		return errors.Join(errs...)
	}
	for _, h := range hosts {
		var u func(context.Context) error
		var e error
		if take != nil {
			u, e = take(ctx, h, info)
		} else {
			u, e = AcquireLock(ctx, h.Host, app, dest, info)
		}
		if e != nil {
			return nil, errors.Join(e, unlock(ctx))
		}
		held = append(held, u)
	}
	return unlock, nil
}
