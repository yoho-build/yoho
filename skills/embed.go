// Package skills embeds the Yoho agent skill so `yoho skill install` can
// write it into a project.
package skills

import "embed"

// FS holds the skill under yoho/ (SKILL.md and references/).
//
//go:embed yoho/*
var FS embed.FS
