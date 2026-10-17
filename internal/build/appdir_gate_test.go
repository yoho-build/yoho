package build

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

// Server builds create source.run-* and publish source/ inside the App
// directory, so they pass through the app_dir gate and refuse during a
// migration.
func TestServerBuildSourceGate(t *testing.T) {
	old, oldTries := release.Root, safepath.GateTries
	release.Root, safepath.GateTries = t.TempDir(), 1
	t.Cleanup(func() { release.Root, safepath.GateTries = old, oldTries })
	o := Options{App: "shop", Destination: "prod", Host: &remote.Local{}}
	o.cacheDir = o.sourceDir()
	o.runDir = o.cacheDir + ".run-g1"
	o.SourceDir = filepath.Join(o.runDir, "source")
	run := func() error {
		return (&remote.Local{}).Run(context.Background(), remote.Cmd{Script: "set -eu\n" + o.sourceGate("build") + "mkdir -p " + remote.Quote(o.runDir) + "\n"})
	}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(safepath.GateDir("shop", "prod")); !os.IsNotExist(err) {
		t.Fatal("gate not released after the build step")
	}
	os.RemoveAll(o.runDir)

	gate := safepath.GateDir("shop", "prod")
	os.MkdirAll(gate, 0o700)
	if err := run(); err == nil || !strings.Contains(err.Error(), "gate") {
		t.Fatalf("held gate: %v", err)
	}
	os.RemoveAll(gate)

	os.WriteFile(safepath.JournalPath("shop", "prod"), []byte(`{}`), 0o600)
	if err := run(); err == nil || !strings.Contains(err.Error(), "migration in progress") {
		t.Fatalf("journal: %v", err)
	}
	if _, err := os.Stat(o.runDir); !os.IsNotExist(err) {
		t.Fatal("source.run created during a migration")
	}
}
