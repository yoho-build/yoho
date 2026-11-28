// Package pgpromote promotes a streaming Postgres standby to primary for
// `yoho db promote` (ADR 0021): read-only preflight, a journaled fence of the
// old primary through `yoho app stop`, a wait for the standby to replay
// everything it can, and pg_promote(). It runs psql in the Services'
// containers over their local socket; no password is ever passed.
package pgpromote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Node is one Postgres Service on its Server.
type Node struct {
	Server  string
	Service string
	// Project is the compose project (release.ProjectName).
	Project string
	// User is the role psql connects as over the local socket.
	User string
	Host remote.Host
}

// PsqlScript runs sql in n's running container: `docker compose exec -T`
// psql over the local socket, unaligned, tuples only, stopping on errors.
func (n Node) PsqlScript(sql string) string {
	return "set -eu\ndocker compose " + remote.QuoteArgs("-p", n.Project, "exec", "-T", n.Service,
		"psql", "-X", "-At", "-v", "ON_ERROR_STOP=1", "-U", n.User, "-d", "postgres", "-c", sql)
}

// Query runs sql on n and returns its trimmed output.
func (n Node) Query(ctx context.Context, sql string) (string, error) {
	out, err := n.Host.Output(ctx, remote.Cmd{Script: n.PsqlScript(sql)})
	if err != nil {
		return "", fmt.Errorf("%s on %s: %s: %w", n.Service, n.Server, sql, err)
	}
	return strings.TrimSpace(out), nil
}

func (n Node) boolean(ctx context.Context, sql string) (bool, error) {
	out, err := n.Query(ctx, sql)
	if err != nil {
		return false, err
	}
	switch out {
	case "t":
		return true, nil
	case "f":
		return false, nil
	}
	return false, fmt.Errorf("%s on %s: %s: unexpected output %q", n.Service, n.Server, sql, out)
}

// InRecovery is pg_is_in_recovery(): true on a standby.
func (n Node) InRecovery(ctx context.Context) (bool, error) {
	return n.boolean(ctx, "SELECT pg_is_in_recovery()")
}

// ReceiverStatus is pg_stat_wal_receiver.status, or "" without a receiver.
func (n Node) ReceiverStatus(ctx context.Context) (string, error) {
	return n.Query(ctx, "SELECT coalesce((SELECT status FROM pg_stat_wal_receiver LIMIT 1), '')")
}

// lsn runs a query returning one pg_lsn, which may be NULL ("").
func (n Node) lsn(ctx context.Context, sql string) (string, error) {
	out, err := n.Query(ctx, sql)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	if _, err := ParseLSN(out); err != nil {
		return "", fmt.Errorf("%s on %s: %w", n.Service, n.Server, err)
	}
	return out, nil
}

// CurrentLSN is pg_current_wal_lsn() on a primary.
func (n Node) CurrentLSN(ctx context.Context) (string, error) {
	return n.lsn(ctx, "SELECT pg_current_wal_lsn()")
}

// ReceiveReplay are pg_last_wal_receive_lsn() and pg_last_wal_replay_lsn()
// on a standby, "" when NULL.
func (n Node) ReceiveReplay(ctx context.Context) (string, string, error) {
	out, err := n.Query(ctx, "SELECT coalesce(pg_last_wal_receive_lsn()::text, '') || ' ' || coalesce(pg_last_wal_replay_lsn()::text, '')")
	if err != nil {
		return "", "", err
	}
	recv, replay, _ := strings.Cut(out, " ")
	for _, l := range []string{recv, replay} {
		if l == "" {
			continue
		}
		if _, err := ParseLSN(l); err != nil {
			return "", "", fmt.Errorf("%s on %s: %w", n.Service, n.Server, err)
		}
	}
	return recv, replay, nil
}

var lsnRe = regexp.MustCompile(`^[0-9A-F]+/[0-9A-F]+$`)

// ParseLSN parses a pg_lsn such as 0/3000060 into a byte position.
func ParseLSN(s string) (uint64, error) {
	if !lsnRe.MatchString(s) {
		return 0, fmt.Errorf("invalid LSN %q", s)
	}
	hi, lo, _ := strings.Cut(s, "/")
	h, err := strconv.ParseUint(hi, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid LSN %q", s)
	}
	l, err := strconv.ParseUint(lo, 16, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid LSN %q", s)
	}
	return h<<32 | l, nil
}

// Journal is the durable state of one promotion, on the new primary's
// Server, so a rerun resumes after the last complete step.
type Journal struct {
	Connection     string            `json:"connection"`
	From           string            `json:"from"`
	To             string            `json:"to"`
	FromServer     string            `json:"from_server"`
	ToServer       string            `json:"to_server"`
	TargetLSN      string            `json:"target_lsn,omitempty"`
	OldPrimaryDown bool              `json:"old_primary_down,omitempty"`
	Steps          map[string]string `json:"steps"`
	// State is running, failed or complete (the standby is primary).
	State       string    `json:"state"`
	OperationID string    `json:"operation_id,omitempty"`
	Started     time.Time `json:"started"`
}

// Step names, in order.
const (
	StepFence   = "fence"
	StepWait    = "wait-replay"
	StepPromote = "promote"
)

// JournalPath is the journal of connection's promotion.
func JournalPath(app, dest, connection string) string {
	return path.Join(release.AppDir(app, dest), "promotions", connection+".json")
}

// ReadJournal reads connection's journal on h; nil when there is none.
func ReadJournal(ctx context.Context, h remote.Host, app, dest, connection string) (*Journal, error) {
	b, err := h.ReadFile(ctx, JournalPath(app, dest, connection), false)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j Journal
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("parse promotion journal %s: %w", connection, err)
	}
	if j.Steps == nil {
		j.Steps = map[string]string{}
	}
	return &j, nil
}

func writeJournal(ctx context.Context, h remote.Host, app, dest string, j *Journal) error {
	b, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	return h.WriteFile(ctx, JournalPath(app, dest, j.Connection), append(b, '\n'), 0o600, false)
}

// Options describe one promotion.
type Options struct {
	App, Destination, Connection string
	// Old is the old primary; its Host is nil when its Server is down.
	Old Node
	New Node
	// OldPrimaryDown promotes without fencing: the operator powered the
	// old primary's Server off. Writes the standby did not receive are lost.
	OldPrimaryDown bool
	MaxLag         uint64
	Timeout        time.Duration
	// Poll is the replay wait interval (default 2s).
	Poll        time.Duration
	OperationID string
	Performer   string
	// Progress reports each step's state (running, complete, skipped, failed).
	Progress func(step, state string, j *Journal)
}

func (o Options) matches(j *Journal) bool {
	return j != nil && j.Connection == o.Connection && j.From == o.Old.Service && j.To == o.New.Service
}

// Status is what Preflight found.
type Status struct {
	// Journal is the existing journal of this promotion, if any.
	Journal *Journal
	// Fenced means the old primary is already stopped (journal or
	// `yoho app stop`); Promoted that the standby is already primary.
	Fenced, Promoted bool
	// Lag is the standby's replay lag behind the old primary in bytes,
	// when both run.
	Lag uint64
}

// Preflight checks, read-only, that the promotion can start or resume.
func Preflight(ctx context.Context, o Options) (Status, error) {
	var st Status
	j, err := ReadJournal(ctx, o.New.Host, o.App, o.Destination, o.Connection)
	if err != nil {
		return st, err
	}
	if j != nil && !o.matches(j) && j.State != "complete" {
		return st, fmt.Errorf("an unfinished promotion of %s → %s for Connection %s is journaled on %s\nhint: finish it first: restore that Connection in the Yoho file and rerun `yoho db promote %s`", j.From, j.To, j.Connection, o.New.Server, j.Connection)
	}
	if o.matches(j) {
		st.Journal = j
		st.Fenced = j.Steps[StepFence] == "complete" || j.Steps[StepFence] == "skipped"
	}
	if !st.Fenced && !o.OldPrimaryDown && o.Old.Host != nil {
		stopped, err := deploy.ReadStopped(ctx, o.Old.Host, o.App, o.Destination)
		if err != nil {
			return st, err
		}
		_, st.Fenced = stopped[o.Old.Service]
	}
	inRecovery, err := o.New.InRecovery(ctx)
	if err != nil {
		return st, err
	}
	if !inRecovery {
		if !st.Fenced && !o.OldPrimaryDown && (st.Journal == nil || st.Journal.Steps[StepPromote] != "complete") {
			return st, fmt.Errorf("%s on %s is not in recovery: it is already a primary while %s still runs\nhint: stop one of them by hand (`yoho app stop %s`) before anything writes to both", o.New.Service, o.New.Server, o.Old.Service, o.Old.Service)
		}
		st.Promoted = true
		return st, nil
	}
	if st.Fenced || o.OldPrimaryDown {
		return st, nil
	}
	status, err := o.New.ReceiverStatus(ctx)
	if err != nil {
		return st, err
	}
	if status != "streaming" {
		if status == "" {
			status = "no WAL receiver"
		}
		return st, fmt.Errorf("%s on %s is not streaming from %s (%s)\nhint: check replication with `yoho app logs %s`; promote only a streaming standby", o.New.Service, o.New.Server, o.Old.Service, status, o.New.Service)
	}
	oldRecovery, err := o.Old.InRecovery(ctx)
	if err != nil {
		return st, err
	}
	if oldRecovery {
		return st, fmt.Errorf("%s on %s is in recovery: it is not the primary", o.Old.Service, o.Old.Server)
	}
	cur, err := o.Old.CurrentLSN(ctx)
	if err != nil {
		return st, err
	}
	_, replay, err := o.New.ReceiveReplay(ctx)
	if err != nil {
		return st, err
	}
	c, _ := ParseLSN(cur)
	r, _ := ParseLSN(replay)
	if replay == "" {
		return st, fmt.Errorf("%s on %s has replayed nothing yet", o.New.Service, o.New.Server)
	}
	if c > r {
		st.Lag = c - r
	}
	if st.Lag > o.MaxLag {
		return st, fmt.Errorf("%s is %d bytes behind %s (--max-lag %d)\nhint: wait for it to catch up, or raise --max-lag", o.New.Service, st.Lag, o.Old.Service, o.MaxLag)
	}
	return st, nil
}

// FenceHint is the next step after a failed fence or replay wait.
func FenceHint(old string) string {
	return old + " is stopped and nothing was promoted. `yoho app start " + old + "` restores the old primary. Rerun to continue. `--old-primary-down` promotes at the standby's received position; writes it did not receive are lost."
}

// Run fences the old primary, waits for the standby to replay its WAL and
// promotes it, resuming from o's journal. Callers hold every Server's lock
// and have run Preflight.
func Run(ctx context.Context, o Options) (err error) {
	if o.Poll <= 0 {
		o.Poll = 2 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	j, err := ReadJournal(ctx, o.New.Host, o.App, o.Destination, o.Connection)
	if err != nil {
		return err
	}
	if !o.matches(j) {
		if j != nil && j.State != "complete" {
			return fmt.Errorf("an unfinished promotion of %s → %s is journaled", j.From, j.To)
		}
		j = &Journal{Connection: o.Connection, From: o.Old.Service, To: o.New.Service, FromServer: o.Old.Server, ToServer: o.New.Server, Steps: map[string]string{}, Started: time.Now().UTC()}
	}
	if j.State == "complete" {
		return nil
	}
	j.State = "running"
	j.OldPrimaryDown = j.OldPrimaryDown || o.OldPrimaryDown
	if o.OperationID != "" {
		j.OperationID = o.OperationID
	}
	save := func(ctx context.Context) error { return writeJournal(ctx, o.New.Host, o.App, o.Destination, j) }
	report := func(step, state string) {
		if o.Progress != nil {
			o.Progress(step, state, j)
		}
	}
	step := func(name string, run func() (string, error)) error {
		if s := j.Steps[name]; s == "complete" || s == "skipped" {
			report(name, s)
			return nil
		}
		j.Steps[name] = "running"
		if e := save(ctx); e != nil {
			return e
		}
		report(name, "running")
		state, e := run()
		if e != nil {
			j.Steps[name] = "failed"
			j.State = "failed"
		} else {
			j.Steps[name] = state
		}
		report(name, j.Steps[name])
		return errors.Join(e, save(context.WithoutCancel(ctx)))
	}
	stopAttempted := false
	if err = step(StepFence, func() (string, error) {
		if o.OldPrimaryDown {
			return "skipped", nil
		}
		if o.Old.Host == nil {
			return "", fmt.Errorf("Server %s of the old primary is not connected", o.Old.Server)
		}
		stopped, e := deploy.ReadStopped(ctx, o.Old.Host, o.App, o.Destination)
		if e != nil {
			return "", e
		}
		if _, already := stopped[o.Old.Service]; !already {
			lsn, e := o.Old.CurrentLSN(ctx)
			if e != nil {
				return "", e
			}
			j.TargetLSN = lsn
			if e := save(ctx); e != nil {
				return "", e
			}
		}
		stopAttempted = true
		res, e := deploy.StopService(ctx, o.Old.Host, o.App, o.Destination, o.Old.Service, deploy.StoppedEntry{By: o.Performer, At: time.Now().UTC(), Reason: "db promote " + o.Connection})
		if e != nil {
			return "", e
		}
		if !res.Clean() {
			var codes []string
			for _, id := range res.Containers {
				codes = append(codes, fmt.Sprintf("%s exited %d", id, res.ExitCodes[id]))
			}
			return "", fmt.Errorf("old primary %s did not shut down cleanly (%s); its last writes may not have reached %s", o.Old.Service, strings.Join(codes, ", "), o.New.Service)
		}
		return "complete", nil
	}); err != nil {
		if stopAttempted {
			return fmt.Errorf("%w\nhint: %s", err, FenceHint(o.Old.Service))
		}
		return err
	}
	if err = step(StepWait, func() (string, error) {
		return "complete", waitReplay(ctx, o, j.TargetLSN)
	}); err != nil {
		if o.OldPrimaryDown {
			return err
		}
		return fmt.Errorf("%w\nhint: %s", err, FenceHint(o.Old.Service))
	}
	if err = step(StepPromote, func() (string, error) {
		inRecovery, e := o.New.InRecovery(ctx)
		if e != nil {
			return "", e
		}
		if inRecovery {
			out, e := o.New.Query(ctx, "SELECT pg_promote(true, 60)")
			if e != nil {
				return "", e
			}
			if out != "t" {
				return "", fmt.Errorf("pg_promote() on %s returned %q", o.New.Service, out)
			}
			if inRecovery, e = o.New.InRecovery(ctx); e != nil {
				return "", e
			}
			if inRecovery {
				return "", fmt.Errorf("%s is still in recovery after pg_promote()", o.New.Service)
			}
		}
		return "complete", nil
	}); err != nil {
		return err
	}
	j.State = "complete"
	return save(ctx)
}

// waitReplay polls the standby until it has replayed everything it
// received and, when target is known, at least target.
func waitReplay(ctx context.Context, o Options, target string) error {
	var want uint64
	if target != "" {
		var err error
		if want, err = ParseLSN(target); err != nil {
			return err
		}
	}
	inRecovery, err := o.New.InRecovery(ctx)
	if err != nil {
		return err
	}
	if !inRecovery {
		return nil // promoted by an earlier run
	}
	deadline := time.Now().Add(o.Timeout)
	last := ""
	for {
		recv, replay, err := o.New.ReceiveReplay(ctx)
		if err != nil {
			return err
		}
		r, _ := ParseLSN(replay)
		caught := replay != "" && (recv == replay || recv == "" && target == "") && (target == "" || r >= want)
		if caught {
			return nil
		}
		last = fmt.Sprintf("received %s, replayed %s", or(recv, "nothing"), or(replay, "nothing"))
		if target != "" {
			last += ", target " + target
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s did not replay the old primary's WAL within %s (%s)", o.New.Service, o.Timeout, last)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(o.Poll):
		}
	}
}

func or(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
