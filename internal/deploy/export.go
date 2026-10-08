package deploy

import (
	"context"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Thin exports shared with the swarm runtime, which uses the same secret
// generation, audit and Release bookkeeping as the compose runtime.

// ValidVersion reports whether v is usable as a Release directory name.
func ValidVersion(v string) bool { return versionRe.MatchString(v) }

// EnsureHMACKey returns the per-Destination audit key in dir, creating it once.
func EnsureHMACKey(ctx context.Context, h remote.Host, dir string) ([]byte, error) {
	return ensureHMACKey(ctx, h, dir)
}

// GeneratedDecls collects x-yoho.generate across Services: key -> kind.
func GeneratedDecls(d *plan.Deploy) (map[string]string, error) { return generatedDecls(d) }

// EnsureGenerated creates missing generated secrets under dir/generated and
// returns all their values; existing values are never replaced.
func EnsureGenerated(ctx context.Context, h remote.Host, dir string, kinds map[string]string) (map[string]string, error) {
	return ensureGenerated(ctx, h, dir, kinds)
}

// ServiceSecrets merges locally resolved and generated secrets per Service:
// Service -> container name -> value.
func ServiceSecrets(d *plan.Deploy, generated map[string]string, warn func(string, ...any)) (map[string]map[string]string, error) {
	return serviceSecrets(d, generated, warn)
}

// SecretAudit returns the Release audit records ("<service>/<NAME>").
func SecretAudit(d *plan.Deploy, svcSecrets map[string]map[string]string, generated map[string]string, key []byte) map[string]release.SecretAudit {
	return secretAudit(d, svcSecrets, generated, key)
}

// EnvFile renders values as a compose/stack env_file without interpolation.
func EnvFile(m map[string]string) []byte { return envFile(m) }

// EscapeDollars rewrites `$` as `$$` in every YAML scalar so a second
// interpolation (docker compose, docker stack deploy) keeps them literal.
func EscapeDollars(src []byte) ([]byte, error) { return escapeDollars(src) }

// ReadRelease reads a release.json.
func ReadRelease(ctx context.Context, h remote.Host, p string) (*release.Release, error) {
	return readRelease(ctx, h, p)
}

// ListReleases lists Releases of an App Destination on h, newest first.
func ListReleases(ctx context.Context, h remote.Host, app, dest string) ([]release.Release, error) {
	return listReleases(ctx, h, app, dest)
}

// WriteJSON writes v as indented JSON (0600).
func WriteJSON(ctx context.Context, h remote.Host, p string, v any) error {
	return writeJSON(ctx, h, p, v)
}

// SHA256Hex is the hex SHA-256 of b.
func SHA256Hex(b []byte) string { return sha256Hex(b) }
