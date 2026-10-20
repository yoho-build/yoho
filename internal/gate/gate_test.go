package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// fakeHost records scripts and answers with reply.
type fakeHost struct {
	mu      sync.Mutex
	scripts []string
	reply   func(script string) (string, error)
}

func (f *fakeHost) Name() string { return "srv" }
func (f *fakeHost) Run(_ context.Context, c remote.Cmd) error {
	f.mu.Lock()
	f.scripts = append(f.scripts, c.Script)
	f.mu.Unlock()
	if c.Stdin != nil {
		io.Copy(io.Discard, c.Stdin)
	}
	out, err := "", error(nil)
	if f.reply != nil {
		out, err = f.reply(c.Script)
	}
	if c.Stdout != nil {
		io.WriteString(c.Stdout, out)
	}
	return err
}
func (f *fakeHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	var b bytes.Buffer
	c.Stdout = &b
	err := f.Run(ctx, c)
	return strings.TrimSpace(b.String()), err
}
func (f *fakeHost) WriteFile(context.Context, string, []byte, os.FileMode, bool) error { return nil }
func (f *fakeHost) ReadFile(context.Context, string, bool) ([]byte, error) {
	return nil, os.ErrNotExist
}
func (f *fakeHost) Close() error { return nil }

func (f *fakeHost) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.scripts, "\n")
}

// setup makes a Root with shop/production deployed and alice a Member.
func setup(t *testing.T, members string) string {
	t.Helper()
	old := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = old })
	dir := release.AccessDir("shop", "production")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if members == "" {
		members = "members:\n  alice: {verbs: [deploy, logs, releases, ps]}\n"
	}
	os.WriteFile(filepath.Join(dir, "members.yml"), []byte(members), 0o600)
	return dir
}

func writeSettings(t *testing.T, s Settings) {
	t.Helper()
	b, _ := json.Marshal(s)
	os.WriteFile(SettingsPath("shop", "production"), b, 0o600)
}

type exitCode int

func (e exitCode) Error() string { return "exit" }
func (e exitCode) ExitCode() int { return int(e) }

// client returns a Gate client whose ssh runs Serve in-process.
func client(t *testing.T, member string, h remote.Host) *remote.Gate {
	t.Helper()
	exec := func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) error {
		for _, a := range argv {
			if a == "-O" {
				return nil // ControlMaster stop
			}
		}
		if argv[len(argv)-2] != "yoho-gate@example.com" {
			t.Errorf("ssh target: %v", argv)
		}
		code := Serve(ctx, Options{Member: member, Key: "SHA256:k", Version: "test", In: stdin, Out: stdout,
			Getenv:  func(k string) string { return map[string]string{"SSH_CONNECTION": "203.0.113.9 5555 10.0.0.1 22"}[k] },
			HostFor: func(string) (remote.Host, error) { return h, nil }})
		if code != 0 {
			return exitCode(code)
		}
		return nil
	}
	s, err := remote.NewSSH("srv", "yoho-gate@example.com", false, remote.SSHOptions{Exec: exec, ControlDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return remote.NewGate(s, "shop", "production")
}

func lastAudit(t *testing.T) AuditEntry {
	t.Helper()
	b, err := os.ReadFile(AuditPath("shop", "production"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var e AuditEntry
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &e); err != nil {
		t.Fatal(err)
	}
	return e
}

func TestHelloAndAudit(t *testing.T) {
	setup(t, "")
	h := &fakeHost{reply: func(s string) (string, error) {
		if strings.Contains(s, "docker version") {
			return "linux/amd64", nil
		}
		return "", nil
	}}
	var res HelloResult
	if err := client(t, "alice", h).Call(context.Background(), VerbHello, nil, nil, nil, &res); err != nil {
		t.Fatal(err)
	}
	if res.Member != "alice" || res.Platform != "linux/amd64" || len(res.Apps) != 1 || res.Apps[0].App != "shop" || res.Protocol != remote.GateProtocol {
		t.Fatalf("hello: %+v", res)
	}
	e := lastAudit(t)
	if e.Member != "alice" || e.Verb != "hello" || e.Key != "SHA256:k" || e.Client != "203.0.113.9" || e.Exit != 0 {
		t.Fatalf("audit: %+v", e)
	}
}

func TestDenied(t *testing.T) {
	setup(t, "")
	h := &fakeHost{}
	err := client(t, "bob", h).Call(context.Background(), VerbPS, nil, nil, nil, nil)
	var ge *remote.GateError
	if !errors.As(err, &ge) || ge.Code != "denied" || !strings.Contains(ge.Message, "bob has no access") {
		t.Fatalf("got %v", err)
	}
	if e := lastAudit(t); e.Exit != ExitDenied || e.Member != "bob" {
		t.Fatalf("audit %+v", e)
	}
	err = client(t, "alice", h).Call(context.Background(), VerbRollback, RollbackArgs{Version: "v1"}, nil, nil, nil)
	if !errors.As(err, &ge) || ge.Code != "denied" || !strings.Contains(ge.Message, "may not rollback") {
		t.Fatalf("got %v", err)
	}
	for _, verb := range []string{"exec", "access"} {
		err = client(t, "alice", h).Call(context.Background(), verb, nil, nil, nil, nil)
		if !errors.As(err, &ge) || ge.Code != "invalid" {
			t.Fatalf("%s: got %v", verb, err)
		}
	}
	if h.all() != "" {
		t.Fatalf("denied requests ran scripts: %s", h.all())
	}
}

func TestBadRequests(t *testing.T) {
	setup(t, "")
	for name, in := range map[string]string{
		"protocol":  `{"protocol":99,"app":"shop","destination":"production","verb":"ps"}` + "\n",
		"not json":  "ps\n",
		"unknown":   `{"protocol":1,"app":"shop","destination":"production","verb":"ps","shell":"id"}` + "\n",
		"traversal": `{"protocol":1,"app":"../../etc","destination":"production","verb":"ps"}` + "\n",
	} {
		var out bytes.Buffer
		code := Serve(context.Background(), Options{Member: "alice", In: strings.NewReader(in), Out: &out, HostFor: func(string) (remote.Host, error) { return &fakeHost{}, nil }})
		if code == 0 || !strings.Contains(out.String(), `"error"`) {
			t.Errorf("%s: code %d out %s", name, code, out.String())
		}
	}
}

func TestPSAndLogs(t *testing.T) {
	setup(t, "")
	foreign := false
	h := &fakeHost{reply: func(s string) (string, error) {
		switch {
		case strings.Contains(s, "docker inspect --type container"):
			if strings.Contains(s, "'yoho-proxy'") {
				return "abc|||", nil
			}
			if strings.Contains(s, "'yoho-shop-production-other-1'") {
				// Same project name, another App Destination (collision).
				return "def|yoho-shop-production|shop-production|x", nil
			}
			return "c0ffee|yoho-shop-production|shop|production", nil
		case strings.Contains(s, "docker ps -a --no-trunc"):
			if foreign {
				return "c0ffee|shop|production|web|running\nbad|shop-production|x|api|running", nil
			}
			return "c0ffee|shop|production|web|running", nil
		case strings.Contains(s, " ps "):
			return "SERVICE NAME\nweb yoho-shop-production-web-1\n", nil
		}
		return "log line\n", nil
	}}
	var out bytes.Buffer
	g := client(t, "alice", h)
	if err := g.Call(context.Background(), VerbPS, nil, nil, &out, nil); err != nil || !strings.Contains(out.String(), "web yoho-shop-production-web-1") {
		t.Fatalf("ps: %v %q", err, out.String())
	}
	out.Reset()
	if err := g.Call(context.Background(), VerbLogs, LogsArgs{ServicesArgs: ServicesArgs{Services: []string{"web"}}, Lines: 999999}, nil, &out, nil); err != nil || out.String() != "log line\n" {
		t.Fatalf("logs: %v %q", err, out.String())
	}
	if !strings.Contains(h.all(), "docker compose -p 'yoho-shop-production' logs --tail 10000 'web' 2>&1") {
		t.Fatalf("scripts: %s", h.all())
	}
	// Dashboard logs name a container; the Gate pins its ID.
	if err := g.Call(context.Background(), VerbLogs, LogsArgs{Container: "yoho-shop-production-web-1"}, nil, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(h.all(), "docker logs --tail 100 'c0ffee' 2>&1") {
		t.Fatalf("scripts: %s", h.all())
	}
	for _, name := range []string{"yoho-proxy", "yoho-shop-production-other-1"} {
		err := g.Call(context.Background(), VerbLogs, LogsArgs{Container: name}, nil, io.Discard, nil)
		if err == nil || !strings.Contains(err.Error(), "not part of shop/production") {
			t.Fatalf("%s logs: %v", name, err)
		}
	}
	err := g.Call(context.Background(), VerbLogs, LogsArgs{ServicesArgs: ServicesArgs{Services: []string{"web; id"}}}, nil, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "invalid Service name") {
		t.Fatalf("injection: %v", err)
	}
	// A project shared with another App Destination: read verbs refuse.
	foreign = true
	for _, verb := range []string{VerbPS, VerbLogs} {
		err := g.Call(context.Background(), verb, nil, nil, io.Discard, nil)
		if err == nil || !strings.Contains(err.Error(), "has containers of shop-production/x") {
			t.Errorf("%s with a project collision: %v", verb, err)
		}
	}
}

func TestReleasesThroughLocal(t *testing.T) {
	setup(t, "")
	dir := release.Dir("shop", "production", "abc")
	os.MkdirAll(dir, 0o700)
	b, _ := json.Marshal(release.Release{App: "shop", Version: "abc", Status: "deployed", Secrets: map[string]release.SecretAudit{"web/X": {Fingerprint: "f"}}})
	os.WriteFile(filepath.Join(dir, "release.json"), b, 0o600)
	var rels []release.Release
	if err := client(t, "alice", &remote.Local{}).Call(context.Background(), VerbReleases, nil, nil, nil, &rels); err != nil {
		t.Fatal(err)
	}
	if len(rels) != 1 || rels[0].Version != "abc" || rels[0].Secrets != nil {
		t.Fatalf("releases: %+v", rels)
	}
}

const webCompose = `services:
  web:
    image: yoho/shop-production-web:abc
    x-yoho: {secrets: [DB_URL], generate: {SESSION: hex32}}
`

func TestDeployNeedsSettings(t *testing.T) {
	setup(t, "")
	err := client(t, "alice", &fakeHost{}).Call(context.Background(), VerbDeploy, DeployArgs{Version: "abc", Compose: webCompose, Built: []string{"web"}}, nil, io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "has not pushed settings") {
		t.Fatalf("got %v", err)
	}
	var ge *remote.GateError
	if !errors.As(err, &ge) || !strings.Contains(ge.Hint, "yoho access secrets push") {
		t.Fatalf("hint: %+v", ge)
	}
}

func TestDeployRefusedByPolicy(t *testing.T) {
	setup(t, "")
	writeSettings(t, Settings{Server: "srv"})
	h := &fakeHost{}
	evil := "services:\n  web:\n    image: alpine\n    privileged: true\n    volumes: [\"/:/host\"]\n"
	err := client(t, "alice", h).Call(context.Background(), VerbDeploy, DeployArgs{Version: "abc", Compose: evil}, nil, io.Discard, nil)
	var ge *remote.GateError
	if !errors.As(err, &ge) || ge.Code != "denied" || !strings.Contains(ge.Message, "privileged is not allowed") || !strings.Contains(ge.Message, "bind mount /:/host") {
		t.Fatalf("got %v", err)
	}
	if h.all() != "" {
		t.Fatalf("refused deploy ran scripts: %s", h.all())
	}
	e := lastAudit(t)
	if e.Verb != "deploy" || e.Exit != ExitDenied || e.Args["compose_sha256"] == nil || strings.Contains(e.Error, "/host\n") {
		t.Fatalf("audit %+v", e)
	}
}

func TestDeployRefusesForeignArchiveTags(t *testing.T) {
	setup(t, "")
	writeSettings(t, Settings{Server: "srv"})
	h := &fakeHost{}
	tarball := archive(t, true, map[string]string{"manifest.json": `[{"RepoTags":["yoho/shop-production-web:abc","basecamp/kamal-proxy:v0.10.2"]}]`})
	err := client(t, "alice", h).Call(context.Background(), VerbDeploy, DeployArgs{Version: "abc", Compose: webCompose, Built: []string{"web"}, Archive: true}, bytes.NewReader(tarball), io.Discard, nil)
	if err == nil || !strings.Contains(err.Error(), "names basecamp/kamal-proxy:v0.10.2") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(h.all(), "docker load") {
		t.Fatal("loaded a refused archive")
	}
}

func TestImagesVerb(t *testing.T) {
	setup(t, "")
	writeSettings(t, Settings{})
	h := &fakeHost{reply: func(s string) (string, error) {
		if strings.Contains(s, "web:abc") {
			return "fp-web", nil
		}
		return "", nil
	}}
	var missing []string
	g := client(t, "alice", h)
	if err := g.Call(context.Background(), "images", ImagesArgs{Images: map[string]string{"yoho/shop-production-web:abc": "fp-web", "yoho/shop-production-worker:abc": "fp-w"}}, nil, nil, &missing); err != nil {
		t.Fatal(err)
	}
	if strings.Join(missing, ",") != "yoho/shop-production-worker:abc" {
		t.Fatalf("missing %v", missing)
	}
	err := g.Call(context.Background(), "images", ImagesArgs{Images: map[string]string{"yoho/other-production-web:abc": "x"}}, nil, nil, &missing)
	if err == nil || !strings.Contains(err.Error(), "not one Yoho builds for shop/production") {
		t.Fatalf("got %v", err)
	}
}

func TestStoredSecrets(t *testing.T) {
	setup(t, "")
	r := load(t, webCompose)
	_, _, err := storedSecrets("shop", "production", r.Ext)
	if err == nil || !strings.Contains(err.Error(), "web/DB_URL") || !strings.Contains(err.Error(), "has not pushed secrets") {
		t.Fatalf("got %v", err)
	}
	h := &remote.Local{}
	pushed, err := deploy.WriteSecretsGeneration(context.Background(), h, "shop", "production", "access-push", map[string]map[string]string{"web": {"DB_URL": "postgres://u:p@db/x", "OLD": "x"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(release.AccessSecretsFile("shop", "production"), []byte(pushed+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A later deploy (here: one that declared no secrets) writes a newer,
	// emptier generation; scoped deploys keep reading the pushed one.
	if err := os.MkdirAll(release.SecretsDir("shop", "production", "29990101T000000Z-v9"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, gen, err := storedSecrets("shop", "production", r.Ext)
	if err != nil || gen != pushed {
		t.Fatal(gen, err)
	}
	// Only declared names: OLD is not delivered, SESSION is generated.
	if len(got["web"]) != 1 || got["web"]["DB_URL"] != "postgres://u:p@db/x" {
		t.Fatalf("secrets %v", got)
	}
}
