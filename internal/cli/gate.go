package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/gate"
	"github.com/yoho-build/yoho/internal/release"
)

func init() { extraCommands = append(extraCommands, gateCmd) }

// GateConfigPath is the Gate's root-owned configuration, written by `yoho
// access install`.
const GateConfigPath = "/etc/yoho/gate.json"

// GateConfig is GateConfigPath.
type GateConfig struct {
	// Root is the Yoho root the Gate serves (default release.DefaultRoot).
	Root string `json:"root,omitempty"`
}

// gateCmd is the Server side of scoped access (ADR 0011). sshd runs it from
// the forced command of a Member's key in /home/yoho-gate/.ssh/authorized_keys,
// through sudo, so it is root; it reads one request on stdin and exits.
// SSH_ORIGINAL_COMMAND is ignored: the request names the verb.
func gateCmd(_ *globals) *cobra.Command {
	var member, key, root, rollout string
	c := &cobra.Command{
		Use:    "gate --member NAME",
		Short:  "Serve one scoped-access request on stdin (run by sshd for Members)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			// A client that goes away closes the pipes: writes must fail
			// (and deploys finish and unlock), not kill the Gate.
			signal.Ignore(syscall.SIGPIPE, syscall.SIGHUP)
			r, err := loadGateRoot(GateConfigPath)
			if err != nil {
				return err
			}
			release.Root = r
			gate.StartRolloutWorker = gate.StartDetachedProcess
			opts := gate.Options{Member: member, Key: key, Version: Version, In: os.Stdin, Out: os.Stdout}
			var code int
			if rollout != "" {
				app, dest, ok := strings.Cut(rollout, "/")
				if !ok || gate.ValidAppDest(app, dest) != nil {
					return fmt.Errorf("rollout %q must be app/destination", rollout)
				}
				code = gate.RunWorker(cmd.Context(), opts, app, dest)
			} else {
				code = gate.Serve(cmd.Context(), opts)
			}
			if code != 0 {
				exitFunc(code)
			}
			return nil
		},
	}
	c.Flags().StringVar(&member, "member", "", "Member this key belongs to")
	c.Flags().StringVar(&key, "key", "", "Fingerprint of the key, for the audit log")
	c.Flags().StringVar(&rollout, "rollout", "", "app/destination of a detached deploy or rollback")
	c.Flags().MarkHidden("rollout")
	// Older forced commands passed --root; the root now comes only from
	// GateConfigPath, so the flag is accepted and ignored.
	c.Flags().StringVar(&root, "root", "", "ignored (see "+GateConfigPath+")")
	c.Flags().MarkHidden("root")
	return c
}

// loadGateRoot reads the Yoho root from the Gate config. A missing file
// means the default; the file must be root-owned and not writable by group
// or others, since it decides which directories the root Gate trusts.
func loadGateRoot(p string) (string, error) {
	fi, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return release.DefaultRoot, nil
	}
	if err != nil {
		return "", err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o022 != 0 {
		return "", fmt.Errorf("%s must be a regular file not writable by group or others", p)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 && os.Geteuid() == 0 {
		return "", fmt.Errorf("%s must be owned by root", p)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	var c GateConfig
	if err := json.Unmarshal(b, &c); err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	if c.Root == "" {
		return release.DefaultRoot, nil
	}
	if !path.IsAbs(c.Root) || path.Clean(c.Root) != c.Root {
		return "", fmt.Errorf("%s: root must be a clean absolute path", p)
	}
	return c.Root, nil
}
