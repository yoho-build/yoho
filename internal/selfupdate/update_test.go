package selfupdate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUpgradeReplacement(t *testing.T) {
	for _, scenario := range []string{"success", "symlink", "verification failure", "missing artifact", "cancelled", "target changed", "package manager"} {
		t.Run(scenario, func(t *testing.T) {
			dir := t.TempDir()
			if scenario == "package manager" {
				dir = filepath.Join(dir, "Cellar", "yoho", "1", "bin")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			target := filepath.Join(dir, "yoho")
			old := []byte("old executable")
			if err := os.WriteFile(target, old, 0o751); err != nil {
				t.Fatal(err)
			}
			executable := target
			if scenario == "symlink" {
				executable = filepath.Join(dir, "link")
				if err := os.Symlink(target, executable); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			downloads := 0
			var downloadCache string
			u := updater{executable: func() (string, error) { return executable, nil }, latest: func(context.Context) (string, error) { return "v0.5.2", nil }, platform: "darwin/arm64"}
			u.download = func(_ context.Context, version, platform, cache string) (string, error) {
				downloads++
				downloadCache = cache
				if scenario == "missing artifact" {
					return filepath.Join(cache, "missing"), nil
				}
				if version != "v0.5.2" || platform != "darwin/arm64" {
					t.Fatal("wrong release or platform")
				}
				if scenario == "verification failure" {
					return "", errors.New("build provenance verification failed")
				}
				if scenario == "cancelled" {
					cancel()
				}
				if scenario == "target changed" {
					replacement := filepath.Join(dir, "other")
					if err := os.WriteFile(replacement, []byte("concurrent executable"), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(replacement, target); err != nil {
						t.Fatal(err)
					}
				}
				file := filepath.Join(cache, "verified")
				return file, os.WriteFile(file, []byte("new executable"), 0o755)
			}
			result, err := u.run(ctx, "v0.5.1")
			success := scenario == "success" || scenario == "symlink"
			if (err == nil) != success || result.Updated != success {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			got, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			want := "old executable"
			if success {
				want = "new executable"
			}
			if scenario == "target changed" {
				want = "concurrent executable"
			}
			if string(got) != want {
				t.Fatalf("binary=%q want %q", got, want)
			}
			if success {
				info, _ := os.Stat(target)
				if info.Mode().Perm() != 0o751 {
					t.Fatalf("permissions=%v", info.Mode())
				}
			}
			if scenario == "symlink" {
				info, _ := os.Lstat(executable)
				if info.Mode()&os.ModeSymlink == 0 {
					t.Fatal("symlink replaced")
				}
			}
			if scenario == "package manager" && downloads != 0 {
				t.Fatal("package manager install downloaded binary")
			}
			if downloadCache != "" {
				if _, err := os.Stat(downloadCache); !os.IsNotExist(err) {
					t.Fatalf("cache not cleaned: %v", err)
				}
			}
			leftovers, _ := filepath.Glob(filepath.Join(dir, ".yoho-upgrade-*"))
			if len(leftovers) != 0 {
				t.Fatalf("leftovers: %v", leftovers)
			}
		})
	}
}

func TestUpgradeNoReplacement(t *testing.T) {
	for _, current := range []string{"v0.5.2", "v0.6.0", "v0.10.0", "v99999999999999999999999999999.0.0"} {
		t.Run(current, func(t *testing.T) {
			u := updater{platform: "linux/amd64", latest: func(context.Context) (string, error) { return "v0.5.2", nil }, executable: func() (string, error) { t.Fatal("looked up executable for no-op"); return "", nil }}
			result, err := u.run(context.Background(), current)
			if err != nil || result.Updated || result.Version != current {
				t.Fatalf("%+v %v", result, err)
			}
		})
	}
}

func TestUpgradePreflightFailures(t *testing.T) {
	for _, tc := range []struct {
		name, platform, latest string
		failure                error
	}{
		{name: "unsupported", platform: "windows/amd64", latest: "v0.5.2"},
		{name: "network", platform: "linux/arm64", failure: errors.New("network unavailable")},
		{name: "invalid latest", platform: "linux/arm64", latest: "../../evil"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := updater{platform: tc.platform, latest: func(context.Context) (string, error) { return tc.latest, tc.failure }, executable: func() (string, error) { t.Fatal("preflight touched executable"); return "", nil }}
			if _, err := u.run(context.Background(), "dev"); err == nil {
				t.Fatal("wanted error")
			}
		})
	}
}

func TestNewerStable(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"v2.0.0", "v1.99.99", true}, {"v1.10.0", "v1.9.9", true}, {"v1.0.9", "v1.0.10", false}, {"dev", "v1.0.0", false}, {"v1.0.0-2-gabcdef", "v1.0.0", false},
	} {
		if got := newerStable(tc.a, tc.b); got != tc.want {
			t.Fatalf("%s > %s = %v", tc.a, tc.b, got)
		}
	}
}
