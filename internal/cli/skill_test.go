package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/skills"
)

func runSkill(t *testing.T, args ...string) (string, error) {
	t.Helper()
	g := &globals{}
	c := skillCmd(g)
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetArgs(args)
	err := c.Execute()
	return buf.String(), err
}

func TestSkillInstallAllSkillsAllAgents(t *testing.T) {
	names := skills.Names()
	if len(names) == 0 {
		t.Fatal("no embedded skills")
	}
	dir := t.TempDir()
	out, err := runSkill(t, "install", "--dir", dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{".claude", ".agents"} {
		for _, name := range names {
			if _, err := os.Stat(filepath.Join(dir, base, "skills", name, "SKILL.md")); err != nil {
				t.Errorf("%s/%s: %v", base, name, err)
			}
		}
	}
	if got := strings.Count(strings.TrimSpace(out), "\n") + 1; got != 2*len(names) {
		t.Errorf("want %d lines, got %d:\n%s", 2*len(names), got, out)
	}
}

func TestSkillInstallSingleAgent(t *testing.T) {
	dir := t.TempDir()
	if _, err := runSkill(t, "install", "--dir", dir, "--agent", "codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".agents", "skills", "yoho", "SKILL.md")); err != nil {
		t.Error(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude")); err == nil {
		t.Error(".claude should not exist")
	}
}

func TestSkillPrint(t *testing.T) {
	for _, name := range skills.Names() {
		want, _ := skills.FS.ReadFile(name + "/SKILL.md")
		args := []string{"print"}
		if name != "yoho" {
			args = append(args, name)
		}
		out, err := runSkill(t, args...)
		if err != nil || out != string(want) {
			t.Errorf("print %s: err=%v", name, err)
		}
	}
	want, _ := skills.FS.ReadFile("yoho/SKILL.md")
	if out, err := runSkill(t, "print"); err != nil || out != string(want) {
		t.Errorf("default print: err=%v", err)
	}
}

func TestSkillPrintUnknown(t *testing.T) {
	_, err := runSkill(t, "print", "nope")
	if err == nil || !strings.Contains(err.Error(), "yoho") {
		t.Fatalf("want error listing skills, got %v", err)
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestSkillInstallOnlyAndExclude(t *testing.T) {
	dir := t.TempDir()
	if _, err := runSkill(t, "install", "--dir", dir, "--agent", "codex", "--only", "yoho"); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, ".agents", "skills")
	if !exists(filepath.Join(base, "yoho", "SKILL.md")) || exists(filepath.Join(base, "yoho-migrate-coolify")) || exists(filepath.Join(base, "yoho-team-access")) {
		t.Error("--only yoho should write only yoho")
	}
	// --only without a yoho-migrate-* skill drops the migrate references.
	refs, _ := filepath.Glob(filepath.Join(base, "yoho", "references", "migrate-*"))
	if len(refs) != 0 {
		t.Errorf("migrate references written: %v", refs)
	}
	if !exists(filepath.Join(base, "yoho", "references", "config-reference.md")) {
		t.Error("config-reference.md missing")
	}

	dir = t.TempDir()
	if _, err := runSkill(t, "install", "--dir", dir, "--agent", "codex", "--exclude", "yoho-migrate-coolify,yoho-team-access"); err != nil {
		t.Fatal(err)
	}
	base = filepath.Join(dir, ".agents", "skills")
	if !exists(filepath.Join(base, "yoho", "SKILL.md")) || exists(filepath.Join(base, "yoho-migrate-coolify")) || exists(filepath.Join(base, "yoho-team-access")) {
		t.Error("--exclude complement wrong")
	}
	// --exclude keeps default behaviour for the migrate references.
	if !exists(filepath.Join(base, "yoho", "references", "migrate-coolify.md")) {
		t.Error("--exclude alone should keep migrate references")
	}
}

func TestSkillInstallSelectionErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := runSkill(t, "install", "--dir", dir, "--only", "yoho", "--exclude", "yoho-team-access"); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("only+exclude: %v", err)
	}
	if _, err := runSkill(t, "install", "--dir", dir, "--only", "nope"); err == nil || !strings.Contains(err.Error(), "available:") {
		t.Errorf("unknown name: %v", err)
	}
	if exists(filepath.Join(dir, ".claude")) || exists(filepath.Join(dir, ".agents")) {
		t.Error("nothing should be written on error")
	}
}

func TestSkillInstallNoMigrateRefs(t *testing.T) {
	dir := t.TempDir()
	out, err := runSkill(t, "install", "--dir", dir, "--agent", "codex", "--no-migrate-refs")
	if err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(dir, ".agents", "skills")
	if refs, _ := filepath.Glob(filepath.Join(base, "yoho", "references", "migrate-*")); len(refs) != 0 {
		t.Errorf("migrate refs written: %v", refs)
	}
	if !exists(filepath.Join(base, "yoho-migrate-coolify", "SKILL.md")) {
		t.Error("the migrate skill itself should still install")
	}
	if !strings.Contains(out, "skipped 4 references/migrate-* files of skill yoho") {
		t.Errorf("missing skipped line:\n%s", out)
	}
}

func TestSkillList(t *testing.T) {
	out, err := runSkill(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range skills.Names() {
		d, err := skills.Describe(name)
		if err != nil || d == "" {
			t.Errorf("%s: description %q: %v", name, d, err)
		}
		if !strings.Contains(out, name) {
			t.Errorf("list missing %s", name)
		}
	}
	g := &globals{json: true}
	c := skillCmd(g)
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetArgs([]string{"list"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(buf.String(), `"event":"skill"`); got != len(skills.Names()) {
		t.Errorf("want %d skill events, got %d", len(skills.Names()), got)
	}
}

func TestSkillInstallRespectsSymlinkedLayout(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, ".agents", "skills")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", ".agents", "skills"), filepath.Join(dir, ".claude", "skills")); err != nil {
		t.Fatal(err)
	}
	// .agent/skills exists as a symlink too.
	if err := os.MkdirAll(filepath.Join(dir, ".agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", ".agents", "skills"), filepath.Join(dir, ".agent", "skills")); err != nil {
		t.Fatal(err)
	}
	g := &globals{json: true}
	c := skillCmd(g)
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetArgs([]string{"install", "--dir", dir, "--only", "yoho"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(buf.String(), `"event":"skill_installed"`); n != 1 {
		t.Errorf("want 1 skill_installed, got %d:\n%s", n, buf.String())
	}
	if n := strings.Count(buf.String(), `"event":"skill_linked"`); n != 2 {
		t.Errorf("want 2 skill_linked (.agents is the real one), got %d:\n%s", n, buf.String())
	}
	if !exists(filepath.Join(real, "yoho", "SKILL.md")) {
		t.Error("real copy missing")
	}
}

func TestSkillInstallPerSkillSymlinkAndAgentDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".agents", "skills", "yoho"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".claude", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".claude", "skills", "yoho")
	if err := os.Symlink(filepath.Join("..", "..", ".agents", "skills", "yoho"), link); err != nil {
		t.Fatal(err)
	}
	out, err := runSkill(t, "install", "--dir", dir, "--only", "yoho")
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Error("per-skill symlink was replaced")
	}
	if strings.Count(out, "installed skill") != 1 || !strings.Contains(out, "not written again") {
		t.Errorf("want one copy and a linked line:\n%s", out)
	}
	// .agent/skills is a separate target only when it already exists.
	if exists(filepath.Join(dir, ".agent")) {
		t.Error(".agent created")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".agent", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := runSkill(t, "install", "--dir", dir, "--only", "yoho"); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(dir, ".agent", "skills", "yoho", "SKILL.md")) {
		t.Error(".agent/skills/yoho not written although the directory exists")
	}
}

func TestYohoSkillWarnsAgainstBuildxHistory(t *testing.T) {
	for _, name := range []string{"yoho", "yoho-migrate-coolify"} {
		b, err := skills.FS.ReadFile(name + "/SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		s := string(b)
		i := strings.Index(s, "docker buildx history")
		if i < 0 || !strings.Contains(strings.ToLower(s[max(0, i-80):i+80]), "never") {
			t.Errorf("%s/SKILL.md must say to never run `docker buildx history` on a Server", name)
		}
	}
}

// Yoho must never run `docker buildx history` on a Server: on Docker 28.x it
// can crash dockerd. Guard the Go sources and embedded scripts under internal/.
// testdata holds generated skill text, which warns against it like skills/.
func TestNoBuildxHistoryInSources(t *testing.T) {
	needle := "buildx" + " history"
	err := filepath.WalkDir(filepath.Join("..", "..", "internal"), func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() && d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if err != nil || d.IsDir() || strings.HasSuffix(p, "_test.go") {
			return err
		}
		b, err := os.ReadFile(p)
		if err == nil && strings.Contains(string(b), needle) {
			t.Errorf("%s contains %q", p, needle)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestSkillListTruncatesHumanKeepsJSONFull(t *testing.T) {
	out, err := runSkill(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if n := len([]rune(line)); n > 80+40 {
			t.Errorf("line too wide (%d): %s", n, line)
		}
	}
	d, _ := skills.Describe("yoho")
	if strings.Contains(out, d) {
		t.Error("human list should truncate the long description")
	}
	c := skillCmd(&globals{json: true})
	var buf bytes.Buffer
	c.SetOut(&buf)
	c.SetArgs([]string{"list"})
	if err := c.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), strings.ReplaceAll(d, `"`, `\"`)) {
		t.Error("--json keeps the full description")
	}
}
