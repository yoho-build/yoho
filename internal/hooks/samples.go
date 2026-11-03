package hooks

import (
	"fmt"
	"strings"
)

// when says when each Hook runs (docs/site/hooks.md has the full table).
var when = map[string]string{
	"docker-setup":      "(reserved for `yoho setup`; not run yet)",
	"pre-connect":       "before Yoho connects to the Servers (deploy, rollback, proxy boot, secrets rotate)",
	"pre-build":         "before building images (deploy)",
	"pre-deploy":        "after the Destination lock is taken, before the Proxy boot and any container change (deploy, rollback, secrets rotate)",
	"post-deploy":       "last, after post-app-boot (deploy, rollback, secrets rotate); YOHO_RUNTIME is set",
	"pre-app-boot":      "after pre-deploy and the Proxy boot, before any new container starts (dependencies, release commands, Services)",
	"post-app-boot":     "after the new containers are healthy and routed and the Release is recorded, before post-deploy",
	"pre-proxy-reboot":  "before `yoho proxy boot` boots the Proxy on a Server (every time), and before a deploy or rollback recreates an existing Proxy whose configuration changed",
	"post-proxy-reboot": "after the Proxy was (re)booted on a Server",
	"pre-backup":        "before a Backup (YOHO_BACKUP_JOB, YOHO_BACKUP_TARGET)",
	"post-backup":       "after a successful Backup (adds YOHO_BACKUP_ID)",
	"pre-restore":       "before a restore (YOHO_BACKUP_JOB, YOHO_BACKUP_TARGET, YOHO_BACKUP_ID)",
	"post-restore":      "after the data is restored, with the consumers still stopped",
	"pre-cutover":       "after the migration cutover preflight, before the old containers stop (YOHO_CUTOVER)",
	"post-cutover":      "after the migration cutover is done (YOHO_CUTOVER)",
}

// Sample returns the `<name>.sample` script `yoho init` writes: a POSIX sh
// script documenting when the Hook runs and its env, that only logs.
func Sample(name string) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	fmt.Fprintf(&b, "# Yoho %s Hook sample. Rename to %s (drop .sample) to enable it.\n", name, name)
	fmt.Fprintf(&b, "# Runs on your machine %s.\n", when[name])
	b.WriteString("# A non-zero exit aborts the command.\n#\n# Env:\n")
	b.WriteString("#   YOHO_APP, YOHO_DESTINATION, YOHO_VERSION, YOHO_SERVICE_VERSION (<app>@<version>)\n")
	b.WriteString("#   YOHO_HOSTS        comma-separated Servers as host[:port] (no ssh user)\n")
	b.WriteString("#   YOHO_COMMAND      deploy, rollback, proxy, backup, secrets rotate, ...\n")
	b.WriteString("#   YOHO_SUBCOMMAND   e.g. boot for `yoho proxy boot`, run/restore for backups\n")
	b.WriteString("#   YOHO_PERFORMER    who runs Yoho; YOHO_RECORDED_AT (UTC RFC3339)\n")
	b.WriteString("#   YOHO_LOCK         true while this command holds the Destination lock, else false\n")
	b.WriteString("#   YOHO_RUNTIME      seconds (post-deploy only); YOHO_CUTOVER (pre/post-cutover)\n")
	b.WriteString("# Secrets are never passed to Hooks.\n\n")
	b.WriteString("set -eu\n")
	fmt.Fprintf(&b, "echo \"%s: ${YOHO_COMMAND:-} ${YOHO_SERVICE_VERSION:-} on ${YOHO_HOSTS:-no Servers yet} (lock=${YOHO_LOCK:-false})\"\n", name)
	return b.String()
}
