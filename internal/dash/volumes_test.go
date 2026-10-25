package dash

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/remote"
)

// volFixture is VolumesScript output from a Debian 12 Server (sudo access),
// plus a volume whose du timed out, one that vanished, and one past the
// budget. A file name holds a newline (as '?'), a tab and an escape.
const volFixture = "@@yoho-du-access\tsudo\tdeploy\n" +
	"@@yoho-du-appdir\n" +
	"12288\t/home/deploy/apps/shop/production\n" +
	"@@yoho-du-vol\tyoho-shop-production_bindvol\tlocal\t/var/lib/docker/volumes/yoho-shop-production_bindvol/_data\tdevice,o,type,\tbindvol\n" +
	"@@yoho-du-vol\tyoho-shop-production_pgdata\tlocal\t/var/lib/docker/volumes/yoho-shop-production_pgdata/_data\t\tpgdata\n" +
	"90128\t/var/lib/docker/volumes/yoho-shop-production_pgdata/_data\n" +
	"61444\t/var/lib/docker/volumes/yoho-shop-production_pgdata/_data/base\n" +
	"25600\t/var/lib/docker/volumes/yoho-shop-production_pgdata/_data/top.sqlite\n" +
	"3076\t/var/lib/docker/volumes/yoho-shop-production_pgdata/_data/pg_wal\n" +
	"4\t/var/lib/docker/volumes/yoho-shop-production_pgdata/_data/evil?99999\tx\x1b[31mred\n" +
	"99999999999999999999999\t/var/lib/docker/volumes/yoho-shop-production_pgdata/_data/overflow\n" +
	"@@yoho-du-vol\tyoho-shop-production_uploads\tlocal\t/var/lib/docker/volumes/yoho-shop-production_uploads/_data\t\tuploads\n" +
	"du: cannot read directory '/var/lib/docker/volumes/yoho-shop-production_uploads/_data/x': Permission denied\n" +
	"@@yoho-du-exit\t1\n" +
	"8200\t/var/lib/docker/volumes/yoho-shop-production_uploads/_data\n" +
	"8196\t/var/lib/docker/volumes/yoho-shop-production_uploads/_data/img\n" +
	"@@yoho-du-vol\tyoho-shop-production_cache\tlocal\t/var/lib/docker/volumes/yoho-shop-production_cache/_data\t\tcache\n" +
	"@@yoho-du-exit\t124\n" +
	"4096\t/var/lib/docker/volumes/yoho-shop-production_cache/_data/a\n" +
	"@@yoho-du-gone\tyoho-shop-production_old\n" +
	"@@yoho-du-vol\tyoho-shop-production_nfs\tlocal\t/var/lib/docker/volumes/yoho-shop-production_nfs/_data\tdevice,o,type,\tnfs\n" +
	"@@yoho-du-vol\tyoho-shop-production_late\tlocal\t/var/lib/docker/volumes/yoho-shop-production_late/_data\t\tlate\n" +
	"@@yoho-du-skip\n" +
	"@@yoho-du-vol\tbad name\tlocal\t/x\t\tbad\n" +
	"@@yoho-du-services\n" +
	"db\tyoho-shop-production_pgdata\n" +
	"web\tyoho-shop-production_bindvol,yoho-shop-production_uploads\n" +
	"yoho-shop-production_worker\tyoho-shop-production_uploads,/etc/hosts\n"

func TestParseVolumes(t *testing.T) {
	d := ServerDisk{}
	ParseVolumes(volFixture, "yoho-shop-production", &d)
	if d.Access != "sudo" || d.User != "deploy" || d.Note != "" || d.Error != "" {
		t.Errorf("access: %+v", d)
	}
	if !d.AppDirSized || d.AppDirPartial || d.AppDirBytes != 12288*1024 {
		t.Errorf("app dir: %+v", d)
	}
	byName := map[string]Volume{}
	for _, v := range d.Volumes {
		byName[strings.TrimPrefix(v.Name, "yoho-shop-production_")] = v
	}
	if len(d.Volumes) != 6 || d.Volumes[0].Name != "yoho-shop-production_pgdata" {
		t.Fatalf("volumes sorted by size, bad and vanished names dropped: %+v", d.Volumes)
	}

	pg := byName["pgdata"]
	if !pg.Sized || pg.Partial || pg.Bytes != 90128*1024 || pg.Key != "pgdata" || pg.Error != "" {
		t.Errorf("pgdata: %+v", pg)
	}
	if strings.Join(pg.Services, ",") != "db" {
		t.Errorf("pgdata services: %v", pg.Services)
	}
	if len(pg.Top) != 4 || pg.Top[0].Name != "base" || pg.Top[1].Name != "top.sqlite" || pg.Top[3].Name != "evil?99999\tx\x1b[31mred" {
		t.Errorf("pgdata top (files included, overflow dropped, odd name one entry): %+v", pg.Top)
	}

	up := byName["uploads"]
	if !up.Sized || !up.Partial || up.Bytes != 8200*1024 || !strings.Contains(up.Error, "Permission denied") {
		t.Errorf("uploads: partial with the du error: %+v", up)
	}
	if strings.Join(up.Services, ",") != "web,worker" {
		t.Errorf("uploads services, swarm prefix stripped: %v", up.Services)
	}

	cache := byName["cache"]
	if !cache.Sized || !cache.Partial || cache.Bytes != 4096*1024 || !strings.Contains(cache.Error, "stopped after") {
		t.Errorf("cache: timed out, lower bound from children: %+v", cache)
	}
	for _, n := range []string{"bindvol", "nfs"} {
		if v := byName[n]; v.Sized || !strings.Contains(v.Error, "volume options (device,o,type)") {
			t.Errorf("%s: options mean not sized: %+v", n, v)
		}
	}
	if late := byName["late"]; late.Sized || !strings.Contains(late.Error, "longer than") {
		t.Errorf("late: past the budget: %+v", late)
	}
}

func TestParseVolumesNoAccess(t *testing.T) {
	out := "@@yoho-du-access\tnone\tdeploy\n@@yoho-du-appdir\n@@yoho-du-missing\n@@yoho-du-lserr\n" +
		"@@yoho-du-vol\tyoho-shop-production_pgdata\tlocal\t/var/lib/docker/volumes/yoho-shop-production_pgdata/_data\t\tpgdata\n" +
		"@@yoho-du-services\n"
	d := ServerDisk{}
	ParseVolumes(out, "yoho-shop-production", &d)
	if d.AppDirSized || len(d.Volumes) != 1 || d.Volumes[0].Sized || d.Volumes[0].Error != "" {
		t.Errorf("listed without sizes: %+v", d)
	}
	if !strings.Contains(d.Note, "deploy ALL=(root) NOPASSWD: /usr/bin/du") {
		t.Errorf("note: %q", d.Note)
	}
	if !strings.Contains(d.Error, "volume ls failed") {
		t.Errorf("a failed listing must not look complete: %q", d.Error)
	}
	d = ServerDisk{}
	ParseVolumes("@@yoho-du-access\tno-timeout\troot\n", "p", &d)
	if !strings.Contains(d.Note, "timeout") {
		t.Errorf("no-timeout note: %q", d.Note)
	}
}

func TestVolumesScriptQuotesAndIsReadOnly(t *testing.T) {
	s := VolumesScript("yoho-it's-prod", "/srv/a b/apps/shop/production")
	for _, want := range []string{
		`'label=com.docker.compose.project=yoho-it'\''s-prod'`,
		`'label=com.docker.stack.namespace=yoho-it'\''s-prod'`,
		`cd '/srv/a b/apps/shop/production'`,
		"sudo -n du", "timeout -k 5 120", "-x -d 1 --",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{" rm ", "down", "kill", "prune", "docker run", "volume rm", "> /", "sudo sh"} {
		if strings.Contains(s, bad) {
			t.Errorf("script must be read-only, found %q", bad)
		}
	}
	if strings.Count(s, "docker volume ls") != 2 || strings.Contains(s, "docker volume ls -q\n") {
		t.Error("volumes must be listed by the App's labels only")
	}
	if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Fatalf("sh -n: %v %s", err, out)
	}
}

// TestVolumesScriptRunsLocally sizes a directory standing in for the App
// directory on this machine; volumes depend on Docker and access.
func TestVolumesScriptRunsLocally(t *testing.T) {
	dir := t.TempDir()
	out, err := (&remote.Local{}).Output(context.Background(), remote.Cmd{Script: VolumesScript("yoho-dash-test-none", dir)})
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	d := ServerDisk{}
	ParseVolumes(out, "yoho-dash-test-none", &d)
	if d.Access == "" || !d.AppDirSized {
		t.Errorf("access and app dir: %+v\n%s", d, out)
	}
	if len(d.Volumes) != 0 {
		t.Errorf("no volumes for an unused project: %+v", d.Volumes)
	}
	out, _ = (&remote.Local{}).Output(context.Background(), remote.Cmd{Script: VolumesScript("yoho-dash-test-none", dir+"/missing")})
	d = ServerDisk{}
	ParseVolumes(out, "yoho-dash-test-none", &d)
	if d.AppDirSized {
		t.Errorf("a missing App dir is not sized: %+v", d)
	}
}

func TestCollectDiskKeepsPartialOutput(t *testing.T) {
	h := &fakeHost{out: volFixture, err: errors.New("ssh: connection lost")}
	src := &Source{App: "shop", Destination: "production", Root: "/var/lib/yoho", Targets: []Target{
		{Name: "web-1", Host: h}, {Name: "gate", Reader: gateless{}},
	}}
	du, err := CollectDisk(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	s := du.Servers[0]
	if s.Error != "ssh: connection lost" || len(s.Volumes) == 0 || s.AppDir != "/var/lib/yoho/apps/shop/production" {
		t.Errorf("partial output must be kept with the error: %+v", s)
	}
	if !strings.Contains(h.scripts[0], "label=com.docker.compose.project=yoho-shop-production") {
		t.Error("script for the App's project")
	}
	if g := du.Servers[1]; g.Access != "scoped" || g.Note == "" {
		t.Errorf("scoped: %+v", g)
	}
}

type gateless struct{}

func (gateless) Server(context.Context) (Server, error) { return Server{}, nil }
func (gateless) Logs(context.Context, string, int) (string, error) {
	return "", nil
}

// slowHost blocks Output until release is closed and counts runs.
type slowHost struct {
	remote.Local
	runs    atomic.Int32
	release chan struct{}
}

func (h *slowHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if !strings.Contains(c.Script, markAccess) {
		return fixture, nil // the Snapshot script
	}
	h.runs.Add(1)
	select {
	case <-h.release:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return volFixture, nil
}

func TestLiveDiskSharesOneCollection(t *testing.T) {
	h := &slowHost{release: make(chan struct{})}
	l := &Live{Src: &Source{App: "shop", Destination: "production", Targets: []Target{{Name: "web-1", Host: h}}}, MaxAge: time.Hour}

	// A caller that goes away neither waits nor cancels the scan.
	gone, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { _, err := l.Disk(gone, true); errc <- err }()
	for h.runs.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller: %v", err)
	}

	// Snapshot is not blocked by the running scan.
	done := make(chan struct{})
	go func() { l.Snapshot(context.Background(), true); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Snapshot waited for du")
	}

	var wg sync.WaitGroup
	got := make([]*DiskUsage, 3)
	for i := range got {
		wg.Add(1)
		go func() { defer wg.Done(); got[i], _ = l.Disk(context.Background(), true) }()
	}
	time.Sleep(20 * time.Millisecond)
	close(h.release)
	wg.Wait()
	if n := h.runs.Load(); n != 1 {
		t.Errorf("concurrent callers must share one scan, got %d", n)
	}
	if got[0] == nil || got[0] != got[1] || got[1] != got[2] {
		t.Errorf("same result for every waiter: %v", got)
	}
	if again, _ := l.Disk(context.Background(), false); again != got[0] || h.runs.Load() != 1 {
		t.Error("a recent result is served from the cache")
	}
	if _, _ = l.Disk(context.Background(), true); h.runs.Load() != 2 {
		t.Error("fresh rescans")
	}
}
