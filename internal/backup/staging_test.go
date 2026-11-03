package backup

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func stagingRoot(t *testing.T) string {
	t.Helper()
	root := privateTemp(t)
	old := release.Root
	release.Root = root
	t.Cleanup(func() { release.Root = old })
	base := release.BackupDir("shop", "production")
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	for p := base; p != root; p = filepath.Dir(p) {
		if err := os.Chmod(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

func makeStaging(t *testing.T, base, name, token string, prior bool) string {
	t.Helper()
	h := &remote.Local{}
	dir := filepath.Join(base, name)
	if err := createPrivateStaging(context.Background(), h, base, dir, token); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data", "yoho-generated")
	if err := os.MkdirAll(data, 0o700); err != nil {
		t.Fatal(err)
	}
	for f, v := range map[string]string{
		filepath.Join(data, "PG_PASSWORD"):                  "s3cret",
		filepath.Join(dir, "data", "db-postgres.dump"):      "dump",
		filepath.Join(dir, "data", "db-pgdata.tar.gz"):      "tar",
		filepath.Join(dir, "data", "manifest.json"):         "{}",
		filepath.Join(dir, "restic-or-archive-leftover.7z"): "x",
	} {
		if err := os.WriteFile(f, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if prior {
		if err := os.WriteFile(filepath.Join(dir, priorStateFile), []byte(`{"Runtime":"compose","Objects":[{"ID":"c1","Service":"web","Status":"running"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

const tok = "0123456789abcdef0123456789abcdef"

func TestListAndRemoveStaging(t *testing.T) {
	base := stagingRoot(t)
	ctx := context.Background()
	h := &remote.Local{}
	drill := makeStaging(t, base, "drill-20260101T000000Z-"+tok, tok, false)
	restore := makeStaging(t, base, "restore-20260102T000000Z-"+tok, tok, true)
	// Not proven Yoho-owned or not a staging name: never listed or removed.
	noMarker := filepath.Join(base, "drill-20260103T000000Z-"+tok)
	os.Mkdir(noMarker, 0o700)
	badMarker := filepath.Join(base, "drill-20260104T000000Z-"+tok)
	os.Mkdir(badMarker, 0o700)
	os.WriteFile(filepath.Join(badMarker, checkOwnerMarker), []byte("not-a-token"), 0o600)
	public := filepath.Join(base, "drill-20260105T000000Z-"+tok)
	os.Mkdir(public, 0o755)
	os.WriteFile(filepath.Join(public, checkOwnerMarker), []byte(tok), 0o600)
	outside := privateTemp(t)
	os.WriteFile(filepath.Join(outside, "keep"), []byte("x"), 0o600)
	link := filepath.Join(base, "drill-20260106T000000Z-"+tok)
	os.Symlink(outside, link)
	other := filepath.Join(base, "run-"+tok)
	os.Mkdir(other, 0o700)
	// A symlink inside a staging dir is removed, not followed.
	os.Symlink(outside, filepath.Join(drill, "data", "escape"))

	dirs, err := ListStaging(ctx, h, "shop", "production", StagingDrill)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0].Path != drill || dirs[0].PriorState || dirs[0].Created.Format(tsLayout) != "20260101T000000Z" {
		t.Fatalf("drill staging = %+v", dirs)
	}
	rdirs, err := ListStaging(ctx, h, "shop", "production", StagingRestore)
	if err != nil {
		t.Fatal(err)
	}
	if len(rdirs) != 1 || rdirs[0].Path != restore || !rdirs[0].PriorState {
		t.Fatalf("restore staging = %+v", rdirs)
	}
	if err := RemoveStaging(ctx, h, "shop", "production", dirs[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(drill); !os.IsNotExist(err) {
		t.Fatalf("drill staging not removed: %v", err)
	}
	for _, p := range []string{noMarker, badMarker, public, link, other, restore, filepath.Join(outside, "keep")} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s must survive: %v", p, err)
		}
	}
	// Forged entries are refused before any shell runs.
	for _, d := range []StagingDir{
		{Kind: StagingDrill, Name: "run-" + tok, Path: other},
		{Kind: StagingDrill, Name: filepath.Base(badMarker), Path: badMarker},
		{Kind: StagingDrill, Name: filepath.Base(public), Path: public},
		{Kind: StagingDrill, Name: filepath.Base(link), Path: link},
		{Kind: StagingDrill, Name: "drill-20260101T000000Z-" + tok, Path: "/etc"},
	} {
		if err := RemoveStaging(ctx, h, "shop", "production", d); err == nil {
			t.Errorf("removed %+v", d)
		}
	}
	if _, err := os.Lstat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("symlink target touched")
	}
}

func TestScrubStagingKeepsPriorStateAndSelectedDumps(t *testing.T) {
	base := stagingRoot(t)
	ctx := context.Background()
	h := &remote.Local{}
	dir := makeStaging(t, base, "restore-20260102T000000Z-"+tok, tok, true)
	data := filepath.Join(dir, "data")
	dump := filepath.Join(data, "db-postgres.dump")
	if err := scrubStaging(ctx, h, dir, "ffffffffffffffffffffffffffffffff", data, nil, true); err == nil {
		t.Fatal("scrub with a foreign token must refuse")
	}
	if err := scrubStaging(ctx, h, dir, tok, data, []string{"/etc/passwd"}, true); err == nil {
		t.Fatal("keeping a file outside data must refuse")
	}
	if err := scrubStaging(ctx, h, dir, tok, data, []string{dump}, true); err != nil {
		t.Fatal(err)
	}
	var left []string
	filepath.Walk(dir, func(p string, _ os.FileInfo, _ error) error {
		left = append(left, strings.TrimPrefix(p, dir))
		return nil
	})
	if got := strings.Join(left, " "); got != " /.yoho-check-owner /data /data/db-postgres.dump /prior-state.json" {
		t.Fatalf("left %q", got)
	}
	if err := scrubStaging(ctx, h, dir, tok, data, nil, true); err != nil {
		t.Fatal(err)
	}
	left = nil
	filepath.Walk(dir, func(p string, _ os.FileInfo, _ error) error {
		left = append(left, strings.TrimPrefix(p, dir))
		return nil
	})
	if got := strings.Join(left, " "); got != " /.yoho-check-owner /prior-state.json" {
		t.Fatalf("left %q", got)
	}
}

func TestMergePriorResumesFirstAttemptsRunningSet(t *testing.T) {
	zero := 0
	cur := &restoreState{Runtime: "compose", Objects: []restoreObject{
		{ID: "c-web", Service: "web", Status: "exited"},
		{ID: "c-db", Service: "db", Status: "exited"},
		{ID: "c-idle", Service: "idle", Status: "exited"},
		{ID: "c-new", Service: "worker", Status: "exited"},
		{ID: "c-up", Service: "up", Status: "running"},
	}}
	first := &restoreState{Runtime: "compose", Objects: []restoreObject{
		{ID: "c-web", Service: "web", Status: "running"},
		{ID: "c-db", Service: "db", Status: "running"},
		{ID: "c-idle", Service: "idle", Status: "exited"},
		{ID: "c-old", Service: "worker", Status: "running"},
	}}
	second := &restoreState{Runtime: "compose", Objects: []restoreObject{
		{ID: "c-web", Service: "web", Status: "exited"},
	}}
	revived, unmatched := cur.mergePrior([]*restoreState{first, second})
	if strings.Join(revived, ",") != "db,web" || strings.Join(unmatched, ",") != "worker" {
		t.Fatalf("revived %v unmatched %v", revived, unmatched)
	}
	for _, o := range cur.Objects {
		want := map[string]string{"web": "running", "db": "running", "idle": "exited", "worker": "exited", "up": "running"}[o.Service]
		if o.Status != want {
			t.Errorf("%s = %s, want %s", o.Service, o.Status, want)
		}
	}
	sw := &restoreState{Runtime: "swarm", Objects: []restoreObject{{ID: "p_web", Service: "web", Replicas: &zero}}}
	three := 3
	revived, _ = sw.mergePrior([]*restoreState{{Runtime: "swarm", Objects: []restoreObject{{ID: "p_web", Service: "web", Replicas: &three}}}, {Runtime: "compose", Objects: []restoreObject{{ID: "p_web", Service: "web", Status: "running"}}}})
	if len(revived) != 1 || *sw.Objects[0].Replicas != 3 {
		t.Fatalf("swarm revived %v replicas %d", revived, *sw.Objects[0].Replicas)
	}
}

// A killed `backup run` or `backup check` leaves run-/check- staging with no
// time in the name: listed only once untouched for StaleAfter.
func TestStaleRunAndCheckStaging(t *testing.T) {
	base := stagingRoot(t)
	ctx := context.Background()
	h := &remote.Local{}
	run := makeStaging(t, base, "run-"+tok, tok, false)
	check := makeStaging(t, base, "check-"+tok, tok, false)
	old := time.Now().Add(-StaleAfter - time.Hour)
	os.Chtimes(run, old, old)
	for kind, want := range map[string]int{StagingRun: 1, StagingCheck: 0} {
		dirs, err := ListStaging(ctx, h, "shop", "production", kind)
		if err != nil || len(dirs) != want {
			t.Fatalf("%s: %+v %v (fresh check staging may be in use)", kind, dirs, err)
		}
		if want == 1 {
			if err := RemoveStaging(ctx, h, "shop", "production", dirs[0]); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := os.Stat(run); !os.IsNotExist(err) {
		t.Fatalf("stale run staging kept: %v", err)
	}
	if _, err := os.Stat(check); err != nil {
		t.Fatalf("fresh check staging removed: %v", err)
	}
	if err := RemoveStaging(ctx, h, "shop", "production", StagingDir{Kind: StagingRun, Name: "run-x", Path: base + "/run-x"}); err == nil {
		t.Fatal("bad name must be refused")
	}
}
