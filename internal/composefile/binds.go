package composefile

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// InAppDir reports whether src (an absolute local path, as compose-go
// resolves relative bind sources) lies inside appDir, and returns it
// relative to appDir ("." for appDir itself).
func InAppDir(appDir, src string) (string, bool) {
	if appDir == "" || !filepath.IsAbs(src) {
		return "", false
	}
	rel, err := filepath.Rel(filepath.Clean(appDir), filepath.Clean(src))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// looksLocal reports whether an absolute bind source outside the App
// directory looks like a path on the operator's machine rather than on the
// Server (macOS home and volume paths, or the operator's home directory).
func looksLocal(src string) bool {
	if strings.HasPrefix(src, "/Users/") || strings.HasPrefix(src, "/Volumes/") {
		return true
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == "/" || home == "/root" {
		return false
	}
	_, in := InAppDir(home, src)
	return in
}

// checkBinds applies the bind mount rules: read-only binds from the App
// directory are shipped with each Release (compose runtime), writable ones
// must be named volumes, and other sources are paths on the Server.
func checkBinds(svc types.ServiceConfig, appDir string, swarm bool, add func(level, f string, a ...any)) {
	for _, v := range svc.Volumes {
		if v.Type != types.VolumeTypeBind {
			continue
		}
		rel, in := InAppDir(appDir, v.Source)
		if !in {
			if looksLocal(v.Source) {
				add(LevelWarning, "bind mount source %s is outside the App directory and looks like a path on this machine; it is mounted from the Server, where Docker creates an empty directory if it is missing (move it into the App directory and mount it :ro to ship it)", v.Source)
			}
			continue
		}
		show := "./" + filepath.ToSlash(rel)
		switch {
		case swarm:
			add(LevelError, "bind mount %s:%s comes from the App directory; the swarm runtime does not ship files to nodes (bake it into the image or use a named volume)", show, v.Target)
		case !v.ReadOnly:
			add(LevelError, "writable bind mount %s:%s is inside the App directory; Yoho ships App files read-only per Release. Keep state in a named volume (volumes: [\"data:%s\"] plus top-level volumes: {data: {}}), or add :ro if the container only reads it", show, v.Target, v.Target)
		default:
			if _, err := os.Stat(v.Source); err != nil {
				add(LevelError, "bind mount source %s does not exist", show)
			}
		}
	}
}
