package mesh

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
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
		hosts = append(hosts, plan.NamedHost{Name: name, Server: config.Server{SSH: "root@203.0.113.1", Endpoint: endpoint}, Host: &fakeHost{files: map[string][]byte{"/var/lib/yoho/mesh/state.json": b, "/etc/wireguard/yoho0.conf": mustRender(t, name, s)}}})
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

func mustRender(t *testing.T, name string, s State) []byte {
	t.Helper()
	out, err := Render(name, s.Network, s)
	if err != nil {
		t.Fatal(err)
	}
	return []byte(out)
}

// pingHost preserves ping's exit status, as the real Host does.
type pingHost struct {
	fakeHost
	output string
	err    error
}

func (h *pingHost) Output(context.Context, remote.Cmd) (string, error) { return h.output, h.err }
func TestPingAverage(t *testing.T) {
	for _, tc := range []struct {
		out, want string
		err       error
	}{
		{"PING peer\n64 bytes from peer\nrtt min/avg/max/mdev = 0.100/1.230/2.000/0.300 ms\n", "1.230ms", nil},
		{"100% packet loss", "", fmt.Errorf("exit 1")},
		{"rtt min/avg/max/mdev = bad/input", "", nil},
	} {
		got, err := Ping(context.Background(), plan.NamedHost{Host: &pingHost{output: tc.out, err: tc.err}}, "10.210.0.2")
		if got != tc.want || (err != nil) != (tc.err != nil) {
			t.Fatalf("got %q, %v", got, err)
		}
	}
}
func TestRouteTypes(t *testing.T) {
	for _, typ := range []string{"blackhole", "unreachable", "prohibit", "throw", "local", "broadcast", "unicast"} {
		if err := Collisions("10.210.0.0/24", typ+" 10.210.0.0/24 table 52", nil); err == nil {
			t.Fatal("missed", typ)
		}
	}
}
func TestRemovalPreservesAddresses(t *testing.T) {
	ctx := context.Background()
	var hosts []plan.NamedHost
	for _, name := range []string{"a", "b", "c"} {
		hosts = append(hosts, plan.NamedHost{Name: name, Server: config.Server{SSH: "root@203.0.113.1"}, Host: &fakeHost{files: map[string][]byte{}, public: fixture().Peers[name].PublicKey}})
	}
	if err := Up(ctx, config.NetworkConfig{}, hosts, false); err != nil {
		t.Fatal(err)
	}
	survivors := []plan.NamedHost{hosts[0], hosts[2]}
	if err := Up(ctx, config.NetworkConfig{}, survivors, false); err != nil {
		t.Fatal(err)
	}
	copies, err := Read(ctx, survivors)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range copies {
		if _, ok := s.Peers["b"]; ok {
			t.Fatal("removed peer retained")
		}
		next, err := Allocate("10.210.0.0/24", []string{"a", "c", "d"}, []State{s})
		if err != nil || next.Addresses["c"] != "10.210.0.3" || next.Addresses["b"] != "10.210.0.2" || next.Addresses["d"] != "10.210.0.4" {
			t.Fatal(next, err)
		}
	}
}
func TestKeyDivergenceAndStaleConfig(t *testing.T) {
	ctx := context.Background()
	var hosts []plan.NamedHost
	s := fixture()
	s.Network = Defaults(config.NetworkConfig{})
	for _, name := range []string{"a", "b", "c"} {
		b, _ := json.Marshal(s)
		hosts = append(hosts, plan.NamedHost{Name: name, Server: config.Server{Endpoint: s.Peers[name].Endpoint}, Host: &fakeHost{files: map[string][]byte{"/var/lib/yoho/mesh/state.json": b, "/etc/wireguard/yoho0.conf": mustRender(t, name, s)}}})
	}
	peer := s.Peers["a"]
	peer.PublicKey = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	s.Peers["a"] = peer
	b, _ := json.Marshal(s)
	hosts[0].Host.(*fakeHost).files[statePath(hosts[0])] = b
	changes, err := Diff(ctx, s.Network, hosts)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range changes {
		if c.Name == "a" && c.Server == "b" {
			found = true
		}
	}
	if !found {
		t.Fatal("public key divergence missed", changes)
	}
	for _, h := range hosts {
		h.Host.(*fakeHost).files[statePath(h)] = b
	}
	changes, err = Diff(ctx, s.Network, hosts)
	if err != nil || len(changes) == 0 {
		t.Fatal("stale render missed", changes, err)
	}
}

// A failed state publication must leave every interface configuration untouched.
type failingStateHost struct {
	*fakeHost
	fail bool
}

func (h *failingStateHost) WriteFile(ctx context.Context, p string, b []byte, m os.FileMode, sudo bool) error {
	if h.fail && strings.HasSuffix(p, "state.json") {
		return fmt.Errorf("interrupted")
	}
	return h.fakeHost.WriteFile(ctx, p, b, m, sudo)
}
func TestRotationPublishesStatesBeforeConfigs(t *testing.T) {
	ctx := context.Background()
	var hosts []plan.NamedHost
	for _, name := range []string{"a", "b"} {
		hosts = append(hosts, plan.NamedHost{Name: name, Server: config.Server{SSH: "root@203.0.113.1"}, Host: &failingStateHost{fakeHost: &fakeHost{files: map[string][]byte{}, public: fixture().Peers[name].PublicKey}}})
	}
	if err := Up(ctx, config.NetworkConfig{}, hosts, false); err != nil {
		t.Fatal(err)
	}
	old := string(hosts[0].Host.(*failingStateHost).files["/etc/wireguard/yoho0.conf"])
	for _, h := range hosts {
		h.Host.(*failingStateHost).public = base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))
	}
	hosts[1].Host.(*failingStateHost).fail = true
	if err := Up(ctx, config.NetworkConfig{}, hosts, true); err == nil {
		t.Fatal("expected interruption")
	}
	if string(hosts[0].Host.(*failingStateHost).files["/etc/wireguard/yoho0.conf"]) != old {
		t.Fatal("configuration changed before all states published")
	}
}
func TestForwardingAndOutboundOnly(t *testing.T) {
	s := fixture()
	hubConfig := string(mustRender(t, "a", s))
	for _, want := range []string{"PostUp = " + forwardUp, "PostDown = " + forwardDown, "--comment yoho-mesh", "DOCKER-USER"} {
		if !strings.Contains(hubConfig, want) {
			t.Fatal("missing", want)
		}
	}
	if strings.Contains(string(mustRender(t, "b", s)), "ListenPort") {
		t.Fatal("NAT Server listens on fixed port")
	}
	for _, script := range []string{prerequisites, forwardUp, forwardDown} {
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(script)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatal(err, string(b))
		}
	}
}

func TestInboundFirewallFamiliesAndTopology(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "rules")
	defaults := filepath.Join(dir, "defaults")
	if err := os.WriteFile(filepath.Join(dir, "ufw"), []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$UFW_LOG\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	sources := map[string][]string{"a": {"203.0.113.1", "2001:db8::1"}, "b": {"203.0.113.2", "2001:db8::2"}, "c": {"203.0.113.3", "2001:db8::3"}}
	for _, mode := range []string{"no", "yes"} {
		if err := os.WriteFile(defaults, []byte("IPV6="+mode+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"a", "b", "c"} {
			_ = os.Remove(log)
			script := inboundFirewall(name, Defaults(config.NetworkConfig{}), fixture(), sources)
			script = strings.ReplaceAll(script, "/etc/default/ufw", remote.Quote(defaults))
			cmd := exec.Command("sh", "-c", "set -eu\n"+script)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "UFW_LOG="+log)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatal(err, string(out), script)
			}
			b, _ := os.ReadFile(log)
			if name == "b" && len(b) != 0 {
				t.Fatal("NAT Server opens inbound", string(b))
			}
			if name == "c" && strings.Contains(string(b), "203.0.113.2") {
				t.Fatal("public non-hub allows NAT peer")
			}
			want := 2
			if name == "b" {
				want = 0
			}
			if name == "c" {
				want = 1
			}
			if mode == "yes" {
				want *= 2
			}
			if strings.Count(string(b), "comment yoho-mesh") != want {
				t.Fatalf("%s/%s: %s", mode, name, b)
			}
		}
	}
}
func TestPreflightBeforeIPAndOptionalDocker(t *testing.T) {
	h := &fakeHost{files: map[string][]byte{}, public: fixture().Peers["a"].PublicKey}
	host := plan.NamedHost{Name: "a", Server: config.Server{SSH: "root@203.0.113.1"}, Host: h}
	if err := Up(context.Background(), config.NetworkConfig{}, []plan.NamedHost{host}, false); err != nil {
		t.Fatal(err)
	}
	if len(h.scripts) < 3 || h.scripts[0] != prerequisites || !strings.Contains(h.scripts[1], "ip -4 route") {
		t.Fatal("ip used before dependencies", h.scripts)
	}
	// Use an empty PATH to model a fresh Server with no Docker binary.
	cmd := exec.Command("/bin/sh", "-c", h.scripts[2])
	cmd.Env = []string{"PATH=" + t.TempDir()}
	if out, err := cmd.CombinedOutput(); err != nil || len(out) != 0 {
		t.Fatal("Docker absence fails collision preflight", err, string(out))
	}
	if err := Down(context.Background(), host); err != nil {
		t.Fatal(err)
	}
	down := h.scripts[len(h.scripts)-1]
	if strings.Index(down, "systemctl stop wg-quick@yoho0") < 0 || strings.Index(down, "systemctl stop") > strings.Index(down, "systemctl disable") || strings.Index(down, "systemctl stop") > strings.Index(down, "wg-quick down") {
		t.Fatal("unit not stopped before teardown", down)
	}
}

func TestForwardRuleIdempotence(t *testing.T) {
	dir := t.TempDir()
	stub := `#!/bin/sh
case "$1" in
 -nL) test "$DOCKER_CHAIN" = yes ;;
 -C) test -f "$RULE_DIR/$2" ;;
 -I) touch "$RULE_DIR/$2"; echo "$*" >> "$RULE_DIR/inserted" ;;
 -D) rm "$RULE_DIR/$2" ;;
 *) exit 1 ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "iptables"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	for _, docker := range []string{"yes", "no"} {
		rules := t.TempDir()
		env := append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "RULE_DIR="+rules, "DOCKER_CHAIN="+docker)
		run := func(script string) {
			t.Helper()
			cmd := exec.Command("sh", "-c", "set -eu\n"+script)
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatal(err, string(out))
			}
		}
		run(forwardUp)
		run(forwardUp)
		b, err := os.ReadFile(filepath.Join(rules, "inserted"))
		if err != nil || strings.Count(string(b), "--comment yoho-mesh") != 1 {
			t.Fatal("non-idempotent", string(b), err)
		}
		chain := "FORWARD"
		if docker == "yes" {
			chain = "DOCKER-USER"
		}
		if !strings.Contains(string(b), "-I "+chain+" 1 -i yoho0 -o yoho0") {
			t.Fatal("wrong forwarding chain", string(b))
		}
		run(forwardDown)
		run(forwardDown)
		if _, err := os.Stat(filepath.Join(rules, chain)); !os.IsNotExist(err) {
			t.Fatal("rule survived teardown", err)
		}
	}
}
func TestDiffDetectsKeyWrittenBeforeState(t *testing.T) {
	s := fixture()
	s.Network = Defaults(config.NetworkConfig{})
	b, _ := json.Marshal(s)
	h := &fakeHost{files: map[string][]byte{"/var/lib/yoho/mesh/state.json": b, "/etc/wireguard/yoho0.conf": mustRender(t, "a", s)}, public: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", 32)))}
	changes, err := Diff(context.Background(), s.Network, []plan.NamedHost{{Name: "a", Server: config.Server{Endpoint: s.Peers["a"].Endpoint}, Host: h}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Name == "a" && strings.Contains(strings.Join(c.Reasons, " "), "public key changed") {
			return
		}
	}
	t.Fatal("key change before state update missed", changes)
}
