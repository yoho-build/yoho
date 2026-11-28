package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func TestStopScriptIsExact(t *testing.T) {
	want := `set -eu
ids=$(docker ps -aq --filter 'label=com.docker.compose.project=yoho-shop-production' --filter 'label=com.docker.compose.service=db' --filter 'label=yoho.app=shop' --filter 'label=yoho.destination=production' --filter label=com.docker.compose.oneoff=False)
[ -n "$ids" ] || exit 0
docker stop $ids >/dev/null
for id in $ids; do printf '%s %s\n' "$id" "$(docker inspect -f '{{.State.ExitCode}}' "$id")"; done
docker rm $ids >/dev/null
`
	if got := StopScript("shop", "production", "db"); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	if strings.Contains(want, "volume") || strings.Contains(want, "down") {
		t.Fatal("stop must never touch volumes")
	}
}

func TestStoppedOverrideOnlyReleaseServices(t *testing.T) {
	s := Stopped{"db": {By: "alice"}, "gone": {By: "bob"}}
	plans := []servicePlan{{Name: "web", Replicas: 1}, {Name: "db", Replicas: 1}}
	got := string(stoppedOverride(s, plans))
	want := "services:\n    db:\n        scale: 0\n        deploy:\n            replicas: 0\n"
	if got != want {
		t.Fatalf("got\n%q\nwant\n%q", got, want)
	}
	if stoppedOverride(Stopped{"gone": {}}, plans) != nil || stoppedOverride(nil, plans) != nil {
		t.Fatal("override for Services outside the Release")
	}
	adj := applyStopped([]servicePlan{{Name: "db", Replicas: 1, ReleaseCommand: []string{"x"}}, {Name: "web", Replicas: 2}}, map[string]bool{"db": true})
	if adj[0].Replicas != 0 || adj[0].ReleaseCommand != nil || adj[1].Replicas != 2 {
		t.Fatal(adj)
	}
}

func TestComposeCLIOverrides(t *testing.T) {
	cc := composeCLI{project: "p", dir: "/d", file: "/d/r/compose.yaml", overrides: []string{"/d/stopped.compose.yaml"}}
	if got, want := cc.cmd("up", "-d"), "docker compose '-p' 'p' '--project-directory' '/d' '-f' '/d/r/compose.yaml' '-f' '/d/stopped.compose.yaml' 'up' '-d'"; got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

// stopRelease deploys v1 so a current Release with plan.json exists.
func stopRelease(t *testing.T) (*dockerHost, string) {
	t.Helper()
	root := withRoot(t)
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	if _, err := (Compose{}).Deploy(context.Background(), testDeploy(t, h, io.Discard)); err != nil {
		t.Fatalf("deploy: %v\n%s", err, h.all())
	}
	h.scripts = nil
	return h, filepath.Join(root, "apps", "shop", "production")
}

func TestStopServiceRecordsFirstAndIsIdempotent(t *testing.T) {
	h, appDir := stopRelease(t)
	ctx := context.Background()
	var recordedBeforeStop bool
	h.respond = func(s string) (string, error) {
		if strings.Contains(s, "docker stop $ids") {
			_, err := os.Stat(filepath.Join(appDir, StoppedFile))
			recordedBeforeStop = err == nil
			return "c1 0\nc2 0", nil
		}
		return "", nil
	}
	at := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	res, err := StopService(ctx, h, "shop", "production", "db", StoppedEntry{By: "alice", At: at, Reason: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	if !recordedBeforeStop {
		t.Fatal("stopped.json must be written before the containers stop")
	}
	if !res.Changed || len(res.Containers) != 2 || res.ExitCodes["c2"] != 0 || !res.Clean() {
		t.Fatalf("%+v", res)
	}
	info, err := os.Stat(filepath.Join(appDir, StoppedFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("stopped.json: %v %v", info, err)
	}
	s, err := ReadStopped(ctx, h, "shop", "production")
	if err != nil || s["db"].By != "alice" || s["db"].Reason != "maintenance" || !s["db"].At.Equal(at) {
		t.Fatalf("%+v %v", s, err)
	}
	override, err := os.ReadFile(filepath.Join(appDir, StoppedOverrideFile))
	if err != nil || !strings.Contains(string(override), "db:") || strings.Contains(string(override), "web") {
		t.Fatalf("override %q %v", override, err)
	}
	if got := h.all(); !strings.Contains(got, StopScript("shop", "production", "db")) {
		t.Fatalf("stop script not run:\n%s", got)
	}

	// Repeat: nothing left to stop, the record stays as first written.
	h.respond = func(string) (string, error) { return "", nil }
	res, err = StopService(ctx, h, "shop", "production", "db", StoppedEntry{By: "bob", At: at.Add(time.Hour)})
	if err != nil || res.Changed || len(res.Containers) != 0 {
		t.Fatalf("repeat %+v %v", res, err)
	}
	if s, _ = ReadStopped(ctx, h, "shop", "production"); s["db"].By != "alice" {
		t.Fatalf("record rewritten: %+v", s)
	}

	// An unclean shutdown is reported, not hidden.
	h.respond = func(s string) (string, error) {
		if strings.Contains(s, "docker stop $ids") {
			return "c3 137", nil
		}
		return "", nil
	}
	if res, err = StopService(ctx, h, "shop", "production", "db", StoppedEntry{By: "alice"}); err != nil || res.Clean() || res.ExitCodes["c3"] != 137 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestDeployAndRollbackKeepStoppedServiceDown(t *testing.T) {
	h, appDir := stopRelease(t)
	ctx := context.Background()
	if _, err := StopService(ctx, h, "shop", "production", "db", StoppedEntry{By: "alice"}); err != nil {
		t.Fatal(err)
	}
	h.respond = respond(nil)
	h.scripts = nil
	d := testDeploy(t, h, io.Discard)
	d.Version = "v2"
	if _, err := (Compose{}).Deploy(ctx, d); err != nil {
		t.Fatalf("%v\n%s", err, h.all())
	}
	override := filepath.Join(appDir, StoppedOverrideFile)
	all := h.all()
	for _, s := range h.scripts {
		if strings.Contains(s, "docker compose ") && !strings.Contains(s, "'-f' '"+override+"'") {
			t.Errorf("compose without the stopped override:\n%s", s)
		}
	}
	if strings.Contains(all, "'up' '-d' '--no-recreate' 'db'") {
		t.Errorf("stopped db started as a release prerequisite:\n%s", all)
	}
	if strings.Contains(all, "Service db: expected") {
		t.Errorf("waited for a stopped Service:\n%s", all)
	}
	if !strings.Contains(all, "'run' '--rm' '--no-deps' '-T' 'web'") {
		t.Errorf("web's release command must still run:\n%s", all)
	}
	// The Release record keeps the declared replicas.
	b, err := os.ReadFile(filepath.Join(appDir, "releases", "v2", "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plans []servicePlan
	if err := json.Unmarshal(b, &plans); err != nil {
		t.Fatal(err)
	}
	for _, sp := range plans {
		if sp.Name == "db" && sp.Replicas != 1 {
			t.Errorf("plan.json changed: %+v", sp)
		}
	}

	h.scripts = nil
	if _, err := (Compose{}).Rollback(ctx, testDeploy(t, h, io.Discard), "v1"); err != nil {
		t.Fatalf("rollback: %v\n%s", err, h.all())
	}
	for _, s := range h.scripts {
		if strings.Contains(s, "docker compose ") && !strings.Contains(s, "'-f' '"+override+"'") {
			t.Errorf("rollback compose without the stopped override:\n%s", s)
		}
	}
}

func TestStartServiceClearsRecordAndWaits(t *testing.T) {
	h, appDir := stopRelease(t)
	ctx := context.Background()
	if _, err := StopService(ctx, h, "shop", "production", "db", StoppedEntry{By: "alice"}); err != nil {
		t.Fatal(err)
	}
	h.scripts = nil
	h.respond = func(s string) (string, error) {
		if strings.Contains(s, "'ps' '--all' '-q' 'db'") {
			return "db1", nil
		}
		if strings.Contains(s, "docker inspect") && strings.Contains(s, "State.Status") {
			return "running healthy 0 0", nil
		}
		return "", nil
	}
	d := &plan.Deploy{App: "shop", Destination: "production", Servers: []plan.NamedHost{{Name: "s1", Host: h}}, Out: io.Discard}
	changed, err := StartService(ctx, d, "db")
	if err != nil {
		t.Fatalf("%v\n%s", err, h.all())
	}
	if !changed {
		t.Fatal("start of a stopped Service must report a change")
	}
	if _, err := os.Stat(filepath.Join(appDir, StoppedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty stopped.json must be removed")
	}
	if _, err := os.Stat(filepath.Join(appDir, StoppedOverrideFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("empty override must be removed")
	}
	all := h.all()
	if !strings.Contains(all, filepath.Join(appDir, "releases", "v1", "compose.yaml")+"' 'up' '-d' '--no-deps' 'db'") {
		t.Fatalf("start command:\n%s", all)
	}
	if !strings.Contains(all, "Service db: expected 1 container(s)") {
		t.Fatalf("no readiness wait:\n%s", all)
	}
	if changed, err = StartService(ctx, d, "db"); err != nil || changed {
		t.Fatalf("repeat start: %v %v", changed, err)
	}
	if _, err := StartService(ctx, d, "nope"); err == nil {
		t.Fatal("unknown Service started")
	}
}

func TestDiffShowsStoppedService(t *testing.T) {
	h, st, _ := deployed(t)
	ctx := context.Background()
	at := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	if err := writeStopped(ctx, h, "shop", "production", Stopped{"db": {By: "alice", At: at}}); err != nil {
		t.Fatal(err)
	}
	// db still has a running container: apply removes it.
	cs, err := Compose{}.Diff(ctx, testDeploy(t, h, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	db := byKey(cs)["service db"]
	if db.Action != plan.ActionUpdate || !db.Stopped || db.Reasons[0] != "replicas 1 → 0 (stopped)" || !strings.Contains(db.Reasons[1], "alice, 2026-11-27 10:00 UTC") {
		t.Fatalf("%+v", db)
	}
	// Once its containers are gone, the stopped Service is a noop.
	var all []map[string]any
	_ = json.Unmarshal([]byte(st.containers), &all)
	var kept []map[string]any
	for _, c := range all {
		if c["Config"].(map[string]any)["Labels"].(map[string]any)["com.docker.compose.service"] != "db" {
			kept = append(kept, c)
		}
	}
	b, _ := json.Marshal(kept)
	st.containers = string(b)
	if cs, err = (Compose{}).Diff(ctx, testDeploy(t, h, io.Discard)); err != nil {
		t.Fatal(err)
	}
	db = byKey(cs)["service db"]
	if db.Action != plan.ActionNoop || !db.Stopped || !strings.Contains(db.Reasons[0], "`yoho app start db` runs it") {
		t.Fatalf("%+v", db)
	}
	for _, c := range cs {
		if c.Action != plan.ActionNoop {
			t.Errorf("unexpected change %+v", c)
		}
	}
	if strings.Contains(h.all(), "stopped.compose.yaml") {
		t.Fatal("Diff must not write the override")
	}
	// A config change of the stopped Service is still a change: apply
	// records the new Release and the Service stays stopped.
	d := testDeploy(t, h, io.Discard)
	db.Reasons = nil
	svc := d.Project.Services["db"]
	svc.Image = "postgres:18"
	svc.Environment = map[string]*string{"POSTGRES_DB": ptr("other")}
	d.Project.Services["db"] = svc
	if cs, err = (Compose{}).Diff(ctx, d); err != nil {
		t.Fatal(err)
	}
	db = byKey(cs)["service db"]
	if db.Action != plan.ActionUpdate || !db.Stopped || !strings.Contains(strings.Join(db.Reasons, "; "), "image postgres:") || !strings.Contains(strings.Join(db.Reasons, "; "), "config changed: environment") || !strings.Contains(strings.Join(db.Reasons, "; "), "stays stopped") {
		t.Fatalf("%+v", db)
	}
}

func ptr(s string) *string { return &s }

// releaseHost answers readlink with releases/v1 and keeps files in memory.
type releaseHost struct{ *rolesHost }

func (h releaseHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if strings.Contains(c.Script, "readlink") {
		return "releases/v1", nil
	}
	return h.rolesHost.Output(ctx, c)
}
func (h releaseHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := h.Output(ctx, c)
	return err
}

func TestRolesDiffPointsProviderSwitchAtDbPromote(t *testing.T) {
	d := rolesTestDeploy()
	d.Ext["standby"] = config.ServiceExt{Stateful: true}
	base := d.Servers[0].Host.(*rolesHost)
	old := release.Release{Version: "v1", Roles: d.Roles.Roles, Connections: map[string]config.Connection{"primary": {Service: "db", Port: 5432, Consumers: []string{"web", "standby"}}}}
	b, _ := json.Marshal(old)
	base.files[path.Join(release.Dir(d.App, d.Destination, "v1"), "release.json")] = b
	d.Servers[0].Host = releaseHost{base}
	d.Servers = d.Servers[:1]
	d.Roles.Roles["replica"] = config.Role{Services: []string{"standby"}, Servers: []string{"a"}}
	d.Roles.Connections = map[string]config.Connection{"primary": {Service: "standby", Port: 5432, Consumers: []string{"web", "db"}}}
	in := []plan.Change{
		{Kind: "service", Name: "standby", Server: "a", Action: plan.ActionReplace},
		{Kind: "firewall", Name: "shop/production", Server: "a", Action: plan.ActionUpdate},
	}
	out, err := annotateProviderSwitch(context.Background(), d, in)
	if err != nil {
		t.Fatal(err)
	}
	want := "connection primary: provider db → standby; for a database switchover use `yoho db promote primary`"
	if len(out[0].Reasons) != 1 || out[0].Reasons[0] != want || len(out[1].Reasons) != 0 {
		t.Fatalf("%+v", out)
	}
	// Not both Stateful: a plain Connection switch, no hint.
	d.Ext["standby"] = config.ServiceExt{}
	in[0].Reasons = nil
	if out, _ = annotateProviderSwitch(context.Background(), d, in); len(out[0].Reasons) != 0 {
		t.Fatalf("%+v", out)
	}
}

func TestStoppedProviderExplainsConnectionFailure(t *testing.T) {
	h := &rolesHost{name: "a", files: map[string][]byte{}}
	ctx := context.Background()
	d := &plan.Deploy{App: "shop", Destination: "production"}
	cause := errors.New("connection db from b: not reachable")
	if got := stoppedOr(ctx, h, d, "primary", "db", "a", cause); got != cause {
		t.Fatal(got)
	}
	if err := writeStopped(ctx, h, "shop", "production", Stopped{"db": {By: "alice"}}); err != nil {
		t.Fatal(err)
	}
	got := stoppedOr(ctx, h, d, "primary", "db", "a", cause)
	if !errors.Is(got, cause) || !strings.HasPrefix(got.Error(), "connection primary provider db is stopped on a (`yoho app stop`); start it or switch the Connection") {
		t.Fatal(got)
	}
}

// A failed compose up keeps the Service marked stopped, so the rerun still
// reports the start as a change; the up itself runs without the Service in
// the override.
func TestStartServiceFailedUpStaysStopped(t *testing.T) {
	h, appDir := stopRelease(t)
	ctx := context.Background()
	if _, err := StopService(ctx, h, "shop", "production", "db", StoppedEntry{By: "alice"}); err != nil {
		t.Fatal(err)
	}
	failUp := true
	overrideAtUp := ""
	h.respond = func(s string) (string, error) {
		if strings.Contains(s, "'up' '-d' '--no-deps' 'db'") {
			b, _ := os.ReadFile(filepath.Join(appDir, StoppedOverrideFile))
			overrideAtUp = string(b)
			if failUp {
				return "", errors.New("compose up failed")
			}
		}
		if strings.Contains(s, "'ps' '--all' '-q' 'db'") {
			return "db1", nil
		}
		if strings.Contains(s, "docker inspect") && strings.Contains(s, "State.Status") {
			return "running healthy 0 0", nil
		}
		return "", nil
	}
	d := &plan.Deploy{App: "shop", Destination: "production", Servers: []plan.NamedHost{{Name: "s1", Host: h}}, Out: io.Discard}
	_, err := StartService(ctx, d, "db")
	var se *StartError
	if !errors.As(err, &se) || !strings.Contains(se.Hint(), "stays marked stopped") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(overrideAtUp, "db:") {
		t.Fatalf("compose up ran with db scaled to 0:\n%s", overrideAtUp)
	}
	if s, _ := ReadStopped(ctx, h, "shop", "production"); s["db"].By != "alice" {
		t.Fatalf("failed up cleared the stopped record: %+v", s)
	}
	failUp = false
	if changed, err := StartService(ctx, d, "db"); err != nil || !changed {
		t.Fatalf("rerun after a failed up: changed=%v %v", changed, err)
	}
	if s, _ := ReadStopped(ctx, h, "shop", "production"); len(s) != 0 {
		t.Fatalf("record kept after start: %+v", s)
	}
}

// app start refuses a compose project another App Destination's containers use.
func TestStartServiceChecksOwnership(t *testing.T) {
	h, _ := stopRelease(t)
	ctx := context.Background()
	if _, err := StopService(ctx, h, "shop", "production", "db", StoppedEntry{By: "alice"}); err != nil {
		t.Fatal(err)
	}
	h.respond = func(s string) (string, error) {
		if strings.Contains(s, "docker ps -a --filter") && strings.Contains(s, LabelApp) {
			return "shop-production|x\n", nil
		}
		return "", nil
	}
	d := &plan.Deploy{App: "shop", Destination: "production", Servers: []plan.NamedHost{{Name: "s1", Host: h}}, Out: io.Discard}
	_, err := StartService(ctx, d, "db")
	var oe *release.OwnershipError
	if !errors.As(err, &oe) {
		t.Fatalf("%v", err)
	}
	if strings.Contains(h.all(), "'up' '-d' '--no-deps' 'db'") {
		t.Fatal("compose up ran for a foreign project")
	}
}

// A stopped same-Server provider fails the readiness check with the
// stopped message instead of being skipped (Deploy and Rollback share it).
func TestConnectionReadyStoppedSameServerProvider(t *testing.T) {
	h := &rolesHost{name: "a", files: map[string][]byte{}}
	ctx := context.Background()
	d := &plan.Deploy{App: "shop", Destination: "production"}
	hostOf := func(string) remote.Host { return h }
	c := config.Connection{Service: "db", Port: 5432}
	if err := connectionReady(ctx, d, h, hostOf, "primary", c, []string{"a"}, "a"); err != nil {
		t.Fatal(err)
	}
	if err := writeStopped(ctx, h, "shop", "production", Stopped{"db": {By: "alice"}}); err != nil {
		t.Fatal(err)
	}
	err := connectionReady(ctx, d, h, hostOf, "primary", c, []string{"a"}, "a")
	if err == nil || err.Error() != "connection primary provider db is stopped on a (`yoho app stop`); start it or switch the Connection" {
		t.Fatal(err)
	}
}

// StopOptions.AfterStop runs between docker stop and docker rm and its
// output comes back per container; plain stop has no such step.
func TestStopServiceAfterStop(t *testing.T) {
	h, _ := stopRelease(t)
	ctx := context.Background()
	var script string
	h.respond = func(s string) (string, error) {
		if strings.Contains(s, "docker stop $ids") {
			script = s
			return "c1 0\nc1| Database cluster state:               shut down\nc1| Latest checkpoint location:           0/3000060", nil
		}
		return "", nil
	}
	res, err := StopService(ctx, h, "shop", "production", "db", StoppedEntry{By: "alice"}, StopOptions{AfterStop: "pg_controldata_here"})
	if err != nil {
		t.Fatal(err)
	}
	stop, check, rm := strings.Index(script, "docker stop $ids"), strings.Index(script, "pg_controldata_here"), strings.Index(script, "docker rm $ids")
	if stop < 0 || check < stop || rm < check {
		t.Fatalf("after-stop must run between stop and rm:\n%s", script)
	}
	if len(res.Containers) != 1 || res.ExitCodes["c1"] != 0 || res.AfterStop["c1"] != "Database cluster state:               shut down\nLatest checkpoint location:           0/3000060" {
		t.Fatalf("%+v", res)
	}
	if strings.Contains(StopScript("shop", "production", "db"), "out=$(") {
		t.Fatal("plain stop runs an after-stop step")
	}
}

// G11: a Release that proxies a Service `yoho app stop` keeps down is
// refused by deploy, plan/apply and rollback before anything changes.
func TestStoppedServiceProxiedInTargetReleaseRefused(t *testing.T) {
	h, _ := stopRelease(t)
	ctx := context.Background()
	if err := writeStopped(ctx, h, "shop", "production", Stopped{"web": {By: "alice"}}); err != nil {
		t.Fatal(err)
	}
	want := "run `yoho app start web` before"
	d := testDeploy(t, h, io.Discard)
	d.Version = "v2"
	if _, err := (Compose{}).Deploy(ctx, d); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("deploy: %v", err)
	}
	if strings.Contains(h.all(), "'up'") {
		t.Fatal("deploy changed containers before refusing")
	}
	if _, err := (Compose{}).Diff(ctx, testDeploy(t, h, io.Discard)); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("plan: %v", err)
	}
	if _, err := (Compose{}).Rollback(ctx, testDeploy(t, h, io.Discard), "v1"); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("rollback: %v", err)
	}
}

// G12: `app start` of a Service no longer in the current Release clears
// its orphaned stopped entry.
func TestStartServiceClearsOrphanedEntry(t *testing.T) {
	h, appDir := stopRelease(t)
	ctx := context.Background()
	if err := writeStopped(ctx, h, "shop", "production", Stopped{"gone": {By: "alice"}, "db": {By: "bob"}}); err != nil {
		t.Fatal(err)
	}
	h.respond = func(string) (string, error) { return "", nil }
	d := &plan.Deploy{App: "shop", Destination: "production", Servers: []plan.NamedHost{{Name: "s1", Host: h}}, Out: io.Discard}
	changed, err := StartService(ctx, d, "gone")
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	s, _ := ReadStopped(ctx, h, "shop", "production")
	if _, ok := s["gone"]; ok || s["db"].By != "bob" {
		t.Fatalf("%+v", s)
	}
	if strings.Contains(h.all(), "'up'") {
		t.Fatal("started something for an orphaned entry")
	}
	if b, _ := os.ReadFile(filepath.Join(appDir, StoppedOverrideFile)); !strings.Contains(string(b), "db:") {
		t.Fatalf("override lost db: %s", b)
	}
	if _, err := StartService(ctx, d, "gone"); err == nil {
		t.Fatal("unknown Service without an entry accepted")
	}
}

// G13: a start that fails after compose up created containers removes them
// again and keeps the Service marked stopped, so no restart policy revives
// a Service marked stopped.
func TestStartServiceFailureRefences(t *testing.T) {
	for _, phase := range []string{"up", "readiness"} {
		t.Run(phase, func(t *testing.T) {
			h, appDir := stopRelease(t)
			ctx := context.Background()
			if _, err := StopService(ctx, h, "shop", "production", "db", StoppedEntry{By: "alice"}); err != nil {
				t.Fatal(err)
			}
			h.scripts = nil
			stopped := false
			h.respond = func(s string) (string, error) {
				switch {
				case phase == "up" && strings.Contains(s, "'up' '-d' '--no-deps' 'db'"):
					return "", errors.New("compose up failed")
				case phase == "readiness" && strings.Contains(s, "expected 1 container(s)"):
					return "", errors.New("not ready")
				case strings.Contains(s, "docker stop $ids"):
					stopped = true
					return "db9 137", nil
				}
				return "", nil
			}
			d := &plan.Deploy{App: "shop", Destination: "production", Servers: []plan.NamedHost{{Name: "s1", Host: h}}, Out: io.Discard}
			_, err := StartService(ctx, d, "db")
			var se *StartError
			if !errors.As(err, &se) || !se.StillStopped || !strings.Contains(se.Hint(), "stays marked stopped") {
				t.Fatalf("%v", err)
			}
			if !stopped || !strings.Contains(h.all(), StopScript("shop", "production", "db")) {
				t.Fatalf("containers not removed:\n%s", h.all())
			}
			if s, _ := ReadStopped(ctx, h, "shop", "production"); s["db"].By != "alice" {
				t.Fatalf("stopped record not kept: %+v", s)
			}
			if b, _ := os.ReadFile(filepath.Join(appDir, StoppedOverrideFile)); !strings.Contains(string(b), "db:") {
				t.Fatalf("override not restored: %s", b)
			}
		})
	}
}

// G14: StopService revalidates under the lock and surfaces errors reading
// the current Release.
func TestStopServiceRevalidatesTarget(t *testing.T) {
	h, appDir := stopRelease(t)
	ctx := context.Background()
	if _, err := StopService(ctx, h, "shop", "production", "web", StoppedEntry{By: "alice"}); err == nil || !strings.Contains(err.Error(), "proxied") {
		t.Fatal(err)
	}
	if _, err := StopService(ctx, h, "shop", "production", "nope", StoppedEntry{By: "alice"}); err == nil || !strings.Contains(err.Error(), "not in the current Release") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(appDir, StoppedFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("refused stop wrote stopped.json")
	}
	os.Remove(filepath.Join(appDir, "releases", "v1", "plan.json"))
	if _, err := StopService(ctx, h, "shop", "production", "db", StoppedEntry{By: "alice"}); err == nil {
		t.Fatal("missing plan.json skipped")
	}
}

// G15: running containers exclude one-offs, like the stop script.
func TestRunningContainersExcludesOneOffs(t *testing.T) {
	h := &dockerHost{local: &remote.Local{}, respond: func(string) (string, error) { return "", nil }}
	if _, err := RunningContainers(context.Background(), h, "shop", "production", "db"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.all(), "--filter label=com.docker.compose.oneoff=False") {
		t.Fatal(h.all())
	}
}
