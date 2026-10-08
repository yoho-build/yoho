package composefile

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"
)

// Finding levels.
const (
	LevelError   = "error"
	LevelWarning = "warning"
)

// Finding is one problem found by Check.
type Finding struct {
	Level   string // error | warning
	Service string // empty for project-level findings
	Message string
}

func (f Finding) String() string {
	if f.Service != "" {
		return fmt.Sprintf("%s: service %s: %s", f.Level, f.Service, f.Message)
	}
	return fmt.Sprintf("%s: %s", f.Level, f.Message)
}

// HasErrors reports whether any finding is an error.
func HasErrors(fs []Finding) bool {
	for _, f := range fs {
		if f.Level == LevelError {
			return true
		}
	}
	return false
}

var generateKinds = map[string]bool{
	"password32": true, "password64": true, "hex32": true, "hex64": true,
	"base64_32": true, "base64_64": true,
}

// reInterp matches $KEY, ${KEY}, ${KEY:-d}, ${KEY?err} etc. "$$" is an escape
// and is consumed first so "$$KEY" is not a reference.
var reInterp = regexp.MustCompile(`\$\$|\$\{([A-Za-z_][A-Za-z0-9_]*)[^}]*\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// InterpolatedVars returns the variable names referenced in raw compose text.
func InterpolatedVars(raw []byte) []string {
	seen := map[string]bool{}
	for _, m := range reInterp.FindAllSubmatch(raw, -1) {
		name := string(m[1])
		if name == "" {
			name = string(m[2])
		}
		if name != "" {
			seen[name] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Check validates r for the given runtime ("compose" or "swarm"). secretKeys
// are the known secret keys (nil skips the unknown-secret check but keeps the
// interpolation scan, which then has nothing to match).
func Check(r *Result, runtime string, secretKeys []string) []Finding {
	if runtime == "" {
		runtime = "compose"
	}
	swarm := runtime == "swarm"
	var out []Finding
	add := func(level, svc, f string, a ...any) {
		out = append(out, Finding{Level: level, Service: svc, Message: fmt.Sprintf(f, a...)})
	}

	known := map[string]bool{}
	for _, k := range secretKeys {
		known[k] = true
	}

	// Secrets must travel as files/env_file, never through ${VAR}, or they
	// would be baked into the compiled compose and shown in `docker inspect`.
	for _, raw := range r.Raw {
		for _, v := range InterpolatedVars(raw) {
			if known[v] {
				add(LevelError, "", "${%s} interpolates a secret key; declare it in x-yoho.secrets instead", v)
			}
		}
	}
	for _, k := range r.EnvFileKeys {
		if known[k] {
			add(LevelWarning, "", ".env contains secret key %s; remove it, secrets belong in .yoho/secrets or a provider", k)
		}
	}

	if swarm {
		add(LevelWarning, "", "swarm: stack deploy ignores build; Yoho builds images first and deploys image references, so this is fine")
	}

	names := r.Project.ServiceNames()
	for _, name := range names {
		svc := r.Project.Services[name]
		ext := r.Ext[name]
		svcAdd := func(level, f string, a ...any) { add(level, name, f, a...) }

		if secretKeys != nil {
			for _, ref := range ext.Secrets {
				if !known[ref.Key] {
					svcAdd(LevelError, "x-yoho.secrets references unknown secret key %q", ref.Key)
				}
			}
		}
		for _, gname := range sortedMapKeys(ext.Generate) {
			if !generateKinds[ext.Generate[gname]] {
				svcAdd(LevelError, "x-yoho.generate.%s: unknown kind %q (use password32, password64, hex32, hex64, base64_32, base64_64)", gname, ext.Generate[gname])
			}
		}
		if ext.ReleaseCommand != nil && (len(ext.ReleaseCommand) == 0 || strings.TrimSpace(strings.Join(ext.ReleaseCommand, "")) == "") {
			svcAdd(LevelError, "x-yoho.release_command must not be empty")
		}
		if ext.Backup != nil {
			vols := map[string]bool{}
			for _, v := range svc.Volumes {
				if v.Type == types.VolumeTypeVolume {
					vols[v.Source] = true
				}
			}
			for _, bv := range ext.Backup.Volumes {
				if !vols[bv] {
					svcAdd(LevelError, "x-yoho.backup.volumes: %q is not a named volume of this service", bv)
				}
			}
		}
		if ext.StrictDrain && !swarm {
			svcAdd(LevelWarning, "x-yoho.strict_drain only applies to runtime swarm")
		}

		if ext.Proxy != nil {
			if len(svc.Ports) > 0 {
				svcAdd(LevelError, "proxied service must not publish ports; the Proxy owns host ports")
			}
			if svc.ContainerName != "" {
				svcAdd(LevelError, "proxied service must not set container_name; Yoho runs old and new containers side by side")
			}
			if ext.Stateful {
				svcAdd(LevelError, "stateful service cannot be proxied; it is recreated stop-first and has no zero-downtime cutover")
			}
			if svc.HealthCheck == nil || svc.HealthCheck.Disable {
				lvl := LevelWarning
				if swarm {
					lvl = LevelError
				}
				svcAdd(lvl, "proxied service has no healthcheck; cutover cannot tell when the new container is ready")
			}
		}

		// Stateful guard (ADR 0005): data must not silently diverge across Servers.
		if !ext.Stateful {
			if desc := stateDescription(svc, r.Project.WorkingDir); desc != "" {
				switch {
				case swarm && !hasPlacement(svc):
					svcAdd(LevelError, "%s but not marked x-yoho.stateful and has no deploy.placement.constraints; tasks could land on any node", desc)
				case !swarm:
					svcAdd(LevelWarning, "%s but not marked x-yoho.stateful; mark it stateful so it is recreated stop-first", desc)
				}
			}
		}

		checkBinds(svc, r.Project.WorkingDir, swarm, svcAdd)
		if swarm {
			checkSwarm(svc, svcAdd)
		}
	}
	return out
}

func checkSwarm(svc types.ServiceConfig, add func(level, f string, a ...any)) {
	var ignored []string
	flag := func(cond bool, key string) {
		if cond {
			ignored = append(ignored, key)
		}
	}
	flag(len(svc.Devices) > 0, "devices")
	flag(svc.NetworkMode != "", "network_mode")
	flag(svc.Privileged, "privileged")
	flag(len(svc.SecurityOpt) > 0, "security_opt")
	flag(svc.Restart != "", "restart (use deploy.restart_policy)")
	flag(svc.ShmSize != 0, "shm_size")
	flag(svc.Pid != "", "pid")
	flag(svc.Ipc != "", "ipc")
	flag(len(svc.Links) > 0, "links")
	flag(svc.ContainerName != "", "container_name")
	flag(len(svc.Expose) > 0, "expose")
	if len(ignored) > 0 {
		add(LevelWarning, "docker stack deploy ignores: %s", strings.Join(ignored, ", "))
	}

	var forbidden []string
	flag2 := func(cond bool, key string) {
		if cond {
			forbidden = append(forbidden, key)
		}
	}
	flag2(svc.Extends != nil, "extends")
	flag2(len(svc.VolumesFrom) > 0, "volumes_from")
	flag2(svc.VolumeDriver != "", "volume_driver")
	flag2(svc.CPUQuota != 0, "cpu_quota")
	flag2(svc.CPUShares != 0, "cpu_shares")
	flag2(svc.CPUSet != "", "cpuset")
	if len(forbidden) > 0 {
		add(LevelError, "docker stack deploy rejects: %s", strings.Join(forbidden, ", "))
	}
	if len(svc.DependsOn) > 0 {
		add(LevelWarning, "depends_on is ignored by stack deploy; services start in any order, make them retry")
	}
	if len(svc.Profiles) > 0 {
		add(LevelWarning, "profiles are not honored by stack deploy")
	}
}

func hasPlacement(svc types.ServiceConfig) bool {
	return svc.Deploy != nil && len(svc.Deploy.Placement.Constraints) > 0
}

// stateDescription names why a service looks stateful, or "".
// Writable binds inside the App directory are reported by checkBinds.
func stateDescription(svc types.ServiceConfig, appDir string) string {
	var named, binds []string
	for _, v := range svc.Volumes {
		switch v.Type {
		case types.VolumeTypeVolume:
			named = append(named, v.Source)
		case types.VolumeTypeBind:
			if _, in := InAppDir(appDir, v.Source); !v.ReadOnly && !in {
				binds = append(binds, v.Source)
			}
		}
	}
	switch {
	case len(named) > 0:
		return fmt.Sprintf("uses named volume(s) %s", strings.Join(named, ", "))
	case len(binds) > 0:
		return fmt.Sprintf("uses writable bind mount(s) %s", strings.Join(binds, ", "))
	}
	return ""
}

func sortedMapKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for n := range m {
		k = append(k, n)
	}
	sort.Strings(k)
	return k
}
