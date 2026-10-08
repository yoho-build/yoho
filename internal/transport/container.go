package transport

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/remote"
)

// Images built by Apple's container CLI live in its own store. They ship via
// `container image save` (an OCI layout archive) into `docker load` on the
// Server. Docker's classic overlay2 store (e.g. Docker 27) cannot load a
// pure OCI layout ("open .../blobs/json: no such file or directory"), so the
// archive gets a docker-archive manifest.json pointing at the OCI blobs;
// both store kinds load that and tag it with RepoTags.

// EngineContainer marks images in Apple container's store (build.Image.Engine).
const EngineContainer = build.EngineContainer

func (p *pusher) buildExec() build.Exec {
	return func(ctx context.Context, c build.Command) error {
		return p.o.Exec(ctx, Command{Argv: c.Argv, Stdin: c.Stdin, Stdout: c.Stdout, Stderr: c.Stderr})
	}
}

// containerIDs returns each image's Fingerprint from container's store.
func (p *pusher) containerIDs(ctx context.Context) (map[string]string, error) {
	res := map[string]string{}
	for _, img := range p.o.Images {
		ci, err := build.InspectContainerImage(ctx, p.buildExec(), img)
		if err != nil {
			return nil, fmt.Errorf("image not found in container's store: %w", err)
		}
		v, ok := ci.Variant(p.o.Platform)
		if !ok {
			return nil, fmt.Errorf("image %s has no %s variant in container's store", img, platformOr(p.o.Platform, "single"))
		}
		res[img] = v.Fingerprint
	}
	return res, nil
}

func platformOr(p, def string) string {
	if p == "" {
		return def
	}
	return p
}

// ContainerSaveArgv is the local export of imgs to out.
func ContainerSaveArgv(platform, out string, imgs []string) []string {
	argv := []string{"container", "image", "save"}
	if platform != "" {
		argv = append(argv, "--platform", platform)
	}
	return append(append(argv, "--output", out), imgs...)
}

// ContainerLoadScript loads a docker archive from stdin and makes sure each
// ref exists, tagging it from its config digest (the image ID in the
// overlay2 store) if the loader named it differently.
func ContainerLoadScript(configs map[string]string, refs []string) string {
	s := "set -eu\ndocker load"
	for _, ref := range refs {
		id := configs[ref]
		if id == "" {
			continue
		}
		q := remote.Quote(ref)
		s += "\ndocker image inspect " + q + " >/dev/null 2>&1 || docker tag " + remote.Quote(id) + " " + q
	}
	return s
}

// loadContainer saves imgs from container's store to a private temp file
// (container cannot write the archive to stdout) and streams it, with a
// docker-archive manifest.json added, into `docker load` on h.
func (p *pusher) loadContainer(ctx context.Context, h remote.Host, imgs []string) error {
	dir, err := os.MkdirTemp("", "yoho-images-") // 0700
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	archive := filepath.Join(dir, "images.tar")
	var stderr bytes.Buffer
	// container prints saved refs on stdout; discard.
	if err := p.o.Exec(ctx, Command{Argv: ContainerSaveArgv(p.o.Platform, archive, imgs), Stdout: io.Discard, Stderr: &stderr}); err != nil {
		return fmt.Errorf("container image save: %w %s", err, strings.TrimSpace(stderr.String()))
	}
	if err := os.Chmod(archive, 0o600); err != nil {
		return err
	}
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	configs, extra, err := DockerArchiveManifest(f, p.o.Platform, imgs)
	if err != nil {
		return err
	}
	for _, img := range imgs {
		if configs[img] == "" {
			return fmt.Errorf("image archive does not contain %s", img)
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	writeErr := make(chan error, 1)
	go func() {
		err := appendTar(pw, f, extra)
		pw.CloseWithError(err)
		writeErr <- err
	}()
	runErr := h.Run(ctx, remote.Cmd{Script: ContainerLoadScript(configs, imgs), Stdin: pr, Stdout: p.out, Stderr: p.out})
	pr.CloseWithError(errors.New("docker load ended"))
	if runErr != nil {
		cancel()
		<-writeErr
		return runErr
	}
	return <-writeErr
}

// appendTar copies the tar in r to w, adding manifest.json when non-nil.
func appendTar(w io.Writer, r io.Reader, manifest []byte) error {
	tr := tar.NewReader(r)
	tw := tar.NewWriter(w)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read image archive: %w", err)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return err
		}
	}
	if manifest != nil {
		if err := tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0o644, Size: int64(len(manifest)), Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(manifest); err != nil {
			return err
		}
	}
	return tw.Close()
}

type ociDescriptor struct {
	MediaType   string            `json:"mediaType"`
	Digest      string            `json:"digest"`
	Annotations map[string]string `json:"annotations"`
	Platform    *struct {
		OS           string `json:"os"`
		Architecture string `json:"architecture"`
		Variant      string `json:"variant"`
	} `json:"platform"`
}

type ociIndex struct {
	Manifests []ociDescriptor `json:"manifests"`
	// Image manifest fields (same struct for brevity).
	Config *ociDescriptor  `json:"config"`
	Layers []ociDescriptor `json:"layers"`
}

type dockerManifestEntry struct {
	Config   string
	RepoTags []string
	Layers   []string
}

// DockerArchiveManifest reads an OCI layout archive and returns ref ->
// config digest for refs, plus the docker-archive manifest.json to append
// (nil when the archive already has one).
func DockerArchiveManifest(r io.Reader, platform string, refs []string) (map[string]string, []byte, error) {
	const maxMeta = 1 << 20
	files := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("read image archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || hdr.Size > maxMeta {
			continue // layers
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, nil, err
		}
		files[strings.TrimPrefix(hdr.Name, "./")] = b
	}
	configs := map[string]string{}
	if b, ok := files["manifest.json"]; ok {
		var entries []dockerManifestEntry
		if err := json.Unmarshal(b, &entries); err != nil {
			return nil, nil, fmt.Errorf("parse manifest.json: %w", err)
		}
		for _, e := range entries {
			for _, t := range e.RepoTags {
				configs[normalizeRef(t)] = "sha256:" + path0(e.Config)
			}
		}
		return pick(configs, refs), nil, nil
	}
	b, ok := files["index.json"]
	if !ok {
		return nil, nil, errors.New("image archive has neither index.json nor manifest.json")
	}
	var idx ociIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return nil, nil, fmt.Errorf("parse index.json: %w", err)
	}
	blob := func(d string) ([]byte, error) {
		b, ok := files["blobs/"+strings.Replace(d, ":", "/", 1)]
		if !ok {
			return nil, fmt.Errorf("image archive lacks blob %s", d)
		}
		return b, nil
	}
	var entries []dockerManifestEntry
	for _, d := range idx.Manifests {
		name := d.Annotations["io.containerd.image.name"]
		if name == "" {
			name = d.Annotations["org.opencontainers.image.ref.name"]
		}
		if name == "" || !strings.Contains(name, ":") && !strings.Contains(name, "@") {
			continue // bare tag annotation without repository
		}
		m, err := resolveManifest(blob, d, platform, 0)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", name, err)
		}
		e := dockerManifestEntry{Config: "blobs/" + strings.Replace(m.Config.Digest, ":", "/", 1), RepoTags: []string{normalizeRef(name)}}
		for _, l := range m.Layers {
			e.Layers = append(e.Layers, "blobs/"+strings.Replace(l.Digest, ":", "/", 1))
		}
		entries = append(entries, e)
		configs[normalizeRef(name)] = m.Config.Digest
	}
	if len(entries) == 0 {
		return nil, nil, errors.New("image archive index.json names no images")
	}
	out, err := json.Marshal(entries)
	if err != nil {
		return nil, nil, err
	}
	return pick(configs, refs), out, nil
}

// resolveManifest walks nested indexes to the image manifest for platform.
func resolveManifest(blob func(string) ([]byte, error), d ociDescriptor, platform string, depth int) (ociIndex, error) {
	if depth > 4 {
		return ociIndex{}, errors.New("image index nested too deeply")
	}
	b, err := blob(d.Digest)
	if err != nil {
		return ociIndex{}, err
	}
	var m ociIndex
	if err := json.Unmarshal(b, &m); err != nil {
		return ociIndex{}, fmt.Errorf("parse %s: %w", d.Digest, err)
	}
	if m.Config != nil {
		return m, nil
	}
	var cands []ociDescriptor
	for _, c := range m.Manifests {
		if c.Platform == nil || c.Platform.OS == "unknown" {
			continue // attestations
		}
		p := c.Platform.OS + "/" + c.Platform.Architecture
		pv := p
		if c.Platform.Variant != "" {
			pv += "/" + c.Platform.Variant
		}
		if platform == "" || platform == p || platform == pv {
			cands = append(cands, c)
		}
	}
	if len(cands) != 1 {
		return ociIndex{}, fmt.Errorf("found %d manifests for platform %q", len(cands), platformOr(platform, "any"))
	}
	return resolveManifest(blob, cands[0], platform, depth+1)
}

// normalizeRef strips the implicit Docker Hub prefixes container adds.
func normalizeRef(r string) string {
	r = strings.TrimPrefix(r, "docker.io/library/")
	return strings.TrimPrefix(r, "docker.io/")
}

func path0(p string) string {
	p = strings.TrimSuffix(filepath.Base(p), ".json")
	return p
}

func pick(configs map[string]string, refs []string) map[string]string {
	out := map[string]string{}
	for _, r := range refs {
		if c, ok := configs[normalizeRef(r)]; ok {
			out[r] = c
		}
	}
	return out
}
