package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
)

func init() { extraCommands = append(extraCommands, lockCmd) }

// lockWaitFlag is --lock-wait[=DURATION] on the commands that take the
// Destination lock.
const lockWaitFlag = "lock-wait"

// defaultLockWait is --lock-wait without a value.
const defaultLockWait = 15 * time.Minute

// lockPollInterval is how often --lock-wait checks the lock (tests shorten
// it).
var lockPollInterval = 15 * time.Second

// addLockWaitFlag adds --lock-wait to c.
func addLockWaitFlag(c *cobra.Command) {
	c.Flags().Duration(lockWaitFlag, 0, "Wait for a held Destination lock instead of refusing (bare flag: 15m), checking every 15s")
	c.Flags().Lookup(lockWaitFlag).NoOptDefVal = defaultLockWait.String()
}

// lockedRuntime is the runtime for a caller that holds the Destination
// lock itself.
func (a *app) lockedRuntime() plan.Runtime {
	if a.dest.Runtime == "swarm" && swarmRuntime != nil {
		return swarmRuntime(true)
	}
	return deploy.Compose{LockHeld: true}
}

// acquireLock takes the Destination lock on h (the first Server), waiting
// up to --lock-wait while someone else holds it.
func (a *app) acquireLock(ctx context.Context, h remote.Host, info deploy.LockInfo) (func(context.Context) error, error) {
	if info.Performer == "" {
		info.Performer = performer()
	}
	return a.waitLock(ctx, func(ctx context.Context) (func(context.Context) error, error) {
		return deploy.AcquireLock(ctx, h, a.cfg.App, a.destName, info)
	})
}

// waitLock calls take until it does not fail with *deploy.LockedError, for
// at most a.lockWait, printing who holds the lock on every check.
func (a *app) waitLock(ctx context.Context, take func(context.Context) (func(context.Context) error, error)) (func(context.Context) error, error) {
	deadline := time.Now().Add(a.lockWait)
	waited := false
	for {
		unlock, err := take(ctx)
		var le *deploy.LockedError
		if err == nil || a.lockWait <= 0 || !errors.As(err, &le) {
			if err == nil && waited {
				a.ui.Event("lock_acquired", "  lock acquired", map[string]any{"destination": a.destName})
			}
			return unlock, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, fmt.Errorf("gave up after waiting %s for the Destination lock: %w", a.lockWait, err)
		}
		waited = true
		f := map[string]any{"holder": lockFields(le.Holder, true), "waited_s": int((a.lockWait - left).Seconds()), "left_s": int(left.Seconds())}
		a.ui.Waiting(fmt.Sprintf("waiting for the lock of %s (%s left): %s", a.destName, roundDur(left), describeHolder(le.Holder)), f)
		t := time.NewTimer(min(lockPollInterval, left))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// describeHolder is one line about a lock holder.
func describeHolder(h deploy.LockInfo) string {
	who := h.Performer
	if who == "" {
		who = "unknown"
	}
	s := "held by " + who
	switch {
	case h.Command == deploy.ManualLockCommand:
		s += " (yoho lock acquire)"
	case h.Command != "":
		s += " (" + strings.TrimSpace(h.Command+" "+h.Version) + ")"
	}
	if !h.Time.IsZero() {
		s += " since " + h.Time.Local().Format("2006-01-02 15:04:05")
	}
	if h.Message != "" {
		s += ": " + h.Message
	}
	return s
}

// lockFields are the --json fields of a lock holder.
func lockFields(h deploy.LockInfo, locked bool) map[string]any {
	f := map[string]any{"locked": locked}
	if !locked {
		return f
	}
	f["performer"], f["command"] = h.Performer, h.Command
	if h.Version != "" {
		f["version"] = h.Version
	}
	if h.Message != "" {
		f["message"] = h.Message
	}
	if !h.Time.IsZero() {
		f["since"] = h.Time.UTC().Format(time.RFC3339)
	}
	f["manual"] = h.Command == deploy.ManualLockCommand
	return f
}

func lockCmd(g *globals) *cobra.Command {
	c := &cobra.Command{
		Use:   "lock",
		Short: "Hold, inspect or release the Destination lock",
		Long: `Every deploy, apply, rollback, secrets rotate, Backup, restore and drill
takes the Destination lock on its (first) Server; a second one refuses
while it is held (or waits with --lock-wait).

'yoho lock acquire -m MESSAGE' holds it by hand, e.g. during maintenance:
deploys, rollbacks and Scheduled Backups refuse with your message until
'yoho lock release'. 'yoho lock status' shows who holds it.
'yoho lock release --force' also removes a lock a killed command left.`,
	}
	var message string
	acquire := &cobra.Command{
		Use:   "acquire -m MESSAGE",
		Short: "Hold the Destination lock until `yoho lock release`",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			if strings.TrimSpace(message) == "" {
				return a.fail(&hintError{"a lock needs a message", "say why, e.g. `yoho lock acquire -m \"database maintenance\"`"})
			}
			return a.lockRun(cmd.Context(), func(ctx context.Context, h remote.Host) error {
				info := deploy.LockInfo{Performer: performer(), Command: deploy.ManualLockCommand, Message: message, Time: time.Now().UTC()}
				step := a.ui.Step(a.dest.Servers[0], "Acquire lock")
				if _, err := a.acquireLock(ctx, h, info); err != nil {
					step.Fail(err, hintFor(err, "check that the Server is reachable"))
					return &silentError{err}
				}
				step.Done()
				a.ui.Event("lock", "Locked "+a.cfg.App+"/"+a.destName+": "+message+"\n  release with `yoho lock release`",
					withAction(lockFields(info, true), "acquire"))
				return nil
			})
		},
	}
	acquire.Flags().StringVarP(&message, "message", "m", "", "Why the Destination is locked (shown to everyone it blocks)")
	addLockWaitFlag(acquire)

	status := &cobra.Command{
		Use:   "status",
		Short: "Show who holds the Destination lock",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			return a.lockRun(cmd.Context(), func(ctx context.Context, h remote.Host) error {
				info, held, err := deploy.ReadLock(ctx, h, a.cfg.App, a.destName)
				if err != nil {
					a.ui.Error(err, "check that the Server is reachable")
					return &silentError{err}
				}
				human := a.cfg.App + "/" + a.destName + " is not locked"
				if held {
					human = a.cfg.App + "/" + a.destName + " is locked: " + describeHolder(info)
				}
				a.ui.Event("lock", human, withAction(lockFields(info, held), "status"))
				return nil
			})
		},
	}

	var force bool
	release := &cobra.Command{
		Use:   "release [--force]",
		Short: "Release a lock taken with `yoho lock acquire` (--force: any lock)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			return a.lockRun(cmd.Context(), func(ctx context.Context, h remote.Host) error {
				step := a.ui.Step(a.dest.Servers[0], "Release lock")
				info, held, err := deploy.ReleaseLock(ctx, h, a.cfg.App, a.destName, force)
				var le *deploy.LockedError
				if errors.As(err, &le) {
					err = fmt.Errorf("the lock is %s, not by `yoho lock acquire`", describeHolder(info))
					step.Fail(err, "wait for it to finish; if that run was killed (nothing is running), rerun with --force")
					return &silentError{err}
				}
				if err != nil {
					step.Fail(err, "check that the Server is reachable")
					return &silentError{err}
				}
				if !held {
					step.Skip("not locked")
					a.ui.Event("lock", "", withAction(lockFields(info, false), "release"))
					return nil
				}
				step.Done(describeHolder(info))
				f := withAction(lockFields(info, true), "release")
				f["locked"], f["released"] = false, true
				a.ui.Event("lock", "Released the lock of "+a.cfg.App+"/"+a.destName, f)
				return nil
			})
		},
	}
	release.Flags().BoolVar(&force, "force", false, "Also remove a lock a running or killed command holds")
	c.AddCommand(acquire, status, release)
	return c
}

func withAction(f map[string]any, action string) map[string]any {
	f["action"] = action
	return f
}

// lockRun connects to the Destination's first Server (the lock lives
// there) and runs f.
func (a *app) lockRun(ctx context.Context, f func(context.Context, remote.Host) error) error {
	name := a.dest.Servers[0]
	srv := a.cfg.Servers[name]
	step := a.ui.Step(name, "Connect %s", srv.SSH)
	h, err := remote.NewSSH(name, srv.SSH, srv.Sudo, a.sshOptions())
	if err != nil {
		step.Fail(err, "check `ssh "+srv.SSH+"` works without a password")
		return &silentError{err}
	}
	defer h.Close()
	step.Done()
	return f(ctx, h)
}
