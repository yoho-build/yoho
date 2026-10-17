package build

import (
	"context"
	"io"
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

// The gate is held only for creating source.run-* and for the renames, not
// for the slow cp -a / rm -rf, so concurrent lock takers don't time out.
// source.run-* still exists during those steps, which keeps migration out.
func TestServerBuildGateNotHeldForSlowSteps(t *testing.T) {
	old := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = old })
	h := &recHost{}
	o := Options{App: "shop", Destination: "prod", Host: h, Out: io.Discard}
	o.cacheDir = o.sourceDir()
	o.runDir = o.cacheDir + ".run-g1"
	o.SourceDir = filepath.Join(o.runDir, "source")
	_ = syncSource(context.Background(), o, &sourcePlan{root: t.TempDir()})
	for _, ok := range []bool{true, false} {
		finishSource(o, ok)
	}
	if len(h.cmds) < 3 {
		t.Fatalf("want sync and two cleanup scripts, got %d", len(h.cmds))
	}
	for i, c := range h.cmds[:3] {
		for _, slow := range []string{"cp -a ", "rm -rf "} {
			// The last one: an rm -rf of a leftover .old from a killed run
			// may still run inside the cache swap.
			j := strings.LastIndex(c.Script, slow)
			if j < 0 {
				continue
			}
			d := strings.Index(c.Script, "yoho_gate_drop\n")
			if d < 0 || d > j {
				t.Fatalf("script %d runs %q before dropping the gate:\n%s", i, slow, c.Script)
			}
		}
	}
}
