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
	// Absolute directory on the Server holding this Destination's App
	// directory (releases, secrets, generated secrets, hmac.key), for example
	// /home/deb/apps/elpoca-engine. Default: <root>/apps/<app>/<destination>.
	// When set, that canonical path becomes a symlink to app_dir; compose
	// project, volume, route and job names do not change. app_dir must be
	// outside root, not exist yet or be empty, and Yoho owns it completely.
	// Changing it moves the directory (same filesystem only) on the next
	// deploy or apply.
	AppDir string `json:"app_dir,omitempty"`
	// Access: operator (default) runs every command over full SSH. scoped
	// reaches this one App through the Gate on the Server (`yoho access`):
	// deploy, rollback, releases, app ps/logs/stats, tui and ui only, no
	// shell, no Docker, no secret values. YOHO_ACCESS overrides it.
	Access string `json:"access,omitempty" jsonschema:"enum=operator,enum=scoped"`
	// Unix user of the Gate on the Server for access: scoped. Default yoho-gate.
	GateUser string `json:"gate_user,omitempty" jsonschema:"pattern=^[a-z_][a-z0-9_-]{0,31}$"`
	// How the CLI reaches the Destination's Server when it has no direct
	// SSH address, e.g. through a Cloudflare Tunnel. Applies to the operator
	// and the scoped path.
	SSH *DestinationSSH `json:"ssh,omitempty"`
	// IPv4 subnet (CIDR, e.g. 10.200.3.0/24) for the App's compose default
	// network, for Servers whose Docker address pools are exhausted. Applied
	// only when the compose file defines no ipam for that network. Compose
	// runtime only. Changing it on a deployed App is refused (Docker would
	// recreate the network), and Yoho never picks it by itself; a failed
	// deploy suggests a free one.
	Subnet string `json:"subnet,omitempty" jsonschema:"example=10.200.3.0/24"`
	// One-shot cutover from another platform (Coolify, Dokploy, plain
	// compose) run by deploy and apply on this Destination only: stop the
	// old containers, prove the claimed data is free, deploy, then repoint
	// Cloudflare Tunnel ingress. Recorded in <app dir>/cutover.json; undo
	// with `yoho rollback --cutover`. Compose runtime and operator access only.
	Migrate *Migrate `json:"migrate,omitempty"`
}

// Migrate is a Destination's one-shot cutover from another platform (ADR 0012).
type Migrate struct {
	// Platform the App comes from: coolify, dokploy or compose. Recorded
	// in the cutover journal; selectors in stop are always explicit.
	From string `json:"from,omitempty" jsonschema:"enum=coolify,enum=dokploy,enum=compose"`
	// Old containers to stop (never remove), matched on the Server.
	Stop MigrateStop `json:"stop" jsonschema:"required"`
	// Data the App reuses: no running container may mount it once stop ran.
	Claim MigrateClaim `json:"claim,omitempty"`
	// Cloudflare Tunnel ingress to repoint to the Proxy after the deploy.
	// Requires proxy.tunnel.token_secret (the tunnel's account and ID).
	Tunnel *MigrateTunnel `json:"tunnel,omitempty"`
	// What to do when a step after stopping fails: restart_old (default)
	// stops this App's Services, starts the old containers again and
	// restores the saved ingress; leave keeps the state for
	// `yoho rollback --cutover` or `yoho deploy --cutover-resume`.
	OnFailure string `json:"on_failure,omitempty" jsonschema:"enum=restart_old,enum=leave"`
}

// MigrateStop selects old containers. A running container matching any
// selector is stopped.
type MigrateStop struct {
	// Compose project of the old stack (label com.docker.compose.project).
	ComposeProject string `json:"compose_project,omitempty"`
	// Docker label filters, key or key=value, e.g. coolify.applicationId=ab12.
	Labels []string `json:"labels,omitempty"`
	// Container names.
	Containers []string `json:"containers,omitempty"`
}

// MigrateClaim is data the App takes over from the old stack.
type MigrateClaim struct {
	// Absolute host paths (bind mounts). A mount of the path, inside it or
	// above it counts.
	Paths []string `json:"paths,omitempty"`
	// Named Docker volumes.
	Volumes []string `json:"volumes,omitempty"`
}

// MigrateTunnel repoints Cloudflare Tunnel ingress rules.
type MigrateTunnel struct {
	// Secret key holding a Cloudflare API token with Cloudflare Tunnel edit
	// permission. Resolved locally, sent only to the Cloudflare API.
	APITokenSecret string `json:"api_token_secret" jsonschema:"required"`
	// Hostnames whose ingress rules get the new service; other rules,
	// including the final catch-all, are kept unchanged.
	Hostnames []string `json:"hostnames" jsonschema:"required,minItems=1"`
	// Service for those rules. Default http://yoho-proxy:80.
	Service string `json:"service,omitempty"`
}

// DefaultMigrateService is where repointed ingress rules send traffic.
const DefaultMigrateService = "http://yoho-proxy:80"

// FailurePolicy returns on_failure with its default.
func (m Migrate) FailurePolicy() string {
	if m.OnFailure == "" {
		return "restart_old"
	}
	return m.OnFailure
}

// TargetService returns service with its default.
func (t MigrateTunnel) TargetService() string {
	if t.Service == "" {
		return DefaultMigrateService
	}
	return t.Service
}

// DestinationSSH routes SSH to a Destination's Server.
type DestinationSSH struct {
	// cloudflare: ssh runs `cloudflared access ssh --hostname HOSTNAME` as
	// its ProxyCommand (a Cloudflare Tunnel ssh:// ingress to the Server's
	// sshd). In CI cloudflared reads TUNNEL_SERVICE_TOKEN_ID and
	// TUNNEL_SERVICE_TOKEN_SECRET.
	Via string `json:"via" jsonschema:"required,enum=cloudflare"`
	// Public hostname of the tunnel's ssh:// ingress, e.g. ssh.example.com.
	Hostname string `json:"hostname" jsonschema:"required"`
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
	// For location=remote: ship images from the Builder straight to the
	// Servers instead of through this machine. auto (default) does so when
	// the Builder can ssh to every Server without a prompt, else falls back;
	// true requires it; false always ships from this machine.
	Direct BuilderDirect `json:"direct,omitempty"`
	// Target platforms, e.g. [linux/amd64]. Default: the Server's arch.
	Platforms []string `json:"platforms,omitempty"`
	// Secret keys passed to buildx as --secret id=KEY,env=KEY.
	Secrets []string `json:"secrets,omitempty"`
	// For location=server: extra rsync exclude patterns, relative to the
	// transfer root (the Yoho file's directory unless build inputs lie
	// outside it). .git and .yoho/secrets* never ship.
	Exclude []string `json:"exclude,omitempty"`
	// For location=server: skip git-ignored paths when syncing source.
	// Default true; false ships them (generated code, vendored artifacts).
	Gitignore *bool `json:"gitignore,omitempty"`
}

// HonorGitignore reports whether server builds skip git-ignored paths.
func (b Builder) HonorGitignore() bool { return b.Gitignore == nil || *b.Gitignore }

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

// SecretsConfig declares password-manager references and optional local value
// requirements in addition to the Kamal-style .yoho/secrets dotenv files.
type SecretsConfig struct {
	// Source keys that must resolve locally to a value other than exactly "".
	// Opt-in; whitespace and multiline values are valid. Server-generated
	// keys cannot be declared here. Container aliases are not source keys.
	Nonempty []string `json:"nonempty,omitempty" jsonschema:"uniqueItems=true,pattern=^[A-Za-z_][A-Za-z0-9_]*$,description=Source secret keys required to resolve locally to a nonempty value; whitespace is valid; Server-generated keys are unsupported"`
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
	// Extra attempts for Scheduled Backups only. Default 0; at most 5.
	// Allowed only for volume-only Backups without dump/pre_backup commands.
	Retries int `json:"retries,omitempty" jsonschema:"minimum=0,maximum=5"`
	// Fixed delay between attempts in seconds; default 30, maximum 300.
	RetryDelaySec int `json:"retry_delay_sec,omitempty" jsonschema:"minimum=0,maximum=300"`
}

// SetupConfig drives `yoho setup` (Server provisioning).
type SetupConfig struct {
	// Deploy user created on the Server. Default: yoho.
	User string `json:"user,omitempty"`
	// Public keys authorized for the deploy user. Default (unset): ~/.ssh/*.pub of the operator. An empty list authorizes no keys.
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
	// Proxy HTTP probe port. Omitted: use the traffic target's port.
	HealthPort *int `json:"health_port,omitempty" jsonschema:"minimum=1,maximum=65535,description=Proxy HTTP probe port; omitted uses the traffic target port"`
	// Timeout for each Proxy HTTP probe (Go duration, e.g. 2s or 500ms).
	// Omitted: use the pinned Proxy default; not the deploy deadline or Docker healthcheck timeout.
	HealthTimeout *string `json:"health_timeout,omitempty" jsonschema:"minLength=1,description=Positive Go duration per Proxy HTTP probe (e.g. 750ms or 2s); omitted uses the pinned Proxy default; separate from deploy_timeout and Docker healthcheck"`
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
