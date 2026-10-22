package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
//	state.json     running | deployed | failed | rolled_back
//	log            NDJSON Gate frames from the worker

const (
	rolloutStatusRunning = "running"
	rolloutPollInterval  = 100 * time.Millisecond
)

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
	Verb    string          `json:"verb"`
	Version string          `json:"version"`
	Member  string          `json:"member"`
	Key     string          `json:"key,omitempty"`
	PID     int             `json:"pid"`
	Boot    string          `json:"boot,omitempty"`
	Started time.Time       `json:"started"`
	Status  string          `json:"status"`
	Error   string          `json:"error,omitempty"`
	Hint    string          `json:"hint,omitempty"`
	Code    string          `json:"code,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
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
	if st, _ := readRolloutState(app, dest); st != nil {
		st.PID = os.Getpid()
		if o.Boot != "" {
			st.Boot = o.Boot
		}
		_ = writeRolloutState(app, dest, st)
	}
	return Serve(ctx, o)
}

func writeRolloutFailed(app, dest string, err error) {
	st, _ := readRolloutState(app, dest)
	if st == nil {
		st = &rolloutState{Status: "failed"}
	}
	st.Status = "failed"
	st.Error = err.Error()
	st.Code = "failed"
	_ = writeRolloutState(app, dest, st)
}

func finishRolloutState(app, dest string, result any, err error) {
	st, _ := readRolloutState(app, dest)
	if st == nil {
		st = &rolloutState{}
	}
	if err != nil {
		ge := gateError(err)
		st.Status = "failed"
		st.Error = ge.Message
		st.Hint = ge.Hint
		st.Code = ge.Code
		st.Result = nil
		_ = writeRolloutState(app, dest, st)
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
	_ = writeRolloutState(app, dest, st)
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

func rolloutAlive(st *rolloutState, boot string) bool {
	if st == nil || st.PID <= 0 {
		return false
	}
	if st.PID == os.Getpid() {
		return true
	}
	if st.Boot != "" && boot != "" && st.Boot != boot {
		return false
	}
	return gateRunning(st.PID)
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

func (c *call) maybeAttach(ctx context.Context, verb, version string, attach bool) (any, error, bool) {
	st, err := readRolloutState(c.req.App, c.req.Destination)
	if err != nil || st == nil {
		if attach {
			return nil, invalid("no rollout of " + version + " to attach; see `yoho releases`"), true
		}
		return nil, nil, false
	}
	if st.Verb != verb || st.Version != version {
		if attach {
			return nil, invalid("no rollout of " + version + " to attach; see `yoho releases`"), true
		}
		return nil, nil, false
	}
	if st.Status == rolloutStatusRunning && !rolloutAlive(st, c.o.Boot) {
		if attach {
			return nil, &remote.GateError{
				Message: "the last rollout of " + version + " did not finish",
				Hint:    "see `yoho releases` and deploy again",
				Code:    "failed",
			}, true
		}
		return nil, nil, false
	}
	if st.Status == rolloutStatusRunning || attach {
		res, aerr := c.attachRollout(ctx)
		return res, aerr, true
	}
	return nil, nil, false
}

func (c *call) startRollout(verb, version string, args any) error {
	lock, err := lockRollout(c.req.App, c.req.Destination)
	if err != nil {
		return fmt.Errorf("rollout lock: %w", err)
	}
	defer func() {
		syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		lock.Close()
	}()
	if st, err := readRolloutState(c.req.App, c.req.Destination); err == nil && st != nil &&
		st.Verb == verb && st.Version == version && st.Status == rolloutStatusRunning && rolloutAlive(st, c.o.Boot) {
		return nil
	}

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
		return err
	}
	dir := rolloutDir(c.req.App, c.req.Destination)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(rolloutRequestPath(c.req.App, c.req.Destination), line, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(rolloutLogPath(c.req.App, c.req.Destination), nil, 0o600); err != nil {
		return err
	}
	st := &rolloutState{
		Verb: verb, Version: version, Member: c.o.Member, Key: c.o.Key,
		Boot: c.o.Boot, Started: c.o.Now().UTC(), Status: rolloutStatusRunning,
	}
	if err := writeRolloutState(c.req.App, c.req.Destination, st); err != nil {
		return err
	}
	pid, err := startWorker(rolloutJob{Options: c.o, App: c.req.App, Dest: c.req.Destination})
	if err != nil {
		st.Status = "failed"
		st.Error = err.Error()
		st.Code = "failed"
		_ = writeRolloutState(c.req.App, c.req.Destination, st)
		return err
	}
	if pid > 0 {
		st.PID = pid
		_ = writeRolloutState(c.req.App, c.req.Destination, st)
	}
	c.audit["detached"] = true
	return nil
}

func (c *call) attachRollout(ctx context.Context) (any, error) {
	c.watchClient()
	app, dest := c.req.App, c.req.Destination
	var (
		offset int64
		carry  []byte
	)
	ticker := time.NewTicker(rolloutPollInterval)
	defer ticker.Stop()
	for {
		offset, carry = c.copyRolloutLog(app, dest, offset, carry)
		st, err := readRolloutState(app, dest)
		if err == nil && st != nil && st.Status != "" && st.Status != rolloutStatusRunning {
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
		if c.out.gone() {
			return nil, errDetached
		}
		select {
		case <-ctx.Done():
			if c.out.gone() {
				return nil, errDetached
			}
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
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
