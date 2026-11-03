package deploy

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

// PendingRotations lists the generated secrets with a pending rotation on
// the Server: generated/<KEY>.next exists because a `yoho secrets rotate
// --generated KEY` stopped before it swapped the new value in. The
// database may already use that value while generated/<KEY> (what deploy,
// rollback and Backups read) still holds the old one. Read-only and cheap
// (one remote listing); names only, never values. Sorted.
func PendingRotations(ctx context.Context, h remote.Host, app, dest string) ([]string, error) {
	dir := path.Join(release.AppDir(app, dest), "generated")
	out, err := h.Output(ctx, remote.Cmd{Script: "set -eu\nd=" + remote.Quote(dir) + "\n[ -d \"$d\" ] || exit 0\n" +
		"for f in \"$d\"/*.next; do\n  [ -f \"$f\" ] || continue\n  b=${f##*/}\n  printf '%s\\n' \"${b%.next}\"\ndone"})
	if err != nil {
		return nil, fmt.Errorf("check pending rotations on %s: %w", h.Name(), err)
	}
	var keys []string
	for _, k := range lines(out) {
		if secretNameRe.MatchString(k) && !slices.Contains(keys, k) {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

// PendingRotationError refuses a command while generated secrets have a
// pending rotation (see PendingRotations).
type PendingRotationError struct{ Keys []string }

func (e *PendingRotationError) Error() string {
	files := make([]string, len(e.Keys))
	flags := make([]string, len(e.Keys))
	for i, k := range e.Keys {
		files[i] = k + " (generated/" + k + ".next)"
		flags[i] = "--generated " + k
	}
	value := "value"
	if len(e.Keys) > 1 {
		value = "values"
	}
	return "pending rotation of " + strings.Join(files, ", ") + ": the database may already use the new " + value +
		"; run `yoho secrets rotate " + strings.Join(flags, " ") + "` to finish it"
}

// PendingRotationHint is the next step for a *PendingRotationError.
const PendingRotationHint = "an interrupted `yoho secrets rotate --generated` left a new value the database may already use, while Services still get the old one; finish that rotation first (it reuses the pending value, nothing is generated twice), then rerun"

// PendingRotationWarning is the plan-time form of a *PendingRotationError.
func PendingRotationWarning(keys []string) string {
	return (&PendingRotationError{Keys: keys}).Error()
}

// CheckPendingRotations returns a *PendingRotationError when a generated
// secret other than those in allow has a pending rotation. allow is the
// set of keys the caller is rotating itself.
func CheckPendingRotations(ctx context.Context, h remote.Host, app, dest string, allow []string) error {
	keys, err := PendingRotations(ctx, h, app, dest)
	if err != nil {
		return err
	}
	var bad []string
	for _, k := range keys {
		if !slices.Contains(allow, k) {
			bad = append(bad, k)
		}
	}
	if len(bad) > 0 {
		return &PendingRotationError{Keys: bad}
	}
	return nil
}

// swappedMarker records that a rotation swapped generated/<KEY>: it holds
// the keyed fingerprint (hmac.key) of the new value, never the value.
func swappedMarker(p string) string { return p + ".swapped" }

// RotationSwapped reports whether an interrupted rotation of key already
// swapped its new value into generated/<key> but did not roll it out: the
// marker matches the value in generated/<key>, and the current Release's
// secret audit does not record that value for any Service. A re-run of
// `yoho secrets rotate --generated key` then finishes the rollout instead
// of generating another value. A stale marker (the value was rolled out by
// a later rotate or deploy, or replaced) is removed and reports false.
func RotationSwapped(ctx context.Context, h remote.Host, app, dest string, cur *release.Release, key string) (bool, error) {
	dir := release.AppDir(app, dest)
	p := path.Join(dir, "generated", key)
	if _, err := h.ReadFile(ctx, p+".next", false); err == nil {
		return false, nil // the database step is pending; that path resumes it
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read pending %s: %w", key, err)
	}
	m, err := h.ReadFile(ctx, swappedMarker(p), false)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read rotation marker of %s: %w", key, err)
	}
	stale := func() (bool, error) {
		_ = h.Run(ctx, remote.Cmd{Script: "rm -f " + remote.Quote(swappedMarker(p))})
		return false, nil
	}
	fp := strings.TrimSpace(string(m))
	val, err := h.ReadFile(ctx, p, false)
	if err != nil || len(val) == 0 || fp == "" {
		return stale()
	}
	b, err := h.ReadFile(ctx, path.Join(dir, "hmac.key"), false)
	if err != nil {
		return stale()
	}
	hk, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(hk) < 16 || secrets.Fingerprint(hk, string(val)) != fp {
		return stale()
	}
	if cur == nil || len(cur.Secrets) == 0 {
		return stale() // cannot tell; rotate afresh as before
	}
	for _, a := range cur.Secrets {
		if a.Fingerprint == fp {
			return stale() // already rolled out
		}
	}
	return true, nil
}

// ClearRotationSwapped removes the swap markers of keys once their rollout
// finished. Best effort: a leftover marker is detected as stale later.
func ClearRotationSwapped(ctx context.Context, h remote.Host, app, dest string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	dir := path.Join(release.AppDir(app, dest), "generated")
	var ps []string
	for _, k := range keys {
		ps = append(ps, swappedMarker(path.Join(dir, k)))
	}
	return h.Run(ctx, remote.Cmd{Script: "rm -f " + remote.QuoteArgs(ps...)})
}

// isRefusal reports errors that stop a deploy or rollback before it
// changed anything: they are returned as they are, without deploy failure
// facts ("Services may already have changed" would be wrong).
func isRefusal(err error) bool {
	var le *LockedError
	var pe *PendingRotationError
	var me *MigrationPendingError
	var ms *MissingSecretsError
	return errors.As(err, &le) || errors.As(err, &pe) || errors.As(err, &me) || errors.As(err, &ms)
}

// IsRefusal is isRefusal for the swarm runtime and the CLI.
func IsRefusal(err error) bool { return isRefusal(err) }
