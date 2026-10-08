package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/swarm"
)

func init() {
	swarmRuntime = func() plan.Runtime { return swarm.Runtime{} }
	extraCommands = append(extraCommands, swarmCmd)
}

func swarmCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "swarm", Short: "Set up Docker Swarm on the Destination's Servers (runtime: swarm)"}
	var yes bool
	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Initialize a Swarm on the first Server (the manager)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, hosts, done, err := g.swarmHosts(cmd, true)
			if err != nil {
				return err
			}
			defer done()
			return a.swarmInit(cmd, hosts[0], yes, len(hosts) > 1)
		},
	}
	initCmd.Flags().BoolVarP(&yes, "yes", "y", false, "Do not ask for confirmation")
	joinCmd := &cobra.Command{
		Use:   "join",
		Short: "Join the other Servers to the manager's Swarm as workers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, hosts, done, err := g.swarmHosts(cmd, true)
			if err != nil {
				return err
			}
			defer done()
			return a.swarmJoin(cmd, hosts, yes)
		},
	}
	joinCmd.Flags().BoolVarP(&yes, "yes", "y", false, "Do not ask for confirmation")
	c.AddCommand(initCmd, joinCmd, &cobra.Command{
		Use:   "status",
		Short: "Show Swarm nodes and the App's services",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, hosts, done, err := g.swarmHosts(cmd, false)
			if err != nil {
				return err
			}
			defer done()
			return a.swarmStatus(cmd.Context(), hosts[0])
		},
	})
	return c
}

// swarmRuntimeError is returned by swarm init and join when the Destination
// is not runtime swarm. Status still warns with the same text and connects.
func swarmRuntimeError(dest string, d config.Destination) error {
	if d.Runtime == "swarm" {
		return nil
	}
	return fmt.Errorf("destination %s uses runtime %s; set destinations.%s.runtime: swarm first (switching needs downtime, see docs/site/swarm.md)", dest, runtimeName(d), dest)
}

func (g *globals) swarmHosts(cmd *cobra.Command, strict bool) (*app, []plan.NamedHost, func(), error) {
	a, err := g.load(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	if err := swarmRuntimeError(a.destName, a.dest); err != nil {
		if strict {
			return nil, nil, nil, err
		}
		a.ui.Warn("%s", err.Error())
	}
	hosts, done, err := a.connect(cmd.Context())
	if err != nil {
		return nil, nil, nil, err
	}
	return a, hosts, done, nil
}

// nodeState returns docker info's Swarm LocalNodeState and ControlAvailable.
func nodeState(ctx context.Context, h remote.Host) (state string, manager bool, err error) {
	out, err := h.Output(ctx, remote.Cmd{Script: "docker info -f '{{.Swarm.LocalNodeState}}|{{.Swarm.ControlAvailable}}'"})
	if err != nil {
		return "", false, err
	}
	s, m, _ := strings.Cut(strings.TrimSpace(out), "|")
	return s, m == "true", nil
}

// advertiseAddr is the address other Servers reach this one on: the
// configured private address (Tailscale / VPC), else the SSH host if it is
// an IP. Docker's --advertise-addr takes an IP or interface, not a hostname.
func advertiseAddr(name string, s config.Server) (string, error) {
	if s.PrivateAddress != "" {
		return s.PrivateAddress, nil
	}
	_, host, _, err := remote.ParseTarget(s.SSH)
	if err != nil {
		return "", err
	}
	if net.ParseIP(host) == nil {
		return "", fmt.Errorf("server %s: set servers.%s.private_address to its private IP (Tailscale or VPC); %q is not an IP", name, name, host)
	}
	return host, nil
}

func (a *app) swarmAdvice(warnPublic bool) {
	u := a.ui
	u.Info("Swarm needs these ports open between the Servers only (never to the internet):")
	u.Info("  2377/tcp      cluster management (manager)")
	u.Info("  7946/tcp+udp  node discovery")
	u.Info("  4789/udp      overlay network traffic (VXLAN, unencrypted by default)")
	u.Info("Run Swarm over Tailscale or a private network: set servers.<name>.private_address so --advertise-addr uses it.")
	u.Info("kamal-proxy runs on every node (overlay network yoho). Point DNS or a Cloudflare Tunnel at any Server.")
	if warnPublic {
		u.Warn("no private_address configured: Swarm advertises the public address")
	}
}

func (a *app) swarmInit(cmd *cobra.Command, mgr plan.NamedHost, yes, more bool) error {
	ctx := cmd.Context()
	u := a.ui
	step := u.Step(mgr.Name, "Check Swarm state")
	state, manager, err := nodeState(ctx, mgr.Host)
	if err != nil {
		step.Fail(err, "check that docker runs on the Server")
		return &silentError{err}
	}
	switch {
	case state == "active" && manager:
		step.Skip("already a Swarm manager")
		a.swarmAdvice(mgr.Server.PrivateAddress == "")
		return nil
	case state != "inactive":
		err := fmt.Errorf("node state is %q", state)
		step.Fail(err, "the Server already belongs to another Swarm as a worker (or is mid-change); run `docker swarm leave` there if that is intended")
		return &silentError{err}
	}
	step.Done("not in a Swarm")

	addr, err := advertiseAddr(mgr.Name, mgr.Server)
	if err != nil {
		u.Finished(err, "")
		return &silentError{err}
	}
	ok, err := confirm(cmd, yes, fmt.Sprintf("Initialize a Swarm on %s (advertise %s)?", mgr.Name, addr))
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("aborted")
	}
	step = u.Step(mgr.Name, "docker swarm init --advertise-addr %s", addr)
	if err := mgr.Host.Run(ctx, remote.Cmd{Script: "docker swarm init --advertise-addr " + remote.Quote(addr) + " >/dev/null", Stderr: step.Output()}); err != nil {
		step.Fail(err, "check the address belongs to the Server (`ip addr`), or set servers."+mgr.Name+".private_address")
		return &silentError{err}
	}
	step.Done()
	a.swarmAdvice(mgr.Server.PrivateAddress == "")
	if more {
		u.Info("next: `yoho swarm join` to add the other Servers")
	}
	return nil
}

func (a *app) swarmJoin(cmd *cobra.Command, hosts []plan.NamedHost, yes bool) error {
	ctx := cmd.Context()
	u := a.ui
	mgr := hosts[0]
	step := u.Step(mgr.Name, "Read join token")
	state, manager, err := nodeState(ctx, mgr.Host)
	if err == nil && (state != "active" || !manager) {
		err = fmt.Errorf("%s is not a Swarm manager (state %q)", mgr.Name, state)
	}
	if err != nil {
		step.Fail(err, "run `yoho swarm init` first")
		return &silentError{err}
	}
	// The token stays in memory and reaches the workers through Cmd.Env,
	// never a command line or the output.
	token, err := mgr.Host.Output(ctx, remote.Cmd{Script: "docker swarm join-token -q worker"})
	var mgrAddr string
	if err == nil {
		mgrAddr, err = mgr.Host.Output(ctx, remote.Cmd{Script: "docker info -f '{{.Swarm.NodeAddr}}'"})
	}
	if err != nil {
		step.Fail(err, "check that the deploy user can run docker on the manager")
		return &silentError{err}
	}
	step.Done("manager " + mgrAddr)
	if len(hosts) == 1 {
		u.Info("only one Server in destination %s; nothing to join", a.destName)
		return nil
	}
	for _, h := range hosts[1:] {
		step := u.Step(h.Name, "Join Swarm")
		state, _, err := nodeState(ctx, h.Host)
		if err != nil {
			step.Fail(err, "check that docker runs on the Server")
			return &silentError{err}
		}
		if state == "active" {
			step.Skip("already in a Swarm")
			continue
		}
		addr, err := advertiseAddr(h.Name, h.Server)
		if err != nil {
			step.Fail(err, "set private_address for every Server")
			return &silentError{err}
		}
		ok, err := confirm(cmd, yes, fmt.Sprintf("Join %s (advertise %s) to the Swarm of %s as a worker?", h.Name, addr, mgr.Name))
		if err != nil {
			step.Fail(err, "rerun with --yes")
			return &silentError{err}
		}
		if !ok {
			step.Skip("declined")
			continue
		}
		script := "docker swarm join --token \"$YOHO_SWARM_TOKEN\" --advertise-addr " + remote.Quote(addr) + " " + remote.Quote(net.JoinHostPort(strings.TrimSpace(mgrAddr), "2377")) + " >/dev/null"
		if err := h.Host.Run(ctx, remote.Cmd{Script: script, Env: map[string]string{"YOHO_SWARM_TOKEN": strings.TrimSpace(token)}}); err != nil {
			step.Fail(err, "open 2377/tcp, 7946/tcp+udp and 4789/udp between the Servers (private network / Tailscale)")
			return &silentError{err}
		}
		step.Done()
	}
	a.swarmAdvice(false)
	return nil
}

func (a *app) swarmStatus(ctx context.Context, mgr plan.NamedHost) error {
	out, err := mgr.Host.Output(ctx, remote.Cmd{Script: "docker node ls --format '{{.Hostname}}|{{.Status}}|{{.Availability}}|{{.ManagerStatus}}|{{.EngineVersion}}'"})
	if err != nil {
		err = fmt.Errorf("%w\nhint: run `yoho swarm init`", err)
		a.ui.Finished(err, "")
		return &silentError{err}
	}
	a.ui.Table([]string{"NODE", "STATUS", "AVAILABILITY", "MANAGER", "ENGINE"}, splitRows(out, 5))
	stack := release.ProjectName(a.cfg.App, a.destName)
	out, err = mgr.Host.Output(ctx, remote.Cmd{Script: "docker service ls --filter " + remote.Quote("label=com.docker.stack.namespace="+stack) + " --format '{{.Name}}|{{.Mode}}|{{.Replicas}}|{{.Image}}'"})
	if err != nil {
		return err
	}
	if rows := splitRows(out, 4); len(rows) > 0 {
		a.ui.Table([]string{"SERVICE", "MODE", "REPLICAS", "IMAGE"}, rows)
	} else {
		a.ui.Info("stack %s is not deployed", stack)
	}
	return nil
}

func splitRows(out string, n int) [][]string {
	var rows [][]string
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		f := strings.SplitN(l, "|", n)
		for len(f) < n {
			f = append(f, "")
		}
		rows = append(rows, f)
	}
	return rows
}
