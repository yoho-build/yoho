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
	if d2, err := FromGit(dir); err != nil || d2 != d1 {
		t.Fatalf("same dirty content should be stable, got %q and %q (%v)", d1, d2, err)
	}
}

func TestDirtyContentHash(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	run(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("ignored.txt\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "a"), []byte("1"), 0o644)
	run(t, dir, "add", ".gitignore", "a")
	run(t, dir, "commit", "-q", "-m", "init")

	clean, err := FromGit(dir)
	if err != nil {
		t.Fatal(err)
	}

	os.WriteFile(filepath.Join(dir, "a"), []byte("2"), 0o644)
	d1, err := FromGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	d2, err := FromGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 || d1 == clean || !IsUncommitted(d1) {
		t.Fatalf("stable dirty version: clean %q d1 %q d2 %q", clean, d1, d2)
	}

	os.WriteFile(filepath.Join(dir, "a"), []byte("3"), 0o644)
	d3, err := FromGit(dir)
	if err != nil || d3 == d1 {
		t.Fatalf("tracked edit should change version: %q -> %q (%v)", d1, d3, err)
	}

	os.WriteFile(filepath.Join(dir, "b"), []byte("new"), 0o644)
	d4, err := FromGit(dir)
	if err != nil || d4 == d3 {
		t.Fatalf("untracked file should change version: %q -> %q (%v)", d3, d4, err)
	}

	os.WriteFile(filepath.Join(dir, "ignored.txt"), []byte("nope"), 0o644)
	d5, err := FromGit(dir)
	if err != nil || d5 != d4 {
		t.Fatalf("ignored file should not change version: %q -> %q (%v)", d4, d5, err)
	}

	os.Remove(filepath.Join(dir, "b"))
	os.WriteFile(filepath.Join(dir, "a"), []byte("xyz"), 0o644)
	unstaged, err := FromGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "a")
	staged, err := FromGit(dir)
	if err != nil || staged != unstaged {
		t.Fatalf("staged and unstaged same content: %q vs %q (%v)", unstaged, staged, err)
	}

	os.WriteFile(filepath.Join(dir, "c"), []byte("same"), 0o644)
	plain, err := FromGit(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "c"), 0o755); err != nil {
		t.Fatal(err)
	}
	execBit, err := FromGit(dir)
	if err != nil || execBit == plain {
		t.Fatalf("executable bit should change version: %q vs %q (%v)", plain, execBit, err)
	}
}

func TestSubdirUntrackedAtRoot(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	sub := filepath.Join(root, "source")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, root, "init", "-q")
	os.WriteFile(filepath.Join(sub, "yoho.yml"), []byte("x"), 0o644)
	run(t, root, "add", ".")
	run(t, root, "commit", "-q", "-m", "init")

	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("one"), 0o644)
	v1, err := FromGit(sub)
	if err != nil {
		t.Fatal(err)
	}
	const emptyHash = "e3b0c442"
	if !IsUncommitted(v1) || strings.HasSuffix(v1, emptyHash) {
		t.Fatalf("untracked file outside dir must count: %q", v1)
	}
	if fromRoot, err := FromGit(root); err != nil || fromRoot != v1 {
		t.Fatalf("version differs by directory: root %q sub %q (%v)", fromRoot, v1, err)
	}

	os.WriteFile(filepath.Join(root, "notes.txt"), []byte("two"), 0o644)
	v2, err := FromGit(sub)
	if err != nil || v2 == v1 {
		t.Fatalf("editing the untracked file should change version: %q -> %q (%v)", v1, v2, err)
	}

	os.WriteFile(filepath.Join(sub, "yoho.yml"), []byte("y"), 0o644)
	v3, err := FromGit(sub)
	if err != nil || v3 == v2 {
		t.Fatalf("tracked edit should change version: %q -> %q (%v)", v2, v3, err)
	}
	if fromRoot, err := FromGit(root); err != nil || fromRoot != v3 {
		t.Fatalf("version differs by directory: root %q sub %q (%v)", fromRoot, v3, err)
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
