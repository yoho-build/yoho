package swarm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/yoho-build/yoho/internal/release"
	"os"
	"path/filepath"
	"reflect"
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

// A fresh Version whose finish switched `current` before the connection
// dropped: recovery resets current and puts the stack back on the previous
// Release's record, not just the link.
func TestFreshVersionFailureRestoresPreviousStack(t *testing.T) {
	withRoot(t)
	fastPolling(t)
	ctx := context.Background()
	h := newFake(newSim())
	if _, err := (Runtime{}).Deploy(ctx, testDeploy(t, h, nil)); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.scripts = nil
	h.mu.Unlock()
	fh := &flakyHost{fakeHost: h, after: true, fail: func(s string) bool {
		return strings.Contains(s, "ln -sfn") && strings.Contains(s, "releases/v2") && strings.Contains(s, "current") && !strings.Contains(s, "readlink")
	}}
	d := testDeploy(t, fh, nil)
	d.Version = "v2"
	if _, err := (Runtime{}).Deploy(ctx, d); err == nil {
		t.Fatal("want error")
	}
	all := h.all()
	i := strings.LastIndex(all, "docker stack deploy")
	if i < 0 || !strings.Contains(all[i:], "releases/v1/compose.yaml") {
		t.Fatalf("last stack deploy is not the v1 record:\n%s", all)
	}
}

// probePreflightHost records all non-read-only operations except acquiring and
// releasing the Destination lock. Invalid current plans may only take that lock.
type probePreflightHost struct {
	*fakeHost
	mutations []string
}

func (h *probePreflightHost) Output(ctx context.Context, c remote.Cmd) (string, error) {
	if !strings.Contains(c.Script, "echo acquired") && !strings.Contains(c.Script, "docker info -f") && !strings.Contains(c.Script, "readlink") {
		h.mutations = append(h.mutations, c.Script)
	}
	return h.fakeHost.Output(ctx, c)
}
func (h *probePreflightHost) Run(ctx context.Context, c remote.Cmd) error {
	if !strings.HasPrefix(c.Script, "rm -rf ") || !strings.Contains(c.Script, "/lock'") {
		h.mutations = append(h.mutations, c.Script)
	}
	return h.fakeHost.Run(ctx, c)
}
func (h *probePreflightHost) WriteFile(ctx context.Context, p string, b []byte, m os.FileMode, sudo bool) error {
	h.mutations = append(h.mutations, "write "+p)
	return h.fakeHost.WriteFile(ctx, p, b, m, sudo)
}

// Capture directories too: preparation of a new Release is a mutation even if
// it never reaches a WriteFile call. Lock files are transient and excluded.
func probeAppFiles(t *testing.T) map[string]string {
	t.Helper()
	root := release.AppDir("shop", "production")
	files := map[string]string{}
	err := filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.Name() == "lock" && e.IsDir() {
			return filepath.SkipDir
		}
		name, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if e.IsDir() {
			files[name] = "directory"
			return nil
		}
		if e.Type()&os.ModeSymlink != 0 {
			v, err := os.Readlink(p)
			files[name] = "link:" + v
			return err
		}
		b, err := os.ReadFile(p)
		files[name] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestInvalidCurrentProxyHealthPlanFailsBeforeMutation(t *testing.T) {
	for _, op := range []string{"deploy", "same-version", "rollback"} {
		for _, tc := range []struct {
			field string
			value any
		}{
			{"health_port", 0}, {"health_port", 65536}, {"health_timeout", "0s"}, {"health_timeout", "bad"},
		} {
			t.Run(op+tc.field+fmt.Sprint(tc.value), func(t *testing.T) {
				withRoot(t)
				fastPolling(t)
				ctx := context.Background()
				base := newFake(newSim())
				d := testDeploy(t, base, nil)
				d.RetainReleases = 5
				if _, err := (Runtime{}).Deploy(ctx, d); err != nil {
					t.Fatal(err)
				}
				d.Version = "v2"
				if _, err := (Runtime{}).Deploy(ctx, d); err != nil {
					t.Fatal(err)
				}
				planPath := filepath.Join(release.Dir("shop", "production", "v2"), "plan.json")
				b, err := os.ReadFile(planPath)
				if err != nil {
					t.Fatal(err)
				}
				var p map[string]any
				if err = json.Unmarshal(b, &p); err != nil {
					t.Fatal(err)
				}
				for _, row := range p["services"].([]any) {
					sp := row.(map[string]any)
					if sp["name"] == "web" {
						sp["proxy"].(map[string]any)[tc.field] = tc.value
					}
				}
				invalid, err := json.Marshal(p)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(planPath, invalid, 0600); err != nil {
					t.Fatal(err)
				}
				before := probeAppFiles(t)
				guarded := &probePreflightHost{fakeHost: base}
				desired := testDeploy(t, guarded, nil)
				desired.Version = "v3"
				if op == "same-version" {
					desired.Version = "v2"
				}
				if op == "rollback" {
					_, err = (Runtime{}).Rollback(ctx, desired, "v1")
				} else {
					_, err = (Runtime{}).Deploy(ctx, desired)
				}
				if err == nil || !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "current Release v2") {
					t.Fatalf("error %v", err)
				}
				if len(guarded.mutations) > 0 {
					t.Fatalf("mutations before rejecting current plan: %v", guarded.mutations)
				}
				if after := probeAppFiles(t); !reflect.DeepEqual(before, after) {
					t.Fatal("retained App files changed after invalid current plan rejection")
				}
			})
		}
	}
}

func TestCurrentProxyHealthPreflightAllowsMissingAndLegacyPlans(t *testing.T) {
	for _, op := range []string{"deploy", "same-version", "rollback"} {
		for _, missing := range []bool{false, true} {
			t.Run(op+fmt.Sprint(missing), func(t *testing.T) {
				withRoot(t)
				fastPolling(t)
				ctx := context.Background()
				h := newFake(newSim())
				d := testDeploy(t, h, nil)
				d.RetainReleases = 5
				if _, err := (Runtime{}).Deploy(ctx, d); err != nil {
					t.Fatal(err)
				}
				d.Version = "v2"
				if _, err := (Runtime{}).Deploy(ctx, d); err != nil {
					t.Fatal(err)
				}
				if missing {
					if err := os.Remove(filepath.Join(release.Dir("shop", "production", "v2"), "plan.json")); err != nil {
						t.Fatal(err)
					}
				}
				d.Version = "v3"
				if op == "same-version" {
					d.Version = "v2"
				}
				var err error
				if op == "rollback" {
					_, err = (Runtime{}).Rollback(ctx, d, "v1")
				} else {
					_, err = (Runtime{}).Deploy(ctx, d)
				}
				if err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
