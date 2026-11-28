package cli

import (
	"context"
	"errors"
	"fmt"
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
	// A promoted journal with a matching Release resumes at apply (Done);
	// an unfinished one resumes from its journal.
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
	user1 := func(flag string, svc types.ServiceConfig) string {
		u, _ := postgresUser(flag, svc, config.ServiceExt{}, "a", config.Server{})
		return u
	}
	if user1("", svc) != "app" || user1("admin", svc) != "admin" || user1("", types.ServiceConfig{}) != "postgres" {
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
	// --old-primary-down needs the old Server out of the Destination ...
	if err := check(pgDest("db-standby", "web", "db"), ext, promoteOptions{OldPrimaryDown: true}, ""); err == nil || !strings.Contains(err.Error(), "still in the Destination") {
		t.Fatal(err)
	}
	// ... except to accept the loss after an unclean fence, with nothing running.
	target.Unclean = true
	if err := check(pgDest("db-standby", "web", "db"), ext, promoteOptions{OldPrimaryDown: true}, "abc123"); err == nil || !strings.Contains(err.Error(), "two primaries") {
		t.Fatal(err)
	}
	if err := check(pgDest("db-standby", "web", "db"), ext, promoteOptions{OldPrimaryDown: true}, ""); err != nil {
		t.Fatal(err)
	}
}

// postgresUser reads POSTGRES_USER from the Server's tag environment; a
// secret-sourced one is never read, only where the container has it.
func TestPostgresUserFromTagsAndSecrets(t *testing.T) {
	compose := "app"
	svc := types.ServiceConfig{ContainerSpec: types.ContainerSpec{Environment: types.MappingWithEquals{"POSTGRES_USER": &compose}}}
	ext := config.ServiceExt{Tags: map[string]config.ServiceTag{"eu": {Env: map[string]string{"POSTGRES_USER": "eu_admin"}}}}
	if u, sec := postgresUser("", svc, ext, "a", config.Server{Tags: []string{"eu"}}); u != "eu_admin" || sec != "" {
		t.Fatal(u)
	}
	if u, _ := postgresUser("", svc, ext, "b", config.Server{}); u != "app" {
		t.Fatal(u)
	}
	ext = config.ServiceExt{Secrets: config.SecretRefs{{Name: "POSTGRES_USER", Key: "PG_USER"}}}
	if u, sec := postgresUser("", types.ServiceConfig{}, ext, "a", config.Server{}); u != "" || sec != pgpromote.UserFromFile {
		t.Fatal(u, sec)
	}
	ext.SecretsAsEnv = true
	if u, sec := postgresUser("", types.ServiceConfig{}, ext, "a", config.Server{}); u != "" || sec != pgpromote.UserFromEnv {
		t.Fatal(u, sec)
	}
	if u, sec := postgresUser("ops", types.ServiceConfig{}, ext, "a", config.Server{}); u != "ops" || sec != "" {
		t.Fatal(u)
	}
}

// The socket/pg_hba hint is for psql failures only.
func TestPreflightHintOnlyForPsqlErrors(t *testing.T) {
	if preflightHint(errors.New("db-standby is 96 bytes behind db")) != "" {
		t.Fatal("hint for a lag error")
	}
	if !strings.Contains(preflightHint(fmt.Errorf("check: %w", &pgpromote.PsqlError{Err: errors.New("FATAL: role")})), "pg_hba") {
		t.Fatal("no hint for a psql error")
	}
}

// The documented lost-Server flow: Server a removed from the Destination,
// db removed from compose and from the consumers, --old-primary-down. The
// derivation and the checks let it through.
func TestDerivePromotionLostServer(t *testing.T) {
	dest := config.Destination{
		Servers: []string{"b"},
		Roles: map[string]config.Role{
			"app":     {Services: []string{"web"}, Servers: []string{"b"}},
			"standby": {Services: []string{"db-standby"}, Servers: []string{"b"}},
		},
		Connections: map[string]config.Connection{"primary": {Service: "db-standby", Port: 5432, Consumers: []string{"web"}}},
	}
	rels := map[string]*release.Release{"b": pgRelease("db")}
	got, err := derivePromotion(dest, rels, nil, "", "")
	if err != nil || got.From != "db" || got.To != "db-standby" || got.FromServer != "a" || got.ToServer != "b" {
		t.Fatalf("%+v %v", got, err)
	}
	ext := map[string]config.ServiceExt{"db-standby": {Stateful: true}}
	proj := &types.Project{Services: types.Services{"db-standby": {Name: "db-standby"}, "web": {Name: "web"}}}
	a := &app{cfg: &config.Config{App: "pg"}, destName: "production", dest: dest, ui: ui.New(io.Discard, ui.Human, false)}
	s := &session{r: &composefile.Result{Project: proj, Ext: ext}, hosts: []plan.NamedHost{{Name: "b", Host: runningHost{}}}}
	if err := a.checkPromotion(context.Background(), s, got, promoteOptions{OldPrimaryDown: true}); err != nil {
		t.Fatal(err)
	}
	if err := a.checkPromotion(context.Background(), s, got, promoteOptions{}); err == nil || !strings.Contains(err.Error(), "power it off") {
		t.Fatal(err)
	}
	// db still in compose but no longer placed: not required as a consumer.
	proj.Services["db"] = types.ServiceConfig{Name: "db"}
	if err := a.checkPromotion(context.Background(), s, got, promoteOptions{OldPrimaryDown: true}); err != nil {
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

// S7: a lost-Server promotion whose new Release was recorded before a later
// apply step failed resumes from its promoted journal: neither the Release
// nor the config places the old primary any more.
func TestDerivePromotionResumesLostServerFromJournal(t *testing.T) {
	dest := config.Destination{
		Servers: []string{"b"},
		Roles: map[string]config.Role{
			"app":     {Services: []string{"web"}, Servers: []string{"b"}},
			"standby": {Services: []string{"db-standby"}, Servers: []string{"b"}},
		},
		Connections: map[string]config.Connection{"primary": {Service: "db-standby", Port: 5432, Consumers: []string{"web"}}},
	}
	rels := map[string]*release.Release{"b": {Roles: dest.Roles, Connections: dest.Connections}}
	j := &pgpromote.Journal{Connection: "primary", From: "db", To: "db-standby", FromServer: "a", ToServer: "b", State: "promoted"}
	got, err := derivePromotion(dest, rels, map[string]*pgpromote.Journal{"primary": j}, "", "")
	if err != nil || got.From != "db" || got.To != "db-standby" || got.FromServer != "a" || !got.Done {
		t.Fatalf("%+v %v", got, err)
	}
}
