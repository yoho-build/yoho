package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"reflect"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/roles"
)

// rolesHost stores each Server's files independently and records lock order.
type rolesHost struct {
	name     string
	files    map[string][]byte
	events   *[]string
	failLock bool
}

func (h *rolesHost) Name() string { return h.name }
func (h *rolesHost) Close() error { return nil }
func (h *rolesHost) ReadFile(_ context.Context, p string, _ bool) ([]byte, error) {
	b, ok := h.files[p]
	if !ok {
		return nil, os.ErrNotExist
	}
	return bytes.Clone(b), nil
}
func (h *rolesHost) WriteFile(_ context.Context, p string, b []byte, _ os.FileMode, _ bool) error {
	h.files[p] = bytes.Clone(b)
	return nil
}
func (h *rolesHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := h.Output(ctx, c)
	return err
}
func (h *rolesHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	if strings.Contains(c.Script, "echo acquired") {
		if h.events != nil {
			*h.events = append(*h.events, "lock "+h.name)
		}
		if h.failLock {
			return "held", nil
		}
		return "acquired", nil
	}
	if strings.HasPrefix(c.Script, "rm -rf ") && strings.Contains(c.Script, "/lock") {
		if h.events != nil {
			*h.events = append(*h.events, "unlock "+h.name)
		}
	}
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		if strings.Contains(c.Script, "generated") {
			for _, line := range strings.Split(c.Script, "\n") {
				if strings.HasPrefix(line, "  ln ") {
					parts := strings.Split(line, "'")
					for _, p := range parts {
						if strings.Contains(p, "/generated/") && !strings.Contains(p, "XXXXXX") {
							h.files[p] = b
						}
					}
				}
			}
		}
	}
	return "", nil
}
func rolesTestDeploy() *plan.Deploy {
	dest := config.Destination{Servers: []string{"b", "a"}, Roles: map[string]config.Role{"app": {Services: []string{"web"}, Servers: []string{"a"}}, "data": {Services: []string{"db"}, Servers: []string{"b"}}}}
	return &plan.Deploy{App: "shop", Destination: "production", Version: "v1", Roles: dest, Project: &types.Project{Services: types.Services{"web": {Name: "web"}, "db": {Name: "db"}}}, Ext: map[string]config.ServiceExt{"web": {Secrets: config.SecretRefs{{Name: "DB_PASS", Key: "PASSWORD"}}}, "db": {Stateful: true, Generate: map[string]string{"PASSWORD": "password32"}}}, Servers: []plan.NamedHost{{Name: "b", Host: &rolesHost{name: "b", files: map[string][]byte{}}}, {Name: "a", Host: &rolesHost{name: "a", files: map[string][]byte{}}}}}
}
func TestRolesGeneratedSelectionAndFanout(t *testing.T) {
	d := rolesTestDeploy()
	if generatedOwner(d, "PASSWORD") != "b" {
		t.Fatal("Stateful consumer must own shared generation")
	}
	ext := d.Ext["db"]
	ext.Stateful = false
	d.Ext["db"] = ext
	if generatedOwner(d, "PASSWORD") != "a" {
		t.Fatal("fallback must use sorted consumer Server")
	}
	ext.Stateful = true
	d.Ext["db"] = ext
	p := path.Join(release.AppDir(d.App, d.Destination), "generated", "PASSWORD")
	d.Servers[0].Host.(*rolesHost).files[p] = []byte("shared-value")
	parts, _, err := roleParts(d)
	if err != nil {
		t.Fatal(err)
	}
	_, err = rolesGenerated(context.Background(), d, parts, false)
	if err != nil {
		t.Fatal(err)
	}
	if parts["a"].ServiceSecrets["web"]["DB_PASS"] != "shared-value" || len(parts["a"].ServiceSecrets["db"]) != 0 {
		t.Fatal("fanout leaked or lost a value")
	}
	if _, err = d.Servers[1].Host.ReadFile(context.Background(), p, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("planning wrote a secret")
	}
}
func TestRolesLockOrderingAndUnwind(t *testing.T) {
	withRoot(t)
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "refusal"}[fail], func(t *testing.T) {
			var events []string
			d := rolesTestDeploy()
			for _, h := range d.Servers {
				f := h.Host.(*rolesHost)
				f.events = &events
				f.failLock = fail && h.Name == "b"
			}
			done, err := (Roles{}).lock(context.Background(), d)
			if fail {
				if err == nil {
					t.Fatal("held lock accepted")
				}
				if !slicesContain(events, "unlock a") {
					t.Fatal("first lock not released", events)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if err = done(); err != nil {
					t.Fatal(err)
				}
			}
			if len(events) < 2 || events[0] != "lock a" || events[1] != "lock b" {
				t.Fatal(events)
			}
		})
	}
}
func slicesContain(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
func TestRolesLegacyCompileBytes(t *testing.T) {
	d := testDeploy(t, nil, nil)
	before, _, _ := compileTest(t, d)
	legacy, err := roles.Render(d, config.Destination{}, "primary", nil)
	if err != nil {
		t.Fatal(err)
	}
	after, _, _ := compileTest(t, legacy)
	if !bytes.Equal(before, after) {
		t.Fatal("legacy compiled bytes changed")
	}
}
func TestRolesMoveValidationAndConfigEdit(t *testing.T) {
	d := rolesTestDeploy()
	svc := d.Project.Services["db"]
	svc.HealthCheck = &types.HealthCheckConfig{Test: []string{"CMD", "true"}}
	svc.Volumes = []types.ServiceVolumeConfig{{Type: types.VolumeTypeVolume, Source: "data", Target: "/data"}}
	d.Project.Services["db"] = svc
	d.Project.Volumes = types.Volumes{"data": {}}
	ext := d.Ext["db"]
	ext.Backup = &config.ServiceBackup{Volumes: []string{"data"}}
	d.Ext["db"] = ext
	m, err := PlanMove(d, "db", "a")
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != "b" || len(m.Volumes) != 1 {
		t.Fatal(m)
	}
	changed := MovedDestination(d.Roles, "db", "a")
	if !reflect.DeepEqual(changed.ServiceServers("db"), []string{"a"}) || d.Roles.ServiceServers("db")[0] != "b" {
		t.Fatal("config edit mutated input")
	}
	d.Project.Volumes["data"] = types.VolumeConfig{External: types.External(true)}
	if _, err = PlanMove(d, "db", "a"); err == nil {
		t.Fatal("foreign volume accepted")
	}
	d.Project.Volumes["data"] = types.VolumeConfig{}
	ext.Backup = nil
	d.Ext["db"] = ext
	if _, err = PlanMove(d, "db", "a"); err == nil {
		t.Fatal("move without Backup accepted")
	}
}
func TestRolesReleaseMetadataOptional(t *testing.T) {
	old, _ := json.Marshal(release.Release{})
	if strings.Contains(string(old), `"roles"`) {
		t.Fatal("legacy Release JSON changed")
	}
	d := rolesTestDeploy()
	b, _ := json.Marshal(release.Release{Roles: d.Roles.Roles})
	var rel release.Release
	if err := json.Unmarshal(b, &rel); err != nil || len(rel.Roles) != 2 {
		t.Fatal(string(b), err)
	}
}

type moveTestHost struct {
	*rolesHost
	respond func(remote.Cmd) (string, error, bool)
}

func (h *moveTestHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if out, e, ok := h.respond(c); ok {
		return out, e
	}
	return h.rolesHost.Output(ctx, c)
}
func (h *moveTestHost) Run(ctx context.Context, c remote.Cmd) error {
	_, e := h.Output(ctx, c)
	return e
}

func TestRolesMoveFailedRestoreRestartsSource(t *testing.T) {
	withRoot(t)
	d := rolesTestDeploy()
	delete(d.Ext, "web")
	d.Ext["db"] = config.ServiceExt{Stateful: true}
	sourceBase := d.Servers[0].Host.(*rolesHost)
	targetBase := d.Servers[1].Host.(*rolesHost)
	rel := release.Release{App: d.App, Destination: d.Destination, Version: "v1", Images: map[string]string{"db": "postgres:17"}, Roles: d.Roles.Roles}
	b, _ := json.Marshal(rel)
	sourceBase.files[path.Join(release.Dir(d.App, d.Destination, "v1"), "release.json")] = b
	sourceBase.files[path.Join(release.Dir(d.App, d.Destination, "v1"), "compose.yaml")] = []byte("services: {db: {image: 'postgres:17'}}")
	volume := "yoho-shop-production_data"
	project := release.ProjectName(d.App, d.Destination)
	volumeJSON := `{"Driver":"local","Options":{},"Labels":{"com.docker.compose.project":"` + project + `"}}`
	var events []string
	created := false
	source := &moveTestHost{rolesHost: sourceBase}
	target := &moveTestHost{rolesHost: targetBase}
	source.respond = func(c remote.Cmd) (string, error, bool) {
		switch {
		case strings.Contains(c.Script, "readlink"):
			return "releases/v1", nil, true
		case strings.Contains(c.Script, "docker volume inspect"):
			return volumeJSON, nil, true
		case strings.Contains(c.Script, "docker stop"):
			events = append(events, "stop source")
			return "", nil, true
		case strings.Contains(c.Script, "tar -C /data -cpf"):
			_, e := io.WriteString(c.Stdout, "data")
			return "", e, true
		case strings.Contains(c.Script, "--no-deps --wait"):
			events = append(events, "restart source")
			return "", nil, true
		case strings.Contains(c.Script, ".LoggingDriver"):
			return "", errors.New("injected later consumer reconcile failure"), true
		}
		return "", nil, false
	}
	target.respond = func(c remote.Cmd) (string, error, bool) {
		switch {
		case strings.Contains(c.Script, "docker volume inspect"):
			if created {
				return volumeJSON, nil, true
			}
			return "", nil, true
		case strings.Contains(c.Script, "docker volume create"):
			created = true
			return "", nil, true
		case strings.Contains(c.Script, "docker ps -aq --filter 'volume="):
			return "", nil, true
		case strings.Contains(c.Script, "tar -C /data -xpf"):
			events = append(events, "restore failed")
			return "", errors.New("injected interrupted restore"), true
		case strings.Contains(c.Script, "docker stop"):
			events = append(events, "stop target")
			return "", nil, true
		}
		return "", nil, false
	}
	d.Servers[0].Host = source
	d.Servers[1].Host = target
	m := MovePlan{Service: "db", Source: "b", Target: "a", Volumes: []string{volume}}
	e := MoveService(context.Background(), d, m, func(context.Context) error { events = append(events, "backup"); return nil }, func(string, string) {})
	if e == nil || !strings.Contains(e.Error(), "injected interrupted restore") {
		t.Fatal(e)
	}
	expected := []string{"backup", "stop source", "restore failed", "stop target", "restart source"}
	if !reflect.DeepEqual(events, expected) {
		t.Fatal(events)
	}
	var journal MoveJournal
	if e := json.Unmarshal(source.files[moveJournalPath(d, "db")], &journal); e != nil {
		t.Fatal(e)
	}
	if journal.State != "failed" || journal.Steps["restore-empty-target"] != "failed" || journal.Steps["restart-source"] != "complete" {
		t.Fatal(journal)
	}
}

func TestRolesLegacyPlanBytes(t *testing.T) {
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	d := testDeploy(t, h, nil)
	before, e := (Compose{}).Diff(context.Background(), d)
	if e != nil {
		t.Fatal(e)
	}
	legacy, e := roles.Render(d, config.Destination{}, "primary", nil)
	if e != nil {
		t.Fatal(e)
	}
	after, e := (Compose{}).Diff(context.Background(), legacy)
	if e != nil {
		t.Fatal(e)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if !bytes.Equal(a, b) {
		t.Fatalf("legacy plans changed: %s vs %s", a, b)
	}
}
