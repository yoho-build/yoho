package deploy

import (
	"context"
	"fmt"
	"strings"

	"github.com/yoho-build/yoho/internal/remote"
)

// Default log rotation for App containers that set no `logging` (issue #47):
// at most LogMaxFile files of LogMaxSize each per container, so many small
// Apps cannot fill a Server's disk with json-file logs.
const (
	LogDriver  = "json-file"
	LogMaxSize = "10m"
	LogMaxFile = "3"
)

// DaemonLogging is what the Server's Docker daemon does with container logs
// by default: its logging driver and whether daemon.json sets log-opts.
type DaemonLogging struct {
	Driver string
	// Opts is true when a daemon.json sets "log-opts" (or one exists but
	// cannot be read, so Yoho cannot tell): the operator chose options for
	// every container, and Yoho does not override them.
	Opts bool
}

// UseDefault reports whether Services without `logging` get the rotating
// json-file default. A daemon set to another driver (local, journald, a log
// shipper), or a json-file daemon with its own log-opts, is the operator's
// choice and is left alone; "" (nil Host) is Docker's own default, json-file.
func (l DaemonLogging) UseDefault() bool {
	return (l.Driver == "" || l.Driver == LogDriver) && !l.Opts
}

// LogDriverError is a Server whose Docker cannot report its logging driver.
// It stops plan and deploy: guessing would add or drop the log default and
// recreate every Service, then flip back on the next deploy.
type LogDriverError struct{ Err error }

func (e *LogDriverError) Error() string {
	return fmt.Sprintf("inspect Docker logging driver: %v", e.Err)
}
func (e *LogDriverError) Unwrap() error { return e.Err }

// LogDriverHint is the next step for a LogDriverError.
const LogDriverHint = "`docker info` failed on the Server; check that the Docker daemon is running and the deploy user can reach it (run `docker info` there), then rerun"

// logProbe prints the daemon's logging driver, then "log-opts" when a
// daemon.json (system or rootless) sets log-opts, or "unreadable" when one
// exists but cannot be read.
const logProbe = `set -eu
docker info --format '{{.LoggingDriver}}'
for f in /etc/docker/daemon.json "${XDG_CONFIG_HOME:-${HOME:-/nonexistent}/.config}/docker/daemon.json"; do
  [ -e "$f" ] || continue
  if [ ! -r "$f" ]; then echo unreadable; continue; fi
  if grep -q '"log-opts"' "$f" 2>/dev/null; then echo log-opts; fi
done`

// DockerLogDriver reads the daemon's default logging from the Server. A nil
// Host is unknown (json-file default). A failing probe is an error: connect
// already proved docker works, and guessing would flip the default.
func DockerLogDriver(ctx context.Context, h remote.Host) (DaemonLogging, error) {
	if h == nil {
		return DaemonLogging{}, nil
	}
	out, err := h.Output(ctx, remote.Cmd{Script: logProbe})
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return DaemonLogging{}, &LogDriverError{Err: err}
	}
	return parseLogProbe(out), nil
}

func parseLogProbe(out string) DaemonLogging {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	l := DaemonLogging{Driver: strings.TrimSpace(lines[0])}
	for _, line := range lines[1:] {
		switch strings.TrimSpace(line) {
		case "log-opts", "unreadable":
			l.Opts = true
		}
	}
	return l
}
