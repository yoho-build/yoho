package build

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/config"
)

func TestResolveEngine(t *testing.T) {
	found := func(string) (string, error) { return "/usr/local/bin/container", nil }
	missing := func(string) (string, error) { return "", errors.New("not found") }
	up := func() error { return nil }
	down := func() error { return errors.New("not running") }
	cases := []struct {
		name         string
		cfg          config.Builder
		goos, goarch string
		look         func(string) (string, error)
		status       func() error
		want         string
	}{
		{"auto mac", config.Builder{}, "darwin", "arm64", found, up, EngineContainer},
		{"auto explicit", config.Builder{Engine: "auto", Location: "local"}, "darwin", "arm64", found, up, EngineContainer},
		{"intel mac", config.Builder{}, "darwin", "amd64", found, up, EngineDocker},
		{"linux", config.Builder{}, "linux", "arm64", found, up, EngineDocker},
		{"not installed", config.Builder{}, "darwin", "arm64", missing, up, EngineDocker},
		{"not running", config.Builder{}, "darwin", "arm64", found, down, EngineDocker},
		{"forced docker", config.Builder{Engine: "docker"}, "darwin", "arm64", found, up, EngineDocker},
		{"forced container", config.Builder{Engine: "container"}, "linux", "amd64", missing, down, EngineContainer},
		{"remote", config.Builder{Location: "remote", Engine: "container"}, "darwin", "arm64", found, up, EngineDocker},
		{"server", config.Builder{Location: "server"}, "darwin", "arm64", found, up, EngineDocker},
	}
	for _, c := range cases {
		got, reason := ResolveEngine(c.cfg, c.goos, c.goarch, c.look, c.status)
		if got != c.want || reason == "" {
			t.Errorf("%s: got %s (%s), want %s", c.name, got, reason, c.want)
		}
	}
}

const inspectJSON = `[{"id":"37bc","variants":[
 {"platform":{"os":"unknown","architecture":"unknown"},"config":{}},
 {"platform":{"os":"linux","architecture":"amd64"},"config":{"os":"linux","architecture":"amd64","created":"2026-10-08T15:38:52.780545709Z","rootfs":{"diff_ids":["sha256:a","sha256:b"]}}}]}]`

func TestParseContainerInspect(t *testing.T) {
	img, err := ParseContainerInspect([]byte(inspectJSON))
	if err != nil {
		t.Fatal(err)
	}
	if img.ID != "sha256:37bc" || len(img.Variants) != 1 {
		t.Fatalf("%+v", img)
	}
	for _, p := range []string{"", "linux/amd64"} {
		v, ok := img.Variant(p)
		if !ok || v.Fingerprint != "linux/amd64|2026-10-08T15:38:52.780545709Z|sha256:a,sha256:b," {
			t.Fatalf("%q: %+v", p, v)
		}
	}
	if _, ok := img.Variant("linux/arm64"); ok {
		t.Fatal("arm64 should be missing")
	}
}

func TestContainerBuild(t *testing.T) {
	r := &recExec{}
	exec := func(ctx context.Context, c Command) error {
		if strings.HasPrefix(strings.Join(c.Argv, " "), "container image inspect") {
			r.calls = append(r.calls, c)
			r.stdins = append(r.stdins, "")
			io.WriteString(c.Stdout, inspectJSON)
			return nil
		}
		return r.exec(ctx, c)
	}
	t.Setenv("FROM_ENV", "from-env")
	o := Options{Dir: "/src", App: "shop", Version: "v1", Project: project("/src"),
		Builder:   config.Builder{Secrets: []string{"GH_TOKEN"}},
		Platforms: []string{"linux/amd64"}, BuildEnv: []string{"GH_TOKEN=ghs_secret", "NPM_TOKEN=npm_secret"},
		GOOS: "darwin", GOARCH: "arm64", LookPath: func(string) (string, error) { return "/x/container", nil }, Exec: exec}
	imgs, err := Images(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if imgs["web"] != (Image{Ref: "yoho/shop-web:v1", ID: "sha256:37bc", Built: true, Engine: EngineContainer}) {
		t.Fatalf("%+v", imgs)
	}
	if _, i := r.find("container system status"); i < 0 {
		t.Fatal("status not checked")
	}
	c, _ := r.find("container build")
	got := strings.Join(c.Argv, " ")
	want := "container build --progress plain --platform linux/amd64 --tag yoho/shop-web:v1 --file /src/app/Dockerfile.prod " +
		"--build-arg FROM_ENV=from-env --build-arg RAILS_ENV=production --label team=x --label yoho.app=shop --label yoho.service=web --label yoho.version=v1 " +
		"--target release --secret id=GH_TOKEN,env=GH_TOKEN --secret id=npm,env=NPM_TOKEN /src/app"
	if got != want {
		t.Fatalf("argv\n got %s\nwant %s", got, want)
	}
	if strings.Contains(got, "_secret") {
		t.Fatal("secret on argv")
	}
	if strings.Join(c.Env, ",") != "GH_TOKEN=ghs_secret,NPM_TOKEN=npm_secret" {
		t.Fatalf("env %v", c.Env)
	}
}

// TestE2EContainerAMD64Build verifies the local Rosetta build path without
// requiring Docker or an SSH Server. Enable with YOHO_E2E=1 on Apple silicon.
func TestE2EContainerAMD64Build(t *testing.T) {
	if os.Getenv("YOHO_E2E") != "1" {
		t.Skip("YOHO_E2E not set")
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("requires an Apple silicon Mac")
	}
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("container CLI not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := ContainerStatus(ctx, osExec); err != nil {
		t.Fatalf("start Apple container with `container system start`: %v", err)
	}
	dir := t.TempDir()
	dockerfile := "FROM alpine\nRUN --mount=type=secret,id=TOK,env=TOK test \"$TOK\" = ok && test \"$(uname -m)\" = x86_64 && uname -m > /arch\nCMD cat /arch\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o600); err != nil {
		t.Fatal(err)
	}
	ver := fmt.Sprintf("t%d", time.Now().UnixNano())
	ref := ImageName(nil, "yoho-e2e-amd64", "", "web", ver)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := osExec(cleanup, Command{Argv: []string{"container", "image", "delete", ref}, Stdout: io.Discard, Stderr: io.Discard}); err != nil {
			t.Errorf("remove QA image %s: %v", ref, err)
		}
	})
	p := &types.Project{WorkingDir: dir, Services: types.Services{"web": {WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: "."}}}}}
	imgs, err := Images(ctx, Options{App: "yoho-e2e-amd64", Version: ver, Project: p,
		Platforms: []string{"linux/amd64"}, Builder: config.Builder{Secrets: []string{"TOK"}},
		BuildEnv: []string{"TOK=ok"}, Out: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	img := imgs["web"]
	if img.Engine != EngineContainer || !img.Built || img.Ref != ref {
		t.Fatalf("auto must select Apple container: %+v", img)
	}
	inspected, err := InspectContainerImage(ctx, osExec, ref)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := inspected.Variant("linux/amd64"); !ok {
		t.Fatalf("AMD64 variant missing: %+v", inspected)
	}
	if _, ok := inspected.Variant("linux/arm64"); ok {
		t.Fatal("built native ARM64 instead of AMD64")
	}
	var out bytes.Buffer
	if err := osExec(ctx, Command{Argv: []string{"container", "run", "--platform", "linux/amd64", "--rosetta", "--rm", ref}, Stdout: &out, Stderr: os.Stderr}); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(out.String()) != "x86_64" {
		t.Fatalf("run output %q", out.String())
	}
}

func TestContainerInlineAndFallback(t *testing.T) {
	var dockerfile string
	r := &recExec{}
	exec := func(ctx context.Context, c Command) error {
		if c.Argv[0] == "container" && c.Argv[1] == "build" {
			for i, a := range c.Argv {
				if a == "--file" {
					b, _ := os.ReadFile(c.Argv[i+1])
					dockerfile = string(b)
				}
			}
		}
		return r.exec(ctx, c)
	}
	mac := func(o Options) Options {
		o.GOOS, o.GOARCH, o.LookPath, o.Exec = "darwin", "arm64", func(string) (string, error) { return "", nil }, exec
		return o
	}
	p := &types.Project{WorkingDir: "/src", Services: types.Services{"w": {WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: ".", DockerfileInline: "FROM alpine"}}}}}
	if _, err := Images(context.Background(), mac(Options{App: "a", Version: "v", Project: p})); err != nil {
		t.Fatal(err)
	}
	if dockerfile != "FROM alpine" {
		t.Fatalf("inline dockerfile %q", dockerfile)
	}
	// Unsupported option: auto falls back to docker, explicit container errors.
	p.Services["w"].Build.CacheFrom = []string{"type=gha"}
	var out strings.Builder
	imgs, err := Images(context.Background(), mac(Options{App: "a", Version: "v", Project: p, Out: &out}))
	if err != nil || imgs["w"].Engine != EngineDocker || !strings.Contains(out.String(), "cache_from") {
		t.Fatalf("%+v %v %q", imgs, err, out.String())
	}
	_, err = Images(context.Background(), mac(Options{App: "a", Version: "v", Project: p, Builder: config.Builder{Engine: EngineContainer}}))
	if err == nil || !strings.Contains(err.Error(), "builder.engine: docker") {
		t.Fatalf("err %v", err)
	}
}
