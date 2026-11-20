package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/dash"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/operation"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/schedule"
	"github.com/yoho-build/yoho/internal/ui"
)

func reviewApp(t *testing.T) (*app, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{App: "shop", Servers: map[string]config.Server{"a": {SSH: "root@app.example", Root: dir}, "b": {SSH: "root@data.example", Root: dir}}, Destinations: map[string]config.Destination{"production": {Servers: []string{"a", "b"}, Roles: map[string]config.Role{"app": {Services: []string{"web"}, Servers: []string{"a"}}, "data": {Services: []string{"db"}, Servers: []string{"b"}}}}}, Backups: config.BackupsConfig{Targets: map[string]config.BackupTarget{"local": {Type: "archive", Repository: "/tmp/backups"}}, Jobs: map[string]config.BackupJob{"data": {Destination: "production", Target: "local", Services: []string{"db"}}}}}
	data, _ := json.Marshal(cfg)
	file := filepath.Join(dir, "yoho.json")
	_ = os.WriteFile(file, data, 0600)
	_ = os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte("services:\n  web:\n    image: nginx:alpine\n  db:\n    image: postgres:17\n    volumes: [data:/data]\n    x-yoho:\n      stateful: true\n      backup: {volumes: [data]}\nvolumes: {data: {}}\n"), 0600)
	a := &app{cfg: cfg, dest: cfg.Destinations["production"], destName: "production", dir: dir, path: file, g: &globals{configPath: file}, ui: ui.New(io.Discard, ui.Human, false)}
	return a, file
}
func TestReviewScopedOverrideRolesRejected(t *testing.T) {
	_, file := reviewApp(t)
	t.Setenv("YOHO_ACCESS", "scoped")
	g := &globals{configPath: file}
	c := &cobra.Command{Use: "deploy"}
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	if _, e := g.load(c); e == nil || !strings.Contains(e.Error(), "operator-only") {
		t.Fatal(e)
	}
}
func TestReviewServiceCommandsAndRemoveUsePlacement(t *testing.T) {
	a, _ := reviewApp(t)
	if server, e := a.serviceServer("db", ""); e != nil || server != "b" {
		t.Fatal(server, e)
	}
	if _, e := a.serviceServer("db", "a"); e == nil {
		t.Fatal("--server outside placement accepted")
	}
	hosts := []plan.NamedHost{{Name: "a"}, {Name: "b"}}
	if len(a.removeHosts(hosts)) != 2 {
		t.Fatal("remove still targets first Server")
	}
	a.dest.Roles["app"] = config.Role{Services: []string{"web"}, Servers: []string{"b", "a"}}
	if server, e := a.serviceServer("web", "b"); e != nil || server != "b" {
		t.Fatal(server, e)
	}
}

func TestReviewRolesLogsAllFollowRequiresServer(t *testing.T) {
	a, _ := reviewApp(t)
	c := &cobra.Command{}
	addServerFlag(c)
	if err := a.rolesLogs(c, nil, dash.LogsOptions{Follow: true}); err == nil || !strings.Contains(err.Error(), "--server") {
		t.Fatal("--all -f silently returned without following any Services", err)
	}
}

func TestReviewRolesLogsAllGroupsPlacements(t *testing.T) {
	a, _ := reviewApp(t)
	// Replicas belong in both groups for --all, even though an explicitly
	// named Service defaults to its first Server.
	a.dest.Roles["app"] = config.Role{Services: []string{"web"}, Servers: []string{"a", "b"}}
	for _, tt := range []struct {
		name     string
		services []string
		server   string
		want     map[string][]string
	}{
		{"--all", nil, "", map[string][]string{"a": {"web"}, "b": {"db", "web"}}},
		{"--all --server b", nil, "b", map[string][]string{"b": {"db", "web"}}},
		{"--all -f --server a", nil, "a", map[string][]string{"a": {"web"}}},
		{"named replica", []string{"web"}, "", map[string][]string{"a": {"web"}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := a.rolesLogGroups(tt.services, tt.server)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatal(got, err, "want", tt.want)
			}
		})
	}
	if _, err := a.rolesLogGroups(nil, "missing"); err == nil {
		t.Fatal("unknown --server silently accepted")
	}
	if _, err := a.rolesLogGroups([]string{"db"}, "a"); err == nil {
		t.Fatal("named Service outside placement accepted")
	}
}
func TestReviewBackupCheckResolvesOwner(t *testing.T) {
	_, file := reviewApp(t)
	old := connectServer
	t.Cleanup(func() { connectServer = old })
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	connectServer = func(name, target string, sudo bool, opts remote.SSHOptions) (remote.Host, error) {
		return &doctorFakeHost{name: name, out: func(c remote.Cmd) (string, error) {
			if strings.Contains(c.Script, "docker version") {
				return "28.0", nil
			}
			return "", errors.New("reached owner operation")
		}}, nil
	}
	var out bytes.Buffer
	c := backupCheckCmd(&globals{configPath: file, json: true})
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"data"})
	e := c.Execute()
	if e == nil || strings.Contains(e.Error(), "no owning Server") || !strings.Contains(out.String(), `"host":"b"`) {
		t.Fatalf("owner not resolved: %v %s", e, out.String())
	}
}
func TestReviewLegacyRollbackLockMetadataAndSingleStep(t *testing.T) {
	var events []string
	h := &reviewLockInfoHost{rolesLockHost: rolesLockHost{name: "one", events: &events}}
	var out bytes.Buffer
	a := &app{cfg: &config.Config{App: "shop"}, destName: "production", ui: ui.New(&out, ui.JSON, false)}
	// This is rollback's caller-owned step; operationLocks must add none.
	step := a.ui.Step("one", "Acquire lock")
	u, e := a.operationLocks(context.Background(), []plan.NamedHost{{Name: "one", Host: h}}, deploy.LockInfo{Version: "v1", Command: "rollback"}, false)
	if e != nil {
		t.Fatal(e)
	}
	step.Done()
	_ = u(context.Background())
	if h.command != "rollback" || strings.Count(out.String(), `"event":"step_start"`) != 1 {
		t.Fatal(h.command, out.String())
	}
}

type reviewLockInfoHost struct {
	rolesLockHost
	command string
}

func (h *reviewLockInfoHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		var info deploy.LockInfo
		_ = json.Unmarshal(b, &info)
		h.command = info.Command
	}
	return h.rolesLockHost.Output(ctx, c)
}
func TestReviewPersistedOutcomeRetainsUnreachedServices(t *testing.T) {
	r := &auditRun{mutated: true, op: &operation.Record{Services: map[string]string{"b/db": "changed"}}}
	ctx := context.WithValue(context.Background(), auditRunKey{}, r)
	e := classifyOperation(ctx, &deploy.RolesFailure{Err: errors.New("consumer unreachable"), Changed: []string{"b/db"}, Unchanged: []string{"a/web"}})
	var m *mutationError
	if !errors.As(e, &m) || !reflect.DeepEqual(m.result.Unchanged, []string{"a/web"}) {
		t.Fatal(e, m)
	}
}
func TestReviewMeshMissingAllocationDoesNotFallback(t *testing.T) {
	a, _ := reviewApp(t)
	a.cfg.Network = config.NetworkConfig{Mesh: "wireguard", Subnet: "10.210.0.0/24"}
	old := connectServer
	t.Cleanup(func() { connectServer = old })
	connectServer = func(name, target string, sudo bool, opts remote.SSHOptions) (remote.Host, error) {
		return &doctorFakeHost{name: name}, nil
	}
	hosts := []plan.NamedHost{{Name: "a", Server: config.Server{PrivateAddress: "10.0.0.99"}}}
	if e := a.rolesAddresses(context.Background(), hosts); e == nil || !strings.Contains(e.Error(), "yoho mesh up") {
		t.Fatal(e)
	}
}
func TestReviewPendingRotationsChecksNonFirstServer(t *testing.T) {
	a, _ := reviewApp(t)
	seen := false
	hosts := []plan.NamedHost{{Name: "a", Host: &doctorFakeHost{out: func(c remote.Cmd) (string, error) { return "", nil }}}, {Name: "b", Host: &doctorFakeHost{out: func(c remote.Cmd) (string, error) { seen = true; return "PASSWORD", nil }}}}
	if e := a.refusePendingRotations(context.Background(), hosts, nil); !seen || e == nil {
		t.Fatal("non-first pending rotation ignored", e)
	}
}

func TestReviewCatchallURLUsesIngressServer(t *testing.T) {
	a, _ := reviewApp(t)
	r := &composefile.Result{Ext: map[string]config.ServiceExt{"web": {Proxy: &config.ServiceProxy{}}}}
	hosts := []plan.NamedHost{{Name: "b", Server: a.cfg.Servers["b"]}, {Name: "a", Server: a.cfg.Servers["a"]}}
	got := a.urls(r, hosts)
	if !strings.Contains(got, "app.example") || strings.Contains(got, "data.example") {
		t.Fatal(got)
	}
}
func TestReviewNamedTunnelKeepsPublicProxyPortsClosed(t *testing.T) {
	a, _ := reviewApp(t)
	a.cfg.Proxy.Tunnel = &config.TunnelConfig{TokenSecret: "TOKEN"}
	p := a.proxyConfig()
	if p.HTTPPort == nil || *p.HTTPPort != 0 || p.HTTPSPort == nil || *p.HTTPSPort != 0 {
		t.Fatal(p)
	}
}

func TestReviewScheduleStatusVisitsEveryRoleServer(t *testing.T) {
	_, file := reviewApp(t)
	old := connectServer
	t.Cleanup(func() { connectServer = old })
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	seenB := false
	connectServer = func(name, target string, sudo bool, opts remote.SSHOptions) (remote.Host, error) {
		spec, _ := json.Marshal(schedule.Spec{Version: 1, App: "shop", Destination: "production", Job: "data", Schedule: "daily"})
		files := map[string][]byte{}
		if name == "b" {
			files[schedule.SpecPath("shop", "production", "data")] = spec
		}
		return &doctorFakeHost{name: name, files: files, out: func(c remote.Cmd) (string, error) {
			if strings.Contains(c.Script, "docker version") {
				return "28.0", nil
			}
			if strings.Contains(c.Script, "ls -1") && name == "b" {
				seenB = true
				return "shop-production-data.json", nil
			}
			return "", nil
		}}, nil
	}
	var out bytes.Buffer
	c := scheduleStatusCmd(&globals{configPath: file, json: true})
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(nil)
	if e := c.Execute(); e != nil || !seenB || !strings.Contains(out.String(), "data") {
		t.Fatal(e, out.String())
	}
}
func TestReviewDoctorDrillReadsAuditAuthority(t *testing.T) {
	a, _ := reviewApp(t)
	old := connectServer
	t.Cleanup(func() { connectServer = old })
	authorityRead := false
	connectServer = func(name, target string, sudo bool, opts remote.SSHOptions) (remote.Host, error) {
		if name != "a" {
			t.Fatal("wrong audit authority", name)
		}
		return &doctorFakeHost{name: name, out: func(c remote.Cmd) (string, error) { authorityRead = true; return "", nil }}, nil
	}
	_ = checkDoctorDrill(context.Background(), a, plan.NamedHost{Name: "b", Host: &doctorFakeHost{name: "b"}}, time.Now())
	if !authorityRead {
		t.Fatal("doctor looked for audit on data Server instead of journal authority")
	}
}

func TestReviewDataOnlyServerDoesNotRequirePublicProxyFirewall(t *testing.T) {
	a, _ := reviewApp(t)
	h := &doctorFakeHost{name: "b", out: func(c remote.Cmd) (string, error) {
		switch {
		case strings.Contains(c.Script, "id -u"):
			return "root", nil
		case strings.Contains(c.Script, "sshd -T"):
			return "port 22\n", nil
		case strings.Contains(c.Script, "ufw status"):
			return "Status: active\n22/tcp ALLOW Anywhere\n", nil
		}
		return "", nil
	}}
	fs := checkDoctorFirewall(context.Background(), a, plan.NamedHost{Name: "b", Host: h})
	if len(fs) != 1 || fs[0].Severity != severityInfo {
		t.Fatal(fs)
	}
}

func TestReviewUnusedGeneratedRotationNeverBecomesFullDeploy(t *testing.T) {
	// No Project/Servers: reaching the full Runtime is an invalid operation.
	// An unused key must merely complete rotation bookkeeping, with no rollout.
	if e := deployRolesRotation(context.Background(), &plan.Deploy{}, nil); e != nil {
		t.Fatal(e)
	}
}
func TestReviewMaintenanceLockAndBackupCoverAllServers(t *testing.T) {
	a, _ := reviewApp(t)
	old := connectServer
	t.Cleanup(func() { connectServer = old })
	held := map[string]bool{}
	pool := map[string]*doctorFakeHost{}
	for _, name := range []string{"a", "b"} {
		name := name
		pool[name] = &doctorFakeHost{name: name, out: func(c remote.Cmd) (string, error) {
			if strings.Contains(c.Script, "docker version") {
				return "28.0", nil
			}
			info, _ := json.Marshal(deploy.LockInfo{Command: deploy.ManualLockCommand, Performer: "test", Message: "maintenance"})
			if strings.Contains(c.Script, "echo acquired") {
				if held[name] {
					return "held\n" + string(info), nil
				}
				held[name] = true
				return "acquired", nil
			}
			if strings.HasPrefix(c.Script, "rm -rf") {
				held[name] = false
				return "", nil
			}
			if strings.Contains(c.Script, "echo held") {
				if held[name] {
					return "held\n" + string(info), nil
				}
				return "free", nil
			}
			return "", nil
		}}
	}
	connectServer = func(name, target string, sudo bool, opts remote.SSHOptions) (remote.Host, error) {
		return pool[name], nil
	}
	if e := a.rolesManualLock(context.Background(), "acquire", "maintenance", false); e != nil {
		t.Fatal(e)
	}
	if !held["a"] || !held["b"] {
		t.Fatal("maintenance lock left a data Server writable", held)
	}
	if _, e := a.backupLock(pool["b"], "backup")(context.Background()); e == nil {
		t.Fatal("Backup bypassed maintenance lock")
	}
	if e := a.rolesManualLock(context.Background(), "release", "", true); e != nil {
		t.Fatal(e)
	}
	if held["a"] || held["b"] {
		t.Fatal("release left another Server locked", held)
	}
}

func TestReviewOperationStatusSearchesEveryRolesServer(t *testing.T) {
	_, file := reviewApp(t)
	old := connectServer
	t.Cleanup(func() { connectServer = old })
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	visited := map[string]bool{}
	record, _ := json.Marshal(operation.Record{ID: "op123", State: "finished", Result: operation.Result{Outcome: "partial", Changed: []string{"b/db"}, Unchanged: []string{"a/web"}}})
	connectServer = func(name, target string, sudo bool, opts remote.SSHOptions) (remote.Host, error) {
		return &doctorFakeHost{name: name, out: func(c remote.Cmd) (string, error) {
			if strings.Contains(c.Script, "docker version") {
				return "28.0", nil
			}
			visited[name] = true
			if name == "a" {
				return "", os.ErrNotExist
			}
			return string(record), nil
		}}, nil
	}
	var out bytes.Buffer
	c := operationStatusCmd(&globals{configPath: file, json: true})
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"--operation", "op123"})
	if e := c.Execute(); e != nil || !visited["a"] || !visited["b"] || !strings.Contains(out.String(), `"b/db"`) {
		t.Fatal(e, out.String(), visited)
	}
}

func TestReviewMaintenanceInspectionDoesNotRequireDockerOrMesh(t *testing.T) {
	a, _ := reviewApp(t)
	a.cfg.Network.Mesh = "wireguard"
	old := connectServer
	t.Cleanup(func() { connectServer = old })
	seen := map[string]bool{}
	connectServer = func(name, target string, sudo bool, opts remote.SSHOptions) (remote.Host, error) {
		return &doctorFakeHost{name: name, out: func(c remote.Cmd) (string, error) {
			if strings.Contains(c.Script, "docker") || strings.Contains(c.Script, "mesh/") {
				t.Fatal("maintenance inspection probed unavailable runtime", c.Script)
			}
			seen[name] = true
			return "free", nil
		}}, nil
	}
	if e := a.rolesManualLock(context.Background(), "status", "", false); e != nil || len(seen) != 2 {
		t.Fatal(e, seen)
	}
}
