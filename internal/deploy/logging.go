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

// DockerLogDriver is the daemon's default logging driver from `docker info`.
// A nil Host reports "" (unknown).
func DockerLogDriver(ctx context.Context, h remote.Host) (string, error) {
	if h == nil {
		return "", nil
	}
	out, err := h.Output(ctx, remote.Cmd{Script: "set -eu\ndocker info --format '{{.LoggingDriver}}'"})
	if err != nil {
		return "", fmt.Errorf("inspect Docker logging driver: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// UseDefaultLogging reports whether Services without `logging` get the
// rotating json-file default. A daemon set to another driver (local,
// journald, a log shipper) is the operator's choice and is left alone;
// unknown ("") is Docker's own default, json-file.
func UseDefaultLogging(daemonDriver string) bool {
	return daemonDriver == "" || daemonDriver == LogDriver
}
