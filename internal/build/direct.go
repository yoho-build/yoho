package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

// ErrDirectUnavailable means the builder cannot stream to the Server, so the
// caller falls back to shipping from the operator's machine. It is never a
// build failure.
var ErrDirectUnavailable = errors.New("builder cannot reach the Server directly")

// Fingerprint identifies image content across image stores. Image IDs
// differ between a containerd store (manifest/index digest) and a classic
// overlay2 store (config digest), but platform, creation time and layer diff
// IDs are the same. range, not join: Docker 27's CLI decodes RootFS.Layers
// as []interface{} and join fails ("wrong type for value"). Apple container
// images get the same string from ParseContainerInspect.
const Fingerprint = `{{.Os}}/{{.Architecture}}|{{.Created}}|{{range .RootFS.Layers}}{{.}},{{end}}`

// Stats reports one image transfer.
type Stats struct {
	Image    string
	Bytes    int64
	Duration time.Duration
}

// DirectProbe checks that src (the builder host) can open a trusted,
// non-interactive SSH connection to dst. Failure wraps ErrDirectUnavailable.
// Yoho never copies keys or accepts new host keys for the builder.
func DirectProbe(ctx context.Context, src, dst remote.Host) error {
	hop, err := hopTo(dst)
	if err != nil {
		return err
	}
	if _, err := src.Output(ctx, remote.Cmd{Script: hop + " true"}); err != nil {
		return fmt.Errorf("%w: %s cannot ssh to %s without a prompt (needs its own key and a known_hosts entry): %v", ErrDirectUnavailable, src.Name(), dst.Name(), err)
	}
	return nil
}

// hopTo is the quoted `ssh ... dest` prefix run on the builder.
func hopTo(dst remote.Host) (string, error) {
	s, ok := dst.(*remote.SSH)
	if !ok {
		return "", fmt.Errorf("%w: %s is not reached over SSH", ErrDirectUnavailable, dst.Name())
	}
	args, dest, ok := s.HopArgs()
	if !ok {
		return "", fmt.Errorf("%w: %s is reached through Cloudflare Access, which the builder lacks", ErrDirectUnavailable, dst.Name())
	}
	return remote.QuoteArgs(args...) + " -- " + remote.Quote(dest), nil
}

// directScript runs on the builder: docker save | gzip | ssh dst docker load.
// The compressed stream is counted by a fifo reader (POSIX sh has no pipefail
// or process substitution) and the total goes to stdout as one marker line;
// everything else is sent to stderr. A failed docker save fails the script.
func directScript(hop string, refs []string, platform string) string {
	save := "docker save "
	if platform != "" {
		save += "--platform " + remote.Quote(platform) + " "
	}
	save += remote.QuoteArgs(refs...)
	return `set -eu
d=$(mktemp -d)
trap 'rm -rf "$d"' EXIT
mkfifo "$d/c"
wc -c <"$d/c" >"$d/n" &
w=$!
{ ` + save + ` || : >"$d/fail"; } | gzip -1 | tee "$d/c" | ` + hop + ` 'docker load >&2'
wait "$w"
[ ! -e "$d/fail" ] || { echo "docker save failed on the builder" >&2; exit 1; }
printf '` + directMarker + `%s\n' "$(tr -d ' \n' <"$d/n")"
`
}

const directMarker = "YOHO_DIRECT_BYTES="

// Direct streams refs from the builder host src to the Server dst without
// passing through the operator's machine. It probes first; when the builder
// cannot reach the Server it returns an error wrapping ErrDirectUnavailable
// and the caller ships the image the usual way. The images must already be in
// src's Docker store, and the caller skips images the Server already has
// (transport.Fingerprint). Nothing secret is sent: no registry credentials,
// no build secrets.
func Direct(ctx context.Context, src, dst remote.Host, refs []string, platform string, out io.Writer) (Stats, error) {
	if len(refs) == 0 {
		return Stats{}, nil
	}
	if out == nil {
		out = io.Discard
	}
	if err := DirectProbe(ctx, src, dst); err != nil {
		return Stats{}, err
	}
	hop, err := hopTo(dst)
	if err != nil {
		return Stats{}, err
	}
	st := Stats{Image: strings.Join(refs, ", ")}
	start := time.Now()
	var stdout strings.Builder
	err = src.Run(ctx, remote.Cmd{Script: directScript(hop, refs, platform), Stdout: &stdout, Stderr: out})
	st.Duration = time.Since(start)
	if err != nil {
		return st, fmt.Errorf("stream %s from %s to %s: %w", st.Image, src.Name(), dst.Name(), err)
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), directMarker); ok {
			st.Bytes, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	return st, nil
}

// UseDirect decides, before the build, whether a builder.location=remote
// build stays in the Builder's own Docker and ships from there (ShipDirect)
// instead of coming back to this machine. It needs one platform, a
// save|load transport, no exported build cache (the Builder's docker driver
// only writes inline caches), a Builder whose own platform is the target (it
// does not cross-build like the docker-container builder) and a Builder that
// reaches every Server (DirectProbe). cacheTo holds the Services' cache_to
// entries. When direct is not used, reason says why for auto; with
// builder.direct: true the same conditions are an error. src is the Builder host.
func UseDirect(ctx context.Context, b config.Builder, transportMode string, platforms, cacheTo []string, src remote.Host, dsts []remote.Host) (use bool, reason string, err error) {
	mode := b.Direct.Mode()
	if b.Location != "remote" || mode == config.DirectOff {
		return false, "", nil
	}
	why := ""
	switch {
	case len(platforms) > 1:
		why = fmt.Sprintf("%d platforms; a direct build holds one", len(platforms))
	case transportMode == "pussh" || transportMode == "registry":
		why = "transport.mode " + transportMode
	case src == nil:
		why = "no Builder host"
	}
	for _, c := range cacheTo {
		if why == "" && !strings.Contains(c, "type=inline") {
			why = "cache_to " + c + " needs the docker-container builder"
		}
	}
	if why == "" {
		why = builderPlatformMismatch(ctx, src, platforms[0])
	}
	for _, d := range dsts {
		if why != "" {
			break
		}
		if perr := DirectProbe(ctx, src, d); perr != nil {
			why = perr.Error()
		}
	}
	if why == "" {
		return true, "", nil
	}
	if mode == config.DirectOn {
		return false, "", fmt.Errorf("builder.direct is true but %s", why)
	}
	return false, why, nil
}

// builderPlatformMismatch says why the Builder's Docker can't build target
// natively, or "" when it can.
func builderPlatformMismatch(ctx context.Context, src remote.Host, target string) string {
	out, err := src.Output(ctx, remote.Cmd{Script: "docker info --format '{{.OSType}}/{{.Architecture}}'"})
	if err != nil {
		return "cannot read the Builder's platform: " + err.Error()
	}
	have := normalizePlatform(strings.TrimSpace(out))
	if want := normalizePlatform(target); have != want {
		return fmt.Sprintf("Builder is %s, target is %s", have, want)
	}
	return ""
}

// normalizePlatform maps docker info and OCI spellings to os/arch.
func normalizePlatform(p string) string {
	os, arch, _ := strings.Cut(p, "/")
	arch, _, _ = strings.Cut(arch, "/") // drop a variant such as arm64/v8
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	}
	return os + "/" + arch
}

// DirectResult is ShipDirect's outcome for one Server.
type DirectResult struct {
	Host string
	// Skipped: the Server already had every image with the same content.
	Skipped bool
	Stats   Stats
}

// ShipDirect streams refs from the Builder host src (built into its Docker
// with Options.Direct) to every Server in parallel. Servers that already
// hold an image with the same Fingerprint skip it. Results are in dsts
// order; errors from all Servers are joined.
func ShipDirect(ctx context.Context, src remote.Host, dsts []remote.Host, refs []string, platform string, out io.Writer) ([]DirectResult, error) {
	if out == nil {
		out = io.Discard
	}
	fp := func(h remote.Host, ref string) string {
		s, _ := h.Output(ctx, remote.Cmd{Script: "docker image inspect --format " + remote.Quote(Fingerprint) + " " + remote.Quote(ref) + " 2>/dev/null || true"})
		return strings.TrimSpace(s)
	}
	want := map[string]string{}
	for _, ref := range refs {
		if want[ref] = fp(src, ref); want[ref] == "" {
			return nil, fmt.Errorf("image %s is missing on the Builder %s", ref, src.Name())
		}
	}
	// docker save --platform needs Docker 28+ on the Builder; the image is
	// single-platform there, so older Docker saves it without the flag.
	if platform != "" {
		help, _ := src.Output(ctx, remote.Cmd{Script: "docker save --help 2>&1 || true"})
		if !strings.Contains(help, "--platform") {
			platform = ""
		}
	}
	res := make([]DirectResult, len(dsts))
	errs := make([]error, len(dsts))
	var mu sync.Mutex
	w := writerFunc(func(b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return out.Write(b)
	})
	var wg sync.WaitGroup
	for i, d := range dsts {
		res[i].Host = d.Name()
		wg.Add(1)
		go func() {
			defer wg.Done()
			var need []string
			for _, ref := range refs {
				if fp(d, ref) != want[ref] {
					need = append(need, ref)
				}
			}
			if len(need) == 0 {
				res[i].Skipped = true
				fmt.Fprintf(w, "%s: images already present\n", d.Name())
				return
			}
			fmt.Fprintf(w, "%s: streaming %d image(s) from %s\n", d.Name(), len(need), src.Name())
			st, err := Direct(ctx, src, d, need, platform, w)
			res[i].Stats = st
			if err != nil {
				errs[i] = err
			}
		}()
	}
	wg.Wait()
	return res, errors.Join(errs...)
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(b []byte) (int, error) { return f(b) }
