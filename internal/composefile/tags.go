package composefile

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/yoho-build/yoho/internal/config"
)

var tagEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// CheckTags validates x-yoho.tags against the Yoho file's Servers and the
// Destination's runtime and access (ADR 0020).
func CheckTags(r *Result, servers map[string]config.Server, d config.Destination) []Finding {
	var out []Finding
	add := func(svc, f string, args ...any) {
		out = append(out, Finding{Level: LevelError, Service: svc, Message: fmt.Sprintf(f, args...)})
	}
	known := config.TagSelectors(servers)
	for _, name := range r.Project.ServiceNames() {
		tags := r.Ext[name].Tags
		if len(tags) == 0 {
			continue
		}
		if d.Runtime == "swarm" {
			add(name, "x-yoho.tags supports the compose runtime only; set the environment in the Compose file instead")
		}
		if d.Access == "scoped" {
			add(name, "x-yoho.tags requires operator access; the Gate refuses per-Server environment")
		}
		for _, sel := range sortedMapKeys(tags) {
			if !slices.Contains(known, sel) {
				add(name, "x-yoho.tags.%s matches no Server name or tag. Hint: known names and tags: %s", sel, strings.Join(known, ", "))
			}
			for _, k := range sortedMapKeys(tags[sel].Env) {
				if !tagEnvKey.MatchString(k) {
					add(name, "x-yoho.tags.%s.env key %q must match %s", sel, k, tagEnvKey)
				} else if strings.HasPrefix(k, "YOHO_") {
					add(name, "x-yoho.tags.%s.env key %s uses the reserved YOHO_ prefix", sel, k)
				}
			}
		}
	}
	return out
}
