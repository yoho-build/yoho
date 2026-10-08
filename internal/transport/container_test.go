package transport

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-dev/yoho/internal/build"
	"github.com/yoho-dev/yoho/internal/config"
	"github.com/yoho-dev/yoho/internal/remote"
)

// ociArchive mimics `container image save --platform linux/amd64`: an
// index.json naming a nested index with one amd64 manifest plus an
// attestation entry.
func ociArchive(t *testing.T, ref string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	add := func(name, body string) {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		io.WriteString(tw, body)
	}
	tw.WriteHeader(&tar.Header{Name: "./", Mode: 0o755, Typeflag: tar.TypeDir})
	add("./oci-layout", `{"imageLayoutVersion":"1.0.0"}`)
	add("./blobs/sha256/layer1", "LAYER1")
	add("./blobs/sha256/cfg", `{"architecture":"amd64","os":"linux"}`)
	add("./blobs/sha256/man", `{"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"sha256:cfg"},"layers":[{"digest":"sha256:layer1"}]}`)
	add("./blobs/sha256/idx", `{"manifests":[{"digest":"sha256:man","platform":{"os":"linux","architecture":"amd64"}},{"digest":"sha256:att","platform":{"os":"unknown","architecture":"unknown"}}]}`)
	add("./index.json", `{"manifests":[{"digest":"sha256:idx","mediaType":"application/vnd.oci.image.index.v1+json","annotations":{"io.containerd.image.name":"`+ref+`","org.opencontainers.image.ref.name":"`+ref+`"}}]}`)
	tw.Close()
	return buf.Bytes()
}

func TestDockerArchiveManifest(t *testing.T) {
	configs, m, err := DockerArchiveManifest(bytes.NewReader(ociArchive(t, "docker.io/yoho/app-web:v1")), "linux/amd64", []string{"yoho/app-web:v1"})
	if err != nil {
		t.Fatal(err)
	}
	if configs["yoho/app-web:v1"] != "sha256:cfg" {
		t.Fatalf("configs %v", configs)
	}
	var got []dockerManifestEntry
	if err := json.Unmarshal(m, &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Config != "blobs/sha256/cfg" || got[0].RepoTags[0] != "yoho/app-web:v1" || strings.Join(got[0].Layers, ",") != "blobs/sha256/layer1" {
		t.Fatalf("manifest %s", m)
	}
	if _, _, err := DockerArchiveManifest(bytes.NewReader(ociArchive(t, "a:1")), "linux/arm64", nil); err == nil {
		t.Fatal("want platform error")
	}
}

const containerInspect = `[{"id":"idx","variants":[{"platform":{"os":"linux","architecture":"amd64"},"config":{"os":"linux","architecture":"amd64","created":"2026-10-08T15:38:52.78Z","rootfs":{"diff_ids":["sha256:d1"]}}}]}]`

const containerFP = "linux/amd64|2026-10-08T15:38:52.78Z|sha256:d1,"

type containerLocal struct {
	fakeLocal
	archive []byte
}

func (f *containerLocal) exec(ctx context.Context, c Command) error {
	a := strings.Join(c.Argv, " ")
	switch {
	case strings.HasPrefix(a, "container image inspect"):
		io.WriteString(c.Stdout, containerInspect)
	case strings.HasPrefix(a, "container image save"):
		for i, x := range c.Argv {
			if x == "--output" {
				if err := os.WriteFile(c.Argv[i+1], f.archive, 0o644); err != nil {
					return err
				}
			}
		}
		io.WriteString(c.Stdout, "yoho/app-web:v1\n")
	}
	return f.fakeLocal.exec(ctx, c)
}

func TestContainerEngineLoad(t *testing.T) {
	img := "yoho/app-web:v1"
	f := &containerLocal{archive: ociArchive(t, img)}
	fresh := &fakeHost{name: "fresh"}
	same := &fakeHost{name: "same", ids: map[string]string{img: containerFP}}
	res, err := Push(context.Background(), PushOptions{Images: []string{img}, Hosts: []remote.Host{fresh, same},
		Platform: "linux/amd64", Engine: EngineContainer, Exec: f.exec})
	if err != nil {
		t.Fatal(err)
	}
	if res[0] != (Result{"fresh", MethodLoad}) || res[1] != (Result{"same", MethodSkip}) {
		t.Fatalf("results %v", res)
	}
	if f.has("docker") {
		t.Fatalf("local docker used: %v", f.calls)
	}
	var saveOut string
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), "container image save --platform linux/amd64 --output ") {
			saveOut = c[6]
		}
	}
	if saveOut == "" || !strings.HasSuffix(saveOut, "/images.tar") {
		t.Fatalf("calls %v", f.calls)
	}
	if _, err := os.Stat(saveOut); !os.IsNotExist(err) {
		t.Fatal("temp archive not removed")
	}
	want := "set -eu\ndocker load\ndocker image inspect 'yoho/app-web:v1' >/dev/null 2>&1 || docker tag 'sha256:cfg' 'yoho/app-web:v1'"
	li := fresh.ran("set -eu\ndocker load")
	if len(li) != 1 || fresh.scripts[li[0]] != want {
		t.Fatalf("scripts %q", fresh.scripts)
	}
	names := map[string]string{}
	tr := tar.NewReader(strings.NewReader(fresh.stdins[li[0]]))
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		names[h.Name] = string(b)
	}
	if names["./blobs/sha256/layer1"] != "LAYER1" || !strings.Contains(names["manifest.json"], `"RepoTags":["yoho/app-web:v1"]`) {
		t.Fatalf("streamed archive %v", names)
	}
	if len(same.ran("set -eu\ndocker load")) != 0 {
		t.Fatal("same host should skip")
	}
	// pussh / registry are not possible for container-store images.
	if _, err := Push(context.Background(), PushOptions{Images: []string{img}, Hosts: []remote.Host{fresh}, Mode: MethodPussh, Engine: EngineContainer, Exec: f.exec}); err == nil {
		t.Fatal("want mode error")
	}
}

// YOHO_E2E=1 YOHO_E2E_SSH=user@host on an Apple silicon Mac with container
// running: build a tiny linux/amd64 image with container, ship it, check
// the Server sees amd64, ship again (must skip), then remove it.
func TestE2EContainerPush(t *testing.T) {
	target := os.Getenv("YOHO_E2E_SSH")
	if os.Getenv("YOHO_E2E") != "1" || target == "" {
		t.Skip("YOHO_E2E=1 and YOHO_E2E_SSH not set")
	}
	if _, err := exec.LookPath("container"); err != nil {
		t.Skip("container CLI not installed")
	}
	ctx := context.Background()
	dir := t.TempDir()
	os.WriteFile(dir+"/Dockerfile", []byte("FROM alpine\nRUN --mount=type=secret,id=TOK,env=TOK test \"$TOK\" = ok && uname -m > /arch\nCMD cat /arch\n"), 0o644)
	p := &types.Project{WorkingDir: dir, Services: types.Services{"w": {WorkloadSpec: types.WorkloadSpec{Build: &types.BuildConfig{Context: "."}}}}}
	imgs, err := build.Images(ctx, build.Options{App: "yoho-e2e-container", Version: "t1", Project: p, Platforms: []string{"linux/amd64"},
		Builder: config.Builder{Engine: build.EngineContainer, Secrets: []string{"TOK"}}, BuildEnv: []string{"TOK=ok"}, Out: os.Stderr})
	if err != nil {
		t.Fatal(err)
	}
	img := imgs["w"]
	if img.Engine != build.EngineContainer {
		t.Fatalf("%+v", img)
	}
	defer osExec(ctx, Command{Argv: []string{"container", "image", "delete", img.Ref}, Stdout: io.Discard, Stderr: io.Discard})
	h, err := remote.NewSSH("e2e", target, false, remote.SSHOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	rm := "docker image rm " + remote.Quote(img.Ref) + " >/dev/null 2>&1 || true"
	h.Run(ctx, remote.Cmd{Script: rm})
	defer h.Run(ctx, remote.Cmd{Script: rm})
	o := PushOptions{Images: []string{img.Ref}, Hosts: []remote.Host{h}, Platform: "linux/amd64", Engine: img.Engine, Out: os.Stderr}
	res, err := Push(ctx, o)
	if err != nil || res[0].Method != MethodLoad {
		t.Fatalf("%v %v", res, err)
	}
	arch, err := h.Output(ctx, remote.Cmd{Script: "docker image inspect --format '{{.Os}}/{{.Architecture}}' " + remote.Quote(img.Ref)})
	if err != nil || arch != "linux/amd64" {
		t.Fatalf("server image %q %v", arch, err)
	}
	if out, err := h.Output(ctx, remote.Cmd{Script: "docker run --rm " + remote.Quote(img.Ref)}); err != nil || out != "x86_64" {
		t.Fatalf("run %q %v", out, err)
	}
	res, err = Push(ctx, o)
	if err != nil || res[0].Method != MethodSkip {
		t.Fatalf("second push did not skip: %v %v", res, err)
	}
}
