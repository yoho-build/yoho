package audit

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func setup(t *testing.T, mkApp bool) (*remote.Local, string) {
	t.Helper()
	old := release.Root
	release.Root = t.TempDir()
	t.Cleanup(func() { release.Root = old })
	dir := release.AppDir("shop", "production")
	if mkApp {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return &remote.Local{HostName: "local"}, dir
}

func rec(run, phase string, at time.Time) Record {
	return Record{Time: at.UTC(), Run: run, Phase: phase, Performer: "ann@laptop", Command: "deploy",
		App: "shop", Destination: "production", Via: ViaCLI, Yoho: "dev"}
}

func TestRedactorMasksSecretValueInExecArgv(t *testing.T) {
	r := NewRedactor("hunter2-very-secret")
	argv := []string{"sh", "-c", "echo hunter2-very-secret", "DB_PASSWORD=plainone", "--token", "abc123xyz", "--api-key=zzz999", "https://u:pw0rd@db.example/x", "ls"}
	got := strings.Join(r.Argv(argv), " ")
	for _, leak := range []string{"hunter2-very-secret", "plainone", "abc123xyz", "zzz999", "pw0rd"} {
		if strings.Contains(got, leak) {
			t.Errorf("argv leaks %q: %s", leak, got)
		}
	}
	for _, keep := range []string{"sh", "ls", "db.example"} {
		if !strings.Contains(got, keep) {
			t.Errorf("argv lost %q: %s", keep, got)
		}
	}
	line, err := Record{Time: time.Now(), Command: "app exec", Args: ArgvArgs("web", r.Argv(argv))}.Line()
	if err != nil || strings.Contains(string(line), "hunter2-very-secret") {
		t.Fatalf("line %s err %v", line, err)
	}
}

func TestRedactorStoreAndErrorText(t *testing.T) {
	r := NewRedactor().WithStore(func(s string) string { return strings.ReplaceAll(s, "storevalue", "***") })
	if got := r.String("failed: storevalue and Authorization: Bearer abcdefghij"); strings.Contains(got, "storevalue") || strings.Contains(got, "abcdefghij") {
		t.Fatal(got)
	}
	var nilR *Redactor
	if nilR.String("x") != "x" {
		t.Fatal("nil redactor")
	}
}

func TestLineBoundsRecord(t *testing.T) {
	r := rec("r1", Finished, time.Now())
	r.Error = "boom\nsecond line " + strings.Repeat("x", 1000)
	r.Args = ArgvArgs("web", []string{strings.Repeat("a", 5000)})
	b, err := r.Line()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "\n") || len(b) > maxLine {
		t.Fatalf("line %d bytes", len(b))
	}
	var back Record
	if err := json.Unmarshal(b, &back); err != nil || back.Error != "boom" {
		t.Fatalf("%v %+v", err, back)
	}
	if a := ArgvArgs("web", make([]string, 200))["argv"].([]string); len(a) != maxArgs+1 {
		t.Fatalf("argv len %d", len(a))
	}
}

type recordingHost struct {
	remote.Host
	script, stdin string
}

func (h *recordingHost) Name() string { return "rec" }
func (h *recordingHost) Run(_ context.Context, c remote.Cmd) error {
	h.script = c.Script
	b, _ := io.ReadAll(c.Stdin)
	h.stdin = string(b)
	return nil
}

func TestScriptHoldsNoRecordValues(t *testing.T) {
	old := release.Root
	release.Root = "/srv/yo'ho"
	defer func() { release.Root = old }()
	s := AppendScript("shop", "production")
	if !strings.Contains(s, remote.Quote("/srv/yo'ho/audit/shop/production.log")) {
		t.Fatal("path not quoted")
	}
	if !strings.Contains(s, "yoho_line=$(cat)") {
		t.Fatal("the record is not read from stdin")
	}
	h := &recordingHost{}
	r := rec("r1", Started, time.Now())
	r.Args = map[string]any{"argv": []string{"echo", "'; rm -rf / #", "$(whoami)"}}
	if err := Append(context.Background(), h, r); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(h.script, "rm -rf /") || strings.Contains(h.script, "whoami") || strings.Contains(h.script, "ann@laptop") {
		t.Fatalf("record value in script: %s", h.script)
	}
	if !strings.Contains(h.stdin, "rm -rf /") {
		t.Fatalf("stdin %q", h.stdin)
	}
}

func TestStartedFinishedPairing(t *testing.T) {
	h, _ := setup(t, true)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Minute)
	zero, one := 0, 1
	steps := []Record{rec("a", Started, t0), rec("b", Started, t0.Add(time.Second)), rec("a", Finished, t0.Add(2*time.Second)), rec("c", Started, t0.Add(3*time.Second))}
	steps[2].Exit, steps[2].Seconds = &zero, 2
	fb := rec("b", Finished, t0.Add(4*time.Second))
	fb.Exit, fb.Error = &one, "deploy failed"
	steps = append(steps, fb)
	for _, r := range steps {
		if err := Append(ctx, h, r); err != nil {
			t.Fatal(err)
		}
	}
	fi, err := os.Stat(Path("shop", "production"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("%v %v", fi, err)
	}
	entries, err := Read(ctx, h, "shop", "production", 100)
	if err != nil || len(entries) != 5 {
		t.Fatalf("%d %v", len(entries), err)
	}
	runs := Pair(entries)
	if len(runs) != 3 {
		t.Fatalf("runs %d", len(runs))
	}
	want := []string{"ok", "exit 1: deploy failed", "unfinished"}
	for i := range want {
		if got := runs[i].Result(); got != want[i] {
			t.Errorf("run %d: %q want %q", i, got, want[i])
		}
	}
	if d, ok := runs[0].Duration(); !ok || d != 2*time.Second {
		t.Errorf("duration %v %v", d, ok)
	}
	if _, ok := runs[2].Duration(); ok {
		t.Error("unfinished run has a duration")
	}
	if last := Select(runs, time.Time{}, 2); len(last) != 2 || last[0].First().Run != "b" {
		t.Errorf("select %+v", last)
	}
	if recent := Select(runs, t0.Add(1500*time.Millisecond), 50); len(recent) != 1 || recent[0].First().Run != "c" {
		t.Errorf("since %+v", recent)
	}
}

func TestFirstWriteWithoutAppDir(t *testing.T) {
	h, dir := setup(t, false)
	if err := Append(context.Background(), h, rec("a", Started, time.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("App dir was created: %v", err)
	}
	for _, d := range []string{filepath.Join(release.Root, "audit"), filepath.Dir(Path("shop", "production"))} {
		if fi, err := os.Stat(d); err != nil || fi.Mode().Perm() != 0o700 {
			t.Fatalf("%s: %v %v", d, fi, err)
		}
	}
	if Path("shop", "production") != filepath.Join(release.Root, "audit", "shop", "production.log") {
		t.Fatal(Path("shop", "production"))
	}
	es, err := Read(context.Background(), h, "shop", "production", 10)
	if err != nil || len(es) != 1 || es[0].Phase != Started {
		t.Fatalf("%v %v", es, err)
	}
}

func TestSurvivesAppDirRemoval(t *testing.T) {
	h, dir := setup(t, true)
	ctx := context.Background()
	if err := Append(ctx, h, rec("a", Started, time.Now())); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := Append(ctx, h, rec("a", Finished, time.Now())); err != nil {
		t.Fatal(err)
	}
	es, err := Read(ctx, h, "shop", "production", 10)
	if err != nil || len(es) != 2 {
		t.Fatalf("%v %v", es, err)
	}
}

func TestMigratesLegacyLog(t *testing.T) {
	h, dir := setup(t, true)
	ctx := context.Background()
	t0 := time.Now().Add(-time.Hour)
	line := func(run string, at time.Time) string {
		b, _ := rec(run, Started, at).Line()
		return string(b) + "\n"
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.WriteFile(filepath.Join(dir, "audit.log.1"), []byte(line("old1", t0)), 0o600))
	must(os.WriteFile(filepath.Join(dir, "audit.log"), []byte(line("old2", t0.Add(time.Second))), 0o600))
	es, err := Read(ctx, h, "shop", "production", 10)
	if err != nil || len(es) != 2 || es[0].Run != "old1" {
		t.Fatalf("%v %v", es, err)
	}
	must(Append(ctx, h, rec("new", Started, time.Now())))
	for _, n := range []string{"audit.log", "audit.log.1"} {
		if _, err := os.Stat(filepath.Join(dir, n)); !os.IsNotExist(err) {
			t.Errorf("legacy %s still there: %v", n, err)
		}
	}
	es, err = Read(ctx, h, "shop", "production", 10)
	if err != nil || len(es) != 3 || es[0].Run != "old1" || es[1].Run != "old2" || es[2].Run != "new" {
		t.Fatalf("%+v %v", es, err)
	}
	must(Append(ctx, h, rec("new2", Started, time.Now())))
	if es, _ = Read(ctx, h, "shop", "production", 10); len(es) != 4 {
		t.Fatalf("%d entries", len(es))
	}
}

func TestRotation(t *testing.T) {
	h, _ := setup(t, true)
	dir := filepath.Dir(Path("shop", "production"))
	old := MaxBytes
	MaxBytes = 600
	defer func() { MaxBytes = old }()
	ctx := context.Background()
	for i := range 40 {
		if err := Append(ctx, h, rec(NewRun(), Started, time.Now().Add(time.Duration(i)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"production.log", "production.log.1", "production.log.2"} {
		fi, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if fi.Size() > 1200 {
			t.Errorf("%s is %d bytes", name, fi.Size())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "production.log.3")); err == nil {
		t.Error("kept more than 2 rotated files")
	}
	entries, err := Read(ctx, h, "shop", "production", 1000)
	if err != nil || len(entries) < 10 || len(entries) >= 40 {
		t.Fatalf("%d entries, %v", len(entries), err)
	}
	for i := 1; i < len(entries); i++ {
		if entries[i].Time.Before(entries[i-1].Time) {
			t.Fatal("entries are not oldest first")
		}
	}
}

func TestParseSkipsGarbage(t *testing.T) {
	good, _ := json.Marshal(rec("a", Started, time.Now()))
	es := Parse("not json\n\n{\"x\":1}\n" + string(good) + "\n")
	if len(es) != 1 || es[0].Raw != string(good) {
		t.Fatalf("%+v", es)
	}
}

func TestRefusesSymlinkedLog(t *testing.T) {
	h, _ := setup(t, true)
	target := filepath.Join(t.TempDir(), "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(Path("shop", "production")), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, Path("shop", "production")); err != nil {
		t.Fatal(err)
	}
	if err := Append(context.Background(), h, rec("a", Started, time.Now())); err == nil {
		t.Fatal("appended through a symlink")
	}
	if b, _ := os.ReadFile(target); len(b) != 0 {
		t.Fatalf("symlink target written: %q", b)
	}
}

func TestRefusesSymlinkedAuditDir(t *testing.T) {
	h, _ := setup(t, true)
	other := t.TempDir()
	if err := os.MkdirAll(filepath.Join(release.Root, "audit"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(release.Root, "audit", "shop")); err != nil {
		t.Fatal(err)
	}
	if err := Append(context.Background(), h, rec("a", Started, time.Now())); err == nil {
		t.Fatal("wrote through a symlinked audit dir")
	}
	if es, _ := os.ReadDir(other); len(es) != 0 {
		t.Fatalf("symlink target written: %v", es)
	}
}
