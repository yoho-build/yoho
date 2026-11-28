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
		return h.stopOut, nil
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
		case strings.Contains(s, "pg_stat_wal_receiver"):
			return h.receiver, nil
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
			if recv == "" {
				recv = r // streaming: replay keeps up with what arrived
			}
			return recv + " " + r, nil
		}
	}
	return "", nil
}

func fixture(t *testing.T) (*pgHost, *pgHost, Options) {
	t.Helper()
	old := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = old })
	a := &pgHost{name: "a", recovery: false, current: "0/3000060", stopOut: "c1 0", receive: "0/3000000", replay: []string{"0/3000000"}}
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
	if err != nil || j.State != "complete" || j.TargetLSN != "0/3000060" || j.From != "db" || j.To != "db-standby" || j.FromServer != "a" {
		t.Fatalf("%+v %v", j, err)
	}
	stopped, _ := deploy.ReadStopped(context.Background(), a, "pg", "production")
	if !strings.HasPrefix(stopped["db"].Reason, "db promote") {
		t.Fatalf("fence did not record app stop: %+v", stopped)
	}
	// A rerun after a complete promotion changes nothing.
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
	if j.Steps[StepFence] != "failed" || j.State != "failed" {
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
	if j.Steps[StepFence] != "complete" || j.Steps[StepWait] != "failed" || j.TargetLSN != "0/3000060" {
		t.Fatalf("%+v", j)
	}
	// Resume: no second fence (the old primary is down), wait, promote.
	a.log = nil
	b.replay = []string{"0/3000060"}
	b.receive = "0/3000060"
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
