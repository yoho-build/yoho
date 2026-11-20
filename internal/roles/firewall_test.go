package roles

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/remote"
)

// Execute the real reconcile script against an xtables state machine. This
// verifies atomic pointer swaps/foreign policy preservation rather than text
// assertions that merely mirror the renderer.
const xtablesStub = `#!/usr/bin/env python3
import json,os,sys
p=os.path.join(os.environ['XT_STATE'],os.path.basename(sys.argv[0])+'.json')
s=json.load(open(p)) if os.path.exists(p) else {'PREROUTING':[]}
a=sys.argv[1:]
while a and a[0] in ('-w','-t'):
 a=a[1:] if a[0]=='-w' else a[2:]
op=a[0];chain=a[1];args=a[2:];code=0
if op=='-N':
 if chain in s: code=1
 else:s[chain]=[]
elif op=='-S':
 if chain not in s: code=1
 else:
  for row in s[chain]:
   def q(x):return '"'+x.replace('"','\\"')+'"' if ' ' in x else x
   print('-A '+chain+' '+' '.join(q(x) for x in row))
elif op=='-A':
 if os.environ.get('XT_FAIL')=='1' and args[-1]=='DROP':code=1
 else:s[chain].append(args)
elif op=='-I':
 pos=int(args[0])-1;s[chain].insert(pos,args[1:])
elif op=='-C':code=0 if args in s.get(chain,[]) else 1
elif op=='-R':s[chain][int(args[0])-1]=args[1:]
elif op=='-F':s[chain]=[]
elif op=='-X':del s[chain]
else:code=2
json.dump(s,open(p,'w'));sys.exit(code)
`

type xtHost struct {
	remote.Host
	dir string
}

func (h xtHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", c.Script)
	cmd.Env = append(os.Environ(), "PATH="+h.dir+string(os.PathListSeparator)+os.Getenv("PATH"), "XT_STATE="+h.dir)
	b, e := cmd.CombinedOutput()
	return strings.TrimSpace(string(b)), e
}
func TestReviewFirewallAtomicRelativeAndReplySafety(t *testing.T) {
	if _, e := exec.LookPath("python3"); e != nil {
		t.Skip("python3 xtables fixture unavailable")
	}
	dir := t.TempDir()
	for _, tool := range []string{"iptables", "ip6tables"} {
		if e := os.WriteFile(filepath.Join(dir, tool), []byte(xtablesStub), 0700); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.WriteFile(filepath.Join(dir, "flock"), []byte("#!/bin/sh\nexit 0\n"), 0700); e != nil {
		t.Fatal(e)
	}
	_, d, addr := fixture()
	a, e := FirewallRules("shop", "staging", "b", d, addr)
	if e != nil {
		t.Fatal(e)
	}
	b, e := FirewallRules("shop", "production", "b", d, addr)
	if e != nil {
		t.Fatal(e)
	}
	for _, r := range a {
		joined := strings.Join(r.Args, " ")
		if !strings.Contains(joined, "--ctstate NEW --ctdir ORIGINAL") || strings.Contains(joined, "ctorigdst") || strings.Contains(joined, "ACCEPT") {
			t.Fatal("reply or filter bypass regression", joined)
		}
	}
	run := func(dest string, rules []Rule, fail bool) error {
		cmd := exec.Command("sh", "-s")
		cmd.Stdin = strings.NewReader(strings.ReplaceAll(FirewallScript("shop", dest, rules), "/run/lock", dir))
		cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"), "XT_STATE="+dir)
		if fail {
			cmd.Env = append(cmd.Env, "XT_FAIL=1")
		}
		out, e := cmd.CombinedOutput()
		if e != nil && !fail {
			t.Fatalf("%v %s", e, out)
		}
		return e
	}
	h := xtHost{dir: dir}
	for _, dest := range []string{"staging", "production", "staging"} {
		rs := a
		if dest == "production" {
			rs = b
		}
		if e := run(dest, rs, false); e != nil {
			t.Fatal(e)
		}
		if e := CheckFirewall(context.Background(), h, a); e != nil {
			t.Fatal("staging policy displaced by another App", e)
		}
	}
	if e := CheckFirewall(context.Background(), h, b); e != nil {
		t.Fatal("production policy displaced", e)
	}
	if e := run("staging", a, true); e == nil {
		t.Fatal("injected fill failure ignored")
	}
	if e := CheckFirewall(context.Background(), h, a); e != nil {
		t.Fatal("failed fill changed active policy", e)
	}
	// Swap relative rule order in just staging; presence alone is insufficient.
	file := filepath.Join(dir, "iptables.json")
	data, _ := os.ReadFile(file)
	state := map[string][][]string{}
	if e := json.Unmarshal(data, &state); e != nil {
		t.Fatal(e)
	}
	for chain, rs := range state {
		if strings.HasPrefix(chain, chainPrefix("shop", "staging")) && len(rs) == 2 {
			rs[0], rs[1] = rs[1], rs[0]
		}
	}
	data, _ = json.Marshal(state)
	_ = os.WriteFile(file, data, 0600)
	if e := CheckFirewall(context.Background(), h, a); e == nil {
		t.Fatal("DROP before consumer RETURN accepted")
	}
	if !strings.Contains(firewallUnit, "Before=network-pre.target docker.service") || strings.Contains(firewallUnit, "After=docker.service") {
		t.Fatal("boot window returned")
	}
}
func TestReviewFirewallReplayContinuesAfterFailure(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "01.sh"), []byte("exit 1\n"), 0600)
	_ = os.WriteFile(filepath.Join(dir, "02.sh"), []byte("echo restored > "+remote.Quote(filepath.Join(dir, "restored"))+"\n"), 0600)
	cmd := exec.Command("sh", "-s")
	cmd.Stdin = strings.NewReader(strings.ReplaceAll(firewallReplay, "/etc/yoho/firewall", dir))
	if e := cmd.Run(); e == nil {
		t.Fatal("replay lost failure exit")
	}
	if _, e := os.Stat(filepath.Join(dir, "restored")); e != nil {
		t.Fatal("later policy skipped", e)
	}
}

type bindingHost struct{ remote.Host }

func (bindingHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	if strings.Contains(c.Script, "docker ps -q") {
		return "container", nil
	}
	return `{"5432/tcp":[{"HostIp":"fd00::2","HostPort":"5432"},{"HostIp":"127.0.0.1","HostPort":"15432"}]}`, nil
}
func TestReviewBindingsCanonicalIPv6AndLoopback(t *testing.T) {
	_, d, addr := fixture()
	addr["b"] = "fd00:0::2"
	if e := CheckBindings(context.Background(), bindingHost{}, "shop", "production", "b", d, addr); e != nil {
		t.Fatal(e)
	}
}
