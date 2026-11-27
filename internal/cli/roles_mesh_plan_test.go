package cli

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/mesh"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
)

func meshFakeServers(t *testing.T, a *app, files map[string][]byte) {
	t.Helper()
	a.cfg.Network = config.NetworkConfig{Mesh: "wireguard", Subnet: "10.210.0.0/24"}
	old := connectServer
	t.Cleanup(func() { connectServer = old })
	connectServer = func(name, target string, sudo bool, opts remote.SSHOptions) (remote.Host, error) {
		return &doctorFakeHost{name: name, files: files}, nil
	}
}

func statePathFor(a *app, name string) string {
	return filepath.Join(a.cfg.Servers[name].Root, "mesh/state.json")
}

// plan before the first `yoho mesh up` uses the planned allocation, never private_address.
func TestMeshPlannedRolesAddressesBeforeMeshUp(t *testing.T) {
	a, _ := reviewApp(t)
	meshFakeServers(t, a, nil)
	a.meshPlanned = true
	hosts := []plan.NamedHost{{Name: "a", Server: config.Server{PrivateAddress: "10.0.0.98"}}, {Name: "b", Server: config.Server{PrivateAddress: "10.0.0.99"}}}
	if e := a.rolesAddresses(context.Background(), hosts); e != nil {
		t.Fatal(e)
	}
	if hosts[0].Server.PrivateAddress != "10.210.0.1" || hosts[1].Server.PrivateAddress != "10.210.0.2" {
		t.Fatal(hosts)
	}
}

// Existing allocations win; only missing Servers get a planned address.
func TestMeshPlannedRolesAddressesKeepExistingState(t *testing.T) {
	a, _ := reviewApp(t)
	b, _ := json.Marshal(mesh.State{Addresses: map[string]string{"b": "10.210.0.1", "gone": "10.210.0.2"}})
	meshFakeServers(t, a, map[string][]byte{statePathFor(a, "b"): b})
	a.meshPlanned = true
	hosts := []plan.NamedHost{{Name: "a"}, {Name: "b"}}
	if e := a.rolesAddresses(context.Background(), hosts); e != nil {
		t.Fatal(e)
	}
	if hosts[0].Server.PrivateAddress != "10.210.0.3" || hosts[1].Server.PrivateAddress != "10.210.0.1" {
		t.Fatal(hosts)
	}
}

// deploy/apply reconcile the mesh before reading addresses, and then read the
// real allocation strictly.
func TestMeshDeployReconcilesBeforeRolesAddresses(t *testing.T) {
	a, _ := reviewApp(t)
	files := map[string][]byte{}
	meshFakeServers(t, a, files)
	old := meshReconcile
	t.Cleanup(func() { meshReconcile = old })
	reconciled := false
	meshReconcile = func(a *app, _ context.Context) error {
		reconciled = true
		b, _ := json.Marshal(mesh.State{Addresses: map[string]string{"a": "10.210.0.7", "b": "10.210.0.8"}})
		files[statePathFor(a, "a")] = b
		return nil
	}
	a.meshPlanned = true
	hosts := []plan.NamedHost{{Name: "a", Server: config.Server{PrivateAddress: "10.0.0.98"}}, {Name: "b"}}
	if e := a.meshRolesAddresses(context.Background(), hosts); e != nil {
		t.Fatal(e)
	}
	if !reconciled || a.meshPlanned || hosts[0].Server.PrivateAddress != "10.210.0.7" || hosts[1].Server.PrivateAddress != "10.210.0.8" {
		t.Fatal(reconciled, a.meshPlanned, hosts)
	}
}

// A failed reconcile leaves no allocation: deploy stops instead of using a
// planned address or private_address.
func TestMeshDeployWithoutAllocationDoesNotFallback(t *testing.T) {
	a, _ := reviewApp(t)
	meshFakeServers(t, a, nil)
	old := meshReconcile
	t.Cleanup(func() { meshReconcile = old })
	meshReconcile = func(*app, context.Context) error { return nil }
	a.meshPlanned = true
	hosts := []plan.NamedHost{{Name: "a", Server: config.Server{PrivateAddress: "10.0.0.99"}}}
	if e := a.meshRolesAddresses(context.Background(), hosts); e == nil || !strings.Contains(e.Error(), "yoho mesh up") {
		t.Fatal(e)
	}
}
