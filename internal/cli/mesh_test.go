package cli

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/mesh"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
)

type meshAddressHost struct {
	remote.Host
	state mesh.State
}

func (h meshAddressHost) ReadFile(context.Context, string, bool) ([]byte, error) {
	if h.state.Addresses == nil {
		return nil, os.ErrNotExist
	}
	return json.Marshal(h.state)
}
func TestMeshAdvertise(t *testing.T) {
	a := &app{cfg: &config.Config{}}
	h := plan.NamedHost{Name: "a", Server: config.Server{PrivateAddress: "10.0.0.1", SSH: "root@203.0.113.1"}, Host: meshAddressHost{state: mesh.State{Addresses: map[string]string{"a": "10.210.0.1"}}}}
	if got, e := a.meshAdvertise(context.Background(), h); e != nil || got != "10.0.0.1" {
		t.Fatal(got, e)
	}
	a.cfg.Network.Mesh = "wireguard"
	if got, e := a.meshAdvertise(context.Background(), h); e != nil || got != "10.210.0.1" {
		t.Fatal(got, e)
	}
	h.Host = meshAddressHost{}
	if _, e := a.meshAdvertise(context.Background(), h); e == nil {
		t.Fatal("must require mesh allocation")
	}
}
func TestMeshCommands(t *testing.T) {
	c := meshCmd(&globals{})
	if len(c.Commands()) != 4 {
		t.Fatal("missing mesh subcommands")
	}
	for _, verb := range []string{"up", "status", "down", "rotate-keys"} {
		sub, _, e := c.Find([]string{verb})
		if e != nil || sub == c {
			t.Fatal(verb, e)
		}
		if e = sub.Args(sub, []string{"extra"}); e == nil {
			t.Fatal("must reject arguments")
		}
	}
}

func TestMeshSSHRouting(t *testing.T) {
	a := &app{cfg: &config.Config{Destinations: map[string]config.Destination{
		"production": {Servers: []string{"home"}, SSH: &config.DestinationSSH{Via: "cloudflare", Hostname: "ssh.home.example.com"}},
		"staging":    {Servers: []string{"cloud"}},
	}}}
	for _, tc := range []struct{ name, want string }{{"home", "ssh.home.example.com"}, {"cloud", ""}, {"unused", ""}} {
		o, err := a.meshSSHOptions(tc.name)
		if err != nil || o.CloudflareHostname != tc.want {
			t.Fatal(tc, o, err)
		}
	}
	a.cfg.Destinations["shared"] = config.Destination{Servers: []string{"home"}}
	if _, err := a.meshSSHOptions("home"); err == nil {
		t.Fatal("accepted ambiguous routing")
	}
	a.cfg.Destinations["shared"] = a.cfg.Destinations["production"]
	if _, err := a.meshSSHOptions("home"); err != nil {
		t.Fatal(err)
	}
}
func TestPreviewSkipsMesh(t *testing.T) {
	// An invalid SSH target would fail if either path attempted mesh access.
	a := &app{previewName: "pr-1", cfg: &config.Config{Network: config.NetworkConfig{Mesh: "wireguard"}, Servers: map[string]config.Server{"production": {SSH: ""}}}}
	if err := a.reconcileMesh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if changes, err := a.meshChanges(context.Background()); err != nil || len(changes) != 0 {
		t.Fatal(changes, err)
	}
}
