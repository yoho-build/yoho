package cli

import (
	"context"
	"fmt"
	"slices"
	"sort"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/mesh"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
)

func init() { extraCommands = append(extraCommands, meshCmd) }
func meshCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "mesh", Short: "Manage the opt-in WireGuard network across Servers"}
	for _, verb := range []string{"up", "status", "down", "rotate-keys"} {
		c.AddCommand(&cobra.Command{Use: verb, Short: map[string]string{"up": "Reconcile the WireGuard mesh", "status": "Show peer handshake, transfer and RTT", "down": "Remove the Yoho mesh", "rotate-keys": "Generate new Server keys and reconcile peers"}[verb], Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			return a.runMesh(cmd.Context(), verb)
		}})
	}
	return c
}
func (a *app) meshHosts(ctx context.Context) ([]plan.NamedHost, func(), error) {
	var hosts []plan.NamedHost
	done := func() {
		for _, h := range hosts {
			h.Host.Close()
		}
	}
	ns := make([]string, 0, len(a.cfg.Servers))
	for n := range a.cfg.Servers {
		ns = append(ns, n)
	}
	sort.Strings(ns)
	for _, n := range ns {
		s := a.cfg.Servers[n]
		opts, e := a.meshSSHOptions(n)
		if e != nil {
			done()
			return nil, nil, e
		}
		h, e := remote.NewSSH(n, s.SSH, s.Sudo, opts)
		if e != nil {
			done()
			return nil, nil, e
		}
		hosts = append(hosts, plan.NamedHost{Name: n, Server: s, Host: h})
	}
	return hosts, done, nil
}
func (a *app) runMesh(ctx context.Context, verb string) error {
	if a.cfg.Network.Mesh != "wireguard" && verb != "down" {
		return fmt.Errorf("set network.mesh: wireguard before running yoho mesh %s", verb)
	}
	hosts, done, err := a.meshHosts(ctx)
	if err != nil {
		return err
	}
	defer done()
	step := a.ui.Step("", "Mesh %s", verb)
	switch verb {
	case "up", "rotate-keys":
		err = mesh.Up(ctx, a.cfg.Network, hosts, verb == "rotate-keys")
	case "down":
		for _, h := range hosts {
			if err = mesh.Down(ctx, h); err != nil {
				break
			}
		}
	case "status":
		var copies []mesh.State
		copies, err = mesh.Read(ctx, hosts)
		if err == nil {
			rows := [][]string{}
			for i, h := range hosts {
				peers, e := mesh.Status(ctx, h)
				if e != nil {
					err = e
					break
				}
				for _, p := range peers {
					name, address := "unknown", ""
					for n, peer := range copies[i].Peers {
						if peer.PublicKey == p.PublicKey {
							name, address = n, peer.Address
						}
					}
					age := "never"
					if p.HandshakeAgeSeconds != nil {
						age = fmt.Sprintf("%ds", *p.HandshakeAgeSeconds)
					}
					rtt := "unreachable"
					if address != "" {
						if out, e := mesh.Ping(ctx, h, address); e == nil {
							rtt = out
						}
					}
					a.ui.Event("mesh_peer", "", map[string]any{"server": h.Name, "peer": name, "address": address, "handshake_age_seconds": p.HandshakeAgeSeconds, "rx_bytes": p.RX, "tx_bytes": p.TX, "rtt": rtt})
					rows = append(rows, []string{h.Name, name, address, age, fmt.Sprint(p.RX), fmt.Sprint(p.TX), rtt})
				}
			}
			if err == nil {
				a.ui.Table([]string{"SERVER", "PEER", "ADDRESS", "HANDSHAKE AGE", "RX BYTES", "TX BYTES", "RTT"}, rows)
			}
		}
	}
	if err != nil {
		step.Fail(err, "check root/passwordless sudo, peer endpoints and `yoho mesh up`; rerun to reconcile partial changes")
		return &silentError{err}
	}
	step.Done()
	return nil
}

func (a *app) meshChanges(ctx context.Context) ([]plan.Change, error) {
	if a.cfg.Network.Mesh != "wireguard" || a.previewName != "" {
		return nil, nil
	}
	hosts, done, err := a.meshHosts(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	return mesh.Diff(ctx, a.cfg.Network, hosts)
}

func (a *app) meshAdvertise(ctx context.Context, h plan.NamedHost) (string, error) {
	if a.cfg.Network.Mesh != "wireguard" {
		return advertiseAddr(h.Name, h.Server)
	}
	copies, err := mesh.Read(ctx, []plan.NamedHost{h})
	if err != nil {
		return "", err
	}
	address := copies[0].Addresses[h.Name]
	if address == "" {
		return "", fmt.Errorf("Server %s has no mesh allocation; run yoho mesh up", h.Name)
	}
	return h.Server.EffectivePrivateAddress(address), nil
}

// A Server shared by Destinations must have the same route in each.
func (a *app) meshSSHOptions(name string) (remote.SSHOptions, error) {
	var result remote.SSHOptions
	found := false
	for _, d := range a.cfg.Destinations {
		if !slices.Contains(d.Servers, name) {
			continue
		}
		var o remote.SSHOptions
		if d.SSH != nil && d.SSH.Via == "cloudflare" {
			o.CloudflareHostname = d.SSH.Hostname
		}
		if found && o.CloudflareHostname != result.CloudflareHostname {
			return result, fmt.Errorf("Server %s has ambiguous Destination SSH routing", name)
		}
		result, found = o, true
	}
	return result, nil
}

func (a *app) reconcileMesh(ctx context.Context) error {
	if a.previewName != "" || a.cfg.Network.Mesh != "wireguard" {
		return nil
	}
	return a.runMesh(ctx, "up")
}
