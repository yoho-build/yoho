package version

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t", "GIT_CONFIG_GLOBAL=/dev/null")
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run(t, dir, "init", "-q")

	if _, err := FromGit(dir); err == nil || !strings.Contains(err.Error(), "--version") {
		t.Fatalf("no commits: want error suggesting --version, got %v", err)
	}

	os.WriteFile(filepath.Join(dir, "a"), []byte("1"), 0o644)
	run(t, dir, "add", "a")
	run(t, dir, "commit", "-q", "-m", "init")

	v, err := FromGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(v) || IsUncommitted(v) || !Valid(v) {
		t.Fatalf("clean version %q", v)
	}

	os.WriteFile(filepath.Join(dir, "b"), []byte("2"), 0o644)
	d1, err := FromGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^`+v+`_uncommitted_[0-9a-f]{8}$`).MatchString(d1) || !IsUncommitted(d1) || !Valid(d1) {
		t.Fatalf("dirty version %q", d1)
	}
	if d2, _ := FromGit(dir); d2 == d1 {
		t.Fatal("dirty versions should differ")
	}
}

func TestFromGitNotARepo(t *testing.T) {
	if _, err := FromGit(t.TempDir()); err == nil || !strings.Contains(err.Error(), "--version") {
		t.Fatalf("got %v", err)
	}
}

func TestValid(t *testing.T) {
	for v, ok := range map[string]bool{"abc123": true, "v1.2.3": true, "": false, "-x": false, "a/b": false, "a b": false, strings.Repeat("a", 129): false} {
		if Valid(v) != ok {
			t.Errorf("Valid(%q) != %v", v, ok)
		}
	}
}
