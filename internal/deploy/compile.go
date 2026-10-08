package deploy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
)

// ContainerEnvNames are injected into compiled Services (like Kamal's
// KAMAL_VERSION, KAMAL_HOST, ...) unless the compose file sets them.
// YOHO_VERSION is only set on Services whose image is tagged with the
// Version, so third-party Services (databases, cloudflared) keep a stable
// config and are not recreated on every deploy.
var ContainerEnvNames = []string{"YOHO_APP", "YOHO_DESTINATION", "YOHO_VERSION", "YOHO_SERVER", "YOHO_SERVICE"}

// Labels set on every compiled Service.
const (
	LabelApp         = "yoho.app"
	LabelDestination = "yoho.destination"
	LabelVersion     = "yoho.version" // only on Version-tagged images, see ContainerEnvNames
	LabelService     = "yoho.service"
	LabelSecrets     = "yoho.secrets" // keyed fingerprint of the Service's secret set
)

// Proxy defaults (x-yoho.proxy).
const (
	defaultProxyPort     = 80
	defaultHealthPath    = "/up"
	defaultDeployTimeout = 30
	defaultDrainTimeout  = 30
)

var versionRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// servicePlan is what cutover needs to know about a Service. Stored as
// releases/<version>/plan.json so rollback does not depend on today's config.
type servicePlan struct {
	Name           string               `json:"name"`
	Image          string               `json:"image,omitempty"`
	Stateful       bool                 `json:"stateful,omitempty"`
	Replicas       int                  `json:"replicas"`
	Proxy          *config.ServiceProxy `json:"proxy,omitempty"`         // defaults applied
	ProxyNetwork   bool                 `json:"proxy_network,omitempty"` // joins the yoho network
	DependsOn      []string             `json:"depends_on,omitempty"`
	ReleaseCommand []string             `json:"release_command,omitempty"`
}

// validate checks what the compose runtime cannot do.
func validate(d *plan.Deploy) error {
	if len(d.Servers) != 1 {
		return fmt.Errorf("compose runtime deploys to exactly one Server, destination %s has %d", d.Destination, len(d.Servers))
	}
	if d.Servers[0].Host == nil {
		return errors.New("server " + d.Servers[0].Name + " has no open connection")
	}
	if d.Project == nil {
		return errors.New("no compose project")
	}
	if d.App == "" || d.Destination == "" {
		return errors.New("app and destination are required")
	}
	if !versionRe.MatchString(d.Version) {
		return fmt.Errorf("invalid version %q", d.Version)
	}
	var errs []error
	for _, name := range sortedKeys(d.Ext) {
		if _, ok := d.Project.Services[name]; !ok {
			errs = append(errs, fmt.Errorf("x-yoho for unknown service %s", name))
		}
	}
	for _, name := range sortedKeys(d.Project.Services) {
		s := d.Project.Services[name]
		ext := d.Ext[name]
		if ext.Proxy == nil {
			continue
		}
		if ext.Stateful {
			errs = append(errs, fmt.Errorf("service %s: stateful services cannot use x-yoho.proxy (they are recreated stop-first, proxy cutover needs two containers)", name))
		}
		if len(s.Ports) > 0 {
			errs = append(errs, fmt.Errorf("service %s: proxied services must not publish ports; the proxy routes to them over the %s network", name, proxy.Network))
		}
		if s.ContainerName != "" {
			errs = append(errs, fmt.Errorf("service %s: proxied services cannot set container_name (zero-downtime cutover runs two containers)", name))
		}
		if s.NetworkMode != "" {
			errs = append(errs, fmt.Errorf("service %s: proxied services cannot set network_mode", name))
		}
		if ext.Proxy.TLS && len(ext.Proxy.Hosts) == 0 {
			errs = append(errs, fmt.Errorf("service %s: x-yoho.proxy.tls requires hosts", name))
		}
		if p := ext.Proxy.Port; p < 0 || p > 65535 {
			errs = append(errs, fmt.Errorf("service %s: invalid proxy port %d", name, p))
		}
	}
	return errors.Join(errs...)
}

// proxyWithDefaults returns a copy of p with defaults filled.
func proxyWithDefaults(p *config.ServiceProxy) *config.ServiceProxy {
	if p == nil {
		return nil
	}
	c := *p
	c.Hosts = slices.Clone(p.Hosts)
	if c.Port == 0 {
		c.Port = defaultProxyPort
	}
	if c.HealthPath == "" {
		c.HealthPath = defaultHealthPath
	}
	if c.DeployTimeout == 0 {
		c.DeployTimeout = defaultDeployTimeout
	}
	if c.DrainTimeout == 0 {
		c.DrainTimeout = defaultDrainTimeout
	}
	return &c
}

// versioned reports whether image is tagged with version (built for it).
func versioned(image, version string) bool {
	if i := strings.Index(image, "@"); i >= 0 {
		image = image[:i]
	}
	return strings.HasSuffix(image, ":"+version)
}

// compile produces the per-Server compose file and the cutover plan. The
// file references secrets by path only; it never contains secret values.
// dockerVersion is the server engine version from `docker version`; empty
// or below 25 skips fast-start healthcheck fields.
func compile(d *plan.Deploy, server, generationDir string, svcSecrets map[string]map[string]string, hmacKey []byte, dockerVersion string) ([]byte, []servicePlan, error) {
	// Deep copy: d.Project stays untouched for the caller.
	p, err := d.Project.WithServicesTransform(func(_ string, s types.ServiceConfig) (types.ServiceConfig, error) { return s, nil })
	if err != nil {
		return nil, nil, err
	}
	p.Name = release.ProjectName(d.App, d.Destination)
	// Bind sources and config files from the App directory are local paths
	// here; point them at the copies shipped to the Server.
	files, err := appFiles(d.Project)
	if err != nil {
		return nil, nil, err
	}
	rewriteAppFiles(p, files, d.App, d.Destination)
	// The loader named networks and volumes after the local project name
	// (<name>_<key>). Rename them so each Destination gets its own volumes
	// on a shared Server instead of silently sharing data.
	if old := d.Project.Name; old != p.Name {
		for k, n := range p.Networks {
			if !n.External && n.Name == old+"_"+k {
				n.Name = p.Name + "_" + k
				p.Networks[k] = n
			}
		}
		for k, v := range p.Volumes {
			if !v.External && v.Name == old+"_"+k {
				v.Name = p.Name + "_" + k
				p.Volumes[k] = v
			}
		}
	}
	if p.Secrets == nil {
		p.Secrets = types.Secrets{}
	}
	needNet := false
	var plans []servicePlan
	var fastStart []string

	for _, name := range sortedKeys(p.Services) {
		s := p.Services[name]
		ext := d.Ext[name]
		isVersioned := versioned(s.Image, d.Version)

		if s.Labels == nil {
			s.Labels = types.Labels{}
		}
		s.Labels[LabelApp] = d.App
		s.Labels[LabelDestination] = d.Destination
		s.Labels[LabelService] = name
		if isVersioned {
			s.Labels[LabelVersion] = d.Version
		}

		if s.Environment == nil {
			s.Environment = types.MappingWithEquals{}
		}
		setEnv := func(k, v string) {
			if _, ok := s.Environment[k]; !ok {
				s.Environment[k] = &v
			}
		}
		setEnv("YOHO_APP", d.App)
		setEnv("YOHO_DESTINATION", d.Destination)
		setEnv("YOHO_SERVER", server)
		setEnv("YOHO_SERVICE", name)
		if isVersioned {
			setEnv("YOHO_VERSION", d.Version)
		}

		if m := svcSecrets[name]; len(m) > 0 {
			s.Labels[LabelSecrets] = secretsFingerprint(hmacKey, m)
			if ext.SecretsAsEnv {
				s.EnvFiles = append(s.EnvFiles, types.EnvFile{Path: path.Join(generationDir, name+".env"), Required: true})
			} else {
				for _, sec := range sortedKeys(m) {
					key := name + "." + sec
					if _, exists := p.Secrets[key]; exists {
						return nil, nil, fmt.Errorf("compose already defines a top-level secret %q", key)
					}
					for _, existing := range s.Secrets {
						if existing.Target == sec || existing.Target == "/run/secrets/"+sec || (existing.Target == "" && existing.Source == sec) {
							return nil, nil, fmt.Errorf("service %s: secret %s is both declared in compose and delivered by yoho", name, sec)
						}
					}
					p.Secrets[key] = types.SecretConfig{File: path.Join(generationDir, name, sec)}
					s.Secrets = append(s.Secrets, types.ServiceSecretConfig{Source: key, Target: sec})
					setEnv(sec+"_FILE", "/run/secrets/"+sec)
				}
			}
		}

		if ext.Proxy != nil {
			if len(s.Networks) == 0 {
				s.Networks = map[string]*types.ServiceNetworkConfig{"default": nil}
			}
			if _, ok := s.Networks[proxy.Network]; !ok {
				s.Networks[proxy.Network] = nil
			}
		}
		_, onNet := s.Networks[proxy.Network]
		needNet = needNet || onNet
		if UseFastStart(ext, s.HealthCheck, false, dockerVersion) {
			fastStart = append(fastStart, name)
		}
		p.Services[name] = s

		replicas := s.GetScale()
		if replicas < 1 {
			replicas = 1
		}
		plans = append(plans, servicePlan{
			Name:           name,
			Image:          s.Image,
			Stateful:       ext.Stateful,
			Replicas:       replicas,
			Proxy:          proxyWithDefaults(ext.Proxy),
			ProxyNetwork:   onNet,
			DependsOn:      sortedKeys(s.DependsOn),
			ReleaseCommand: slices.Clone(ext.ReleaseCommand),
		})
	}

	if needNet {
		if p.Networks == nil {
			p.Networks = types.Networks{}
		}
		if n, ok := p.Networks[proxy.Network]; ok {
			if !n.External {
				return nil, nil, fmt.Errorf("network %q must be external (shared with the Server's proxy)", proxy.Network)
			}
		} else {
			p.Networks[proxy.Network] = types.NetworkConfig{Name: proxy.Network, External: true}
		}
	}

	raw, err := p.MarshalYAML()
	if err != nil {
		return nil, nil, fmt.Errorf("marshal compose: %w", err)
	}
	raw, err = injectFastStart(raw, fastStart)
	if err != nil {
		return nil, nil, err
	}
	out, err := escapeDollars(raw)
	if err != nil {
		return nil, nil, err
	}
	return out, plans, nil
}

// escapeDollars rewrites `$` as `$$` in every scalar value. The project was
// interpolated when loaded; `docker compose` on the Server would interpolate
// again, so literal dollars must be escaped.
func escapeDollars(src []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return nil, fmt.Errorf("re-parse compiled compose: %w", err)
	}
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c)
			}
		case yaml.MappingNode:
			for i := 1; i < len(n.Content); i += 2 {
				walk(n.Content[i])
			}
		case yaml.ScalarNode:
			if n.Tag == "!!str" || n.Tag == "" {
				n.Value = strings.ReplaceAll(n.Value, "$", "$$")
			}
		}
	}
	walk(&doc)
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
