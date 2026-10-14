package dash

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

const fixture = `@@yoho-host
hostname=web-1
os=Debian GNU/Linux 13 (trixie)
kernel=6.12.0
arch=x86_64
cpus=4
uptime=3600.52
load=0.10 0.20 0.30
memtotal=8000000
memavail=4000000
disk=yoho	/var/lib/yoho	/	100000	40000	60000
docker=29.1.0
disk=docker	/var/lib/docker	/var/lib/docker	100000	40010	59990
disk=docker	/srv/docker	/srv	500	1	499
@@yoho-ps
{"ID":"aaa","Names":"yoho-shop-production-web-1","Image":"shop-web:abc","State":"running","Status":"Up 2 hours (healthy)","Ports":"80/tcp","Labels":"com.docker.compose.project=yoho-shop-production,com.docker.compose.service=web","RunningFor":"2 hours ago"}
{"ID":"bbb","Names":"yoho-shop-production-db-1","Image":"postgres:17","State":"exited","Status":"Exited (1) 3 minutes ago","Ports":"","Labels":"com.docker.compose.service=db","RunningFor":"1 day ago"}
{"ID":"ccc","Names":"yoho-proxy","Image":"basecamp/kamal-proxy:v0.9","State":"running","Status":"Up 3 days","Ports":"0.0.0.0:80->80/tcp","Labels":"","RunningFor":"3 days ago"}
{"ID":"ddd","Names":"yoho-shop-production_worker.1.x","Image":"w","State":"running","Status":"Up 1 minute (health: starting)","Labels":"com.docker.stack.namespace=yoho-shop-production,com.docker.swarm.service.name=yoho-shop-production_worker","RunningFor":""}
{"ID":"eee","Names":"yoho-shop-production_worker.1.old","Image":"w","State":"exited","Status":"Exited (0) 1 hour ago","Labels":"com.docker.stack.namespace=yoho-shop-production,com.docker.swarm.task.id=abc"}
{"ID":"fff","Names":"yoho-shop-production-migrate-1","Image":"m","State":"exited","Status":"Exited (0) 5 minutes ago","Labels":"com.docker.compose.service=migrate"}
not json
@@yoho-stats
{"ID":"aaa","Name":"yoho-shop-production-web-1","CPUPerc":"12.50%","MemPerc":"3.25%","MemUsage":"100MiB / 7.6GiB","NetIO":"1kB / 2kB","BlockIO":"0B / 0B","PIDs":"9"}
{"ID":"ccc","Name":"yoho-proxy","CPUPerc":"0.01%","MemPerc":"0.20%","MemUsage":"15MiB / 7.6GiB","NetIO":"","BlockIO":"","PIDs":"7"}
`

func TestParse(t *testing.T) {
	p := Parse(fixture)
	h := p.Host
	if h.Hostname != "web-1" || h.CPUs != 4 || h.Load != [3]float64{0.1, 0.2, 0.3} || h.UptimeSeconds != 3600.52 {
		t.Errorf("host: %+v", h)
	}
	if h.MemTotal != 8000000*1024 || h.MemAvailable != 4000000*1024 || h.DockerVersion != "29.1.0" {
		t.Errorf("mem/docker: %+v", h)
	}
	if len(h.Disks) != 2 || h.Disks[0].Label != "yoho" || h.Disks[0].Used != 40000*1024 || h.Disks[1].Mount != "/srv" {
		t.Errorf("disks (docker root on the same mount is folded): %+v", h.Disks)
	}
	if p.Proxy == nil || p.Proxy.Service != "proxy" || p.Proxy.CPUPercent != 0.01 {
		t.Fatalf("proxy: %+v", p.Proxy)
	}
	if len(p.Containers) != 4 {
		t.Fatalf("containers (old swarm tasks dropped): %+v", p.Containers)
	}
	db, migrate, web, worker := p.Containers[0], p.Containers[1], p.Containers[2], p.Containers[3]
	if db.Completed || !migrate.Completed || web.Completed {
		t.Errorf("completed: db %v migrate %v web %v", db.Completed, migrate.Completed, web.Completed)
	}
	if db.Service != "db" || db.State != "exited" || db.Health != "" {
		t.Errorf("db: %+v", db)
	}
	if web.Service != "web" || web.Health != "healthy" || web.CPUPercent != 12.5 || web.MemPercent != 3.25 || web.PIDs != "9" {
		t.Errorf("web: %+v", web)
	}
	if worker.Service != "worker" || worker.Health != "starting" {
		t.Errorf("swarm worker: %+v", worker)
	}
}

func TestParseDockerError(t *testing.T) {
	p := Parse("@@yoho-host\nhostname=x\n@@yoho-docker-error\npermission denied\n")
	if p.Host.DockerError != "permission denied" || len(p.Containers) != 0 || p.Containers == nil {
		t.Errorf("%+v", p)
	}
}

func TestScriptQuotesAndParses(t *testing.T) {
	s := Script("yoho-it's-prod", "/var/lib/yoho")
	if !strings.Contains(s, `'label=com.docker.compose.project=yoho-it'\''s-prod'`) {
		t.Errorf("project not quoted:\n%s", s)
	}
	if strings.Contains(s, " rm ") || strings.Contains(s, "down") || strings.Contains(s, "kill") {
		t.Errorf("script must be read-only:\n%s", s)
	}
	if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v %s", err, out)
	}
}

// TestScriptRunsLocally runs the script with sh on this machine: host facts
// must parse whether or not Docker is reachable.
func TestScriptRunsLocally(t *testing.T) {
	h := &remote.Local{}
	out, err := h.Output(context.Background(), remote.Cmd{Script: Script("yoho-dash-test-none", t.TempDir()+"/missing/root")})
	if err != nil {
		t.Fatal(err)
	}
	p := Parse(out)
	if p.Host.Hostname == "" || p.Host.Kernel == "" || p.Host.CPUs < 1 {
		t.Errorf("host facts: %+v\n%s", p.Host, out)
	}
	if len(p.Host.Disks) == 0 || p.Host.Disks[0].Total == 0 {
		t.Errorf("disk of a missing root must fall back to its existing parent: %+v", p.Host.Disks)
	}
	if p.Host.DockerVersion == "" && p.Host.DockerError == "" {
		t.Errorf("docker: neither version nor error\n%s", out)
	}
	for _, c := range p.Containers {
		t.Errorf("unexpected container for an unused project: %+v", c)
	}
}

type fakeHost struct {
	remote.Local
	out     string
	err     error
	scripts []string
}

func (f *fakeHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	time.Sleep(time.Millisecond)
	f.scripts = append(f.scripts, c.Script)
	return f.out, f.err
}

func TestCollect(t *testing.T) {
	ok := &fakeHost{out: fixture}
	bad := &fakeHost{err: errors.New("ssh: connect refused")}
	src := &Source{
		App: "shop", Destination: "production", Runtime: "compose", Root: "/var/lib/yoho",
		Targets: []Target{{Name: "web-1", SSH: "root@a", Host: ok}, {Name: "web-2", SSH: "root@b", Host: bad}},
		Releases: func(_ context.Context, h remote.Host) ([]release.Release, error) {
			return []release.Release{
				{Version: "v2", Status: "deployed", DeployedAt: time.Unix(2, 0), Secrets: map[string]release.SecretAudit{"K": {}}},
				{Version: "v1", Status: "deployed", DeployedAt: time.Unix(1, 0)},
			}, nil
		},
	}
	snap, err := Collect(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Project != "yoho-shop-production" || len(snap.Servers) != 2 {
		t.Fatalf("%+v", snap)
	}
	s1, s2 := snap.Servers[0], snap.Servers[1]
	if s1.Error != "" || len(s1.Containers) != 4 || len(s1.Releases) != 2 || !s1.Releases[0].Current || s1.Releases[1].Current {
		t.Errorf("web-1: %+v", s1)
	}
	if s1.Elapsed <= 0 {
		t.Errorf("elapsed must be recorded: %v", s1.Elapsed)
	}
	if s2.Error == "" || s2.Containers == nil || s2.Releases == nil {
		t.Errorf("web-2 must report its error with empty lists: %+v", s2)
	}

	if !HasContainer(snap, "web-1", "yoho-proxy") || !HasContainer(snap, "web-1", "yoho-shop-production-web-1") {
		t.Error("owned containers must be readable")
	}
	if HasContainer(snap, "web-1", "postgres-unrelated") || HasContainer(snap, "web-2", "yoho-proxy") {
		t.Error("only containers of the Snapshot's Server may be read")
	}
	if _, err := Logs(context.Background(), src, snap, "web-1", "other", 10); err == nil {
		t.Error("Logs must refuse containers outside the Snapshot")
	}
	ok.out = "line"
	got, err := Logs(context.Background(), src, snap, "web-1", "yoho-proxy", 50)
	if err != nil || got != "line" {
		t.Fatalf("logs: %q %v", got, err)
	}
	if last := ok.scripts[len(ok.scripts)-1]; last != "docker logs --timestamps --tail 50 'yoho-proxy' 2>&1" {
		t.Errorf("logs script: %s", last)
	}
}

func TestLogsScriptClampsLines(t *testing.T) {
	if s := LogsScript("x", 999999); !strings.Contains(s, "--tail 200 ") {
		t.Error(s)
	}
}

func TestLiveCachesAndRefreshesForNewContainers(t *testing.T) {
	h := &fakeHost{out: fixture}
	l := &Live{Src: &Source{App: "shop", Destination: "production", Targets: []Target{{Name: "web-1", Host: h}}}, MaxAge: time.Hour}
	ctx := context.Background()
	a, _ := l.Snapshot(ctx, false)
	b, _ := l.Snapshot(ctx, false)
	if a != b || len(h.scripts) != 1 {
		t.Fatalf("cached Snapshot expected, %d collections", len(h.scripts))
	}
	if _, err := l.Logs(ctx, "web-1", "yoho-new-1", 10); err == nil {
		t.Error("unknown container must fail after a refresh")
	}
	if len(h.scripts) != 2 {
		t.Errorf("an unknown container must trigger one fresh collection, got %d scripts", len(h.scripts))
	}
}

func TestSwarmReleasesFromManagerOnly(t *testing.T) {
	var asked []string
	src := &Source{
		App: "shop", Destination: "production", Runtime: "swarm",
		Targets: []Target{{Name: "manager", Host: &fakeHost{out: fixture}}, {Name: "worker", Host: &fakeHost{out: fixture}}},
		Releases: func(_ context.Context, h remote.Host) ([]release.Release, error) {
			asked = append(asked, "x")
			return []release.Release{{Version: "v1", Status: "deployed"}}, nil
		},
	}
	snap, err := Collect(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || len(snap.Servers[0].Releases) != 1 || len(snap.Servers[1].Releases) != 0 {
		t.Errorf("asked %d, manager %d, worker %d", len(asked), len(snap.Servers[0].Releases), len(snap.Servers[1].Releases))
	}
}
