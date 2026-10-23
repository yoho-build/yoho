package config

import (
	"fmt"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

var appPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// RolesMessage is shown when a compose Destination lists several Servers.
const RolesMessage = "Roles are not supported yet; use runtime: swarm for several Servers"

func sortedKeys[V any](m map[string]V) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}

func rootOrDefault(r string) string {
	if r == "" {
		return "/var/lib/yoho"
	}
	return r
}

// Validate checks cross references and enums. It returns all problems, in
// deterministic order, so users fix them in one pass.
func Validate(c *Config) []error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }

	if !appPattern.MatchString(c.App) {
		add("app %q must match %s", c.App, appPattern)
	}
	if len(c.Servers) == 0 {
		add("servers: at least one Server is required")
	}
	for _, n := range sortedKeys(c.Servers) {
		if c.Servers[n].SSH == "" {
			add("servers.%s.ssh is required", n)
		}
		if r := c.Servers[n].Root; r != "" && !strings.HasPrefix(r, "/") {
			add("servers.%s.root %q must be an absolute path", n, r)
		}
	}
	if len(c.Destinations) == 0 {
		add("destinations: at least one Destination is required")
	}
	for _, n := range sortedKeys(c.Destinations) {
		d := c.Destinations[n]
		rt := d.Runtime
		if rt != "" && rt != "compose" && rt != "swarm" {
			add("destinations.%s.runtime %q must be compose or swarm", n, rt)
		}
		if len(d.Servers) == 0 {
			add("destinations.%s.servers: at least one Server is required", n)
		}
		for _, s := range d.Servers {
			if _, ok := c.Servers[s]; !ok {
				add("destinations.%s.servers: unknown Server %q", n, s)
			}
		}
		roots := map[string]bool{}
		for _, s := range d.Servers {
			if sv, ok := c.Servers[s]; ok {
				roots[rootOrDefault(sv.Root)] = true
			}
		}
		if len(roots) > 1 {
			add("destinations.%s: all Servers must share the same root", n)
		}
		if (rt == "" || rt == "compose") && len(d.Servers) > 1 {
			add("destinations.%s: %s", n, RolesMessage)
		}
		if d.AppDir != "" {
			errs = append(errs, validateAppDir(c, n, d)...)
		}
		switch d.Access {
		case "", "operator":
		case "scoped":
			if rt == "swarm" {
				add("destinations.%s: access: scoped supports the compose runtime only", n)
			}
		default:
			add("destinations.%s.access %q must be operator or scoped", n, d.Access)
		}
		if d.Subnet != "" && rt == "swarm" {
			add("destinations.%s.subnet supports the compose runtime only; remove it or pin the subnet in the compose file's networks.default.ipam", n)
		}
		if d.Subnet != "" {
			if p, err := netip.ParsePrefix(d.Subnet); err != nil || !p.Addr().Is4() || p != p.Masked() {
				add("destinations.%s.subnet %q must be an IPv4 network in CIDR form, like 10.200.3.0/24", n, d.Subnet)
			}
		}
		if d.GateUser != "" && !unixUserPattern.MatchString(d.GateUser) {
			add("destinations.%s.gate_user %q must be a Unix user name", n, d.GateUser)
		}
		if d.Migrate != nil {
			errs = append(errs, validateMigrate(c, n, d)...)
		}
		if d.SSH != nil {
			if d.SSH.Via != "cloudflare" {
				add("destinations.%s.ssh.via %q must be cloudflare", n, d.SSH.Via)
			}
			if !hostnamePattern.MatchString(d.SSH.Hostname) || len(d.SSH.Hostname) > 253 {
				add("destinations.%s.ssh.hostname %q must be a DNS hostname such as ssh.example.com", n, d.SSH.Hostname)
			}
			if len(d.Servers) > 1 {
				add("destinations.%s.ssh: one hostname reaches one Server; the Destination lists %d", n, len(d.Servers))
			}
		}
	}
	errs = append(errs, validateAppDirOverlap(c)...)
	switch c.Builder.Location {
	case "", "local", "server":
	case "remote":
		if c.Builder.Remote == "" {
			add("builder.remote is required when builder.location is remote")
		}
	default:
		add("builder.location %q must be local, remote or server", c.Builder.Location)
	}
	switch c.Transport.Mode {
	case "", "auto", "pussh", "load", "registry":
	default:
		add("transport.mode %q must be auto, pussh, load or registry", c.Transport.Mode)
	}
	if c.Builder.Direct == DirectOn {
		switch {
		case c.Builder.Location != "remote":
			add("builder.direct: true needs builder.location remote")
		case len(c.Builder.Platforms) > 1:
			add("builder.direct: true ships one platform; builder.platforms lists %d", len(c.Builder.Platforms))
		case c.Transport.Mode == "pussh" || c.Transport.Mode == "registry":
			add("builder.direct: true streams images with docker save | docker load; transport.mode %s cannot apply", c.Transport.Mode)
		}
	}
	if c.Transport.Mode == "registry" && c.Registry == nil {
		add("transport.mode registry requires a registry section")
	}
	errs = append(errs, ValidateNonempty(c.Secrets.Nonempty)...)
	for _, n := range sortedKeys(c.Secrets.Values) {
		v := c.Secrets.Values[n]
		if _, ok := c.Secrets.Providers[v.Provider]; !ok {
			add("secrets.values.%s: unknown provider %q", n, v.Provider)
		}
		for _, d := range v.Destinations {
			if _, ok := c.Destinations[d]; !ok {
				add("secrets.values.%s.destinations: unknown Destination %q", n, d)
			}
		}
	}
	for _, n := range sortedKeys(c.Secrets.Providers) {
		p := c.Secrets.Providers[n]
		if p.Type == "command" && len(p.Command) == 0 {
			add("secrets.providers.%s.command is required for type command", n)
		}
	}
	for _, n := range sortedKeys(c.Backups.Targets) {
		t := c.Backups.Targets[n]
		if t.Repository == "" {
			add("backups.targets.%s.repository is required", n)
		}
		if t.Type != "" && t.Type != "restic" && t.Type != "archive" {
			add("backups.targets.%s.type %q must be restic or archive", n, t.Type)
		}
		if k := t.RcloneConfigSecret; k != "" {
			if !secretKeyPattern.MatchString(k) {
				add("backups.targets.%s.rclone_config_secret must be a secret key matching [A-Za-z_][A-Za-z0-9_]*", n)
			}
			if !t.UsesRclone() {
				add("backups.targets.%s.rclone_config_secret requires an rclone repository: type archive with remote:path, or restic rclone:remote:path", n)
			}
			if k == t.PasswordSecret || slices.Contains(t.EnvSecrets, k) {
				add("backups.targets.%s.rclone_config_secret %s is also used as password_secret or env_secrets", n, k)
			}
		}
	}
	for _, n := range sortedKeys(c.Backups.Jobs) {
		j := c.Backups.Jobs[n]
		if j.Retries < 0 || j.Retries > 5 {
			add("backups.jobs.%s.retries must be between 0 and 5", n)
		}
		if j.RetryDelaySec < 0 || j.RetryDelaySec > 300 {
			add("backups.jobs.%s.retry_delay_sec must be between 0 and 300 (0 means 30)", n)
		}
		if j.Retries > 0 && j.Schedule == "" {
			add("backups.jobs.%s.retries requires a schedule", n)
		}
		if _, ok := c.Destinations[j.Destination]; !ok {
			add("backups.jobs.%s.destination: unknown Destination %q", n, j.Destination)
		}
		if _, ok := c.Backups.Targets[j.Target]; !ok {
			add("backups.jobs.%s.target: unknown Backup Target %q", n, j.Target)
		}
	}
	if c.RetainReleases < 0 {
		add("retain_releases must be at least 1")
	}
	return errs
}

// ValidateNonempty checks the opt-in source-key declaration without reading
// values or running providers. Invalid entries are identified by position,
// never echoed (an accidental value is not safe diagnostic text).
func ValidateNonempty(keys []string) []error {
	var errs []error
	seen := map[string]bool{}
	for i, key := range keys {
		if !secretKeyPattern.MatchString(key) {
			errs = append(errs, fmt.Errorf("secrets.nonempty[%d]: source key must match [A-Za-z_][A-Za-z0-9_]*", i))
			continue
		}
		if seen[key] {
			errs = append(errs, fmt.Errorf("secrets.nonempty: duplicate source key %s", key))
		}
		seen[key] = true
	}
	return errs
}

var secretKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

var (
	unixUserPattern = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	// hostnamePattern: dot-separated DNS labels (letters, digits, inner
	// dashes), at least two. It ends up in an ssh ProxyCommand.
	hostnamePattern = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

// Dest returns a Destination by name. An empty name selects production when
// that Destination is defined, otherwise the only Destination.
func (c *Config) Dest(name string) (Destination, error) {
	n, err := c.DestName(name)
	if err != nil {
		return Destination{}, err
	}
	return c.Destinations[n], nil
}

// DestName resolves name to a Destination name. An empty name selects
// production when it is defined, otherwise the only Destination. Callers
// apply YOHO_DESTINATION before calling with an empty name; this method does
// not read the environment.
func (c *Config) DestName(name string) (string, error) {
	if name == "" {
		if _, ok := c.Destinations["production"]; ok {
			return "production", nil
		}
		keys := sortedKeys(c.Destinations)
		if len(keys) == 0 {
			return "", &NoDestinationsError{Servers: sortedKeys(c.Servers)}
		}
		if len(keys) == 1 {
			return keys[0], nil
		}
		return "", fmt.Errorf("several Destinations (production not defined), choose one with -d: %s", strings.Join(keys, ", "))
	}
	if _, ok := c.Destinations[name]; !ok {
		return "", fmt.Errorf("unknown Destination %q (have: %s)", name, strings.Join(sortedKeys(c.Destinations), ", "))
	}
	return name, nil
}

// NoDestinationsError means the Yoho file defines no Destinations at all.
// File is filled in by the loader when known.
type NoDestinationsError struct {
	Servers []string
	File    string
}

func (e *NoDestinationsError) Error() string {
	file := e.File
	if file == "" {
		file = "the Yoho file"
	}
	names := "<server names>"
	if len(e.Servers) > 0 {
		names = strings.Join(e.Servers, ", ")
	}
	return fmt.Sprintf("no Destinations defined: add destinations.production.servers: [%s] to %s", names, file)
}

// ValidateHealthCheck checks optional Proxy HTTP probe settings. Pointer fields
// distinguish omission from explicit zero/empty values, which are invalid.
func (p ServiceProxy) ValidateHealthCheck() error {
	if p.HealthPort != nil && (*p.HealthPort < 1 || *p.HealthPort > 65535) {
		return fmt.Errorf("x-yoho.proxy.health_port must be between 1 and 65535")
	}
	if p.HealthTimeout != nil {
		d, err := time.ParseDuration(*p.HealthTimeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("x-yoho.proxy.health_timeout must be a positive duration (e.g. 2s or 500ms)")
		}
	}
	return nil
}

// Validate checks Proxy HTTP probes and custom-domain / TLS routing against
// pinned kamal-proxy v0.10.2: --tls-on-demand-url requires --tls, forbids
// --host, and accepts a path or an absolute http(s) URL.
func (p ServiceProxy) Validate() error {
	if err := p.ValidateHealthCheck(); err != nil {
		return err
	}
	return p.ValidateRouting()
}

// CustomDomainsAllow is the on-demand TLS check endpoint, or empty.
func (p ServiceProxy) CustomDomainsAllow() string {
	if p.CustomDomains == nil {
		return ""
	}
	return strings.TrimSpace(p.CustomDomains.Allow)
}

// ValidateRouting checks TLS hosts versus custom_domains.allow.
func (p ServiceProxy) ValidateRouting() error {
	if p.CustomDomains != nil {
		if p.CustomDomainsAllow() == "" {
			return fmt.Errorf("x-yoho.proxy.custom_domains.allow is required")
		}
		if !p.TLS {
			return fmt.Errorf("x-yoho.proxy.custom_domains requires tls: true")
		}
		if len(p.Hosts) > 0 {
			return fmt.Errorf("x-yoho.proxy.custom_domains cannot be set with proxy.hosts (kamal-proxy on-demand TLS is a catch-all route; put known hostnames on another Service or check them in the allow endpoint)")
		}
		if err := validateTLSOnDemandAllow(p.CustomDomainsAllow()); err != nil {
			return err
		}
		return nil
	}
	if p.TLS && len(p.Hosts) == 0 {
		return fmt.Errorf("x-yoho.proxy.tls requires hosts or custom_domains.allow")
	}
	return nil
}

// validateTLSOnDemandAllow mirrors kamal-proxy v0.10.2 parseTLSOnDemandURL
// (internal/server/tls_on_demand.go): a path or an absolute http(s) URL.
func validateTLSOnDemandAllow(allow string) error {
	if strings.HasPrefix(allow, "//") {
		return fmt.Errorf("x-yoho.proxy.custom_domains.allow must be a path or an absolute http(s) URL")
	}
	if strings.HasPrefix(allow, "/") {
		if _, err := url.Parse(allow); err != nil {
			return fmt.Errorf("x-yoho.proxy.custom_domains.allow is not a valid path")
		}
		return nil
	}
	u, err := url.ParseRequestURI(allow)
	if err != nil {
		return fmt.Errorf("x-yoho.proxy.custom_domains.allow must be a path or an absolute http(s) URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("x-yoho.proxy.custom_domains.allow must use http or https")
	}
	if u.Host == "" {
		return fmt.Errorf("x-yoho.proxy.custom_domains.allow URL is missing a host")
	}
	if u.User != nil {
		return fmt.Errorf("x-yoho.proxy.custom_domains.allow must not contain credentials (they would show in the Server's process list)")
	}
	return nil
}

// safePathPattern is the set of characters Yoho accepts in Server paths it
// writes into scripts and journals (same as internal/schedule).
var safePathPattern = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)

// destRoot is the root shared by a Destination's Servers (the first known).
func destRoot(c *Config, d Destination) string {
	for _, s := range d.Servers {
		if sv, ok := c.Servers[s]; ok {
			return rootOrDefault(sv.Root)
		}
	}
	return rootOrDefault("")
}

// pathWithin reports whether p equals dir or lies inside it (lexically).
func pathWithin(p, dir string) bool {
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}

// validateAppDir checks one Destination's app_dir. The Server re-checks
// ownership, modes and physical containment before using it.
func validateAppDir(c *Config, n string, d Destination) []error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	p := d.AppDir
	switch {
	case !strings.HasPrefix(p, "/"):
		add("destinations.%s.app_dir %q must be an absolute path", n, p)
		return errs
	case p == "/":
		add("destinations.%s.app_dir must not be /", n)
		return errs
	case path.Clean(p) != p:
		add("destinations.%s.app_dir %q must be a clean path (no trailing slash, //, . or ..); use %q", n, p, path.Clean(p))
		return errs
	case !safePathPattern.MatchString(p):
		add("destinations.%s.app_dir %q may only use letters, digits, _ . / and -", n, p)
		return errs
	}
	if !appPattern.MatchString(n) {
		add("destinations.%s: the Destination name must match %s when app_dir is set", n, appPattern)
	}
	root := destRoot(c, d)
	if !safePathPattern.MatchString(root) {
		add("destinations.%s.app_dir: the Server root %q may only use letters, digits, _ . / and - when app_dir is set", n, root)
	}
	root = path.Clean(root)
	switch {
	case pathWithin(p, root):
		add("destinations.%s.app_dir %q must be outside the Server root %s", n, p, root)
	case pathWithin(root, p):
		add("destinations.%s.app_dir %q must not contain the Server root %s", n, p, root)
	}
	return errs
}

// validateAppDirOverlap refuses the same or nested app_dir on Destinations
// that share a Server (by name or SSH target).
func validateAppDirOverlap(c *Config) []error {
	var errs []error
	names := sortedKeys(c.Destinations)
	for i, a := range names {
		da := c.Destinations[a]
		if da.AppDir == "" || !strings.HasPrefix(da.AppDir, "/") {
			continue
		}
		for _, b := range names[i+1:] {
			db := c.Destinations[b]
			if db.AppDir == "" || !strings.HasPrefix(db.AppDir, "/") || !shareServer(c, da, db) {
				continue
			}
			pa, pb := path.Clean(da.AppDir), path.Clean(db.AppDir)
			switch {
			case pa == pb:
				errs = append(errs, fmt.Errorf("destinations.%s.app_dir and destinations.%s.app_dir are the same directory on a shared Server", a, b))
			case pathWithin(pa, pb) || pathWithin(pb, pa):
				errs = append(errs, fmt.Errorf("destinations.%s.app_dir and destinations.%s.app_dir are nested on a shared Server", a, b))
			}
		}
	}
	return errs
}

func shareServer(c *Config, a, b Destination) bool {
	for _, x := range a.Servers {
		for _, y := range b.Servers {
			if x == y {
				return true
			}
			sx, okx := c.Servers[x]
			sy, oky := c.Servers[y]
			if okx && oky && sx.SSH != "" && sx.SSH == sy.SSH {
				return true
			}
		}
	}
	return false
}

var (
	composeProjectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	dockerNamePattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	labelFilterPattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_./-]*(=[^\x00-\x1f]*)?$`)
	// ingressHostPattern allows Cloudflare's leading wildcard label.
	ingressHostPattern = regexp.MustCompile(`^(\*\.)?(?:[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?\.)+[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
)

// validateMigrate checks a Destination's one-shot cutover (ADR 0012).
func validateMigrate(c *Config, n string, d Destination) []error {
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	m := d.Migrate
	if d.Runtime == "swarm" {
		add("destinations.%s.migrate supports the compose runtime only; cut over to a compose Destination first, or stop the old stack by hand before `yoho deploy`", n)
	}
	if d.Access == "scoped" {
		add("destinations.%s.migrate needs access: operator (the cutover stops other containers on the Server)", n)
	}
	switch m.From {
	case "", "coolify", "dokploy", "compose":
	default:
		add("destinations.%s.migrate.from %q must be coolify, dokploy or compose", n, m.From)
	}
	switch m.OnFailure {
	case "", "restart_old", "leave":
	default:
		add("destinations.%s.migrate.on_failure %q must be restart_old or leave", n, m.OnFailure)
	}
	st := m.Stop
	if st.ComposeProject == "" && len(st.Labels) == 0 && len(st.Containers) == 0 {
		add("destinations.%s.migrate.stop: set compose_project, labels or containers to select the old containers", n)
	}
	if p := st.ComposeProject; p != "" {
		switch {
		case !composeProjectPattern.MatchString(p):
			add("destinations.%s.migrate.stop.compose_project %q is not a compose project name", n, p)
		case strings.HasPrefix(p, "yoho-"):
			add("destinations.%s.migrate.stop.compose_project %q is a Yoho project; stop only the old platform's containers", n, p)
		}
	}
	for _, l := range st.Labels {
		key, _, _ := strings.Cut(l, "=")
		switch {
		case !labelFilterPattern.MatchString(l):
			add("destinations.%s.migrate.stop.labels: %q must be key or key=value", n, l)
		case strings.HasPrefix(key, "yoho."):
			add("destinations.%s.migrate.stop.labels: %q selects Yoho containers", n, l)
		}
	}
	for _, name := range st.Containers {
		switch {
		case !dockerNamePattern.MatchString(name):
			add("destinations.%s.migrate.stop.containers: %q is not a container name", n, name)
		case strings.HasPrefix(name, "yoho-"):
			add("destinations.%s.migrate.stop.containers: %q is a Yoho container", n, name)
		}
	}
	for _, p := range m.Claim.Paths {
		switch {
		case !strings.HasPrefix(p, "/") || p == "/":
			add("destinations.%s.migrate.claim.paths: %q must be an absolute path other than /", n, p)
		case path.Clean(p) != p:
			add("destinations.%s.migrate.claim.paths: %q must be a clean path; use %q", n, p, path.Clean(p))
		}
	}
	for _, v := range m.Claim.Volumes {
		if !dockerNamePattern.MatchString(v) {
			add("destinations.%s.migrate.claim.volumes: %q is not a volume name", n, v)
		}
	}
	if t := m.Tunnel; t != nil {
		if !secretKeyPattern.MatchString(t.APITokenSecret) {
			add("destinations.%s.migrate.tunnel.api_token_secret must be a secret key name", n)
		}
		if len(t.Hostnames) == 0 {
			add("destinations.%s.migrate.tunnel.hostnames: list the hostnames to repoint", n)
		}
		for _, h := range t.Hostnames {
			if !ingressHostPattern.MatchString(h) || len(h) > 253 {
				add("destinations.%s.migrate.tunnel.hostnames: %q must be a DNS hostname such as app.example.com", n, h)
			}
		}
		if s := t.Service; s != "" {
			if u, err := url.Parse(s); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add("destinations.%s.migrate.tunnel.service %q must be an http:// or https:// URL", n, s)
			}
		}
		tun := c.Proxy.Tunnel
		if d.Proxy != nil && d.Proxy.Tunnel != nil {
			tun = d.Proxy.Tunnel
		}
		if tun == nil || tun.TokenSecret == "" {
			add("destinations.%s.migrate.tunnel needs proxy.tunnel.token_secret: the tunnel token names the account and tunnel whose ingress is repointed", n)
		}
	}
	return errs
}

// UsesRclone reports whether the Backup Target stores through rclone: an
// archive remote:path, or a restic rclone: repository.
func (t BackupTarget) UsesRclone() bool {
	r := t.Repository
	if t.Type == "archive" {
		return !strings.HasPrefix(r, "/") && strings.Contains(r, ":")
	}
	return (t.Type == "" || t.Type == "restic") && strings.HasPrefix(r, "rclone:")
}

// SecretKeys returns the secret keys the Backup Target references.
func (t BackupTarget) SecretKeys() []string {
	var keys []string
	if t.PasswordSecret != "" {
		keys = append(keys, t.PasswordSecret)
	}
	keys = append(keys, t.EnvSecrets...)
	if t.RcloneConfigSecret != "" {
		keys = append(keys, t.RcloneConfigSecret)
	}
	return keys
}

// RcloneWarnings warns about independent token files seeded from one OAuth
// authorization. Targets within one Destination deliberately share a file.
func RcloneWarnings(c *Config) []string {
	uses := map[string]map[string]bool{}
	for _, job := range c.Backups.Jobs {
		key := c.Backups.Targets[job.Target].RcloneConfigSecret
		if key == "" || job.Destination == "" {
			continue
		}
		if uses[key] == nil {
			uses[key] = map[string]bool{}
		}
		uses[key][job.Destination] = true
	}
	var warnings []string
	for _, key := range sortedKeys(uses) {
		dests := sortedKeys(uses[key])
		for i, a := range dests {
			for _, b := range dests[i+1:] {
				warnings = append(warnings, fmt.Sprintf("rclone_config_secret %s is used by Destinations %s and %s; OAuth remotes rotate refresh tokens, so give each Destination its own authorized remote/secret", key, a, b))
			}
		}
	}
	return warnings
}
