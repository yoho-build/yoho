package build

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

// scriptHost records scripts and fails those containing failOn.
type scriptHost struct {
	remote.Host
	name    string
	scripts []string
	failOn  string
}

func (h *scriptHost) Name() string { return h.name }
func (h *scriptHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	h.scripts = append(h.scripts, c.Script)
	if h.failOn != "" && strings.Contains(c.Script, h.failOn) {
		return "", errors.New("Permission denied (publickey)")
	}
	return "", nil
}
func (h *scriptHost) Run(ctx context.Context, c remote.Cmd) error {
	h.scripts = append(h.scripts, c.Script)
	if c.Stdout != nil {
		c.Stdout.Write([]byte("noise\n" + directMarker + "4242\n"))
	}
	return nil
}

func sshServer(t *testing.T, target string, opts remote.SSHOptions) *remote.SSH {
	t.Helper()
	opts.ControlDir = t.TempDir()
	s, err := remote.NewSSH("web", target, false, opts)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDirectScriptAndStats(t *testing.T) {
	src := &scriptHost{name: "builder"}
	dst := sshServer(t, "deploy@10.0.0.5:2222", remote.SSHOptions{})
	st, err := Direct(context.Background(), src, dst, []string{"reg/app-web:v1", "it's:x"}, "linux/amd64", nil)
	if err != nil {
		t.Fatal(err)
	}
	if st.Bytes != 4242 || st.Duration <= 0 || !strings.Contains(st.Image, "reg/app-web:v1") {
		t.Fatalf("stats %+v", st)
	}
	if len(src.scripts) != 2 {
		t.Fatalf("want probe then stream, got %d scripts", len(src.scripts))
	}
	probe, script := src.scripts[0], src.scripts[1]
	if !strings.HasSuffix(probe, "-- 'deploy@10.0.0.5' true") {
		t.Errorf("probe %q", probe)
	}
	for _, want := range []string{"set -eu", "docker save --platform 'linux/amd64' 'reg/app-web:v1' 'it'\\''s:x'", "gzip -1",
		"'BatchMode=yes'", "'StrictHostKeyChecking=yes'", "'-p' '2222'", "-- 'deploy@10.0.0.5' 'docker load >&2'"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	for _, bad := range []string{"accept-new", "StrictHostKeyChecking=no", "ControlPath", "-i "} {
		if strings.Contains(probe+script, bad) {
			t.Errorf("must not contain %q", bad)
		}
	}
}

func TestDirectProbeFailureFallsBack(t *testing.T) {
	src := &scriptHost{name: "builder", failOn: " true"}
	dst := sshServer(t, "deploy@10.0.0.5", remote.SSHOptions{})
	_, err := Direct(context.Background(), src, dst, []string{"app:v1"}, "", nil)
	if !errors.Is(err, ErrDirectUnavailable) {
		t.Fatalf("want ErrDirectUnavailable, got %v", err)
	}
	if len(src.scripts) != 1 {
		t.Fatalf("no stream may run after a failed probe: %v", src.scripts)
	}
}

func TestDirectUnavailableDestinations(t *testing.T) {
	src := &scriptHost{name: "builder"}
	for name, dst := range map[string]remote.Host{
		"local":      &remote.Local{},
		"cloudflare": sshServer(t, "deploy@10.0.0.5", remote.SSHOptions{CloudflareHostname: "ssh.example.com", Exec: func(context.Context, []string, io.Reader, io.Writer, io.Writer) error { return nil }}),
	} {
		if _, err := Direct(context.Background(), src, dst, []string{"app:v1"}, "", nil); !errors.Is(err, ErrDirectUnavailable) {
			t.Errorf("%s: want ErrDirectUnavailable, got %v", name, err)
		}
	}
	if len(src.scripts) != 0 {
		t.Fatalf("builder must not be contacted: %v", src.scripts)
	}
}

// TestDirectCountsRealStream runs the script with stub docker and ssh
// binaries: bytes are the gzip stream the Server received.
func TestDirectCountsRealStream(t *testing.T) {
	bin, out := t.TempDir(), filepath.Join(t.TempDir(), "received")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("docker", `if [ -n "${STUB_FAIL:-}" ]; then echo partial; exit 3; fi; head -c 100000 /dev/zero`)
	write("ssh", `for a; do last=$a; done; [ "$last" = true ] && exit 0; cat >"$STUB_OUT"`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STUB_OUT", out)
	dst := sshServer(t, "deploy@10.0.0.5", remote.SSHOptions{})
	st, err := Direct(context.Background(), &remote.Local{HostName: "builder"}, dst, []string{"app:v1"}, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(out)
	if err != nil || st.Bytes == 0 || st.Bytes != fi.Size() {
		t.Fatalf("counted %d bytes, Server got %v (%v)", st.Bytes, fi, err)
	}
	t.Setenv("STUB_FAIL", "1")
	if _, err := Direct(context.Background(), &remote.Local{HostName: "builder"}, dst, []string{"app:v1"}, "", nil); err == nil || !strings.Contains(err.Error(), "docker save failed") {
		t.Fatalf("a failed docker save must fail the transfer, got %v", err)
	}
}

// dockerHost is a thread-safe fake host with an image store (ref ->
// Fingerprint). Probes fail when unreachable; streams print a byte count.
type dockerHost struct {
	remote.Host
	name        string
	mu          sync.Mutex
	images      map[string]string
	scripts     []string
	unreachable string // probe fails for this destination
	oldDocker   bool
}

func (h *dockerHost) Name() string { return h.name }
func (h *dockerHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scripts = append(h.scripts, c.Script)
	switch {
	case strings.HasPrefix(c.Script, "'ssh'"):
		if h.unreachable != "" && strings.Contains(c.Script, h.unreachable) {
			return "", errors.New("Host key verification failed")
		}
		return "", nil
	case strings.HasPrefix(c.Script, "docker save --help"):
		if h.oldDocker {
			return "Usage: docker save [OPTIONS] IMAGE", nil
		}
		return "      --platform string", nil
	case strings.HasPrefix(c.Script, "docker image inspect"):
		for ref, fp := range h.images {
			if strings.Contains(c.Script, remote.Quote(ref)) {
				return fp + "\n", nil
			}
		}
	}
	return "", nil
}
func (h *dockerHost) Run(ctx context.Context, c remote.Cmd) error {
	h.mu.Lock()
	h.scripts = append(h.scripts, c.Script)
	h.mu.Unlock()
	c.Stdout.Write([]byte(directMarker + "1000\n"))
	return nil
}

func TestUseDirect(t *testing.T) {
	ctx := context.Background()
	web := sshServer(t, "deploy@10.0.0.5", remote.SSHOptions{})
	remoteB := func(d config.BuilderDirect) config.Builder {
		return config.Builder{Location: "remote", Remote: "b@builder", Direct: d}
	}
	one := []string{"linux/amd64"}
	for _, tc := range []struct {
		name      string
		b         config.Builder
		mode      string
		platforms []string
		src       *dockerHost
		use       bool
		reason    string
		err       string
	}{
		{"local builder", config.Builder{}, "", one, &dockerHost{}, false, "", ""},
		{"off", remoteB(config.DirectOff), "", one, &dockerHost{}, false, "", ""},
		{"auto reachable", remoteB(""), "", one, &dockerHost{}, true, "", ""},
		{"auto load", remoteB(config.DirectAuto), "load", one, &dockerHost{}, true, "", ""},
		{"auto unreachable", remoteB(""), "", one, &dockerHost{unreachable: "10.0.0.5"}, false, "cannot ssh to web", ""},
		{"auto multi-platform", remoteB(""), "", []string{"linux/amd64", "linux/arm64"}, &dockerHost{}, false, "2 platforms", ""},
		{"auto registry", remoteB(""), "registry", one, &dockerHost{}, false, "transport.mode registry", ""},
		{"true unreachable", remoteB(config.DirectOn), "", one, &dockerHost{unreachable: "10.0.0.5"}, false, "", "builder.direct is true but"},
	} {
		tc.src.name = "builder"
		use, reason, err := UseDirect(ctx, tc.b, tc.mode, tc.platforms, tc.src, []remote.Host{web})
		if use != tc.use || !strings.Contains(reason, tc.reason) || (tc.reason == "") != (reason == "") {
			t.Errorf("%s: use=%v reason=%q", tc.name, use, reason)
		}
		if (err != nil) != (tc.err != "") || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: err %v, want %q", tc.name, err, tc.err)
		}
	}
	// Cloudflare-only and local Servers cannot be reached from the Builder.
	if use, reason, _ := UseDirect(ctx, remoteB(""), "", one, &dockerHost{name: "b"}, []remote.Host{&remote.Local{}}); use || reason == "" {
		t.Errorf("local Server: use=%v reason=%q", use, reason)
	}
}

func TestShipDirectSkipsPresentImages(t *testing.T) {
	src := &dockerHost{name: "builder", images: map[string]string{"app-web:v1": "fp-web", "app-api:v1": "fp-api"}}
	// Servers answer every command with no output: they have no images.
	none := remote.SSHOptions{Exec: func(context.Context, []string, io.Reader, io.Writer, io.Writer) error { return nil }}
	a := sshServer(t, "deploy@10.0.0.5", none)
	b := sshServer(t, "deploy@10.0.0.6", none)
	var out strings.Builder
	res, err := ShipDirect(context.Background(), src, []remote.Host{a, b}, []string{"app-web:v1", "app-api:v1"}, "linux/amd64", &out)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Skipped || res[0].Stats.Bytes != 1000 || res[1].Stats.Bytes != 1000 {
		t.Fatalf("results %+v", res)
	}
	var streams int
	for _, s := range src.scripts {
		if strings.Contains(s, "docker save --platform 'linux/amd64' 'app-web:v1' 'app-api:v1'") {
			streams++
		}
	}
	if streams != 2 {
		t.Fatalf("want one stream per Server, got %d:\n%s", streams, strings.Join(src.scripts, "\n---\n"))
	}

	if _, err := ShipDirect(context.Background(), src, []remote.Host{a}, []string{"gone:v1"}, "", nil); err == nil || !strings.Contains(err.Error(), "missing on the Builder") {
		t.Fatalf("missing image: %v", err)
	}
}

func TestShipDirectSkipAndOldDocker(t *testing.T) {
	src := &dockerHost{name: "builder", images: map[string]string{"app:v1": "fp"}, oldDocker: true}
	dst := &dockerHost{name: "web", images: map[string]string{"app:v1": "fp"}}
	res, err := ShipDirect(context.Background(), src, []remote.Host{dst}, []string{"app:v1"}, "linux/amd64", nil)
	if err != nil || len(res) != 1 || !res[0].Skipped || res[0].Host != "web" {
		t.Fatalf("want skip: %+v %v", res, err)
	}
	dst.images = nil
	src.scripts = nil
	// A non-SSH Server cannot be reached from the Builder.
	if _, err := ShipDirect(context.Background(), src, []remote.Host{dst}, []string{"app:v1"}, "linux/amd64", nil); !errors.Is(err, ErrDirectUnavailable) {
		t.Fatalf("want ErrDirectUnavailable, got %v", err)
	}
}
