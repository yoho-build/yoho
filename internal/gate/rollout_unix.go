//go:build unix

package gate

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/yoho-build/yoho/internal/remote"
)

// StartDetachedProcess starts `yoho gate --rollout app/dest` in a new
// session so it is not in the SSH/sudo process group. sshd sending SIGHUP
// (or sudo dying with the session) does not kill the rollout.
func StartDetachedProcess(job rolloutJob) (int, error) {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		exe = os.Args[0]
	}
	cmd := exec.Command(exe, "gate", "--member", job.Options.Member, "--key", job.Options.Key, "--rollout", job.App+"/"+job.Dest)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Env = []string{"PATH=" + remote.SafePath, "HOME=/root", "LANG=C"}
	if job.Options.Getenv != nil {
		if v := job.Options.Getenv("SSH_CONNECTION"); v != "" {
			cmd.Env = append(cmd.Env, "SSH_CONNECTION="+v)
		}
	}
	log, err := os.OpenFile(rolloutLogPath(job.App, job.Dest), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return 0, err
	}
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		log.Close()
		return 0, fmt.Errorf("start detached rollout: %w", err)
	}
	go func() {
		_ = cmd.Wait()
		log.Close()
	}()
	return cmd.Process.Pid, nil
}
