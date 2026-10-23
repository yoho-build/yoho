package gate

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/config"
)

// Policy is what a scoped deploy may contain. A compose project is root on
// the Server (it can mount the Docker socket or /), so the Gate refuses
// everything that reaches outside the App's own containers, volumes and
// networks. It is an allowlist where compose allows one, a denylist of
// escape vectors elsewhere; an unknown escape is a Gate bug.
type Policy struct {
	// Project is the compose project name, release.ProjectName.
	Project string
	// Built is Service -> the image Yoho builds for it at this Version
	// (build.ImageName). Built Services must run exactly that image.
	Built map[string]string
	// Routes are the Proxy hostnames the operator allowed ("" is the
	// catch-all route). nil allows none.
	Routes []string
	// Reserved are repository prefixes of images Yoho builds for any App
	// ("yoho/", the Registry prefix): only built Services may run them, so
	// a Service cannot run another App's image that is already on the
	// Server.
	Reserved []string
	// Limits are the operator's bounds (WithDefaults applied).
	Limits Limits
}

// raw keys that make docker compose (or compose-go) read files on the
// Server; the CLI resolves them locally before sending.
var refusedRawService = []string{"extends", "env_file", "label_file", "build"}

// CheckRaw refuses compose features that read Server files when the project
// is loaded, before compose-go sees it.
func CheckRaw(raw []byte) error {
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("compose: %w", err)
	}
	var errs []error
	if _, ok := doc["include"]; ok {
		errs = append(errs, errors.New("include is not allowed with scoped access (the CLI sends one resolved project)"))
	}
	svcs, _ := doc["services"].(map[string]any)
	for _, name := range sortedKeys(svcs) {
		svc, _ := svcs[name].(map[string]any)
		for _, k := range refusedRawService {
			if _, ok := svc[k]; ok {
				errs = append(errs, fmt.Errorf("service %s: %s is not allowed with scoped access (it would read files on the Server)", name, k))
			}
		}
	}
	return errors.Join(errs...)
}

// Check returns every reason p (with its x-yoho ext) breaks the policy.
func (pol Policy) Check(p *types.Project, ext map[string]config.ServiceExt) error {
	pol.Limits = pol.Limits.WithDefaults()
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	if len(p.Services) == 0 && !pol.Limits.AllowEmpty {
		add("the project has no Services; deploying it would stop every container of the App (the operator can allow this with limits.allow_empty)")
	}
	for _, name := range sortedKeys(p.Services) {
		s := p.Services[name]
		svc := func(f string, a ...any) { add("service %s: "+f, append([]any{name}, a...)...) }
		pol.checkContainer(p, s.ContainerSpec, svc)
		for i, h := range s.PreStart {
			pol.checkContainer(p, h.ContainerSpec, func(f string, a ...any) {
				svc("pre_start[%d]: "+f, append([]any{i}, a...)...)
			})
		}
		for _, hooks := range [][]types.ServiceHook{s.PostStart, s.PreStop} {
			for _, h := range hooks {
				if h.Privileged {
					svc("privileged lifecycle hooks are not allowed")
				}
			}
		}
		if s.Build != nil {
			svc("build is not allowed; the CLI builds images before deploying")
		}
		if s.Provider != nil {
			svc("provider services run a plugin on the Server and are not allowed")
		}
		if s.Extends != nil {
			svc("extends is not allowed")
		}
		if len(s.ExternalLinks) > 0 {
			svc("external_links reach containers of other Apps and are not allowed")
		}
		if s.Net != "" {
			svc("net is not allowed")
		}
		if s.ContainerName != "" && !strings.HasPrefix(s.ContainerName, pol.Project+"-") {
			svc("container_name %q must start with %s-", s.ContainerName, pol.Project)
		}
		if s.Deploy != nil {
			for _, r := range []*types.Resource{s.Deploy.Resources.Limits, s.Deploy.Resources.Reservations} {
				if r != nil && len(r.Devices) > 0 {
					svc("deploy.resources devices are not allowed")
				}
				if r != nil && (r.Pids < 0 || r.Pids > pol.Limits.PidsLimit) {
					svc("deploy.resources pids %d is outside 1..%d", r.Pids, pol.Limits.PidsLimit)
				}
			}
		}
		if want, ok := pol.Built[name]; ok && s.Image != want {
			svc("image %q must be %s, the image Yoho built for this Version", s.Image, want)
		}
		for _, port := range s.Ports {
			pol.checkPort(port, svc)
		}
		pol.checkDeployResources(s, svc)
		pol.checkRoutes(ext[name], svc)
	}
	if n := replicas(p); n > pol.Limits.MaxContainers {
		add("the project runs %d containers, above the limit of %d", n, pol.Limits.MaxContainers)
	}
	for _, name := range sortedKeys(pol.Built) {
		if _, ok := p.Services[name]; !ok {
			add("built Service %s is not in the project", name)
		}
	}
	for _, k := range sortedKeys(p.Volumes) {
		v := p.Volumes[k]
		switch {
		case bool(v.External):
			add("volume %s: external volumes may belong to other Apps and are not allowed", k)
		case v.Driver != "" && v.Driver != "local":
			add("volume %s: driver %q is not allowed (local only)", k, v.Driver)
		case len(v.DriverOpts) > 0:
			add("volume %s: driver_opts are not allowed (a local volume with o=bind mounts any Server path)", k)
		case v.Name != "" && !strings.HasPrefix(v.Name, pol.Project+"_"):
			add("volume %s: name %q must start with %s_", k, v.Name, pol.Project)
		}
	}
	for _, k := range sortedKeys(p.Networks) {
		n := p.Networks[k]
		switch {
		case bool(n.External):
			add("network %s: external network %q is not allowed (scoped Apps get their own network to the Proxy; shared networks reach other Apps)", k, n.Name)
		case n.Driver != "" && n.Driver != "bridge":
			add("network %s: driver %q is not allowed (bridge only)", k, n.Driver)
		case len(n.DriverOpts) > 0:
			add("network %s: driver_opts are not allowed", k)
		case n.Name != "" && !strings.HasPrefix(n.Name, pol.Project+"_"):
			add("network %s: name %q must start with %s_", k, n.Name, pol.Project)
		}
	}
	for _, k := range sortedKeys(p.Secrets) {
		add("secret %s: top-level secrets are not allowed; declare them in x-yoho.secrets (the operator pushes the values)", k)
	}
	for _, k := range sortedKeys(p.Configs) {
		c := p.Configs[k]
		if c.File != "" || c.Environment != "" || bool(c.External) || c.Driver != "" {
			add("config %s: only inline content is allowed (file, environment, external and drivers read the Server)", k)
		}
	}
	if len(p.Models) > 0 {
		add("models are not allowed")
	}
	return errors.Join(errs...)
}

// allowedSecurityOpt are the only security_opt entries a scoped deploy may
// set: seccomp=FILE reads the Server, unconfined and label=disable drop
// isolation.
var allowedSecurityOpt = []string{"no-new-privileges", "no-new-privileges:true", "no-new-privileges=true"}

// checkContainer applies the container rules to a Service or a pre_start
// init container.
func (pol Policy) checkContainer(p *types.Project, c types.ContainerSpec, add func(string, ...any)) {
	if c.Image != "" && !pol.built(c.Image) {
		for _, r := range pol.Reserved {
			if HasRepoPrefix(c.Image, r) {
				add("image %s is a Yoho-built image (%s...) that this deploy did not build; only built Services may run one", c.Image, r)
			}
		}
	}
	pol.checkResources(c, add)
	if c.Privileged {
		add("privileged is not allowed")
	}
	if c.OomScoreAdj < 0 || c.OomKillDisable {
		add("oom_score_adj below 0 and oom_kill_disable are not allowed (they shield the container from the OOM killer at the Server's expense)")
	}
	if c.UseAPISocket {
		add("use_api_socket mounts the Docker socket and is not allowed")
	}
	if len(c.CapAdd) > 0 {
		add("cap_add is not allowed (%s)", strings.Join(c.CapAdd, ", "))
	}
	if len(c.Devices) > 0 {
		add("devices are not allowed")
	}
	if len(c.DeviceCgroupRules) > 0 {
		add("device_cgroup_rules are not allowed")
	}
	if len(c.Gpus) > 0 {
		add("gpus are not allowed")
	}
	for _, o := range c.SecurityOpt {
		if !slices.Contains(allowedSecurityOpt, strings.TrimSpace(o)) {
			add("security_opt %q is not allowed (only no-new-privileges)", o)
		}
	}
	switch nm := c.NetworkMode; nm {
	case "", "none":
	default:
		add("network_mode %q is not allowed (bridge and default join Docker's shared bridge, host, service: and container: leave the App's networks; omit it to use the App's own networks)", nm)
	}
	switch c.PullPolicy {
	case "", types.PullPolicyMissing, types.PullPolicyIfNotPresent, types.PullPolicyNever:
	default:
		add("pull_policy %q is not allowed (the Gate pulls images itself before the deploy)", c.PullPolicy)
	}
	if len(c.StorageOpt) > 0 {
		add("storage_opt is not allowed")
	}
	if c.Pid != "" {
		add("pid %q is not allowed", c.Pid)
	}
	switch c.Ipc {
	case "", "private", "shareable", "none":
	default:
		add("ipc %q is not allowed", c.Ipc)
	}
	if c.Uts != "" {
		add("uts %q is not allowed", c.Uts)
	}
	if c.UserNSMode != "" {
		add("userns_mode %q is not allowed", c.UserNSMode)
	}
	if c.Cgroup != "" && c.Cgroup != "private" {
		add("cgroup %q is not allowed", c.Cgroup)
	}
	if c.CgroupParent != "" {
		add("cgroup_parent is not allowed")
	}
	if c.Runtime != "" {
		add("runtime %q is not allowed", c.Runtime)
	}
	if c.Isolation != "" && c.Isolation != "default" {
		add("isolation %q is not allowed", c.Isolation)
	}
	if c.CredentialSpec != nil {
		add("credential_spec is not allowed")
	}
	if c.VolumeDriver != "" {
		add("volume_driver is not allowed")
	}
	if len(c.VolumesFrom) > 0 {
		add("volumes_from is not allowed")
	}
	if len(c.EnvFiles) > 0 {
		add("env_file is not allowed (the CLI inlines it into environment)")
	}
	if len(c.LabelFiles) > 0 {
		add("label_file is not allowed (the CLI inlines it into labels)")
	}
	for k := range c.Labels {
		if reservedLabel(k) {
			add("label %s is reserved for Docker and Yoho", k)
		}
	}
	for _, v := range c.Volumes {
		switch v.Type {
		case types.VolumeTypeVolume:
			if v.Source == "" {
				continue // anonymous volume
			}
			if _, ok := p.Volumes[v.Source]; !ok {
				add("volume %s is not declared in the project", v.Source)
			}
		case types.VolumeTypeTmpfs: // sizes: checkResources
		case types.VolumeTypeImage:
			if !pol.built(v.Source) {
				add("image volume %s: only images built in this deploy may be mounted (%s is not)", v.Target, v.Source)
			}
		case types.VolumeTypeBind:
			add("bind mount %s:%s is not allowed with scoped access; use a named volume or bake the files into the image", v.Source, v.Target)
		default:
			add("%s mount %s is not allowed", v.Type, v.Target)
		}
	}
	for _, s := range c.Secrets {
		if _, ok := p.Secrets[s.Source]; !ok {
			add("secret %s is not declared in the project", s.Source)
		}
	}
}

// Harden applies the Gate's defaults to every container of p after Check:
// NET_RAW is dropped (no raw sockets: no ARP or ICMP tricks on the App's
// networks), pull_policy is never (the Gate pulls first), containers
// without a pids limit get Limits.PidsLimit, Services without a memory or
// CPU limit get Limits.MaxMemoryBytes and MaxCPUs, and unsized tmpfs
// mounts get Limits.MaxTmpfsBytes.
func (pol Policy) Harden(p *types.Project) {
	pol.Limits = pol.Limits.WithDefaults()
	for name, s := range p.Services {
		var lim *types.Resource
		if s.Deploy != nil {
			lim = s.Deploy.Resources.Limits
		}
		if lim != nil && lim.Pids != 0 && s.PidsLimit == 0 {
			s.PidsLimit = lim.Pids
		}
		pol.harden(&s.ContainerSpec)
		if s.MemLimit == 0 && lim != nil {
			s.MemLimit = lim.MemoryBytes
		}
		if s.MemLimit == 0 {
			s.MemLimit = types.UnitBytes(pol.Limits.MaxMemoryBytes)
		}
		if s.CPUS == 0 && lim != nil {
			s.CPUS = lim.NanoCPUs.Value()
		}
		if s.CPUS == 0 {
			s.CPUS = float32(pol.Limits.MaxCPUs)
		}
		// compose refuses top-level and deploy limits that differ.
		if lim != nil {
			lim.MemoryBytes, lim.NanoCPUs, lim.Pids = s.MemLimit, types.NanoCPUs(s.CPUS), s.PidsLimit
		}
		for i := range s.PreStart {
			c := &s.PreStart[i].ContainerSpec
			pol.harden(c)
			if c.MemLimit == 0 {
				c.MemLimit = types.UnitBytes(pol.Limits.MaxMemoryBytes)
			}
			if c.CPUS == 0 {
				c.CPUS = float32(pol.Limits.MaxCPUs)
			}
		}
		p.Services[name] = s
	}
}

func (pol Policy) harden(c *types.ContainerSpec) {
	if !slices.ContainsFunc(c.CapDrop, func(x string) bool {
		x = strings.ToUpper(x)
		return x == "NET_RAW" || x == "CAP_NET_RAW" || x == "ALL"
	}) {
		c.CapDrop = append(c.CapDrop, "NET_RAW")
	}
	if c.PidsLimit == 0 {
		c.PidsLimit = pol.Limits.PidsLimit
	}
	// Images are on the Server before compose runs (the Gate pulled them).
	c.PullPolicy = types.PullPolicyNever
	for i, t := range c.Tmpfs {
		if size, ok := tmpfsSize(t); ok && size == 0 {
			sep := ":"
			if strings.Contains(t, ":") {
				sep = ","
			}
			c.Tmpfs[i] = t + sep + "size=" + strconv.FormatInt(pol.Limits.MaxTmpfsBytes, 10)
		}
	}
	for i, v := range c.Volumes {
		if v.Type != types.VolumeTypeTmpfs {
			continue
		}
		if v.Tmpfs == nil {
			v.Tmpfs = &types.ServiceVolumeTmpfs{}
		}
		if v.Tmpfs.Size == 0 {
			v.Tmpfs.Size = types.UnitBytes(pol.Limits.MaxTmpfsBytes)
		}
		c.Volumes[i] = v
	}
}

// CheckCompiled checks the compiled compose of a Release (what the runtime
// runs on rollback) against today's limits: the values Harden set when it
// was deployed must still be present and within bounds.
func (pol Policy) CheckCompiled(p *types.Project) error {
	pol.Limits = pol.Limits.WithDefaults()
	var errs []error
	add := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }
	hardened := func(c types.ContainerSpec, add func(string, ...any)) {
		pol.checkResources(c, add)
		if c.PidsLimit == 0 {
			add("no pids_limit")
		}
		if c.PullPolicy != types.PullPolicyNever {
			add("pull_policy is not never")
		}
		if !slices.ContainsFunc(c.CapDrop, func(x string) bool {
			x = strings.ToUpper(x)
			return x == "NET_RAW" || x == "CAP_NET_RAW" || x == "ALL"
		}) {
			add("NET_RAW is not dropped")
		}
		if c.NetworkMode != "" && c.NetworkMode != "none" {
			add("network_mode %q", c.NetworkMode)
		}
		for _, t := range c.Tmpfs {
			if size, _ := tmpfsSize(t); size == 0 {
				add("tmpfs %s has no size", t)
			}
		}
		for _, v := range c.Volumes {
			if v.Type == types.VolumeTypeTmpfs && (v.Tmpfs == nil || v.Tmpfs.Size == 0) {
				add("tmpfs %s has no size", v.Target)
			}
		}
	}
	for _, name := range sortedKeys(p.Services) {
		s := p.Services[name]
		svc := func(f string, a ...any) { add("service %s: "+f, append([]any{name}, a...)...) }
		hardened(s.ContainerSpec, svc)
		for i, h := range s.PreStart {
			pre := func(f string, a ...any) { svc("pre_start[%d]: "+f, append([]any{i}, a...)...) }
			hardened(h.ContainerSpec, pre)
			if h.MemLimit == 0 {
				pre("no memory limit")
			}
			if h.CPUS == 0 {
				pre("no CPU limit")
			}
		}
		var lim *types.Resource
		if s.Deploy != nil {
			lim = s.Deploy.Resources.Limits
		}
		if s.MemLimit == 0 && (lim == nil || lim.MemoryBytes == 0) {
			svc("no memory limit")
		}
		if s.CPUS == 0 && (lim == nil || lim.NanoCPUs == 0) {
			svc("no CPU limit")
		}
		pol.checkDeployResources(s, svc)
	}
	if n := replicas(p); n > pol.Limits.MaxContainers {
		add("the Release runs %d containers, above the limit of %d", n, pol.Limits.MaxContainers)
	}
	return errors.Join(errs...)
}

// built reports whether ref is an image built in this deploy.
func (pol Policy) built(ref string) bool {
	n := NormalizeRef(ref)
	for _, b := range pol.Built {
		if NormalizeRef(b) == n {
			return true
		}
	}
	return false
}

// checkPort allows only host ports the operator listed.
func (pol Policy) checkPort(port types.ServicePortConfig, add func(string, ...any)) {
	pub := port.Published
	if pub == "" {
		add("ports: publishing container port %d on a random host port is not allowed; use the Proxy, or ask the operator to allow a host port", port.Target)
		return
	}
	lo, hi, ok := strings.Cut(pub, "-")
	if !ok {
		hi = lo
	}
	a, err1 := strconv.Atoi(lo)
	b, err2 := strconv.Atoi(hi)
	if err1 != nil || err2 != nil || a > b {
		add("ports: published %q is not a port", pub)
		return
	}
	for n := a; n <= b; n++ {
		if !slices.Contains(pol.Limits.Ports, n) {
			add("ports: host port %d is not allowed for this App (the operator allows: %s); use the Proxy instead", n, portList(pol.Limits.Ports))
			return
		}
	}
}

func portList(ports []int) string {
	if len(ports) == 0 {
		return "none"
	}
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ", ")
}

// checkResources bounds shm, tmpfs, ulimits and pids.
func (pol Policy) checkResources(c types.ContainerSpec, add func(string, ...any)) {
	l := pol.Limits
	if int64(c.ShmSize) > l.MaxShmBytes {
		add("shm_size %d is above the limit of %d bytes", c.ShmSize, l.MaxShmBytes)
	}
	for _, v := range c.Volumes {
		if v.Type == types.VolumeTypeTmpfs && v.Tmpfs != nil && (v.Tmpfs.Size < 0 || int64(v.Tmpfs.Size) > l.MaxTmpfsBytes) {
			add("tmpfs %s: size %d is above the limit of %d bytes", v.Target, v.Tmpfs.Size, l.MaxTmpfsBytes)
		}
	}
	for _, t := range c.Tmpfs {
		if size, ok := tmpfsSize(t); ok && size > l.MaxTmpfsBytes {
			add("tmpfs %s: size is above the limit of %d bytes", t, l.MaxTmpfsBytes)
		} else if !ok {
			add("tmpfs %s: bad size option", t)
		}
	}
	for _, k := range sortedKeys(c.Ulimits) {
		u := c.Ulimits[k]
		if u == nil {
			continue
		}
		for _, v := range []int{u.Single, u.Soft, u.Hard} {
			if v < 0 || int64(v) > l.MaxUlimit {
				add("ulimit %s: %d is outside 0..%d", k, v, l.MaxUlimit)
				break
			}
		}
	}
	if c.PidsLimit < 0 || c.PidsLimit > l.PidsLimit {
		add("pids_limit %d is outside 1..%d", c.PidsLimit, l.PidsLimit)
	}
	if c.MemLimit < 0 || int64(c.MemLimit) > l.MaxMemoryBytes {
		add("mem_limit %d is above the limit of %d bytes", c.MemLimit, l.MaxMemoryBytes)
	}
	if c.MemSwapLimit != 0 {
		add("memswap_limit is not allowed (swap follows mem_limit)")
	}
	if c.CPUS < 0 || cpusAbove(float64(c.CPUS), l.MaxCPUs) {
		add("cpus %g is above the limit of %g", c.CPUS, l.MaxCPUs)
	}
	if c.CPUQuota != 0 || c.CPUPeriod != 0 || c.CPURTPeriod != 0 || c.CPURTRuntime != 0 {
		add("cpu_quota, cpu_period and cpu_rt_* are not allowed (use cpus)")
	}
}

// cpusAbove compares CPU counts in thousandths: compose stores cpus as
// float32, so 0.1 must not read as above a limit of 0.1.
func cpusAbove(v, max float64) bool { return math.Round(v*1000) > math.Round(max*1000) }

// checkDeployResources bounds deploy.resources.limits and replicas.
func (pol Policy) checkDeployResources(s types.ServiceConfig, add func(string, ...any)) {
	l := pol.Limits
	if s.Deploy != nil && s.Deploy.Resources.Limits != nil {
		r := s.Deploy.Resources.Limits
		if r.MemoryBytes < 0 || int64(r.MemoryBytes) > l.MaxMemoryBytes {
			add("deploy.resources.limits.memory %d is above the limit of %d bytes", r.MemoryBytes, l.MaxMemoryBytes)
		}
		if r.NanoCPUs < 0 || cpusAbove(float64(r.NanoCPUs.Value()), l.MaxCPUs) {
			add("deploy.resources.limits.cpus %g is above the limit of %g", r.NanoCPUs, l.MaxCPUs)
		}
	}
	if n := s.GetScale(); n > l.MaxReplicas {
		add("%d replicas is above the limit of %d", n, l.MaxReplicas)
	}
}

// replicas counts the containers p runs (scale 0 runs none).
func replicas(p *types.Project) int {
	n := 0
	for _, s := range p.Services {
		n += max(s.GetScale(), 0)
	}
	return n
}

// tmpfsSize reads the size= option of a short tmpfs entry ("/run:size=64m,
// mode=1777"); 0 when absent. ok is false for a size it cannot read.
func tmpfsSize(t string) (int64, bool) {
	_, opts, _ := strings.Cut(t, ":")
	for _, o := range strings.Split(opts, ",") {
		v, ok := strings.CutPrefix(strings.TrimSpace(o), "size=")
		if !ok {
			continue
		}
		var b types.UnitBytes
		if err := b.DecodeMapstructure(v); err != nil {
			return 0, false
		}
		return int64(b), true
	}
	return 0, true
}

// reservedLabel: labels Docker, compose, swarm and Yoho set and read (an App
// could otherwise pose as another App's containers).
func reservedLabel(k string) bool {
	for _, p := range []string{"com.docker.", "yoho."} {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// checkRoutes refuses Proxy hostnames the operator did not allow.
func (pol Policy) checkRoutes(e config.ServiceExt, add func(string, ...any)) {
	if e.Proxy == nil {
		return
	}
	// On-demand TLS mints certificates for any hostname the allow check
	// approves, and the Proxy calls that endpoint: only the operator's
	// exact allow value is accepted, never a plain catch-all grant.
	if allow := e.Proxy.CustomDomainsAllow(); allow != "" {
		if !slices.Contains(pol.Routes, CustomDomainsRoute(allow)) {
			add("custom_domains with allow %s is not allowed for this App; the operator allows: %s", allow, routeList(pol.Routes))
		}
		return
	}
	hosts := e.Proxy.Hosts
	if len(hosts) == 0 {
		hosts = []string{""}
	}
	for _, h := range hosts {
		if slices.Contains(pol.Routes, strings.ToLower(h)) {
			continue
		}
		if h == "" {
			add("the catch-all Proxy route (x-yoho.proxy without hosts) is not allowed for this App")
		} else {
			add("Proxy host %s is not allowed for this App; the operator allows: %s", h, routeList(pol.Routes))
		}
	}
}

func routeList(r []string) string {
	if len(r) == 0 {
		return "none"
	}
	out := make([]string, len(r))
	for i, h := range r {
		// The allow endpoint may be an internal URL; Members see only
		// that the route exists.
		if strings.HasPrefix(h, customDomainsRoutePrefix) {
			h = customDomainsRoutePrefix + "(operator's allow)"
		}
		out[i] = RouteLabel(h)
	}
	return strings.Join(out, ", ")
}

// customDomainsRoutePrefix marks a custom_domains route in Settings.Routes.
const customDomainsRoutePrefix = "custom_domains:"

// CustomDomainsRoute is the Settings.Routes entry for a custom_domains
// route with this allow endpoint.
func CustomDomainsRoute(allow string) string { return customDomainsRoutePrefix + allow }

// RouteLabel is how a Settings.Routes entry reads in output.
func RouteLabel(r string) string {
	if r == "" {
		return "(catch-all)"
	}
	if allow, ok := strings.CutPrefix(r, customDomainsRoutePrefix); ok {
		return "custom domains via " + allow
	}
	return r
}

// Routes lists the Proxy hostnames of ext ("" for a catch-all route,
// CustomDomainsRoute for on-demand TLS), hostnames lowercased, sorted
// and unique.
func Routes(ext map[string]config.ServiceExt) []string {
	var out []string
	for _, e := range ext {
		if e.Proxy == nil {
			continue
		}
		if allow := e.Proxy.CustomDomainsAllow(); allow != "" {
			out = append(out, CustomDomainsRoute(allow))
			continue
		}
		if len(e.Proxy.Hosts) == 0 {
			out = append(out, "")
		}
		for _, h := range e.Proxy.Hosts {
			out = append(out, strings.ToLower(h))
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
