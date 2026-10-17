package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/safepath"
)

// adEnv is a private temp dir with a Yoho root at base/yoho and room for
// app_dir candidates next to it (base/apps/...), like /home/deb/apps/yoho
// and /home/deb/apps/elpoca-engine.
type adEnv struct {
	t     *testing.T
	base  string
	root  string
	canon string
	h     *remote.Local
}

func newADEnv(t *testing.T) *adEnv {
	t.Helper()
	base := t.TempDir()
	if err := os.Chmod(base, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "yoho")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	old, oldTries := release.Root, safepath.GateTries
	release.Root, safepath.GateTries = root, 1
	t.Cleanup(func() { release.Root, safepath.GateTries = old, oldTries })
	return &adEnv{t: t, base: base, root: root, canon: release.AppDir("shop", "prod"), h: &remote.Local{}}
}

func (e *adEnv) path(rel string) string { return filepath.Join(e.base, rel) }

func (e *adEnv) ensure(want string, shim ...string) (*AppDirResult, error) {
	return runEnsureAppDir(context.Background(), e.h, appDirParams{app: "shop", dest: "prod", want: want, shim: strings.Join(shim, "")})
}

func (e *adEnv) mustEnsure(want, action string) *AppDirResult {
	e.t.Helper()
	res, err := e.ensure(want)
	if err != nil {
		e.t.Fatalf("ensure %q: %v", want, err)
	}
	if res.Action != action {
		e.t.Fatalf("ensure %q: action %q, want %q", want, res.Action, action)
	}
	return res
}

// raw runs a hand-edited ensure script (to simulate a foreign owner).
func (e *adEnv) raw(want string, edit func(string) string) (*AppDirResult, error) {
	s := edit(ensureAppDirScript(appDirParams{app: "shop", dest: "prod", want: want}))
	out, err := e.h.Output(context.Background(), remote.Cmd{Script: s})
	if err != nil {
		return nil, appDirFailure(err)
	}
	return parseEnsureOutput(out)
}

func (e *adEnv) mkdir(p string, mode os.FileMode) {
	e.t.Helper()
	if err := os.MkdirAll(p, 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		e.t.Fatal(err)
	}
}

func (e *adEnv) write(p, data string) {
	e.t.Helper()
	e.mkdir(filepath.Dir(p), 0o700)
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

func (e *adEnv) mark(dir, dest string) {
	e.write(filepath.Join(dir, safepath.MarkerName), safepath.Marker("shop", dest))
}

// seed creates a deployed default App directory with a key and a Release.
func (e *adEnv) seed() {
	e.mkdir(e.canon, 0o700)
	e.write(filepath.Join(e.canon, "hmac.key"), "key-1")
	e.write(filepath.Join(e.canon, "releases", "v1", "release.json"), `{"version":"v1","images":{"web":"yoho/shop-prod-web:v1"}}`)
}

func (e *adEnv) journal(kind, from, to string) {
	e.write(safepath.JournalPath("shop", "prod"), fmt.Sprintf(`{"kind":%q,"from":%q,"to":%q}`, kind, from, to))
}

func (e *adEnv) noJournal() {
	e.t.Helper()
	if _, err := os.Lstat(safepath.JournalPath("shop", "prod")); !os.IsNotExist(err) {
		e.t.Fatalf("journal left behind: %v", err)
	}
}

func (e *adEnv) noGates() {
	e.t.Helper()
	for _, g := range []string{safepath.GateDir("shop", "prod"), safepath.AppGateDir("shop")} {
		if _, err := os.Lstat(g); !os.IsNotExist(err) {
			e.t.Fatalf("gate %s left behind: %v", g, err)
		}
	}
}

// linked asserts canonical -> want with this App's files and marker.
func (e *adEnv) linked(want string) {
	e.t.Helper()
	got, err := os.Readlink(e.canon)
	if err != nil || got != want {
		e.t.Fatalf("canonical link = %q, %v; want %q", got, err, want)
	}
	e.files(want)
	e.noJournal()
	e.noGates()
	if _, err := os.Lstat(safepath.LinkTmpPath("shop", "prod")); !os.IsNotExist(err) {
		e.t.Fatal("link.tmp left behind")
	}
}

// real asserts canonical is a real directory with this App's files.
func (e *adEnv) real() {
	e.t.Helper()
	fi, err := os.Lstat(e.canon)
	if err != nil || !fi.IsDir() {
		e.t.Fatalf("canonical is not a real directory: %v %v", fi, err)
	}
	e.files(e.canon)
	e.noJournal()
	e.noGates()
}

func (e *adEnv) files(dir string) {
	e.t.Helper()
	if b, err := os.ReadFile(filepath.Join(dir, "hmac.key")); err != nil || string(b) != "key-1" {
		e.t.Fatalf("hmac.key in %s: %q %v", dir, b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, safepath.MarkerName)); err != nil || string(b) != safepath.Marker("shop", "prod") {
		e.t.Fatalf("marker in %s: %q %v", dir, b, err)
	}
	fi, err := os.Stat(filepath.Join(dir, safepath.MarkerName))
	if err != nil || fi.Mode().Perm() != 0o600 {
		e.t.Fatalf("marker mode: %v %v", fi, err)
	}
}

func refused(t *testing.T, err error, sub string) {
	t.Helper()
	var ae *AppDirError
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), sub) {
		t.Fatalf("want App directory refusal containing %q, got %T %v", sub, err, err)
	}
}

func TestAppDirFreshDefaultAndUse(t *testing.T) {
	e := newADEnv(t)
	e.mustEnsure("", "created")
	if fi, err := os.Lstat(e.canon); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
		t.Fatalf("canonical: %v %v", fi, err)
	}
	e.write(filepath.Join(e.canon, "hmac.key"), "key-1")
	e.mustEnsure("", "used")
	e.real()
}

func TestAppDirUseExistingDirWritesMarker(t *testing.T) {
	e := newADEnv(t)
	e.seed()
	e.mustEnsure("", "used")
	e.real()
}

func TestAppDirFreshWithAppDir(t *testing.T) {
	e := newADEnv(t)
	want := e.path("apps/elpoca/engine")
	e.mustEnsure(want, "created")
	fi, err := os.Stat(want)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("app_dir: %v %v", fi, err)
	}
	e.write(filepath.Join(want, "hmac.key"), "key-1")
	res := e.mustEnsure(want, "used")
	if !strings.HasSuffix(res.Physical, "/apps/elpoca/engine") {
		t.Fatalf("physical %q", res.Physical)
	}
	e.linked(want)
}

func TestAppDirFreshAdoptsEmptyOrOwnMarkedDir(t *testing.T) {
	e := newADEnv(t)
	want := e.path("apps/engine")
	e.mkdir(want, 0o700)
	e.mustEnsure(want, "created")
	e.write(filepath.Join(want, "hmac.key"), "key-1")
	// The link was removed by hand; the marker proves the directory is ours.
	os.Remove(e.canon)
	e.mustEnsure(want, "created")
	e.linked(want)
}

func TestAppDirForwardBackRetarget(t *testing.T) {
	e := newADEnv(t)
	e.seed()
	a, b := e.path("apps/engine"), e.path("srv/engine-b")
	e.mustEnsure(a, "forward")
	e.linked(a)
	e.mustEnsure(a, "used")
	e.mustEnsure(b, "retarget")
	e.linked(b)
	if _, err := os.Lstat(a); !os.IsNotExist(err) {
		t.Fatal("old app_dir left behind")
	}
	e.mustEnsure("", "back")
	e.real()
	if _, err := os.Lstat(b); !os.IsNotExist(err) {
		t.Fatal("app_dir left behind after moving back")
	}
	// Forward into an existing empty directory.
	e.mkdir(a, 0o755)
	e.mustEnsure(a, "forward")
	e.linked(a)
}

func TestAppDirResume(t *testing.T) {
	type tc struct {
		name    string
		want    string // "" default, "a" or "b"
		prepare func(e *adEnv, a, b string)
		check   func(e *adEnv, a, b string)
		resumed string
	}
	linkTo := func(e *adEnv, target string) {
		e.t.Helper()
		if _, err := e.ensure(target); err != nil {
			e.t.Fatal(err)
		}
	}
	cases := []tc{
		{"forward/journal only", "a", func(e *adEnv, a, b string) {
			e.mark(e.canon, "prod")
			e.journal("forward", e.canon, a)
		}, func(e *adEnv, a, b string) { e.linked(a) }, "forward"},
		{"forward/journal and empty target", "a", func(e *adEnv, a, b string) {
			e.mark(e.canon, "prod")
			e.mkdir(a, 0o700)
			e.journal("forward", e.canon, a)
		}, func(e *adEnv, a, b string) { e.linked(a) }, "forward"},
		{"forward/renamed no link", "a", func(e *adEnv, a, b string) {
			e.mark(e.canon, "prod")
			e.journal("forward", e.canon, a)
			e.mkdir(filepath.Dir(a), 0o700)
			os.Rename(e.canon, a)
		}, func(e *adEnv, a, b string) { e.linked(a) }, "forward"},
		{"forward/renamed stale link.tmp", "a", func(e *adEnv, a, b string) {
			e.mark(e.canon, "prod")
			e.journal("forward", e.canon, a)
			e.mkdir(filepath.Dir(a), 0o700)
			os.Rename(e.canon, a)
			os.Symlink(b, safepath.LinkTmpPath("shop", "prod"))
		}, func(e *adEnv, a, b string) { e.linked(a) }, "forward"},
		{"forward/linked journal left", "a", func(e *adEnv, a, b string) {
			linkTo(e, a)
			e.journal("forward", e.canon, a)
		}, func(e *adEnv, a, b string) { e.linked(a) }, "forward"},
		{"back/link present", "", func(e *adEnv, a, b string) {
			linkTo(e, a)
			e.journal("back", a, e.canon)
		}, func(e *adEnv, a, b string) { e.real() }, "back"},
		{"back/link removed", "", func(e *adEnv, a, b string) {
			linkTo(e, a)
			e.journal("back", a, e.canon)
			os.Remove(e.canon)
		}, func(e *adEnv, a, b string) { e.real() }, "back"},
		{"back/renamed journal left", "", func(e *adEnv, a, b string) {
			linkTo(e, a)
			e.journal("back", a, e.canon)
			os.Remove(e.canon)
			os.Rename(a, e.canon)
		}, func(e *adEnv, a, b string) { e.real() }, "back"},
		{"retarget/journal only", "b", func(e *adEnv, a, b string) {
			linkTo(e, a)
			e.journal("retarget", a, b)
		}, func(e *adEnv, a, b string) { e.linked(b) }, "retarget"},
		{"retarget/renamed dangling link", "b", func(e *adEnv, a, b string) {
			linkTo(e, a)
			e.journal("retarget", a, b)
			e.mkdir(filepath.Dir(b), 0o700)
			os.Rename(a, b)
		}, func(e *adEnv, a, b string) { e.linked(b) }, "retarget"},
		{"retarget/renamed link.tmp to target", "b", func(e *adEnv, a, b string) {
			linkTo(e, a)
			e.journal("retarget", a, b)
			e.mkdir(filepath.Dir(b), 0o700)
			os.Rename(a, b)
			os.Symlink(b, safepath.LinkTmpPath("shop", "prod"))
		}, func(e *adEnv, a, b string) { e.linked(b) }, "retarget"},
		{"retarget/renamed link.tmp elsewhere", "b", func(e *adEnv, a, b string) {
			linkTo(e, a)
			e.journal("retarget", a, b)
			e.mkdir(filepath.Dir(b), 0o700)
			os.Rename(a, b)
			os.Symlink(e.path("elsewhere"), safepath.LinkTmpPath("shop", "prod"))
		}, func(e *adEnv, a, b string) { e.linked(b) }, "retarget"},
		{"retarget/published link.tmp and journal left", "b", func(e *adEnv, a, b string) {
			linkTo(e, a)
			linkTo(e, b)
			e.journal("retarget", a, b)
			os.Symlink(b, safepath.LinkTmpPath("shop", "prod"))
		}, func(e *adEnv, a, b string) { e.linked(b) }, "retarget"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newADEnv(t)
			e.seed()
			a, b := e.path("apps/engine"), e.path("srv/engine-b")
			c.prepare(e, a, b)
			want := map[string]string{"": "", "a": a, "b": b}[c.want]
			res, err := e.ensure(want)
			if err != nil {
				t.Fatal(err)
			}
			if res.Resumed != c.resumed || res.Action != "used" {
				t.Fatalf("result %+v", res)
			}
			c.check(e, a, b)
		})
	}
}

func TestAppDirResumeThenConvergeToNewAppDir(t *testing.T) {
	e := newADEnv(t)
	e.seed()
	a, b := e.path("apps/engine"), e.path("srv/engine-b")
	e.mark(e.canon, "prod")
	e.journal("forward", e.canon, a)
	res, err := e.ensure(b)
	if err != nil {
		t.Fatal(err)
	}
	if res.Resumed != "forward" || res.Action != "retarget" {
		t.Fatalf("result %+v", res)
	}
	e.linked(b)
}

func TestAppDirResumeUnlistedStateRefuses(t *testing.T) {
	for name, prepare := range map[string]func(e *adEnv, a string){
		"forward both missing": func(e *adEnv, a string) {
			e.journal("forward", e.canon, a)
			os.RemoveAll(e.canon)
		},
		"forward from is not canonical": func(e *adEnv, a string) {
			e.journal("forward", e.path("other"), a)
		},
		"retarget unmarked target": func(e *adEnv, a string) {
			if _, err := e.ensure(a); err != nil {
				e.t.Fatal(err)
			}
			b := e.path("srv/b")
			e.write(filepath.Join(b, "foreign"), "x")
			e.journal("retarget", a, b)
		},
		"garbage": func(e *adEnv, a string) {
			e.write(safepath.JournalPath("shop", "prod"), "not json")
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newADEnv(t)
			e.seed()
			a := e.path("apps/engine")
			prepare(e, a)
			_, err := e.ensure(a)
			refused(t, err, "unexpected state")
			if _, err := os.Lstat(safepath.JournalPath("shop", "prod")); err != nil {
				t.Fatal("journal must stay for manual inspection")
			}
			e.noGates()
		})
	}
}

func TestAppDirRefusals(t *testing.T) {
	cases := []struct {
		name    string
		seed    bool
		prepare func(e *adEnv) string // returns app_dir
		shim    string
		sub     string
	}{
		{"fresh non-empty unmarked", false, func(e *adEnv) string {
			w := e.path("apps/checkout")
			e.write(filepath.Join(w, "README"), "git checkout")
			return w
		}, "", "is not empty"},
		{"fresh foreign marker", false, func(e *adEnv) string {
			w := e.path("apps/engine")
			e.mark(w, "staging")
			return w
		}, "", "another App or Destination"},
		{"forward non-empty target", true, func(e *adEnv) string {
			w := e.path("apps/checkout")
			e.write(filepath.Join(w, "README"), "x")
			return w
		}, "", "is not empty"},
		{"forward target already holds this App", true, func(e *adEnv) string {
			w := e.path("apps/engine")
			e.mark(w, "prod")
			e.write(filepath.Join(w, "hmac.key"), "other copy")
			return w
		}, "", "both the current App directory"},
		{"forward canonical has foreign marker", true, func(e *adEnv) string {
			e.mark(e.canon, "staging")
			return e.path("apps/engine")
		}, "", "not this App's ownership marker"},
		{"forward lock present", true, func(e *adEnv) string {
			e.mkdir(filepath.Join(e.canon, "lock"), 0o700)
			return e.path("apps/engine")
		}, "", "lock exists"},
		{"forward source.run present", true, func(e *adEnv) string {
			e.mkdir(filepath.Join(e.canon, "source.run-abc"), 0o700)
			return e.path("apps/engine")
		}, "", "Server build is running"},
		{"forward cross-device", true, func(e *adEnv) string { return e.path("apps/engine") },
			`yoho_dev() { case "$1" in *"/apps/engine"|*"/apps") echo 2;; *) echo 1;; esac; }` + "\n", "different filesystems"},
		{"fresh cross-device reserved namespace", false, func(e *adEnv) string { return e.path("apps/engine") },
			`yoho_dev() { case "$1" in *"/appdirs/"*) echo 2;; *) echo 1;; esac; }` + "\n", "different filesystems"},
		{"inside root lexical", true, func(e *adEnv) string { return filepath.Join(e.root, "inner") }, "", "inside the Yoho root"},
		{"contains root", true, func(e *adEnv) string { return e.base }, "", "contains the Yoho root"},
		{"untrusted parent", true, func(e *adEnv) string {
			e.mkdir(e.path("shared"), 0o777)
			return e.path("shared/engine")
		}, "", "not trusted (unsafe: "},
		{"inside another App directory", false, func(e *adEnv) string {
			e.mkdir(e.path("other"), 0o700)
			e.write(e.path("other/.yoho-app.json"), `{"app":"other","destination":"production"}`)
			return e.path("other/child")
		}, "", "inside the App directory"},
		{"forward source under group-writable root", true, func(e *adEnv) string {
			os.Chmod(e.root, 0o770)
			return e.path("apps/engine")
		}, "", "has an untrusted parent: "},
		{"app_dir is a symlink", false, func(e *adEnv) string {
			e.mkdir(e.path("real"), 0o700)
			os.Symlink(e.path("real"), e.path("apps-link"))
			return e.path("apps-link")
		}, "", "not trusted"},
		{"canonical is a file", false, func(e *adEnv) string {
			e.write(e.canon, "x")
			return ""
		}, "", "not a directory or symlink"},
		{"fresh non-private empty dir", false, func(e *adEnv) string {
			w := e.path("apps/engine")
			e.mkdir(w, 0o755)
			return w
		}, "", "mode 0700"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newADEnv(t)
			if c.seed {
				e.seed()
			}
			want := c.prepare(e)
			_, err := e.ensure(want, c.shim)
			refused(t, err, c.sub)
			e.noJournal()
			e.noGates()
			if c.seed {
				fi, err := os.Lstat(e.canon)
				if err != nil || !fi.IsDir() {
					t.Fatal("canonical App directory changed by a refused migration")
				}
				if b, _ := os.ReadFile(filepath.Join(e.canon, "hmac.key")); string(b) != "key-1" {
					t.Fatal("App files changed by a refused migration")
				}
			}
		})
	}
}

// The root itself reached through a link: lexically outside, physically inside.
func TestAppDirPhysicalRootContainment(t *testing.T) {
	e := newADEnv(t)
	real := e.path("realroot")
	e.mkdir(real, 0o700)
	if err := os.Symlink(real, e.path("rootlink")); err != nil {
		t.Fatal(err)
	}
	release.Root = e.path("rootlink")
	_, err := e.ensure(filepath.Join(real, "engine"))
	refused(t, err, "inside the Yoho root")
	// A sibling of the physical root is fine.
	if _, err := e.ensure(e.path("apps/engine")); err != nil {
		t.Fatal(err)
	}
}

func TestAppDirUntrustedLinks(t *testing.T) {
	cases := map[string]func(e *adEnv){
		"no marker": func(e *adEnv) {
			e.mkdir(e.path("apps/x"), 0o700)
			os.Symlink(e.path("apps/x"), e.canon)
		},
		"marker of another Destination": func(e *adEnv) {
			e.mark(e.path("apps/x"), "staging")
			os.Symlink(e.path("apps/x"), e.canon)
		},
		"dangling without journal": func(e *adEnv) {
			os.Symlink(e.path("apps/gone"), e.canon)
		},
		"relative target": func(e *adEnv) {
			e.mark(e.path("apps/x"), "prod")
			os.Symlink("../../../apps/x", e.canon)
		},
		"target under untrusted parent": func(e *adEnv) {
			e.mkdir(e.path("shared"), 0o777)
			e.mark(e.path("shared/x"), "prod")
			os.Symlink(e.path("shared/x"), e.canon)
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			e := newADEnv(t)
			e.mkdir(filepath.Dir(e.canon), 0o700)
			prepare(e)
			for _, want := range []string{"", e.path("apps/x"), e.path("apps/y")} {
				_, err := e.ensure(want)
				refused(t, err, "does not trust")
			}
			_, err := AcquireLock(context.Background(), e.h, "shop", "prod", LockInfo{Performer: "bob"})
			if err == nil || !strings.Contains(err.Error(), "unsafe App directory path") {
				t.Fatalf("lock through untrusted link: %v", err)
			}
			e.noJournal()
			e.noGates()
		})
	}
	t.Run("link owned by another user", func(t *testing.T) {
		e := newADEnv(t)
		want := e.path("apps/x")
		e.mustEnsure(want, "created")
		ownerCheck := `[ -L "$1" ] && [ -n "$(find "$1" -prune -user "$yoho_uid" -print)" ] || yoho_bad "$1"`
		_, err := e.raw(want, func(s string) string {
			if !strings.Contains(s, ownerCheck) {
				t.Fatal("owner check not found")
			}
			return strings.Replace(s, ownerCheck, `[ -L "$1" ] && [ -n "$(find "$1" -prune -user 2147483646 -print)" ] || yoho_bad "$1"`, 1)
		})
		refused(t, err, "does not trust")
	})
}

func TestAppDirGates(t *testing.T) {
	t.Run("Destination gate held", func(t *testing.T) {
		e := newADEnv(t)
		e.seed()
		gate := safepath.GateDir("shop", "prod")
		e.write(filepath.Join(gate, "info.json"), `{"performer":"alice","command":"app_dir"}`)
		_, err := e.ensure(e.path("apps/engine"))
		var le *LockedError
		if !errors.As(err, &le) || le.Dir != gate || le.Holder.Performer != "alice" {
			t.Fatalf("ensure: %v", err)
		}
		_, err = AcquireLock(context.Background(), e.h, "shop", "prod", LockInfo{Performer: "bob"})
		if !errors.As(err, &le) || le.Dir != gate || !strings.Contains(err.Error(), "remove "+gate) {
			t.Fatalf("lock: %v", err)
		}
		if _, err := os.Lstat(LockDir("shop", "prod")); !os.IsNotExist(err) {
			t.Fatal("inner lock taken while the gate was held")
		}
		os.RemoveAll(gate)
		e.mustEnsure(e.path("apps/engine"), "forward")
	})
	t.Run("App gate held during a migration", func(t *testing.T) {
		e := newADEnv(t)
		e.seed()
		appGate := safepath.AppGateDir("shop")
		e.write(filepath.Join(appGate, "info.json"), `{"performer":"yoho","command":"image retention scan"}`)
		_, err := e.ensure(e.path("apps/engine"))
		var le *LockedError
		if !errors.As(err, &le) || le.Dir != appGate {
			t.Fatalf("ensure: %v", err)
		}
		e.noJournal()
		if _, err := os.Lstat(safepath.GateDir("shop", "prod")); !os.IsNotExist(err) {
			t.Fatal("Destination gate not released")
		}
		fi, err := os.Lstat(e.canon)
		if err != nil || !fi.IsDir() {
			t.Fatal("moved without the App gate")
		}
		// No move needed: the App gate is not required.
		e.mustEnsure("", "used")
	})
}

func TestLockThroughAppDirLink(t *testing.T) {
	e := newADEnv(t)
	e.seed()
	want := e.path("apps/engine")
	e.mustEnsure(want, "forward")
	ctx := context.Background()
	unlock, err := AcquireLock(ctx, e.h, "shop", "prod", LockInfo{Performer: "alice", Command: "deploy"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(want, "lock", "info.json")); err != nil {
		t.Fatal("lock not taken in app_dir", err)
	}
	// A migration refuses while the App is in use.
	_, err = e.ensure("")
	refused(t, err, "lock exists")
	e.linked(want)
	if err := unlock(ctx); err != nil {
		t.Fatal(err)
	}
	e.noGates()
	e.mustEnsure("", "back")
	e.real()
}

func TestLockRefusesDuringMigrationJournal(t *testing.T) {
	e := newADEnv(t)
	e.seed()
	e.journal("forward", e.canon, e.path("apps/engine"))
	_, err := AcquireLock(context.Background(), e.h, "shop", "prod", LockInfo{Performer: "bob"})
	var me *MigrationPendingError
	if !errors.As(err, &me) || me.Journal != safepath.JournalPath("shop", "prod") || !strings.Contains(err.Error(), "rerun yoho deploy or yoho apply") {
		t.Fatalf("lock during journal: %v", err)
	}
	if _, err := os.Lstat(LockDir("shop", "prod")); !os.IsNotExist(err) {
		t.Fatal("inner lock taken during a migration")
	}
	e.noGates()
}

func TestLockCreatesMissingCanonicalAsDirectory(t *testing.T) {
	e := newADEnv(t)
	unlock, err := AcquireLock(context.Background(), e.h, "shop", "prod", LockInfo{Performer: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(context.Background())
	if fi, err := os.Lstat(e.canon); err != nil || !fi.IsDir() {
		t.Fatal("canonical not created", err)
	}
	e.noGates()
}

func TestRetainedImageRefsThroughLinkAndDuringMigration(t *testing.T) {
	e := newADEnv(t)
	e.seed()
	e.mustEnsure(e.path("apps/engine"), "forward")
	staging := release.AppDir("shop", "staging")
	e.write(filepath.Join(staging, "releases", "s1", "release.json"), `{"version":"s1","images":{"web":"yoho/shop-staging-web:s1"}}`)
	ctx := context.Background()
	keep, err := RetainedImageRefs(ctx, e.h, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if !keep["yoho/shop-prod-web:v1"] || !keep["yoho/shop-staging-web:s1"] {
		t.Fatalf("keep %v", keep)
	}
	e.noGates()

	// Another Destination is mid-migration: never prune from a partial scan.
	e.write(safepath.JournalPath("shop", "staging"), `{"kind":"forward","from":"x","to":"y"}`)
	_, err = RetainedImageRefs(ctx, e.h, "shop")
	var me *AppDirMigratingError
	if !errors.As(err, &me) || !strings.Contains(err.Error(), "Destination staging") {
		t.Fatalf("journal: %v", err)
	}
	h := &dockerHost{local: e.h, respond: func(s string) (string, error) {
		if strings.Contains(s, "docker image ls") {
			return "sha256:old|yoho/shop-prod-web|v0|0\n", nil
		}
		return "", nil
	}}
	var logs []string
	PruneImages(ctx, h, "shop", func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }, PruneOptions{})
	if strings.Contains(h.all(), "docker image rm") || !strings.Contains(strings.Join(logs, "\n"), "not pruning images: Destination staging") {
		t.Fatalf("pruned during a migration:\n%s\nlogs: %v", h.all(), logs)
	}
	os.Remove(safepath.JournalPath("shop", "staging"))

	// The App gate is held by a migration.
	e.mkdir(safepath.AppGateDir("shop"), 0o700)
	if _, err = RetainedImageRefs(ctx, e.h, "shop"); !errors.As(err, &me) || !strings.Contains(err.Error(), "App gate") {
		t.Fatalf("App gate: %v", err)
	}
}

func TestReadAndPlanAppDir(t *testing.T) {
	e := newADEnv(t)
	ctx := context.Background()
	want := e.path("apps/engine")
	state := func() AppDirState {
		s, err := ReadAppDir(ctx, e.h, "shop", "prod")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if _, ok := PlanAppDir(state(), "shop", "prod", ""); ok {
		t.Fatal("missing default: no change")
	}
	if ch, ok := PlanAppDir(state(), "shop", "prod", want); !ok || ch.Action != plan.ActionCreate {
		t.Fatalf("missing with app_dir: %+v", ch)
	}
	e.seed()
	ch, ok := PlanAppDir(state(), "shop", "prod", want)
	if !ok || ch.Action != plan.ActionUpdate || ch.Reasons[0] != "app_dir: "+e.canon+" (default) → "+want {
		t.Fatalf("forward: %+v", ch)
	}
	if _, ok := PlanAppDir(state(), "shop", "prod", ""); ok {
		t.Fatal("default dir: no change")
	}
	e.mustEnsure(want, "forward")
	if _, ok := PlanAppDir(state(), "shop", "prod", want); ok {
		t.Fatal("linked: no change")
	}
	if ch, ok := PlanAppDir(state(), "shop", "prod", ""); !ok || ch.Reasons[0] != "app_dir: "+want+" → "+e.canon+" (default)" {
		t.Fatalf("back: %+v", ch)
	}
	if ch, ok := PlanAppDir(state(), "shop", "prod", "/srv/b"); !ok || ch.Reasons[0] != "app_dir: "+want+" → /srv/b" {
		t.Fatalf("retarget: %+v", ch)
	}
	e.journal("retarget", want, "/srv/b")
	if ch, ok := PlanAppDir(state(), "shop", "prod", want); !ok || !strings.Contains(ch.Reasons[0], "finish interrupted app_dir migration") {
		t.Fatalf("journal: %+v", ch)
	}
	e.noGates()
}

// Review fixes for v0.1.8: refusals that used to happen only after the
// journal was written (leaving the Destination stuck) now happen before it.
func TestAppDirRefusesBeforeJournal(t *testing.T) {
	t.Run("unreadable non-empty target", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root can list any directory")
		}
		e := newADEnv(t)
		e.seed()
		w := e.path("locked")
		e.write(filepath.Join(w, "userfile"), "keep")
		e.mkdir(w, 0o300)
		t.Cleanup(func() { os.Chmod(w, 0o700) })
		if _, err := e.ensure(w); err == nil || !strings.Contains(err.Error(), "cannot list") {
			t.Fatalf("got %v", err)
		}
		e.noJournal()
		e.noGates()
	})
	t.Run("retarget physically nested through an alias", func(t *testing.T) {
		e := newADEnv(t)
		real, err := filepath.EvalSymlinks(e.base)
		if err != nil || real == e.base {
			t.Skip("needs a root-owned alias in the temp path (macOS /var -> /private/var)")
		}
		e.seed()
		a := e.path("a")
		e.mustEnsure(a, "forward")
		alias := filepath.Join(real, "a", "inner")
		if _, err := e.ensure(alias); err == nil || !strings.Contains(err.Error(), "is inside") {
			t.Fatalf("got %v", err)
		}
		e.noJournal()
		e.linked(a)
	})
}
