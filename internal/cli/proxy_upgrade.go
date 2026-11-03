package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/remote"
)

// proxyHookFor runs `yoho proxy <sub>` Hooks: YOHO_COMMAND=proxy,
// YOHO_SUBCOMMAND=sub, without the Destination lock (the Proxy has its own
// Server-wide lock).
func (a *app) proxyHookFor(sub, version string, hosts []plan.NamedHost) func(context.Context, string) error {
	h := a.hookFunc(version, "proxy", hosts)
	return func(ctx context.Context, name string) error {
		return h(ctx, name, map[string]string{"YOHO_SUBCOMMAND": sub})
	}
}

// proxyHealthPaths maps this App's route names to their health paths, so the
// upgrade probes them. Routes of other Apps are probed at /up and compared
// with their own pre-upgrade answer, so a wrong path cannot fail an upgrade.
func (a *app) proxyHealthPaths(ctx context.Context) map[string]string {
	r, err := a.compose(ctx)
	if err != nil {
		return nil
	}
	paths := map[string]string{}
	for name, ext := range r.Ext {
		if ext.Proxy != nil && ext.Proxy.HealthPath != "" {
			paths[a.cfg.App+"-"+a.destName+"-"+name] = ext.Proxy.HealthPath
		}
	}
	return paths
}

func proxyUpgradeCmd(g *globals) *cobra.Command {
	var yes, allowDowngrade bool
	c := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade the shared Proxy to the pinned (or proxy.image) kamal-proxy",
		Long: `Recreate yoho-proxy with the configured kamal-proxy image on each Server of
the Destination, one Server at a time.

The image is pulled first: when the pull fails nothing was stopped. The
container is then recreated with the same ports, flags, network and state
volume, so the routes of every App on the Server are kept. Afterwards every
route must be listed again, unchanged, and answer through the Proxy as it did
before; otherwise the command fails and says which route.

Recreating the container interrupts traffic for a moment on every App served
by that Server (kamal-proxy has no handover between versions): plan a
maintenance window, or take the Server out of the load balancer in the
pre-proxy-reboot Hook. A newer running version than the configured one is
refused unless --allow-downgrade.

Hooks: pre-proxy-reboot and post-proxy-reboot around each Server
(YOHO_COMMAND=proxy, YOHO_SUBCOMMAND=upgrade). The post Hook runs only after
the routes were verified.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			cfg := a.proxyConfig()
			ok, err := confirm(cmd, yes, fmt.Sprintf("Upgrade %s to %s on %s? Traffic is briefly interrupted.", proxy.ContainerName, proxy.VersionLabel(proxy.Image(cfg)), strings.Join(a.dest.Servers, ", ")))
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("upgrade cancelled")
			}
			if err := a.proxyHookFor("upgrade", "", nil)(ctx, "pre-connect"); err != nil {
				return err
			}
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			paths := a.proxyHealthPaths(ctx)
			for _, h := range hosts {
				hook := a.proxyHookFor("upgrade", a.serverVersion(ctx, h.Host), []plan.NamedHost{h})
				step := a.ui.Step(h.Name, "Upgrade %s", proxy.ContainerName)
				res, err := proxy.Upgrade(ctx, h.Host, cfg, step.Output(), proxy.UpgradeOptions{
					AllowDowngrade: allowDowngrade, SkipNetwork: a.dest.Runtime == "swarm",
					OnReboot: hook, HealthPaths: paths,
				})
				if err != nil {
					step.Fail(err, "routes live in the "+proxy.Volume+" volume; inspect with `yoho proxy status` and `yoho proxy logs`")
					return &silentError{err}
				}
				if !res.Changed {
					step.Done("already " + proxy.VersionLabel(res.To))
					continue
				}
				verified := fmt.Sprintf("%d route(s) verified", res.Routes)
				if res.Blind {
					verified = "routes not compared (old Proxy cannot list as JSON)"
				}
				step.Done(fmt.Sprintf("%s -> %s, %s, recreate took %s", proxy.VersionLabel(res.From), proxy.VersionLabel(res.To), verified, res.Swap.Round(100*time.Millisecond)))
				if len(res.Unverified) > 0 {
					a.ui.Warn("%s: routes that did not answer before the upgrade either, so not verified: %s", h.Name, strings.Join(res.Unverified, ", "))
				}
			}
			return nil
		},
	}
	c.Flags().BoolVarP(&yes, "yes", "y", false, "Do not ask for confirmation")
	c.Flags().BoolVar(&allowDowngrade, "allow-downgrade", false, "Allow moving to an older kamal-proxy than the running one")
	return c
}

func proxyStatusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show the Proxy container, its version and its routes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			srv := a.cfg.Servers[a.dest.Servers[0]]
			h, err := remote.NewSSH(a.dest.Servers[0], srv.SSH, srv.Sudo, a.sshOptions())
			if err != nil {
				return err
			}
			defer h.Close()
			ctx := cmd.Context()
			script := "docker ps --filter name=^/" + proxy.ContainerName + "$ --format '{{.Names}}\\t{{.Status}}\\t{{.Ports}}'; docker exec " + proxy.ContainerName + " kamal-proxy list"
			if err := h.Run(ctx, remote.Cmd{Script: script, Stdout: cmd.OutOrStdout(), Stderr: cmd.ErrOrStderr()}); err != nil {
				return err
			}
			st, err := proxy.Status(ctx, h, a.proxyConfig())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), st.Summary())
			return nil
		},
	}
}
