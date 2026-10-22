// Package skills embeds the Yoho agent skills so `yoho skill install` can
// write them into a project. Every subdirectory with a SKILL.md is a skill.
package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
)

// FS holds one directory per skill (SKILL.md and optional references/).
// The pattern also embeds this package's Go files; callers use Names to
// list only directories that contain a SKILL.md.
//
//go:embed *
var FS embed.FS

// Names returns the embedded skill names (directories with a SKILL.md), sorted.
func Names() []string {
	entries, err := FS.ReadDir(".")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := FS.ReadFile(e.Name() + "/SKILL.md"); err == nil {
			names = append(names, e.Name())
		}
	}
	return names
}

// Describe returns the description from the YAML frontmatter of the skill's
// SKILL.md. The description must be a single-line scalar.
func Describe(name string) (string, error) {
	b, err := FS.ReadFile(name + "/SKILL.md")
	if err != nil {
		return "", fmt.Errorf("unknown skill %q", name)
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", fmt.Errorf("skill %s: SKILL.md has no frontmatter", name)
	}
	for _, l := range lines[1:] {
		if strings.TrimSpace(l) == "---" {
			break
		}
		if v, ok := strings.CutPrefix(l, "description:"); ok {
			v = strings.TrimSpace(v)
			if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
				v = v[1 : len(v)-1]
			}
			if v != "" {
				return v, nil
			}
		}
	}
	return "", fmt.Errorf("skill %s: SKILL.md frontmatter has no description", name)
}

// FileCount returns the number of files in the skill.
func FileCount(name string) int {
	n := 0
	_ = fs.WalkDir(FS, name, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}
