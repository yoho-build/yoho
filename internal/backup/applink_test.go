package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/safepath"
)

// appLinkLayout builds <root>/apps/shop/production -> base/engine (app_dir)
// with a marker, a generated secret and a current Release, as the deploy
// side's EnsureAppDir leaves it.
func appLinkLayout(t *testing.T) (base, target string) {
	t.Helper()
	base = privateTemp(t)
	old := release.Root
	release.Root = filepath.Join(base, "yoho")
	t.Cleanup(func() { release.Root = old })
	target = filepath.Join(base, "engine")
	for _, d := range []string{filepath.Join(release.Root, "apps", "shop"), filepath.Join(target, "generated"), filepath.Join(target, "releases", "v1")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(target, safepath.MarkerName), safepath.Marker("shop", "production"))
	write(filepath.Join(target, "generated", "PG"), "generated-value")
	write(filepath.Join(target, "releases", "v1", "release.json"), `{"version":"v1"}`)
	write(filepath.Join(target, "releases", "v1", "plan.json"), `[]`)
	write(filepath.Join(target, "releases", "v1", "compose.yaml"), "services: {}\n")
	if err := os.Symlink("releases/v1", filepath.Join(target, "current")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, release.AppDir("shop", "production")); err != nil {
		t.Fatal(err)
	}
	return base, target
}

func TestBackupTrustThroughAppDirLink(t *testing.T) {
	base, target := appLinkLayout(t)
	ctx := context.Background()
	h := &remote.Local{}

	// Run: generated-secret listing and staging, current Release version.
	names, err := ListGenerated(ctx, h, "shop", "production")
	if err != nil || strings.Join(names, ",") != "PG" {
		t.Fatalf("list: %v %v", names, err)
	}
	staging := filepath.Join(base, "staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := stageGenerated(ctx, h, "shop", "production", staging, names); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(staging, generatedPrefix, "PG")); string(b) != "generated-value" {
		t.Fatalf("staged %q", b)
	}
	if v := currentVersion(ctx, h, "shop", "production"); v != "v1" {
		t.Fatalf("current version %q", v)
	}
	// Restore: deployed plan/compose reads and generated-secret writes.
	for _, f := range []string{"plan.json", "compose.yaml"} {
		if err := localScript(appLink("shop", "production") + safeCurrentFile(release.AppDir("shop", "production"), f)); err != nil {
			t.Fatalf("current %s: %v", f, err)
		}
	}
	data := filepath.Join(base, "data")
	os.MkdirAll(filepath.Join(data, generatedPrefix), 0o700)
	os.WriteFile(filepath.Join(data, generatedPrefix, "PG"), []byte("restored"), 0o600)
	os.WriteFile(filepath.Join(data, generatedPrefix, "NEW"), []byte("new"), 0o600)
	wrote, changed, err := restoreGenerated(ctx, h, "shop", "production", data, []string{"NEW", "PG"}, func(string, ...any) {})
	if err != nil || strings.Join(wrote, ",") != "NEW,PG" || strings.Join(changed, ",") != "PG" {
		t.Fatalf("restore: %v %v %v", wrote, changed, err)
	}
	if b, _ := os.ReadFile(filepath.Join(target, "generated", "PG")); string(b) != "restored" {
		t.Fatalf("restored %q", b)
	}
	// Without the exception the same link is still refused.
	if err := localScript(safeDir(filepath.Join(release.AppDir("shop", "production"), "generated"), true, false)); err == nil {
		t.Fatal("App link accepted without yoho_applink")
	}
}

func TestBackupTrustRefusesUntrustedAppLink(t *testing.T) {
	cases := map[string]func(t *testing.T, base, target string) (string, string){
		"no marker": func(t *testing.T, base, target string) (string, string) {
			os.Remove(filepath.Join(target, safepath.MarkerName))
			return "", ""
		},
		"marker of another Destination": func(t *testing.T, base, target string) (string, string) {
			os.WriteFile(filepath.Join(target, safepath.MarkerName), []byte(safepath.Marker("shop", "staging")), 0o600)
			return "", ""
		},
		"marker is a link": func(t *testing.T, base, target string) (string, string) {
			m := filepath.Join(target, safepath.MarkerName)
			os.Rename(m, filepath.Join(base, "marker"))
			os.Symlink(filepath.Join(base, "marker"), m)
			return "", ""
		},
		"dangling": func(t *testing.T, base, target string) (string, string) {
			os.Rename(target, target+"-moved")
			return "", ""
		},
		"relative target": func(t *testing.T, base, target string) (string, string) {
			link := release.AppDir("shop", "production")
			os.Remove(link)
			os.Symlink("../../../engine", link)
			return "", ""
		},
		"public target": func(t *testing.T, base, target string) (string, string) {
			os.Chmod(target, 0o755)
			return "", ""
		},
		"target under shared parent": func(t *testing.T, base, target string) (string, string) {
			shared := filepath.Join(base, "shared")
			os.Mkdir(shared, 0o777)
			os.Chmod(shared, 0o777)
			moved := filepath.Join(shared, "engine")
			os.Rename(target, moved)
			link := release.AppDir("shop", "production")
			os.Remove(link)
			os.Symlink(moved, link)
			return "", ""
		},
		"link owned by another user": func(t *testing.T, base, target string) (string, string) {
			return `[ -L "$1" ] && [ -n "$(find "$1" -prune -user "$yoho_uid" -print)" ] || yoho_bad "$1"`,
				`[ -L "$1" ] && [ -n "$(find "$1" -prune -user 2147483646 -print)" ] || yoho_bad "$1"`
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			base, target := appLinkLayout(t)
			from, to := prepare(t, base, target)
			edit := func(s string) string {
				if from == "" {
					return s
				}
				if !strings.Contains(s, from) {
					t.Fatal("owner check not found")
				}
				return strings.ReplaceAll(s, from, to)
			}
			app := release.AppDir("shop", "production")
			scripts := []string{
				appLink("shop", "production") + safeDir(filepath.Join(app, "generated"), true, false),
				appLink("shop", "production") + safeCurrentFile(app, "release.json"),
				appLink("shop", "production") + safeFile(filepath.Join(app, "generated", "PG"), true),
			}
			if name == "public target" {
				// As for a real App directory, only checks that require the
				// App directory itself to be private (yoho_private) apply.
				scripts = scripts[1:2]
			}
			for _, s := range scripts {
				if err := localScript(edit(s)); err == nil || !strings.Contains(err.Error(), "unsafe Backup path") {
					t.Fatalf("untrusted App link accepted: %v", err)
				}
			}
			if from == "" && name != "public target" {
				if _, err := ListGenerated(context.Background(), &remote.Local{}, "shop", "production"); err == nil {
					t.Fatal("generated secrets listed through an untrusted link")
				}
				if v := currentVersion(context.Background(), &remote.Local{}, "shop", "production"); v != "" {
					t.Fatalf("current version read through an untrusted link: %q", v)
				}
			}
		})
	}
}

// Links inside the App directory stay rejected even through a trusted App link.
func TestBackupTrustRejectsLinksInsideTrustedAppDir(t *testing.T) {
	base, target := appLinkLayout(t)
	outside := filepath.Join(base, "outside")
	os.Mkdir(outside, 0o700)
	os.WriteFile(filepath.Join(outside, "PG"), []byte("foreign"), 0o600)
	os.RemoveAll(filepath.Join(target, "generated"))
	os.Symlink(outside, filepath.Join(target, "generated"))
	if _, err := ListGenerated(context.Background(), &remote.Local{}, "shop", "production"); err == nil {
		t.Fatal("linked generated dir inside app_dir accepted")
	}
}
