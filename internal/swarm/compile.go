package swarm

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/secrets"
)

// Proxy defaults (x-yoho.proxy), same as the compose runtime.
const (
	defaultProxyPort     = 80
	defaultHealthPath    = "/up"
	defaultDeployTimeout = 30
	defaultDrainTimeout  = 30
)

// Swarm rolling update defaults (ADR 0007): new tasks first so old tasks keep
// serving, every replica at once unless parallelism is set, and a short
// monitor so a failed task rolls back without a long quiet window.
const updateMonitor = "5s"

// servicePlan is what the runtime needs to know about a Service after
// compiling. Stored in releases/<version>/plan.json.
type servicePlan struct {
	Name     string `json:"name"`
	Image    string `json:"image,omitempty"`
	Stateful bool   `json:"stateful,omitempty"`
	// Replicas; 0 for global mode.
	Replicas int `json:"replicas"`
	// Global is deploy.mode global (Replicas is 0 and not a count).
	Global         bool                 `json:"global,omitempty"`
	Proxy          *config.ServiceProxy `json:"proxy,omitempty"` // defaults applied
	StrictDrain    bool                 `json:"strict_drain,omitempty"`
	ProxyNetwork   bool                 `json:"proxy_network,omitempty"`
	DependsOn      []string             `json:"depends_on,omitempty"`
	ReleaseCommand []string             `json:"release_command,omitempty"`
}

// stackPlan is releases/<version>/plan.json for the swarm runtime.
type stackPlan struct {
	Stack    string        `json:"stack"`
	Services []servicePlan `json:"services"`
	// Swarm secret names the stack references; secrets referenced by any
	// retained Release are never pruned.
	Secrets []string `json:"secrets,omitempty"`
	// Some Service reads secrets from an env_file in the secrets generation.
	EnvFiles bool `json:"env_files,omitempty"`
}

func (p stackPlan) needsProxy() bool {
	return slices.ContainsFunc(p.Services, func(sp servicePlan) bool { return sp.Proxy != nil || sp.ProxyNetwork })
}

// Secret names become env var names (<NAME>_FILE) and secret targets.
var secretNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// maxSecretName is Swarm's limit on object names.
const maxSecretName = 64

// SecretName is the Swarm secret holding one value: content-addressed
// (`<stack>_<NAME>_<hash8>`) because Swarm secrets are immutable, so a
// changed value gets a new secret and triggers a rolling update. The hash is
// keyed with the Destination's audit key so low-entropy values cannot be
// brute-forced from the name.
func SecretName(stack, name, value string, key []byte) string {
	h := secrets.Fingerprint(key, name+"\x00"+value)[:8]
	n := stack + "_" + name
	if max := maxSecretName - len(h) - 1; len(n) > max {
		n = strings.TrimRight(n[:max], "_.-")
	}
	return n + "_" + h
}

// compileInput is everything compile needs besides the Deploy.
type compileInput struct {
	// Hostname of the Server Stateful Services are pinned to.
	PinHost string
	// Secrets generation directory (env_file delivery).
	GenerationDir string
	// Service -> container name -> value.
	SvcSecrets map[string]map[string]string
	HMACKey    []byte
	// DockerVersion is `docker version` Server.Version. Empty or below 25
	// skips start_interval; older engines reject that field.
	DockerVersion string
}

// compiled is the result of compile.
type compiled struct {
	YAML     []byte
	Doc      map[string]any // sanitized stack document (unescaped)
	Plan     stackPlan
	Warnings []string
}

// Service keys `docker stack deploy` accepts (docker/cli compose schema
// v3.x, probed against Docker CLI 29). Others are dropped with a warning.
var allowedServiceKeys = map[string]bool{
	"cap_add": true, "cap_drop": true, "cgroup_parent": true, "command": true, "configs": true,
	"container_name": true, "credential_spec": true, "deploy": true, "devices": true, "dns": true,
	"dns_search": true, "domainname": true, "entrypoint": true, "env_file": true, "environment": true,
	"expose": true, "external_links": true, "extra_hosts": true, "healthcheck": true, "hostname": true,
	"image": true, "init": true, "ipc": true, "isolation": true, "labels": true, "links": true,
	"logging": true, "mac_address": true, "network_mode": true, "networks": true, "oom_score_adj": true,
	"pid": true, "ports": true, "privileged": true, "read_only": true, "restart": true, "secrets": true,
	"security_opt": true, "shm_size": true, "stdin_open": true, "stop_grace_period": true,
	"stop_signal": true, "sysctls": true, "tmpfs": true, "tty": true, "ulimits": true, "user": true,
	"userns_mode": true, "volumes": true, "working_dir": true,
}

// Dropped without a warning: handled elsewhere or flagged by composefile.Check.
var silentlyDropped = map[string]bool{"depends_on": true, "scale": true, "pull_policy": true, "build": true}

var allowedServiceNetworkKeys = map[string]bool{"aliases": true, "ipv4_address": true, "ipv6_address": true}

var allowedNetworkKeys = map[string]bool{
	"driver": true, "driver_opts": true, "ipam": true, "external": true, "internal": true,
	"attachable": true, "labels": true, "name": true, "enable_ipv6": true,
}

var allowedPortKeys = map[string]bool{"mode": true, "target": true, "published": true, "protocol": true}

// validate checks what the swarm runtime cannot do.
func validate(d *plan.Deploy) error {
	if len(d.Servers) == 0 {
		return fmt.Errorf("destination %s has no Servers", d.Destination)
	}
	for _, s := range d.Servers {
		if s.Host == nil {
			return errors.New("server " + s.Name + " has no open connection")
		}
	}
	if d.Project == nil {
		return errors.New("no compose project")
	}
	if d.App == "" || d.Destination == "" {
		return errors.New("app and destination are required")
	}
	if !deploy.ValidVersion(d.Version) {
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
		if ext.Stateful {
			if s.GetScale() > 1 {
				errs = append(errs, fmt.Errorf("service %s: stateful services run exactly one replica", name))
			}
			if s.Deploy != nil && s.Deploy.Mode != "" && s.Deploy.Mode != "replicated" {
				errs = append(errs, fmt.Errorf("service %s: stateful services must use deploy.mode replicated", name))
			}
		}
		if ext.Proxy == nil {
			continue
		}
		if ext.Stateful {
			errs = append(errs, fmt.Errorf("service %s: stateful services cannot use x-yoho.proxy (they are updated stop-first)", name))
		}
		if len(s.Ports) > 0 {
			errs = append(errs, fmt.Errorf("service %s: proxied services must not publish ports; the proxy routes to them over the %s network", name, proxy.Network))
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

// compile turns the compose project into a `docker stack deploy` file. The
// file references secrets by Swarm secret name (or env_file path) only; it
// never contains secret values.
func compile(d *plan.Deploy, in compileInput) (*compiled, error) {
	stack := release.ProjectName(d.App, d.Destination)
	raw, err := d.Project.MarshalYAML()
	if err != nil {
		return nil, fmt.Errorf("marshal compose: %w", err)
	}
	var src map[string]any
	if err := yaml.Unmarshal(raw, &src); err != nil {
		return nil, fmt.Errorf("re-parse compose: %w", err)
	}
	c := &compiled{Plan: stackPlan{Stack: stack}}
	warn := func(f string, a ...any) { c.Warnings = append(c.Warnings, fmt.Sprintf(f, a...)) }

	doc := map[string]any{}
	for _, k := range sortedKeys(src) {
		switch k {
		case "services", "networks", "volumes", "secrets", "configs":
			if m, ok := src[k].(map[string]any); ok {
				doc[k] = m
			}
		case "name":
		default:
			if !strings.HasPrefix(k, "x-") {
				warn("top-level %s is not supported by docker stack deploy; dropped", k)
			}
		}
	}
	// The loader named networks and volumes <project>_<key>; let the stack
	// namespace them instead (<stack>_<key>), as it does for Services.
	for _, kind := range []string{"networks", "volumes"} {
		m := mapOf(doc, kind)
		for _, k := range sortedKeys(m) {
			e, _ := m[k].(map[string]any)
			if e == nil {
				continue
			}
			if ext, _ := e["external"].(bool); !ext && e["name"] == d.Project.Name+"_"+k {
				delete(e, "name")
			}
			if kind == "networks" {
				for _, nk := range sortedKeys(e) {
					if !allowedNetworkKeys[nk] {
						delete(e, nk)
						if !strings.HasPrefix(nk, "x-") {
							warn("network %s: %s is not supported by docker stack deploy; dropped", k, nk)
						}
					}
				}
			}
			if len(e) == 0 {
				m[k] = nil
			}
		}
	}

	services := mapOf(doc, "services")
	topSecrets := ensureMap(doc, "secrets")
	secretSet := map[string]bool{}
	needNet := false

	for _, name := range sortedKeys(d.Project.Services) {
		ps := d.Project.Services[name]
		ext := d.Ext[name]
		s, _ := services[name].(map[string]any)
		if s == nil {
			return nil, fmt.Errorf("service %s missing from marshaled compose", name)
		}
		for _, k := range sortedKeys(s) {
			if allowedServiceKeys[k] {
				continue
			}
			delete(s, k)
			if !silentlyDropped[k] && !strings.HasPrefix(k, "x-") {
				warn("service %s: %s is not supported by docker stack deploy; dropped", name, k)
			}
		}
		for _, k := range []string{"command", "entrypoint"} {
			if v, ok := s[k]; ok && v == nil {
				delete(s, k)
			}
		}
		fixEnvFiles(s)
		fixPorts(s, func(f string, a ...any) { warn("service "+name+": "+f, a...) })
		for net, v := range mapOf(s, "networks") {
			if nc, ok := v.(map[string]any); ok {
				for k := range nc {
					if !allowedServiceNetworkKeys[k] {
						delete(nc, k)
					}
				}
				if len(nc) == 0 {
					mapOf(s, "networks")[net] = nil
				}
			}
		}

		dep := ensureMap(s, "deploy")
		if res := mapOf(dep, "resources"); res != nil {
			if rv, ok := res["reservations"].(map[string]any); ok {
				if _, ok := rv["devices"]; ok {
					delete(rv, "devices")
					warn("service %s: deploy.resources.reservations.devices is not supported by docker stack deploy; dropped", name)
				}
			}
		}
		mode, _ := dep["mode"].(string)
		replicas := 0
		global := mode == "global"
		if mode == "" || mode == "replicated" {
			// GetScale is 1 when neither scale nor deploy.replicas is set, so
			// an explicit 0 (parked Service) survives.
			replicas = ps.GetScale()
			if replicas < 0 {
				replicas = 1
			}
			if ext.Stateful {
				replicas = 1
			}
			dep["replicas"] = replicas
		}

		isVersioned := versioned(ps.Image, d.Version)
		labels := map[string]any{
			deploy.LabelApp: d.App, deploy.LabelDestination: d.Destination, deploy.LabelService: name,
		}
		if isVersioned {
			labels[deploy.LabelVersion] = d.Version
		}
		for _, target := range []map[string]any{ensureMap(s, "labels"), ensureMap(dep, "labels")} {
			for k, v := range labels {
				target[k] = v
			}
		}

		env := ensureMap(s, "environment")
		setEnv := func(k, v string) {
			if _, ok := env[k]; !ok {
				env[k] = v
			}
		}
		setEnv("YOHO_APP", d.App)
		setEnv("YOHO_DESTINATION", d.Destination)
		// Tasks may run on any node: Swarm expands the template per task.
		setEnv("YOHO_SERVER", "{{.Node.Hostname}}")
		setEnv("YOHO_SERVICE", name)
		if isVersioned {
			setEnv("YOHO_VERSION", d.Version)
		}

		if m := in.SvcSecrets[name]; len(m) > 0 {
			if ext.SecretsAsEnv {
				files, _ := s["env_file"].([]any)
				s["env_file"] = append(files, in.GenerationDir+"/"+name+".env")
				c.Plan.EnvFiles = true
				// The env_file path is generation-dependent and the planner
				// ignores it; a keyed digest of the values makes rotation a
				// visible change and rolls the Service (same as the Compose
				// runtime's yoho.secrets label).
				ensureMap(s, "labels")[deploy.LabelSecrets] = secretSetFingerprint(in.HMACKey, m)
			} else {
				existing, _ := s["secrets"].([]any)
				for _, sec := range sortedKeys(m) {
					if !secretNameRe.MatchString(sec) {
						return nil, fmt.Errorf("service %s: invalid secret name %q (letters, digits, underscore)", name, sec)
					}
					for _, e := range existing {
						if secretTarget(e) == sec {
							return nil, fmt.Errorf("service %s: secret %s is both declared in compose and delivered by yoho", name, sec)
						}
					}
					sn := SecretName(c.Plan.Stack, sec, m[sec], in.HMACKey)
					if _, ok := topSecrets[sn]; ok && !secretSet[sn] {
						return nil, fmt.Errorf("compose already defines a top-level secret %q", sn)
					}
					topSecrets[sn] = map[string]any{"external": true, "name": sn}
					secretSet[sn] = true
					existing = append(existing, map[string]any{"source": sn, "target": sec})
					setEnv(sec+"_FILE", "/run/secrets/"+sec)
				}
				s["secrets"] = existing
			}
		}

		up := ensureMap(dep, "update_config")
		rb := ensureMap(dep, "rollback_config")
		if ext.Stateful {
			// Two tasks must never share one volume: stop the old one first.
			if o, _ := up["order"].(string); o == "start-first" {
				warn("service %s: stateful services update stop-first; ignoring update_config.order start-first", name)
			}
			up["order"] = "stop-first"
			rb["order"] = "stop-first"
			// Always pinned to the first Server (ADR 0005): user constraints
			// narrow further but never replace the pin.
			if in.PinHost == "" {
				return nil, fmt.Errorf("service %s: stateful service needs a Server to pin to", name)
			}
			pl := ensureMap(dep, "placement")
			cs, _ := pl["constraints"].([]any)
			pinned := false
			for _, e := range cs {
				host, ok := hostnameConstraint(e)
				if !ok {
					continue
				}
				if host != in.PinHost {
					return nil, fmt.Errorf("service %s: placement constraint %q contradicts the Stateful pin to %s (the first Server); remove it", name, e, in.PinHost)
				}
				pinned = true
			}
			if !pinned {
				cs = append(cs, "node.hostname == "+in.PinHost)
			}
			pl["constraints"] = cs
		} else {
			setDefault(up, "order", "start-first")
			setDefault(rb, "order", "start-first")
		}
		// Unset parallelism matches the replica count so every new task starts
		// together. Global mode has no fixed count; 0 updates every task at once.
		// Rollback uses the same width and monitor unless the user set them.
		setDefault(up, "parallelism", replicas)
		setDefault(up, "failure_action", "rollback")
		setDefault(up, "monitor", updateMonitor)
		setDefault(rb, "parallelism", replicas)
		setDefault(rb, "monitor", updateMonitor)

		if deploy.UseFastStart(ext, ps.HealthCheck, true, in.DockerVersion) {
			if hc, ok := s["healthcheck"].(map[string]any); ok {
				if _, ok := hc["start_period"]; !ok {
					if _, ok := hc["start_interval"]; !ok {
						hc["start_period"] = deploy.FastStartPeriod
						hc["start_interval"] = deploy.FastStartInterval
					}
				}
			}
		}

		onNet := false
		if ext.Proxy != nil {
			nets := ensureMap(s, "networks")
			if len(nets) == 0 {
				nets["default"] = nil
			}
			nets[proxy.Network] = nil
			if ext.StrictDrain {
				dep["endpoint_mode"] = "dnsrr"
			}
		}
		if _, ok := mapOf(s, "networks")[proxy.Network]; ok {
			onNet = true
			needNet = true
		}

		c.Plan.Services = append(c.Plan.Services, servicePlan{
			Name: name, Image: ps.Image, Stateful: ext.Stateful, Replicas: replicas, Global: global,
			Proxy: proxyWithDefaults(ext.Proxy), StrictDrain: ext.StrictDrain && ext.Proxy != nil,
			ProxyNetwork: onNet, DependsOn: sortedKeys(ps.DependsOn),
			ReleaseCommand: slices.Clone(ext.ReleaseCommand),
		})
	}

	if needNet {
		nets := ensureMap(doc, "networks")
		if n, ok := nets[proxy.Network]; ok {
			if m, _ := n.(map[string]any); m == nil || m["external"] != true {
				return nil, fmt.Errorf("network %q must be external (the shared overlay network of the Proxy)", proxy.Network)
			}
		}
		nets[proxy.Network] = map[string]any{"external": true, "name": proxy.Network}
	}
	if len(topSecrets) == 0 {
		delete(doc, "secrets")
	}
	c.Plan.Secrets = sortedKeys(secretSet)
	c.Doc = doc
	c.YAML, err = encode(doc)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// encode marshals a stack document with `$` escaped, because docker stack
// deploy interpolates again and the project was interpolated when loaded.
func encode(doc map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("marshal stack file: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return deploy.EscapeDollars(buf.Bytes())
}

// subset returns the stack file with only the given Services (top-level
// networks, volumes and secrets kept). Used to start release_command
// dependencies before the full update.
func subset(doc map[string]any, keep []string) ([]byte, error) {
	out := map[string]any{}
	for k, v := range doc {
		out[k] = v
	}
	svcs := map[string]any{}
	for name, s := range mapOf(doc, "services") {
		if slices.Contains(keep, name) {
			svcs[name] = s
		}
	}
	out["services"] = svcs
	return encode(out)
}

// releaseJobStack builds a separate stack running svc's release_command
// once (deploy.mode replicated-job, no restarts). It reuses the compiled
// Service (image, env, secrets, volumes, placement) and joins the main
// stack's networks and volumes as external, so migrations reach the
// database exactly like the Service will.
func releaseJobStack(doc map[string]any, stack, svc string, command []string) ([]byte, error) {
	orig, _ := mapOf(doc, "services")[svc].(map[string]any)
	if orig == nil {
		return nil, fmt.Errorf("service %s not in stack", svc)
	}
	s := map[string]any{}
	for k, v := range orig {
		switch k {
		case "ports", "healthcheck", "deploy", "container_name", "hostname":
		default:
			s[k] = v
		}
	}
	cmd := make([]any, len(command))
	for i, a := range command {
		cmd[i] = a
	}
	s["command"] = cmd
	s["healthcheck"] = map[string]any{"disable": true}
	job := map[string]any{
		"mode":           "replicated-job",
		"replicas":       1,
		"restart_policy": map[string]any{"condition": "none"},
	}
	if od := mapOf(orig, "deploy"); od != nil {
		if pl, ok := od["placement"]; ok {
			job["placement"] = pl
		}
		if l, ok := od["labels"]; ok {
			job["labels"] = l
		}
		if r, ok := od["resources"]; ok {
			job["resources"] = r
		}
	}
	s["deploy"] = job

	out := map[string]any{"services": map[string]any{svc: s}}
	// Networks: the main stack's, by their real names.
	nets := map[string]any{}
	svcNets := mapOf(s, "networks")
	if len(svcNets) == 0 {
		svcNets = map[string]any{"default": nil}
		s["networks"] = svcNets
	}
	for _, n := range sortedKeys(svcNets) {
		nets[n] = map[string]any{"external": true, "name": realName(doc, "networks", n, stack)}
	}
	out["networks"] = nets
	// Named volumes, by their real names.
	vols := map[string]any{}
	if vs, ok := s["volumes"].([]any); ok {
		for _, v := range vs {
			m, _ := v.(map[string]any)
			if m == nil || m["type"] != "volume" {
				continue
			}
			src, _ := m["source"].(string)
			if src == "" {
				continue
			}
			vols[src] = map[string]any{"external": true, "name": realName(doc, "volumes", src, stack)}
		}
	}
	if len(vols) > 0 {
		out["volumes"] = vols
	}
	secs := map[string]any{}
	if ss, ok := s["secrets"].([]any); ok {
		for _, sv := range ss {
			src := secretSource(sv)
			if def, ok := mapOf(doc, "secrets")[src]; ok {
				if m, _ := def.(map[string]any); m != nil && m["external"] == true {
					secs[src] = def
					continue
				}
				// Compose-declared file secrets were created by the main
				// stack as <stack>_<key>.
				secs[src] = map[string]any{"external": true, "name": realName(doc, "secrets", src, stack)}
			}
		}
	}
	if len(secs) > 0 {
		out["secrets"] = secs
	}
	// Top-level configs the Service references. External ones are shared;
	// others get a copy of the definition (without the main stack's fixed
	// name) so the release stack owns its own and can run on a first deploy.
	cfgs := map[string]any{}
	if cs, ok := s["configs"].([]any); ok {
		for _, cv := range cs {
			src := secretSource(cv)
			def, ok := mapOf(doc, "configs")[src]
			if !ok {
				continue
			}
			m, _ := def.(map[string]any)
			if m == nil || m["external"] == true {
				cfgs[src] = def
				continue
			}
			cp := map[string]any{}
			for k, v := range m {
				if k != "name" {
					cp[k] = v
				}
			}
			cfgs[src] = cp
		}
	}
	if len(cfgs) > 0 {
		out["configs"] = cfgs
	}
	return encode(out)
}

// realName is the Docker object name of a stack-level network/volume/secret.
func realName(doc map[string]any, kind, key, stack string) string {
	if m, _ := mapOf(doc, kind)[key].(map[string]any); m != nil {
		if n, _ := m["name"].(string); n != "" {
			return n
		}
	}
	return stack + "_" + key
}

// hostnameConstraint parses `node.hostname == X` (the only form that
// selects one node); other constraints report false.
func hostnameConstraint(v any) (string, bool) {
	c, _ := v.(string)
	l, r, ok := strings.Cut(c, "==")
	if !ok || strings.TrimSpace(l) != "node.hostname" {
		return "", false
	}
	return strings.TrimSpace(r), true
}

// secretSetFingerprint is a keyed digest of one Service's secret set.
func secretSetFingerprint(key []byte, m map[string]string) string {
	var b strings.Builder
	for _, k := range sortedKeys(m) {
		b.WriteString(k)
		b.WriteByte(0)
		b.WriteString(m[k])
		b.WriteByte(0)
	}
	return secrets.Fingerprint(key, b.String())
}

func fixEnvFiles(s map[string]any) {
	files, ok := s["env_file"].([]any)
	if !ok {
		return
	}
	for i, f := range files {
		if m, ok := f.(map[string]any); ok {
			files[i] = m["path"]
		}
	}
}

func fixPorts(s map[string]any, warn func(string, ...any)) {
	ports, ok := s["ports"].([]any)
	if !ok {
		return
	}
	for _, p := range ports {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		if v, ok := m["published"].(string); ok {
			if n, err := strconv.Atoi(v); err == nil {
				m["published"] = n
			}
		}
		for _, k := range sortedKeys(m) {
			if allowedPortKeys[k] {
				continue
			}
			if k == "host_ip" {
				warn("port %v: host_ip is not supported by docker stack deploy (ingress publishes on all addresses); dropped", m["target"])
			}
			delete(m, k)
		}
	}
}

func secretTarget(v any) string {
	switch e := v.(type) {
	case string:
		return e
	case map[string]any:
		t, _ := e["target"].(string)
		if t == "" {
			t, _ = e["source"].(string)
		}
		return strings.TrimPrefix(t, "/run/secrets/")
	}
	return ""
}

func secretSource(v any) string {
	switch e := v.(type) {
	case string:
		return e
	case map[string]any:
		s, _ := e["source"].(string)
		return s
	}
	return ""
}

func setDefault(m map[string]any, k string, v any) {
	if _, ok := m[k]; !ok {
		m[k] = v
	}
}

// mapOf returns m[k] as a map, or nil.
func mapOf(m map[string]any, k string) map[string]any {
	if m == nil {
		return nil
	}
	v, _ := m[k].(map[string]any)
	return v
}

// ensureMap returns m[k] as a map, creating it when missing or null.
func ensureMap(m map[string]any, k string) map[string]any {
	if v, ok := m[k].(map[string]any); ok {
		return v
	}
	v := map[string]any{}
	m[k] = v
	return v
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
