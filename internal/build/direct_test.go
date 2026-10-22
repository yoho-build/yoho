package build

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
