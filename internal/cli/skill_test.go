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
