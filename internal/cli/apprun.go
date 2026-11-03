package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/remote"
)

// termOptions are the -i/-t flags of `app exec` and `app run`.
type termOptions struct {
	interactive bool // keep stdin open and pass the local stdin
	tty         bool // allocate a pseudo-terminal (ssh -t, no -T)
}

func addTermFlags(c *cobra.Command, o *termOptions) {
	c.Flags().BoolVarP(&o.interactive, "interactive", "i", false, "Pass this terminal's stdin to the command")
	c.Flags().BoolVarP(&o.tty, "tty", "t", false, "Allocate a pseudo-terminal (for consoles and shells; use -it)")
}

// check refuses -t without a local terminal: ssh cannot give the remote
// side a pty then.
func (o termOptions) check(cmd *cobra.Command) error {
	if o.tty && !(isTerminal(cmd.InOrStdin()) && isTerminal(cmd.OutOrStdout())) {
		return &hintError{"-t needs a terminal (stdin and stdout)", "drop -t, or pass -i to pipe stdin to the command"}
	}
	return nil
}

func (o termOptions) attached() bool { return o.interactive || o.tty }

// runTerminal runs script on the Destination's first Server: with -i/-t
// attached to this terminal (ssh -t for a pty), otherwise as today with
// stdin closed.
func (a *app) runTerminal(cmd *cobra.Command, script string, o termOptions) error {
	if !o.attached() {
		return a.remoteRun(cmd, script)
	}
	name := a.dest.Servers[0]
	srv := a.cfg.Servers[name]
	h, err := remote.NewSSH(name, srv.SSH, srv.Sudo, a.sshOptions())
	if err != nil {
		return err
	}
	defer h.Close()
	return commandExit(h.RunTerminal(cmd.Context(), script, o.tty, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()))
}

// commandExit makes the remote command's exit status yoho's: its output
// already reached this terminal, so nothing more is printed. ssh's own
// failures (255) stay errors.
func commandExit(err error) error {
	var ee *remote.ExitError
	if errors.As(err, &ee) && ee.Code > 0 && ee.Code != 255 {
		return &silentError{&exitError{code: ee.Code, err: err}}
	}
	return err
}

// composeExecScript is `docker compose exec` of the running Service.
// Without a pty it passes -T, so a piped stdin and output stay clean.
func composeExecScript(project string, o termOptions, args []string) string {
	flags := "exec -T "
	if o.tty {
		flags = "exec "
	}
	return "docker compose -p " + remote.Quote(project) + " " + flags + remote.QuoteArgs(args...)
}

// swarmExecFlags are the `docker exec` flags for a swarm task container.
func swarmExecFlags(o termOptions) string {
	switch {
	case o.tty:
		return "-it "
	case o.interactive:
		return "-i "
	}
	return ""
}

func appRunCmd(g *globals) *cobra.Command {
	var o termOptions
	var allowStateful bool
	c := &cobra.Command{
		Use:   "run SERVICE [-i] [-t] -- COMMAND...",
		Short: "Run a one-off command in a new container of the current Release of SERVICE",
		Long: `Run COMMAND in a new one-off container of SERVICE from the current Release:
the same image, secrets files and env, networks and volumes as the Service,
but no published ports, no Proxy route and no dependency started. The
container is removed when the command exits and the serving containers are
not touched. Use it for database migrations, consoles and maintenance tasks:

  yoho app run web -- bin/rails db:migrate
  yoho app run web -it -- bin/rails console

It runs ` + "`docker compose run --rm --no-deps`" + ` with the current Release's
compiled compose file and labels the container yoho.oneoff=run.

A Stateful Service is refused unless --allow-stateful: a second container
on the same volume (ADR 0005) can corrupt its data. Not available on
runtime swarm; use ` + "`yoho app exec`" + ` or x-yoho.release_command.`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			if a.dest.Runtime == "swarm" {
				return a.fail(&hintError{"app run is not available on runtime swarm", "use `yoho app exec SERVICE -- COMMAND` in a running task, or x-yoho.release_command for migrations"})
			}
			if err := o.check(cmd); err != nil {
				return a.fail(err)
			}
			svc, command := args[0], args[1:]
			ctx := cmd.Context()
			name := a.dest.Servers[0]
			srv := a.cfg.Servers[name]
			h, err := remote.NewSSH(name, srv.SSH, srv.Sudo, a.sshOptions())
			if err != nil {
				return err
			}
			defer h.Close()
			ver, svcs, err := deploy.CurrentServices(ctx, h, a.cfg.App, a.destName)
			if errors.Is(err, deploy.ErrNoCurrentRelease) {
				return a.fail(&hintError{fmt.Sprintf("%s has no current Release on %s", a.cfg.App, name), "run `yoho deploy` first"})
			}
			if err != nil {
				return a.fail(err)
			}
			if err := checkRunService(svcs, svc, allowStateful); err != nil {
				return a.fail(err)
			}
			script := deploy.OneOffScript(a.cfg.App, a.destName, ver, svc, o.tty, command)
			if !g.json {
				// stderr: stdout belongs to the command.
				fmt.Fprintf(cmd.ErrOrStderr(), "one-off %s container from Release %s on %s\n", svc, shortVersion(ver), name)
			}
			if !o.attached() {
				return commandExit(h.Run(ctx, remote.Cmd{Script: script, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}))
			}
			return commandExit(h.RunTerminal(ctx, script, o.tty, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()))
		},
	}
	addTermFlags(c, &o)
	c.Flags().BoolVar(&allowStateful, "allow-stateful", false, "Run a one-off container of a Stateful Service next to the running one (it shares the volume)")
	return c
}

// checkRunService checks svc is a Service of the current Release and not a
// Stateful one unless allowed.
func checkRunService(svcs []deploy.CurrentService, svc string, allowStateful bool) error {
	var names []string
	for _, s := range svcs {
		names = append(names, s.Name)
		if s.Name != svc {
			continue
		}
		if s.Stateful && !allowStateful {
			return &hintError{fmt.Sprintf("Service %s is stateful: a one-off container would run next to it on the same data (ADR 0005)", svc),
				"use `yoho app exec " + svc + " -- COMMAND` in the running container, or pass --allow-stateful if the command is safe to run twice"}
		}
		return nil
	}
	return &hintError{fmt.Sprintf("Service %s is not in the current Release", svc), "Services: " + strings.Join(names, ", ")}
}
