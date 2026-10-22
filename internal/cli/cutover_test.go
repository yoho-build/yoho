package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/migrate"
	"github.com/yoho-build/yoho/internal/migrate/migratetest"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/secrets"
	"github.com/yoho-build/yoho/internal/ui"
)

const cutoverAPIToken = "cf-api-token-for-cli-tests"

func cutoverFixture(t *testing.T, onFailure string) (*app, *session, *migratetest.Host, *migratetest.Cloudflare, *bytes.Buffer) {
	t.Helper()
	cf := migratetest.NewCloudflare(t, cutoverAPIToken, migratetest.IngressConfig)
	t.Setenv("YOHO_CLOUDFLARE_API", cf.URL)
	h := &migratetest.Host{Containers: []*migratetest.Container{
		{ID: "aaa111", Name: "old-web", Labels: map[string]string{"com.docker.compose.project": "old-stack"}, Running: true},
		{ID: "bbb222", Name: "old-db", Labels: map[string]string{"com.docker.compose.project": "old-stack"}, Running: true,
			Mounts: []migratetest.Mount{{Type: "volume", Name: "old_pgdata", RW: true}}},
	}}
	var out bytes.Buffer
	m := &config.Migrate{
		From: "coolify", OnFailure: onFailure,
		Stop:   config.MigrateStop{ComposeProject: "old-stack"},
		Claim:  config.MigrateClaim{Volumes: []string{"old_pgdata"}},
		Tunnel: &config.MigrateTunnel{APITokenSecret: "CF_API_TOKEN", Hostnames: []string{"app.example.com"}},
	}
	a := &app{
		g: &globals{json: true}, ui: ui.New(&out, ui.JSON, false), destName: "production",
		cfg:  &config.Config{App: "shop", Proxy: config.ProxyConfig{Tunnel: &config.TunnelConfig{TokenSecret: "CF_TUNNEL_TOKEN"}}},
		dest: config.Destination{Runtime: "compose", Migrate: m},
	}
	store := secrets.New(map[string]string{"CF_API_TOKEN": cutoverAPIToken, "CF_TUNNEL_TOKEN": cf.TunnelToken()})
	s := &session{a: a, store: store, hosts: []plan.NamedHost{{Name: "s1", Host: h}}}
	return a, s, h, cf, &out
}

func cutoverPhases(t *testing.T, out string) []string {
	t.Helper()
	var phases []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("invalid NDJSON %q: %v", line, err)
		}
		if ev["event"] == "cutover" {
			phases = append(phases, ev["phase"].(string))
		}
	}
	return phases
}

func startYoho(h *migratetest.Host) {
	h.Containers = append(h.Containers, &migratetest.Container{ID: "fff666", Name: "yoho-shop-production-db-1", Running: true,
		Labels: map[string]string{"com.docker.compose.project": "yoho-shop-production", "yoho.app": "shop", "yoho.destination": "production"},
		Mounts: []migratetest.Mount{{Type: "volume", Name: "old_pgdata", RW: true}}})
}

func TestCutoverJSONEventSequence(t *testing.T) {
	ctx := context.Background()
	a, s, h, cf, out := cutoverFixture(t, "")
	co, err := a.beginCutover(ctx, s, false, false)
	if err != nil || co == nil {
		t.Fatalf("begin: %v", err)
	}
	if err := co.prepare(ctx); err != nil {
		t.Fatalf("prepare: %v\n%s", err, out)
	}
	startYoho(h)
	if err := co.finish(ctx, &release.Release{Version: "v1"}); err != nil {
		t.Fatalf("finish: %v\n%s", err, out)
	}
	if got := strings.Join(cutoverPhases(t, out.String()), ","); got != "preflight,stopped,claims_clear,ingress_switched,done" {
		t.Fatalf("phases %s\n%s", got, out)
	}
	for _, secret := range []string{cutoverAPIToken, cf.TunnelToken()} {
		if strings.Contains(out.String(), secret) {
			t.Fatal("token in output")
		}
	}
	j, _ := migrate.ReadJournal(ctx, h, "shop", "production")
	if j.State != migrate.StateDone || j.Version != "v1" || !j.Deployed {
		t.Fatalf("journal %+v", j)
	}

	// Rerun after done: a normal deploy, nothing stopped.
	ops := len(h.Ops)
	co, err = a.beginCutover(ctx, s, false, false)
	if err != nil || co != nil || len(h.Ops) != ops {
		t.Fatalf("rerun after done: %v %v", co, err)
	}

	// rollback --cutover brings the old stack back.
	out.Reset()
	if err := a.rollbackCutoverOn(ctx, s.hosts[0], s.store, false); err != nil {
		t.Fatalf("rollback: %v\n%s", err, out)
	}
	if got := strings.Join(cutoverPhases(t, out.String()), ","); got != "rolled_back" {
		t.Fatalf("phases %s", got)
	}
	if !h.Container("old-db").Running || h.Container("yoho-shop-production-db-1").Running || cf.Puts != 2 {
		t.Fatalf("rollback state: ops %v puts %d", h.Ops, cf.Puts)
	}
}

func TestCutoverInProgressRefusesDeploy(t *testing.T) {
	ctx := context.Background()
	a, s, h, _, out := cutoverFixture(t, "leave")
	co, _ := a.beginCutover(ctx, s, false, false)
	if err := co.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := co.failed(ctx, &silentError{errors.New("rollout failed")}); err == nil {
		t.Fatal("want error")
	}
	if h.Container("old-db").Running || !strings.Contains(out.String(), "on_failure: leave") {
		t.Fatalf("leave must keep old containers stopped:\n%s", out)
	}
	_, err := a.beginCutover(ctx, s, false, false)
	if err == nil || !strings.Contains(err.Error(), "--cutover-resume") {
		t.Fatalf("err = %v", err)
	}
	co, err = a.beginCutover(ctx, s, true, false)
	if err != nil || co == nil || co.action != migrate.Resume {
		t.Fatalf("resume: %v %v", co, err)
	}
}

func TestCutoverRestartOldOnFailure(t *testing.T) {
	ctx := context.Background()
	a, s, h, cf, out := cutoverFixture(t, "")
	co, _ := a.beginCutover(ctx, s, false, false)
	if err := co.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	startYoho(h)
	if err := co.failed(ctx, &silentError{errors.New("rollout failed")}); err == nil {
		t.Fatal("want error")
	}
	if got := strings.Join(cutoverPhases(t, out.String()), ","); got != "preflight,stopped,claims_clear,rolled_back" {
		t.Fatalf("phases %s", got)
	}
	if !h.Container("old-web").Running || !h.Container("old-db").Running || h.Container("yoho-shop-production-db-1").Running || cf.Puts != 0 {
		t.Fatalf("ops %v puts %d", h.Ops, cf.Puts)
	}
	// The next deploy starts a new cutover.
	if co, err := a.beginCutover(ctx, s, false, false); err != nil || co == nil || co.action != migrate.Start {
		t.Fatalf("after restart_old: %v %v", co, err)
	}
}

func TestCutoverPreflightFailureStopsNothing(t *testing.T) {
	ctx := context.Background()
	a, s, h, cf, out := cutoverFixture(t, "")
	s.store = secrets.New(map[string]string{"CF_API_TOKEN": "expired-api-token", "CF_TUNNEL_TOKEN": cf.TunnelToken()})
	co, err := a.beginCutover(ctx, s, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := co.prepare(ctx); err == nil {
		t.Fatal("want preflight failure")
	}
	if len(h.Ops) != 0 || len(h.Files) != 0 || len(cutoverPhases(t, out.String())) != 0 {
		t.Fatalf("changed something: ops %v files %v", h.Ops, h.Files)
	}
	if !strings.Contains(out.String(), "nothing was stopped") || strings.Contains(out.String(), "expired-api-token") {
		t.Fatalf("output:\n%s", out)
	}
	// A missing API token fails before anything is built.
	s.store = secrets.New(map[string]string{"CF_TUNNEL_TOKEN": cf.TunnelToken()})
	if _, err := a.beginCutover(ctx, s, false, false); err == nil || !strings.Contains(out.String(), "CF_API_TOKEN is missing") {
		t.Fatalf("err %v", err)
	}
}

func TestRollbackCutoverRefusesDrift(t *testing.T) {
	ctx := context.Background()
	a, s, _, cf, out := cutoverFixture(t, "")
	co, _ := a.beginCutover(ctx, s, false, false)
	if err := co.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := co.finish(ctx, nil); err != nil {
		t.Fatal(err)
	}
	cf.SetConfig(strings.Replace(string(cf.Config()), "http://other:80", "http://other:81", 1))
	out.Reset()
	err := a.rollbackCutoverOn(ctx, s.hosts[0], s.store, false)
	if err == nil || !strings.Contains(out.String(), "--force") {
		t.Fatalf("err %v\n%s", err, out)
	}
}

func TestRollbackCutoverArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"v1"}, true},
		{[]string{"--cutover"}, true},
		{[]string{"--cutover", "--force"}, true},
		{[]string{"--cutover", "v1"}, false},
		{[]string{"--force", "v1"}, false},
		{nil, false},
	} {
		c := rollbackCmd(&globals{})
		if err := c.ParseFlags(tc.args); err != nil {
			t.Fatal(err)
		}
		if err := c.Args(c, c.Flags().Args()); (err == nil) != tc.ok {
			t.Errorf("%v: %v", tc.args, err)
		}
	}
}

func TestPlanCutover(t *testing.T) {
	ctx := context.Background()
	a, s, h, _, _ := cutoverFixture(t, "")
	ch, err := a.planCutover(ctx, s.hosts[0])
	if err != nil || len(ch) != 1 || ch[0].Action != plan.ActionCreate || !ch[0].Downtime ||
		ch[0].Reasons[0] != "stop project old-stack, check 1 claim(s), switch app.example.com → http://yoho-proxy:80" {
		t.Fatalf("%+v %v", ch, err)
	}
	j := migrate.NewJournal("shop", "production", "", time.Now())
	j.State = migrate.StateDone
	_ = j.Save(ctx, h)
	if ch, err := a.planCutover(ctx, s.hosts[0]); err != nil || len(ch) != 0 {
		t.Fatalf("done: %+v %v", ch, err)
	}
}

func TestCutoverLockSerializesAndRereadsJournal(t *testing.T) {
	ctx := context.Background()
	a, s, h, _, _ := cutoverFixture(t, "")
	first, _ := a.beginCutover(ctx, s, false, false)
	second, _ := a.beginCutover(ctx, s, false, false)
	unlock, err := first.lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.lock(ctx); err == nil {
		t.Fatal("second cutover acquired held lock")
	} else {
		var locked *deploy.LockedError
		if !errors.As(err, &locked) || !strings.Contains(err.Error(), "if the lock is stale") {
			t.Fatalf("error %v", err)
		}
	}
	if _, err := deploy.AcquireLock(ctx, h, "shop", "production", deploy.LockInfo{Command: "rollback --cutover"}); err == nil {
		t.Fatal("rollback acquired held lock")
	}
	if err := first.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := unlock(ctx); err != nil {
		t.Fatal(err)
	}
	// second began before first saved the journal; it must see the new state.
	if _, err := second.lock(ctx); err == nil || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("stale journal accepted: %v", err)
	}
	if h.Locked {
		t.Fatal("journal rejection leaked lock")
	}
}

func TestCutoverResumeAfterIngressOnlyFinishes(t *testing.T) {
	ctx := context.Background()
	a, s, h, cf, _ := cutoverFixture(t, "")
	co, _ := a.beginCutover(ctx, s, false, false)
	if err := co.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := co.recordDeploy(ctx, &release.Release{Version: "v1"}); err != nil {
		t.Fatal(err)
	}
	co.j.TunnelReconciled = true
	if _, err := co.plan.SwitchIngress(ctx, h, co.j); err != nil {
		t.Fatal(err)
	}
	// Saved ingress but no done marker, as after an interrupted final write.
	co, err := a.beginCutover(ctx, s, true, false)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := co.lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(ctx)
	if !co.deployed("v1") || co.deployed("v2") || !co.j.TunnelReconciled {
		t.Fatalf("resume phases %+v", co.j)
	}
	hooks := 0
	co.hook = func(context.Context, string, map[string]string) error { hooks++; return nil }
	h.Scripts = nil
	if err := co.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if hooks != 0 {
		t.Fatal("repeated pre-cutover Hook")
	}
	if err := co.finish(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if cf.Puts != 1 || co.j.State != migrate.StateDone {
		t.Fatalf("puts %d state %s", cf.Puts, co.j.State)
	}
	for _, script := range h.Scripts {
		if strings.Contains(script, "docker stop") {
			t.Fatal("repeated stop")
		}
	}
}

func TestCutoverRestartOldRefusesConflictingWriter(t *testing.T) {
	ctx := context.Background()
	a, s, h, _, out := cutoverFixture(t, "")
	co, _ := a.beginCutover(ctx, s, false, false)
	if err := co.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	startYoho(h)
	h.Containers = append(h.Containers, &migratetest.Container{ID: "stray", Name: "stray-writer", Running: true, Mounts: []migratetest.Mount{{Type: "volume", Name: "old_pgdata", RW: true}}})
	err := co.failed(ctx, errors.New("rollout failed"))
	if err == nil || !strings.Contains(err.Error(), "stray-writer") {
		t.Fatalf("missing recovery error: %v", err)
	}
	if h.Container("old-db").Running || h.Container("yoho-shop-production-db-1").Running {
		t.Fatalf("unsafe restart: %v", h.Ops)
	}
	if !strings.Contains(out.String(), "stray-writer") {
		t.Fatal("missing conflict diagnostic")
	}
}

func TestCutoverPostHookFailureResumesOnlyHook(t *testing.T) {
	for _, deployed := range []bool{true, false} {
		t.Run(fmt.Sprintf("deployed=%v", deployed), func(t *testing.T) {
			ctx := context.Background()
			a, s, h, cf, out := cutoverFixture(t, "")
			co, err := a.beginCutover(ctx, s, false, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := co.prepare(ctx); err != nil {
				t.Fatal(err)
			}
			if deployed {
				if err := co.recordDeploy(ctx, &release.Release{Version: "v1"}); err != nil {
					t.Fatal(err)
				}
			}
			co.j.TunnelReconciled = true
			failed := false
			hook := func(_ context.Context, name string, _ map[string]string) error {
				if name != "post-cutover" {
					t.Fatalf("repeated Hook %s", name)
				}
				saved, err := migrate.ReadJournal(ctx, h, "shop", "production")
				if err != nil || saved.State != migrate.StateStarted || !saved.PostCutoverPending {
					t.Fatalf("premature completion: %+v %v", saved, err)
				}
				if !failed {
					failed = true
					return errors.New("post Hook failed")
				}
				return nil
			}
			co.hook = hook
			err = co.finish(ctx, nil)
			if err == nil || !strings.Contains(err.Error(), "--cutover-resume") {
				t.Fatalf("error %v", err)
			}
			if strings.Contains(strings.Join(cutoverPhases(t, out.String()), ","), "done") {
				t.Fatal("reported done before Hook succeeded")
			}
			// Resume must not even inspect ingress or Docker while retrying only
			// the Hook. An expired API token would make another preflight fail.
			s.store = secrets.New(map[string]string{"CF_API_TOKEN": "expired", "CF_TUNNEL_TOKEN": cf.TunnelToken()})
			co, err = a.beginCutover(ctx, s, true, false)
			if err != nil {
				t.Fatal(err)
			}
			co.hook = hook
			if !co.deployed("v1") {
				t.Fatal("resume would repeat rollout")
			}
			h.Scripts = nil
			requests := len(cf.Headers)
			if err := co.prepare(ctx); err != nil {
				t.Fatal(err)
			}
			if err := co.finish(ctx, nil); err != nil {
				t.Fatal(err)
			}
			saved, err := migrate.ReadJournal(ctx, h, "shop", "production")
			if err != nil || saved.State != migrate.StateDone || saved.PostCutoverPending || len(h.Scripts) != 0 || len(cf.Headers) != requests || cf.Puts != 1 {
				t.Fatalf("resume repeated completed work: journal %+v scripts %v requests %d/%d puts %d err %v", saved, h.Scripts, len(cf.Headers), requests, cf.Puts, err)
			}
		})
	}
}

func TestCutoverCreatesHooksWithoutRuntimeRollout(t *testing.T) {
	ctx := context.Background()
	a, s, _, _, _ := cutoverFixture(t, "")
	dir := t.TempDir()
	log := filepath.Join(dir, "hooks.log")
	t.Setenv("CUTOVER_HOOK_LOG", log)
	a.cfg.Hooks.Path = dir
	s.ver = "v1"
	// No runtime diff means no buildAndShip or rollout provides a Hook.
	s.hook = nil
	for _, name := range []string{"pre-cutover", "post-cutover"} {
		script := "#!/bin/sh\nset -eu\nprintf '%s|%s|%s|%s\\n' '" + name + "' \"$YOHO_CUTOVER\" \"$YOHO_VERSION\" \"$YOHO_DESTINATION\" >> \"$CUTOVER_HOOK_LOG\"\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	co, err := a.beginCutover(ctx, s, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := co.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	if err := co.finish(ctx, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "pre-cutover|" + co.j.ID + "|v1|production\npost-cutover|" + co.j.ID + "|v1|production\n"
	if string(data) != want {
		t.Fatalf("Hooks %q, want %q", data, want)
	}
}

func TestCutoverRestartOldRefusesPortsHeldBySharedProxy(t *testing.T) {
	ctx := context.Background()
	a, s, h, _, _ := cutoverFixture(t, "")
	co, err := a.beginCutover(ctx, s, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := co.prepare(ctx); err != nil {
		t.Fatal(err)
	}
	h.Container("old-web").PortBindings = map[string][]migratetest.PortBinding{"3000/tcp": {{HostPort: "80"}}}
	h.Containers = append(h.Containers, &migratetest.Container{ID: "proxy", Name: "yoho-proxy", Running: true, PortBindings: map[string][]migratetest.PortBinding{"80/tcp": {{HostPort: "80"}}}})
	err = co.failed(ctx, errors.New("rollout failed"))
	if err == nil || !strings.Contains(err.Error(), "old-web needs host port 80/tcp held by yoho-proxy") {
		t.Fatalf("error %v", err)
	}
	if h.Container("old-db").Running || h.Container("old-web").Running || !h.Container("yoho-proxy").Running || co.j.State != migrate.StateStarted {
		t.Fatalf("unsafe rollback: ops %v state %s", h.Ops, co.j.State)
	}
}

func TestMigratePlanDisplaysRollbackPortWarning(t *testing.T) {
	a, s, h, _, out := cutoverFixture(t, "")
	h.Container("old-web").PortBindings = map[string][]migratetest.PortBinding{"3000/tcp": {{HostPort: "80"}}}
	if err := a.migratePlanOn(context.Background(), s.hosts[0], s.store); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "old-web publishes host port 80/tcp") || !strings.Contains(out.String(), "yoho-proxy") {
		t.Fatalf("missing warning: %s", out)
	}
}
