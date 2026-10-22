package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/yoho-build/yoho/internal/remote"
)

// ErrDirectUnavailable means the builder cannot stream to the Server, so the
// caller falls back to shipping from the operator's machine. It is never a
// build failure.
var ErrDirectUnavailable = errors.New("builder cannot reach the Server directly")

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
