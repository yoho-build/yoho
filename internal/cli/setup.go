package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/yoho-dev/yoho/internal/remote"
	"github.com/yoho-dev/yoho/internal/setup"
)

func init() {
	extraCommands = append(extraCommands, setupCmd)
}

func stdinIsTTY() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

var stdinReader = bufio.NewReader(os.Stdin)

// promptLine asks on w and returns the trimmed, lowercased answer.
func promptLine(w io.Writer, prompt string) (string, error) {
	fmt.Fprint(w, prompt)
	line, err := stdinReader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read answer: %w", err)
	}
	return strings.ToLower(strings.TrimSpace(line)), nil
}

func setupCmd(g *globals) *cobra.Command {
	var yes, planOnly, containerd bool
	c := &cobra.Command{
		Use:   "setup",
		Short: "Provision the Destination's Servers (Docker, deploy user, firewall, updates)",
		Long: `Setup inspects each Server, prints a checklist of provisioning steps and
applies the needed ones after asking per step (y = yes, N = no, a = all).
Every change needs root or passwordless sudo (servers.<name>.sudo: true).
Supports Debian and Ubuntu.`,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			u := a.ui
			defer func() {
				if err != nil {
					u.Finished(err, "")
				}
			}()
			interactive := !yes && !planOnly && !g.json && stdinIsTTY()
			if !yes && !planOnly && !interactive {
				u.Warn("no TTY to ask on; showing the plan only (pass --yes to apply every step)")
				planOnly = true
			}
			cfg := setup.Config{
				Setup:                a.cfg.Setup,
				ProxyPorts:           a.setupProxyPorts(),
				AuthorizedKeys:       a.cfg.Setup.AuthorizedKeys,
				ContainerdImageStore: containerd,
			}
			if len(cfg.AuthorizedKeys) == 0 {
				cfg.AuthorizedKeys = localPublicKeys()
			}
			for _, name := range a.dest.Servers {
				srv := a.cfg.Servers[name]
				cfg.Sudo = srv.Sudo
				// Connecting as a non-root user means that user deploys; a
				// separate deploy user is only created when setup runs as root.
				cfg.Setup.User = a.cfg.Setup.User
				if user, _, _, perr := remote.ParseTarget(srv.SSH); perr == nil && user != "" && user != "root" {
					cfg.Setup.User = user
				}
				u.Title("Setup %s (%s)", name, srv.SSH)
				// No docker check here: setup is what installs Docker.
				step := u.Step(name, "Connect %s", srv.SSH)
				h, cerr := remote.NewSSH(name, srv.SSH, srv.Sudo, remote.SSHOptions{})
				if cerr == nil {
					_, cerr = h.Output(ctx, remote.Cmd{Script: "true"})
				}
				if cerr != nil {
					step.Fail(cerr, "check `ssh "+srv.SSH+"` works without a password")
					return &silentError{cerr}
				}
				step.Done()
				err = a.setupServer(cmd, h, name, cfg, planOnly, yes)
				h.Close()
				if err != nil {
					return err
				}
			}
			if planOnly {
				u.Finished(nil, "Plan only; nothing was changed")
			} else {
				u.Finished(nil, "Setup finished")
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "Apply every needed step without asking")
	c.Flags().BoolVar(&planOnly, "plan", false, "Only show the plan; change nothing")
	c.Flags().BoolVar(&containerd, "containerd-image-store", false, "Also enable Docker's containerd image store (restarts Docker; needed for pussh)")
	return c
}

func (a *app) setupServer(cmd *cobra.Command, h remote.Host, name string, cfg setup.Config, planOnly, yes bool) error {
	u := a.ui
	ctx := cmd.Context()
	step := u.Step(name, "Inspect Server")
	steps, err := setup.Plan(ctx, h, cfg)
	if err != nil {
		step.Fail(err, "setup supports Debian/Ubuntu; check that the user can run sudo -n true")
		return &silentError{err}
	}
	needed := 0
	for _, ps := range steps {
		if ps.Needed {
			needed++
		}
	}
	step.Done(fmt.Sprintf("%d of %d steps needed", needed, len(steps)))

	rows := make([][]string, 0, len(steps))
	for _, ps := range steps {
		state := "ok"
		switch {
		case ps.Needed && ps.Blocked != "":
			state = "needs sudo"
		case ps.Needed:
			state = "needed"
		}
		rows = append(rows, []string{setupMark(state), ps.Step.Title, state, ps.Detail})
	}
	u.Table([]string{"", "STEP", "STATE", "DETAIL"}, rows)
	for _, ps := range steps {
		if ps.Needed && ps.Blocked != "" {
			u.Warn("%s", ps.Blocked)
			break
		}
	}
	if planOnly || needed == 0 {
		return nil
	}

	all := yes
	w := cmd.OutOrStdout()
	var promptErr error
	confirm := func(ps setup.PlannedStep) bool {
		if all {
			return true
		}
		if promptErr != nil {
			return false
		}
		fmt.Fprintf(w, "  %s\n    %s\n", ps.Step.Title, ps.Detail)
		ans, err := promptLine(w, "  Apply? [y/N/a=all] ")
		if err != nil {
			promptErr = err
			return false
		}
		switch ans {
		case "a", "all":
			all = true
			return true
		case "y", "yes":
			return true
		}
		return false
	}
	step = u.Step(name, "Apply")
	err = setup.Apply(ctx, h, steps, confirm, step.Output())
	if err == nil {
		err = promptErr
	}
	if err != nil {
		hint := "fix the cause and rerun `yoho setup`; completed steps are skipped"
		if strings.Contains(err.Error(), "need sudo") {
			hint = "set `sudo: true` for this Server with passwordless sudo, or run the steps as root"
		}
		step.Fail(err, hint)
		return &silentError{err}
	}
	step.Done()
	return nil
}

func setupMark(state string) string {
	switch state {
	case "ok":
		return "[x]"
	case "needed":
		return "[ ]"
	}
	return "[!]"
}

// setupProxyPorts are the Proxy ports the firewall must allow.
func (a *app) setupProxyPorts() []int {
	p := a.proxyConfig()
	if p.Tunnel != nil {
		// Behind Cloudflare Tunnel nothing needs to be reachable from outside.
		return nil
	}
	http, https := 80, 443
	if p.HTTPPort != nil {
		http = *p.HTTPPort
	}
	if p.HTTPSPort != nil {
		https = *p.HTTPSPort
	}
	var ports []int
	for _, x := range []int{http, https} {
		if x > 0 {
			ports = append(ports, x)
		}
	}
	return ports
}

// localPublicKeys reads ~/.ssh/*.pub as the default authorized keys.
func localPublicKeys() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(home, ".ssh", "*.pub"))
	var keys []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		k := strings.TrimSpace(string(b))
		if k != "" && !strings.Contains(k, "\n") {
			keys = append(keys, k)
		}
	}
	return keys
}
