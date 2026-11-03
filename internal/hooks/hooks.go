// Package hooks runs Kamal-compatible Hooks: executable scripts in
// .yoho/hooks/<name> on the operator's machine. A missing Hook is a no-op; a
// non-zero exit aborts the command.
package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/plan"
)

// DefaultDir is where Hooks live relative to the Yoho file.
const DefaultDir = ".yoho/hooks"

// Names are the Hook names Yoho runs.
var Names = []string{
	"docker-setup",
	"pre-connect",
	"pre-build",
	"pre-deploy",
	"post-deploy",
	"pre-app-boot",
	"post-app-boot",
	"pre-proxy-reboot",
	"post-proxy-reboot",
	"pre-backup",
	"post-backup",
	"pre-restore",
	"post-restore",
	"pre-cutover",
	"post-cutover",
}

// Env names set for Hooks (documented for users and the agent skill).
// Callers fill them through base and per-call extras; secrets are never
// injected wholesale.
var EnvNames = []string{
	"YOHO_APP", "YOHO_DESTINATION", "YOHO_VERSION", "YOHO_SERVICE_VERSION",
	"YOHO_HOSTS", "YOHO_COMMAND", "YOHO_SUBCOMMAND", "YOHO_PERFORMER",
	"YOHO_RECORDED_AT", "YOHO_LOCK", "YOHO_RUNTIME", "YOHO_CUTOVER",
}

// Hosts formats YOHO_HOSTS: the comma-separated Servers as host or
// host:port, without the ssh user or scheme (a Server's name when it has no
// ssh target). Deploy, rollback, proxy and backup Hooks all use it.
func Hosts(hosts []plan.NamedHost) string {
	hs := make([]string, 0, len(hosts))
	for _, h := range hosts {
		t := strings.TrimPrefix(strings.TrimSpace(h.Server.SSH), "ssh://")
		if i := strings.LastIndex(t, "@"); i >= 0 {
			t = t[i+1:]
		}
		if t == "" {
			t = h.Name
		}
		hs = append(hs, t)
	}
	return strings.Join(hs, ",")
}

// YOHO_LOCK values: LockHeld while the running command holds the
// Destination lock, LockNotHeld otherwise (the default).
const (
	LockHeld    = "true"
	LockNotHeld = "false"
)

// New returns a HookFunc running Hooks from dir with base env (YOHO_APP,
// YOHO_DESTINATION, ...) plus per-call extras, which win over base.
// YOHO_RECORDED_AT defaults to the call time; YOHO_SERVICE_VERSION defaults
// to <app>@<version>; YOHO_LOCK defaults to "false". Hook stdout/stderr go
// to out.
func New(dir string, base map[string]string, out io.Writer) plan.HookFunc {
	if dir == "" {
		dir = DefaultDir
	}
	if out == nil {
		out = io.Discard
	}
	return func(ctx context.Context, name string, extra map[string]string) error {
		if !slices.Contains(Names, name) {
			return fmt.Errorf("unknown hook %q", name)
		}
		path := filepath.Join(dir, name)
		fi, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("hook %s: %w", name, err)
		}
		if fi.IsDir() {
			return fmt.Errorf("hook %s: %s is a directory", name, path)
		}
		if fi.Mode().Perm()&0o111 == 0 {
			fmt.Fprintf(out, "warning: hook %s is not executable, skipping (chmod +x %s)\n", name, path)
			return nil
		}

		env := map[string]string{"YOHO_RECORDED_AT": time.Now().UTC().Format(time.RFC3339), "YOHO_LOCK": LockNotHeld}
		for k, v := range base {
			env[k] = v
		}
		for k, v := range extra {
			env[k] = v
		}
		if env["YOHO_SERVICE_VERSION"] == "" && env["YOHO_APP"] != "" && env["YOHO_VERSION"] != "" {
			env["YOHO_SERVICE_VERSION"] = env["YOHO_APP"] + "@" + env["YOHO_VERSION"]
		}

		abs, err := filepath.Abs(path)
		if err != nil {
			return fmt.Errorf("hook %s: %w", name, err)
		}
		cmd := exec.CommandContext(ctx, abs)
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		cmd.Stdout = out
		cmd.Stderr = out
		fmt.Fprintf(out, "running hook %s\n", name)
		if err := cmd.Run(); err != nil {
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return fmt.Errorf("hook %s failed (exit %d), aborting", name, ee.ExitCode())
			}
			return fmt.Errorf("hook %s: %w", name, err)
		}
		return nil
	}
}
