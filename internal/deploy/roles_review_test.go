package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"path"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/roles"
)

func TestReviewPinnedGeneratedOwnerDeclaredNamesAndPruning(t *testing.T) {
	d := rolesTestDeploy()
	keyPath := path.Join(release.AppDir(d.App, d.Destination), "generated", "PASSWORD")
	d.Roles.Roles["app"] = config.Role{Services: []string{"web"}, Servers: []string{"a", "b"}}
	ext := d.Ext["db"]
	ext.Stateful = false
	ext.Secrets = config.SecretRefs{{Name: "POSTGRES_PASSWORD", Key: "PASSWORD"}}
	d.Ext["db"] = ext
	rel := release.Release{Version: "v1", GeneratedOwners: map[string]string{"PASSWORD": "b"}}
	b, _ := json.Marshal(rel)
	var pruned []string
	for i, h := range d.Servers {
		base := h.Host.(*rolesHost)
		base.files[path.Join(release.Dir(d.App, d.Destination, "v1"), "release.json")] = b
		if h.Name == "b" {
			base.files[keyPath] = []byte("old-encryption-key")
		}
		hostName := h.Name
		d.Servers[i].Host = &moveTestHost{rolesHost: base, respond: func(c remote.Cmd) (string, error, bool) {
			if strings.Contains(c.Script, "readlink") {
				return "releases/v1", nil, true
			}
			if strings.Contains(c.Script, "ln \"$t\"") {
				return "", nil, true
			}
			if strings.Contains(c.Script, "rm -f --") {
				pruned = append(pruned, hostName+":"+c.Script)
				return "", nil, true
			}
			return "", nil, false
		}}
	}
	parts, _, e := roleParts(d)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = rolesGenerated(context.Background(), d, parts, true); e != nil {
		t.Fatal(e)
	}
	if d.GeneratedOwners["PASSWORD"] != "b" || parts["a"].ServiceSecrets["web"]["DB_PASS"] != "old-encryption-key" {
		t.Fatal("owner shifted or value regenerated", d.GeneratedOwners)
	}
	if _, leaked := parts["b"].ServiceSecrets["db"]["PASSWORD"]; leaked {
		t.Fatal("undeclared generator name leaked")
	}
	if len(parts["b"].Ext["db"].Generate) > 0 {
		t.Fatal("per-Server runtime can regenerate shared key")
	}
	ext = d.Ext["web"]
	ext.Secrets = nil
	d.Ext["web"] = ext
	parts, _, _ = roleParts(d)
	if _, e = rolesGenerated(context.Background(), d, parts, true); e != nil {
		t.Fatal(e)
	}
	if len(pruned) == 0 || !strings.HasPrefix(pruned[0], "a:") {
		t.Fatal("unused consumer key not pruned", pruned)
	}
}

func TestReviewPreflightAllServersBeforeMutation(t *testing.T) {
	d := rolesTestDeploy()
	d.Roles.Connections = map[string]config.Connection{"db": {Service: "db", Port: 5432, Consumers: []string{"web"}}}
	d.PrivateAddresses = map[string]string{"a": "10.0.0.1", "b": "10.0.0.2"}
	var mutations []string
	for i, h := range d.Servers {
		base := h.Host.(*rolesHost)
		server := h.Name
		d.Servers[i].Host = &moveTestHost{rolesHost: base, respond: func(c remote.Cmd) (string, error, bool) {
			if strings.Contains(c.Script, "readlink") {
				return "", nil, true
			}
			if strings.Contains(c.Script, "LoggingDriver") {
				return "json-file", nil, true
			}
			if strings.Contains(c.Script, "docker ps -a --filter") && server == "a" {
				return "foreign-app|production", nil, true
			}
			if c.Stdin != nil || strings.Contains(c.Script, "up'") || strings.Contains(c.Script, "iptables") {
				mutations = append(mutations, c.Script)
			}
			return "", nil, true
		}}
	}
	_, e := (Roles{LockHeld: true}).Deploy(context.Background(), d)
	if e == nil {
		t.Fatal("foreign consumer project accepted")
	}
	if len(mutations) > 0 {
		t.Fatal("provider mutated before consumer preflight", mutations)
	}
}

func TestReviewMigrationBeforeFirstReplicaOnProviderServer(t *testing.T) {
	d := rolesTestDeploy()
	d.Roles.Roles["app"] = config.Role{Services: []string{"web"}, Servers: []string{"a", "b"}}
	d.Roles.Connections = map[string]config.Connection{"db": {Service: "db", Port: 5432, Consumers: []string{"web"}}}
	d.PrivateAddresses = map[string]string{"a": "10.0.0.1", "b": "10.0.0.2"}
	ext := d.Ext["web"]
	ext.ReleaseCommand = []string{"migrate"}
	d.Ext["web"] = ext
	var events []string
	for i, h := range d.Servers {
		base := h.Host.(*rolesHost)
		server := h.Name
		base.files[path.Join(release.AppDir(d.App, d.Destination), "hmac.key")] = []byte(strings.Repeat("ab", 32))
		base.files[path.Join(release.AppDir(d.App, d.Destination), "generated", "PASSWORD")] = []byte("password")
		d.Servers[i].Host = &moveTestHost{rolesHost: base, respond: func(c remote.Cmd) (string, error, bool) {
			if strings.Contains(c.Script, "LoggingDriver") {
				return "json-file", nil, true
			}
			if strings.Contains(c.Script, "'migrate'") {
				events = append(events, "migration "+server)
				return "", errors.New("migration failed"), true
			}
			if strings.Contains(c.Script, "'up'") {
				events = append(events, "start "+server)
			}
			return "", nil, true
		}}
	}
	parts, order, e := roleParts(d)
	if e != nil {
		t.Fatal(e)
	}
	if !slices.Equal(order, []string{"b", "a"}) {
		t.Fatal(order)
	}
	if _, e = rolesGenerated(context.Background(), d, parts, false); e != nil {
		t.Fatal(e)
	}
	e = rolesReleasePhase(context.Background(), d, parts)
	if e == nil || !strings.Contains(e.Error(), "migration") {
		t.Fatal(e)
	}
	if !slices.Contains(events, "migration a") || slices.Contains(events, "start a") {
		t.Fatal("migration ran on wrong Server or consumer replica started", events)
	}
}

func TestReviewFirstProxiedReleaseCommandPreparesProxy(t *testing.T) {
	for _, bootFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "first deploy", true: "network failure"}[bootFails], func(t *testing.T) {
			d := rolesTestDeploy()
			d.Ext = map[string]config.ServiceExt{
				"web": {Proxy: &config.ServiceProxy{Port: 8000}, ReleaseCommand: []string{"migrate"}},
				"db":  {Stateful: true},
			}
			network, proxy, migrated := false, false, false
			for i, h := range d.Servers {
				base, server := h.Host.(*rolesHost), h.Name
				base.files[path.Join(release.AppDir(d.App, d.Destination), "hmac.key")] = []byte(strings.Repeat("ab", 32))
				d.Servers[i].Host = &moveTestHost{rolesHost: base, respond: func(c remote.Cmd) (string, error, bool) {
					switch {
					case strings.Contains(c.Script, "LoggingDriver"):
						return "json-file", nil, true
					case strings.Contains(c.Script, "docker network create yoho"):
						if server != "a" {
							t.Fatal("Proxy network prepared on data-only Server")
						}
						if bootFails {
							return "", errors.New("network unavailable"), true
						}
						network = true
					case strings.Contains(c.Script, "docker run -d --label 'yoho.proxy.config="):
						if server != "a" || !network {
							t.Fatal("Proxy booted before network or on wrong Server")
						}
						proxy = true
					case strings.Contains(c.Script, "'migrate'"):
						if server != "a" || !network || !proxy {
							return "", errors.New("release container requires yoho network and Proxy"), true
						}
						migrated = true
					}
					return "", nil, true
				}}
			}
			parts, _, err := roleParts(d)
			if err != nil {
				t.Fatal(err)
			}
			err = rolesReleasePhase(context.Background(), d, parts)
			if bootFails {
				if err == nil || !strings.Contains(err.Error(), "network unavailable") || migrated {
					t.Fatal("release ran after network failure", err, migrated)
				}
			} else if err != nil || !migrated {
				t.Fatal("fresh proxied release command failed", err, migrated)
			}
		})
	}
}
func TestReviewMoveBackupCoversEveryVolumeEvenWithDump(t *testing.T) {
	d := rolesTestDeploy()
	svc := d.Project.Services["db"]
	svc.HealthCheck = &types.HealthCheckConfig{Test: []string{"CMD", "true"}}
	svc.Volumes = []types.ServiceVolumeConfig{{Type: types.VolumeTypeVolume, Source: "data", Target: "/data"}, {Type: types.VolumeTypeVolume, Source: "uploads", Target: "/uploads"}}
	d.Project.Services["db"] = svc
	d.Project.Volumes = types.Volumes{"data": {}, "uploads": {}}
	ext := d.Ext["db"]
	ext.Backup = &config.ServiceBackup{Volumes: []string{"data"}, Dump: []string{"pg_dump"}}
	d.Ext["db"] = ext
	if _, e := PlanMove(d, "db", "a"); e == nil || !strings.Contains(e.Error(), "uploads") {
		t.Fatal("dump incorrectly exempted unrelated volume", e)
	}
}
func TestReviewOwnershipGuardAlsoRunsDuringPlan(t *testing.T) {
	d := rolesTestDeploy()
	base := d.Servers[0].Host.(*rolesHost)
	old := release.Release{Version: "v1", Images: map[string]string{"db": "postgres"}, Roles: d.Roles.Roles}
	b, _ := json.Marshal(old)
	base.files[path.Join(release.Dir(d.App, d.Destination, "v1"), "release.json")] = b
	d.Servers[0].Host = &moveTestHost{rolesHost: base, respond: func(c remote.Cmd) (string, error, bool) {
		if strings.Contains(c.Script, "readlink") {
			return "releases/v1", nil, true
		}
		return "", nil, true
	}}
	d.Roles.Roles = map[string]config.Role{"all": {Services: []string{"web", "db"}, Servers: []string{"a"}}}
	if _, e := (Roles{}).Diff(context.Background(), d); e == nil || !strings.Contains(e.Error(), "ownership") {
		t.Fatal("plan accepted Stateful relocation", e)
	}
}
func TestReviewOrderRefusesMissingRollbackServer(t *testing.T) {
	d := rolesTestDeploy()
	d.Roles.Connections = map[string]config.Connection{"db": {Service: "db", Port: 5432, Consumers: []string{"web"}}}
	d.Roles.Servers = []string{"a"}
	if _, e := roles.Order(d.Roles); e == nil {
		t.Fatal("missing provider Server accepted")
	}
}

func TestReviewLateMoveFailureKeepsAuthoritativeTarget(t *testing.T) {
	withRoot(t)
	d := rolesTestDeploy()
	delete(d.Ext, "web")
	d.Ext["db"] = config.ServiceExt{Stateful: true}
	rel := release.Release{Version: "v1", Images: map[string]string{"db": "postgres"}, Roles: d.Roles.Roles}
	b, _ := json.Marshal(rel)
	sourceBase := d.Servers[0].Host.(*rolesHost)
	targetBase := d.Servers[1].Host.(*rolesHost)
	sourceBase.files[path.Join(release.Dir(d.App, d.Destination, "v1"), "release.json")] = b
	sourceBase.files[path.Join(release.Dir(d.App, d.Destination, "v1"), "compose.yaml")] = []byte("services: {db: {image: postgres}}")
	for _, base := range []*rolesHost{sourceBase, targetBase} {
		base.files[path.Join(release.AppDir(d.App, d.Destination), "hmac.key")] = []byte(strings.Repeat("ab", 32))
	}
	var events []string
	created, started := false, false
	project := release.ProjectName(d.App, d.Destination)
	vjson := `{"Driver":"local","Labels":{"com.docker.compose.project":"` + project + `"}}`
	source := &moveTestHost{rolesHost: sourceBase}
	target := &moveTestHost{rolesHost: targetBase}
	source.respond = func(c remote.Cmd) (string, error, bool) {
		switch {
		case strings.Contains(c.Script, "readlink"):
			return "releases/v1", nil, true
		case strings.Contains(c.Script, "docker volume inspect"):
			return vjson, nil, true
		case strings.Contains(c.Script, "LoggingDriver"):
			if started {
				return "", errors.New("late consumer failed after target accepted writes"), true
			}
			return "json-file", nil, true
		case strings.Contains(c.Script, "docker stop"):
			events = append(events, "source stopped")
			return "", nil, true
		case strings.Contains(c.Script, "tar -C /data -cpf"):
			_, e := io.WriteString(c.Stdout, "data")
			return "", e, true
		case strings.Contains(c.Script, "--force-recreate"):
			events = append(events, "source restarted")
			return "", nil, true
		}
		return "", nil, true
	}
	target.respond = func(c remote.Cmd) (string, error, bool) {
		switch {
		case strings.Contains(c.Script, "readlink"):
			return "", nil, true
		case strings.Contains(c.Script, "docker volume inspect"):
			if created {
				return vjson, nil, true
			}
			return "", nil, true
		case strings.Contains(c.Script, "docker volume create"):
			created = true
			return "", nil, true
		case strings.Contains(c.Script, "tar -C /data -xpf"):
			_, e := io.Copy(io.Discard, c.Stdin)
			return "", e, true
		case strings.Contains(c.Script, "LoggingDriver"):
			return "json-file", nil, true
		case strings.Contains(c.Script, "'up'"):
			started = true
			events = append(events, "target started")
			return "", nil, true
		case strings.Contains(c.Script, "docker stop"):
			events = append(events, "target stopped")
			return "", nil, true
		}
		return "", nil, true
	}
	d.Servers[0].Host = source
	d.Servers[1].Host = target
	e := MoveService(context.Background(), d, MovePlan{Service: "db", Source: "b", Target: "a", Volumes: []string{project + "_data"}}, func(context.Context) error { return nil }, func(string, string) {})
	var partial *RolesFailure
	if !errors.As(e, &partial) || !started || slices.Contains(events, "source restarted") || slices.Contains(events, "target stopped") {
		t.Fatal("late failure discarded target writes", e, events)
	}
	var journal MoveJournal
	_ = json.Unmarshal(source.files[moveJournalPath(d, "db")], &journal)
	if journal.State != "partial" || journal.Steps["source"] != "stopped-volume-retained" {
		t.Fatal(journal)
	}
}
func TestReviewRollbackRejectsIncoherentOrMissingInventory(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing Server", true: "failed Release"}[failed], func(t *testing.T) {
			d := rolesTestDeploy()
			old := release.Release{Version: "v1", Roles: maps.Clone(d.Roles.Roles), Status: "deployed"}
			if failed {
				old.Status = "failed"
			} else {
				old.Roles["removed"] = config.Role{Services: []string{"cache"}, Servers: []string{"c"}}
			}
			b, _ := json.Marshal(old)
			h := d.Servers[0].Host.(*rolesHost)
			h.files[path.Join(release.Dir(d.App, d.Destination, "v1"), "release.json")] = b
			_, e := (Roles{LockHeld: true}).Rollback(context.Background(), d, "v1")
			if e == nil || !(strings.Contains(e.Error(), "missing Server") || strings.Contains(e.Error(), "successful Release")) {
				t.Fatal(e)
			}
		})
	}
}

func TestReviewOwnerPinSurvivesFailedRolloutAndServerRemoval(t *testing.T) {
	d := rolesTestDeploy()
	d.Servers = d.Servers[1:]
	d.Roles.Servers = []string{"a"}
	d.Roles.Roles = map[string]config.Role{"all": {Services: []string{"web", "db"}, Servers: []string{"a"}}}
	h := d.Servers[0].Host.(*rolesHost)
	h.files[path.Join(release.AppDir(d.App, d.Destination), "generated-owners.json")] = []byte(`{"PASSWORD":"b"}`)
	parts, _, e := roleParts(d)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = rolesGenerated(context.Background(), d, parts, false); e == nil || !strings.Contains(e.Error(), "pinned") {
		t.Fatal("first-generation owner silently changed after interrupted rollout", e)
	}
}
