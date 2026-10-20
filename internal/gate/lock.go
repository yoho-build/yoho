package gate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// BootID is the kernel boot id ("" where there is none, e.g. macOS).
func BootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ClearStaleLock removes the deploy lock of app/dest when a Gate run took
// it and is gone: its pid is not running a Gate on this boot, or the
// Server rebooted since. It reports whether it removed the lock. Locks of
// the operator, and Gate locks without a boot id, are left alone.
//
// Gates check and remove under one flock (access/stale-lock.flock), so
// two Gates that both saw the same stale lock cannot have the second
// remove the lock the first then took: the second reads the lock only
// after the first has removed it, and finds it gone or held by a live Gate.
// The record is read again just before removal and must be unchanged.
func ClearStaleLock(app, dest, boot string) (bool, error) {
	if boot == "" {
		return false, nil
	}
	f, err := os.OpenFile(filepath.Join(release.AccessDir(app, dest), "stale-lock.flock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return false, fmt.Errorf("stale lock check: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return false, fmt.Errorf("stale lock check: %w", err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)

	dir := deploy.LockDir(app, dest)
	info := filepath.Join(dir, "info.json")
	b, err := os.ReadFile(info)
	if err != nil {
		return false, nil
	}
	var li deploy.LockInfo
	if json.Unmarshal(b, &li) != nil || li.PID <= 0 || li.Boot == "" {
		return false, nil
	}
	if li.Boot == boot && gateRunning(li.PID) {
		return false, nil
	}
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return false, nil
	}
	staleHook()
	if again, err := os.ReadFile(info); err != nil || !bytes.Equal(again, b) {
		return false, nil
	}
	if err := os.RemoveAll(dir); err != nil {
		return false, fmt.Errorf("clear stale lock: %w", err)
	}
	return true, nil
}

// staleHook runs between the stale check and the removal (tests).
var staleHook = func() {}

// gateRunning reports whether pid is a live `yoho gate` process. When
// /proc cannot tell, a live pid counts as running.
var gateRunning = func(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	cmd, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline")
	if err != nil {
		return true
	}
	args := bytes.Split(bytes.TrimRight(cmd, "\x00"), []byte{0})
	for _, a := range args {
		if string(a) == "gate" {
			return true
		}
	}
	return false
}

// lockedError rewrites a held lock for a Member, who cannot remove it.
func lockedError(err error) error {
	var le *deploy.LockedError
	if !errors.As(err, &le) {
		return err
	}
	h := le.Holder
	who := h.Performer
	if who == "" {
		who = "someone"
	}
	msg := "another deploy is running: " + who
	if h.Command != "" {
		msg += " (" + strings.TrimSpace(h.Command+" "+h.Version) + ")"
	}
	if !h.Time.IsZero() {
		msg += " since " + h.Time.UTC().Format(time.RFC3339)
	}
	return &remote.GateError{
		Message: msg,
		Hint:    "wait for it to finish and try again; a Gate deploy that died is cleared automatically, any other stale lock only by the operator",
		Code:    "locked",
	}
}
