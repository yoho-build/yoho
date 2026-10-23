package gate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// A rollout is a Gate deploy or rollback that outlives the Member's SSH
// session (ADR 0011): sshd and sudo may kill the session process, but the
// worker is in its own session and writes the Release status itself.
//
// Files under <access>/rollout/:
//
//	request.json   GateRequest line the worker re-reads
//	state.json     preparing | running | deployed | failed | rolled_back
//	log            NDJSON Gate frames from the worker
//	done/<id>.json final state of a finished rollout, for a late attach

const (
	// The Gate that took the request is pulling and checking images;
	// its pid is the one recorded. An attach waits for it.
	rolloutStatusPreparing = "preparing"
	rolloutStatusRunning   = "running"
	rolloutPollInterval    = 100 * time.Millisecond
	rolloutDoneRetention   = 24 * time.Hour
)

var rolloutIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// errDetached means the client went away while a worker is still running.
// Serve exits 0 without a result frame; the client reattaches.
var errDetached = errors.New("client detached")

// rolloutJob is what StartRolloutWorker needs to spawn the worker.
type rolloutJob struct {
	Options Options
	App     string
	Dest    string
}

// StartRolloutWorker starts a rollout that outlives the SSH session.
// Nil (the default) runs StartRolloutInProcess. The CLI sets this to
// StartDetachedProcess so sshd/sudo cannot kill the worker.
var StartRolloutWorker func(rolloutJob) (int, error)

func startWorker(job rolloutJob) (int, error) {
	fn := StartRolloutWorker
	if fn == nil {
		fn = StartRolloutInProcess
	}
	return fn(job)
}

// rolloutState is <access>/rollout/state.json.
type rolloutState struct {
	ID      string `json:"id,omitempty"`
	Verb    string `json:"verb"`
	Version string `json:"version"`
	Member  string `json:"member"`
	Key     string `json:"key,omitempty"`
	PID     int    `json:"pid"`
	// Start is the pid's start time (/proc/<pid>/stat), so a reused pid
	// does not keep a dead rollout alive.
	Start   string          `json:"start,omitempty"`
	Boot    string          `json:"boot,omitempty"`
	Started time.Time       `json:"started"`
	Status  string          `json:"status"`
	Error   string          `json:"error,omitempty"`
	Hint    string          `json:"hint,omitempty"`
	Code    string          `json:"code,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	// Unstarted: it ended while preparing, so the log is not its own.
	Unstarted bool `json:"unstarted,omitempty"`
}

func rolloutDir(app, dest string) string {
	return filepath.Join(release.AccessDir(app, dest), "rollout")
}

func rolloutRequestPath(app, dest string) string {
	return filepath.Join(rolloutDir(app, dest), "request.json")
}

func rolloutStatePath(app, dest string) string {
	return filepath.Join(rolloutDir(app, dest), "state.json")
}

func rolloutLogPath(app, dest string) string {
	return filepath.Join(rolloutDir(app, dest), "log")
}

func rolloutDonePath(app, dest, id string) string {
	return filepath.Join(rolloutDir(app, dest), "done", id+".json")
}

// readRolloutDone is the final state of rollout id, if it finished.
func readRolloutDone(app, dest, id string) *rolloutState {
	b, err := os.ReadFile(rolloutDonePath(app, dest, id))
	if err != nil {
		return nil
	}
	var st rolloutState
	if json.Unmarshal(b, &st) != nil {
		return nil
	}
	return &st
}

// pruneRolloutDone removes finished-rollout records older than a day.
func pruneRolloutDone(app, dest string, now time.Time) {
	dir := filepath.Dir(rolloutDonePath(app, dest, "x"))
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > rolloutDoneRetention {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

func rolloutLockPath(app, dest string) string {
	return filepath.Join(release.AccessDir(app, dest), "rollout.lock")
}

// StartRolloutInProcess runs the worker in a goroutine of this process.
// Used by tests and as a fallback. The live Gate replaces this with
// StartDetachedProcess so sshd/sudo cannot kill the rollout.
func StartRolloutInProcess(job rolloutJob) (int, error) {
	go func() { _ = RunWorker(context.Background(), job.Options, job.App, job.Dest) }()
	return os.Getpid(), nil
}

// RunWorker is `yoho gate --rollout app/dest`: read the stored request,
// write frames to the rollout log, record the final state.
func RunWorker(ctx context.Context, o Options, app, dest string) int {
	o.RolloutWorker = true
	req, err := os.Open(rolloutRequestPath(app, dest))
	if err != nil {
		writeRolloutFailed(app, dest, err)
		return ExitFailed
	}
	defer req.Close()
	log, err := os.OpenFile(rolloutLogPath(app, dest), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		writeRolloutFailed(app, dest, err)
		return ExitFailed
	}
	defer log.Close()
	o.In, o.Out = req, log
	updateRolloutState(app, dest, func(st *rolloutState) {
		st.PID, st.Start = os.Getpid(), procStart(os.Getpid())
		if o.Boot != "" {
			st.Boot = o.Boot
		}
	})
	return Serve(ctx, o)
}

func writeRolloutFailed(app, dest string, err error) {
	updateRolloutState(app, dest, func(st *rolloutState) {
		st.Status = "failed"
		st.Error = err.Error()
		st.Code = "failed"
	})
}

func finishRolloutState(app, dest string, result any, err error) {
	updateRolloutState(app, dest, func(st *rolloutState) { setRolloutOutcome(st, result, err) })
}

func setRolloutOutcome(st *rolloutState, result any, err error) {
	if err != nil {
		ge := gateError(err)
		st.Status = "failed"
		st.Error = ge.Message
		st.Hint = ge.Hint
		st.Code = ge.Code
		st.Result = nil
		return
	}
	st.Error, st.Hint, st.Code = "", "", ""
	switch r := result.(type) {
	case *release.Release:
		if r != nil {
			st.Status = r.Status
			st.Result, _ = json.Marshal(r)
		}
	case release.Release:
		st.Status = r.Status
		st.Result, _ = json.Marshal(r)
	default:
		if st.Status == rolloutStatusRunning || st.Status == "" {
			st.Status = "deployed"
		}
		st.Result, _ = json.Marshal(result)
	}
	if st.Status == "" || st.Status == rolloutStatusRunning {
		st.Status = "deployed"
	}
}

// updateRolloutState changes state.json under the rollout lock, so the
// worker and the Gate that started it never overwrite each other.
func updateRolloutState(app, dest string, fn func(*rolloutState)) {
	lock, err := lockRollout(app, dest)
	if err != nil {
		return
	}
	defer unlockRollout(lock)
	st, _ := readRolloutState(app, dest)
	if st == nil {
		st = &rolloutState{}
	}
	fn(st)
	_ = writeRolloutState(app, dest, st)
	if st.ID != "" && !rolloutPending(st) {
		p := rolloutDonePath(app, dest, st.ID)
		if os.MkdirAll(filepath.Dir(p), 0o700) == nil {
			if b, err := json.Marshal(st); err == nil {
				_ = os.WriteFile(p, b, 0o600)
			}
		}
	}
}

func readRolloutState(app, dest string) (*rolloutState, error) {
	b, err := os.ReadFile(rolloutStatePath(app, dest))
	if err != nil {
		return nil, err
	}
	var st rolloutState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	return &st, nil
}

func writeRolloutState(app, dest string, st *rolloutState) error {
	if err := os.MkdirAll(rolloutDir(app, dest), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := rolloutStatePath(app, dest) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, rolloutStatePath(app, dest))
}

// rolloutPending is a rollout that has not recorded an outcome yet.
func rolloutPending(st *rolloutState) bool {
	return st != nil && (st.Status == rolloutStatusPreparing || st.Status == rolloutStatusRunning)
}

// rolloutLive is a pending rollout whose process is still there.
func rolloutLive(st *rolloutState, boot string) bool {
	return rolloutPending(st) && rolloutAlive(st, boot)
}

func rolloutAlive(st *rolloutState, boot string) bool {
	if st == nil || st.PID <= 0 {
		return false
	}
	if st.Boot != "" && boot != "" && st.Boot != boot {
		return false
	}
	if st.Start != "" {
		if s := procStart(st.PID); s != "" && s != st.Start {
			return false
		}
	}
	if st.PID == os.Getpid() {
		return true
	}
	return gateRunning(st.PID)
}

// procStart is the start time of pid in clock ticks since boot ("" when
// /proc is not there).
func procStart(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	// Fields after the parenthesized command; starttime is field 22.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

func lockRollout(app, dest string) (*os.File, error) {
	if err := os.MkdirAll(release.AccessDir(app, dest), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(rolloutLockPath(app, dest), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func unlockRollout(f *os.File) {
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	f.Close()
}

// rolloutBusy is the error for a request while another rollout is live.
func rolloutBusy(st *rolloutState) error {
	return &remote.GateError{
		Message: fmt.Sprintf("another %s is running: %s %s since %s", st.Verb, st.Member, st.Version, st.Started.UTC().Format(time.RFC3339)),
		Hint:    "wait for it to finish, then see `yoho releases`",
		Code:    "locked",
	}
}

// checkRollout handles an Attach request. ok reports that the request
// was answered.
func (c *call) checkRollout(ctx context.Context, verb, id string, attach bool) (any, error, bool) {
	if id != "" && !rolloutIDRe.MatchString(id) {
		return nil, invalid(fmt.Sprintf("invalid rollout id %q", id)), true
	}
	if !attach {
		return nil, nil, false
	}
	st, _ := readRolloutState(c.req.App, c.req.Destination)
	if id != "" && (st == nil || st.ID != id) {
		if done := readRolloutDone(c.req.App, c.req.Destination, id); done != nil && done.Verb == verb {
			res, err := rolloutOutcome(done)
			return res, err, true
		}
	}
	if id == "" || st == nil || st.ID != id || st.Verb != verb {
		return nil, &remote.GateError{
			Message: "the " + verb + " did not start on the Server",
			Hint:    "see `yoho releases` and " + verb + " again",
			Code:    "failed",
		}, true
	}
	res, err := c.attachRollout(ctx, id)
	return res, err, true
}

// prepareRollout records rollout id as preparing by this Gate, before any
// image is pulled or checked, and refuses it while another one is live. A
// client whose session drops meanwhile attaches and waits for it instead
// of being told it did not start. Call abandonRollout when the request
// ends.
func (c *call) prepareRollout(verb, version, id string) (string, error) {
	lock, err := lockRollout(c.req.App, c.req.Destination)
	if err != nil {
		return "", fmt.Errorf("rollout lock: %w", err)
	}
	defer unlockRollout(lock)
	if st, err := readRolloutState(c.req.App, c.req.Destination); err == nil && rolloutLive(st, c.o.Boot) {
		return "", rolloutBusy(st)
	}
	if id == "" {
		id = rand.Text()
	}
	st := &rolloutState{
		ID: id, Verb: verb, Version: version, Member: c.o.Member, Key: c.o.Key,
		PID: os.Getpid(), Start: procStart(os.Getpid()), Boot: c.o.Boot,
		Started: c.o.Now().UTC(), Status: rolloutStatusPreparing,
	}
	if err := writeRolloutState(c.req.App, c.req.Destination, st); err != nil {
		return "", err
	}
	return id, nil
}

// abandonRollout records err as the outcome of rollout id if it never got
// past preparing (a failed pull or check, or a refused request).
func (c *call) abandonRollout(id string, err error) {
	if err == nil {
		err = errors.New("the rollout did not start")
	}
	updateRolloutState(c.req.App, c.req.Destination, func(st *rolloutState) {
		if st.ID == id && st.Status == rolloutStatusPreparing {
			setRolloutOutcome(st, nil, err)
			st.Unstarted = true
		}
	})
}

// startRollout starts the worker of prepared rollout id. The lock is held
// until the worker's pid is recorded; the worker's own state updates wait
// for it.
func (c *call) startRollout(verb, version, id string, args any) (string, error) {
	lock, err := lockRollout(c.req.App, c.req.Destination)
	if err != nil {
		return "", fmt.Errorf("rollout lock: %w", err)
	}
	defer unlockRollout(lock)
	if st, err := readRolloutState(c.req.App, c.req.Destination); err == nil && rolloutLive(st, c.o.Boot) &&
		(st.ID != id || st.Status != rolloutStatusPreparing) {
		return "", rolloutBusy(st)
	}
	pruneRolloutDone(c.req.App, c.req.Destination, c.o.Now())

	switch a := args.(type) {
	case DeployArgs:
		a.Archive = false
		a.Attach = false
		args = a
	case RollbackArgs:
		a.Attach = false
		args = a
	}
	line, err := remote.EncodeGateRequest(c.req.App, c.req.Destination, verb, args)
	if err != nil {
		return "", err
	}
	dir := rolloutDir(c.req.App, c.req.Destination)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(rolloutRequestPath(c.req.App, c.req.Destination), line, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(rolloutLogPath(c.req.App, c.req.Destination), nil, 0o600); err != nil {
		return "", err
	}
	st := &rolloutState{
		ID: id, Verb: verb, Version: version, Member: c.o.Member, Key: c.o.Key,
		Boot: c.o.Boot, Started: c.o.Now().UTC(), Status: rolloutStatusRunning,
		// This Gate stands in until the worker's pid is recorded, so an
		// attach polling meanwhile does not take the rollout for dead.
		PID: os.Getpid(), Start: procStart(os.Getpid()),
	}
	if err := writeRolloutState(c.req.App, c.req.Destination, st); err != nil {
		return "", err
	}
	pid, err := startWorker(rolloutJob{Options: c.o, App: c.req.App, Dest: c.req.Destination})
	if err != nil {
		st.Status = "failed"
		st.Error = err.Error()
		st.Code = "failed"
		_ = writeRolloutState(c.req.App, c.req.Destination, st)
		return "", err
	}
	if pid > 0 {
		st.PID, st.Start = pid, procStart(pid)
		_ = writeRolloutState(c.req.App, c.req.Destination, st)
	}
	c.audit["detached"] = true
	c.audit["rollout"] = id
	return id, nil
}

func (c *call) attachRollout(ctx context.Context, id string) (any, error) {
	c.watchClient()
	app, dest := c.req.App, c.req.Destination
	var (
		offset int64
		carry  []byte
	)
	ticker := time.NewTicker(rolloutPollInterval)
	defer ticker.Stop()
	for {
		st, err := readRolloutState(app, dest)
		// While preparing, the log is still the previous rollout's.
		if err == nil && st != nil && st.ID == id && st.Status != rolloutStatusPreparing && !st.Unstarted {
			offset, carry = c.copyRolloutLog(app, dest, offset, carry)
		}
		if err == nil && st != nil && st.ID != id {
			if done := readRolloutDone(app, dest, id); done != nil {
				return rolloutOutcome(done)
			}
			return nil, &remote.GateError{Message: "the rollout was replaced by another one", Hint: "see `yoho releases`", Code: "failed"}
		}
		if err == nil && rolloutPending(st) && !rolloutAlive(st, c.o.Boot) {
			// The worker may have finished between the two reads.
			if st2, _ := readRolloutState(app, dest); st2 != nil && st2.ID == id && !rolloutPending(st2) {
				st = st2
			} else if st.Status == rolloutStatusPreparing {
				return nil, &remote.GateError{
					Message: "the " + st.Verb + " of " + st.Version + " stopped before it started",
					Hint:    "see `yoho releases` and " + st.Verb + " again",
					Code:    "failed",
				}
			} else {
				c.copyRolloutLog(app, dest, offset, carry)
				return nil, &remote.GateError{
					Message: "the " + st.Verb + " of " + st.Version + " stopped without recording a result",
					Hint:    "see `yoho releases` and `yoho app ps`, then deploy again",
					Code:    "failed",
				}
			}
		}
		if err == nil && st != nil && st.Status != "" && !rolloutPending(st) {
			if !st.Unstarted {
				c.copyRolloutLog(app, dest, offset, carry)
			}
			return rolloutOutcome(st)
		}
		if c.out.gone() {
			return nil, errDetached
		}
		select {
		case <-ctx.Done():
			// sshd/sudo signal the Gate when the session drops; the
			// worker keeps going and the client reattaches.
			return nil, errDetached
		case <-ticker.C:
		}
	}
}

// rolloutOutcome is the result or error a finished rollout recorded.
func rolloutOutcome(st *rolloutState) (any, error) {
	if len(st.Result) > 0 {
		var rel release.Release
		if err := json.Unmarshal(st.Result, &rel); err != nil {
			return nil, fmt.Errorf("rollout result: %w", err)
		}
		return &rel, nil
	}
	code := st.Code
	if code == "" {
		code = "failed"
	}
	msg := st.Error
	if msg == "" {
		msg = "rollout " + st.Status
	}
	return nil, &remote.GateError{Message: msg, Hint: st.Hint, Code: code}
}

// watchClient marks the client gone when stdin closes, without cancelling
// the rollout (unlike watchStdin, which is for logs -f).
func (c *call) watchClient() {
	if c.watching {
		return
	}
	c.watching = true
	go func() {
		io.Copy(io.Discard, c.payload)
		c.out.markGone()
	}()
}

func (c *call) copyRolloutLog(app, dest string, offset int64, carry []byte) (int64, []byte) {
	f, err := os.Open(rolloutLogPath(app, dest))
	if err != nil {
		return offset, carry
	}
	defer f.Close()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, carry
	}
	b, err := io.ReadAll(f)
	if err != nil || len(b) == 0 {
		return offset, carry
	}
	offset += int64(len(b))
	data := append(carry, b...)
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return offset, data
		}
		c.replayRolloutLine(data[:i])
		data = data[i+1:]
	}
}

func (c *call) replayRolloutLine(line []byte) {
	if len(bytes.TrimSpace(line)) == 0 {
		return
	}
	var fr remote.GateFrame
	if err := json.Unmarshal(line, &fr); err != nil || fr.Out == "" {
		return
	}
	_, _ = c.fw.Write([]byte(fr.Out))
}

func parseRolloutRef(s string) (app, dest string, err error) {
	app, dest, ok := strings.Cut(s, "/")
	if !ok || ValidAppDest(app, dest) != nil {
		return "", "", fmt.Errorf("rollout %q must be app/destination", s)
	}
	return app, dest, nil
}
