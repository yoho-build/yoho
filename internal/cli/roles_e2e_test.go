package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/audit"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/roles"
)

// Run only against explicitly supplied disposable Linux Servers with Docker,
// sudo, private routing, iptables/ip6tables DOCKER-USER chains and systemd.
// Nothing is added to CI. Addresses correspond to YOHO_E2E_ROLES_HOSTS order.
func TestE2ERolesPostgresMoveAndRecovery(t *testing.T) {
	if os.Getenv("YOHO_E2E") != "1" || os.Getenv("YOHO_E2E_ROLES_HOSTS") == "" {
		t.Skip("requires YOHO_E2E=1 and YOHO_E2E_ROLES_HOSTS=user@a,user@b[,user@c]")
	}
	targets := strings.Split(os.Getenv("YOHO_E2E_ROLES_HOSTS"), ",")
	if len(targets) < 2 {
		t.Fatal("at least two Roles Servers required")
	}
	addresses := strings.Split(os.Getenv("YOHO_E2E_ROLES_ADDRESSES"), ",")
	if len(addresses) != len(targets) {
		t.Fatal("set YOHO_E2E_ROLES_ADDRESSES to matching private IPs")
	}
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	appName := "roles-" + audit.NewRun()
	root := "/tmp/yoho-" + appName
	release.Root = root
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var hosts []remote.Host
	for i, target := range targets {
		h, e := remote.NewSSH(fmt.Sprint(i), target, true, remote.SSHOptions{})
		if e != nil {
			t.Fatal(e)
		}
		hosts = append(hosts, h)
	}
	project := release.ProjectName(appName, "production")
	// Cleanup selects exact project labels and owned App paths; no down -v.
	t.Cleanup(func() {
		for _, h := range hosts {
			cctx, cc := context.WithTimeout(context.Background(), time.Minute)
			_ = h.Run(cctx, remote.Cmd{Script: "set -eu\nids=$(docker ps -aq --filter " + remote.Quote("label=com.docker.compose.project="+project) + ")\n[ -z \"$ids\" ] || docker rm -f $ids\nvols=$(docker volume ls -q --filter " + remote.Quote("label=com.docker.compose.project="+project) + ")\n[ -z \"$vols\" ] || docker volume rm $vols\nrm -rf " + remote.Quote(filepath.Join(root, "apps", appName)) + "\n"})
			_ = roles.Protect(cctx, h, appName, "production", nil)
			_, _ = proxy.RemoveTunnelOwned(cctx, h, proxy.TunnelOwner(appName, "production"), io.Discard)
			cc()
			_ = h.Close()
		}
	})
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "yoho.json")
	composePath := filepath.Join(dir, "compose.yaml")
	cfg := config.Config{App: appName, Servers: map[string]config.Server{"a": {SSH: targets[0], Root: root, Sudo: true, PrivateAddress: addresses[0]}, "b": {SSH: targets[1], Root: root, Sudo: true, PrivateAddress: addresses[1]}}, Destinations: map[string]config.Destination{"production": {Servers: []string{"a", "b"}, Roles: map[string]config.Role{"app": {Services: []string{"web", "worker"}, Servers: []string{"a"}}, "data": {Services: []string{"db"}, Servers: []string{"b"}}}, Connections: map[string]config.Connection{"database": {Service: "db", Port: 5432, Consumers: []string{"web", "worker"}}}}}, Backups: config.BackupsConfig{Targets: map[string]config.BackupTarget{"local": {Type: "archive", Repository: filepath.Join(root, "apps", appName, "production", "backups")}}, Jobs: map[string]config.BackupJob{"db": {Destination: "production", Target: "local", Services: []string{"db"}}}}}
	writeCfg := func() {
		t.Helper()
		b, _ := json.Marshal(cfg)
		if e := os.WriteFile(cfgPath, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	writeCfg()
	// PostgreSQL trust authentication is confined to this disposable test and
	// its private, consumer-allowlisted port. No production credentials are used.
	body := `services:
  web:
    image: nginx:1.28-alpine
    restart: unless-stopped
    healthcheck:
      test: [CMD, wget, -q, -O, /dev/null, http://127.0.0.1/]
      interval: 1s
    x-yoho:
      proxy:
        hosts: [HOSTNAME]
        tls: false
        health_path: /
  worker:
    image: postgres:17-alpine
    restart: unless-stopped
    command: [sh, -ec, 'while :; do psql postgres://postgres@db/postgres -c "select 42"; sleep 1; done']
  db:
    image: postgres:17-alpine
    restart: unless-stopped
    environment: {POSTGRES_HOST_AUTH_METHOD: trust}
    volumes: [data:/var/lib/postgresql/data]
    healthcheck:
      test: [CMD, pg_isready, -h, 127.0.0.1]
      interval: 1s
    x-yoho:
      stateful: true
      backup: {volumes: [data]}
volumes: {data: {}}
`
	hostname := appName + ".test"
	body = strings.ReplaceAll(body, "HOSTNAME", hostname)
	if e := os.WriteFile(composePath, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	run := func(ctx context.Context, args ...string) (string, error) {
		var out bytes.Buffer
		g := &globals{configPath: cfgPath, json: true}
		c := &cobra.Command{Use: "yoho", SilenceUsage: true, SilenceErrors: true}
		c.SetOut(&out)
		c.SetErr(&out)
		c.AddCommand(deployCmd(g), applyCmd(g), rollbackCmd(g), backupCmd(g), serviceCmd(g), operationStatusCmd(g))
		auditWrap(g, c)
		c.SetArgs(args)
		e := c.ExecuteContext(ctx)
		return out.String(), e
	}
	must := func(args ...string) string {
		t.Helper()
		out, e := run(ctx, args...)
		if e != nil {
			t.Fatalf("%v: %v\n%s", args, e, out)
		}
		return out
	}
	must("deploy", "--version", "v1")
	sql := func(h remote.Host, query string) string {
		t.Helper()
		out, e := h.Output(ctx, remote.Cmd{Script: "set -eu\nid=$(docker ps -q --filter " + remote.Quote("label=com.docker.compose.project="+project) + " --filter label=com.docker.compose.service=db)\ndocker exec \"$id\" psql -U postgres -At -c " + remote.Quote(query)})
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	sql(hosts[1], "create table roles_rows(n integer); insert into roles_rows values(42)")
	// Compose-name resolution is tested inside the remote consumer, not on SSH.
	if _, e := hosts[0].Output(ctx, remote.Cmd{Script: "set -eu\nid=$(docker ps -q --filter " + remote.Quote("label=com.docker.compose.project="+project) + " --filter label=com.docker.compose.service=worker)\ndocker exec \"$id\" psql postgres://postgres@db/postgres -At -c 'select n from roles_rows'"}); e != nil {
		t.Fatal(e)
	}
	if len(hosts) > 2 {
		if e := hosts[2].Run(ctx, remote.Cmd{Script: "set -eu\ncommand -v timeout >/dev/null\ncommand -v nc >/dev/null\n! timeout 3 nc -z -w 1 " + remote.Quote(addresses[1]) + " 5432"}); e != nil {
			t.Fatal("third Server can reach protected database port", e)
		}
	}
	if public := os.Getenv("YOHO_E2E_ROLES_PUBLIC_B"); public != "" {
		if e := hosts[0].Run(ctx, remote.Cmd{Script: "set -eu\ncommand -v timeout >/dev/null\ncommand -v nc >/dev/null\n! timeout 3 nc -z -w 1 " + remote.Quote(public) + " 5432"}); e != nil {
			t.Fatal("database port reachable through provider public IP", e)
		}
	}
	must("backup", "run", "db")
	must("backup", "restore", "latest", "--job", "db", "--yes")
	must("deploy", "--version", "v2")
	must("rollback", "v1")
	// A consumer continuously requests its Proxy route while web is redeployed.
	pollCtx, pollCancel := context.WithCancel(ctx)
	pollDone := make(chan error, 1)
	go func() {
		pollDone <- hosts[0].Run(pollCtx, remote.Cmd{Script: "set -eu\ni=0; while [ \"$i\" -lt 100 ]; do curl -fsS -H " + remote.Quote("Host: "+hostname) + " http://127.0.0.1/ >/dev/null; i=$((i+1)); sleep 0.1; done"})
	}()
	must("deploy", "--version", "v3")
	e := <-pollDone
	pollCancel()
	if e != nil {
		t.Fatal("request failed across redeploy", e)
	}
	// Cancel the SSH-backed rollout after its durable operation starts.
	interrupted := strings.Replace(body, "  db:\n", "    x-yoho:\n      release_command: [sh, -c, 'sleep 30']\n  db:\n", 1)
	if e := os.WriteFile(composePath, []byte(interrupted), 0600); e != nil {
		t.Fatal(e)
	}
	interruptedCtx, interrupt := context.WithCancel(ctx)
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() { out, e := run(interruptedCtx, "deploy", "--version", "interrupted"); done <- result{out, e} }()
	deadline := time.Now().Add(time.Minute)
	started := false
	for time.Now().Before(deadline) {
		out, e := hosts[0].Output(ctx, remote.Cmd{Script: "set -eu\nfor f in " + remote.Quote(filepath.Join(root, "apps", appName, "production", "operations")) + "/*.json; do [ ! -f \"$f\" ] || cat \"$f\"; done"})
		if e == nil && strings.Contains(out, `"state":"running"`) {
			started = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	interrupt()
	res := <-done
	if !started || res.err == nil || !(strings.Contains(res.out, `"outcome":"unknown"`) || strings.Contains(res.out, `"outcome":"partial"`)) {
		t.Fatalf("interrupted result: %v\n%s", res.err, res.out)
	}
	if e := os.WriteFile(composePath, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	must("deploy", "--version", "converged")
	must("service", "move", "db", "--to", "a", "--auto-approve")
	if got := sql(hosts[0], "select n from roles_rows"); strings.TrimSpace(got) != "42" {
		t.Fatal("rows lost in move", got)
	}
	if e := hosts[1].Run(ctx, remote.Cmd{Script: "set -eu\ndocker volume inspect " + remote.Quote(project+"_data") + " >/dev/null"}); e != nil {
		t.Fatal("source data removed", e)
	}
	// Apply the printed placement as the operator would; a normal apply converges.
	dest := cfg.Destinations["production"]
	dest.Roles["data"] = config.Role{Services: []string{"db"}, Servers: []string{"a"}}
	cfg.Destinations["production"] = dest
	writeCfg()
	must("deploy", "--version", "v4")
	t.Run("named-tunnel-multiple-connectors", func(t *testing.T) {
		token, publicURL := os.Getenv("YOHO_E2E_ROLES_TUNNEL_TOKEN"), os.Getenv("YOHO_E2E_ROLES_TUNNEL_URL")
		if token == "" || publicURL == "" {
			t.Skip("requires a disposable named Tunnel token and its routed public URL")
		}
		if e := os.MkdirAll(filepath.Join(dir, ".yoho"), 0700); e != nil {
			t.Fatal(e)
		}
		encoded, _ := json.Marshal(token)
		if e := os.WriteFile(filepath.Join(dir, ".yoho", "secrets"), []byte("ROLES_TUNNEL="+string(encoded)+"\n"), 0600); e != nil {
			t.Fatal(e)
		}
		parsed, e := url.Parse(publicURL)
		if e != nil || parsed.Hostname() == "" {
			t.Fatal("invalid public Tunnel URL")
		}
		if e = os.WriteFile(composePath, []byte(strings.ReplaceAll(body, hostname, parsed.Hostname())), 0600); e != nil {
			t.Fatal(e)
		}
		cfg.Proxy.Tunnel = &config.TunnelConfig{TokenSecret: "ROLES_TUNNEL"}
		dest := cfg.Destinations["production"]
		dest.Roles["app"] = config.Role{Services: []string{"web", "worker"}, Servers: []string{"a", "b"}}
		cfg.Destinations["production"] = dest
		writeCfg()
		must("deploy", "--version", "tunnel")
		for _, h := range hosts[:2] {
			status, e := proxy.TunnelStatusOf(ctx, h)
			if e != nil || !status.Healthy() {
				t.Fatalf("connector on %s: %v %+v", h.Name(), e, status)
			}
		}
		client := &http.Client{Timeout: 10 * time.Second}
		request := func() error {
			req, e := http.NewRequestWithContext(ctx, http.MethodGet, publicURL, nil)
			if e != nil {
				return e
			}
			res, e := client.Do(req)
			if e != nil {
				return e
			}
			io.Copy(io.Discard, res.Body)
			res.Body.Close()
			if res.StatusCode != 200 {
				return fmt.Errorf("Tunnel returned HTTP %d", res.StatusCode)
			}
			return nil
		}
		if e := request(); e != nil {
			t.Fatal(e)
		}
		// Simulate loss of one ingress Server by stopping only this App's web
		// containers and its owned connector, preserving every other App.
		stopped, e := hosts[1].Output(ctx, remote.Cmd{Script: "set -eu\nids=$(docker ps -q --filter " + remote.Quote("label=yoho.tunnel.owner="+proxy.TunnelOwner(appName, "production")) + ")\nweb=$(docker ps -q --filter " + remote.Quote("label=com.docker.compose.project="+project) + " --filter label=com.docker.compose.service=web)\n[ -z \"$ids $web\" ] || docker stop $ids $web"})
		if e != nil {
			t.Fatal(e)
		}
		defer func() {
			_ = hosts[1].Run(context.Background(), remote.Cmd{Script: "set -eu\ndocker start " + remote.QuoteArgs(strings.Fields(stopped)...)})
		}()
		for i := 0; i < 10; i++ {
			if e := request(); e != nil {
				t.Fatal("request failed with one ingress Server down", e)
			}
		}
	})

}
