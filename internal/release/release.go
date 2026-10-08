// Package release defines the Release record and the on-Server layout.
//
// Layout (owned by the deploy user, dirs 0700):
//
//	<root>/apps/<app>/<destination>/
//	  lock/                         deploy/backup lock (mkdir-based), lock/info.json
//	  releases/<version>/compose.yaml   compiled, contains no secret values
//	  releases/<version>/release.json   this Release record
//	  current -> releases/<version>
//	  secrets/<generation>/<NAME>   0444 files inside 0700 dirs (bind-mounted)
//	  secrets/<generation>/<service>.env  env_file fallback (0600)
//	  generated/<NAME>              secrets generated once on the Server (0600)
//	  hmac.key                      audit fingerprint key (0600)
//	  source/                       synced build context (builder.location=server)
//	<root>/backups/<app>/<destination>/   local Backup spool (0700)
//	<root>/jobs/<app>-<destination>-<job>.json   Scheduled Job specs
//
// <root> is Root, /var/lib/yoho by default.
package release

import (
	"path"
	"time"
)

// Root is the base directory on the Server being operated on. Configurable
// per Server (config.Server.Root) because Servers without passwordless sudo
// cannot use /var/lib; the CLI sets it before operating on a Server.
// All Servers of one Destination must share the same Root.
var Root = DefaultRoot

// DefaultRoot is used when config.Server.Root is empty.
const DefaultRoot = "/var/lib/yoho"

// OwnershipError reports a name (compose project, Proxy route, Scheduled Job
// unit) that another App or Destination already uses on the Server. The names
// join App and Destination with '-', so App foo-bar / prod and App foo /
// bar-prod collide.
type OwnershipError struct{ Msg string }

func (e *OwnershipError) Error() string { return e.Msg }

// AppDir is the per-App, per-Destination directory on a Server.
func AppDir(app, destination string) string {
	return path.Join(Root, "apps", app, destination)
}

// Dir is a Release directory.
func Dir(app, destination, version string) string {
	return path.Join(AppDir(app, destination), "releases", version)
}

// SecretsDir is a secrets generation directory.
func SecretsDir(app, destination, generation string) string {
	return path.Join(AppDir(app, destination), "secrets", generation)
}

// BackupDir is the local Backup spool.
func BackupDir(app, destination string) string {
	return path.Join(Root, "backups", app, destination)
}

// JobsDir holds Scheduled Job specs.
func JobsDir() string { return path.Join(Root, "jobs") }

// ProjectName is the compose project / swarm stack name.
func ProjectName(app, destination string) string {
	return "yoho-" + app + "-" + destination
}

// Release records one deploy of an App to a Server.
type Release struct {
	App         string    `json:"app"`
	Destination string    `json:"destination"`
	Server      string    `json:"server"`
	Version     string    `json:"version"`
	Runtime     string    `json:"runtime"` // compose | swarm
	Role        string    `json:"role"`    // "all" until Roles exist
	DeployedAt  time.Time `json:"deployed_at"`
	Performer   string    `json:"performer"`
	// Service -> image reference actually deployed (tag@digest when known).
	Images map[string]string `json:"images"`
	// Service -> local image ID on the Server when the Release was
	// deployed. Tags are mutable (one Version built per Destination shares
	// a tag; upstream tags move), so rollback re-points Yoho-built tags at
	// these IDs and image pruning keeps them.
	ImageIDs map[string]string `json:"image_ids,omitempty"`
	// Services whose image Yoho built for this Release (possibly none: an
	// empty list, not absent). Only those are re-tagged on rollback. Absent
	// in records from before this field; rollback then goes by image name.
	Built []string `json:"built"`
	// Secrets generation directory name used by this Release.
	SecretsGeneration string `json:"secrets_generation"`
	// Secret name -> fingerprint (HMAC-SHA256 truncated, keyed per Destination)
	// plus provider reference. Never the value.
	Secrets map[string]SecretAudit `json:"secrets,omitempty"`
	// Content hashes of the App files (files/<hash>) this Release mounts.
	Files []string `json:"files,omitempty"`
	// SHA-256 of the compiled compose file.
	ComposeSHA256 string `json:"compose_sha256"`
	Status        string `json:"status"` // deployed | failed | rolled_back
}

// SecretAudit is the audit record of one secret in a Release.
type SecretAudit struct {
	Ref         string `json:"ref,omitempty"`
	Fingerprint string `json:"fingerprint"`
}
