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
	// User is the role psql connects as over the local socket, when it is
	// not secret.
	User string
	// UserSecret, when set, says the role is an x-yoho.secrets value that
	// never leaves the container: UserFromEnv reads $POSTGRES_USER,
	// UserFromFile /run/secrets/POSTGRES_USER.
	UserSecret string
	Host       remote.Host
}

// Where a secret-sourced role is read inside the container.
const (
	UserFromEnv  = "env"
	UserFromFile = "file"
)

// PsqlScript runs sql in n's running container: `docker compose exec -T`
// psql over the local socket, unaligned, tuples only, stopping on errors. A
// secret role is resolved inside the container into PGUSER, so it is never
// in argv.
func (n Node) PsqlScript(sql string) string {
	if n.UserSecret == "" {
		return "set -eu\ndocker compose " + remote.QuoteArgs(append(append([]string{"-p", n.Project, "exec", "-T", n.Service}, psqlArgs...), "-U", n.User, "-d", "postgres", "-c", sql)...)
	}
	return "set -eu\ndocker compose " + remote.QuoteArgs("-p", n.Project, "exec", "-T", n.Service, "sh", "-c", psqlInner(n.UserSecret), "sh", sql)
}

var psqlArgs = []string{"psql", "-X", "-At", "-v", "ON_ERROR_STOP=1"}

// psqlInner is the in-container script that runs psql as the secret role
// ($1 is the SQL).
func psqlInner(secret string) string {
	user := `PGUSER=${POSTGRES_USER:-}; `
	if secret == UserFromFile {
		user = `PGUSER=$(cat /run/secrets/POSTGRES_USER) || { echo "yoho: cannot read /run/secrets/POSTGRES_USER" >&2; exit 1; }; `
	}
	// Never run psql with an empty role: it would fall back to the OS user.
	return user + `[ -n "$PGUSER" ] || { echo "yoho: POSTGRES_USER is empty in the container" >&2; exit 1; }; export PGUSER; exec ` +
		strings.Join(psqlArgs, " ") + ` -d postgres -c "$1"`
}

// PsqlError is a psql run that failed: it could not connect or
// authenticate, or the SQL failed.
type PsqlError struct{ Err error }

func (e *PsqlError) Error() string { return e.Err.Error() }
func (e *PsqlError) Unwrap() error { return e.Err }

// Query runs sql on n and returns its trimmed output.
func (n Node) Query(ctx context.Context, sql string) (string, error) {
	out, err := n.Host.Output(ctx, remote.Cmd{Script: n.PsqlScript(sql)})
	if err != nil {
		return "", &PsqlError{fmt.Errorf("%s on %s: %s: %w", n.Service, n.Server, sql, err)}
	}
	return strings.TrimSpace(out), nil
}

// SystemID is pg_control_system().system_identifier: equal on a primary
// and its standbys, different for another cluster.
func (n Node) SystemID(ctx context.Context) (string, error) {
	out, err := n.Query(ctx, "SELECT system_identifier FROM pg_control_system()")
	if err != nil {
		return "", err
	}
	if _, e := strconv.ParseUint(out, 10, 64); e != nil {
		return "", fmt.Errorf("%s on %s: unexpected system identifier %q", n.Service, n.Server, out)
	}
	return out, nil
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

// ReceivedTLI is pg_stat_wal_receiver.received_tli on a standby, "" without
// a receiver.
func (n Node) ReceivedTLI(ctx context.Context) (string, error) {
	return n.number(ctx, "SELECT coalesce((SELECT received_tli FROM pg_stat_wal_receiver LIMIT 1)::text, '')")
}

// TimelineID is pg_control_checkpoint().timeline_id: the current timeline
// of a running primary.
func (n Node) TimelineID(ctx context.Context) (string, error) {
	return n.number(ctx, "SELECT timeline_id FROM pg_control_checkpoint()")
}

// number runs a query returning one integer or "" (NULL).
func (n Node) number(ctx context.Context, sql string) (string, error) {
	out, err := n.Query(ctx, sql)
	if err != nil {
		return "", err
	}
	if out == "" {
		return "", nil
	}
	if _, e := strconv.ParseUint(out, 10, 64); e != nil {
		return "", fmt.Errorf("%s on %s: %s: unexpected output %q", n.Service, n.Server, sql, out)
	}
	return out, nil
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
	// '|' separates them: Query trims spaces, which would shift a NULL
	// receive position into the replay field.
	out, err := n.Query(ctx, "SELECT coalesce(pg_last_wal_receive_lsn()::text, '') || '|' || coalesce(pg_last_wal_replay_lsn()::text, '')")
	if err != nil {
		return "", "", err
	}
	recv, replay, _ := strings.Cut(out, "|")
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

// ControlDataScript is the StopOptions.AfterStop snippet of the fence: it
// runs pg_controldata on the stopped old primary's data directory (PGDATA
// from the container's environment, default /var/lib/postgresql/data) in a
// throwaway container of the same image that shares its volumes.
const ControlDataScript = `image=$(docker inspect -f '{{.Config.Image}}' "$id")
pgdata=$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$id" | sed -n 's/^PGDATA=//p' | tail -n 1)
docker run --rm --pull never --network none --volumes-from "$id" -e LC_ALL=C --entrypoint pg_controldata "$image" -D "${pgdata:-/var/lib/postgresql/data}"`

// ControlData is what the fence reads from pg_controldata.
type ControlData struct {
	// State is "Database cluster state", "shut down" after a clean stop.
	State string
	// Checkpoint is "Latest checkpoint location": after a clean stop the
	// shutdown checkpoint, the end of the old primary's WAL.
	Checkpoint string
	// SystemID is "Database system identifier".
	SystemID string
	// Timeline is "Latest checkpoint's TimeLineID".
	Timeline string
}

// ParseControlData reads pg_controldata output (LC_ALL=C).
func ParseControlData(out string) ControlData {
	var cd ControlData
	for _, l := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "Database cluster state":
			cd.State = v
		case "Latest checkpoint location":
			cd.Checkpoint = v
		case "Database system identifier":
			cd.SystemID = v
		case "Latest checkpoint's TimeLineID":
			cd.Timeline = v
		}
	}
	return cd
}

// Journal is the retry marker of one promotion, on the new primary's
// Server, so a rerun resumes after the last complete step. `yoho db
// promote` deletes it once the follow-up apply succeeded.
type Journal struct {
	Connection     string `json:"connection"`
	From           string `json:"from"`
	To             string `json:"to"`
	FromServer     string `json:"from_server"`
	ToServer       string `json:"to_server"`
	TargetLSN      string `json:"target_lsn,omitempty"`
	OldPrimaryDown bool   `json:"old_primary_down,omitempty"`
	// Unclean records a fence whose old primary did not shut down cleanly
	// (exit codes or cluster state); a rerun refuses it without
	// --old-primary-down.
	Unclean []string `json:"unclean,omitempty"`
	// FenceStarted is set before the fence first stops the old primary; a
	// later attempt that finds no container cannot verify a clean
	// shutdown and treats it as unclean.
	FenceStarted bool `json:"fence_started,omitempty"`
	// FenceAt identifies the fence: the `at` of the old primary's
	// stopped.json entry it relies on. Another entry (started and stopped
	// again) is another fence.
	FenceAt time.Time `json:"fence_at,omitzero"`
	// Timeline is the old primary's timeline checked before the fence.
	Timeline string            `json:"timeline,omitempty"`
	Steps    map[string]string `json:"steps"`
	// State is running, failed or promoted (the standby is primary; the
	// apply may still be due). Older journals say complete for promoted.
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

// DeleteJournal removes connection's journal on h once the promotion and
// its apply are done.
func DeleteJournal(ctx context.Context, h remote.Host, app, dest, connection string) error {
	return h.Run(ctx, remote.Cmd{Script: "set -eu\nrm -f -- " + remote.Quote(JournalPath(app, dest, connection))})
}

// Promoted reports whether j says the standby was promoted.
func (j *Journal) Promoted() bool {
	return j != nil && (j.State == "promoted" || j.State == "complete")
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

// Fenced reports, from Server state, whether the old primary is fenced:
// recorded in stopped.json and without running containers.
func Fenced(ctx context.Context, o Options) (bool, error) {
	_, ok, err := fence(ctx, o)
	return ok, err
}

// fence is the old primary's stopped.json entry and whether it is fenced.
func fence(ctx context.Context, o Options) (deploy.StoppedEntry, bool, error) {
	if o.Old.Host == nil {
		return deploy.StoppedEntry{}, false, nil
	}
	stopped, err := deploy.ReadStopped(ctx, o.Old.Host, o.App, o.Destination)
	if err != nil {
		return deploy.StoppedEntry{}, false, err
	}
	e, ok := stopped[o.Old.Service]
	if !ok {
		return e, false, nil
	}
	ids, err := deploy.RunningContainers(ctx, o.Old.Host, o.App, o.Destination, o.Old.Service)
	if err != nil {
		return e, false, err
	}
	return e, len(ids) == 0, nil
}

// journalFenced reports whether j's fence still holds: the old primary is
// fenced by the very stop record j relies on.
func journalFenced(ctx context.Context, o Options, j *Journal) (bool, error) {
	e, ok, err := fence(ctx, o)
	if err != nil || !ok {
		return false, err
	}
	// A fence without a recorded identity is unverified: it matches no
	// stop record.
	return !j.FenceAt.IsZero() && e.At.Equal(j.FenceAt), nil
}

// promotedButNotFenced refuses to resume a promoted journal while the old
// primary runs again on a connected Server.
func promotedButNotFenced(ctx context.Context, o Options) error {
	if o.Old.Host == nil {
		return nil
	}
	ok, err := Fenced(ctx, o)
	if err != nil || ok {
		return err
	}
	return fmt.Errorf("%s is promoted, but the old primary %s runs again on %s\nhint: stop it (`yoho app stop %s`) before anything writes to both, then rerun", o.New.Service, o.Old.Service, o.Old.Server, o.Old.Service)
}

// UncleanError refuses to continue after an unclean fence.
func UncleanError(j *Journal) error {
	return fmt.Errorf("old primary %s did not shut down cleanly (%s); its last writes may not have reached %s\nhint: `yoho app start %s` restores the old primary; or rerun with --old-primary-down to promote %s at the position it received, accepting that writes it did not receive are lost", j.From, strings.Join(j.Unclean, ", "), j.To, j.From, j.To)
}

// resumable returns j when it is o's journal and still describes the
// cluster: a promoted journal whose new primary is in recovery is stale
// (a later switchover reversed it) and is dropped.
func (o Options) resumable(j *Journal, inRecovery bool) *Journal {
	if !o.matches(j) || j.Promoted() && inRecovery {
		return nil
	}
	return j
}

// Status is what Preflight found.
type Status struct {
	// Journal is the existing journal of this promotion, if any.
	Journal *Journal
	// Fenced means the old primary is stopped, read from its Server
	// (stopped.json and no running container), never from the journal;
	// Promoted that the standby is already primary.
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
	if j != nil && !o.matches(j) && !j.Promoted() {
		return st, fmt.Errorf("an unfinished promotion of %s → %s for Connection %s is journaled on %s\nhint: finish it first: restore that Connection in the Yoho file and rerun `yoho db promote %s`", j.From, j.To, j.Connection, o.New.Server, j.Connection)
	}
	inRecovery, err := o.New.InRecovery(ctx)
	if err != nil {
		return st, err
	}
	st.Journal = o.resumable(j, inRecovery)
	// The fence is Server state, never the journal's word: the operator
	// may have started the old primary again after a failure, or started
	// and stopped it again.
	if st.Journal != nil && st.Journal.Steps[StepFence] != "" {
		st.Fenced, err = journalFenced(ctx, o, st.Journal)
	} else {
		st.Fenced, err = Fenced(ctx, o)
	}
	if err != nil {
		return st, err
	}
	if st.Journal.Promoted() && !inRecovery {
		if err := promotedButNotFenced(ctx, o); err != nil {
			return st, err
		}
	}
	if st.Journal != nil && st.Journal.Steps[StepFence] == "unclean" && st.Fenced && !o.OldPrimaryDown {
		return st, UncleanError(st.Journal)
	}
	if !inRecovery {
		if !st.Fenced && !o.OldPrimaryDown && (st.Journal == nil || st.Journal.Steps[StepPromote] != "complete" && !st.Journal.Promoted()) {
			return st, fmt.Errorf("%s on %s is not in recovery: it is already a primary while %s still runs\nhint: stop one of them by hand (`yoho app stop %s`) before anything writes to both", o.New.Service, o.New.Server, o.Old.Service, o.Old.Service)
		}
		st.Promoted = true
		return st, nil
	}
	if st.Fenced || o.OldPrimaryDown {
		return st, nil
	}
	st.Lag, _, err = checkReplication(ctx, o)
	return st, err
}

// checkReplication checks, read-only, that the standby streams from the
// running old primary on its timeline, belongs to its cluster and lags at
// most MaxLag. It returns the lag and the old primary's timeline.
func checkReplication(ctx context.Context, o Options) (lag uint64, timeline string, err error) {
	status, err := o.New.ReceiverStatus(ctx)
	if err != nil {
		return 0, "", err
	}
	if status != "streaming" {
		if status == "" {
			status = "no WAL receiver"
		}
		return 0, "", fmt.Errorf("%s on %s is not streaming from %s (%s)\nhint: check replication with `yoho app logs %s`; promote only a streaming standby", o.New.Service, o.New.Server, o.Old.Service, status, o.New.Service)
	}
	oldRecovery, err := o.Old.InRecovery(ctx)
	if err != nil {
		return 0, "", err
	}
	if oldRecovery {
		return 0, "", fmt.Errorf("%s on %s is in recovery: it is not the primary", o.Old.Service, o.Old.Server)
	}
	oldID, err := o.Old.SystemID(ctx)
	if err != nil {
		return 0, "", err
	}
	newID, err := o.New.SystemID(ctx)
	if err != nil {
		return 0, "", err
	}
	if oldID != newID {
		return 0, "", sameClusterError(o, oldID, newID)
	}
	timeline, err = o.Old.TimelineID(ctx)
	if err != nil {
		return 0, "", err
	}
	received, err := o.New.ReceivedTLI(ctx)
	if err != nil {
		return 0, "", err
	}
	if received != timeline {
		return 0, "", fmt.Errorf("%s receives timeline %s, but %s is on timeline %s\nhint: promote only a standby that follows the current primary's timeline", o.New.Service, or(received, "none"), o.Old.Service, timeline)
	}
	cur, err := o.Old.CurrentLSN(ctx)
	if err != nil {
		return 0, "", err
	}
	_, replay, err := o.New.ReceiveReplay(ctx)
	if err != nil {
		return 0, "", err
	}
	if replay == "" {
		return 0, "", fmt.Errorf("%s on %s has replayed nothing yet", o.New.Service, o.New.Server)
	}
	c, _ := ParseLSN(cur)
	r, _ := ParseLSN(replay)
	if c > r {
		lag = c - r
	}
	if lag > o.MaxLag {
		return lag, "", fmt.Errorf("%s is %d bytes behind %s (--max-lag %d)\nhint: wait for it to catch up, or raise --max-lag", o.New.Service, lag, o.Old.Service, o.MaxLag)
	}
	return lag, timeline, nil
}

func sameClusterError(o Options, oldID, newID string) error {
	return fmt.Errorf("%s (system identifier %s) is not a standby of %s (system identifier %s): they are different clusters\nhint: point the Connection at a standby cloned from %s", o.New.Service, newID, o.Old.Service, oldID, o.Old.Service)
}

// FenceHint is the next step after a failed fence or replay wait.
func FenceHint(old string) string {
	return old + " is stopped and nothing was promoted. `yoho app start " + old + "` restores the old primary. Rerun to continue. `--old-primary-down` promotes at the standby's received position; writes it did not receive are lost."
}

// Run fences the old primary, waits for the standby to replay its WAL and
// promotes it, resuming from o's journal, which it leaves in state
// promoted for the caller to delete after its apply. Callers hold every
// Server's lock and have run Preflight.
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
	if j != nil && !o.matches(j) && !j.Promoted() {
		return fmt.Errorf("an unfinished promotion of %s → %s is journaled", j.From, j.To)
	}
	inRecovery, err := o.New.InRecovery(ctx)
	if err != nil {
		return err
	}
	if j = o.resumable(j, inRecovery); j == nil {
		j = &Journal{Connection: o.Connection, From: o.Old.Service, To: o.New.Service, FromServer: o.Old.Server, ToServer: o.New.Server, Steps: map[string]string{}, Started: time.Now().UTC()}
	}
	if j.Promoted() {
		return promotedButNotFenced(ctx, o)
	}
	save := func(ctx context.Context) error { return writeJournal(ctx, o.New.Host, o.App, o.Destination, j) }
	// Re-verify the fence from Server state under the locks: after
	// `yoho app start <old>` the journal's fence no longer holds, so fence
	// again and take a fresh target.
	if o.Old.Host != nil && j.Steps[StepFence] != "" {
		fenced, e := journalFenced(ctx, o, j)
		if e != nil {
			return e
		}
		if !fenced {
			// A fence was attempted: a container gone without a verified
			// shutdown is unclean.
			j.FenceStarted = true
			delete(j.Steps, StepFence)
			delete(j.Steps, StepWait)
			j.TargetLSN, j.Unclean, j.FenceAt, j.Timeline = "", nil, time.Time{}, ""
		} else if j.Steps[StepFence] == "unclean" {
			if !o.OldPrimaryDown {
				return UncleanError(j)
			}
			// The operator accepts the loss: wait without a target.
			j.Steps[StepFence], j.OldPrimaryDown, j.TargetLSN = "skipped", true, ""
		}
	}
	j.State = "running"
	j.OldPrimaryDown = j.OldPrimaryDown || o.OldPrimaryDown
	if o.OperationID != "" {
		j.OperationID = o.OperationID
	}
	report := func(step, state string) {
		if o.Progress != nil {
			o.Progress(step, state, j)
		}
	}
	// step runs one journaled step. A failing run may name the state to
	// record instead of failed.
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
			if state == "" {
				state = "failed"
			}
			j.State = "failed"
		}
		j.Steps[name] = state
		report(name, j.Steps[name])
		return errors.Join(e, save(context.WithoutCancel(ctx)))
	}
	// An incomplete fence step (running or failed) attempted a stop, even
	// when FenceStarted was never journaled.
	if f := j.Steps[StepFence]; f == "running" || f == "failed" {
		j.FenceStarted = true
	}
	stopAttempted := false
	if err = step(StepFence, func() (string, error) {
		if o.Old.Host == nil {
			if o.OldPrimaryDown {
				return "skipped", nil
			}
			return "", fmt.Errorf("Server %s of the old primary is not connected", o.Old.Server)
		}
		j.TargetLSN, j.Unclean = "", nil
		// Recheck replication under the locks while the old primary runs.
		running, e := deploy.RunningContainers(ctx, o.Old.Host, o.App, o.Destination, o.Old.Service)
		if e != nil {
			return "", e
		}
		if len(running) > 0 && !o.OldPrimaryDown {
			if _, j.Timeline, e = checkReplication(ctx, o); e != nil {
				return "", e
			}
		}
		earlier := j.FenceStarted
		j.FenceStarted = true
		if e := save(ctx); e != nil {
			return "", e
		}
		stopAttempted = true
		res, e := deploy.StopService(ctx, o.Old.Host, o.App, o.Destination, o.Old.Service, deploy.StoppedEntry{By: o.Performer, At: time.Now().UTC(), Reason: "db promote " + o.Connection}, deploy.StopOptions{AfterStop: ControlDataScript})
		if e != nil {
			return "", e
		}
		entry, _, e := fence(ctx, o)
		if e != nil {
			return "", e
		}
		j.FenceAt = entry.At
		if len(res.Containers) == 0 && earlier {
			j.Unclean = append(j.Unclean, "its containers were removed by an earlier attempt before a clean shutdown was verified")
		}
		for _, id := range res.Containers {
			if code := res.ExitCodes[id]; code != 0 {
				j.Unclean = append(j.Unclean, fmt.Sprintf("%s exited %d", id, code))
			} else if cd := ParseControlData(res.AfterStop[id]); cd.State != "shut down" {
				j.Unclean = append(j.Unclean, fmt.Sprintf("%s cluster state %q", id, cd.State))
			}
		}
		if len(j.Unclean) > 0 {
			if o.OldPrimaryDown {
				j.OldPrimaryDown = true
				return "skipped", nil
			}
			return "unclean", UncleanError(j)
		}
		switch len(res.Containers) {
		case 0:
			// Already stopped and removed before this run: no target.
			return "complete", nil
		case 1:
		default:
			return "", fmt.Errorf("old primary %s ran %d containers; a Postgres primary runs one", o.Old.Service, len(res.Containers))
		}
		cd := ParseControlData(res.AfterStop[res.Containers[0]])
		if _, e := ParseLSN(cd.Checkpoint); e != nil {
			return "", fmt.Errorf("pg_controldata of %s: latest checkpoint location: %w", o.Old.Service, e)
		}
		newID, e := o.New.SystemID(ctx)
		if e != nil {
			return "", e
		}
		if cd.SystemID != newID {
			return "", sameClusterError(o, cd.SystemID, newID)
		}
		if j.Timeline != "" && cd.Timeline != j.Timeline {
			return "", fmt.Errorf("pg_controldata of %s: timeline %s, but it was on timeline %s before the fence", o.Old.Service, or(cd.Timeline, "none"), j.Timeline)
		}
		j.TargetLSN = cd.Checkpoint
		return "complete", nil
	}); err != nil {
		if stopAttempted && j.Steps[StepFence] != "unclean" {
			return fmt.Errorf("%w\nhint: %s", err, FenceHint(o.Old.Service))
		}
		return err
	}
	if err = step(StepWait, func() (string, error) {
		if e := waitReplay(ctx, o, j.TargetLSN); e != nil {
			return "", e
		}
		return "complete", nil
	}); err != nil {
		if j.OldPrimaryDown {
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
	j.State = "promoted"
	return save(ctx)
}

// waitReplay polls the standby. With a target (the old primary's shutdown
// checkpoint) it waits until replay reaches it; receive is not consulted,
// it may be NULL once the old primary is gone. Without one it waits until
// replay is stable for two polls and either everything received is
// replayed or the WAL receiver is not streaming.
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
	last, prev := "", ""
	for {
		recv, replay, err := o.New.ReceiveReplay(ctx)
		if err != nil {
			return err
		}
		if target != "" {
			if r, _ := ParseLSN(replay); replay != "" && r >= want {
				return nil
			}
		} else if replay != "" && replay == prev {
			if recv == replay {
				return nil
			}
			status, err := o.New.ReceiverStatus(ctx)
			if err != nil {
				return err
			}
			if status != "streaming" {
				return nil
			}
		}
		prev = replay
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
