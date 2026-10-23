package gate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Options configures one Gate run.
type Options struct {
	// Member is the --member of the forced command; Key the fingerprint of
	// the key that authenticated (best effort, for the audit log).
	Member string
	Key    string
	// Version is this yoho's version, reported by hello.
	Version string
	In      io.Reader
	Out     io.Writer
	Getenv  func(string) string
	// HostFor returns the Host verbs run on, acting as the owner of the App
	// directory. Default: OwnerHost.
	HostFor func(appDir string) (remote.Host, error)
	Now     func() time.Time
	// RequestTimeout bounds waiting for the request line (default 30s).
	RequestTimeout time.Duration
	// Boot is the kernel boot id, recorded in deploy locks with the pid
	// (default: read from /proc; "" disables stale-lock detection).
	Boot string
	// RolloutWorker is set on the detached process that finishes a deploy
	// or rollback after the Member's SSH session is gone.
	RolloutWorker bool
}

// DefaultRequestTimeout is how long the Gate waits for a request line.
const DefaultRequestTimeout = 30 * time.Second

// Exit codes of a Gate run.
const (
	ExitOK     = 0
	ExitFailed = 1
	ExitDenied = 2
)

// call is one request being served.
type call struct {
	o        Options
	req      *remote.GateRequest
	host     remote.Host
	fw       *remote.FrameWriter
	out      *clientWriter
	payload  *bufio.Reader
	audit    map[string]any
	cancel   context.CancelFunc
	watching bool
}

type handler func(ctx context.Context, c *call) (any, error)

var handlers map[string]handler

func init() {
	handlers = map[string]handler{
		VerbHello:    hello,
		VerbPS:       ps,
		VerbStats:    stats,
		VerbLogs:     logs,
		VerbDash:     dashVerb,
		VerbReleases: releasesVerb,
		verbImages:   imagesVerb,
		VerbDeploy:   deployVerb,
		VerbRollback: rollbackVerb,
	}
}

func denied(msg, hint string) error {
	return &remote.GateError{Message: msg, Hint: hint, Code: "denied"}
}

func invalid(msg string) error { return &remote.GateError{Message: msg, Code: "invalid"} }

// Serve handles one request from o.In and returns the process exit code.
func Serve(ctx context.Context, o Options) int {
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.HostFor == nil {
		o.HostFor = OwnerHost
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Boot == "" {
		o.Boot = BootID()
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = DefaultRequestTimeout
	}
	start := o.Now()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Writes fail once the client is gone. Open-ended verbs (logs -f) stop
	// then; deploys and rollbacks run to completion and release the lock.
	out := &clientWriter{w: o.Out}
	fw := remote.NewFrameWriter(out)

	req, payload, err := readRequest(o.In, o.RequestTimeout)
	if err != nil {
		fw.Fail(&remote.GateError{Message: err.Error(), Code: "invalid"})
		return ExitFailed
	}
	entry := AuditEntry{
		Time: start.UTC(), Member: o.Member, Key: o.Key, App: req.App, Destination: req.Destination, Verb: req.Verb,
	}
	if f := strings.Fields(o.Getenv("SSH_CONNECTION")); len(f) > 0 {
		entry.Client = f[0]
	}
	c := &call{o: o, req: req, fw: fw, out: out, payload: payload, audit: map[string]any{}, cancel: cancel}
	result, err := c.serve(ctx)
	code := ExitOK
	switch {
	case errors.Is(err, errDetached):
		c.audit["detached"] = true
	case err != nil:
		ge := gateError(err)
		code = ExitFailed
		if ge.Code == "denied" {
			code = ExitDenied
		}
		fw.Fail(ge)
		entry.Error = truncate(ge.Message, 500)
	default:
		if werr := fw.Result(result); werr != nil {
			code = ExitFailed
		}
	}
	if o.RolloutWorker {
		finishRolloutState(req.App, req.Destination, result, err)
	}
	entry.Exit = code
	entry.Seconds = o.Now().Sub(start).Seconds()
	if len(c.audit) > 0 {
		entry.Args = c.audit
	}
	// Requests for an App Destination with scoped access go to its log,
	// everything else (probes of other Apps, typos) to the Server's.
	p := ServerAuditPath()
	if ValidAppDest(req.App, req.Destination) == nil && dirExists(release.AccessDir(req.App, req.Destination)) {
		p = AuditPath(req.App, req.Destination)
	}
	if aerr := AppendAudit(p, entry); aerr != nil {
		fmt.Fprintf(os.Stderr, "yoho gate: audit log: %v\n", aerr)
	}
	return code
}

func (c *call) serve(ctx context.Context) (any, error) {
	req := c.req
	if !MemberRe.MatchString(c.o.Member) {
		return nil, denied("this key has no valid Member", "the operator adds keys with `yoho access key add`")
	}
	if err := ValidAppDest(req.App, req.Destination); err != nil {
		return nil, invalid(err.Error())
	}
	h, ok := handlers[req.Verb]
	if !ok {
		return nil, invalid(fmt.Sprintf("unknown verb %q", req.Verb))
	}
	members, err := loadMembers(req.App, req.Destination)
	if err != nil {
		return nil, fmt.Errorf("access: %w", err)
	}
	if _, ok := members.Members[c.o.Member]; !ok {
		return nil, denied(fmt.Sprintf("Member %s has no access to %s/%s", c.o.Member, req.App, req.Destination), "ask the operator to run `yoho access add "+c.o.Member+" -d "+req.Destination+"`")
	}
	if !members.Allowed(c.o.Member, req.Verb) {
		return nil, denied(fmt.Sprintf("Member %s may not %s on %s/%s (allowed: %s)", c.o.Member, grant(req.Verb), req.App, req.Destination, strings.Join(members.Members[c.o.Member].Verbs, ", ")), "ask the operator to grant it with `yoho access add "+c.o.Member+" --verbs ...`")
	}
	appDir := release.AppDir(req.App, req.Destination)
	if !dirExists(appDir) {
		return nil, &remote.GateError{Message: req.App + "/" + req.Destination + " has not been deployed on this Server", Hint: "the operator deploys it once with `yoho deploy`", Code: "failed"}
	}
	if c.host, err = c.o.HostFor(appDir); err != nil {
		return nil, err
	}
	return h(ctx, c)
}

// watchStdin cancels the request when the client closes stdin (it went
// away). Only open-ended verbs (logs -f) use it: a deploy or rollback whose
// client disconnects runs to completion rather than stopping halfway.
func (c *call) watchStdin() {
	if c.watching {
		return
	}
	c.watching = true
	c.out.onError(c.cancel)
	go func() {
		io.Copy(io.Discard, c.payload)
		c.cancel()
	}()
}

// readRequest reads the request line, giving up after timeout (a client
// that connects and sends nothing must not hold a Gate process).
func readRequest(in io.Reader, timeout time.Duration) (*remote.GateRequest, *bufio.Reader, error) {
	type res struct {
		req *remote.GateRequest
		br  *bufio.Reader
		err error
	}
	ch := make(chan res, 1)
	go func() {
		req, br, err := remote.ReadGateRequest(in)
		ch <- res{req, br, err}
	}()
	select {
	case r := <-ch:
		return r.req, r.br, r.err
	case <-time.After(timeout):
		return nil, nil, fmt.Errorf("no request within %s", timeout)
	}
}

// clientWriter is the Gate's stdout. After a failed write (the client went
// away) it drops output and calls the registered cancel, once.
type clientWriter struct {
	mu     sync.Mutex
	w      io.Writer
	failed bool
	cancel func()
}

func (w *clientWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed {
		return 0, errClientGone
	}
	n, err := w.w.Write(p)
	if err != nil {
		w.failed = true
		if w.cancel != nil {
			w.cancel()
		}
	}
	return n, err
}

func (w *clientWriter) onError(cancel func()) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancel = cancel
	if w.failed {
		cancel()
	}
}

func (w *clientWriter) gone() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failed
}

func (w *clientWriter) markGone() {
	w.mu.Lock()
	w.failed = true
	w.mu.Unlock()
}

var errClientGone = errors.New("client disconnected")

// args decodes the request arguments strictly.
func (c *call) args(v any) error {
	if len(c.req.Args) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(c.req.Args)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid(c.req.Verb + ": bad arguments: " + err.Error())
	}
	return nil
}

func gateError(err error) *remote.GateError {
	var ge *remote.GateError
	if errors.As(err, &ge) {
		return ge
	}
	return &remote.GateError{Message: err.Error(), Code: "failed"}
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// OwnerHost returns a Local Host acting as the owner of appDir: the deploy
// user whose Docker access and files the operator path uses, so a Gate
// deploy leaves the App directory as an operator deploy would.
func OwnerHost(appDir string) (remote.Host, error) {
	fi, err := os.Stat(appDir)
	if err != nil {
		return nil, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, errors.New("cannot read the owner of " + appDir)
	}
	if int(st.Uid) == os.Geteuid() {
		return &remote.Local{HostName: hostname(), Group: true}, nil
	}
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("the Gate must run as root to act as the owner of %s (run it through sudo, see `yoho access install`)", appDir)
	}
	u, err := user.LookupId(strconv.FormatUint(uint64(st.Uid), 10))
	if err != nil {
		return nil, fmt.Errorf("owner of %s: %w", appDir, err)
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil {
		return nil, err
	}
	as := &remote.User{Name: u.Username, UID: st.Uid, GID: uint32(gid), Home: u.HomeDir}
	gids, _ := u.GroupIds()
	for _, g := range gids {
		if n, err := strconv.ParseUint(g, 10, 32); err == nil {
			as.Groups = append(as.Groups, uint32(n))
		}
	}
	return &remote.Local{HostName: hostname(), As: as}, nil
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "local"
	}
	return h
}
