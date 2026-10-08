// Package config defines the Yoho file (yoho.yml / .toml / .json / .jsonc)
// and the per-Service `x-yoho` compose extension.
//
// All formats are decoded to a generic map first and then into these structs
// through encoding/json, so the json tags are the single source of truth for
// field names in every format and in the generated JSON Schema.
package config

// Config is the Yoho file: where and how an App is deployed.
type Config struct {
	Schema string `json:"$schema,omitempty" jsonschema:"description=Optional JSON Schema URL for editors"`

	// App name. Lowercase letters, digits and dashes. Used in compose project
	// names (yoho-<app>-<destination>) and Server paths.
	App string `json:"app" jsonschema:"required,pattern=^[a-z0-9][a-z0-9-]*$"`

	// Compose files relative to the Yoho file. Default: compose.yaml, then
	// compose.yml, docker-compose.yaml, docker-compose.yml.
	Compose []string `json:"compose,omitempty"`

	// Servers by name.
	Servers map[string]Server `json:"servers" jsonschema:"required"`

	// Destinations by name, e.g. production, staging.
	Destinations map[string]Destination `json:"destinations" jsonschema:"required"`

	Builder   Builder       `json:"builder,omitempty"`
	Transport Transport     `json:"transport,omitempty"`
	Registry  *Registry     `json:"registry,omitempty"`
	Secrets   SecretsConfig `json:"secrets,omitempty"`
	Proxy     ProxyConfig   `json:"proxy,omitempty"`
	Backups   BackupsConfig `json:"backups,omitempty"`
	Setup     SetupConfig   `json:"setup,omitempty"`
	Hooks     HooksConfig   `json:"hooks,omitempty"`

	// Number of Releases kept per Server for rollback. Default 5.
	RetainReleases int `json:"retain_releases,omitempty" jsonschema:"minimum=1"`
}

// Server is a machine reachable over SSH.
type Server struct {
	// SSH target: [user@]host[:port]. Honors ~/.ssh/config, agent, ProxyJump.
	SSH string `json:"ssh" jsonschema:"required"`
	// Use sudo -n for privileged steps (setup, schedule install).
	Sudo bool `json:"sudo,omitempty"`
	// Absolute base directory for Yoho state on this Server. Default
	// /var/lib/yoho. Use a home directory path when sudo is unavailable.
	Root string `json:"root,omitempty"`
	// Private address reachable from other Servers (Tailscale, VPC). Reserved for Roles.
	PrivateAddress string `json:"private_address,omitempty"`
	// Arbitrary labels, reserved for Roles / Swarm placement.
	Labels map[string]string `json:"labels,omitempty"`
}

// Destination is a named environment of an App.
type Destination struct {
	// Servers running this Destination. v1 requires exactly one for the
	// compose runtime; swarm may list several (first is the manager).
	Servers []string `json:"servers" jsonschema:"required,minItems=1"`
	// Runtime: compose (default) or swarm. Switching requires downtime.
	Runtime string `json:"runtime,omitempty" jsonschema:"enum=compose,enum=swarm"`
	// Extra non-secret environment for compose interpolation (image tags, ports).
	Env map[string]string `json:"env,omitempty"`
	// Overrides for this Destination's Proxy.
	Proxy *ProxyConfig `json:"proxy,omitempty"`
}

// Builder controls where images are built.
type Builder struct {
	// local (default), remote (dedicated build Server via ssh), or server
	// (build on the Destination's Server from synced source).
	Location string `json:"location,omitempty" jsonschema:"enum=local,enum=remote,enum=server"`
	// Local build engine: auto (default; Apple container on Apple silicon
	// Macs when installed, else Docker), docker, or container (apple/container).
	Engine string `json:"engine,omitempty" jsonschema:"enum=auto,enum=docker,enum=container"`
	// For location=remote: ssh target of the build machine.
	Remote string `json:"remote,omitempty"`
	// Target platforms, e.g. [linux/amd64]. Default: the Server's arch.
	Platforms []string `json:"platforms,omitempty"`
	// Secret keys passed to buildx as --secret id=KEY,env=KEY.
	Secrets []string `json:"secrets,omitempty"`
	// Paths excluded when syncing source for location=server (beyond .git).
	Exclude []string `json:"exclude,omitempty"`
}

// Transport controls how images reach Servers.
type Transport struct {
	// auto (default): pussh, then save|load, then registry.
	Mode string `json:"mode,omitempty" jsonschema:"enum=auto,enum=pussh,enum=load,enum=registry"`
}

// Registry is an optional image registry.
type Registry struct {
	Server   string `json:"server"`
	Username string `json:"username,omitempty"`
	// Secret key holding the password.
	PasswordSecret string `json:"password_secret,omitempty"`
	// Repository prefix, e.g. ghcr.io/me. Images become <prefix>/<app>-<service>.
	Prefix string `json:"prefix,omitempty"`
}

// SecretsConfig declares password-manager references in addition to the
// Kamal-style .yoho/secrets dotenv files.
type SecretsConfig struct {
	// Named providers.
	Providers map[string]SecretProvider `json:"providers,omitempty"`
	// Secret key -> reference resolved through a provider.
	Values map[string]SecretValue `json:"values,omitempty"`
}

// SecretProvider is a password manager or command.
type SecretProvider struct {
	// op (1Password CLI), bw (Bitwarden CLI), bws (Bitwarden Secrets Manager), command.
	Type string `json:"type" jsonschema:"required,enum=op,enum=bw,enum=bws,enum=command"`
	// For type=command: argv; the reference is appended as last argument.
	Command []string `json:"command,omitempty"`
	// Optional account / profile flag passed to the CLI.
	Account string `json:"account,omitempty"`
}

// SecretValue binds a secret key to a provider reference.
type SecretValue struct {
	Provider string `json:"provider" jsonschema:"required"`
	// op://vault/item/field, bw item id or name, bws secret id, or command arg.
	Ref string `json:"ref" jsonschema:"required"`
	// Optional: only for these Destinations.
	Destinations []string `json:"destinations,omitempty"`
}

// ProxyConfig configures the shared kamal-proxy on a Server.
type ProxyConfig struct {
	// Image, default basecamp/kamal-proxy:v0.10.2 (pinned).
	Image string `json:"image,omitempty"`
	// Published host ports. Default 80/443. 0 disables publishing that port
	// (e.g. behind Cloudflare Tunnel only).
	HTTPPort  *int `json:"http_port,omitempty"`
	HTTPSPort *int `json:"https_port,omitempty"`
	// Bind address for published ports, e.g. 127.0.0.1. Default all.
	Bind string `json:"bind,omitempty"`
	// Cloudflare Tunnel in front of the Proxy, managed by Yoho as the
	// yoho-tunnel container. Without token_secret a Quick Tunnel
	// (random *.trycloudflare.com URL, no account) is started.
	Tunnel *TunnelConfig `json:"tunnel,omitempty"`
}

// TunnelConfig configures the managed cloudflared connector.
type TunnelConfig struct {
	// Secret key holding the tunnel token (remotely managed tunnel). Empty: Quick Tunnel.
	TokenSecret string `json:"token_secret,omitempty"`
	// cloudflared image. Default cloudflare/cloudflared pinned by Yoho.
	Image string `json:"image,omitempty"`
	// Connector replicas for zero-downtime cloudflared upgrades. Default 1 (Quick Tunnels support 1).
	Replicas int `json:"replicas,omitempty" jsonschema:"minimum=1"`
}

// BackupsConfig defines Backup Targets and Backup jobs.
type BackupsConfig struct {
	Targets map[string]BackupTarget `json:"targets,omitempty"`
	Jobs    map[string]BackupJob    `json:"jobs,omitempty"`
}

// BackupTarget is where Backups are stored.
type BackupTarget struct {
	// restic (default; repository may be s3:, b2:, sftp:, rclone:, or a path)
	// or archive (tar.gz / zip / 7z files copied to a path or rclone remote).
	Type string `json:"type,omitempty" jsonschema:"enum=restic,enum=archive"`
	// restic repository string, or for archive: local dir or rclone remote:path.
	Repository string `json:"repository" jsonschema:"required"`
	// For archive: tar.gz (default), zip (AES-256), 7z (AES-256).
	Format string `json:"format,omitempty" jsonschema:"enum=tar.gz,enum=zip,enum=7z"`
	// Secret key holding the encryption password (restic repo password or archive password).
	PasswordSecret string `json:"password_secret,omitempty"`
	// Secret keys exported as env for the target (AWS_ACCESS_KEY_ID, B2_ACCOUNT_KEY, RCLONE_CONFIG_*...).
	EnvSecrets []string `json:"env_secrets,omitempty"`
	// Retention: keep last N (archive) or restic forget --keep-last N.
	KeepLast int `json:"keep_last,omitempty"`
}

// BackupJob is a scheduled or on-demand Backup of an App's Services.
type BackupJob struct {
	Destination string `json:"destination" jsonschema:"required"`
	// Services to back up; default all Services with x-yoho.backup.
	Services []string `json:"services,omitempty"`
	Target   string   `json:"target" jsonschema:"required"`
	// systemd OnCalendar expression, e.g. "*-*-* 03:00:00". Empty: on demand only.
	Schedule string `json:"schedule,omitempty"`
}

// SetupConfig drives `yoho setup` (Server provisioning).
type SetupConfig struct {
	// Deploy user created on the Server. Default: yoho.
	User string `json:"user,omitempty"`
	// Public keys authorized for the deploy user. Default: ~/.ssh/*.pub of the operator.
	AuthorizedKeys []string `json:"authorized_keys,omitempty"`
	// Extra apt packages.
	Packages []string `json:"packages,omitempty"`
	// Firewall (ufw). Default enabled with SSH + Proxy ports.
	Firewall *bool `json:"firewall,omitempty"`
	// Extra TCP ports to allow.
	AllowPorts []int `json:"allow_ports,omitempty"`
	// unattended-upgrades. Default true.
	AutoUpdates *bool `json:"auto_updates,omitempty"`
	// Swap size, e.g. 2G. Empty: no change.
	Swap string `json:"swap,omitempty"`
	// Timezone, e.g. UTC.
	Timezone string `json:"timezone,omitempty"`
}

// HooksConfig locates Hooks. Default directory .yoho/hooks.
type HooksConfig struct {
	Path string `json:"path,omitempty"`
}

// ServiceExt is the `x-yoho` extension on a compose Service.
type ServiceExt struct {
	// Route this Service through the Proxy for zero-downtime cutover.
	Proxy *ServiceProxy `json:"proxy,omitempty"`
	// Stateful Services own data, are pinned to one Server, and are
	// recreated (stop-first), never scaled.
	Stateful bool `json:"stateful,omitempty"`
	// Secrets this Service receives. Map form {CONTAINER_NAME: SECRET_KEY}
	// or list form ["KEY", "CONTAINER_NAME:SECRET_KEY"]. Decoded by SecretRefs.
	Secrets SecretRefs `json:"secrets,omitempty"`
	// Deliver secrets as env (env_file) instead of files. Default files.
	SecretsAsEnv bool `json:"secrets_as_env,omitempty"`
	// Secrets generated once on the Server: {NAME: kind}, kind one of
	// password32, password64, hex32, hex64, base64_32, base64_64.
	Generate map[string]string `json:"generate,omitempty"`
	// Command run in a one-off container from the new image before cutover
	// (e.g. migrations). Failure aborts the deploy.
	ReleaseCommand []string       `json:"release_command,omitempty"`
	Backup         *ServiceBackup `json:"backup,omitempty"`
	// Swarm only: route the Proxy to individual tasks (dnsrr) for strict draining.
	StrictDrain bool `json:"strict_drain,omitempty"`
	// Reserved for Roles: address other Servers use to reach this Service.
	Endpoint string `json:"endpoint,omitempty"`
}

// ServiceProxy configures how the Proxy routes to a Service.
type ServiceProxy struct {
	// Hostnames routed to this Service. Empty: catch-all.
	Hosts []string `json:"hosts,omitempty"`
	// Container port. Default 80.
	Port int `json:"port,omitempty"`
	// Health check path. Default /up.
	HealthPath string `json:"health_path,omitempty"`
	// Terminate TLS with Let's Encrypt in the Proxy. Off behind Cloudflare Tunnel.
	TLS bool `json:"tls,omitempty"`
	// Seconds to wait for the new container to become healthy. Default 30.
	DeployTimeout int `json:"deploy_timeout,omitempty"`
	// Seconds to drain the old container. Default 30.
	DrainTimeout int `json:"drain_timeout,omitempty"`
}

// ServiceBackup defines what a Backup copies for a Service.
type ServiceBackup struct {
	// Named volumes (compose keys) to archive.
	Volumes []string `json:"volumes,omitempty"`
	// Command run inside the container whose stdout is a dump, e.g.
	// ["pg_dump", "-U", "app", "app"].
	Dump []string `json:"dump,omitempty"`
	// Command run inside the container before copying volumes. When absent
	// and Volumes are set, the container is paused during the copy.
	PreBackup []string `json:"pre_backup,omitempty"`
	// Command run inside the container after a restore.
	PostRestore []string `json:"post_restore,omitempty"`
	// Command run inside the container with the dump on stdin to restore it,
	// e.g. ["psql", "-U", "app", "app"]. Empty: dumps are kept on the Server.
	RestoreDump []string `json:"restore_dump,omitempty"`
}

// SecretRef maps a name seen by the container to a secret key.
type SecretRef struct {
	Name string // inside the container
	Key  string // in .yoho/secrets / providers
}

// SecretRefs accepts map or list form; see ServiceExt.Secrets.
type SecretRefs []SecretRef
