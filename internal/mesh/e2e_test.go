package mesh

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
)

// Requires dedicated Debian/Ubuntu Servers with root/passwordless sudo, Docker,
// and no existing yoho0. The last Server is treated as inbound-disabled/NATed.
func TestE2EMesh(t *testing.T) {
	raw := os.Getenv("YOHO_E2E_MESH_HOSTS")
	if os.Getenv("YOHO_E2E") != "1" || raw == "" {
		t.Skip("requires YOHO_E2E=1 and YOHO_E2E_MESH_HOSTS=user@h1,user@h2[,user@h3]")
	}
	targets := strings.Split(raw, ",")
	if len(targets) < 2 || len(targets) > 3 {
		t.Fatal("need two or three dedicated Servers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	var hosts []plan.NamedHost
	for i, target := range targets {
		name := fmt.Sprintf("server%d", i)
		s := config.Server{SSH: strings.TrimSpace(target), Sudo: true, Root: "/var/lib/yoho-mesh-e2e"}
		if i == len(targets)-1 {
			s.Endpoint = "none"
		}
		h, e := remote.NewSSH(name, s.SSH, true, remote.SSHOptions{})
		if e != nil {
			t.Fatal(e)
		}
		if e = h.Run(ctx, remote.Cmd{Script: "set -eu\ntest ! -e /etc/wireguard/yoho0.conf\n! ip link show yoho0 >/dev/null 2>&1", Sudo: true}); e != nil {
			h.Close()
			t.Fatal("Server already has yoho0", e)
		}
		named := plan.NamedHost{Name: name, Server: s, Host: h}
		hosts = append(hosts, named)
		t.Cleanup(func() {
			clean, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			if e := Down(clean, named); e != nil {
				t.Error(e)
			}
			h.Close()
		})
	}
	n := Defaults(config.NetworkConfig{Mesh: "wireguard"})
	if e := Up(ctx, n, hosts, false); e != nil {
		t.Fatal(e)
	}
	states, e := Read(ctx, hosts)
	if e != nil {
		t.Fatal(e)
	}
	// The NATed Server has no inbound accept rule for this port; a preceding
	// deny proves its traffic must initiate outbound through the public hub.
	nat := hosts[len(hosts)-1]
	if e = nat.Host.Run(ctx, remote.Cmd{Sudo: true, Script: "set -eu\nufw insert 1 deny in proto udp to any port 51820 comment yoho-mesh >/dev/null"}); e != nil {
		t.Fatal(e)
	}
	for _, from := range hosts {
		for _, to := range hosts {
			if from.Name == to.Name {
				continue
			}
			if _, e = Ping(ctx, from, states[0].Addresses[to.Name]); e != nil {
				t.Fatalf("%s -> %s: %v", from.Name, to.Name, e)
			}
		}
	}
	if e = Up(ctx, n, hosts, true); e != nil {
		t.Fatal(e)
	}
	states, e = Read(ctx, hosts)
	if e != nil {
		t.Fatal(e)
	}
	kept := hosts[:len(hosts)-1]
	if e = Up(ctx, n, kept, false); e != nil {
		t.Fatal(e)
	}
	for _, h := range kept {
		b, e := h.Host.ReadFile(ctx, "/etc/wireguard/yoho0.conf", true)
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(string(b), states[0].Peers[nat.Name].PublicKey) {
			t.Fatal("removed public key remains")
		}
	}
	for _, h := range hosts {
		if e = Down(ctx, h); e != nil {
			t.Fatal(e)
		}
		if e = h.Host.Run(ctx, remote.Cmd{Sudo: true, Script: "set -eu\n! ip link show yoho0 >/dev/null 2>&1\ntest ! -e /etc/wireguard/yoho0.key\n! ufw status | grep '# yoho-mesh'"}); e != nil {
			t.Fatal(e)
		}
	}
}
