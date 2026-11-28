package pgpromote

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// pgHost is a Server with one fake Postgres. Files are real (under a temp
// release.Root); docker scripts are answered from the fake state.
type pgHost struct {
	remote.Local
	name string
	mu   sync.Mutex
	// state
	recovery     bool
	receiver     string
	current      string
	receive      string
	replay       []string // successive pg_last_wal_replay_lsn answers
	promoteOut   string
	stopOut      string
	controlData  string // pg_controldata output after the stop
	sysid        string
	running      string // docker ps -q answer
	noReceive    bool   // pg_last_wal_receive_lsn() is NULL
	tli          string // pg_control_checkpoint().timeline_id
	recvTLI      string // pg_stat_wal_receiver.received_tli
	failCheck    error  // the stop script's after-stop check fails (containers kept)
	failQuery    error
	log          []string
	writes       int
	promoteCalls int
}

func (h *pgHost) Name() string { return h.name }

func (h *pgHost) WriteFile(ctx context.Context, p string, b []byte, m os.FileMode, sudo bool) error {
	h.mu.Lock()
	h.writes++
	h.mu.Unlock()
	return h.Local.WriteFile(ctx, p, b, m, sudo)
}

func (h *pgHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := h.Output(ctx, c)
	return err
}

func (h *pgHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	s := c.Script
	if !strings.Contains(s, "docker") {
		return h.Local.Output(ctx, c)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case strings.Contains(s, "docker stop $ids"):
		h.log = append(h.log, "stop")
		if h.failCheck != nil && strings.Contains(s, "pg_controldata") {
			h.running = ""
			return "", h.failCheck
		}
		out := h.stopOut
		if strings.Contains(s, "pg_controldata") && h.controlData != "" && out != "" {
			id, _, _ := strings.Cut(out, " ")
			for _, l := range strings.Split(h.controlData, "\n") {
				out += "\n" + id + "| " + l
			}
		}
		h.running = ""
		return out, nil
	case strings.Contains(s, "docker ps -q "):
		return h.running, nil
	case strings.Contains(s, "'psql'"):
		if h.failQuery != nil {
			return "", h.failQuery
		}
		switch {
		case strings.Contains(s, "pg_promote(true, 60)"):
			h.log = append(h.log, "promote")
			h.promoteCalls++
			h.recovery = false
			return h.promoteOut, nil
		case strings.Contains(s, "pg_is_in_recovery()"):
			return map[bool]string{true: "t", false: "f"}[h.recovery], nil
		case strings.Contains(s, "received_tli"):
			if h.receiver == "" {
				return "", nil
			}
			return or(h.recvTLI, "1"), nil
		case strings.Contains(s, "pg_control_checkpoint()"):
			return or(h.tli, "1"), nil
		case strings.Contains(s, "pg_stat_wal_receiver"):
			return h.receiver, nil
		case strings.Contains(s, "pg_control_system()"):
			if h.sysid == "" {
				return "7000000000000000001", nil
			}
			return h.sysid, nil
		case strings.Contains(s, "pg_current_wal_lsn()"):
			h.log = append(h.log, "current")
			return h.current, nil
		case strings.Contains(s, "pg_last_wal_replay_lsn()"):
			h.log = append(h.log, "replay")
			r := h.replay[0]
			if len(h.replay) > 1 {
				h.replay = h.replay[1:]
			}
			recv := h.receive
			if h.noReceive {
				recv = ""
			} else if recv == "" {
				recv = r // streaming: replay keeps up with what arrived
			}
			return strings.TrimSpace(recv + "|" + r), nil
		}
	}
	return "", nil
}

func fixture(t *testing.T) (*pgHost, *pgHost, Options) {
	t.Helper()
	old := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = old })
	a := &pgHost{name: "a", recovery: false, current: "0/3000060", stopOut: "c1 0", controlData: "Database system identifier:           7000000000000000001\nDatabase cluster state:               shut down\nLatest checkpoint location:           0/3000080\nLatest checkpoint's TimeLineID:       1", running: "c1", receive: "0/3000000", replay: []string{"0/3000000"}}
	b := &pgHost{name: "b", recovery: true, receiver: "streaming", replay: []string{"0/3000000", "0/3000060", "0/3000100"}, promoteOut: "t"}
	o := Options{
		App: "pg", Destination: "production", Connection: "primary",
		Old:     Node{Server: "a", Service: "db", Project: "yoho-pg-production", User: "postgres", Host: a},
		New:     Node{Server: "b", Service: "db-standby", Project: "yoho-pg-production", User: "postgres", Host: b},
		MaxLag:  16 << 20,
		Timeout: time.Second,
		Poll:    time.Millisecond,
	}
	return a, b, o
}

func TestPsqlScriptUsesLocalSocketWithoutSecrets(t *testing.T) {
	n := Node{Service: "db", Project: "yoho-pg-production", User: "app user"}
	want := "set -eu\ndocker compose '-p' 'yoho-pg-production' 'exec' '-T' 'db' 'psql' '-X' '-At' '-v' 'ON_ERROR_STOP=1' '-U' 'app user' '-d' 'postgres' '-c' 'SELECT 1'"
	if got := n.PsqlScript("SELECT 1"); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestParseLSN(t *testing.T) {
	for in, want := range map[string]uint64{"0/3000060": 0x3000060, "1/0": 1 << 32, "16/B374D848": 0x16<<32 | 0xB374D848} {
		if got, err := ParseLSN(in); err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0", "0/30;rm -rf /", "abc", "0/3000060\n1/2", "0/xyz", "100000000/0"} {
		if _, err := ParseLSN(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPreflightIsReadOnlyAndChecksLag(t *testing.T) {
	a, b, o := fixture(t)
	st, err := Preflight(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if st.Lag != 0x60 || st.Fenced || st.Promoted {
		t.Fatalf("%+v", st)
	}
	if a.writes+b.writes != 0 || strings.Contains(strings.Join(append(a.log, b.log...), ","), "stop") || b.promoteCalls != 0 {
		t.Fatalf("preflight mutated: %v %v", a.log, b.log)
	}
	o.MaxLag = 0x10
	b.replay = []string{"0/3000000"}
	if _, err := Preflight(context.Background(), o); err == nil || !strings.Contains(err.Error(), "96 bytes behind") {
		t.Fatal(err)
	}
	o.MaxLag = 16 << 20
	b.receiver = ""
	if _, err := Preflight(context.Background(), o); err == nil || !strings.Contains(err.Error(), "not streaming") {
		t.Fatal(err)
	}
	b.receiver = "streaming"
	a.recovery = true
	if _, err := Preflight(context.Background(), o); err == nil || !strings.Contains(err.Error(), "not the primary") {
		t.Fatal(err)
	}
	a.recovery = false
	b.recovery = false
	if _, err := Preflight(context.Background(), o); err == nil || !strings.Contains(err.Error(), "already a primary while db still runs") {
		t.Fatal(err)
	}
}

func TestRunFencesWaitsAndPromotesInOrder(t *testing.T) {
	a, b, o := fixture(t)
	var steps []string
	o.Progress = func(step, state string, _ *Journal) { steps = append(steps, step+":"+state) }
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	// Replication is rechecked under the locks (current), then the old
	// primary is stopped. The target is the shutdown checkpoint (0/3000080)
	// read after the clean stop, not pg_current_wal_lsn() (0/3000060).
	if got := strings.Join(a.log, ","); got != "current,stop" {
		t.Fatalf("old primary: %s", got)
	}
	if b.promoteCalls != 1 || b.recovery {
		t.Fatal("not promoted")
	}
	if got := strings.Join(steps, ","); got != "fence:running,fence:complete,wait-replay:running,wait-replay:complete,promote:running,promote:complete" {
		t.Fatal(got)
	}
	j, err := ReadJournal(context.Background(), b, "pg", "production", "primary")
	if err != nil || j.State != "promoted" || j.TargetLSN != "0/3000080" || j.From != "db" || j.To != "db-standby" || j.FromServer != "a" {
		t.Fatalf("%+v %v", j, err)
	}
	stopped, _ := deploy.ReadStopped(context.Background(), a, "pg", "production")
	if !strings.HasPrefix(stopped["db"].Reason, "db promote") {
		t.Fatalf("fence did not record app stop: %+v", stopped)
	}
	// A rerun before the caller's apply deleted the journal changes nothing.
	a.log, b.log = nil, nil
	if err := Run(context.Background(), o); err != nil || len(a.log)+len(b.log) != 0 {
		t.Fatalf("rerun: %v %v %v", err, a.log, b.log)
	}
	if st, err := Preflight(context.Background(), o); err != nil || !st.Promoted || !st.Fenced {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestRunRefusesUncleanShutdown(t *testing.T) {
	a, b, o := fixture(t)
	a.stopOut = "c1 137"
	err := Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "did not shut down cleanly") || !strings.Contains(err.Error(), "`yoho app start db` restores the old primary") {
		t.Fatal(err)
	}
	if b.promoteCalls != 0 {
		t.Fatal("promoted after an unclean shutdown")
	}
	j, _ := ReadJournal(context.Background(), b, "pg", "production", "primary")
	if j.Steps[StepFence] != "unclean" || j.State != "failed" || len(j.Unclean) != 1 || j.Unclean[0] != "c1 exited 137" {
		t.Fatalf("%+v", j)
	}
}

func TestRunTimesOutThenResumesFromJournal(t *testing.T) {
	a, b, o := fixture(t)
	b.replay = []string{"0/3000000"} // never reaches the target
	b.receive = "0/3000000"
	o.Timeout = 20 * time.Millisecond
	err := Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "did not replay") || !strings.Contains(err.Error(), "Rerun to continue") {
		t.Fatal(err)
	}
	if b.promoteCalls != 0 {
		t.Fatal("promoted before replay caught up")
	}
	j, _ := ReadJournal(context.Background(), b, "pg", "production", "primary")
	if j.Steps[StepFence] != "complete" || j.Steps[StepWait] != "failed" || j.TargetLSN != "0/3000080" {
		t.Fatalf("%+v", j)
	}
	// Resume: no second fence (the old primary is down), wait, promote.
	a.log = nil
	b.replay = []string{"0/3000080"}
	b.receive = "0/3000080"
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(a.log) != 0 || b.promoteCalls != 1 {
		t.Fatalf("resume: old %v, promotes %d", a.log, b.promoteCalls)
	}
}

func TestRunResumesAfterPromoteBeforeJournalCompleted(t *testing.T) {
	a, b, o := fixture(t)
	j := &Journal{Connection: "primary", From: "db", To: "db-standby", Steps: map[string]string{StepFence: "complete", StepWait: "complete", StepPromote: "running"}, State: "running"}
	if err := writeJournal(context.Background(), b, "pg", "production", j); err != nil {
		t.Fatal(err)
	}
	b.recovery = false // pg_promote ran, the journal did not record it
	if _, err := deploy.StopService(context.Background(), a, "pg", "production", "db", deploy.StoppedEntry{By: "alice"}); err != nil {
		t.Fatal(err)
	}
	a.log = nil
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if b.promoteCalls != 0 || len(a.log) != 0 {
		t.Fatalf("repeated work: promotes %d, old %v", b.promoteCalls, a.log)
	}
}

func TestRunOldPrimaryDownSkipsFence(t *testing.T) {
	a, b, o := fixture(t)
	o.OldPrimaryDown = true
	o.Old.Host = nil
	b.receiver = ""
	b.receive = "0/3000100"
	b.replay = []string{"0/3000060", "0/3000100"}
	if _, err := Preflight(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	j, _ := ReadJournal(context.Background(), b, "pg", "production", "primary")
	if j.Steps[StepFence] != "skipped" || !j.OldPrimaryDown || j.TargetLSN != "" || b.promoteCalls != 1 || len(a.log) != 0 {
		t.Fatalf("%+v old=%v", j, a.log)
	}
}

func TestUnfinishedOtherPromotionRefused(t *testing.T) {
	_, b, o := fixture(t)
	j := &Journal{Connection: "primary", From: "db-standby", To: "db", Steps: map[string]string{StepFence: "complete"}, State: "failed"}
	if err := writeJournal(context.Background(), b, "pg", "production", j); err != nil {
		t.Fatal(err)
	}
	if _, err := Preflight(context.Background(), o); err == nil || !strings.Contains(err.Error(), "unfinished promotion of db-standby → db") {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err == nil {
		t.Fatal("ran over an unfinished promotion")
	}
	if p := JournalPath("pg", "production", "primary"); filepath.Base(p) != "primary.json" || !strings.Contains(p, "/promotions/") {
		t.Fatal(p)
	}
}

func TestQueryErrorsAreReported(t *testing.T) {
	_, b, o := fixture(t)
	b.failQuery = &remote.ExitError{Code: 2, Stderr: "psql: FATAL: role \"postgres\" does not exist"}
	_, err := Preflight(context.Background(), o)
	var ee *remote.ExitError
	if !errors.As(err, &ee) || !strings.Contains(fmt.Sprint(err), "db-standby on b") {
		t.Fatal(err)
	}
}

func freshStart(t *testing.T, a *pgHost) {
	t.Helper()
	// `yoho app start db`: the record is gone and the container runs.
	if err := os.Remove(filepath.Join(release.AppDir("pg", "production"), deploy.StoppedFile)); err != nil {
		t.Fatal(err)
	}
	a.running = "c2"
}

// Item: a journal is a retry marker. A promoted journal left behind must
// not swallow a later switchover with the same from/to: when the new
// primary is in recovery again, the journal is stale.
func TestPromotedJournalIsNotReusedForANewSwitchover(t *testing.T) {
	a, b, o := fixture(t)
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	// B → A by hand, then A → B again: b is a standby, a runs.
	freshStart(t, a)
	a.recovery, b.recovery = false, true
	b.replay = []string{"0/3000080"}
	if st, err := Preflight(context.Background(), o); err != nil || st.Journal != nil || st.Promoted || st.Fenced {
		t.Fatalf("stale journal trusted: %+v %v", st, err)
	}
	a.log = nil
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if strings.Join(a.log, ",") != "current,stop" || b.promoteCalls != 2 {
		t.Fatalf("second switchover skipped: old %v, promotes %d", a.log, b.promoteCalls)
	}
	if err := DeleteJournal(context.Background(), b, "pg", "production", "primary"); err != nil {
		t.Fatal(err)
	}
	if j, err := ReadJournal(context.Background(), b, "pg", "production", "primary"); err != nil || j != nil {
		t.Fatalf("journal kept: %+v %v", j, err)
	}
}

// Item: the fence is re-verified from Server state on every run. After
// `yoho app start db` the journal's fence=complete is not trusted: fence
// again and take a fresh target.
func TestResumeRefencesAfterOldPrimaryStarted(t *testing.T) {
	a, b, o := fixture(t)
	b.replay = []string{"0/3000000"}
	b.receive = "0/3000000"
	o.Timeout = 20 * time.Millisecond
	if err := Run(context.Background(), o); err == nil {
		t.Fatal("wait should time out")
	}
	freshStart(t, a)
	if st, err := Preflight(context.Background(), o); err != nil || st.Fenced {
		t.Fatalf("journal fence trusted over Server state: %+v %v", st, err)
	}
	a.log = nil
	a.controlData = strings.Replace(a.controlData, "0/3000080", "0/3000090", 1)
	b.replay = []string{"0/3000090"}
	b.receive = "0/3000090"
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	j, _ := ReadJournal(context.Background(), b, "pg", "production", "primary")
	if strings.Join(a.log, ",") != "current,stop" || j.TargetLSN != "0/3000090" || b.promoteCalls != 1 {
		t.Fatalf("no re-fence: old %v, %+v", a.log, j)
	}
}

// Item: an unclean shutdown is remembered: the retry refuses it unless
// --old-primary-down accepts the loss.
func TestUncleanFenceIsRememberedOnRetry(t *testing.T) {
	a, b, o := fixture(t)
	a.stopOut = "c1 137"
	if err := Run(context.Background(), o); err == nil {
		t.Fatal("unclean accepted")
	}
	a.log = nil
	if _, err := Preflight(context.Background(), o); err == nil || !strings.Contains(err.Error(), "did not shut down cleanly (c1 exited 137)") || !strings.Contains(err.Error(), "--old-primary-down") {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "did not shut down cleanly") {
		t.Fatal(err)
	}
	if len(a.log) != 0 || b.promoteCalls != 0 {
		t.Fatalf("retry went on: old %v, promotes %d", a.log, b.promoteCalls)
	}
	o.OldPrimaryDown = true
	b.receiver = ""
	if _, err := Preflight(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	j, _ := ReadJournal(context.Background(), b, "pg", "production", "primary")
	if b.promoteCalls != 1 || len(a.log) != 0 || j.Steps[StepFence] != "skipped" || !j.OldPrimaryDown || j.TargetLSN != "" {
		t.Fatalf("old %v, %+v", a.log, j)
	}
}

// Item: the target is the stopped old primary's shutdown checkpoint, which
// must exist; a cluster that is not "shut down" is unclean.
func TestFenceReadsShutdownCheckpoint(t *testing.T) {
	a, b, o := fixture(t)
	a.controlData = strings.Replace(a.controlData, "shut down", "in production", 1)
	if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), `cluster state "in production"`) {
		t.Fatal(err)
	}
	a, b, o = fixture(t)
	a.controlData = strings.Replace(a.controlData, "0/3000080", "0/30;x", 1)
	if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "invalid LSN") || b.promoteCalls != 0 {
		t.Fatal(err)
	}
	cd := ParseControlData("pg_control version number:            1700\nDatabase system identifier:           7\nDatabase cluster state:               shut down\nLatest checkpoint location:           0/1A2B3C")
	if cd != (ControlData{State: "shut down", Checkpoint: "0/1A2B3C", SystemID: "7"}) {
		t.Fatalf("%+v", cd)
	}
	if !strings.Contains(ControlDataScript, "--volumes-from \"$id\"") || !strings.Contains(ControlDataScript, "/var/lib/postgresql/data") {
		t.Fatal(ControlDataScript)
	}
}

// Item: with a target only replay counts; receive may be NULL.
func TestWaitReplayTargetIgnoresNullReceive(t *testing.T) {
	_, b, o := fixture(t)
	b.noReceive = true
	b.replay = []string{"0/3000080"}
	if err := waitReplay(context.Background(), o, "0/3000080"); err != nil {
		t.Fatal(err)
	}
}

// Item: without a target, wait until replay is stable for two polls and
// receive == replay or the WAL receiver is not streaming.
func TestWaitReplayWithoutTarget(t *testing.T) {
	_, b, o := fixture(t)
	o.Timeout = 20 * time.Millisecond
	b.receive = "0/3000100"
	b.replay = []string{"0/3000060"}
	b.receiver = "streaming"
	if err := waitReplay(context.Background(), o, ""); err == nil {
		t.Fatal("returned while still streaming behind")
	}
	b.receiver = ""
	if err := waitReplay(context.Background(), o, ""); err != nil {
		t.Fatal(err)
	}
	b.receiver = "streaming"
	b.noReceive = true
	if err := waitReplay(context.Background(), o, ""); err == nil {
		t.Fatal("NULL receive while streaming accepted")
	}
}

// Item: the standby must belong to the old primary's cluster.
func TestPreflightRefusesAnotherCluster(t *testing.T) {
	a, b, o := fixture(t)
	a.sysid = "7000000000000000002"
	if _, err := Preflight(context.Background(), o); err == nil || !strings.Contains(err.Error(), "different clusters") {
		t.Fatal(err)
	}
	a.sysid = ""
	a.controlData = strings.Replace(a.controlData, "7000000000000000001", "7000000000000000002", 1)
	if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "different clusters") || b.promoteCalls != 0 {
		t.Fatal(err)
	}
}

// S2: a fence is identified by its stop record. Started and stopped again
// after a failed promotion, the old primary has another record: the old
// target predates the restart, and with its containers gone no fresh one
// can be read, so the retry refuses as unclean.
func TestFenceIdentityMismatchIsNotReused(t *testing.T) {
	a, b, o := fixture(t)
	b.replay = []string{"0/3000000"}
	b.receive = "0/3000000"
	o.Timeout = 20 * time.Millisecond
	if err := Run(context.Background(), o); err == nil {
		t.Fatal("wait should time out")
	}
	j, _ := ReadJournal(context.Background(), b, "pg", "production", "primary")
	if j.FenceAt.IsZero() || !j.FenceStarted {
		t.Fatalf("fence identity not journaled: %+v", j)
	}
	// `yoho app start db`, writes, then `yoho app stop db`.
	freshStart(t, a)
	a.stopOut = "c2 0"
	if _, err := deploy.StopService(context.Background(), a, "pg", "production", "db", deploy.StoppedEntry{By: "bob", At: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	a.stopOut, a.log = "", nil
	b.replay = []string{"0/3000080"}
	b.receive = "0/3000080"
	if st, err := Preflight(context.Background(), o); err != nil || st.Fenced {
		t.Fatalf("another stop record trusted as the journal's fence: %+v %v", st, err)
	}
	err := Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "did not shut down cleanly") || !strings.Contains(err.Error(), "removed by an earlier attempt") || b.promoteCalls != 0 {
		t.Fatalf("%v, promotes %d", err, b.promoteCalls)
	}
}

// S2: a promoted journal resumes at apply only while the old primary stays
// fenced.
func TestPromotedJournalChecksFence(t *testing.T) {
	a, _, o := fixture(t)
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	freshStart(t, a)
	if _, err := Preflight(context.Background(), o); err == nil || !strings.Contains(err.Error(), "runs again") {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "runs again") {
		t.Fatal(err)
	}
	o.Old.Host = nil // the old Server is gone
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
}

// S3: an attempt that died after docker rm but before journaling the
// result leaves no container and no target: the retry is unclean.
func TestFenceStartedWithoutContainersIsUnclean(t *testing.T) {
	a, b, o := fixture(t)
	if _, err := deploy.StopService(context.Background(), a, "pg", "production", "db", deploy.StoppedEntry{By: "yoho", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	j := &Journal{Connection: "primary", From: "db", To: "db-standby", FenceStarted: true, Steps: map[string]string{StepFence: "running"}, State: "running"}
	if err := writeJournal(context.Background(), b, "pg", "production", j); err != nil {
		t.Fatal(err)
	}
	a.stopOut = ""
	err := Run(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "removed by an earlier attempt") || b.promoteCalls != 0 {
		t.Fatalf("%v, promotes %d", err, b.promoteCalls)
	}
	if j, _ = ReadJournal(context.Background(), b, "pg", "production", "primary"); j.Steps[StepFence] != "unclean" || j.TargetLSN != "" {
		t.Fatalf("%+v", j)
	}
	// --old-primary-down accepts the loss.
	o.OldPrimaryDown = true
	b.receiver = ""
	if err := Run(context.Background(), o); err != nil || b.promoteCalls != 1 {
		t.Fatalf("%v, promotes %d", err, b.promoteCalls)
	}
}

// S3: a failed pg_controldata check keeps the stopped containers, so the
// retry reads the shutdown checkpoint.
func TestFailedControlDataCheckKeepsContainers(t *testing.T) {
	a, b, o := fixture(t)
	a.failCheck = errors.New("check of stopped container c1 failed; containers kept")
	if err := Run(context.Background(), o); err == nil || b.promoteCalls != 0 {
		t.Fatal(err)
	}
	j, _ := ReadJournal(context.Background(), b, "pg", "production", "primary")
	if j.Steps[StepFence] != "failed" || j.TargetLSN != "" {
		t.Fatalf("%+v", j)
	}
	a.failCheck = nil // c1 is still there, stopped
	if err := Run(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if j, _ = ReadJournal(context.Background(), b, "pg", "production", "primary"); j.TargetLSN != "0/3000080" || b.promoteCalls != 1 {
		t.Fatalf("%+v", j)
	}
}

// S4: the standby must receive the old primary's timeline; Run rechecks
// replication under the locks; the stopped data directory must still be
// on that timeline.
func TestTimelineChecks(t *testing.T) {
	a, b, o := fixture(t)
	a.tli = "2"
	if _, err := Preflight(context.Background(), o); err == nil || !strings.Contains(err.Error(), "receives timeline 1, but db is on timeline 2") {
		t.Fatal(err)
	}
	if err := Run(context.Background(), o); err == nil || strings.Contains(strings.Join(a.log, ","), "stop") {
		t.Fatalf("Run fenced without rechecking: %v %v", err, a.log)
	}

	a, b, o = fixture(t)
	if _, err := Preflight(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	b.receiver = "" // stopped streaming after the unlocked preflight
	if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "not streaming") || strings.Contains(strings.Join(a.log, ","), "stop") {
		t.Fatalf("%v %v", err, a.log)
	}

	a, b, o = fixture(t)
	a.controlData = strings.Replace(a.controlData, "TimeLineID:       1", "TimeLineID:       3", 1)
	if err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "timeline 3, but it was on timeline 1") || b.promoteCalls != 0 {
		t.Fatal(err)
	}
}

// P2: a secret role never reaches argv; it is resolved in the container.
func TestPsqlScriptSecretUser(t *testing.T) {
	n := Node{Service: "db", Project: "yoho-pg-production", UserSecret: UserFromFile}
	want := "set -eu\ndocker compose '-p' 'yoho-pg-production' 'exec' '-T' 'db' 'sh' '-c' 'PGUSER=\"$(cat /run/secrets/POSTGRES_USER)\" exec psql -X -At -v ON_ERROR_STOP=1 -d postgres -c \"$1\"' 'sh' 'SELECT 1'"
	if got := n.PsqlScript("SELECT 1"); got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
	n.UserSecret = UserFromEnv
	want = "set -eu\ndocker compose '-p' 'yoho-pg-production' 'exec' '-T' 'db' 'sh' '-c' 'PGUSER=\"$POSTGRES_USER\" exec psql -X -At -v ON_ERROR_STOP=1 -d postgres -c \"$1\"' 'sh' 'SELECT 1'"
	if got := n.PsqlScript("SELECT 1"); got != want || strings.Contains(got, "-U") {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}
