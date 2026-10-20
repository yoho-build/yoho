package remote

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// SafePath is the PATH of commands run as another user (Local.As).
const SafePath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// runAs makes c run as u: credentials, supplementary groups (docker), and
// the user's HOME so the Docker CLI finds its config and registry logins.
// The environment is rebuilt, not inherited: only the locale survives, so
// nothing the caller's environment says (DOCKER_HOST, DOCKER_CONFIG,
// COMPOSE_*, PATH) steers what runs.
func runAs(c *exec.Cmd, u *User) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Credential = &syscall.Credential{Uid: u.UID, Gid: u.GID, Groups: u.Groups}
	c.Env = asEnv(c.Env, u)
	if fi, err := os.Stat(u.Home); err == nil && fi.IsDir() {
		c.Dir = u.Home
	}
}

// asEnv is the environment of a command run as u.
func asEnv(environ []string, u *User) []string {
	env := []string{"PATH=" + SafePath, "HOME=" + u.Home, "USER=" + u.Name, "LOGNAME=" + u.Name}
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if k == "LANG" || strings.HasPrefix(k, "LC_") {
			env = append(env, kv)
		}
	}
	return env
}

// ownGroup runs c in a new process group that is killed as a whole when
// its context ends; WaitDelay then stops waiting for output pipes that a
// stray grandchild might still hold.
func ownGroup(c *exec.Cmd) {
	if c.SysProcAttr == nil {
		c.SysProcAttr = &syscall.SysProcAttr{}
	}
	c.SysProcAttr.Setpgid = true
	c.Cancel = func() error {
		if err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL); err != nil {
			return c.Process.Kill()
		}
		return nil
	}
	c.WaitDelay = 5 * time.Second
}
