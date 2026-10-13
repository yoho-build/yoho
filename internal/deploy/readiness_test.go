package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Execute the actual POSIX readiness script, not an inspect-string mock. The
// fake clock advances on sleep so timeout/startup-grace tests take milliseconds.
type readinessHost struct {
	*remote.Local
	env   map[string]string
	calls int
}

func (h *readinessHost) Run(ctx context.Context, c remote.Cmd) error {
	h.calls++
	c.Env = h.env
	return h.Local.Run(ctx, c)
}
func (h *readinessHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	c.Env = h.env
	return h.Local.Output(ctx, c)
}
func fakeReadinessHost(t *testing.T, state, health, code string, readyAt int, enabled bool) *readinessHost {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"clock": "0\n",
		"date":  "#!/bin/sh\nread -r t < \"$FAKE_DIR/clock\"; printf '%s\\n' \"$t\"\n",
		"sleep": "#!/bin/sh\nread -r t < \"$FAKE_DIR/clock\"; printf '%s\\n' \"$((t+1))\" > \"$FAKE_DIR/clock\"\n",
		"docker": `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FAKE_DIR/docker.log"
read -r t < "$FAKE_DIR/clock"
if [ "$1" = version ]; then printf '%s\n' "$ENGINE_VERSION"; exit 0; fi
if [ "$1" = compose ]; then
  case " $* " in
    *" up "*)
      if [ "$CANDIDATE" != no ]; then : > "$FAKE_DIR/created"; fi
      [ "$CANDIDATE" != partial ] || exit 1
      exit 0 ;;
  esac
  printf '%s' "$IDS"
  if [ -f "$FAKE_DIR/created" ]; then printf '\n%s\n' "$FRESH_IDS"; fi
  exit 0
fi
if [ "$1" = ps ]; then printf '%s\n' "$ORPHANS"; exit 0; fi
if [ "$1" = rm ] || [ "$1" = stop ]; then exit 0; fi
[ "$1" = inspect ] || exit 99
[ "$MISSING" != yes ] || exit 1
case "$3" in
  *com.docker.compose.oneoff*) if [ "$4" = "$ONEOFF_ID" ]; then printf '%s\n' "$ONEOFF_LABEL"; else printf 'False\n'; fi ;;
  *StartInterval*) [ "$START_INTERVAL" != unsupported ] || exit 1; printf '%s\n' "$START_INTERVAL" ;;
  *Config.Healthcheck*) printf '%s\n' "$HC" ;;
  *State.Status*)
    if [ "$3" = '{{.State.Status}}' ]; then printf '%s\n' "$PROCESS"; exit 0; fi
    state=$PROCESS; health=$HEALTH; code=$CODE; restarts=0
    if [ "$t" -ge "$READY_AT" ]; then state=$FINAL_PROCESS; health=$FINAL_HEALTH; code=$FINAL_CODE; fi
    if [ "$CRASH" = yes ] && [ "$t" -gt 0 ]; then restarts=1; fi
    printf '%s %s %s %s\n' "$state" "$health" "$code" "$restarts" ;;
  *\{\{.Name\}\}*) printf '/%s\n' "$4" ;;
  *RestartCount*) printf '0\n' ;;
  *yes*) printf '%s\n' "$ENABLED" ;;
  *State.Health*) printf '%s\n' "$HEALTH" ;;
  *) exit 98 ;;
esac
`,
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	yes := "no"
	if enabled {
		yes = "yes"
	}
	finalProcess, finalHealth, finalCode := state, health, code
	if readyAt > 0 {
		finalProcess, finalHealth, finalCode = "running", "healthy", "0"
	}
	return &readinessHost{Local: &remote.Local{}, env: map[string]string{
		"PATH": dir + ":" + os.Getenv("PATH"), "FAKE_DIR": dir, "IDS": "id1\n", "MISSING": "no", "ONEOFF_ID": "", "ONEOFF_LABEL": "True", "CANDIDATE": "no", "FRESH_IDS": "", "ORPHANS": "",
		"PROCESS": state, "HEALTH": health, "CODE": code, "FINAL_PROCESS": finalProcess, "FINAL_HEALTH": finalHealth, "FINAL_CODE": finalCode,
		"READY_AT": strconv.Itoa(readyAt), "ENABLED": yes, "HC": "0 0 0 0", "START_INTERVAL": "unsupported", "ENGINE_VERSION": "24.0.9", "CRASH": "no",
	}}
}

func TestPlainReadinessScript(t *testing.T) {
	cases := []struct {
		name, process, health, code string
		job, enabled, fail          bool
		ready                       int
		change                      map[string]string
	}{
		{name: "healthy", process: "running", health: "healthy", code: "0", enabled: true},
		{name: "inherited health startup grace", process: "running", health: "starting", code: "0", enabled: true, ready: 125, change: map[string]string{"HC": "120000000000 1000000000 1000000000 3"}},
		{name: "slow startup probes", process: "running", health: "starting", code: "0", enabled: true, ready: 70, change: map[string]string{"HC": "1000000000 1000000000 1000000000 1", "START_INTERVAL": "60000000000"}},
		{name: "default five second startup probes", process: "running", health: "starting", code: "0", enabled: true, ready: 36, change: map[string]string{"HC": "31000000000 1000000000 1000000000 1", "START_INTERVAL": "0", "ENGINE_VERSION": "25.0.1"}},
		{name: "old engine has no startup probes", process: "running", health: "starting", code: "0", enabled: true, ready: 36, fail: true, change: map[string]string{"HC": "31000000000 1000000000 1000000000 1", "START_INTERVAL": "unsupported"}},
		{name: "unhealthy", process: "running", health: "unhealthy", code: "0", enabled: true, fail: true, ready: 1000},
		{name: "exit zero is not a job", process: "exited", health: "none", code: "0", fail: true, ready: 1000},
		{name: "exit nonzero", process: "exited", health: "none", code: "7", fail: true, ready: 1000},
		{name: "crashing", process: "restarting", health: "none", code: "1", fail: true, ready: 1000},
		{name: "restart during health startup", process: "running", health: "starting", code: "0", enabled: true, ready: 10, fail: true, change: map[string]string{"CRASH": "yes"}},
		{name: "job success", process: "running", health: "none", code: "0", job: true, ready: 5, change: map[string]string{"FINAL_PROCESS": "exited", "FINAL_HEALTH": "none"}},
		{name: "successful completion ignores inherited server healthcheck", process: "exited", health: "unhealthy", code: "0", job: true, enabled: true},
		{name: "job nonzero", process: "exited", health: "none", code: "7", job: true, fail: true, ready: 1000},
		{name: "job running timeout", process: "running", health: "none", code: "0", job: true, fail: true, ready: 1000},
		{name: "missing containers", process: "running", health: "none", code: "0", fail: true, change: map[string]string{"IDS": ""}},
		{name: "missing inspect", process: "running", health: "none", code: "0", fail: true, change: map[string]string{"MISSING": "yes"}},
		{name: "no healthcheck", process: "running", health: "none", code: "0"},
		{name: "disabled healthcheck", process: "running", health: "none", code: "0", change: map[string]string{"HC": "999000000000 0 0 0"}},
		{name: "health timeout", process: "running", health: "starting", code: "0", enabled: true, fail: true, ready: 1000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := fakeReadinessHost(t, tc.process, tc.health, tc.code, tc.ready, tc.enabled)
			for k, v := range tc.change {
				h.env[k] = v
			}
			var out bytes.Buffer
			r := runner{h: h, out: &out, cc: composeCLI{project: "test", file: "compose.yaml", dir: "."}}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			err := r.waitService(ctx, servicePlan{Name: "worker", Replicas: 1, CompletedSuccessfully: &tc.job})
			if (err != nil) != tc.fail {
				t.Fatalf("error=%v, output=%s", err, out.String())
			}
			if !tc.fail && !tc.job && tc.health == "none" && !strings.Contains(out.String(), "only guarantees the process is running") {
				t.Fatalf("missing weak readiness notice: %s", out.String())
			}
		})
	}
}

func TestReadinessZeroAndReplicaCount(t *testing.T) {
	h := fakeReadinessHost(t, "running", "none", "0", 0, false)
	r := runner{h: h, out: &bytes.Buffer{}}
	if err := r.waitService(context.Background(), servicePlan{Name: "worker", Replicas: 0}); err != nil || h.calls != 0 {
		t.Fatalf("zero replicas: %v calls=%d", err, h.calls)
	}
	if err := r.waitService(context.Background(), servicePlan{Name: "worker", Replicas: 2}); err == nil {
		t.Fatal("missing replica accepted")
	}
}

func TestPlainReadinessFailureBlocksProxy(t *testing.T) {
	h := &dockerHost{local: &remote.Local{}, respond: func(s string) (string, error) {
		if strings.Contains(s, "'ps' '--all'") {
			return "", errors.New("unhealthy")
		}
		return "", nil
	}}
	d := testDeploy(t, h, &bytes.Buffer{})
	_, plans, err := compile(d, "primary", "/secrets", nil, []byte("key"), "")
	if err != nil {
		t.Fatal(err)
	}
	r := newRunner(d, "/compose.yaml")
	if err := r.cutover(context.Background(), plans); err == nil {
		t.Fatal("readiness failure ignored")
	}
	if strings.Contains(h.all(), "'--scale'") || strings.Contains(h.all(), "kamal-proxy' 'deploy") {
		t.Fatal("Proxy cutover ran after plain failure")
	}
	if strings.Contains(h.all(), "'--no-recreate'") {
		t.Fatal("plain startup must remain stop-first")
	}
}

func TestReadinessPlanCompileAndLegacyRollback(t *testing.T) {
	src := `services:
  web:
    image: web
    depends_on:
      migrate:
        condition: service_completed_successfully
      worker:
        condition: service_started
  migrate:
    image: migrate
    restart: "no"
  worker:
    image: worker
    restart: "no"
`
	d := testDeploy(t, &remote.Local{}, &bytes.Buffer{})
	d.Project = loadProject(t, src)
	d.Ext = nil
	raw, plans, err := compile(d, "primary", "/secrets", nil, []byte("key"), "")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(plans)
	if err != nil {
		t.Fatal(err)
	}
	var restored []servicePlan
	if err := json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	for _, sp := range restored {
		if sp.Name == "web" && strings.Join(sp.CompletedDependencies, ",") != "migrate" {
			t.Fatalf("completion contract not serialized: %+v", sp)
		}
		if sp.CompletedSuccessfully == nil || *sp.CompletedSuccessfully != (sp.Name == "migrate") {
			t.Fatalf("bad classification: %+v", sp)
		}
	}
	for i := range restored {
		restored[i].CompletedSuccessfully = nil
	}
	if err := recoverReadinessPlans(restored, raw); err != nil {
		t.Fatal(err)
	}
	for _, sp := range restored {
		if *sp.CompletedSuccessfully != (sp.Name == "migrate") {
			t.Fatalf("legacy rollback classification: %+v", sp)
		}
	}
	if err := recoverReadinessPlans([]servicePlan{{Name: "gone"}}, raw); err == nil {
		t.Fatal("incomplete legacy Compose accepted")
	}
}

func TestProxyReadinessUsesSameStatePolicy(t *testing.T) {
	for _, process := range []string{"running", "exited", "restarting"} {
		t.Run(process, func(t *testing.T) {
			h := fakeReadinessHost(t, process, "none", "0", 0, false)
			err := h.Run(context.Background(), remote.Cmd{Script: waitHealthyScript([]string{"id1"}, 1)})
			if (err != nil) != (process != "running") {
				t.Fatalf("%s: %v", process, err)
			}
		})
	}
	h := fakeReadinessHost(t, "running", "starting", "0", 100, true)
	if err := h.Run(context.Background(), remote.Cmd{Script: waitHealthyScript([]string{"id1"}, 1)}); err == nil {
		t.Fatal("explicit Proxy timeout ignored")
	}
}

func TestDependencyReadinessFailureBlocksReleaseCommand(t *testing.T) {
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}, respond: func(s string) (string, error) {
		if strings.Contains(s, "'ps' '--all' '-q' 'db'") {
			return "", errors.New("database unhealthy")
		}
		return respond(nil)(s)
	}}
	d := testDeploy(t, h, &bytes.Buffer{})
	if _, err := (Compose{}).Deploy(context.Background(), d); err == nil {
		t.Fatal("dependency failure ignored")
	}
	if strings.Contains(h.all(), "'run' '--rm'") || strings.Contains(h.all(), "'--scale'") {
		t.Fatal("release command or Proxy started after dependency failure")
	}
}

// The worker's healthcheck needs the API; the API needs the database. Input
// order deliberately starts with the worker to exercise dependency traversal.
func TestCutoverDependencyOrderedCandidates(t *testing.T) {
	for _, failAt := range []string{"", "api", "worker"} {
		t.Run("fail-"+failAt, func(t *testing.T) {
			dbReady, apiReady, workerReady := false, false, false
			h := &dockerHost{local: &remote.Local{}}
			h.respond = func(s string) (string, error) {
				switch {
				case strings.Contains(s, "'ps' '--all' '-q' 'db'"):
					dbReady = true
				case strings.Contains(s, "'ps' '-q' 'api'") && strings.Contains(s, "status=$(docker inspect"):
					if !dbReady {
						t.Fatal("API prepared before database readiness")
					}
					return "", nil // first deploy: no old API
				case strings.Contains(s, "'--scale' 'api=1'"):
					return "api-new", nil
				case strings.Contains(s, "set -- 'api-new'"):
					if !dbReady {
						t.Fatal("API gated before database")
					}
					if failAt == "api" {
						return "", errors.New("API unhealthy")
					}
					apiReady = true
				case strings.Contains(s, "{{.Name}}"):
					return "/api-new", nil
				case strings.Contains(s, "'up' '-d' '--no-deps' 'worker'"):
					if !apiReady {
						t.Fatal("worker started before API readiness")
					}
				case strings.Contains(s, "'ps' '--all' '-q' 'worker'"):
					if !apiReady {
						t.Fatal("worker gated before API readiness")
					}
					if failAt == "worker" {
						return "", errors.New("worker unhealthy")
					}
					workerReady = true
				case strings.Contains(s, "kamal-proxy' 'deploy"):
					if !workerReady {
						t.Fatal("Proxy traffic switched before worker readiness")
					}
				}
				return "", nil
			}
			plans := []servicePlan{
				{Name: "worker", Replicas: 1, DependsOn: []string{"api"}},
				{Name: "api", Replicas: 1, DependsOn: []string{"db"}, Proxy: proxyWithDefaults(&config.ServiceProxy{})},
				{Name: "db", Replicas: 1},
			}
			r := runner{h: h, out: &bytes.Buffer{}, app: "test", dest: "prod", cc: composeCLI{project: "test", file: "/compose.yaml", dir: "/app"}}
			err := r.cutover(context.Background(), plans)
			if (err != nil) != (failAt != "") {
				t.Fatalf("error=%v", err)
			}
			all := h.all()
			if failAt != "" {
				if strings.Contains(all, "kamal-proxy' 'deploy") {
					t.Fatal("failed gate switched traffic")
				}
				if !strings.Contains(all, "docker rm -f 'api-new'") {
					t.Fatal("unswitched candidate was not cleaned")
				}
			} else if strings.Contains(all, "docker rm -f 'api-new'") {
				t.Fatal("success removed serving candidate")
			}
			if strings.Contains(all, "'--scale' 'api=2'") || strings.Contains(all, "'--scale' 'worker") {
				t.Fatal("bootstrap duplication or worker overlap")
			}
		})
	}
}

// An uncertain first switch must not prevent cleanup of other candidates that
// were never offered to any route. Existing single-route cancellation tests
// separately exercise both certain rejection and uncertain switches.
func TestStagedCutoverUncertainSwitchCleanup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		switch {
		case strings.Contains(s, "'--scale' 'a=1'"):
			return "new-a", nil
		case strings.Contains(s, "'--scale' 'b=1'"):
			return "new-b", nil
		case strings.Contains(s, "{{.Name}}") && strings.Contains(s, "'new-a'"):
			return "/new-a", nil
		case strings.Contains(s, "{{.Name}}") && strings.Contains(s, "'new-b'"):
			return "/new-b", nil
		case strings.Contains(s, "kamal-proxy' 'deploy"):
			cancel()
			return "", context.Canceled
		case strings.Contains(s, "kamal-proxy list --json"):
			return `{}`, nil
		}
		return "", nil
	}
	r := runner{h: h, out: &bytes.Buffer{}, app: "test", dest: "prod"}
	plans := []servicePlan{{Name: "a", Replicas: 1, Proxy: proxyWithDefaults(&config.ServiceProxy{})}, {Name: "b", Replicas: 1, Proxy: proxyWithDefaults(&config.ServiceProxy{})}}
	if err := r.cutover(ctx, plans); err == nil {
		t.Fatal("switch failure ignored")
	}
	if strings.Contains(h.all(), "docker rm -f 'new-a'") {
		t.Fatal("uncertain serving candidate deleted")
	}
	if !strings.Contains(h.all(), "docker rm -f 'new-b'") {
		t.Fatal("never-switched candidate leaked")
	}
}

func TestCutoverRejectsInvalidDependencyPlanBeforeStartup(t *testing.T) {
	for _, plans := range [][]servicePlan{
		{{Name: "a", DependsOn: []string{"b"}}, {Name: "b", DependsOn: []string{"a"}}},
		{{Name: "a", DependsOn: []string{"missing"}}},
	} {
		h := &dockerHost{local: &remote.Local{}}
		r := runner{h: h, out: &bytes.Buffer{}}
		if err := r.cutover(context.Background(), plans); err == nil || len(h.scripts) != 0 {
			t.Fatalf("invalid plan: %v calls=%v", err, h.scripts)
		}
	}
}

// Assert discovery and candidate removal keep working after the caller cancels,
// and both have finite cleanup deadlines rather than abandoning partial creates.
type boundedCandidateHost struct {
	*dockerHost
	t *testing.T
}

func (h *boundedCandidateHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	discovery := strings.Contains(c.Script, "'ps' '--all' '-q' 'web'") && !strings.Contains(c.Script, "'--scale'") && !strings.Contains(c.Script, "status=$(docker inspect")
	removal := strings.Contains(c.Script, "docker rm -f")
	if discovery || removal {
		if ctx.Err() != nil {
			h.t.Fatal("candidate recovery received cancelled context")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 31*time.Second {
			h.t.Fatal("candidate recovery is unbounded")
		}
	}
	return h.dockerHost.Output(ctx, c)
}
func (h *boundedCandidateHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := h.Output(ctx, c)
	return err
}
func TestPreparePartialCreateRecovery(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		for _, discoveryFails := range []bool{false, true} {
			t.Run("cancel-"+strconv.FormatBool(cancelled)+"-discovery-fails-"+strconv.FormatBool(discoveryFails), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				h := &boundedCandidateHost{dockerHost: &dockerHost{local: &remote.Local{}}, t: t}
				h.respond = func(s string) (string, error) {
					switch {
					case strings.Contains(s, "'--scale' 'web=3'"):
						if cancelled {
							cancel()
							return "untrusted-partial-output", context.Canceled
						}
						return "untrusted-partial-output", errors.New("compose failed after creating one replica")
					case strings.Contains(s, "'ps' '-q' 'web'") && strings.Contains(s, "status=$(docker inspect"):
						return "old1", nil
					case strings.Contains(s, "'ps' '--all' '-q' 'web'"):
						if discoveryFails {
							return "", errors.New("discovery unavailable")
						}
						return "old1\nfresh1", nil
					}
					return "", nil
				}
				var out bytes.Buffer
				r := runner{h: h, out: &out, app: "test", dest: "prod", cc: composeCLI{project: "yoho-test-prod", file: "/saved.yaml", dir: "/app"}}
				sp := servicePlan{Name: "web", Replicas: 2, Proxy: proxyWithDefaults(&config.ServiceProxy{})}
				if err := r.cutover(ctx, []servicePlan{sp}); err == nil {
					t.Fatal("partial startup failure ignored")
				}
				all := h.all()
				if strings.Contains(all, "kamal-proxy' 'deploy") {
					t.Fatal("partial startup switched traffic")
				}
				if strings.Contains(all, "docker rm -f 'old1'") || strings.Contains(all, "docker rm -f 'untrusted-partial-output'") {
					t.Fatal("old or unverified container removed")
				}
				if discoveryFails {
					if strings.Contains(all, "docker rm -f") || !strings.Contains(out.String(), "no unverified containers will be removed") {
						t.Fatal("failed discovery must warn without blind deletion")
					}
				} else if !strings.Contains(all, "docker rm -f 'fresh1'") {
					t.Fatal("partial fresh container leaked")
				}
				if !strings.Contains(all, "'-p' 'yoho-test-prod'") || !strings.Contains(all, "'ps' '--all' '-q' 'web'") {
					t.Fatal("discovery was not scoped to project and Service")
				}
			})
		}
	}
}

func TestReadinessExcludesComposeRunOneoffs(t *testing.T) {
	for _, label := range []string{"True", "true"} {
		t.Run(label, func(t *testing.T) {
			h := fakeReadinessHost(t, "running", "none", "0", 0, false)
			h.env["IDS"] = "id1\njob1\n"
			h.env["ONEOFF_ID"] = "job1"
			h.env["ONEOFF_LABEL"] = label
			r := runner{h: h, out: &bytes.Buffer{}}
			if err := r.waitService(context.Background(), servicePlan{Name: "worker", Replicas: 1}); err != nil {
				t.Fatal(err)
			}
			log, _ := os.ReadFile(filepath.Join(h.env["FAKE_DIR"], "docker.log"))
			for _, line := range strings.Split(string(log), "\n") {
				if strings.HasSuffix(line, " job1") && !strings.Contains(line, "com.docker.compose.oneoff") {
					t.Fatalf("one-off readiness inspected: %s", line)
				}
			}
		})
	}
}
func TestProxyEnumerationAndPartialDiscoveryExcludeOneoffs(t *testing.T) {
	for _, partial := range []bool{false, true} {
		t.Run(strconv.FormatBool(partial), func(t *testing.T) {
			h := fakeReadinessHost(t, "running", "healthy", "0", 0, true)
			h.env["IDS"] = "old1\njob1\n"
			h.env["ONEOFF_ID"] = "job1"
			h.env["FRESH_IDS"] = "fresh1"
			h.env["CANDIDATE"] = "yes"
			if partial {
				h.env["CANDIDATE"] = "partial"
			}
			r := runner{h: h, out: &bytes.Buffer{}}
			sp := servicePlan{Name: "web", Replicas: 1, Proxy: proxyWithDefaults(&config.ServiceProxy{})}
			stage, err := r.prepareProxied(context.Background(), sp)
			if (err != nil) != partial {
				t.Fatalf("error=%v", err)
			}
			if stage == nil || strings.Join(stage.old, ",") != "old1" || strings.Join(stage.fresh, ",") != "fresh1" {
				t.Fatalf("wrong candidate enumeration: %+v", stage)
			}
			cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r.removeCandidate(cleanup, stage)
			log, _ := os.ReadFile(filepath.Join(h.env["FAKE_DIR"], "docker.log"))
			if !strings.Contains(string(log), "rm -f fresh1") || strings.Contains(string(log), "rm -f job1") || strings.Contains(string(log), "rm job1") {
				t.Fatalf("wrong deletion: %s", log)
			}
		})
	}
}

func TestInactiveConsumersDoNotIdentifyJobs(t *testing.T) {
	src := `services:
  worker:
    image: worker
    restart: "no"
  consumer:
    image: consumer
    deploy: {replicas: 0}
    depends_on:
      worker: {condition: service_completed_successfully}
`
	d := testDeploy(t, &remote.Local{}, &bytes.Buffer{})
	d.Project = loadProject(t, src)
	d.Ext = nil
	raw, plans, err := compile(d, "primary", "/secrets", nil, []byte("key"), "")
	if err != nil {
		t.Fatal(err)
	}
	for _, sp := range plans {
		if sp.Name == "worker" && *sp.CompletedSuccessfully {
			t.Fatal("inactive consumer classified running worker as a job")
		}
	}
	for i := range plans {
		plans[i].CompletedSuccessfully = nil
		plans[i].CompletedDependencies = nil
	}
	if err := recoverReadinessPlans(plans, raw); err != nil {
		t.Fatal(err)
	}
	for _, sp := range plans {
		if sp.Name == "worker" && *sp.CompletedSuccessfully {
			t.Fatal("legacy inactive consumer classified worker as a job")
		}
	}
}

func TestScaledZeroCompletionDependencyPreflight(t *testing.T) {
	src := `services:
  job:
    image: job
    deploy: {replicas: 0}
  consumer:
    image: consumer
    depends_on:
      job: {condition: service_completed_successfully}
`
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	d := testDeploy(t, h, &bytes.Buffer{})
	d.Project = loadProject(t, src)
	d.Ext = nil
	if _, _, err := compile(d, "primary", "/secrets", nil, []byte("key"), ""); err == nil || !strings.Contains(err.Error(), "scaled-zero") {
		t.Fatalf("compile preflight error: %v", err)
	}
	if _, err := (Compose{}).Deploy(context.Background(), d); err == nil || !strings.Contains(err.Error(), "scaled-zero") {
		t.Fatalf("deploy preflight error: %v", err)
	}
	if strings.Contains(h.all(), "'up'") || strings.Contains(h.all(), "'run' '--rm'") {
		t.Fatal("startup ran before preflight")
	}
	legacy := []servicePlan{{Name: "job", Replicas: 0}, {Name: "consumer", Replicas: 1, DependsOn: []string{"job"}}}
	dir := release.Dir(d.App, d.Destination, d.Version)
	if err := h.WriteFile(context.Background(), filepath.Join(dir, "compose.yaml"), []byte(src), 0600, false); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(context.Background(), h, filepath.Join(dir, "plan.json"), legacy); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(context.Background(), h, filepath.Join(dir, "release.json"), &release.Release{Version: d.Version, Status: "deployed"}); err != nil {
		t.Fatal(err)
	}
	h.scripts = nil
	if _, err := (Compose{}).Rollback(context.Background(), d, d.Version); err == nil || !strings.Contains(err.Error(), "scaled-zero") {
		t.Fatalf("legacy rollback preflight error: %v", err)
	}
	if len(h.scripts) != 0 {
		t.Fatalf("rollback commands ran before preflight: %v", h.scripts)
	}
	// No active consumer requires this job: scaled-zero remains legitimate.
	if err := validateReadinessPlans([]servicePlan{{Name: "job", Replicas: 0}}); err != nil {
		t.Fatal(err)
	}
}

func TestOptionalProfileDependencyPlansAndLegacyRollback(t *testing.T) {
	dir := t.TempDir()
	base := `services:
  worker:
    image: worker
    depends_on: [optional]
  optional:
    image: optional
`
	overlay := `services:
  worker:
    depends_on:
      optional: {condition: service_started, required: false}
  optional:
    profiles: [debug]
`
	for name, src := range map[string]string{"compose.yaml": base, "compose.production.yaml": overlay} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0600); err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := composefile.Load(context.Background(), dir, nil, nil, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Project.Services["optional"]; ok {
		t.Fatal("profile dependency should be inactive")
	}
	if _, ok := loaded.Project.Services["worker"].DependsOn["optional"]; !ok {
		t.Fatal("test did not retain loader's optional edge")
	}
	oldCompose, err := loaded.Project.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	d := testDeploy(t, &remote.Local{}, &bytes.Buffer{})
	d.Project = loaded.Project
	d.Ext = loaded.Ext
	compiled, plans, err := compile(d, "primary", "/secrets", nil, []byte("key"), "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(compiled), "optional:") {
		t.Fatal("compiled Compose retains absent optional edge")
	}
	if _, ok := loaded.Project.Services["worker"].DependsOn["optional"]; !ok {
		t.Fatal("compile mutated source dependency map")
	}
	if len(plans) != 1 || len(plans[0].DependsOn) != 0 {
		t.Fatalf("optional edge not pruned: %+v", plans)
	}
	legacy := []servicePlan{{Name: "worker", Replicas: 1, DependsOn: []string{"optional"}}}
	if err := recoverReadinessPlans(legacy, oldCompose); err != nil {
		t.Fatal(err)
	}
	if len(legacy[0].DependsOn) != 0 {
		t.Fatal("legacy optional edge not recovered")
	}
	required := strings.ReplaceAll(string(oldCompose), "required: false", "required: true")
	if err := recoverReadinessPlans([]servicePlan{{Name: "worker", Replicas: 1, DependsOn: []string{"optional"}}}, []byte(required)); err == nil {
		t.Fatal("missing required dependency silently discarded")
	}
}

func TestOrphanCleanupNeverDeletesComposeRunOneoffs(t *testing.T) {
	h := fakeReadinessHost(t, "running", "none", "0", 0, false)
	h.env["ORPHANS"] = "job1 gone True\njob2 gone true\nreplica gone False"
	r := runner{h: h, out: &bytes.Buffer{}}
	if err := r.removeOrphans(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(filepath.Join(h.env["FAKE_DIR"], "docker.log"))
	if !strings.Contains(string(log), "stop replica") || !strings.Contains(string(log), "rm replica") || strings.Contains(string(log), "stop job") || strings.Contains(string(log), "rm job") {
		t.Fatalf("one-off orphan cleanup: %s", log)
	}
}
