// Package remote runs commands and writes files on a Server.
//
// Two implementations share one interface: SSH (operator machine -> Server,
// shelling out to the system ssh with ControlMaster so ~/.ssh/config, agents
// and ProxyJump work) and Local (yoho running on the Server itself, e.g. a
// Scheduled Job, and in tests).
package remote

import (
	"context"
	"io"
	"os"
	"strconv"
)

// Cmd is a shell script executed by `sh -c` on the Server.
type Cmd struct {
	Script string
	Stdin  io.Reader
	Stdout io.Writer // nil: discarded unless captured by Output
	Stderr io.Writer // nil: captured into the returned error message
	// Run with sudo -n (no-op when already root or Host is not configured for sudo).
	Sudo bool
	// Extra environment variables for the script. Values are passed over
	// stdin-safe channels, never on the ssh command line.
	Env map[string]string
}

// Host is a Server Yoho can operate on.
type Host interface {
	// Name is the Server name from the Yoho file (or "local").
	Name() string
	// Run executes cmd and returns an *ExitError on non-zero exit.
	Run(ctx context.Context, cmd Cmd) error
	// Output runs cmd and returns trimmed stdout.
	Output(ctx context.Context, cmd Cmd) (string, error)
	// WriteFile atomically writes data to path (temp file + rename) with mode.
	// Parent directories are created with 0700.
	WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode, sudo bool) error
	// ReadFile reads a file; returns os.ErrNotExist (wrapped) when missing.
	ReadFile(ctx context.Context, path string, sudo bool) ([]byte, error)
	// Close releases connections (ssh ControlMaster).
	Close() error
}

// ExitError reports a failed remote command.
type ExitError struct {
	Host   string
	Script string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	msg := e.Stderr
	if len(msg) > 2000 {
		msg = msg[len(msg)-2000:]
	}
	return "remote command failed on " + e.Host + " (exit " + strconv.Itoa(e.Code) + "): " + msg
}

// Quote returns s single-quoted for POSIX sh.
func Quote(s string) string {
	out := []byte{'\''}
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\'', '\\', '\'', '\'')
		} else {
			out = append(out, s[i])
		}
	}
	return string(append(out, '\''))
}

// QuoteArgs quotes and space-joins argv.
func QuoteArgs(argv ...string) string {
	b := make([]byte, 0, 64)
	for i, a := range argv {
		if i > 0 {
			b = append(b, ' ')
		}
		b = append(b, Quote(a)...)
	}
	return string(b)
}
