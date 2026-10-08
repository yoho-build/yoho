// Package version derives an App's version (the image tag and Release
// version) from git.
package version

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// UncommittedMarker separates the commit SHA from the random suffix added
// when the working tree has uncommitted changes.
const UncommittedMarker = "_uncommitted_"

// FromGit returns the full HEAD commit SHA of the repository at dir. When the
// working tree is dirty (including untracked files) it appends
// _uncommitted_<8 random hex> so the image tag never collides with a clean
// build of the same commit; callers should warn (see IsUncommitted).
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
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return sha + UncommittedMarker + hex.EncodeToString(b), nil
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

func git(dir string, args ...string) (string, error) {
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", errors.New(msg)
	}
	return strings.TrimSpace(out.String()), nil
}
