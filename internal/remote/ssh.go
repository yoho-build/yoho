package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ExecFunc runs a local process. Errors exposing ExitCode() int (like
// *exec.ExitError) are treated as non-zero exits. Injectable for tests.
type ExecFunc func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error

// SSHOptions tunes the ssh client.
type SSHOptions struct {
	// Directory for ControlMaster sockets. Default: os.TempDir()/yoho-<uid>,
	// falling back to /tmp when that would exceed the unix socket path limit.
	ControlDir string
	// Seconds the master connection lingers after the last use. Default 60.
	ControlPersist int
	// ConnectTimeout in seconds. Default 10.
	ConnectTimeout int
	// Extra ssh arguments placed before the destination, e.g. ["-i", "key"].
	ExtraArgs []string
	// Binary, default "ssh".
	Binary string
	// Exec runs local processes. Default: os/exec.
	Exec ExecFunc
}

// SSH is a Server reached by shelling out to the system ssh, so
// ~/.ssh/config, agents and ProxyJump work. One ControlMaster connection is
// shared by all commands.
type SSH struct {
	name string
	user string
	host string
	port int
	sudo bool
	opts SSHOptions

	controlPath string
}

var _ Host = (*SSH)(nil)

// exitNotExist is the exit code ReadFile's script uses for a missing file.
const exitNotExist = 44

// NewSSH creates an SSH Host for target [user@]host[:port] (IPv6 as [addr]:port).
// sudo enables sudo -n for Cmd.Sudo and privileged file operations.
func NewSSH(name, target string, sudo bool, opts SSHOptions) (*SSH, error) {
	user, host, port, err := ParseTarget(target)
	if err != nil {
		return nil, err
	}
	if opts.ControlPersist == 0 {
		opts.ControlPersist = 60
	}
	if opts.ConnectTimeout == 0 {
		opts.ConnectTimeout = 10
	}
	if opts.Binary == "" {
		opts.Binary = "ssh"
	}
	if opts.Exec == nil {
		opts.Exec = osExec
	}
	s := &SSH{name: name, user: user, host: host, port: port, sudo: sudo, opts: opts}
	if s.name == "" {
		s.name = host
	}
	s.controlPath, err = controlPath(opts.ControlDir, target)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// ParseTarget splits [user@]host[:port]. Port 0 means "ssh default/config".
func ParseTarget(target string) (user, host string, port int, err error) {
	t := strings.TrimPrefix(strings.TrimSpace(target), "ssh://")
	if i := strings.LastIndex(t, "@"); i >= 0 {
		user, t = t[:i], t[i+1:]
		if user == "" {
			return "", "", 0, fmt.Errorf("ssh target %q: empty user", target)
		}
	}
	host = t
	var p string
	if strings.HasPrefix(t, "[") {
		end := strings.Index(t, "]")
		if end < 0 {
			return "", "", 0, fmt.Errorf("ssh target %q: missing ]", target)
		}
		host, p = t[1:end], strings.TrimPrefix(t[end+1:], ":")
	} else if i := strings.LastIndex(t, ":"); i >= 0 && strings.Count(t, ":") == 1 {
		host, p = t[:i], t[i+1:]
	}
	if p != "" {
		port, err = strconv.Atoi(p)
		if err != nil || port < 1 || port > 65535 {
			return "", "", 0, fmt.Errorf("ssh target %q: invalid port", target)
		}
	}
	if host == "" || strings.HasPrefix(host, "-") || strings.HasPrefix(user, "-") ||
		strings.ContainsAny(host+user, " \t\n'\"`$;&|<>\\") {
		return "", "", 0, fmt.Errorf("ssh target %q: invalid host", target)
	}
	return user, host, port, nil
}

// controlPath picks a short socket path: macOS limits unix sockets to 104 bytes.
func controlPath(dir, target string) (string, error) {
	sum := sha256.Sum256([]byte(target))
	name := hex.EncodeToString(sum[:8])
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "yoho-"+strconv.Itoa(os.Getuid()))
		// ssh appends a random suffix to the socket while creating it.
		if len(dir)+1+len(name)+20 > 100 {
			dir = filepath.Join("/tmp", "yoho-"+strconv.Itoa(os.Getuid()))
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("ssh control dir: %w", err)
	}
	return filepath.Join(dir, name), nil
}

func (s *SSH) Name() string { return s.name }

// Target returns the ssh target as [user@]host[:port] (IPv6 bracketed).
func (s *SSH) Target() string {
	h := s.host
	if strings.Contains(h, ":") {
		h = "[" + h + "]"
	}
	if s.user != "" {
		h = s.user + "@" + h
	}
	if s.port != 0 {
		h += ":" + strconv.Itoa(s.port)
	}
	return h
}

// Destination is the ssh destination ([user@]host) without port, as used by
// rsync and scp.
func (s *SSH) Destination() string {
	if s.user != "" {
		return s.user + "@" + s.host
	}
	return s.host
}

// SSHArgs returns ssh argv (binary + options, no destination) sharing this
// Host's ControlMaster. Port is included.
func (s *SSH) SSHArgs() []string {
	a := []string{s.opts.Binary,
		"-o", "ControlMaster=auto",
		"-o", "ControlPath=" + s.controlPath,
		"-o", "ControlPersist=" + strconv.Itoa(s.opts.ControlPersist),
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-o", "ConnectTimeout=" + strconv.Itoa(s.opts.ConnectTimeout),
	}
	if s.port != 0 {
		a = append(a, "-p", strconv.Itoa(s.port))
	}
	return append(a, s.opts.ExtraArgs...)
}

// RshCommand is SSHArgs as one string for rsync -e.
func (s *SSH) RshCommand() string { return QuoteArgs(s.SSHArgs()...) }

func (s *SSH) argv(remoteCmd string) []string {
	return append(s.SSHArgs(), "--", s.Destination(), remoteCmd)
}

func (s *SSH) useSudo(c Cmd) bool { return c.Sudo && s.sudo }

// rootOr runs sh with args as-is when already root, else via sudo -n.
func rootOr(shArgs string) string {
	prog := `if [ "$(id -u)" = 0 ]; then exec sh ` + shArgs + `; else exec sudo -n sh ` + shArgs + `; fi`
	return "sh -c " + Quote(prog)
}

// exportLines renders env as sh export statements (sorted for stable output).
func exportLines(env map[string]string) (string, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		if !validEnvName(k) {
			return "", fmt.Errorf("invalid environment variable name %q", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString("export " + k + "=" + Quote(env[k]) + "\n")
	}
	return b.String(), nil
}

func validEnvName(k string) bool {
	if k == "" {
		return false
	}
	for i, r := range k {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

// plan returns the remote command and stdin for cmd. envFile, when non-empty,
// is a remote file holding exports that the script sources and deletes.
//
// Stdin free: the script travels on stdin to `sh -s`, wrapped in a brace group
// so the shell parses it fully before running and commands read /dev/null,
// not the rest of the script. Env exports sit inside that stdin stream.
// Stdin used: the script is a quoted `sh -c` argument and env comes from envFile.
func (s *SSH) plan(cmd Cmd, envFile string) (remoteCmd string, stdin io.Reader, err error) {
	if cmd.Stdin == nil {
		exports, err := exportLines(cmd.Env)
		if err != nil {
			return "", nil, err
		}
		body := "{\n" + exports + cmd.Script + "\n} </dev/null\n"
		rc := "sh -s"
		if s.useSudo(cmd) {
			rc = rootOr("-s")
		}
		return rc, strings.NewReader(body), nil
	}
	script := cmd.Script
	if envFile != "" {
		q := Quote(envFile)
		script = ". " + q + "; rm -f " + q + "\n" + script
	}
	if s.useSudo(cmd) {
		return rootOr(`-c "$1"`) + " yoho " + Quote(script), cmd.Stdin, nil
	}
	return "sh -c " + Quote(script), cmd.Stdin, nil
}

// Run implements Host.
func (s *SSH) Run(ctx context.Context, cmd Cmd) error {
	var envFile string
	if cmd.Stdin != nil && len(cmd.Env) > 0 {
		f, err := s.writeEnvFile(ctx, cmd.Env)
		if err != nil {
			return err
		}
		envFile = f
	}
	rc, stdin, err := s.plan(cmd, envFile)
	if err != nil {
		return err
	}
	return s.exec(ctx, cmd.Script, s.argv(rc), stdin, cmd.Stdout, cmd.Stderr)
}

// writeEnvFile uploads exports to a 0600 temp file on the Server over stdin.
func (s *SSH) writeEnvFile(ctx context.Context, env map[string]string) (string, error) {
	exports, err := exportLines(env)
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	script := `umask 077; f=$(mktemp "${TMPDIR:-/tmp}/yoho-env.XXXXXX"); cat > "$f"; echo "$f"`
	if err := s.exec(ctx, "write env file", s.argv("sh -c "+Quote(script)), strings.NewReader(exports), &out, nil); err != nil {
		return "", err
	}
	f := strings.TrimSpace(out.String())
	if f == "" {
		return "", fmt.Errorf("write env file on %s: no path returned", s.name)
	}
	return f, nil
}

func (s *SSH) exec(ctx context.Context, label string, argv []string, stdin io.Reader, stdout, stderrW io.Writer) error {
	var stderr bytes.Buffer
	var ew io.Writer = &stderr
	if stderrW != nil {
		ew = &teeLimit{w: stderrW, buf: &stderr}
	}
	if stdout == nil {
		stdout = io.Discard
	}
	err := s.opts.Exec(ctx, argv, stdin, stdout, ew)
	if err == nil {
		return nil
	}
	var ec interface{ ExitCode() int }
	if errors.As(err, &ec) && ec.ExitCode() >= 0 {
		return &ExitError{Host: s.name, Script: label, Code: ec.ExitCode(), Stderr: strings.TrimSpace(stderr.String())}
	}
	return fmt.Errorf("ssh %s: %w", s.name, err)
}

// Output implements Host.
func (s *SSH) Output(ctx context.Context, cmd Cmd) (string, error) {
	var out bytes.Buffer
	cmd.Stdout = &out
	err := s.Run(ctx, cmd)
	return strings.TrimSpace(out.String()), err
}

// WriteFile implements Host: data streams over stdin into a temp file that is
// renamed into place.
func (s *SSH) WriteFile(ctx context.Context, p string, data []byte, mode os.FileMode, sudo bool) error {
	return s.Run(ctx, Cmd{Script: writeFileScript(p, mode), Stdin: bytes.NewReader(data), Sudo: sudo})
}

func writeFileScript(p string, mode os.FileMode) string {
	return fmt.Sprintf("set -eu\nmkdir -p -m 0700 %s\nt=$(mktemp %s)\ntrap 'rm -f \"$t\"' EXIT\ncat > \"$t\"\nchmod %o \"$t\"\nmv -f \"$t\" %s\ntrap - EXIT",
		Quote(path.Dir(p)), Quote(p+".XXXXXX"), mode.Perm(), Quote(p))
}

// ReadFile implements Host.
func (s *SSH) ReadFile(ctx context.Context, p string, sudo bool) ([]byte, error) {
	var out bytes.Buffer
	script := fmt.Sprintf("if [ -e %s ]; then exec cat %s; else exit %d; fi", Quote(p), Quote(p), exitNotExist)
	err := s.Run(ctx, Cmd{Script: script, Stdout: &out, Sudo: sudo})
	var ee *ExitError
	if errors.As(err, &ee) && ee.Code == exitNotExist {
		return nil, fmt.Errorf("read %s on %s: %w", p, s.name, os.ErrNotExist)
	}
	if err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Close stops the ControlMaster. Errors (e.g. no master running) are ignored.
func (s *SSH) Close() error {
	argv := append(s.SSHArgs(), "-O", "exit", "--", s.Destination())
	_ = s.opts.Exec(context.Background(), argv, nil, io.Discard, io.Discard)
	return nil
}

// Forward forwards 127.0.0.1:localPort on this machine to remoteAddr
// (host:port as seen from the Server) through the ControlMaster, e.g. for a
// tunneled registry. stop cancels the forward.
func (s *SSH) Forward(ctx context.Context, localPort int, remoteAddr string) (stop func(), err error) {
	if localPort <= 0 || localPort > 65535 {
		return nil, fmt.Errorf("forward: invalid local port %d", localPort)
	}
	// Ensure the master is up; -O forward needs it.
	if err := s.Run(ctx, Cmd{Script: "true"}); err != nil {
		return nil, err
	}
	spec := "127.0.0.1:" + strconv.Itoa(localPort) + ":" + remoteAddr
	fwd := append(s.SSHArgs(), "-o", "ExitOnForwardFailure=yes", "-O", "forward", "-L", spec, "--", s.Destination())
	if err := s.exec(ctx, "forward "+spec, fwd, nil, nil, nil); err != nil {
		return nil, err
	}
	return func() {
		c := append(s.SSHArgs(), "-O", "cancel", "-L", spec, "--", s.Destination())
		_ = s.opts.Exec(context.Background(), c, nil, io.Discard, io.Discard)
	}, nil
}

func osExec(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = stdin, stdout, stderr
	return c.Run()
}
