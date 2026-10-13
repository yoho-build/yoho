package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/yoho-build/yoho/internal/config"
)

func restoreFixtureJSON(runtime, svc, status string, replicas int) string {
	o := restoreObject{ID: "cid123", Service: svc, Status: status, HasHealthcheck: true, Labels: map[string]string{
		"yoho.app": "shop", "yoho.destination": "production", "com.docker.compose.project": "yoho-shop-production",
	}}
	if runtime == "swarm" {
		o.ID = "yoho-shop-production_" + svc
		o.Replicas = &replicas
		o.Constraints = []string{"node.hostname == node-1"}
		o.Image = "postgres:17"
		o.HealthcheckMode = "enabled"
		o.Labels["com.docker.stack.namespace"] = "yoho-shop-production"
	} else if svc != "db" {
		o.ID = "cid-" + svc
	}
	b, _ := json.Marshal(o)
	return string(b)
}

type restoreRig struct {
	h       *fakeHost
	o       RestoreOptions
	running map[string]int
	objects map[string]string
	after   func(context.Context, string) error
}

func newRestoreRig(t *testing.T, runtime string, dumpOnly bool) *restoreRig {
	t.Helper()
	r := &restoreRig{running: map[string]int{"db": 1, "web": 2, "worker": 1, "idle": 0}, objects: map[string]string{}}
	for svc, n := range r.running {
		status := "running"
		if n == 0 {
			status = "exited"
		}
		r.objects[svc] = restoreFixtureJSON(runtime, svc, status, n)
	}
	r.h = &fakeHost{}
	base := dockerResponder(nil)
	r.h.respond = func(ctx context.Context, s string) (string, error) {
		switch {
		case strings.HasPrefix(s, "set -eu; docker ps -aq --filter"), strings.HasPrefix(s, "set -eu; docker service ls -q"):
			return strings.Join(sortedKeys(r.objects), " "), nil
		case strings.HasPrefix(s, "set -eu; docker inspect -f"), strings.HasPrefix(s, "set -eu; docker service inspect -f"):
			for _, svc := range sortedKeys(r.objects) {
				if strings.HasSuffix(s, " '"+svc+"'") {
					return r.objects[svc], nil
				}
			}
		}
		for svc := range r.running {
			id := "cid-" + svc
			if svc == "db" {
				id = "cid123"
			}
			switch {
			case strings.Contains(s, "docker stop '"+id+"'"):
				r.running[svc] = 0
			case strings.Contains(s, "docker start '"+id+"'"):
				r.running[svc] = 1
			case strings.Contains(s, "docker service scale 'yoho-shop-production_"+svc+"="):
				v := strings.Split(s, "docker service scale 'yoho-shop-production_"+svc+"=")[1]
				n, _ := strconv.Atoi(strings.Split(v, "'")[0])
				r.running[svc] = n
			}
		}
		if r.after != nil {
			if err := r.after(ctx, s); err != nil {
				return "", err
			}
		}
		return base(ctx, s)
	}
	ms := ManifestService{Dump: "db.dump"}
	if !dumpOnly {
		ms.Volumes = map[string]string{"pgdata": "db-pgdata.tar.gz"}
	}
	m, _ := json.Marshal(Manifest{App: "shop", Destination: "production", Services: map[string]ManifestService{"db": ms}})
	r.h.files = map[string][]byte{"/var/lib/yoho/backups/shop/production/restore-20261008T030000Z/data/manifest.json": m}
	plan := `[{"name":"db"},{"name":"web"},{"name":"worker"},{"name":"idle"}]`
	if runtime == "swarm" {
		plan = `{"services":` + plan + `}`
	}
	r.h.files["/var/lib/yoho/apps/shop/production/current/plan.json"] = []byte(plan)
	r.o = RestoreOptions{
		App: "shop", Destination: "production", Host: r.h, Confirm: true, Runtime: runtime,
		ID: "yoho-shop-production-20261001T030000Z.tar.gz", Target: config.BackupTarget{Type: "archive", Repository: "/srv/b"},
		Services: map[string]config.ServiceBackup{"db": {RestoreDump: []string{"psql"}, PostRestore: []string{"/post"}}},
		Now:      func() time.Time { return time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC) },
	}
	return r
}

func assertRestoreStopped(t *testing.T, r *restoreRig) {
	t.Helper()
	for svc, n := range r.running {
		if n != 0 {
			t.Errorf("%s still running (%d)\n%s", svc, n, r.h.scripts())
		}
	}
}

func TestRestoreFailClosed(t *testing.T) {
	for _, runtime := range []string{"compose", "swarm"} {
		for _, dumpOnly := range []bool{false, true} {
			for _, phase := range []string{"stop", "volume", "start", "ready", "dump", "service-hook", "app-hook", "resume"} {
				if dumpOnly && phase == "volume" {
					continue
				}
				for _, cancelInstead := range []bool{false, true} {
					t.Run(runtime+"/dumpOnly="+strconv.FormatBool(dumpOnly)+"/"+phase+"/cancel="+strconv.FormatBool(cancelInstead), func(t *testing.T) {
						r := newRestoreRig(t, runtime, dumpOnly)
						ctx, cancel := context.WithCancel(context.Background())
						defer cancel()
						failed := false
						failure := func() error {
							failed = true
							if cancelInstead {
								cancel()
								return context.Canceled
							}
							return errors.New("injected failure")
						}
						r.after = func(ctx context.Context, s string) error {
							if failed {
								if ctx.Err() != nil {
									t.Error("cleanup used cancelled context")
								}
								return nil
							}
							isStart := strings.Contains(s, "docker start 'cid123'") || strings.Contains(s, "docker service scale 'yoho-shop-production_db=1'")
							isResume := strings.Contains(s, "docker start 'cid-web'") || strings.Contains(s, "docker service scale 'yoho-shop-production_web=2'")
							isStop := strings.Contains(s, "docker stop 'cid123'") || strings.Contains(s, "docker service scale 'yoho-shop-production_db=0'")
							hit := phase == "stop" && isStop || phase == "volume" && strings.Contains(s, "-exec rm -rf {} +") || phase == "start" && isStart || phase == "ready" && strings.Contains(s, "State.Health") || phase == "dump" && strings.Contains(s, "docker exec -i") || phase == "service-hook" && strings.Contains(s, "'/post'") || phase == "resume" && isResume
							if hit {
								return failure()
							}
							return nil
						}
						if phase == "app-hook" {
							r.o.AfterRestore = func(context.Context) error { return failure() }
						}
						_, err := Restore(ctx, r.o)
						if err == nil || !failed || !strings.Contains(err.Error(), "Services left stopped") || !strings.Contains(err.Error(), "no data rollback") {
							t.Fatalf("expected isolated failure, got %v (injected=%v)", err, failed)
						}
						if cancelInstead && !errors.Is(err, context.Canceled) {
							t.Fatalf("cancellation cause lost: %v", err)
						}
						assertRestoreStopped(t, r)
						if r.h.index("rm -rf '/var/lib/yoho/backups") >= 0 {
							t.Fatal("failed restore deleted the staged Backup")
						}
						if r.h.files["/var/lib/yoho/backups/shop/production/restore-20261008T030000Z/prior-state.json"] == nil {
							t.Fatal("prior state not retained for recovery")
						}
					})
				}
			}
		}
	}
}

func TestRestoreConsumersResumeOnlyOnSuccess(t *testing.T) {
	for _, runtime := range []string{"compose", "swarm"} {
		for _, priorStopped := range []bool{false, true} {
			t.Run(runtime+"/targetStopped="+strconv.FormatBool(priorStopped), func(t *testing.T) {
				r := newRestoreRig(t, runtime, true)
				if priorStopped {
					r.running["db"] = 0
					r.objects["db"] = restoreFixtureJSON(runtime, "db", "exited", 0)
				}
				r.after = func(_ context.Context, s string) error {
					if strings.Contains(s, "docker exec -i") || strings.Contains(s, "'/post'") {
						if r.running["db"] != 1 || r.running["web"] != 0 || r.running["worker"] != 0 || r.running["idle"] != 0 {
							t.Fatalf("consumers active during data operations: %v", r.running)
						}
					}
					return nil
				}
				r.o.AfterRestore = func(context.Context) error {
					if r.running["web"] != 0 || r.running["worker"] != 0 {
						t.Fatal("post-restore Hook ran after consumers resumed")
					}
					return nil
				}
				if _, err := Restore(context.Background(), r.o); err != nil {
					t.Fatal(err)
				}
				if r.running["idle"] != 0 || (priorStopped && r.running["db"] != 0) || (!priorStopped && r.running["db"] != 1) || r.running["worker"] != 1 || r.running["web"] == 0 {
					t.Fatalf("prior running state not preserved: %v", r.running)
				}
				if runtime == "swarm" && r.running["web"] != 2 {
					t.Fatal("Swarm replica count lost")
				}
				for _, c := range r.h.cmds {
					if !strings.HasPrefix(c.Script, "#") {
						if out, err := exec.Command("sh", "-n", "-c", c.Script).CombinedOutput(); err != nil {
							t.Fatalf("invalid POSIX script: %v %s\n%s", err, out, c.Script)
						}
					}
				}
			})
		}
	}
}

func TestRestorePreflightRefusesUnsafeTargets(t *testing.T) {
	for _, runtime := range []string{"compose", "swarm"} {
		for _, unsafe := range []string{"owner", "unlabelled", "published", "shared-network", "proxy-route", "global"} {
			if runtime != "swarm" && unsafe == "global" {
				continue
			}
			t.Run(runtime+"/"+unsafe, func(t *testing.T) {
				r := newRestoreRig(t, runtime, false)
				var o restoreObject
				_ = json.Unmarshal([]byte(r.objects["db"]), &o)
				switch unsafe {
				case "owner":
					o.Labels["yoho.app"] = "other"
				case "unlabelled":
					delete(o.Labels, "yoho.app")
				case "published":
					o.Ports = map[string]json.RawMessage{"5432/tcp": json.RawMessage(`[{"HostPort":"5432"}]`)}
					o.Published = []json.RawMessage{json.RawMessage(`{"PublishedPort":5432}`)}
				case "shared-network":
					o.Networks = map[string]json.RawMessage{"yoho": json.RawMessage(`{}`)}
				case "proxy-route":
					respond := r.h.respond
					r.h.respond = func(ctx context.Context, s string) (string, error) {
						if strings.Contains(s, "kamal-proxy list --json") {
							return `{"shop-production-db":{"targets":["db:5432"],"state":"deployed"}}`, nil
						}
						return respond(ctx, s)
					}
				case "global":
					o.Replicas = nil
				}
				b, _ := json.Marshal(o)
				r.objects["db"] = string(b)
				_, err := Restore(context.Background(), r.o)
				if err == nil || !strings.Contains(err.Error(), "restore isolation") {
					t.Fatalf("unsafe target not refused: %v", err)
				}
				if r.h.index("docker stop") >= 0 || r.h.index("docker service scale") >= 0 || r.h.index("-exec rm -rf {} +") >= 0 {
					t.Fatalf("unsafe restore mutated runtime/data:\n%s", r.h.scripts())
				}
				if strings.Contains(r.h.scripts(), "kamal-proxy remove") {
					t.Fatal("restore must not remove routes")
				}
			})
		}
	}
}

func TestRestoreSeparatesDumpAndHookStartup(t *testing.T) {
	for _, runtime := range []string{"compose", "swarm"} {
		for _, hookIsDumpPrerequisite := range []bool{false, true} {
			t.Run(runtime+"/hookPrerequisite="+strconv.FormatBool(hookIsDumpPrerequisite), func(t *testing.T) {
				r := newRestoreRig(t, runtime, true)
				r.running["worker"] = 0
				r.objects["worker"] = restoreFixtureJSON(runtime, "worker", "exited", 0)
				r.o.Services = map[string]config.ServiceBackup{
					"db":     {RestoreDump: []string{"psql"}},
					"worker": {PostRestore: []string{"/post-worker"}},
				}
				m, _ := json.Marshal(Manifest{App: "shop", Destination: "production", Services: map[string]ManifestService{
					"db": {Dump: "db.dump"}, "worker": {},
				}})
				r.h.files["/var/lib/yoho/backups/shop/production/restore-20261008T030000Z/data/manifest.json"] = m
				if hookIsDumpPrerequisite {
					p := `[{"name":"db","depends_on":["worker"],"dependency_conditions":{"worker":"service_started"}},{"name":"worker"}]`
					if runtime == "swarm" {
						p = `{"services":` + p + `}`
					}
					r.h.files["/var/lib/yoho/apps/shop/production/current/plan.json"] = []byte(p)
					r.h.files["/var/lib/yoho/apps/shop/production/current/compose.yaml"] = []byte("services:\n  db:\n    depends_on: [worker]\n  worker: {}\n")
				}
				r.after = func(_ context.Context, s string) error {
					if strings.Contains(s, "docker exec -i") && r.running["worker"] != 0 {
						t.Fatal("hook target's default worker process ran while dump loaded")
					}
					return nil
				}
				_, err := Restore(context.Background(), r.o)
				if hookIsDumpPrerequisite {
					if err == nil || !strings.Contains(err.Error(), "hook-only target") || r.h.index("docker stop") >= 0 || r.h.index("docker service scale") >= 0 {
						t.Fatalf("unsafe hook prerequisite was not refused before mutation: %v\n%s", err, r.h.scripts())
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				start := r.h.index("docker start 'cid-worker'")
				if runtime == "swarm" {
					start = r.h.index("docker service scale 'yoho-shop-production_worker=1'")
				}
				load, post := r.h.index("docker exec -i"), r.h.index("'/post-worker'")
				if !(load >= 0 && load < start && start < post) {
					t.Fatalf("dump/hook startup ordering violated: %d %d %d\n%s", load, start, post, r.h.scripts())
				}
				if r.running["worker"] != 0 {
					t.Fatal("original zero/stopped hook target was not preserved")
				}
			})
		}
	}
}

func TestRestoreLoadsPrerequisiteDumpBeforeDependentStartup(t *testing.T) {
	for _, runtime := range []string{"compose", "swarm"} {
		t.Run(runtime, func(t *testing.T) {
			r := newRestoreRig(t, runtime, true)
			r.running["api"] = 1
			r.objects["api"] = restoreFixtureJSON(runtime, "api", "running", 1)
			r.o.Services = map[string]config.ServiceBackup{
				"api": {RestoreDump: []string{"api-load"}},
				"db":  {RestoreDump: []string{"psql"}},
			}
			manifest, _ := json.Marshal(Manifest{App: "shop", Destination: "production", Services: map[string]ManifestService{
				"api": {Dump: "api.dump"}, "db": {Dump: "db.dump"},
			}})
			r.h.files["/var/lib/yoho/backups/shop/production/restore-20261008T030000Z/data/manifest.json"] = manifest
			plan := `[{"name":"api","depends_on":["db"],"dependency_conditions":{"db":"service_healthy"}},{"name":"db"}]`
			if runtime == "swarm" {
				plan = `{"services":` + plan + `}`
			}
			r.h.files["/var/lib/yoho/apps/shop/production/current/plan.json"] = []byte(plan)
			r.h.files["/var/lib/yoho/apps/shop/production/current/compose.yaml"] = []byte("services:\n  api:\n    depends_on:\n      db:\n        condition: service_healthy\n  db: {}\n")
			r.after = func(_ context.Context, s string) error {
				if strings.Contains(s, "docker exec -i") && strings.Contains(s, "'psql'") && r.running["api"] != 0 {
					t.Fatal("dependent normal process started before its prerequisite dump completed")
				}
				return nil
			}
			if _, err := Restore(context.Background(), r.o); err != nil {
				t.Fatal(err)
			}
			loaded, started := r.h.index("'psql'"), r.h.index("docker start 'cid-api'")
			if runtime == "swarm" {
				started = r.h.index("docker service scale 'yoho-shop-production_api=1'")
			}
			if loaded < 0 || started <= loaded {
				t.Fatalf("prerequisite dump was not loaded before dependent startup: %s", r.h.scripts())
			}
		})
	}
}

func TestRestoreUsesSwarmSchedulingHostname(t *testing.T) {
	r := newRestoreRig(t, "swarm", true)
	respond := r.h.respond
	r.h.respond = func(ctx context.Context, s string) (string, error) {
		if strings.Contains(s, "docker node inspect") {
			if !strings.Contains(s, ".Description.Hostname") {
				t.Fatal("placement identity did not use the Swarm scheduling hostname")
			}
			return "manager-node|actual-manager", nil
		}
		return respond(ctx, s)
	}
	_, err := Restore(context.Background(), r.o)
	if err == nil || !strings.Contains(err.Error(), "not pinned") || r.h.index("docker service scale") >= 0 {
		t.Fatalf("daemon-name pin accepted instead of actual scheduling hostname: %v", err)
	}
}

func TestRestoreSwarmUnknownTaskStatesRemainActive(t *testing.T) {
	h := &fakeHost{}
	if err := waitServiceReplicas(context.Background(), h, "service", 0); err != nil {
		t.Fatal(err)
	}
	var activeLine string
	for _, line := range strings.Split(h.cmds[0].Script, "\n") {
		if strings.Contains(line, "active=$(") {
			activeLine = line
		}
	}
	if activeLine == "" {
		t.Fatal("task activity classifier missing")
	}
	for states, want := range map[string]string{
		"": "0", "Shutdown 1 second ago": "0", "Complete 1 second ago": "0",
		"Failed 1 second ago": "0", "Rejected 1 second ago": "0",
		"Orphaned 1 second ago": "1", "Remove 1 second ago": "1",
		"Running 1 second ago": "1", "Unknown 1 second ago": "1",
		"Shutdown 1 second ago\nOrphaned 2 seconds ago": "1",
	} {
		out, err := exec.Command("sh", "-c", "states=$1\n"+activeLine+"\nprintf '%s' \"$active\"", "sh", states).CombinedOutput()
		if err != nil || string(out) != want {
			t.Errorf("active(%q) = %q, %v; want %s", states, out, err, want)
		}
	}
}

func TestRestoreSwarmRejectsUnpinnedTemporaryTargets(t *testing.T) {
	for _, constraints := range [][]string{nil, {"node.hostname == worker-1"}, {"node.role == worker"}} {
		r := newRestoreRig(t, "swarm", false)
		var o restoreObject
		_ = json.Unmarshal([]byte(r.objects["db"]), &o)
		o.Constraints = constraints
		b, _ := json.Marshal(o)
		r.objects["db"] = string(b)
		_, err := Restore(context.Background(), r.o)
		if err == nil || !strings.Contains(err.Error(), "not pinned") || r.h.index("docker service scale") >= 0 || r.h.index("-exec rm -rf {} +") >= 0 {
			t.Fatalf("unsafe Swarm placement not refused before mutation: %v\n%s", err, r.h.scripts())
		}
	}
}

func TestRestoreSwarmForeignVolumeSpecs(t *testing.T) {
	for _, replicas := range []int{0, 1} {
		t.Run(strconv.Itoa(replicas), func(t *testing.T) {
			r := newRestoreRig(t, "swarm", false)
			respond := r.h.respond
			r.h.respond = func(ctx context.Context, s string) (string, error) {
				if s == "set -eu; docker service ls -q" {
					return "foreign", nil
				}
				if strings.Contains(s, "docker service inspect -f") && strings.HasSuffix(s, " 'foreign'") {
					return `{"ID":"other_db","Replicas":` + strconv.Itoa(replicas) + `,"Labels":{"yoho.app":"other","yoho.destination":"production","com.docker.stack.namespace":"other"},"Mounts":[{"Type":"volume","Source":"yoho-shop-production_pgdata"}]}`, nil
				}
				return respond(ctx, s)
			}
			_, err := Restore(context.Background(), r.o)
			if err == nil || !strings.Contains(err.Error(), "foreign Swarm Service") || r.h.index("docker service scale") >= 0 || r.h.index("-exec rm -rf {} +") >= 0 {
				t.Fatalf("foreign cluster mount not refused before mutation: %v\n%s", err, r.h.scripts())
			}
		})
	}
}

func TestRestoreStopBroadcastPrecedesConvergence(t *testing.T) {
	oldAttempt, oldWait := stopAttemptTimeout, stopWaitTimeout
	stopAttemptTimeout, stopWaitTimeout = 5*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { stopAttemptTimeout, stopWaitTimeout = oldAttempt, oldWait })
	for _, runtime := range []string{"compose", "swarm"} {
		t.Run(runtime, func(t *testing.T) {
			h := &fakeHost{respond: func(ctx context.Context, s string) (string, error) {
				if strings.Contains(s, "'first=0'") || strings.Contains(s, "docker stop 'first'") || (strings.Contains(s, "docker service ps") && strings.Contains(s, "'first'")) {
					<-ctx.Done()
					return "", ctx.Err()
				}
				return "", nil
			}}
			s := &restoreState{Runtime: runtime, Objects: []restoreObject{{ID: "first"}, {ID: "second"}, {ID: "third"}}}
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := s.stopBounded(ctx, h); err == nil {
				t.Fatal("slow first stop should fail")
			}
			second, third := h.index("docker stop 'second'"), h.index("docker stop 'third'")
			if runtime == "swarm" {
				second, third = h.index("docker service scale 'second=0'"), h.index("docker service scale 'third=0'")
				if wait := h.index("docker service ps"); wait <= third {
					t.Fatalf("wait started before all scale-zero requests: %s", h.scripts())
				}
			}
			if second < 0 || third < second || ctx.Err() != nil {
				t.Fatalf("slow first stop starved later requests: %s", h.scripts())
			}
		})
	}
}

func TestRestoreInitialStopPreservesGrace(t *testing.T) {
	oldAttempt, oldWait := stopAttemptTimeout, stopWaitTimeout
	stopAttemptTimeout, stopWaitTimeout = 5*time.Millisecond, 5*time.Millisecond
	t.Cleanup(func() { stopAttemptTimeout, stopWaitTimeout = oldAttempt, oldWait })
	for _, runtime := range []string{"compose", "swarm"} {
		t.Run(runtime, func(t *testing.T) {
			h := &fakeHost{respond: func(ctx context.Context, s string) (string, error) {
				if strings.Contains(s, "docker stop") || strings.Contains(s, "docker service ps") {
					select {
					case <-time.After(20 * time.Millisecond):
					case <-ctx.Done():
						return "", ctx.Err()
					}
				}
				return "", nil
			}}
			s := &restoreState{Runtime: runtime, Objects: []restoreObject{{ID: "slow-graceful"}}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := s.stop(ctx, h); err != nil {
				t.Fatalf("cleanup budget truncated initial graceful shutdown: %v", err)
			}
		})
	}
}

func TestRestoreSwarmEffectiveHealthchecks(t *testing.T) {
	for _, scenario := range []string{"image-inherited", "timing-only", "disabled", "image-disabled"} {
		t.Run(scenario, func(t *testing.T) {
			r := newRestoreRig(t, "swarm", true)
			var o restoreObject
			_ = json.Unmarshal([]byte(r.objects["db"]), &o)
			o.HasHealthcheck = false
			o.HealthcheckMode = "inherit"
			if scenario == "disabled" {
				o.HealthcheckMode = "disabled"
			}
			b, _ := json.Marshal(o)
			r.objects["db"] = string(b)
			respond := r.h.respond
			r.h.respond = func(ctx context.Context, s string) (string, error) {
				if strings.Contains(s, "docker image inspect -f") {
					if scenario == "image-disabled" {
						return "false", nil
					}
					return "true", nil
				}
				return respond(ctx, s)
			}
			_, err := snapshotRestoreState(context.Background(), r.h, "yoho-shop-production", "shop", "production", "swarm", []string{"db"}, []string{"db"}, map[string]bool{"db": true})
			if scenario == "disabled" || scenario == "image-disabled" {
				if err == nil || !strings.Contains(err.Error(), "no enabled healthcheck") {
					t.Fatalf("disabled healthcheck accepted: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if scenario == "disabled" && r.h.index("docker image inspect") >= 0 {
				t.Fatal("explicit NONE must not inherit image HEALTHCHECK")
			}
		})
	}
	// Exercise the actual Docker templates, including a timing-only override,
	// rather than relying solely on fixtures that already contain derived flags.
	for _, health := range []any{nil, map[string]any{"Interval": 1000}, map[string]any{"Test": []string{"NONE"}}, map[string]any{"Test": []string{"CMD", "private-test"}}} {
		root := map[string]any{"Spec": map[string]any{
			"Name": "yoho-shop-production_db", "Labels": map[string]string{"yoho.app": "shop", "yoho.destination": "production", "yoho.service": "db", "com.docker.stack.namespace": "yoho-shop-production"},
			"Mode": map[string]any{"Replicated": map[string]any{"Replicas": 1}}, "EndpointSpec": map[string]any{},
			"TaskTemplate": map[string]any{"ContainerSpec": map[string]any{"Image": "postgres:17", "Healthcheck": health}},
		}}
		tpl, err := template.New("docker").Funcs(template.FuncMap{"json": func(v any) string { b, _ := json.Marshal(v); return string(b) }}).Parse(restoreServiceFormat)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := tpl.Execute(&output, root); err != nil {
			t.Fatal(err)
		}
		var o restoreObject
		if err := json.Unmarshal(output.Bytes(), &o); err != nil {
			t.Fatalf("invalid Docker template JSON: %v %s", err, output.String())
		}
		want := "inherit"
		if h, ok := health.(map[string]any); ok {
			if tests, ok := h["Test"].([]string); ok {
				want = "enabled"
				if tests[0] == "NONE" {
					want = "disabled"
				}
			}
		}
		if o.HealthcheckMode != want || strings.Contains(output.String(), "private-test") {
			t.Fatalf("incorrect effective healthcheck metadata: %s", output.String())
		}
	}
}

func TestRestorePrivatePrerequisite(t *testing.T) {
	for _, runtime := range []string{"compose", "swarm"} {
		for _, scenario := range []string{"success", "exposed", "cycle", "completion", "missing-plan", "missing-condition", "no-healthcheck", "remote-placement"} {
			if runtime == "compose" && scenario == "remote-placement" {
				continue
			}
			t.Run(runtime+"/"+scenario, func(t *testing.T) {
				r := newRestoreRig(t, runtime, true)
				r.running["postgres"] = 0
				r.objects["postgres"] = restoreFixtureJSON(runtime, "postgres", "exited", 0)
				r.o.Services = map[string]config.ServiceBackup{
					"db":       {PostRestore: []string{"/post"}},
					"postgres": {Volumes: []string{"pgdata"}},
				}
				const base = "/var/lib/yoho/apps/shop/production/current/"
				const manifest = "/var/lib/yoho/backups/shop/production/restore-20261008T030000Z/data/manifest.json"
				r.h.files[manifest], _ = json.Marshal(Manifest{App: "shop", Destination: "production", Services: map[string]ManifestService{
					"db": {}, "postgres": {Volumes: map[string]string{"pgdata": "postgres-pgdata.tar.gz"}},
				}})
				condition := "service_healthy"
				if scenario == "completion" {
					condition = "service_completed_successfully"
				}
				plan := `[{"name":"db","depends_on":["postgres"],"dependency_conditions":{"postgres":"` + condition + `"}},{"name":"postgres"}]`
				compose := "services:\n  db:\n    depends_on:\n      postgres:\n        condition: " + condition + "\n  postgres: {}\n"
				if scenario == "cycle" {
					plan = `[{"name":"db","depends_on":["postgres"],"dependency_conditions":{"postgres":"service_started"}},{"name":"postgres","depends_on":["db"],"dependency_conditions":{"db":"service_started"}}]`
					compose = "services:\n  db:\n    depends_on: [postgres]\n  postgres:\n    depends_on: [db]\n"
				}
				if scenario == "missing-plan" {
					plan = `[{"name":"db","depends_on":["postgres"],"dependency_conditions":{"postgres":"service_started"}}]`
				}
				if scenario == "missing-condition" {
					plan = `[{"name":"db","depends_on":["postgres"]},{"name":"postgres"}]`
					compose = "services:\n  db: {}\n  postgres: {}\n"
				}
				if runtime == "swarm" {
					plan = `{"services":` + plan + `}`
				}
				r.h.files[base+"plan.json"] = []byte(plan)
				r.h.files[base+"compose.yaml"] = []byte(compose)
				if scenario == "exposed" {
					var o restoreObject
					_ = json.Unmarshal([]byte(r.objects["postgres"]), &o)
					o.Ports = map[string]json.RawMessage{"5432/tcp": json.RawMessage(`[{"HostPort":"5432"}]`)}
					o.Published = []json.RawMessage{json.RawMessage(`{"PublishedPort":5432}`)}
					b, _ := json.Marshal(o)
					r.objects["postgres"] = string(b)
				}
				if scenario == "remote-placement" {
					var o restoreObject
					_ = json.Unmarshal([]byte(r.objects["postgres"]), &o)
					o.Constraints = []string{"node.hostname == worker-1"}
					b, _ := json.Marshal(o)
					r.objects["postgres"] = string(b)
				}
				if scenario == "no-healthcheck" {
					var o restoreObject
					_ = json.Unmarshal([]byte(r.objects["postgres"]), &o)
					o.HasHealthcheck = false
					b, _ := json.Marshal(o)
					r.objects["postgres"] = string(b)
				}
				postChecked := false
				r.after = func(_ context.Context, s string) error {
					if strings.Contains(s, "'/post'") {
						postChecked = true
						if r.running["postgres"] != 1 || r.running["web"] != 0 || r.running["worker"] != 0 {
							t.Fatalf("prerequisite unavailable or consumers running during Hook: %v", r.running)
						}
					}
					return nil
				}
				_, err := Restore(context.Background(), r.o)
				if scenario != "success" {
					if err == nil || !strings.Contains(err.Error(), "restore isolation") || r.h.index("docker stop") >= 0 || r.h.index("docker service scale") >= 0 || r.h.index("-exec rm -rf {} +") >= 0 {
						t.Fatalf("unsafe prerequisites did not fail before mutation: %v\n%s", err, r.h.scripts())
					}
					return
				}
				if err != nil || !postChecked {
					t.Fatalf("private prerequisite restore: %v", err)
				}
				var first, dependent, ready int
				if runtime == "swarm" {
					first = r.h.index("docker service scale 'yoho-shop-production_postgres=1'")
					dependent = r.h.index("docker service scale 'yoho-shop-production_db=1'")
					ready = r.h.index("State.Health")
				} else {
					first = r.h.index("docker start 'cid-postgres'")
					dependent = r.h.index("docker start 'cid123'")
					ready = r.h.index("State.Health")
				}
				if !(first >= 0 && first < ready && ready < dependent) {
					t.Fatalf("prerequisite not ready before target: %d %d %d\n%s", first, ready, dependent, r.h.scripts())
				}
				if r.running["postgres"] != 0 || r.running["web"] == 0 || r.running["worker"] != 1 {
					t.Fatalf("prior state not restored: %v", r.running)
				}
			})
		}
	}
}

func TestRestoreRefusesForeignVolumeConsumer(t *testing.T) {
	for _, runtime := range []string{"compose", "swarm"} {
		t.Run(runtime, func(t *testing.T) {
			r := newRestoreRig(t, runtime, false)
			respond := r.h.respond
			r.h.respond = func(ctx context.Context, s string) (string, error) {
				if strings.Contains(s, "docker ps -a --filter 'volume=") {
					return "foreign|another-project|another-app|production", nil
				}
				return respond(ctx, s)
			}
			_, err := Restore(context.Background(), r.o)
			if err == nil || !strings.Contains(err.Error(), "shared data") || r.h.index("docker stop") >= 0 || r.h.index("docker service scale") >= 0 {
				t.Fatalf("foreign volume consumer not refused before mutation: %v\n%s", err, r.h.scripts())
			}
		})
	}
}

func TestRestoreGeneratedFailureKeepsConsumersStopped(t *testing.T) {
	for _, runtime := range []string{"compose", "swarm"} {
		t.Run(runtime, func(t *testing.T) {
			r := newRestoreRig(t, runtime, true)
			const file = "/var/lib/yoho/backups/shop/production/restore-20261008T030000Z/data/manifest.json"
			var m Manifest
			_ = json.Unmarshal(r.h.files[file], &m)
			m.GeneratedSecrets = []string{"MISSING"}
			r.h.files[file], _ = json.Marshal(m)
			_, err := Restore(context.Background(), r.o)
			if err == nil || !strings.Contains(err.Error(), "restore generated secrets") || !strings.Contains(err.Error(), "Services left stopped") {
				t.Fatalf("generated secret failure not isolated: %v", err)
			}
			assertRestoreStopped(t, r)
		})
	}
}

func TestRestoreComposePreservesMixedReplicaStates(t *testing.T) {
	h := &fakeHost{}
	state := &restoreState{Runtime: "compose", Objects: []restoreObject{
		{ID: "web-running", Service: "web", Status: "running"},
		{ID: "web-stopped", Service: "web", Status: "exited"},
		{ID: "web-created", Service: "web", Status: "created"},
	}}
	if err := state.resume(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if h.index("docker start 'web-running'") < 0 || h.index("docker start 'web-stopped'") >= 0 || h.index("docker start 'web-created'") >= 0 {
		t.Fatalf("resume changed prior inactive containers: %s", h.scripts())
	}
}

func TestRestoreIsolationCleanupFailureReported(t *testing.T) {
	r := newRestoreRig(t, "compose", true)
	failed := false
	r.after = func(_ context.Context, s string) error {
		if strings.Contains(s, "docker exec -i") {
			failed = true
			return errors.New("dump failed")
		}
		if failed && strings.Contains(s, "docker stop 'cid123'") {
			return errors.New("Server unreachable")
		}
		return nil
	}
	_, err := Restore(context.Background(), r.o)
	if err == nil || !strings.Contains(err.Error(), "dump failed") || !strings.Contains(err.Error(), "isolation cleanup failed") || !strings.Contains(err.Error(), "Server unreachable") {
		t.Fatalf("cleanup failure not reported: %v", err)
	}
	if r.h.index("docker start 'cid-web'") >= 0 {
		t.Fatal("failed cleanup reopened consumers")
	}
}
