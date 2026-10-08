package transport

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

type fakeHost struct {
	name string
	mu   sync.Mutex
	// remote image IDs
	ids        map[string]string
	containerd bool
	loadFails  bool
	scripts    []string
	stdins     []string
}

func (h *fakeHost) Name() string { return h.name }
func (h *fakeHost) Run(ctx context.Context, c remote.Cmd) error {
	var in []byte
	if c.Stdin != nil {
		in, _ = io.ReadAll(c.Stdin)
	}
	h.mu.Lock()
	h.scripts = append(h.scripts, c.Script)
	h.stdins = append(h.stdins, string(in))
	h.mu.Unlock()
	switch {
	case strings.HasPrefix(c.Script, "docker image inspect"):
		for img, id := range h.ids {
			if strings.HasSuffix(c.Script, " "+remote.Quote(img)+" 2>/dev/null") {
				io.WriteString(c.Stdout, id+"\n")
				return nil
			}
		}
		return &remote.ExitError{Host: h.name, Code: 1}
	case strings.HasPrefix(c.Script, "docker info"):
		if h.containerd {
			io.WriteString(c.Stdout, `[["driver-type","io.containerd.snapshotter.v1"]]`)
		} else {
			io.WriteString(c.Stdout, `[["Backing Filesystem","extfs"]]`)
		}
	case c.Script == LoadScript && h.loadFails:
		return &remote.ExitError{Host: h.name, Code: 1, Stderr: "no space"}
	}
	return nil
}
func (h *fakeHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	var b bytes.Buffer
	c.Stdout = &b
	err := h.Run(ctx, c)
	return strings.TrimSpace(b.String()), err
}
func (h *fakeHost) WriteFile(context.Context, string, []byte, os.FileMode, bool) error { return nil }
func (h *fakeHost) ReadFile(context.Context, string, bool) ([]byte, error)             { return nil, nil }
func (h *fakeHost) Close() error                                                       { return nil }

func (h *fakeHost) ran(prefix string) []int {
	var idx []int
	for i, s := range h.scripts {
		if strings.HasPrefix(s, prefix) {
			idx = append(idx, i)
		}
	}
	return idx
}

type fakeLocal struct {
	mu     sync.Mutex
	calls  [][]string
	stdins []string
	pussh  bool
}

func (f *fakeLocal) exec(ctx context.Context, c Command) error {
	var in []byte
	if c.Stdin != nil {
		in, _ = io.ReadAll(c.Stdin)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c.Argv)
	f.stdins = append(f.stdins, string(in))
	f.mu.Unlock()
	a := strings.Join(c.Argv, " ")
	switch {
	case strings.HasPrefix(a, "docker image inspect --platform"):
		for _, img := range c.Argv[7:] {
			io.WriteString(c.Stdout, "fp-"+img+"\n")
		}
	case strings.HasPrefix(a, "docker image inspect"):
		for _, img := range c.Argv[5:] {
			io.WriteString(c.Stdout, "fp-"+img+"\n")
		}
	case strings.HasPrefix(a, "docker pussh --help"):
		if !f.pussh {
			return errors.New("not a docker command")
		}
	case a == "docker save --help":
		io.WriteString(c.Stdout, "  --platform string  Save only the given platform\n")
	case strings.HasPrefix(a, "docker save"):
		io.WriteString(c.Stdout, "TAR:"+strings.Join(c.Argv[2:], ","))
	}
	return nil
}

func (f *fakeLocal) has(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			return true
		}
	}
	return false
}

func gunzip(t *testing.T, s string) string {
	t.Helper()
	r, err := gzip.NewReader(strings.NewReader(s))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r)
	return string(b)
}

func TestAutoSkipsAndLoads(t *testing.T) {
	imgs := []string{"yoho/app-web:v1", "yoho/app-worker:v1"}
	up := &fakeHost{name: "up", ids: map[string]string{"yoho/app-web:v1": "fp-yoho/app-web:v1", "yoho/app-worker:v1": "fp-yoho/app-worker:v1"}}
	partial := &fakeHost{name: "partial", ids: map[string]string{"yoho/app-web:v1": "fp-yoho/app-web:v1", "yoho/app-worker:v1": "fp-old"}}
	f := &fakeLocal{}
	var out bytes.Buffer
	res, err := Push(context.Background(), PushOptions{Images: imgs, Hosts: []remote.Host{up, partial}, Platform: "linux/amd64", Exec: f.exec, Out: &out})
	if err != nil {
		t.Fatal(err)
	}
	if res[0] != (Result{"up", MethodSkip}) || res[1] != (Result{"partial", MethodLoad}) {
		t.Fatalf("results %v", res)
	}
	li := partial.ran(LoadScript)
	if len(li) != 1 {
		t.Fatalf("scripts %v", partial.scripts)
	}
	if got := gunzip(t, partial.stdins[li[0]]); got != "TAR:--platform,linux/amd64,yoho/app-worker:v1" {
		t.Fatalf("loaded %q", got)
	}
	if !strings.Contains(out.String(), "pussh unavailable") {
		t.Fatalf("out %q", out.String())
	}
}

func TestPusshNeedsContainerdStore(t *testing.T) {
	imgs := []string{"yoho/app-web:v1"}
	f := &fakeLocal{pussh: true}
	cd := &fakeHost{name: "cd", containerd: true}
	ov := &fakeHost{name: "ov"}
	res, err := Push(context.Background(), PushOptions{Images: imgs, Hosts: []remote.Host{cd, ov},
		Targets: map[string]string{"cd": "u@cd", "ov": "u@ov"}, Exec: f.exec})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Method != MethodPussh || res[1].Method != MethodLoad {
		t.Fatalf("results %v", res)
	}
	if !f.has("docker pussh yoho/app-web:v1 u@cd") || f.has("docker pussh yoho/app-web:v1 u@ov") {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestRegistryFallbackPasswordOnStdin(t *testing.T) {
	imgs := []string{"ghcr.io/me/app-web:v1"}
	f := &fakeLocal{}
	h := &fakeHost{name: "h", loadFails: true}
	reg := &config.Registry{Prefix: "ghcr.io/me", Username: "me"}
	res, err := Push(context.Background(), PushOptions{Images: imgs, Hosts: []remote.Host{h}, Registry: reg, RegistryPassword: "pw-secret", Exec: f.exec})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Method != MethodRegistry {
		t.Fatalf("results %v", res)
	}
	if !f.has("docker login ghcr.io --username me --password-stdin") || !f.has("docker push ghcr.io/me/app-web:v1") {
		t.Fatalf("calls %v", f.calls)
	}
	for i, c := range f.calls {
		if strings.Contains(strings.Join(c, " "), "pw-secret") {
			t.Fatal("password on argv")
		}
		if c[1] == "login" && f.stdins[i] != "pw-secret" {
			t.Fatal("password not on stdin")
		}
	}
	login := h.ran("'docker' 'login'")
	if len(login) != 1 || h.stdins[login[0]] != "pw-secret" || strings.Contains(h.scripts[login[0]], "pw-secret") {
		t.Fatalf("remote login %v", h.scripts)
	}
	if len(h.ran("set -eu\ndocker pull 'ghcr.io/me/app-web:v1'")) != 1 {
		t.Fatalf("no pull: %v", h.scripts)
	}
}

func TestExplicitModeErrors(t *testing.T) {
	f := &fakeLocal{}
	h := &fakeHost{name: "h", loadFails: true}
	_, err := Push(context.Background(), PushOptions{Images: []string{"a:1"}, Hosts: []remote.Host{h}, Mode: MethodLoad, Exec: f.exec})
	if err == nil || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("err %v", err)
	}
	_, err = Push(context.Background(), PushOptions{Images: []string{"a:1"}, Hosts: []remote.Host{h}, Mode: MethodRegistry, Exec: f.exec})
	if err == nil || !strings.Contains(err.Error(), "no registry") {
		t.Fatalf("err %v", err)
	}
	if _, err := Push(context.Background(), PushOptions{Images: []string{"a:1"}, Mode: "bogus"}); err == nil {
		t.Fatal("want mode error")
	}
}

// End-to-end load through a real shell: docker replaced by stub scripts.
func TestLoadPipeWithLocalHost(t *testing.T) {
	dir := t.TempDir()
	stub := "#!/bin/sh\ncase \"$1\" in load) gzip -dc > " + dir + "/loaded ;; image) exit 1 ;; esac\n"
	if err := os.WriteFile(dir+"/docker", []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	f := &fakeLocal{}
	h := &remote.Local{HostName: "loc"}
	res, err := Push(context.Background(), PushOptions{Images: []string{"x:1"}, Hosts: []remote.Host{h}, Mode: MethodLoad, Exec: f.exec})
	if err != nil || res[0].Method != MethodLoad {
		t.Fatalf("%v %v", res, err)
	}
	b, _ := os.ReadFile(dir + "/loaded")
	if string(b) != "TAR:x:1" {
		t.Fatalf("loaded %q", b)
	}
}

// YOHO_E2E=1 YOHO_E2E_SSH=user@host: ship busybox via the auto ladder, twice
// (second run must skip).
func TestE2EPush(t *testing.T) {
	target := os.Getenv("YOHO_E2E_SSH")
	if os.Getenv("YOHO_E2E") != "1" || target == "" {
		t.Skip("YOHO_E2E=1 and YOHO_E2E_SSH not set")
	}
	ctx := context.Background()
	h, err := remote.NewSSH("e2e", target, false, remote.SSHOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	plat := os.Getenv("YOHO_E2E_PLATFORM")
	if plat == "" {
		plat = "linux/amd64"
	}
	img := "yoho/e2e-transport:t"
	for _, a := range [][]string{{"docker", "pull", "--platform", plat, "busybox:latest"}, {"docker", "tag", "busybox:latest", img}} {
		if err := osExec(ctx, Command{Argv: a, Stdout: io.Discard, Stderr: os.Stderr}); err != nil {
			t.Fatal(err)
		}
	}
	defer h.Run(ctx, remote.Cmd{Script: "docker image rm " + remote.Quote(img) + " >/dev/null 2>&1 || true"})
	h.Run(ctx, remote.Cmd{Script: "docker image rm " + remote.Quote(img) + " >/dev/null 2>&1 || true"})
	res, err := Push(ctx, PushOptions{Images: []string{img}, Hosts: []remote.Host{h}, Platform: plat, Out: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("first: %v", res)
	if res[0].Method == MethodSkip {
		t.Fatal("expected a transfer")
	}
	res, err = Push(ctx, PushOptions{Images: []string{img}, Hosts: []remote.Host{h}, Platform: plat, Out: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Method != MethodSkip {
		t.Fatalf("second run did not skip: %v", res)
	}
}
