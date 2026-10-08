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
// (Scheduled Jobs) and in tests. Sudo is honored only when UseSudo is true.
type Local struct {
	HostName string
	UseSudo  bool
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
	if sudo && l.UseSudo && os.Geteuid() != 0 {
		script := fmt.Sprintf("mkdir -p -m 0700 %s && t=$(mktemp %s.XXXXXX) && cat > \"$t\" && chmod %o \"$t\" && mv -f \"$t\" %s",
			Quote(filepath.Dir(path)), Quote(path), mode.Perm(), Quote(path))
		return l.Run(ctx, Cmd{Script: script, Stdin: bytes.NewReader(data), Sudo: true})
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
	if sudo && l.UseSudo && os.Geteuid() != 0 {
		out, err := l.Output(ctx, Cmd{Script: "cat " + Quote(path), Sudo: true})
		return []byte(out), err
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
