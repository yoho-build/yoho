package build

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-dev/yoho/internal/config"
	"github.com/yoho-dev/yoho/internal/remote"
)

type recExec struct {
	calls  []Command
	stdins []string
	fail   func(argv string) error
}

func (r *recExec) exec(_ context.Context, c Command) error {
	in := ""
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		in = string(b)
	}
	r.calls = append(r.calls, c)
	r.stdins = append(r.stdins, in)
	a := strings.Join(c.Argv, " ")
	if r.fail != nil {
		if err := r.fail(a); err != nil {
			return err
		}
	}
	if strings.HasPrefix(a, "docker image inspect") {
		io.WriteString(c.Stdout, "sha256:abc\n")
	}
	return nil
}

func (r *recExec) find(prefix string) (Command, int) {
	for i, c := range r.calls {
		if strings.HasPrefix(strings.Join(c.Argv, " "), prefix) {
			return c, i
		}
	}
	return Command{}, -1
}

type recHost struct {
	remote.Local
	cmds []remote.Cmd
}

func (h *recHost) Run(ctx context.Context, c remote.Cmd) error {
	h.cmds = append(h.cmds, c)
	if c.Stdout != nil && strings.HasPrefix(c.Script, "docker image inspect") {
		io.WriteString(c.Stdout, "sha256:srv\n")
	}
	if c.Stdout != nil && strings.HasPrefix(c.Script, "docker version") {
		io.WriteString(c.Stdout, "linux/x86_64\n")
	}
	return nil
}

func (h *recHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	var b bytes.Buffer
	c.Stdout = &b
	err := h.Run(ctx, c)
	return strings.TrimSpace(b.String()), err
}

func strp(s string) *string { return &s }

func project(dir string) *types.Project {
	return &types.Project{
		WorkingDir: dir,
		Services: types.Services{
			"web": {Name: "web", WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{
				Context:    "./app",
				Dockerfile: "Dockerfile.prod",
				Args:       types.MappingWithEquals{"RAILS_ENV": strp("production"), "FROM_ENV": nil},
				Target:     "release",
				Labels:     types.Labels{"team": "x"},
				Secrets:    []types.ServiceSecretConfig{{Source: "npm"}},
			}}},
			"db": {Name: "db", ContainerSpec: types.ContainerSpec{Image: "postgres:17"}},
		},
		Secrets: types.Secrets{"npm": {Environment: "NPM_TOKEN"}},
	}
}

func TestImageName(t *testing.T) {
	if got := ImageName(nil, "Shop", "Web", "abc"); got != "yoho/shop-web:abc" {
		t.Fatal(got)
	}
	if got := ImageName(&config.Registry{Server: "ghcr.io", Prefix: "ghcr.io/me/"}, "shop", "web", "v"); got != "ghcr.io/me/shop-web:v" {
		t.Fatal(got)
	}
	if got := ImageName(&config.Registry{Server: "reg.local:5000"}, "shop", "web", "v"); got != "reg.local:5000/shop-web:v" {
		t.Fatal(got)
	}
}

func TestLocalBuild(t *testing.T) {
	r := &recExec{}
	o := Options{Dir: "/src", App: "shop", Version: "v1", Project: project("/src"),
		Builder:   config.Builder{Engine: EngineDocker, Secrets: []string{"GH_TOKEN"}},
		Platforms: []string{"linux/amd64"}, BuildEnv: []string{"GH_TOKEN=ghs_secret", "NPM_TOKEN=npm_secret"}, Exec: r.exec}
	imgs, err := Images(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if imgs["db"] != (Image{Ref: "postgres:17"}) || imgs["web"] != (Image{Ref: "yoho/shop-web:v1", ID: "sha256:abc", Built: true, Engine: EngineDocker}) {
		t.Fatalf("images %+v", imgs)
	}
	c, _ := r.find("docker buildx build")
	got := strings.Join(c.Argv, " ")
	want := "docker buildx build --load --platform linux/amd64 --tag yoho/shop-web:v1 --file /src/app/Dockerfile.prod " +
		"--build-arg FROM_ENV --build-arg RAILS_ENV=production --label team=x --label yoho.app=shop --label yoho.service=web --label yoho.version=v1 " +
		"--target release --secret id=GH_TOKEN,env=GH_TOKEN --secret id=npm,env=NPM_TOKEN /src/app"
	if got != want {
		t.Fatalf("argv\n got %s\nwant %s", got, want)
	}
	if strings.Contains(got, "secret_") || strings.Contains(got, "ghs_") {
		t.Fatal("secret on argv")
	}
	if strings.Join(c.Env, ",") != "GH_TOKEN=ghs_secret,NPM_TOKEN=npm_secret" {
		t.Fatalf("env %v", c.Env)
	}
}

func TestInlineDockerfileAndUndefinedSecret(t *testing.T) {
	r := &recExec{}
	p := &types.Project{WorkingDir: "/src", Services: types.Services{"w": {WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: ".", DockerfileInline: "FROM alpine"}}}}}
	if _, err := Images(context.Background(), Options{App: "a", Version: "v", Project: p, Builder: config.Builder{Engine: EngineDocker}, Exec: r.exec}); err != nil {
		t.Fatal(err)
	}
	c, i := r.find("docker buildx build")
	if !strings.Contains(strings.Join(c.Argv, " "), "--file - ") || r.stdins[i] != "FROM alpine" {
		t.Fatalf("inline: %v %q", c.Argv, r.stdins[i])
	}
	p.Services["w"].Build.Secrets = []types.ServiceSecretConfig{{Source: "nope"}}
	if _, err := Images(context.Background(), Options{App: "a", Version: "v", Project: p, Builder: config.Builder{Engine: EngineDocker}, Exec: r.exec}); err == nil {
		t.Fatal("want undefined secret error")
	}
}

func TestRemoteBuilder(t *testing.T) {
	r := &recExec{fail: func(a string) error {
		if strings.HasPrefix(a, "docker buildx inspect") {
			return io.EOF
		}
		return nil
	}}
	o := Options{App: "shop", Version: "v1", Project: project("/src"), Builder: config.Builder{Location: "remote", Remote: "builder@b1"}, Exec: r.exec}
	if _, err := Images(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	name := RemoteBuilderName("builder@b1")
	if _, i := r.find("docker buildx create --name " + name + " --driver docker-container ssh://builder@b1"); i < 0 {
		t.Fatalf("no create: %v", r.calls)
	}
	if _, i := r.find("docker buildx build --load --builder " + name); i < 0 {
		t.Fatalf("no remote build: %v", r.calls)
	}
}

func TestServerBuild(t *testing.T) {
	r := &recExec{}
	h := &recHost{}
	h.HostName = "web1"
	o := Options{Dir: "/src", App: "shop", Version: "v1", Project: project("/src"),
		Builder:  config.Builder{Location: "server", Exclude: []string{"node_modules"}},
		BuildEnv: []string{"NPM_TOKEN=npm_secret"}, Host: h, RsyncTarget: "deploy@web1", SourceDir: "/srv/source", Exec: r.exec}
	imgs, err := Images(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if imgs["web"].ID != "sha256:srv" {
		t.Fatalf("%+v", imgs)
	}
	c, _ := r.find("rsync")
	if got := strings.Join(c.Argv, " "); got != "rsync -az --delete --exclude=.git --exclude=/.yoho/secrets* --exclude=node_modules -e ssh -o BatchMode=yes /src/ deploy@web1:/srv/source/" {
		t.Fatalf("rsync %s", got)
	}
	if h.cmds[0].Script != "mkdir -p -m 0700 '/srv/source'" {
		t.Fatalf("mkdir %q", h.cmds[0].Script)
	}
	b := h.cmds[1]
	if !strings.HasPrefix(b.Script, "set -eu\nDOCKER_BUILDKIT=1 docker build '--tag' 'yoho/shop-web:v1' '--file' '/srv/source/app/Dockerfile.prod'") ||
		!strings.HasSuffix(b.Script, "'--secret' 'id=npm,env=NPM_TOKEN' '/srv/source/app'") {
		t.Fatalf("script %s", b.Script)
	}
	if strings.Contains(b.Script, "npm_secret") || b.Env["NPM_TOKEN"] != "npm_secret" {
		t.Fatal("secret must travel via Env only")
	}
}

func TestRsyncArgvSSH(t *testing.T) {
	s, err := remote.NewSSH("web1", "deploy@example.com:2222", false, remote.SSHOptions{ControlDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	argv, err := RsyncArgv(Options{Dir: "/src", Host: s, SourceDir: "/x/source"})
	if err != nil {
		t.Fatal(err)
	}
	n := len(argv)
	if argv[n-4] != "-e" || !strings.Contains(argv[n-3], "'-p' '2222'") || argv[n-1] != "deploy@example.com:/x/source/" {
		t.Fatalf("%v", argv)
	}
	argv, err = RsyncArgv(Options{Dir: "/src", Host: s, SourceDir: "/x/source", RsyncTarget: "u@[::1]:22"})
	if err != nil || argv[len(argv)-3] != "ssh -o BatchMode=yes -p 22" || argv[len(argv)-1] != "u@[::1]:/x/source/" {
		t.Fatalf("%v %v", argv, err)
	}
}

func TestServerContextOutsideDir(t *testing.T) {
	p := &types.Project{WorkingDir: "/src", Services: types.Services{"w": {WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: "../other"}}}}}
	_, _, err := ServerScript(Options{Dir: "/src", App: "a", Version: "v", Project: p, SourceDir: "/s"}, "w", p.Services["w"], "x")
	if err == nil {
		t.Fatal("want outside error")
	}
}

func TestServerPlatform(t *testing.T) {
	h := &recHost{}
	p, err := ServerPlatform(context.Background(), h)
	if err != nil || p != "linux/amd64" {
		t.Fatalf("%q %v", p, err)
	}
	for in, want := range map[string]string{"linux/aarch64": "linux/arm64", "linux/armv7l": "linux/arm/v7", "linux/amd64": "linux/amd64"} {
		if got, _ := NormalizePlatform(in); got != want {
			t.Errorf("%s: %s", in, got)
		}
	}
	if _, err := NormalizePlatform("garbage"); err == nil {
		t.Error("want error")
	}
}

func TestE2ELocalDockerBuild(t *testing.T) {
	if os.Getenv("YOHO_E2E") != "1" {
		t.Skip("YOHO_E2E=1 not set")
	}
	dir := t.TempDir()
	os.WriteFile(dir+"/Dockerfile", []byte("FROM busybox\nRUN --mount=type=secret,id=TOK,env=TOK test \"$TOK\" = ok\n"), 0o644)
	p := &types.Project{WorkingDir: dir, Services: types.Services{"w": {WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: "."}}}}}
	o := Options{App: "e2e", Version: "t", Project: p, Platforms: []string{"linux/amd64"}, Builder: config.Builder{Engine: EngineDocker, Secrets: []string{"TOK"}}, BuildEnv: []string{"TOK=ok"}, Out: os.Stderr}
	imgs, err := Images(context.Background(), o)
	if err != nil || imgs["w"].ID == "" {
		t.Fatalf("%+v %v", imgs, err)
	}
	target := os.Getenv("YOHO_E2E_SSH")
	if target == "" {
		return
	}
	// location=server against a real Server.
	h, err := remote.NewSSH("e2e", target, false, remote.SSHOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	ctx := context.Background()
	plat, err := ServerPlatform(ctx, h)
	if err != nil || !strings.HasPrefix(plat, "linux/") {
		t.Fatalf("platform %q %v", plat, err)
	}
	src := "/tmp/yoho-e2e-build-src"
	defer h.Run(ctx, remote.Cmd{Script: "rm -rf " + src + "; docker image rm yoho/e2e-w:t >/dev/null 2>&1 || true"})
	o.Dir, o.Host, o.SourceDir, o.Platforms = dir, h, src, nil
	o.Builder.Location = "server"
	imgs, err = Images(ctx, o)
	if err != nil || !strings.HasPrefix(imgs["w"].ID, "sha256:") {
		t.Fatalf("%+v %v", imgs, err)
	}
}
