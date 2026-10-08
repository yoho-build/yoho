package swarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/remote"
)

// flakyHost fails scripts chosen by fail. With after set the script runs
// first, like an SSH drop that hides a success.
type flakyHost struct {
	*fakeHost
	fail  func(script string) bool
	after bool
}

func (f *flakyHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if !f.fail(c.Script) {
		return f.fakeHost.Output(ctx, c)
	}
	if f.after {
		f.fakeHost.Output(ctx, c)
	}
	return "", errors.New("connection lost")
}

func (f *flakyHost) Run(ctx context.Context, c remote.Cmd) error {
	_, err := f.Output(ctx, c)
	return err
}

func currentLink(t *testing.T, root string) string {
	t.Helper()
	l, err := os.Readlink(filepath.Join(root, "apps", "shop", "production", "current"))
	if err != nil {
		return ""
	}
	return l
}

func TestDeployAbortsWhenCurrentCannotBeRead(t *testing.T) {
	root := withRoot(t)
	fastPolling(t)
	ctx := context.Background()
	sim := newSim()
	h := newFake(sim)
	if _, err := (Runtime{}).Deploy(ctx, testDeploy(t, h, nil)); err != nil {
		t.Fatal(err)
	}
	h.scripts = nil
	fh := &flakyHost{fakeHost: h, fail: func(s string) bool { return strings.Contains(s, "readlink") }}
	d := testDeploy(t, fh, nil)
	d.Version = "v2"
	_, err := (Runtime{}).Deploy(ctx, d)
	if err == nil || !strings.Contains(err.Error(), "which Release is current") {
		t.Fatalf("err = %v", err)
	}
	if n := strings.Count(h.all(), "docker stack deploy"); n != 0 {
		t.Fatalf("stack touched after a failed lookup:\n%s", h.all())
	}
	if _, err := os.Stat(filepath.Join(root, "apps", "shop", "production", "releases", "v2", "compose.yaml")); err == nil {
		t.Error("v2 files written")
	}
	if l := currentLink(t, root); l != "releases/v1" {
		t.Errorf("current = %q", l)
	}
}

// finish switched `current` but the client saw an error: recovery points it
// back at the Release that was current.
func TestFailedFinishResetsCurrent(t *testing.T) {
	ln := func(s string) bool {
		return strings.Contains(s, "ln -sfn") && strings.Contains(s, "releases/v2") && strings.Contains(s, "current") && !strings.Contains(s, "readlink")
	}
	for _, tc := range []string{"fresh version", "redeploy of an existing Version"} {
		t.Run(tc, func(t *testing.T) {
			root := withRoot(t)
			fastPolling(t)
			ctx := context.Background()
			sim := newSim()
			h := newFake(sim)
			if _, err := (Runtime{}).Deploy(ctx, testDeploy(t, h, nil)); err != nil {
				t.Fatal(err)
			}
			if tc != "fresh version" {
				d := testDeploy(t, h, nil)
				d.Version = "v2"
				if _, err := (Runtime{}).Deploy(ctx, d); err != nil {
					t.Fatal(err)
				}
				cur := filepath.Join(root, "apps", "shop", "production", "current")
				os.Remove(cur)
				if err := os.Symlink("releases/v1", cur); err != nil {
					t.Fatal(err)
				}
			}
			fh := &flakyHost{fakeHost: h, fail: ln, after: true}
			d := testDeploy(t, fh, nil)
			d.Version = "v2"
			if _, err := (Runtime{}).Deploy(ctx, d); err == nil {
				t.Fatal("want error")
			}
			if l := currentLink(t, root); l != "releases/v1" {
				t.Errorf("current = %q, want releases/v1", l)
			}
		})
	}
}

func TestFailedFirstFinishRemovesCurrent(t *testing.T) {
	root := withRoot(t)
	fastPolling(t)
	h := newFake(newSim())
	fh := &flakyHost{fakeHost: h, after: true, fail: func(s string) bool {
		return strings.Contains(s, "ln -sfn") && strings.Contains(s, "releases/v1") && !strings.Contains(s, "readlink")
	}}
	if _, err := (Runtime{}).Deploy(context.Background(), testDeploy(t, fh, nil)); err == nil {
		t.Fatal("want error")
	}
	if l := currentLink(t, root); l != "" {
		t.Errorf("current = %q, want none", l)
	}
}
