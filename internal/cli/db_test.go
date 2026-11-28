package cli

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/pgpromote"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/ui"
)

func pgDest(provider string, consumers ...string) config.Destination {
	return config.Destination{
		Servers: []string{"a", "b"},
		Roles: map[string]config.Role{
			"app":     {Services: []string{"web"}, Servers: []string{"a"}},
			"primary": {Services: []string{"db"}, Servers: []string{"a"}},
			"standby": {Services: []string{"db-standby"}, Servers: []string{"b"}},
		},
		Connections: map[string]config.Connection{"primary": {Service: provider, Port: 5432, Consumers: consumers}},
	}
}

func pgRelease(provider string) *release.Release {
	d := pgDest(provider, "web")
	return &release.Release{Roles: d.Roles, Connections: d.Connections}
}

func TestDerivePromotion(t *testing.T) {
	edited := pgDest("db-standby", "web", "db")
	both := map[string]*release.Release{"a": pgRelease("db"), "b": pgRelease("db")}

	got, err := derivePromotion(edited, both, nil, "", "")
	if err != nil || got.Connection != "primary" || got.From != "db" || got.To != "db-standby" || got.FromServer != "a" || got.ToServer != "b" || got.Done {
		t.Fatalf("%+v %v", got, err)
	}
	if _, err = derivePromotion(edited, both, nil, "primary", "db"); err != nil {
		t.Fatal(err)
	}
	if _, err = derivePromotion(edited, both, nil, "", "web"); err == nil || !strings.Contains(err.Error(), "--from web, but") {
		t.Fatal(err)
	}
	if _, err = derivePromotion(edited, both, nil, "nope", ""); err == nil || !strings.Contains(err.Error(), "unknown Connection nope") {
		t.Fatal(err)
	}

	// Nothing edited: say what to edit.
	same := map[string]*release.Release{"a": pgRelease("db-standby"), "b": pgRelease("db-standby")}
	if _, err = derivePromotion(edited, same, nil, "", ""); err == nil || !strings.Contains(err.Error(), "edit `connections.primary.service` to the standby and list the old primary in `consumers`") {
		t.Fatal(err)
	}
	// --from when the Release already names the new primary.
	if got, err = derivePromotion(edited, same, nil, "", "db"); err != nil || got.From != "db" || got.FromServer != "a" {
		t.Fatalf("%+v %v", got, err)
	}
	// A finished promotion with a matching Release is a no-op; an
	// unfinished one resumes from its journal.
	j := &pgpromote.Journal{Connection: "primary", From: "db", To: "db-standby", State: "complete"}
	if got, err = derivePromotion(edited, same, map[string]*pgpromote.Journal{"primary": j}, "", ""); err != nil || !got.Done || got.From != "db" {
		t.Fatalf("%+v %v", got, err)
	}
	j.State = "failed"
	if got, err = derivePromotion(edited, same, map[string]*pgpromote.Journal{"primary": j}, "", ""); err != nil || got.Done || got.From != "db" {
		t.Fatalf("%+v %v", got, err)
	}

	// Releases that disagree refuse.
	mixed := map[string]*release.Release{"a": pgRelease("db"), "b": pgRelease("web")}
	if _, err = derivePromotion(edited, mixed, nil, "", ""); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatal(err)
	}
	// Two Connections moved: name one.
	two := pgDest("db-standby", "web", "db")
	two.Connections["cache"] = config.Connection{Service: "db", Port: 6379, Consumers: []string{"web"}}
	rel := pgRelease("db")
	rel.Connections["cache"] = config.Connection{Service: "db-standby", Port: 6379, Consumers: []string{"web"}}
	if _, err = derivePromotion(two, map[string]*release.Release{"a": rel}, nil, "", ""); err == nil || !strings.Contains(err.Error(), "several Connections") {
		t.Fatal(err)
	}
	if got, err = derivePromotion(two, map[string]*release.Release{"a": rel}, nil, "primary", ""); err != nil || got.From != "db" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestParseBytes(t *testing.T) {
	for in, want := range map[string]uint64{"16MiB": 16 << 20, "512KiB": 512 << 10, "1GiB": 1 << 30, "4096": 4096, "10B": 10} {
		if got, err := parseBytes(in); err != nil || got != want {
			t.Errorf("%s: %d %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "-1", "1TB", "MiB"} {
		if _, err := parseBytes(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestPostgresUserAndVolumes(t *testing.T) {
	user := "app"
	svc := types.ServiceConfig{ContainerSpec: types.ContainerSpec{Environment: types.MappingWithEquals{"POSTGRES_USER": &user}}}
	if postgresUser("", svc) != "app" || postgresUser("admin", svc) != "admin" || postgresUser("", types.ServiceConfig{}) != "postgres" {
		t.Fatal("user default")
	}
	p := &types.Project{
		Services: types.Services{"db": {Name: "db", ContainerSpec: types.ContainerSpec{Volumes: []types.ServiceVolumeConfig{{Type: types.VolumeTypeVolume, Source: "pgdata", Target: "/var/lib/postgresql/data"}, {Type: types.VolumeTypeBind, Source: "./x", Target: "/x"}}}}},
		Volumes:  types.Volumes{"pgdata": {}},
	}
	if got := serviceVolumeNames(p, "yoho-pg-production", "db"); len(got) != 1 || got[0] != "yoho-pg-production_pgdata" {
		t.Fatal(got)
	}
}

type runningHost struct {
	remote.Host
	ids string
}

func (h runningHost) Output(context.Context, remote.Cmd) (string, error) { return h.ids, nil }

func TestCheckPromotionRefusals(t *testing.T) {
	ext := map[string]config.ServiceExt{"db": {Stateful: true}, "db-standby": {Stateful: true}}
	proj := &types.Project{Services: types.Services{"db": {Name: "db"}, "db-standby": {Name: "db-standby"}, "web": {Name: "web"}}}
	target := promoteTarget{Connection: "primary", From: "db", To: "db-standby", FromServer: "a", ToServer: "b"}
	check := func(dest config.Destination, ext map[string]config.ServiceExt, o promoteOptions, running string) error {
		a := &app{cfg: &config.Config{App: "pg"}, destName: "production", dest: dest, ui: ui.New(io.Discard, ui.Human, false)}
		s := &session{r: &composefile.Result{Project: proj, Ext: ext}, hosts: []plan.NamedHost{{Name: "a", Host: runningHost{ids: running}}, {Name: "b", Host: runningHost{}}}}
		return a.checkPromotion(context.Background(), s, target, o)
	}
	if err := check(pgDest("db-standby", "web", "db"), ext, promoteOptions{}, ""); err != nil {
		t.Fatal(err)
	}
	if err := check(pgDest("db-standby", "web"), ext, promoteOptions{}, ""); err == nil || !strings.Contains(err.Error(), "never as a second primary") {
		t.Fatal(err)
	}
	if err := check(pgDest("db-standby", "web", "db"), map[string]config.ServiceExt{"db": {Stateful: true}}, promoteOptions{}, ""); err == nil || !strings.Contains(err.Error(), "x-yoho.stateful") {
		t.Fatal(err)
	}
	gone := pgDest("db-standby", "web", "db")
	gone.Servers = []string{"b"}
	if err := check(gone, ext, promoteOptions{}, ""); err == nil || !strings.Contains(err.Error(), "power it off") {
		t.Fatal(err)
	}
	if err := check(gone, ext, promoteOptions{OldPrimaryDown: true}, ""); err != nil {
		t.Fatal(err)
	}
	if err := check(pgDest("db-standby", "web", "db"), ext, promoteOptions{OldPrimaryDown: true}, "abc123"); err == nil || !strings.Contains(err.Error(), "two primaries") {
		t.Fatal(err)
	}
	if err := check(pgDest("db-standby", "web", "db"), ext, promoteOptions{OldPrimaryDown: true}, ""); err != nil {
		t.Fatal(err)
	}
}

func TestDbPromoteRefusesWithoutRoles(t *testing.T) {
	a := &app{cfg: &config.Config{App: "pg"}, destName: "production", dest: config.Destination{Servers: []string{"a"}}, ui: ui.New(io.Discard, ui.Human, false)}
	err := a.dbPromote(context.Background(), "", promoteOptions{MaxLag: defaultMaxLag})
	if err == nil || mutationCode(err, false) != 10 || !strings.Contains(err.Error(), "Roles") {
		t.Fatal(err)
	}
	a.dest = pgDest("db-standby", "web", "db")
	a.dest.Runtime = "swarm"
	if err = a.dbPromote(context.Background(), "", promoteOptions{MaxLag: defaultMaxLag}); err == nil || !strings.Contains(err.Error(), "swarm") {
		t.Fatal(err)
	}
	a.dest.Runtime = ""
	if err = a.dbPromote(context.Background(), "", promoteOptions{MaxLag: "lots"}); err == nil || !strings.Contains(err.Error(), "byte size") {
		t.Fatal(err)
	}
}
