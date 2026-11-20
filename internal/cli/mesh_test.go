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
