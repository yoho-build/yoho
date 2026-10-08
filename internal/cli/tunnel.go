package cli

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/yoho-dev/yoho/internal/config"
	"github.com/yoho-dev/yoho/internal/proxy"
)

func init() { extraCommands = append(extraCommands, tunnelCmd) }

// tunnelConfig returns the Destination's tunnel settings or a hinted error.
func (a *app) tunnelConfig() (config.TunnelConfig, error) {
	if t := a.proxyConfig().Tunnel; t != nil {
		return *t, nil
	}
	return config.TunnelConfig{}, errors.New("no tunnel configured\nhint: add `tunnel: {}` (Quick Tunnel) or `tunnel: {token_secret: CF_TUNNEL_TOKEN}` under proxy in the Yoho file")
}

func tunnelCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "tunnel", Short: "Manage the Cloudflare Tunnel connector in front of the Proxy"}

	c.AddCommand(&cobra.Command{
		Use:   "up",
		Short: "Start or update the cloudflared connector(s)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			cfg, err := a.tunnelConfig()
			if err != nil {
				return err
			}
			token := ""
			if cfg.TokenSecret != "" {
				step := a.ui.Step("", "Resolve tunnel token")
				store, err := a.loadSecrets(ctx)
				if err == nil {
					var ok bool
					if token, ok = store.Get(cfg.TokenSecret); !ok {
						err = fmt.Errorf("secret %s not found", cfg.TokenSecret)
					}
				}
				if err != nil {
					step.Fail(err, "add "+cfg.TokenSecret+" to .yoho/secrets (the token from Cloudflare Zero Trust > Networks > Tunnels)")
					return &silentError{err}
				}
				step.Done()
			}
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			mode := "Quick Tunnel"
			if token != "" {
				mode = "managed tunnel"
			}
			var urls []string
			for _, h := range hosts {
				step := a.ui.Step(h.Name, "Start %s (%s)", proxy.TunnelContainer, mode)
				st, err := proxy.EnsureTunnel(ctx, h.Host, cfg, token, step.Output())
				if err != nil {
					step.Fail(err, "check `yoho tunnel status`, outbound access to Cloudflare (port 7844) and that the token is valid")
					return &silentError{err}
				}
				step.Done(fmt.Sprintf("%d connector(s)", len(st.Containers)))
				if st.URL != "" {
					urls = append(urls, h.Name+": "+st.URL)
				}
			}
			for _, u := range urls {
				a.ui.Info("  → %s", u)
			}
			if token != "" {
				a.ui.Info("  In the Cloudflare dashboard, set the tunnel's public hostname service to %s", proxy.TunnelOrigin)
			} else {
				a.ui.Info("  Quick Tunnel URLs are random and change when the connector is recreated; use token_secret for a stable hostname")
			}
			return nil
		},
	})

	c.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the connector(s), registered connections and Quick Tunnel URL",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			hosts, closeHosts, err := a.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer closeHosts()
			var rows [][]string
			for _, h := range hosts {
				st, err := proxy.TunnelStatusOf(cmd.Context(), h.Host)
				if err != nil {
					a.ui.Step(h.Name, "Tunnel status").Fail(err, "check that Docker is running on the Server")
					return &silentError{err}
				}
				if !st.Exists() {
					rows = append(rows, []string{h.Name, "-", "not running", "-", "-", "-"})
					continue
				}
				for _, c := range st.Containers {
					rows = append(rows, []string{h.Name, c.Name, c.State, strconv.Itoa(c.Connections), st.Mode, st.URL})
				}
			}
			a.ui.Table([]string{"Server", "Container", "State", "Connections", "Mode", "URL"}, rows)
			return nil
		},
	})

	c.AddCommand(&cobra.Command{
		Use:   "down",
		Short: "Remove the cloudflared connector(s) and the token file",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			hosts, closeHosts, err := a.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer closeHosts()
			for _, h := range hosts {
				step := a.ui.Step(h.Name, "Remove %s", proxy.TunnelContainer)
				if err := proxy.RemoveTunnel(cmd.Context(), h.Host, step.Output()); err != nil {
					step.Fail(err, "check that the user can run docker on the Server")
					return &silentError{err}
				}
				step.Done()
			}
			return nil
		},
	})
	return c
}
