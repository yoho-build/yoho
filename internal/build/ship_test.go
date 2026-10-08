package build

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/yoho-build/yoho/internal/remote"
)

type shipHost struct {
	name    string
	imageID string // image ID reported by docker image inspect
	mu      sync.Mutex
	loaded  string
	scripts []string
}

func (h *shipHost) Name() string { return h.name }
func (h *shipHost) Run(_ context.Context, c remote.Cmd) error {
	h.mu.Lock()
	h.scripts = append(h.scripts, c.Script)
	h.mu.Unlock()
	if strings.Contains(c.Script, "docker save") {
		_, err := io.WriteString(c.Stdout, "TARBALL")
		return err
	}
	if strings.HasPrefix(c.Script, "docker load") {
		b, _ := io.ReadAll(c.Stdin)
		h.mu.Lock()
		h.loaded = string(b)
		h.mu.Unlock()
	}
	return nil
}
func (h *shipHost) Output(_ context.Context, c remote.Cmd) (string, error) {
	if strings.Contains(c.Script, "docker image inspect") {
		return h.imageID + "\n", nil
	}
	return "", nil
}
func (h *shipHost) WriteFile(context.Context, string, []byte, os.FileMode, bool) error { return nil }
func (h *shipHost) ReadFile(context.Context, string, bool) ([]byte, error) {
	return nil, os.ErrNotExist
}
func (h *shipHost) Close() error { return nil }

func TestShipFromServerLoadsWorkersThatLackTheImage(t *testing.T) {
	src := &shipHost{name: "mgr", imageID: "sha256:aaa"}
	worker := &shipHost{name: "w1"}                       // image missing
	stale := &shipHost{name: "w2", imageID: "sha256:old"} // other image under the same tag
	have := &shipHost{name: "w3", imageID: "sha256:aaa"}  // already there
	got, err := ShipFromServer(context.Background(), src, []remote.Host{worker, stale, have}, []string{"yoho/shop-web:v1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("shipped to %v", got)
	}
	if worker.loaded != "TARBALL" || stale.loaded != "TARBALL" {
		t.Errorf("not loaded: %q %q", worker.loaded, stale.loaded)
	}
	if len(have.scripts) != 0 {
		t.Errorf("up-to-date Server touched: %v", have.scripts)
	}
	if !strings.Contains(strings.Join(src.scripts, "\n"), "docker save 'yoho/shop-web:v1' | gzip -1") {
		t.Errorf("save script: %v", src.scripts)
	}
}

func TestShipFromServerMissingSourceImage(t *testing.T) {
	_, err := ShipFromServer(context.Background(), &shipHost{name: "mgr"}, []remote.Host{&shipHost{name: "w1"}}, []string{"x:1"}, nil)
	if err == nil || !strings.Contains(err.Error(), "missing on mgr") {
		t.Fatalf("err = %v", err)
	}
}
