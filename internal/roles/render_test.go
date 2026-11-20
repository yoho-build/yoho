package roles

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/yoho-build/yoho/internal/remote"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
)

func fixture() (*plan.Deploy, config.Destination, map[string]string) {
	dest := config.Destination{Servers: []string{"a", "b"}, Roles: map[string]config.Role{"app": {Services: []string{"web", "worker"}, Servers: []string{"a"}}, "data": {Services: []string{"db"}, Servers: []string{"b"}}}, Connections: map[string]config.Connection{"database": {Service: "db", Port: 5432, Consumers: []string{"web", "worker"}}}}
	d := &plan.Deploy{App: "shop", Destination: "production", Servers: []plan.NamedHost{{Name: "a"}, {Name: "b"}}, Project: &types.Project{Services: types.Services{"web": {Name: "web", WorkloadSpec: types.WorkloadSpec{DependsOn: types.DependsOnConfig{"db": {Condition: types.ServiceConditionHealthy}, "worker": {}}}}, "worker": {Name: "worker"}, "db": {Name: "db"}}}, Ext: map[string]config.ServiceExt{}, ServiceSecrets: map[string]map[string]string{"db": {"PASSWORD": "secret"}}}
	return d, dest, map[string]string{"a": "10.0.0.1", "b": "10.0.0.2"}
}
func TestRolesRender(t *testing.T) {
	d, dest, addresses := fixture()
	a, err := Render(d, dest, "a", addresses)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Project.Services) != 2 || a.Project.Services["db"].Name != "" {
		t.Fatal(a.Project.Services)
	}
	web := a.Project.Services["web"]
	if !reflect.DeepEqual(web.ExtraHosts, types.HostsList{"db": {"10.0.0.2"}, "db.shop.yoho.internal": {"10.0.0.2"}}) {
		t.Fatal(web.ExtraHosts)
	}
	if *web.Environment["YOHO_CONNECTION_DATABASE_HOST"] != "10.0.0.2" || *web.Environment["YOHO_CONNECTION_DATABASE_PORT"] != "5432" {
		t.Fatal(web.Environment)
	}
	if _, ok := web.DependsOn["db"]; ok {
		t.Fatal("cross-Server depends_on retained")
	}
	if _, ok := web.DependsOn["worker"]; !ok {
		t.Fatal("local dependency removed")
	}
	b, err := Render(d, dest, "b", addresses)
	if err != nil {
		t.Fatal(err)
	}
	port := b.Project.Services["db"].Ports[0]
	if port.HostIP != "10.0.0.2" || port.Target != 5432 || port.Published != "5432" {
		t.Fatal(port)
	}
	if len(a.ServiceSecrets["db"]) > 0 {
		t.Fatal("unplaced secret leaked")
	}
	if len(d.Project.Services["web"].DependsOn) != 2 || len(d.Project.Services["db"].Ports) > 0 {
		t.Fatal("input mutated")
	}
	addresses["b"] = "10.0.0.3"
	updated, _ := Render(d, dest, "a", addresses)
	if updated.Project.Services["web"].ExtraHosts["db"][0] != "10.0.0.3" {
		t.Fatal("stale address")
	}
}
func TestRolesSameServerAndLegacy(t *testing.T) {
	d, dest, addresses := fixture()
	dest.Roles["data"] = config.Role{Services: []string{"db"}, Servers: []string{"a"}}
	p, e := Render(d, dest, "a", addresses)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Project.Services["web"].ExtraHosts) != 0 || len(p.Project.Services["web"].Environment) != 0 {
		t.Fatal("local consumer changed")
	}
	legacy, e := Render(d, config.Destination{}, "a", nil)
	if e != nil || legacy != d {
		t.Fatal("legacy rendering changed")
	}
}
func TestRolesOrder(t *testing.T) {
	_, dest, _ := fixture()
	order, err := Order(dest)
	if err != nil || !reflect.DeepEqual(order, []string{"b", "a"}) {
		t.Fatal(order, err)
	}
	dest.Connections["reverse"] = config.Connection{Service: "web", Consumers: []string{"db"}, Port: 80}
	if _, err := Order(dest); err == nil {
		t.Fatal("cycle accepted")
	}
}
func TestRolesFirewall(t *testing.T) {
	_, dest, addresses := fixture()
	rules, err := FirewallRules("shop", "production", "b", dest, addresses)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatal(rules)
	}
	script := FirewallScript("shop", "production", rules)
	for _, want := range []string{"DOCKER-USER", "--ctorigdstport' '5432", "yoho-roles shop/production/database", "ip6tables", "-w -C", "sort -rn"} {
		if !strings.Contains(script, want) {
			t.Fatalf("missing %q: %s", want, script)
		}
	}
	drop, allow := strings.Index(script, "'-j' 'DROP'"), strings.Index(script, "'-j' 'ACCEPT'")
	if drop < 0 || allow < drop {
		t.Fatal("DROP must be inserted before allowlist")
	}
	if strings.Contains(script, "-F") {
		t.Fatal("must not flush shared chains")
	}
	empty := FirewallScript("shop", "production", nil)
	if !strings.Contains(empty, "keep=0") {
		t.Fatal("removed connections not reconciled")
	}
}

func TestRolesFirewallReconcilePreservesForeignRules(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX remote scripts")
	}
	dir := t.TempDir()
	stub := `#!/bin/sh
set -eu
state="$RULE_TEST_DIR/$(basename "$0").state"
[ -f "$state" ] || : > "$state"
[ "$1" != -w ] || shift
action=$1; shift
case "$action" in
-S) cat "$state";;
-I) { printf '%s\n' "$*"; cat "$state"; } > "$state.tmp"; mv "$state.tmp" "$state";;
-L) awk '{print NR, $0}' "$state" | sed 's/--comment \(yoho-roles [^ ]*\)/\/\* \1 \*\//' ;;
-D) awk -v n="$2" 'NR!=n' "$state" > "$state.tmp"; mv "$state.tmp" "$state";;
-C) grep -Fx -- "$*" "$state" >/dev/null;;
*) exit 2;;
esac
`
	for _, tool := range []string{"iptables", "ip6tables"} {
		if e := os.WriteFile(filepath.Join(dir, tool), []byte(stub), 0700); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(filepath.Join(dir, tool+".state"), []byte("DOCKER-USER -j RETURN --comment foreign\n"), 0600); e != nil {
			t.Fatal(e)
		}
	}
	_, dest, addresses := fixture()
	rules, e := FirewallRules("shop", "production", "b", dest, addresses)
	if e != nil {
		t.Fatal(e)
	}
	run := func(rules []Rule) {
		t.Helper()
		cmd := exec.Command("sh", "-s")
		cmd.Stdin = strings.NewReader(FirewallScript("shop", "production", rules))
		cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "RULE_TEST_DIR="+dir)
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("%v: %s", e, b)
		}
	}
	run(rules)
	first, _ := os.ReadFile(filepath.Join(dir, "iptables.state"))
	run(rules)
	second, _ := os.ReadFile(filepath.Join(dir, "iptables.state"))
	if !bytes.Equal(first, second) {
		t.Fatalf("reconcile not idempotent:\n%s\n%s", first, second)
	}
	run(nil)
	for _, tool := range []string{"iptables", "ip6tables"} {
		b, _ := os.ReadFile(filepath.Join(dir, tool+".state"))
		if string(b) != "DOCKER-USER -j RETURN --comment foreign\n" {
			t.Fatalf("foreign rules modified or stale rules retained: %s", b)
		}
	}
}

func TestRolesRenderGolden(t *testing.T) {
	d, dest, addresses := fixture()
	for _, server := range []string{"a", "b"} {
		p, e := Render(d, dest, server, addresses)
		if e != nil {
			t.Fatal(e)
		}
		got, e := p.Project.MarshalYAML()
		if e != nil {
			t.Fatal(e)
		}
		file := filepath.Join("testdata", "roles-"+server+".yaml.golden")
		if os.Getenv("UPDATE_GOLDEN") == "1" {
			if e = os.MkdirAll(filepath.Dir(file), 0755); e != nil {
				t.Fatal(e)
			}
			if e = os.WriteFile(file, got, 0644); e != nil {
				t.Fatal(e)
			}
		}
		want, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("Server %s Compose differs:\n%s", server, got)
		}
	}
}

// Multi-connector tunnels terminate TLS before traffic reaches each Proxy.
func TestRolesTunnelDisablesIndependentACME(t *testing.T) {
	d, dest, addresses := fixture()
	dest.Roles["app"] = config.Role{Services: []string{"web", "worker"}, Servers: []string{"a", "b"}}
	d.Proxy.Tunnel = &config.TunnelConfig{TokenSecret: "TUNNEL"}
	d.Ext["web"] = config.ServiceExt{Proxy: &config.ServiceProxy{TLS: true}}
	p, e := Render(d, dest, "a", addresses)
	if e != nil {
		t.Fatal(e)
	}
	if p.Ext["web"].Proxy.TLS {
		t.Fatal("independent ACME left enabled")
	}
	if !d.Ext["web"].Proxy.TLS {
		t.Fatal("input mutated")
	}
}

type policyHost struct {
	remote.Host
	data []byte
}

func (h policyHost) ReadFile(context.Context, string, bool) ([]byte, error) { return h.data, nil }
func TestRolesExpandProtectionKeepsOldConsumersUntilCutover(t *testing.T) {
	_, dest, addresses := fixture()
	old, e := FirewallRules("shop", "production", "b", dest, addresses)
	if e != nil {
		t.Fatal(e)
	}
	data, _ := json.Marshal(old)
	addresses["a"] = "10.0.0.3"
	desired, e := FirewallRules("shop", "production", "b", dest, addresses)
	if e != nil {
		t.Fatal(e)
	}
	expanded, e := ExpandProtection(context.Background(), policyHost{data: data}, "shop", "production", desired)
	if e != nil {
		t.Fatal(e)
	}
	if len(expanded) != 4 {
		t.Fatal(expanded)
	}
	drop := false
	foundOld := false
	for _, r := range expanded {
		if r.Args[len(r.Args)-1] == "DROP" {
			drop = true
		} else if drop {
			t.Fatal("old consumer ACCEPT placed after DROP")
		}
		for _, a := range r.Args {
			foundOld = foundOld || a == "10.0.0.1"
		}
	}
	if !foundOld {
		t.Fatal("old consumer lost protection during rollout")
	}
}

func TestRolesFiltersResourcesAndSortsConnections(t *testing.T) {
	d, dest, addresses := fixture()
	svc := d.Project.Services["db"]
	svc.Volumes = []types.ServiceVolumeConfig{{Type: types.VolumeTypeVolume, Source: "data", Target: "/data"}}
	svc.Secrets = []types.ServiceSecretConfig{{Source: "db-only"}}
	d.Project.Services["db"] = svc
	d.Project.Volumes = types.Volumes{"data": {}}
	d.Project.Secrets = types.Secrets{"db-only": {File: "secret.txt"}}
	dest.Connections["metrics"] = config.Connection{Service: "db", Port: 9187, Consumers: []string{"web"}}
	p, e := Render(d, dest, "a", addresses)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Project.Volumes) != 0 || len(p.Project.Secrets) != 0 {
		t.Fatal("resources leaked to consumer Server")
	}
	b, e := Render(d, dest, "b", addresses)
	if e != nil {
		t.Fatal(e)
	}
	if len(b.Project.Volumes) != 1 || len(b.Project.Secrets) != 1 {
		t.Fatal("provider resources dropped")
	}
	want, _ := b.Project.MarshalYAML()
	for i := 0; i < 20; i++ {
		b, e := Render(d, dest, "b", addresses)
		if e != nil {
			t.Fatal(e)
		}
		got, _ := b.Project.MarshalYAML()
		if !bytes.Equal(got, want) {
			t.Fatal("connection map changed rendered bytes")
		}
	}
}
