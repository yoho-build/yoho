package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Local runs commands on this machine. Used when yoho runs on the Server
// (Scheduled Jobs, the Gate) and in tests. Sudo is honored only when UseSudo
// is true.
type Local struct {
	HostName string
	UseSudo  bool
	// As runs every command, file write and file read as this user instead
	// of the current one. The Gate runs as root and acts as the owner of the
	// App directory, so files it creates stay the deploy user's. Its
	// commands get a fixed minimal environment, not the caller's.
	As *User
	// Group runs each command in its own process group and kills the whole
	// group when ctx ends, so a cancelled `docker logs -f` under `sh -c`
	// does not outlive the request. Implied by As.
	Group bool
}

// User is a local account commands run as.
type User struct {
	Name   string
	UID    uint32
	GID    uint32
	Groups []uint32
	Home   string
}

// shellFiles reports whether file operations must go through sh (another
// user, or sudo) instead of this process.
func (l *Local) shellFiles(sudo bool) bool {
	return l.As != nil || sudo && l.UseSudo && os.Geteuid() != 0
}

var _ Host = (*Local)(nil)

func (l *Local) Name() string {
	if l.HostName == "" {
		return "local"
	}
	return l.HostName
}

func (l *Local) Run(ctx context.Context, cmd Cmd) error {
	script := cmd.Script
	if cmd.Sudo && l.UseSudo && os.Geteuid() != 0 {
		script = "sudo -n sh -c " + Quote(script)
	}
	c := exec.CommandContext(ctx, "sh", "-c", script)
	c.Env = os.Environ()
	if l.As != nil {
		runAs(c, l.As)
	}
	if l.Group || l.As != nil {
		ownGroup(c)
	}
	for k, v := range cmd.Env {
		c.Env = append(c.Env, k+"="+v)
	}
	c.Stdin = cmd.Stdin
	c.Stdout = cmd.Stdout
	var stderr bytes.Buffer
	if cmd.Stderr != nil {
		c.Stderr = &teeLimit{w: cmd.Stderr, buf: &stderr}
	} else {
		c.Stderr = &stderr
	}
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return &ExitError{Host: l.Name(), Script: cmd.Script, Code: ee.ExitCode(), Stderr: strings.TrimSpace(stderr.String())}
		}
		return fmt.Errorf("run on %s: %w", l.Name(), err)
	}
	return nil
}

func (l *Local) Output(ctx context.Context, cmd Cmd) (string, error) {
	var out bytes.Buffer
	cmd.Stdout = &out
	err := l.Run(ctx, cmd)
	return strings.TrimSpace(out.String()), err
}

func (l *Local) WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode, sudo bool) error {
	if l.shellFiles(sudo) {
		return l.Run(ctx, Cmd{Script: writeFileScript(path, mode), Stdin: bytes.NewReader(data), Sudo: sudo})
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode.Perm()); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (l *Local) ReadFile(ctx context.Context, path string, sudo bool) ([]byte, error) {
	if l.shellFiles(sudo) {
		var out bytes.Buffer
		script := fmt.Sprintf("if [ -e %s ]; then exec cat %s; else exit %d; fi", Quote(path), Quote(path), exitNotExist)
		err := l.Run(ctx, Cmd{Script: script, Stdout: &out, Sudo: sudo})
		var ee *ExitError
		if errors.As(err, &ee) && ee.Code == exitNotExist {
			return nil, fmt.Errorf("read %s on %s: %w", path, l.Name(), os.ErrNotExist)
		}
		return out.Bytes(), err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s on %s: %w", path, l.Name(), err)
	}
	return b, nil
}

func (l *Local) Close() error { return nil }

// teeLimit writes to w and keeps a copy for error messages.
type teeLimit struct {
	w   interface{ Write([]byte) (int, error) }
	buf *bytes.Buffer
}

func (t *teeLimit) Write(p []byte) (int, error) {
	if t.buf.Len() < 64<<10 {
		t.buf.Write(p)
	}
	return t.w.Write(p)
}
