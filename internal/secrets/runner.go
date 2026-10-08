package secrets

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// Runner executes a local command. Tests inject a fake.
type Runner interface {
	// Run executes argv with env appended to the process environment and
	// returns stdout and stderr separately. Stdin is not connected.
	Run(ctx context.Context, argv []string, env []string) (stdout, stderr []byte, err error)
}

// ExecRunner runs commands with os/exec.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, argv []string, env []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

// memoRunner caches identical calls within one Load so a ref (or a bw item
// read for several fields) hits the password manager once; vault read
// limits are shared account-wide.
type memoRunner struct {
	r     Runner
	mu    sync.Mutex
	calls map[string]*memoCall
}

type memoCall struct {
	done        chan struct{}
	out, stderr []byte
	err         error
}

func (m *memoRunner) Run(ctx context.Context, argv []string, env []string) ([]byte, []byte, error) {
	k := strings.Join(argv, "\x00") + "\x01" + strings.Join(env, "\x00")
	m.mu.Lock()
	c, ok := m.calls[k]
	if !ok {
		c = &memoCall{done: make(chan struct{})}
		m.calls[k] = c
		m.mu.Unlock()
		c.out, c.stderr, c.err = m.r.Run(ctx, argv, env)
		close(c.done)
	} else {
		m.mu.Unlock()
		<-c.done
	}
	return c.out, c.stderr, c.err
}

// commandError reports a failed command with its stderr, redacting stdout
// (which may be a partial secret) and any known values, and truncating.
func commandError(name string, err error, stdout, stderr []byte, known []string) error {
	msg := strings.TrimSpace(string(stderr))
	if msg == "" {
		return fmt.Errorf("%s failed: %v", name, err)
	}
	vals := append([]string{strings.TrimSpace(string(stdout))}, known...)
	msg = redactString(msg, vals)
	const max = 2000
	if len(msg) > max {
		msg = msg[:max] + "…"
	}
	return fmt.Errorf("%s failed: %v: %s", name, err, msg)
}
