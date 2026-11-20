package mesh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
	"github.com/yoho-build/yoho/internal/ui"
)

func TestAllocation(t *testing.T) {
	s, err := Allocate("10.210.0.0/29", []string{"b", "a"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ns := range [][]string{{"a", "b"}, {"b", "a", "c"}, {"b"}} {
		next, e := Allocate("10.210.0.0/29", ns, []State{s})
		if e != nil {
			t.Fatal(e)
		}
		for n, a := range s.Addresses {
			if next.Addresses[n] != a {
				t.Fatalf("allocation changed for %s", n)
			}
		}
	}
	t.Run("union", func(t *testing.T) {
		next, e := Allocate("10.210.0.0/29", []string{"a", "b", "c"}, []State{s, {Addresses: map[string]string{"c": "10.210.0.3"}}})
		if e != nil || next.Addresses["c"] != "10.210.0.3" {
			t.Fatal(next, e)
		}
	})
	t.Run("authority", func(t *testing.T) {
		next, e := Allocate("10.210.0.0/29", nil, []State{s, {Addresses: map[string]string{"a": "10.210.0.5"}}})
		if e != nil || next.Addresses["a"] != s.Addresses["a"] {
			t.Fatal(next, e)
		}
	})
	for _, tc := range []struct {
		subnet string
		copies []State
		ns     []string
	}{{"bad", nil, nil}, {"10.210.0.0/30", nil, []string{"a", "b", "c"}}, {"10.210.1.0/24", []State{s}, nil}, {"10.210.0.0/29", []State{{Addresses: map[string]string{"a": "10.210.0.1", "b": "10.210.0.1"}}}, nil}} {
		if _, e := Allocate(tc.subnet, tc.ns, tc.copies); e == nil {
			t.Fatal("expected invalid allocation", tc)
		}
	}
}
func fixture() State {
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	return State{Addresses: map[string]string{"a": "10.210.0.1", "b": "10.210.0.2", "c": "10.210.0.3"}, Peers: map[string]Peer{"a": {Address: "10.210.0.1", PublicKey: key, Endpoint: "203.0.113.1:51820"}, "b": {Address: "10.210.0.2", PublicKey: key, Endpoint: "none"}, "c": {Address: "10.210.0.3", PublicKey: key, Endpoint: "203.0.113.3:51820"}}}
}
func TestRenderGolden(t *testing.T) {
	for _, n := range []string{"a", "b", "c"} {
		t.Run(n, func(t *testing.T) {
			got, e := Render(n, config.NetworkConfig{}, fixture())
			if e != nil {
				t.Fatal(e)
			}
			b, e := os.ReadFile("testdata/" + n + ".golden")
			if e != nil {
				t.Fatal(e)
			}
			if got != string(b) {
				t.Fatalf("render mismatch\n%s", got)
			}
			again, e := Render(n, config.NetworkConfig{}, fixture())
			if e != nil || again != got {
				t.Fatal("non-idempotent")
			}
		})
	}
}
func TestCollisions(t *testing.T) {
	for _, tc := range []struct {
		name, routes string
		pools        []string
		bad          bool
	}{{"default", "default via 10.210.0.1 dev eth0", nil, false}, {"owned", "10.210.0.0/24 dev yoho0", nil, false}, {"route", "10.0.0.0/8 dev eth0", nil, true}, {"docker", "", []string{"10.210.0.0/25"}, true}, {"host", "local 10.210.0.1 dev eth0", nil, true}, {"unrelated", "172.17.0.0/16 dev docker0", nil, false}} {
		t.Run(tc.name, func(t *testing.T) {
			if e := Collisions("10.210.0.0/24", tc.routes, tc.pools); (e != nil) != tc.bad {
				t.Fatal(e)
			}
		})
	}
}
func TestStatus(t *testing.T) {
	p, e := ParseStatus("public 100 123 456\nnever 0 0 0", time.Unix(110, 0))
	if e != nil || len(p) != 2 || *p[0].HandshakeAgeSeconds != 10 || p[0].RX != 123 || p[1].HandshakeAgeSeconds != nil {
		t.Fatal(p, e)
	}
	for _, s := range []string{"private interface row", "k x 0 0", "k 0 x 0", "k 0 0 x"} {
		if _, e := ParseStatus(s, time.Now()); e == nil {
			t.Fatal("accepted invalid dump")
		}
	}
}

type fakeHost struct {
	files   map[string][]byte
	scripts []string
	public  string
}

func (h *fakeHost) Name() string { return "fake" }
func (h *fakeHost) Close() error { return nil }
func (h *fakeHost) Run(_ context.Context, c remote.Cmd) error {
	h.scripts = append(h.scripts, c.Script)
	return nil
}
func (h *fakeHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	h.scripts = append(h.scripts, c.Script)
	if strings.Contains(c.Script, "wg pubkey") {
		return h.public, nil
	}
	if strings.Contains(c.Script, "docker network") {
		return "172.17.0.0/16", nil
	}
	return "", nil
}
func (h *fakeHost) WriteFile(_ context.Context, p string, b []byte, _ os.FileMode, _ bool) error {
	h.files[p] = append([]byte(nil), b...)
	return nil
}
func (h *fakeHost) ReadFile(_ context.Context, p string, _ bool) ([]byte, error) {
	b, ok := h.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return b, nil
}
func TestReconcileAndPublicOnly(t *testing.T) {
	ctx := context.Background()
	var hosts []plan.NamedHost
	for _, n := range []string{"a", "b"} {
		h := &fakeHost{files: map[string][]byte{}, public: base64.StdEncoding.EncodeToString([]byte(strings.Repeat(n, 32)))}
		hosts = append(hosts, plan.NamedHost{Name: n, Server: config.Server{SSH: "root@203.0.113.1"}, Host: h})
	}
	if e := Up(ctx, config.NetworkConfig{}, hosts, false); e != nil {
		t.Fatal(e)
	}
	saved := map[string]string{}
	for _, h := range hosts {
		f := h.Host.(*fakeHost)
		saved[h.Name] = string(f.files["/etc/wireguard/yoho0.conf"])
	}
	if e := Up(ctx, config.NetworkConfig{}, hosts, false); e != nil {
		t.Fatal(e)
	}
	for _, h := range hosts {
		f := h.Host.(*fakeHost)
		if saved[h.Name] != string(f.files["/etc/wireguard/yoho0.conf"]) {
			t.Fatal("rerender changed")
		}
		for p, b := range f.files {
			if strings.Contains(string(b), "PRIVATE_KEY_SENTINEL") || strings.Contains(p, ".key") {
				t.Fatal("private key in local data")
			}
		}
		if !strings.Contains(strings.Join(f.scripts, "\n"), "wg genkey > /etc/wireguard/yoho0.key.next") {
			t.Fatal("key not generated remotely")
		}
	}
	changes, e := Diff(ctx, config.NetworkConfig{}, hosts)
	if e != nil || len(changes) != 0 {
		t.Fatal(changes, e)
	}
	copies, e := Read(ctx, hosts)
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(copies[0], copies[1]) {
		t.Fatal("states differ")
	}
	hosts = hosts[:1]
	if e = Up(ctx, config.NetworkConfig{}, hosts, false); e != nil {
		t.Fatal(e)
	}
	b := hosts[0].Host.(*fakeHost).files[statePath(hosts[0])]
	var s State
	if e = json.Unmarshal(b, &s); e != nil {
		t.Fatal(e)
	}
	if _, ok := s.Peers["b"]; ok || s.Addresses["b"] == "" {
		t.Fatal("removed peer or tombstone incorrect")
	}
	if e = Down(ctx, hosts[0]); e != nil {
		t.Fatal(e)
	}
}

func TestStatusFiltersPrivateKeyBeforeTransport(t *testing.T) {
	dir := t.TempDir()
	sentinel := "PRIVATE_KEY_SENTINEL_MUST_STAY_ON_SERVER"
	script := "#!/bin/sh\nif [ \"$4\" = dump ]; then\n printf '%s\\n' '" + sentinel + " public 51820 off' 'peer preshared endpoint 10.210.0.2/32 100 123 456 25'\nfi\n"
	// wg arguments are: show yoho0 dump (three arguments).
	script = strings.ReplaceAll(script, "$4", "$3")
	if e := os.WriteFile(filepath.Join(dir, "wg"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("sh", "-c", statusScript)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	stdout, e := cmd.Output()
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(stdout), sentinel) || strings.Contains(string(stdout), "preshared") {
		t.Fatal("key material crossed transport")
	}
	peers, e := ParseStatus(string(stdout), time.Unix(110, 0))
	if e != nil || len(peers) != 1 {
		t.Fatal(peers, e)
	}
	var events bytes.Buffer
	redactor := secrets.NewRedactor(&events) // Does not know the Server's private key.
	u := ui.New(redactor, ui.JSON, false)
	u.Event("mesh_peer", "", map[string]any{"peer": peers[0]})
	if e = redactor.Flush(); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(events.String(), sentinel) || strings.Contains(events.String(), "preshared") {
		t.Fatal("private material in events")
	}
	for _, name := range keys(fixture().Peers) {
		rendered, e := Render(name, config.NetworkConfig{}, fixture())
		if e != nil {
			t.Fatal(e)
		}
		if strings.Contains(rendered, sentinel) {
			t.Fatal("private key in config")
		}
	}
}

func TestRemoteScriptsArePOSIX(t *testing.T) {
	for _, script := range []string{prepare, clearFirewall, statusScript} {
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(script)
		if b, e := cmd.CombinedOutput(); e != nil {
			t.Fatalf("invalid script: %v %s", e, b)
		}
	}
}
func TestMeshDiff(t *testing.T) {
	ctx := context.Background()
	s := fixture()
	s.Network = Defaults(config.NetworkConfig{})
	var hosts []plan.NamedHost
	for _, name := range []string{"a", "b", "c"} {
		b, e := json.Marshal(s)
		if e != nil {
			t.Fatal(e)
		}
		endpoint := s.Peers[name].Endpoint
		hosts = append(hosts, plan.NamedHost{Name: name, Server: config.Server{SSH: "root@203.0.113.1", Endpoint: endpoint}, Host: &fakeHost{files: map[string][]byte{"/var/lib/yoho/mesh/state.json": b}}})
	}
	if c, e := Diff(ctx, config.NetworkConfig{}, hosts); e != nil || len(c) != 0 {
		t.Fatal(c, e)
	}
	n := Defaults(config.NetworkConfig{})
	n.Port++
	c, e := Diff(ctx, n, hosts)
	if e != nil {
		t.Fatal(e)
	}
	if len(c) != 3 {
		t.Fatalf("missing interface port changes: %+v", c)
	}
	hosts = hosts[:2]
	c, e = Diff(ctx, config.NetworkConfig{}, hosts)
	if e != nil {
		t.Fatal(e)
	}
	removed := 0
	for _, change := range c {
		if change.Action == plan.ActionDelete && change.Name == "c" {
			removed++
		}
	}
	if removed != 2 {
		t.Fatalf("missing peer removals: %+v", c)
	}
}

func TestTwoNATPeersUseHub(t *testing.T) {
	s := fixture()
	s.Peers["d"] = Peer{Address: "10.210.0.4", PublicKey: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("d", 32))), Endpoint: "none"}
	for _, name := range []string{"b", "d"} {
		out, e := Render(name, config.NetworkConfig{}, s)
		if e != nil {
			t.Fatal(e)
		}
		if strings.Count(out, "[Peer]") != 1 || !strings.Contains(out, "AllowedIPs = 10.210.0.0/24") || !strings.Contains(out, "PersistentKeepalive = 25") {
			t.Fatal(out)
		}
	}
	out, e := Render("a", config.NetworkConfig{}, s)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out, "10.210.0.2/32") || !strings.Contains(out, "10.210.0.4/32") {
		t.Fatal(out)
	}
	out, e = Render("c", config.NetworkConfig{}, s)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out, "AllowedIPs = 10.210.0.1/32, 10.210.0.2/32, 10.210.0.4/32") {
		t.Fatal(out)
	}
}

func TestFirewallCleanupIncludesInactiveRules(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "deleted")
	script := `#!/bin/sh
if [ "$1 $2" = 'show added' ]; then
 cat <<'RULES'
Added user rules (see 'ufw status' for running firewall):
ufw allow from 203.0.113.1 to any port 51820 proto udp comment 'yoho-mesh'
ufw route allow in on yoho0 out on yoho0 from 10.210.0.0/24 to 10.210.0.0/24 comment 'yoho-mesh'
ufw allow from 203.0.113.2 to any port 22 proto tcp comment 'another-app'
RULES
else
 printf '%s\n' "$*" >> "$UFW_LOG"
fi
`
	if e := os.WriteFile(filepath.Join(dir, "ufw"), []byte(script), 0700); e != nil {
		t.Fatal(e)
	}
	cmd := exec.Command("sh", "-c", "set -eu\n"+clearFirewall)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "UFW_LOG="+log)
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatal(e, string(out))
	}
	b, e := os.ReadFile(log)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Count(string(b), "--force delete") != 2 || strings.Contains(string(b), "203.0.113.2") {
		t.Fatalf("wrong rules removed: %s", b)
	}
}
