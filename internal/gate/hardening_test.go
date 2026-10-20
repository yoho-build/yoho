package gate

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Regression tests for the scoped-access QA findings.

func TestCollisions(t *testing.T) {
	setup(t, "")
	mk := func(app, dest string, built ...string) {
		dir := release.Dir(app, dest, "v1")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(release.Release{Version: "v1", Built: built})
		os.WriteFile(filepath.Join(dir, "release.json"), b, 0o600)
	}
	mk("shop", "prod", "web", "worker")
	// production-eu next to prod: fine unless prod builds a Service eu-web.
	if err := collisions("shop", "prod-eu", []string{"web"}); err != nil {
		t.Fatalf("no collision: %v", err)
	}
	mk("shop", "prod", "eu-web")
	if err := collisions("shop", "prod-eu", []string{"web"}); err == nil || !strings.Contains(err.Error(), "same name as one shop/prod builds (shop-prod-eu-web)") {
		t.Fatalf("image (shorter App first): %v", err)
	}
	// shop/prod-web and shop-prod/web are both yoho-shop-prod-web.
	mk("shop", "prod-web")
	if err := collisions("shop-prod", "web", nil); err == nil || !strings.Contains(err.Error(), "compose project yoho-shop-prod-web") {
		t.Fatalf("project: %v", err)
	}
	// shop-prod/x + api would be yoho/shop-prod-x-api; shop/prod builds
	// x-api (ours is the longer name here).
	mk("shop", "prod", "x-api")
	if err := collisions("shop-prod", "x", []string{"api"}); err == nil || !strings.Contains(err.Error(), "shop-prod-x-api") {
		t.Fatalf("image (longer App first): %v", err)
	}
}

// imageHost answers docker image/volume inspects from maps.
func imageHost(images map[string]string, volumes map[string]string) *fakeHost {
	return &fakeHost{reply: func(s string) (string, error) {
		switch {
		case strings.HasPrefix(s, "docker image inspect --format"):
			for ref, info := range images {
				if strings.Contains(s, remote.Quote(ref)+" 2>") {
					return info, nil
				}
			}
		case strings.HasPrefix(s, "docker volume inspect"):
			for name, info := range volumes {
				if strings.Contains(s, remote.Quote(name)+" 2>") {
					return info, nil
				}
			}
		}
		return "", nil
	}}
}

func TestCheckImages(t *testing.T) {
	setup(t, "")
	pol := Policy{Project: project, Built: map[string]string{"web": "yoho/shop-production-web:abc"}, Reserved: []string{"yoho/"}}
	cases := []struct {
		name, compose string
		images        map[string]string
		want          string
	}{
		{"registry image", "services: {web: {image: yoho/shop-production-web:abc}, db: {image: postgres:17}}",
			map[string]string{"postgres:17": "||"}, ""},
		{"absent image", "services: {web: {image: yoho/shop-production-web:abc}, db: {image: postgres:17}}", nil, ""},
		{"own build", "services: {web: {image: yoho/shop-production-web:abc}, db: {image: mine:1}}",
			map[string]string{"mine:1": "shop|production|"}, ""},
		{"another App's image by retag or ID", "services: {web: {image: yoho/shop-production-web:abc}, db: {image: 3f2a9c1b7d4e}}",
			map[string]string{"3f2a9c1b7d4e": "other|production|"}, "belongs to other/production"},
		{"built tag held by another App", "services: {web: {image: yoho/shop-production-web:abc}}",
			map[string]string{"yoho/shop-production-web:abc": "shop-production|x|"}, "belongs to shop-production/x"},
		{"pre_start image", "services: {web: {image: yoho/shop-production-web:abc, pre_start: [{image: 'tool:1'}]}}",
			map[string]string{"tool:1": "other|x|"}, "tool:1 on the Server belongs to other/x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := load(t, c.compose)
			call := &call{req: &remote.GateRequest{App: "shop", Destination: "production"}, host: imageHost(c.images, nil)}
			err := call.checkImages(context.Background(), pol, r.Project)
			if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

// Images the deploy did not build are pulled by the Gate: anonymously
// (no Server credentials, even when a copy is already local) unless their
// registry is in limits.registries.
func TestPullImages(t *testing.T) {
	setup(t, "")
	r := load(t, `
services:
  web: {image: yoho/shop-production-web:abc}
  db: {image: postgres:17}
  cache: {image: "GHCR.IO/acme/cache:1"}
  tool: {image: ghcr.io/acme/tool:2}
  zleak: {image: ghcr.io/other/private:1}
`)
	pol := Policy{Project: project, Built: map[string]string{"web": "yoho/shop-production-web:abc"}, Limits: Limits{Registries: []string{"ghcr.io/acme/"}}}
	h := imageHost(map[string]string{"ghcr.io/acme/tool:2": "||", "postgres:17": "||"}, nil)
	failPrivate := h.reply
	h.reply = func(s string) (string, error) {
		if strings.Contains(s, "DOCKER_CONFIG") && strings.Contains(s, "ghcr.io/other/private") {
			return "", errors.New("unauthorized")
		}
		return failPrivate(s)
	}
	c := &call{req: &remote.GateRequest{App: "shop", Destination: "production"}, host: h, fw: remote.NewFrameWriter(io.Discard)}
	err := c.pullImages(context.Background(), pol, r.Project)
	if err == nil || !strings.Contains(err.Error(), "ghcr.io/other/private:1 cannot be pulled anonymously") {
		t.Fatalf("private image: %v", err)
	}
	all := h.all()
	for _, want := range []string{
		"DOCKER_CONFIG=\"$d\" docker pull -q 'postgres:17'", // local copy, still proven public
		"docker pull -q 'GHCR.IO/acme/cache:1' >/dev/null",  // allowed registry, missing: Server login
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	for _, not := range []string{"'yoho/shop-production-web:abc' >/dev/null", "pull -q 'ghcr.io/acme/tool:2'", "DOCKER_CONFIG=\"$d\" docker pull -q 'GHCR.IO"} {
		if strings.Contains(all, not) {
			t.Errorf("unexpected %q in\n%s", not, all)
		}
	}
}

func TestCheckVolumes(t *testing.T) {
	setup(t, "")
	r := load(t, "services: {db: {image: postgres:17, volumes: [pg:/data]}}\nvolumes: {pg: {}}")
	for info, ok := range map[string]bool{
		"":                              true, // not created yet
		"local|yoho-shop-production|0":  true,
		"local|yoho-other-production|0": false,
		"local||0":                      false,
		"local|yoho-shop-production|3":  false,
		"rexray|yoho-shop-production|0": false,
	} {
		call := &call{req: &remote.GateRequest{App: "shop", Destination: "production"}, host: imageHost(nil, map[string]string{"yoho-shop-production_pg": info})}
		err := call.checkVolumes(context.Background(), r.Project)
		if (err == nil) != ok {
			t.Errorf("%q: %v", info, err)
		}
	}
}

func TestClearStaleLock(t *testing.T) {
	setup(t, "")
	dir := deploy.LockDir("shop", "production")
	lock := func(info deploy.LockInfo) {
		os.RemoveAll(dir)
		os.MkdirAll(dir, 0o700)
		b, _ := json.Marshal(info)
		os.WriteFile(filepath.Join(dir, "info.json"), b, 0o600)
	}
	old := gateRunning
	t.Cleanup(func() { gateRunning = old })
	alive := map[int]bool{100: true}
	gateRunning = func(pid int) bool { return alive[pid] }
	for _, c := range []struct {
		name  string
		info  deploy.LockInfo
		clear bool
	}{
		{"operator lock", deploy.LockInfo{Performer: "me"}, false},
		{"live Gate", deploy.LockInfo{PID: 100, Boot: "b1"}, false},
		{"dead Gate", deploy.LockInfo{PID: 200, Boot: "b1"}, true},
		{"before reboot", deploy.LockInfo{PID: 100, Boot: "b0"}, true},
		{"no boot id", deploy.LockInfo{PID: 200}, false},
	} {
		lock(c.info)
		cleared, err := ClearStaleLock("shop", "production", "b1")
		_, statErr := os.Stat(dir)
		if err != nil || cleared != c.clear || (statErr == nil) == c.clear {
			t.Errorf("%s: cleared=%v err=%v exists=%v", c.name, cleared, err, statErr == nil)
		}
	}
	if old(1 << 30) {
		t.Error("a pid that does not exist counts as a running Gate")
	}
}

// Two Gates see the same stale lock. The first clears it and takes a fresh
// lock; the second must not then remove that fresh lock.
func TestClearStaleLockRace(t *testing.T) {
	setup(t, "")
	dir := deploy.LockDir("shop", "production")
	write := func(info deploy.LockInfo) {
		os.MkdirAll(dir, 0o700)
		b, _ := json.Marshal(info)
		os.WriteFile(filepath.Join(dir, "info.json"), b, 0o600)
	}
	oldRunning, oldHook := gateRunning, staleHook
	t.Cleanup(func() { gateRunning, staleHook = oldRunning, oldHook })
	gateRunning = func(pid int) bool { return pid == 300 }
	write(deploy.LockInfo{PID: 200, Boot: "b1"}) // dead Gate

	second := make(chan bool, 1)
	var once sync.Once
	staleHook = func() {
		once.Do(func() {
			// Gate B starts while A holds the check: it must wait.
			go func() {
				cleared, _ := ClearStaleLock("shop", "production", "b1")
				second <- cleared
			}()
			select {
			case <-second:
				t.Error("the second Gate did not wait for the first")
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
	// A: clears the stale lock, then (as AcquireLock would) takes its own
	// before B gets to look. The flock makes B's read come after A's removal.
	cleared, err := ClearStaleLock("shop", "production", "b1")
	if err != nil || !cleared {
		t.Fatalf("first: %v %v", cleared, err)
	}
	write(deploy.LockInfo{PID: 300, Boot: "b1"}) // A's fresh lock (live)
	if <-second {
		// B ran after the removal; it must have found no lock or A's.
		t.Fatal("the second Gate removed a lock")
	}
	if _, err := os.Stat(filepath.Join(dir, "info.json")); err != nil {
		t.Fatalf("the fresh lock is gone: %v", err)
	}
}

func TestLockedErrorHasNoServerPath(t *testing.T) {
	err := lockedError(&deploy.LockedError{Dir: "/var/lib/yoho/apps/shop/production/lock", Holder: deploy.LockInfo{Performer: "bob (gate)", Command: "deploy", Version: "v1"}})
	var ge *remote.GateError
	if !errors.As(err, &ge) || strings.Contains(ge.Message+ge.Hint, "/var/lib") || !strings.Contains(ge.Message, "bob (gate) (deploy v1)") {
		t.Fatalf("got %+v", err)
	}
}

func TestMatchRelease(t *testing.T) {
	long := "04f59b245603549747dc26da215668b9f12c94af_uncommitted_4a61b760"
	rels := []release.Release{{Version: "abc123"}, {Version: "abd456"}, {Version: "abc"}, {Version: long}}
	for in, want := range map[string]string{"abc123": "abc123", "abc": "abc", "abd": "abd456", "04f59b245603_uncommitted_4a61b760": long, "04f5": long} {
		if got, err := matchRelease(rels, in); err != nil || got != want {
			t.Errorf("%s: %q %v", in, got, err)
		}
	}
	if _, err := matchRelease(rels, "ab"); err == nil || !strings.Contains(err.Error(), "matches 3") {
		t.Errorf("ambiguous: %v", err)
	}
	if _, err := matchRelease(rels, "zz"); err == nil {
		t.Error("want error for no match")
	}
}

func TestVetRollback(t *testing.T) {
	setup(t, "")
	s := &Settings{Limits: Limits{}.WithDefaults()}
	c := &call{req: &remote.GateRequest{App: "shop", Destination: "production"}}
	const hard = "cap_drop: [NET_RAW], pids_limit: 4096, pull_policy: never, mem_limit: 2g, cpus: 2"
	compiled := []byte("services: {web: {image: yoho/shop-production-web:v1, " + hard + "}}\nnetworks: {yoho: {external: true, name: yoho-shop-production-proxy}}\n")
	rel := &release.Release{Version: "v1", Built: []string{"web"}, ComposeSHA256: deploy.SHA256Hex(compiled)}
	ctx := context.Background()

	// The operator's Release: no Gate record.
	if err := c.vetRollback(ctx, s, "v1", compiled, rel); err == nil || !strings.Contains(err.Error(), "not deployed through the Gate") {
		t.Fatalf("operator Release: %v", err)
	}
	record := func(v Vetted) {
		os.MkdirAll(filepath.Dir(vettedPath("shop", "production", "v1")), 0o700)
		b, _ := json.Marshal(v)
		os.WriteFile(vettedPath("shop", "production", "v1"), b, 0o600)
	}
	good := Vetted{Version: "v1", Compose: "services: {web: {image: yoho/shop-production-web:v1}}", Built: []string{"web"}, ComposeSHA256: rel.ComposeSHA256}
	record(good)
	if err := c.vetRollback(ctx, s, "v1", compiled, rel); err != nil {
		t.Fatalf("Gate Release: %v", err)
	}
	// The Release on the Server was replaced after the Gate deployed it.
	if err := c.vetRollback(ctx, s, "v1", []byte("services: {web: {image: x, privileged: true}}"), rel); err == nil {
		t.Fatal("want refusal for a changed compose.yaml")
	}
	// Today's policy refuses what the Release did (e.g. a route removed).
	bad := good
	bad.Compose = "services: {web: {image: yoho/shop-production-web:v1, x-yoho: {proxy: {hosts: [shop.example.com]}}}}"
	record(bad)
	if err := c.vetRollback(ctx, s, "v1", compiled, rel); err == nil || !strings.Contains(err.Error(), "Proxy host shop.example.com is not allowed") {
		t.Fatalf("policy: %v", err)
	}
	// A Release on the shared network.
	shared := []byte("services: {web: {image: yoho/shop-production-web:v1, " + hard + "}}\nnetworks: {yoho: {external: true, name: yoho}}\n")
	good.ComposeSHA256 = deploy.SHA256Hex(shared)
	record(good)
	rel.ComposeSHA256 = good.ComposeSHA256
	if err := c.vetRollback(ctx, s, "v1", shared, rel); err == nil || !strings.Contains(err.Error(), "shared network yoho") {
		t.Fatalf("network: %v", err)
	}
	// Hardened under older, larger limits (or before hardening existed).
	for compiledSvc, want := range map[string]string{
		"cap_drop: [NET_RAW], pids_limit: 9000, pull_policy: never, mem_limit: 2g, cpus: 2":                                                           "pids_limit 9000",
		"cap_drop: [NET_RAW], pids_limit: 4096, pull_policy: never, mem_limit: 16g, cpus: 2":                                                          "mem_limit",
		"cap_drop: [NET_RAW], pids_limit: 4096, pull_policy: never, mem_limit: 2g, cpus: 2, scale: 9":                                                 "9 replicas",
		"cap_drop: [NET_RAW], pids_limit: 4096, mem_limit: 2g, cpus: 2":                                                                               "pull_policy is not never",
		"pids_limit: 4096, pull_policy: never":                                                                                                        "no memory limit",
		"cap_drop: [NET_RAW], pids_limit: 4096, pull_policy: never, mem_limit: 2g, cpus: 2, tmpfs: [/run]":                                            "tmpfs /run has no size",
		"cap_drop: [NET_RAW], pids_limit: 4096, pull_policy: never, mem_limit: 2g, cpus: 2, volumes: [{type: tmpfs, target: /t, tmpfs: {size: 64g}}]": "tmpfs /t: size",
		"cap_drop: [NET_RAW], pids_limit: 4096, pull_policy: never, mem_limit: 2g, cpus: 2, pre_start: [{image: y, mem_limit: 64g}]":                  "pre_start[0]: mem_limit",
	} {
		old := []byte("services: {web: {image: yoho/shop-production-web:v1, " + compiledSvc + "}}\nnetworks: {yoho: {external: true, name: yoho-shop-production-proxy}}\n")
		good.ComposeSHA256 = deploy.SHA256Hex(old)
		record(good)
		rel.ComposeSHA256 = good.ComposeSHA256
		if err := c.vetRollback(ctx, s, "v1", old, rel); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", compiledSvc, want, err)
		}
	}
}

func TestReceiveLimits(t *testing.T) {
	var buf bytes.Buffer
	if _, err := receive(&buf, strings.NewReader(strings.Repeat("x", 100)), 64, time.Second); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("size: %v", err)
	}
	pr, pw := io.Pipe()
	defer pw.Close()
	start := time.Now()
	if _, err := receive(io.Discard, pr, 64, 50*time.Millisecond); err == nil || !strings.Contains(err.Error(), "did not finish") || time.Since(start) > time.Second {
		t.Fatalf("timeout: %v", err)
	}
	buf.Reset()
	if n, err := receive(&buf, strings.NewReader("abc"), 64, time.Second); err != nil || n != 3 {
		t.Fatalf("ok: %d %v", n, err)
	}
}

func TestOneUploadAtATime(t *testing.T) {
	access := setup(t, "")
	stale := filepath.Join(access, ".gate-upload-old")
	os.MkdirAll(stale, 0o700)
	f, err := os.OpenFile(filepath.Join(access, "upload.lock"), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	c := &call{req: &remote.GateRequest{App: "shop", Destination: "production"}, fw: remote.NewFrameWriter(io.Discard), audit: map[string]any{}}
	if err := c.loadArchive(context.Background(), Limits{}, nil); err == nil || !strings.Contains(err.Error(), "in progress") {
		t.Fatalf("concurrent: %v", err)
	}
	if _, err := os.Stat(stale); err != nil {
		t.Fatal("swept an upload while another held the lock")
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	c.payload = bufio.NewReader(strings.NewReader(""))
	c.host = &fakeHost{}
	_ = c.loadArchive(context.Background(), Limits{}, nil)
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale upload not swept")
	}
}

func TestRequestTimeout(t *testing.T) {
	setup(t, "")
	pr, pw := io.Pipe()
	defer pw.Close()
	var out bytes.Buffer
	start := time.Now()
	code := Serve(context.Background(), Options{Member: "alice", In: pr, Out: &out, RequestTimeout: 50 * time.Millisecond})
	if code == 0 || time.Since(start) > 2*time.Second || !strings.Contains(out.String(), "no request within") {
		t.Fatalf("code %d out %s", code, out.String())
	}
}

func TestServerAuditAndRotation(t *testing.T) {
	setup(t, "")
	old := MaxAuditBytes
	MaxAuditBytes = 300
	t.Cleanup(func() { MaxAuditBytes = old })
	for range 5 {
		req, _ := remote.EncodeGateRequest("other", "production", VerbPS, nil)
		Serve(context.Background(), Options{Member: "alice", In: bytes.NewReader(req), Out: io.Discard})
	}
	b, err := os.ReadFile(ServerAuditPath())
	if err != nil || !strings.Contains(string(b), `"app":"other"`) {
		t.Fatalf("server audit: %v %s", err, b)
	}
	if _, err := os.Stat(ServerAuditPath() + ".1"); err != nil {
		t.Fatalf("not rotated: %v", err)
	}
	if fi, _ := os.Stat(ServerAuditPath()); fi.Size() > 2*MaxAuditBytes {
		t.Fatalf("log grew to %d", fi.Size())
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

func TestClientWriterCancelsFollow(t *testing.T) {
	w := &clientWriter{w: failWriter{}}
	cancelled := 0
	w.Write([]byte("x"))
	w.onError(func() { cancelled++ })
	if cancelled != 1 {
		t.Fatalf("cancel after an earlier failure: %d", cancelled)
	}
	if _, err := w.Write([]byte("y")); err == nil || cancelled != 1 {
		t.Fatalf("second write: %v, cancelled %d", err, cancelled)
	}
}
