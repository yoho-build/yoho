package build

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/yoho-build/yoho/internal/remote"
)

// ShipFromServer copies images built on src (builder.location=server) to the
// other Servers of the Destination with `docker save | gzip` on src streamed
// into `docker load` on each destination. Servers that already have the exact
// image are skipped. It returns the names of the Servers it loaded.
func ShipFromServer(ctx context.Context, src remote.Host, dsts []remote.Host, images []string, out io.Writer) ([]string, error) {
	if len(images) == 0 || len(dsts) == 0 {
		return nil, nil
	}
	if out == nil {
		out = io.Discard
	}
	idOf := func(h remote.Host, ref string) string {
		s, _ := h.Output(ctx, remote.Cmd{Script: "docker image inspect --format '{{.Id}}' " + remote.Quote(ref) + " 2>/dev/null || true"})
		return strings.TrimSpace(s)
	}
	want := map[string]string{}
	for _, ref := range images {
		if want[ref] = idOf(src, ref); want[ref] == "" {
			return nil, fmt.Errorf("image %s is missing on %s", ref, src.Name())
		}
	}
	var mu sync.Mutex
	var shipped []string
	errs := make([]error, len(dsts))
	var wg sync.WaitGroup
	for i, d := range dsts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var need []string
			for _, ref := range images {
				if idOf(d, ref) != want[ref] {
					need = append(need, ref)
				}
			}
			if len(need) == 0 {
				return
			}
			fmt.Fprintf(out, "%s: loading %d image(s) built on %s\n", d.Name(), len(need), src.Name())
			if err := streamImages(ctx, src, d, need); err != nil {
				errs[i] = fmt.Errorf("ship images to %s: %w", d.Name(), err)
				return
			}
			mu.Lock()
			shipped = append(shipped, d.Name())
			mu.Unlock()
		}()
	}
	wg.Wait()
	return shipped, errors.Join(errs...)
}

func streamImages(ctx context.Context, src, dst remote.Host, refs []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	saveErr := make(chan error, 1)
	go func() {
		err := src.Run(ctx, remote.Cmd{Script: "set -eu\ndocker save " + remote.QuoteArgs(refs...) + " | gzip -1", Stdout: pw})
		if err != nil {
			err = fmt.Errorf("docker save on %s: %w", src.Name(), err)
		}
		pw.CloseWithError(err)
		saveErr <- err
	}()
	runErr := dst.Run(ctx, remote.Cmd{Script: "docker load >/dev/null", Stdin: pr})
	pr.CloseWithError(errors.New("docker load ended"))
	if runErr != nil {
		cancel()
		<-saveErr
		return runErr
	}
	return <-saveErr
}
