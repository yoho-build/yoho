package dist

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const goodNotes = "# v1.2.3\n\n## Upgrade notes\n\n- **Gate:** no.\n\n## Changes\n\n- x\n"

// repoRoot is the repository root, from this file's path.
func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// runNotesCheck runs scripts/check-release-notes.sh on dir with args.
func runNotesCheck(t *testing.T, dir string, args ...string) (string, string, int) {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command(sh, append([]string{filepath.Join(repoRoot(), "scripts", "check-release-notes.sh")}, args...)...)
	cmd.Env = append(os.Environ(), "RELEASE_NOTES_DIR="+dir)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), code
}

// Issue #66: release notes need an Upgrade notes section, and a stable
// tag cannot be released without notes.
func TestReleaseNotesCheck(t *testing.T) {
	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	write(dir, "v1.2.3.md", goodNotes)
	write(dir, "next.md", strings.Replace(goodNotes, "# v1.2.3", "# Next release\n\n<!-- renamed at release -->", 1))
	if _, stderr, code := runNotesCheck(t, dir); code != 0 {
		t.Fatalf("good notes: %d %s", code, stderr)
	}
	out, stderr, code := runNotesCheck(t, dir, "--tag", "v1.2.3")
	if code != 0 || strings.TrimSpace(out) != filepath.Join(dir, "v1.2.3.md") {
		t.Fatalf("--tag v1.2.3: %d %q %s", code, out, stderr)
	}

	for name, c := range map[string]struct {
		args []string
		body string // written as v1.2.4.md when set
		want string
	}{
		"stable tag without notes":   {args: []string{"--tag", "v1.2.4"}, want: "v1.2.4.md is missing"},
		"no section":                 {args: []string{"--tag", "v1.2.4"}, body: "# v1.2.4\n\n## Changes\n\n- x\n", want: "no '## Upgrade notes' section"},
		"section in a sentence only": {args: []string{"--tag", "v1.2.4"}, body: "# v1.2.4\n\nsee ## Upgrade notes\n", want: "no '## Upgrade notes' section"},
		"empty section":              {args: []string{"--tag", "v1.2.4"}, body: "# v1.2.4\n\n## Upgrade notes\n\n<!-- todo -->\n\n## Changes\n\n- x\n", want: "section is empty"},
		"every file is checked":      {body: "# v1.2.4\n\n## Upgrade notes\n", want: "v1.2.4.md: the '## Upgrade notes' section is empty"},
		"tag that is a path":         {args: []string{"--tag", "../../etc/passwd"}, want: "is not vMAJOR.MINOR.PATCH"},
	} {
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			write(d, "v1.2.3.md", goodNotes)
			if c.body != "" {
				write(d, "v1.2.4.md", c.body)
			}
			out, stderr, code := runNotesCheck(t, d, c.args...)
			if code != 1 || !strings.Contains(stderr, c.want) || out != "" {
				t.Fatalf("exit %d, stdout %q, stderr %q; want %q", code, out, stderr, c.want)
			}
		})
	}

	// A prerelease may go without notes; when it has a file, it is checked.
	pre := t.TempDir()
	if out, stderr, code := runNotesCheck(t, pre, "--tag", "v1.3.0-rc.1"); code != 0 || out != "" {
		t.Fatalf("prerelease without notes: %d %q %s", code, out, stderr)
	}
	write(pre, "v1.3.0-rc.1.md", "# v1.3.0-rc.1\n")
	if _, _, code := runNotesCheck(t, pre, "--tag", "v1.3.0-rc.1"); code != 1 {
		t.Fatalf("prerelease with bad notes: exit %d", code)
	}
	if _, stderr, code := runNotesCheck(t, t.TempDir()); code != 1 || !strings.Contains(stderr, "no release notes") {
		t.Fatalf("empty dir: %d %s", code, stderr)
	}
}

// The repository's own notes pass the check CI runs.
func TestRepositoryReleaseNotes(t *testing.T) {
	if _, stderr, code := runNotesCheck(t, filepath.Join(repoRoot(), "docs", "releases")); code != 0 {
		t.Fatalf("docs/releases: %s", stderr)
	}
}
