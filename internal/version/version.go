// Package version derives an App's version (the image tag and Release
// version) from git.
package version

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// UncommittedMarker separates the commit SHA from the content hash added
// when the working tree has uncommitted changes.
const UncommittedMarker = "_uncommitted_"

// FromGit returns the full HEAD commit SHA of the repository at dir. When the
// working tree is dirty (tracked changes or untracked, non-ignored files) it
// appends _uncommitted_<8 hex>, the first 8 hex characters of a SHA-256 over
// that uncommitted content. The same content yields the same version, and the
// suffix keeps the tag distinct from a clean build of that commit. Callers
// should warn (see IsUncommitted).
func FromGit(dir string) (string, error) {
	sha, err := git(dir, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", fmt.Errorf("cannot derive version from git in %s (%v); pass --version", dir, err)
	}
	status, err := git(dir, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("git status in %s: %w", dir, err)
	}
	if status == "" {
		return sha, nil
	}
	suffix, err := uncommittedSuffix(dir)
	if err != nil {
		return "", err
	}
	return sha + UncommittedMarker + suffix, nil
}

// IsUncommitted reports whether v was built from a dirty working tree.
func IsUncommitted(v string) bool { return strings.Contains(v, UncommittedMarker) }

// Valid reports whether v is usable as a Docker tag and Server path element.
func Valid(v string) bool {
	if v == "" || len(v) > 128 || v[0] == '.' || v[0] == '-' {
		return false
	}
	for _, r := range v {
		if !(r == '_' || r == '.' || r == '-' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')) {
			return false
		}
	}
	return true
}

// uncommittedSuffix is the first 8 hex of SHA-256(diff || untracked files).
// The diff is worktree vs HEAD, so staged and unstaged copies of the same
// content hash the same. Untracked files are path, an executable bit, and bytes.
//
// Everything runs from the repository top level so the diff, the untracked
// listing and the hashed paths are repo-wide and independent of which
// subdirectory dir is (the Yoho file often lives below the repo root).
func uncommittedSuffix(dir string) (string, error) {
	top, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", fmt.Errorf("git rev-parse --show-toplevel in %s: %w", dir, err)
	}
	diff, err := gitRaw(top,
		"diff", "HEAD",
		"--binary", "--no-color", "--no-ext-diff", "--no-textconv",
		"--src-prefix=a/", "--dst-prefix=b/",
	)
	if err != nil {
		return "", fmt.Errorf("git diff in %s: %w", dir, err)
	}
	listed, err := gitRaw(top, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return "", fmt.Errorf("git ls-files in %s: %w", dir, err)
	}
	paths := splitNUL(listed)
	sort.Strings(paths)

	sum := sha256.New()
	sum.Write(diff)
	for _, p := range paths {
		hashUntracked(sum, top, p)
	}
	return hex.EncodeToString(sum.Sum(nil)[:4]), nil
}

func splitNUL(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	parts := bytes.Split(b, []byte{0})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if len(p) == 0 {
			continue
		}
		out = append(out, string(p))
	}
	return out
}

// hashUntracked writes path NUL mode NUL content. Unreadable and non-regular
// files are skipped. Symlinks contribute their target, not the followed file.
func hashUntracked(h io.Writer, dir, rel string) {
	full := filepath.Join(dir, filepath.FromSlash(rel))
	fi, err := os.Lstat(full)
	if err != nil {
		return
	}
	mode := fi.Mode()
	var body []byte
	execBit := byte('0')
	switch {
	case mode&os.ModeSymlink != 0:
		target, err := os.Readlink(full)
		if err != nil {
			return
		}
		body = []byte(target)
	case mode.IsRegular():
		f, err := os.Open(full)
		if err != nil {
			return
		}
		body, err = io.ReadAll(f)
		f.Close()
		if err != nil {
			return
		}
		if mode&0o111 != 0 {
			execBit = '1'
		}
	default:
		return
	}
	h.Write([]byte(rel))
	h.Write([]byte{0, execBit, 0})
	h.Write(body)
}

func git(dir string, args ...string) (string, error) {
	out, err := gitRaw(dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitRaw(dir string, args ...string) ([]byte, error) {
	full := append([]string{"-C", dir, "--no-pager",
		"-c", "core.quotepath=off",
		"-c", "diff.noprefix=false",
		"-c", "diff.mnemonicPrefix=false",
		"-c", "core.autocrlf=false",
	}, args...)
	c := exec.Command("git", full...)
	c.Env = gitEnv()
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(msg)
	}
	return out.Bytes(), nil
}

func gitEnv() []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, e := range os.Environ() {
		k, _, _ := strings.Cut(e, "=")
		// Both change diff bytes without appearing in the command line.
		if k == "GIT_EXTERNAL_DIFF" || k == "GIT_DIFF_OPTS" {
			continue
		}
		env = append(env, e)
	}
	return append(env, "GIT_PAGER=cat", "PAGER=cat", "NO_COLOR=1", "GIT_CONFIG_NOSYSTEM=1")
}
