package cli

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/ui"
)

func TestRolesBackupAndProxyOwnership(t *testing.T) {
	a := &app{dest: config.Destination{Servers: []string{"a", "b"}, Roles: map[string]config.Role{"app": {Services: []string{"web"}, Servers: []string{"a"}}, "data": {Services: []string{"db"}, Servers: []string{"b"}}}}}
	hosts := []plan.NamedHost{{Name: "a"}, {Name: "b"}}
	owner, e := a.backupOwner(hosts, &backupJob{services: map[string]config.ServiceBackup{"db": {}}})
	if e != nil || owner.Name != "b" {
		t.Fatal(owner, e)
	}
	if _, e = a.backupOwner(hosts, &backupJob{services: map[string]config.ServiceBackup{"db": {}, "web": {}}}); e == nil {
		t.Fatal("job spanning data owners accepted")
	}
	ext := map[string]config.ServiceExt{"web": {Proxy: &config.ServiceProxy{}}, "db": {Stateful: true}}
	if !a.serverHasProxy("a", ext) || a.serverHasProxy("b", ext) {
		t.Fatal("Proxy or connector selected for wrong Server")
	}
	a.dest.Roles = nil
	owner, e = a.backupOwner(hosts, &backupJob{})
	if e != nil || owner.Name != "a" || !a.serverHasProxy("b", nil) {
		t.Fatal("legacy ownership changed")
	}
}

type rolesLockHost struct {
	remote.Host
	name   string
	events *[]string
	refuse bool
}

func (h *rolesLockHost) Name() string { return h.name }
func (h *rolesLockHost) Output(context.Context, remote.Cmd) (string, error) {
	*h.events = append(*h.events, "lock "+h.name)
	if h.refuse {
		return "held", nil
	}
	return "acquired", nil
}
func (h *rolesLockHost) Run(context.Context, remote.Cmd) error {
	*h.events = append(*h.events, "unlock "+h.name)
	return nil
}
func TestRolesApplyLockOrderAndRefusal(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(map[bool]string{false: "all", true: "refused"}[refuse], func(t *testing.T) {
			var events []string
			a := &app{cfg: &config.Config{App: "shop"}, destName: "production", dest: config.Destination{Roles: map[string]config.Role{}}, ui: ui.New(io.Discard, ui.Human, false)}
			hosts := []plan.NamedHost{{Name: "b", Host: &rolesLockHost{name: "b", events: &events, refuse: refuse}}, {Name: "a", Host: &rolesLockHost{name: "a", events: &events}}}
			unlock, e := a.deployLocks(context.Background(), hosts, "v1")
			if refuse {
				var held *deploy.LockedError
				if !errors.As(e, &held) || mutationCode(e, false) != 11 {
					t.Fatal(e)
				}
				if !reflect.DeepEqual(events, []string{"lock a", "lock b", "unlock a"}) {
					t.Fatal(events)
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				if e = unlock(context.Background()); e != nil {
					t.Fatal(e)
				}
				if !reflect.DeepEqual(events, []string{"lock a", "lock b", "unlock b", "unlock a"}) {
					t.Fatal(events)
				}
			}
		})
	}
}
func TestRolesOutcomeKeepsServerIdentity(t *testing.T) {
	failure := &deploy.RolesFailure{Err: errors.New("provider failed"), Changed: []string{"a/web"}, Unchanged: []string{"b/db"}}
	r := mutationResult(failure, true)
	if r.Outcome != "partial" || mutationCode(failure, true) != 13 || !reflect.DeepEqual(r.Changed, []string{"a/web"}) {
		t.Fatal(r)
	}
	failure.Err = context.Canceled
	r = mutationResult(failure, true)
	if r.Outcome != "unknown" || mutationCode(failure, true) != 14 {
		t.Fatal(r)
	}
}
