// Package gate is the scoped access path of ADR 0011: `yoho gate`, run by
// sshd from a forced command of the yoho-gate user, reads one structured
// request, checks it against the App Destination's Members file and the
// deploy Policy, runs it with the existing runtime code as the owner of the
// App directory, appends an audit line and exits. A deploy or rollback is
// detached so it finishes after the SSH session drops; nothing else stays
// running.
package gate

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
)

// Verbs a Member can be granted. hello is always allowed for a Member of
// the App Destination. Access management is never a verb.
const (
	VerbHello    = "hello"
	VerbPS       = "ps"
	VerbStats    = "stats"
	VerbLogs     = "logs"
	VerbDash     = "dash"
	VerbReleases = "releases"
	VerbDeploy   = "deploy"
	VerbRollback = "rollback"
	// verbImages is the image check before a deploy; it needs deploy.
	verbImages = "images"
)

// GrantableVerbs is every verb `yoho access add --verbs` accepts.
var GrantableVerbs = []string{VerbDeploy, VerbRollback, VerbReleases, VerbPS, VerbLogs, VerbStats, VerbDash}

// ReadVerbs are the read-only verbs.
var ReadVerbs = []string{VerbReleases, VerbPS, VerbLogs, VerbStats, VerbDash}

// grant is the verb a request needs.
func grant(verb string) string {
	if verb == verbImages {
		return VerbDeploy
	}
	return verb
}

var (
	// MemberRe is a Member name: it appears in authorized_keys and audit lines.
	MemberRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	nameRe   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	destRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// ValidAppDest checks request names before they become paths.
func ValidAppDest(app, dest string) error {
	if !nameRe.MatchString(app) || len(app) > 128 {
		return fmt.Errorf("invalid App name %q", app)
	}
	if !destRe.MatchString(dest) || len(dest) > 128 {
		return fmt.Errorf("invalid Destination name %q", dest)
	}
	return nil
}

// Members is <access>/members.yml.
type Members struct {
	Members map[string]Member `yaml:"members" json:"members"`
}

// Member is one Member's grant.
type Member struct {
	Verbs []string `yaml:"verbs" json:"verbs"`
}

// MembersPath is the Members file of an App Destination.
func MembersPath(app, dest string) string {
	return path.Join(release.AccessDir(app, dest), "members.yml")
}

// SettingsPath holds the operator Settings for Gate deploys.
func SettingsPath(app, dest string) string {
	return path.Join(release.AccessDir(app, dest), "settings.json")
}

// AuditPath is the audit log.
func AuditPath(app, dest string) string { return path.Join(release.AccessDir(app, dest), "audit.log") }

// ParseMembers decodes a Members file and validates it.
func ParseMembers(b []byte) (*Members, error) {
	var m Members
	if err := yaml.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("members.yml: %w", err)
	}
	if m.Members == nil {
		m.Members = map[string]Member{}
	}
	for name, mem := range m.Members {
		if !MemberRe.MatchString(name) {
			return nil, fmt.Errorf("members.yml: invalid Member name %q", name)
		}
		if err := CheckVerbs(mem.Verbs); err != nil {
			return nil, fmt.Errorf("members.yml: %s: %w", name, err)
		}
	}
	return &m, nil
}

// Marshal renders the Members file.
func (m *Members) Marshal() ([]byte, error) {
	for name, mem := range m.Members {
		v := slices.Clone(mem.Verbs)
		slices.Sort(v)
		m.Members[name] = Member{Verbs: slices.Compact(v)}
	}
	b, err := yaml.Marshal(m)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Managed by `yoho access`. Member -> verbs the Gate allows.\n"), b...), nil
}

// CheckVerbs rejects verbs that cannot be granted.
func CheckVerbs(verbs []string) error {
	for _, v := range verbs {
		if !slices.Contains(GrantableVerbs, v) {
			return fmt.Errorf("unknown verb %q (verbs: %s)", v, strings.Join(GrantableVerbs, ", "))
		}
	}
	return nil
}

// ParseVerbs reads a --verbs list: comma-separated verbs, "all" or "read".
func ParseVerbs(s string) ([]string, error) {
	var out []string
	for _, v := range strings.Split(s, ",") {
		switch v = strings.TrimSpace(v); v {
		case "":
		case "all":
			out = append(out, GrantableVerbs...)
		case "read":
			out = append(out, ReadVerbs...)
		default:
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no verbs given")
	}
	if err := CheckVerbs(out); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// Allowed reports whether member may run verb. hello needs membership only.
func (m *Members) Allowed(member, verb string) bool {
	mem, ok := m.Members[member]
	if !ok {
		return false
	}
	return verb == VerbHello || slices.Contains(mem.Verbs, grant(verb))
}

// loadMembers reads the Members file. A missing file means no Members.
func loadMembers(app, dest string) (*Members, error) {
	b, err := os.ReadFile(MembersPath(app, dest))
	if errors.Is(err, fs.ErrNotExist) {
		return &Members{Members: map[string]Member{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseMembers(b)
}

// AppGrant is one App Destination a Member may use.
type AppGrant struct {
	App         string   `json:"app"`
	Destination string   `json:"destination"`
	Verbs       []string `json:"verbs"`
}

// grantsOf scans every App Destination under Root for member.
func grantsOf(member string) []AppGrant {
	files, _ := filepath.Glob(filepath.Join(release.Root, "apps", "*", "*", "access", "members.yml"))
	var out []AppGrant
	for _, f := range files {
		destDir := filepath.Dir(filepath.Dir(f))
		app, dest := filepath.Base(filepath.Dir(destDir)), filepath.Base(destDir)
		if ValidAppDest(app, dest) != nil {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		m, err := ParseMembers(b)
		if err != nil {
			continue
		}
		if mem, ok := m.Members[member]; ok {
			out = append(out, AppGrant{App: app, Destination: dest, Verbs: mem.Verbs})
		}
	}
	return out
}

// Settings are the operator's choices a Gate deploy uses instead of the
// Member's Yoho file: the shared Proxy and the Registry belong to the
// operator, and so do the hostnames the App may claim. `yoho access secrets
// push` writes them with the secrets.
type Settings struct {
	Proxy          config.ProxyConfig `json:"proxy"`
	Registry       *config.Registry   `json:"registry,omitempty"`
	RetainReleases int                `json:"retain_releases,omitempty"`
	// Routes: Proxy hostnames the App may use ("" is the catch-all route).
	Routes []string `json:"routes"`
	// Server name in the operator's Yoho file, recorded in Releases.
	Server string `json:"server"`
	// Limits bound what a scoped deploy may ask of the Server. `yoho access
	// secrets push` keeps the operator's values; edit them in settings.json.
	Limits Limits `json:"limits"`
}

// Limits are the operator's bounds on scoped deploys. Zero means the
// default (see the Default* constants).
type Limits struct {
	// MaxUploadBytes bounds an image archive, compressed and decompressed.
	MaxUploadBytes int64 `json:"max_upload_bytes,omitempty"`
	// UploadTimeoutSeconds bounds receiving an image archive.
	UploadTimeoutSeconds int `json:"upload_timeout_seconds,omitempty"`
	// Ports are the host ports Services may publish (none by default).
	Ports []int `json:"ports,omitempty"`
	// MaxShmBytes bounds shm_size; MaxTmpfsBytes each tmpfs mount (an
	// unsized tmpfs gets this size).
	MaxShmBytes   int64 `json:"max_shm_bytes,omitempty"`
	MaxTmpfsBytes int64 `json:"max_tmpfs_bytes,omitempty"`
	// MaxUlimit bounds every ulimit; unlimited (-1) is refused.
	MaxUlimit int64 `json:"max_ulimit,omitempty"`
	// PidsLimit is set on containers without one and bounds explicit ones.
	PidsLimit int64 `json:"pids_limit,omitempty"`
	// AllowEmpty allows a deploy with no Services (it stops every container
	// of the App).
	AllowEmpty bool `json:"allow_empty,omitempty"`
	// MaxMemoryBytes and MaxCPUs bound each container (mem_limit, cpus and
	// deploy.resources.limits); containers without a limit get these.
	MaxMemoryBytes int64   `json:"max_memory_bytes,omitempty"`
	MaxCPUs        float64 `json:"max_cpus,omitempty"`
	// MaxReplicas bounds each Service's replicas (scale, deploy.replicas);
	// MaxContainers the App's replicas in total.
	MaxReplicas   int `json:"max_replicas,omitempty"`
	MaxContainers int `json:"max_containers,omitempty"`
	// Registries are repository prefixes ("ghcr.io/acme/") the Gate pulls
	// with the Server's `docker login`. Other images are pulled anonymously,
	// so a Member cannot use the Server's credentials for private images.
	Registries []string `json:"registries,omitempty"`
}

// Limit defaults.
const (
	DefaultMaxUploadBytes       = 8 << 30
	DefaultUploadTimeoutSeconds = 1800
	DefaultMaxShmBytes          = 1 << 30
	DefaultMaxTmpfsBytes        = 1 << 30
	DefaultMaxUlimit            = 1 << 20
	DefaultPidsLimit            = 4096
	DefaultMaxMemoryBytes       = 2 << 30
	DefaultMaxCPUs              = 2
	DefaultMaxReplicas          = 4
	DefaultMaxContainers        = 16
)

// WithDefaults fills zero limits with the defaults.
func (l Limits) WithDefaults() Limits {
	def := func(v *int64, d int64) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&l.MaxUploadBytes, DefaultMaxUploadBytes)
	def(&l.MaxShmBytes, DefaultMaxShmBytes)
	def(&l.MaxTmpfsBytes, DefaultMaxTmpfsBytes)
	def(&l.MaxUlimit, DefaultMaxUlimit)
	def(&l.PidsLimit, DefaultPidsLimit)
	def(&l.MaxMemoryBytes, DefaultMaxMemoryBytes)
	if l.MaxCPUs <= 0 {
		l.MaxCPUs = DefaultMaxCPUs
	}
	if l.MaxReplicas <= 0 {
		l.MaxReplicas = DefaultMaxReplicas
	}
	if l.MaxContainers <= 0 {
		l.MaxContainers = DefaultMaxContainers
	}
	if l.UploadTimeoutSeconds <= 0 {
		l.UploadTimeoutSeconds = DefaultUploadTimeoutSeconds
	}
	return l
}

// ParseSettings decodes settings.json.
func ParseSettings(b []byte) (*Settings, error) {
	var s Settings
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("settings.json: %w", err)
	}
	if s.Registry != nil {
		r := *s.Registry
		r.PasswordSecret = "" // never resolved on the Server
		s.Registry = &r
	}
	for _, p := range s.Limits.Ports {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("settings.json: limits.ports: %d is not a port", p)
		}
	}
	for _, r := range s.Limits.Registries {
		if !strings.HasSuffix(r, "/") || strings.ContainsAny(r, " @\t\n") {
			return nil, fmt.Errorf("settings.json: limits.registries: %q must be a repository prefix ending in /", r)
		}
	}
	s.Limits = s.Limits.WithDefaults()
	return &s, nil
}
