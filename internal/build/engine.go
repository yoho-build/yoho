package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/config"
)

// Local build engines.
const (
	EngineDocker    = "docker"
	EngineContainer = "container" // Apple's container CLI (github.com/apple/container)
)

// ResolveEngine picks the local build engine. auto chooses Apple container
// on Apple silicon Macs when the `container` CLI is on PATH and its system
// service answers (statusFn, i.e. `container system status`), else Docker.
// Only location=local uses container; remote and server builds need buildx
// or a Server's Docker. reason explains the choice for humans.
func ResolveEngine(cfg config.Builder, goos, goarch string, lookPath func(string) (string, error), statusFn func() error) (engine, reason string) {
	if loc := cfg.Location; loc != "" && loc != "local" {
		return EngineDocker, "builder.location=" + loc + " builds with Docker"
	}
	switch cfg.Engine {
	case EngineDocker:
		return EngineDocker, "builder.engine=docker"
	case EngineContainer:
		return EngineContainer, "builder.engine=container"
	}
	if goos != "darwin" || goarch != "arm64" {
		return EngineDocker, "Apple container needs an Apple silicon Mac (this is " + goos + "/" + goarch + ")"
	}
	if lookPath == nil {
		return EngineDocker, "container CLI lookup unavailable"
	}
	if _, err := lookPath("container"); err != nil {
		return EngineDocker, "container CLI not installed (run `yoho builder setup`)"
	}
	if statusFn != nil {
		if err := statusFn(); err != nil {
			return EngineDocker, "container system service is not running (run `container system start` or `yoho builder setup`)"
		}
	}
	return EngineContainer, "Apple silicon Mac with container installed and running"
}

// ContainerStatus runs `container system status` through ex.
func ContainerStatus(ctx context.Context, ex Exec) error {
	return ex(ctx, Command{Argv: []string{"container", "system", "status"}, Stdout: io.Discard, Stderr: io.Discard})
}

// containerUnsupported lists compose build options `container build` lacks.
func containerUnsupported(b *types.BuildConfig) []string {
	var u []string
	if b.Network != "" {
		u = append(u, "network")
	}
	if len(b.CacheFrom) > 0 {
		u = append(u, "cache_from")
	}
	if len(b.CacheTo) > 0 {
		u = append(u, "cache_to")
	}
	if len(b.AdditionalContexts) > 0 {
		u = append(u, "additional_contexts")
	}
	for _, k := range b.SSH {
		if k.ID != "default" || k.Path != "" {
			u = append(u, "ssh (only `default` is supported)")
			break
		}
	}
	if isRemoteContext(b.Context) {
		u = append(u, "remote build context")
	}
	return u
}

// ContainerBuildArgv renders `container build` for one Service. Build
// secrets are referenced by env name only; values travel in the process env.
// Build args without a value take it from env (container needs key=val).
func ContainerBuildArgv(o Options, service string, b *types.BuildConfig, s spec, ref string, getenv func(string) string) ([]string, error) {
	a := []string{"container", "build", "--progress", "plain"}
	for _, p := range o.Platforms {
		a = append(a, "--platform", p)
	}
	a = append(a, "--tag", ref)
	if s.dockerfile != "" {
		a = append(a, "--file", s.dockerfile)
	}
	for _, k := range sortedKeys(b.Args) {
		if v := b.Args[k]; v != nil {
			a = append(a, "--build-arg", k+"="+*v)
		} else if v := getenv(k); v != "" {
			a = append(a, "--build-arg", k+"="+v)
		}
	}
	a = append(a, labelArgs(o, service, b)...)
	if b.Target != "" {
		a = append(a, "--target", b.Target)
	}
	if b.NoCache {
		a = append(a, "--no-cache")
	}
	if b.Pull {
		a = append(a, "--pull")
	}
	if len(b.SSH) > 0 {
		a = append(a, "--ssh", "default")
	}
	sec, err := secretArgs(o, b, func(p string) (string, error) { return p, nil })
	if err != nil {
		return nil, err
	}
	a = append(a, sec...)
	a = append(a, o.BuildSecretArgs...)
	return append(a, s.context), nil
}

func buildContainer(ctx context.Context, o Options, service string, svc types.ServiceConfig, ref string) (string, error) {
	s := localSpec(o, svc.Build)
	if s.inline != "" {
		// container build has no stdin Dockerfile; use a private temp file.
		dir, err := os.MkdirTemp("", "yoho-dockerfile-")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(dir)
		s.dockerfile = filepath.Join(dir, "Dockerfile")
		if err := os.WriteFile(s.dockerfile, []byte(s.inline), 0o600); err != nil {
			return "", err
		}
		s.inline = ""
	}
	env := envLookup(o.BuildEnv)
	argv, err := ContainerBuildArgv(o, service, svc.Build, s, ref, env)
	if err != nil {
		return "", err
	}
	if err := o.Exec(ctx, Command{Argv: argv, Env: o.BuildEnv, Stdout: o.Out, Stderr: o.Out}); err != nil {
		return "", err
	}
	img, err := InspectContainerImage(ctx, o.Exec, ref)
	if err != nil {
		return "", nil // ID is best effort
	}
	return img.ID, nil
}

// envLookup reads extra KEY=value entries first, then the process env.
func envLookup(kv []string) func(string) string {
	return func(k string) string {
		for i := len(kv) - 1; i >= 0; i-- {
			if key, v, ok := strings.Cut(kv[i], "="); ok && key == k {
				return v
			}
		}
		return os.Getenv(k)
	}
}

// ContainerImage is the part of `container image inspect` Yoho uses.
type ContainerImage struct {
	ID       string // sha256:<index digest>
	Variants []ContainerVariant
}

// ContainerVariant is one platform of an image in container's store.
type ContainerVariant struct {
	Platform    string // os/arch[/variant]
	Fingerprint string // same format as Docker's transport.Fingerprint
}

type containerInspect struct {
	ID       string `json:"id"`
	Variants []struct {
		Platform struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Variant      string `json:"variant"`
		} `json:"platform"`
		Config struct {
			OS           string `json:"os"`
			Architecture string `json:"architecture"`
			Created      string `json:"created"`
			RootFS       struct {
				DiffIDs []string `json:"diff_ids"`
			} `json:"rootfs"`
		} `json:"config"`
	} `json:"variants"`
}

// InspectContainerImage reads ref from container's image store.
func InspectContainerImage(ctx context.Context, ex Exec, ref string) (ContainerImage, error) {
	var out, stderr bytes.Buffer
	if err := ex(ctx, Command{Argv: []string{"container", "image", "inspect", ref}, Stdout: &out, Stderr: &stderr}); err != nil {
		return ContainerImage{}, fmt.Errorf("container image inspect %s: %w %s", ref, err, strings.TrimSpace(stderr.String()))
	}
	return ParseContainerInspect(out.Bytes())
}

// ParseContainerInspect parses `container image inspect` output (one image).
func ParseContainerInspect(b []byte) (ContainerImage, error) {
	var raw []containerInspect
	if err := json.Unmarshal(b, &raw); err != nil {
		return ContainerImage{}, fmt.Errorf("parse container image inspect: %w", err)
	}
	if len(raw) == 0 {
		return ContainerImage{}, errors.New("container image inspect: no image")
	}
	r := raw[0]
	img := ContainerImage{ID: r.ID}
	if img.ID != "" && !strings.HasPrefix(img.ID, "sha256:") {
		img.ID = "sha256:" + img.ID
	}
	for _, v := range r.Variants {
		if v.Platform.OS == "unknown" || v.Config.OS == "" {
			continue // attestation manifests
		}
		p := v.Platform.OS + "/" + v.Platform.Architecture
		if v.Platform.Variant != "" {
			p += "/" + v.Platform.Variant
		}
		created := v.Config.Created
		// Docker prints Created as RFC3339Nano of the parsed time.
		if t, err := time.Parse(time.RFC3339Nano, created); err == nil {
			created = t.UTC().Format(time.RFC3339Nano)
		}
		fp := v.Config.OS + "/" + v.Config.Architecture + "|" + created + "|"
		for _, l := range v.Config.RootFS.DiffIDs {
			fp += l + ","
		}
		img.Variants = append(img.Variants, ContainerVariant{Platform: p, Fingerprint: fp})
	}
	sort.SliceStable(img.Variants, func(i, j int) bool { return img.Variants[i].Platform < img.Variants[j].Platform })
	return img, nil
}

// Variant returns the variant for platform, or the only variant when
// platform is empty.
func (c ContainerImage) Variant(platform string) (ContainerVariant, bool) {
	for _, v := range c.Variants {
		if v.Platform == platform {
			return v, true
		}
	}
	if platform == "" && len(c.Variants) == 1 {
		return c.Variants[0], true
	}
	// linux/arm64 matches linux/arm64/v8.
	for _, v := range c.Variants {
		if platform != "" && strings.HasPrefix(v.Platform, platform+"/") {
			return v, true
		}
	}
	return ContainerVariant{}, false
}
