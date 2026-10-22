package migrate_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/migrate"
	"github.com/yoho-build/yoho/internal/migrate/migratetest"
	"github.com/yoho-build/yoho/internal/proxy"
)

const apiToken = "cf-api-token-0123456789abcdef"

func oldStack() *migratetest.Host {
	project := func(p string) map[string]string { return map[string]string{"com.docker.compose.project": p} }
	return &migratetest.Host{Containers: []*migratetest.Container{
		{ID: "aaa111", Name: "old-web", Labels: project("old-stack"), Running: true},
		{ID: "bbb222", Name: "old-db", Labels: project("old-stack"), Running: true, Mounts: []migratetest.Mount{
			{Type: "volume", Name: "old_pgdata", Source: "/var/lib/docker/volumes/old_pgdata/_data", RW: true},
			{Type: "bind", Source: "/data/app/postgres", RW: true},
		}},
		{ID: "ccc333", Name: "other-app", Labels: project("other"), Running: true, Mounts: []migratetest.Mount{{Type: "bind", Source: "/srv/other", RW: true}}},
		{ID: "ddd444", Name: "node-exporter", Running: true, Mounts: []migratetest.Mount{{Type: "bind", Source: "/", RW: false}}},
		{ID: "eee555", Name: "yoho-proxy", Running: true},
	}}
}

func yohoService(h *migratetest.Host) {
	h.Containers = append(h.Containers, &migratetest.Container{ID: "fff666", Name: "yoho-shop-production-db-1", Running: true,
		Labels: map[string]string{"com.docker.compose.project": "yoho-shop-production", "yoho.app": "shop", "yoho.destination": "production"},
		Mounts: []migratetest.Mount{{Type: "volume", Name: "old_pgdata", RW: true}}})
}

func migrateConfig() config.Migrate {
	return config.Migrate{
		From:   "coolify",
		Stop:   config.MigrateStop{ComposeProject: "old-stack"},
		Claim:  config.MigrateClaim{Paths: []string{"/data/app/postgres"}, Volumes: []string{"old_pgdata"}},
		Tunnel: &config.MigrateTunnel{APITokenSecret: "CF_API_TOKEN", Hostnames: []string{"app.example.com", "api.example.com"}},
	}
}

func setup(t *testing.T, token string) (*migratetest.Host, *migratetest.Cloudflare, migrate.Options) {
	t.Helper()
	cf := migratetest.NewCloudflare(t, apiToken, migratetest.IngressConfig)
	t.Setenv("YOHO_CLOUDFLARE_API", cf.URL)
	api, err := proxy.NewTunnelAPI(token, cf.TunnelToken())
	if err != nil {
		t.Fatal(err)
	}
	return oldStack(), cf, migrate.Options{App: "shop", Destination: "production", Migrate: migrateConfig(), CF: api}
}

func stopped(h *migratetest.Host) bool {
	for _, s := range h.Scripts {
		if strings.Contains(s, "docker stop") {
			return true
		}
	}
	return len(h.Ops) > 0
}

func TestPreflightFailureStopsNothing(t *testing.T) {
	ctx := context.Background()
	t.Run("expired token", func(t *testing.T) {
		h, _, o := setup(t, "expired-token-value")
		_, err := migrate.Preflight(ctx, h, o, nil)
		if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "Cloudflare Tunnel > Edit") {
			t.Fatalf("err = %v", err)
		}
		if strings.Contains(err.Error(), "expired-token-value") {
			t.Fatal("token in error")
		}
		if stopped(h) {
			t.Fatalf("docker stop sent: %v", h.Scripts)
		}
	})
	t.Run("missing hostname", func(t *testing.T) {
		h, _, o := setup(t, apiToken)
		o.Migrate.Tunnel.Hostnames = append(o.Migrate.Tunnel.Hostnames, "missing.example.com")
		_, err := migrate.Preflight(ctx, h, o, nil)
		if err == nil || !strings.Contains(err.Error(), "missing.example.com") {
			t.Fatalf("err = %v", err)
		}
		if stopped(h) {
			t.Fatal("stopped on preflight failure")
		}
	})
	t.Run("no match", func(t *testing.T) {
		h, _, o := setup(t, apiToken)
		o.Migrate.Stop = config.MigrateStop{Containers: []string{"gone"}}
		if _, err := migrate.Preflight(ctx, h, o, nil); err == nil || !strings.Contains(err.Error(), "--cutover-allow-empty") {
			t.Fatalf("err = %v", err)
		}
		o.AllowEmpty = true
		o.Migrate.Claim = config.MigrateClaim{}
		if _, err := migrate.Preflight(ctx, h, o, nil); err != nil {
			t.Fatalf("allow empty: %v", err)
		}
	})
	t.Run("yoho container selected", func(t *testing.T) {
		h, _, o := setup(t, apiToken)
		o.Migrate.Stop = config.MigrateStop{Containers: []string{"old-web"}, Labels: []string{"com.docker.compose.project"}}
		yohoService(h)
		_, err := migrate.Preflight(ctx, h, o, nil)
		if err == nil || !strings.Contains(err.Error(), "matches Yoho containers") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("stray mount", func(t *testing.T) {
		h, _, o := setup(t, apiToken)
		h.Containers = append(h.Containers, &migratetest.Container{ID: "999", Name: "stray", Running: true,
			Mounts: []migratetest.Mount{{Type: "bind", Source: "/data/app", RW: true}}})
		p, err := migrate.Preflight(ctx, h, o, nil)
		if err == nil || !strings.Contains(err.Error(), "stray mounts /data/app (claimed /data/app/postgres)") {
			t.Fatalf("err = %v", err)
		}
		if len(p.Conflicts) != 1 || stopped(h) {
			t.Fatalf("conflicts %v, ops %v", p.Conflicts, h.Ops)
		}
	})
}

func TestPreflightFindsContainersAndRules(t *testing.T) {
	h, _, o := setup(t, apiToken)
	p, err := migrate.Preflight(context.Background(), h, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range p.Containers {
		names = append(names, c.Name)
	}
	if strings.Join(names, ",") != "old-db,old-web" {
		t.Errorf("containers %v", names)
	}
	if len(p.Rules) != 2 || p.Rules[0].Service != "http://old-web:3000" {
		t.Errorf("rules %+v", p.Rules)
	}
}

func TestClaimCheckRefusesStrayMount(t *testing.T) {
	ctx := context.Background()
	claim := migrateConfig().Claim
	// The old platform restarts the database after docker stop.
	h := oldStack()
	h.AfterStop = func(h *migratetest.Host, c *migratetest.Container) {
		if c.Name == "old-db" {
			c.Running = true
		}
	}
	_, _, o := setup(t, apiToken)
	p, err := migrate.Preflight(ctx, h, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	j := migrate.NewJournal("shop", "production", "coolify", time.Now())
	if err := p.Stop(ctx, h, j); err != nil {
		t.Fatal(err)
	}
	err = p.CheckClaims(ctx, h)
	if err == nil || !strings.Contains(err.Error(), "old-db mounts old_pgdata") || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("err = %v", err)
	}

	// An unrelated container on the claimed volume; read-only mounts and
	// this App's own Services do not count.
	h = oldStack()
	h.Container("old-db").Running = false
	yohoService(h)
	if err := migrate.CheckClaims(ctx, h, "shop", "production", claim); err != nil {
		t.Fatalf("clear: %v", err)
	}
	h.Containers = append(h.Containers, &migratetest.Container{ID: "777", Name: "backup", Running: true,
		Mounts: []migratetest.Mount{{Type: "volume", Name: "old_pgdata", RW: true}}})
	if err := migrate.CheckClaims(ctx, h, "shop", "production", claim); err == nil || !strings.Contains(err.Error(), "backup mounts old_pgdata") {
		t.Fatalf("err = %v", err)
	}
	if err := migrate.CheckClaims(ctx, h, "shop", "staging", claim); err == nil || !strings.Contains(err.Error(), "yoho-shop-production-db-1") {
		t.Fatalf("another Destination of the App must count: %v", err)
	}
}

func cutover(t *testing.T) (*migratetest.Host, *migratetest.Cloudflare, migrate.Options, *migrate.Plan, *migrate.Journal) {
	t.Helper()
	ctx := context.Background()
	h, cf, o := setup(t, apiToken)
	p, err := migrate.Preflight(ctx, h, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	j := migrate.NewJournal("shop", "production", "coolify", time.Now())
	if err := p.Stop(ctx, h, j); err != nil {
		t.Fatal(err)
	}
	if err := p.CheckClaims(ctx, h); err != nil {
		t.Fatal(err)
	}
	yohoService(h)
	if _, err := p.SwitchIngress(ctx, h, j); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(ctx, h, j); err != nil {
		t.Fatal(err)
	}
	return h, cf, o, p, j
}

func TestSwitchIngressChangesOnlyListedHostnames(t *testing.T) {
	h, cf, _, p, j := cutover(t)
	rules, err := migrate.Rules(cf.Config())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://yoho-proxy:80", "http://yoho-proxy:80", "http://other:80", "http_status:404"}
	if len(rules) != len(want) {
		t.Fatalf("rules %+v", rules)
	}
	for i, r := range rules {
		if r.Service != want[i] {
			t.Errorf("rule %d (%s) service %q, want %q", i, r.Hostname, r.Service, want[i])
		}
	}
	if rules[3].Hostname != "" || rules[1].Path != "/v1" {
		t.Errorf("catch-all or path lost: %+v", rules)
	}
	var cfg map[string]any
	_ = json.Unmarshal(cf.Config(), &cfg)
	first := cfg["ingress"].([]any)[0].(map[string]any)
	if first["originRequest"] == nil || cfg["warp-routing"] == nil {
		t.Errorf("other fields lost: %s", cf.Config())
	}
	if migrate.Hash(j.TunnelBefore) != migrate.Hash(json.RawMessage(migratetest.IngressConfig)) || j.TunnelAfterHash != migrate.Hash(cf.Config()) {
		t.Error("journal does not hold the configuration before and the hash after")
	}
	// The saved Journal on the Server matches, 0600.
	path := migrate.JournalPath("shop", "production")
	if h.Modes[path] != 0o600 {
		t.Errorf("journal mode %v", h.Modes[path])
	}
	saved, err := migrate.ReadJournal(context.Background(), h, "shop", "production")
	if err != nil || saved.State != migrate.StateDone || len(saved.Stopped) != 2 || saved.TunnelAfterHash != j.TunnelAfterHash {
		t.Fatalf("saved %+v %v", saved, err)
	}
	// Rerun: already switched, no PUT; stopping again changes nothing.
	ops := len(h.Ops)
	wrote, err := p.SwitchIngress(context.Background(), h, j)
	if err != nil || wrote || cf.Puts != 1 {
		t.Fatalf("rerun wrote=%v puts=%d err=%v", wrote, cf.Puts, err)
	}
	if err := p.Stop(context.Background(), h, j); err != nil || len(h.Ops) != ops {
		t.Fatalf("second stop: %v %v", err, h.Ops)
	}
}

func TestRewriteIngressKeepsCatchAll(t *testing.T) {
	out, err := migrate.RewriteIngress(json.RawMessage(migratetest.IngressConfig), []string{"APP.example.com"}, "http://yoho-proxy:80")
	if err != nil {
		t.Fatal(err)
	}
	rules, _ := migrate.Rules(out)
	if rules[0].Service != "http://yoho-proxy:80" || rules[1].Service != "http://old-api:8080" || rules[3].Service != "http_status:404" {
		t.Errorf("rules %+v", rules)
	}
}

func TestRollbackRestoresAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h, cf, o, _, j := cutover(t)
	h.Ops = nil
	if err := migrate.Rollback(ctx, h, o.CF, j, migrate.RollbackOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.Ops, ","); got != "update yoho-shop-production-db-1 no,stop yoho-shop-production-db-1,start old-db,start old-web" {
		t.Errorf("ops %s", got)
	}
	if h.Container("other-app").Running != true || h.Container("yoho-proxy").Running != true {
		t.Error("touched containers outside the App")
	}
	if migrate.Hash(cf.Config()) != migrate.Hash(json.RawMessage(migratetest.IngressConfig)) {
		t.Errorf("ingress not restored: %s", cf.Config())
	}
	saved, _ := migrate.ReadJournal(ctx, h, "shop", "production")
	if saved.State != migrate.StateRolledBack || saved.RolledBackAt == nil {
		t.Errorf("state %s", saved.State)
	}
	for _, s := range h.Scripts {
		if strings.Contains(s, "docker rm") || strings.Contains(s, "down") {
			t.Errorf("destructive script: %s", s)
		}
	}
	puts, ops := cf.Puts, len(h.Ops)
	if err := migrate.Rollback(ctx, h, o.CF, saved, migrate.RollbackOptions{}); err != nil || cf.Puts != puts || len(h.Ops) != ops {
		t.Fatalf("second rollback not a no-op: %v", err)
	}
}

func TestRollbackRefusesDrift(t *testing.T) {
	ctx := context.Background()
	h, cf, o, _, j := cutover(t)
	edited := strings.Replace(string(cf.Config()), "http://other:80", "http://other:81", 1)
	cf.SetConfig(edited)
	h.Ops = nil
	err := migrate.Rollback(ctx, h, o.CF, j, migrate.RollbackOptions{})
	var drift *migrate.DriftError
	if !errors.As(err, &drift) {
		t.Fatalf("err = %v", err)
	}
	if len(h.Ops) != 0 || j.State != migrate.StateDone {
		t.Fatalf("changed before refusing: %v", h.Ops)
	}
	if err := migrate.Rollback(ctx, h, o.CF, j, migrate.RollbackOptions{Force: true}); err != nil {
		t.Fatal(err)
	}
	if migrate.Hash(cf.Config()) != migrate.Hash(json.RawMessage(migratetest.IngressConfig)) {
		t.Error("force did not restore")
	}
}

func TestNext(t *testing.T) {
	done := &migrate.Journal{ID: "c1", State: migrate.StateDone}
	started := &migrate.Journal{ID: "c1", State: migrate.StateStarted, Destination: "production"}
	back := &migrate.Journal{ID: "c1", State: migrate.StateRolledBack}
	for _, tc := range []struct {
		j      *migrate.Journal
		resume bool
		want   migrate.Action
		err    string
	}{
		{nil, false, migrate.Start, ""},
		{nil, true, migrate.Skip, "no unfinished cutover"},
		{done, false, migrate.Skip, ""},
		{done, true, migrate.Skip, ""},
		{started, false, migrate.Skip, "yoho rollback --cutover"},
		{started, true, migrate.Resume, ""},
		{back, false, migrate.Start, ""},
	} {
		got, err := migrate.Next(tc.j, tc.resume)
		if got != tc.want || (tc.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.err)) {
			t.Errorf("Next(%v, %v) = %v, %v", tc.j, tc.resume, got, err)
		}
	}
}

func TestResumeUsesSavedContainers(t *testing.T) {
	ctx := context.Background()
	h, _, o := setup(t, apiToken)
	p, err := migrate.Preflight(ctx, h, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	j := migrate.NewJournal("shop", "production", "", time.Now())
	if err := p.Stop(ctx, h, j); err != nil {
		t.Fatal(err)
	}
	// Interrupted here; the old containers are no longer running.
	p, err = migrate.Preflight(ctx, h, o, j)
	if err != nil || len(p.Containers) != 2 {
		t.Fatalf("resume preflight: %v %+v", err, p)
	}
}

func TestNoTokenInScriptsJournalOrOutput(t *testing.T) {
	h, cf, o, _, j := cutover(t)
	_ = migrate.Rollback(context.Background(), h, o.CF, j, migrate.RollbackOptions{})
	tunnelSecret := "dHVubmVsLXNlY3JldC12YWx1ZQ=="
	all := strings.Join(h.Scripts, "\n") + strings.Join(h.Env, "\n")
	for _, f := range h.Files {
		all += string(f)
	}
	for _, b := range cf.Bodies {
		all += b
	}
	for _, secret := range []string{apiToken, cf.TunnelToken(), tunnelSecret} {
		if strings.Contains(all, secret) {
			t.Errorf("secret %q leaked into scripts, journal or request bodies", secret[:8])
		}
	}
	for _, hd := range cf.Headers {
		if hd != "Bearer "+apiToken {
			t.Errorf("Authorization header %q", hd)
		}
	}
}

func TestRollbackRechecksClaimsAndRetiresContainers(t *testing.T) {
	ctx := context.Background()
	h, _, o, _, j := cutover(t)
	h.Container("yoho-shop-production-db-1").RestartPolicy = "on-failure:3"
	h.Containers = append(h.Containers, &migratetest.Container{ID: "stray", Name: "stray-writer", Running: true, Mounts: []migratetest.Mount{{Type: "volume", Name: "old_pgdata", RW: true}}})
	h.Ops = nil
	err := migrate.Rollback(ctx, h, o.CF, j, migrate.RollbackOptions{})
	if err == nil || !strings.Contains(err.Error(), "stray-writer") || !strings.Contains(err.Error(), "yoho rollback --cutover") {
		t.Fatalf("error %v", err)
	}
	for _, op := range h.Ops {
		if strings.HasPrefix(op, "start ") {
			t.Fatalf("unsafe restart: %v", h.Ops)
		}
	}
	if h.Container("yoho-shop-production-db-1").Running || h.Container("yoho-shop-production-db-1").RestartPolicy != "no" {
		t.Fatalf("not retired: %v", h.Ops)
	}
	saved, err := migrate.ReadJournal(ctx, h, "shop", "production")
	if err != nil || len(saved.Retired) != 1 || saved.Retired[0].RestartPolicy != "on-failure:3" || len(saved.Claim.Volumes) != 1 {
		t.Fatalf("saved %+v, %v", saved, err)
	}
	h.Container("stray-writer").Running = false
	if err := migrate.Rollback(ctx, h, o.CF, saved, migrate.RollbackOptions{}); err != nil {
		t.Fatal(err)
	}
	if !h.Container("old-db").Running {
		t.Fatal("old database not restarted")
	}
	if err := migrate.RestoreRetired(ctx, h, saved); err != nil {
		t.Fatal(err)
	}
	if h.Container("yoho-shop-production-db-1").RestartPolicy != "on-failure:3" {
		t.Fatal("lost original restart policy on retry")
	}
}

func TestSwitchIngressRevalidatesHostnames(t *testing.T) {
	ctx := context.Background()
	h, cf, o := setup(t, apiToken)
	p, err := migrate.Preflight(ctx, h, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	j := migrate.NewJournal("shop", "production", "", time.Now())
	cf.SetConfig(strings.Replace(migratetest.IngressConfig, "app.example.com", "removed.example.com", 1))
	if _, err := p.SwitchIngress(ctx, h, j); err == nil || !strings.Contains(err.Error(), "app.example.com") {
		t.Fatalf("error %v", err)
	}
	if cf.Puts != 0 || j.IngressSwitched() || j.State == migrate.StateDone {
		t.Fatalf("unexpected switch: %+v puts %d", j, cf.Puts)
	}
}

func TestResumeSkipsCompletedStop(t *testing.T) {
	ctx := context.Background()
	h, _, o, p, j := cutover(t)
	h.Container("old-db").Running = true
	h.Scripts = nil
	p, err := migrate.Preflight(ctx, h, o, j)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(ctx, h, j); err != nil {
		t.Fatal(err)
	}
	for _, script := range h.Scripts {
		if strings.Contains(script, "docker stop") {
			t.Fatal("repeated completed stop")
		}
	}
	if err := p.CheckClaims(ctx, h); err == nil {
		t.Fatal("resume must still detect restarted old writer")
	}
}

type afterPutAPI struct {
	migrate.CloudflareAPI
	after func()
}

func (a afterPutAPI) PutConfig(ctx context.Context, config json.RawMessage) error {
	if err := a.CloudflareAPI.PutConfig(ctx, config); err != nil {
		return err
	}
	a.after()
	return nil
}

func TestSwitchIngressRevalidatesReadback(t *testing.T) {
	ctx := context.Background()
	h, cf, o := setup(t, apiToken)
	o.CF = afterPutAPI{CloudflareAPI: o.CF, after: func() {
		cf.SetConfig(strings.Replace(string(cf.Config()), "api.example.com", "removed.example.com", 1))
	}}
	p, err := migrate.Preflight(ctx, h, o, nil)
	if err != nil {
		t.Fatal(err)
	}
	j := migrate.NewJournal("shop", "production", "", time.Now())
	wrote, err := p.SwitchIngress(ctx, h, j)
	if !wrote || err == nil || !strings.Contains(err.Error(), "api.example.com") {
		t.Fatalf("wrote %v error %v", wrote, err)
	}
	if j.IngressSwitched() || j.State == migrate.StateDone {
		t.Fatalf("accepted incomplete ingress: %+v", j)
	}
}
