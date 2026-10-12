// Package skills embeds the Yoho agent skills so `yoho skill install` can
// write them into a project. Every subdirectory with a SKILL.md is a skill.
package skills

import "embed"

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
