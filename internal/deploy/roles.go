package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path"
	"slices"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/roles"
	"github.com/yoho-build/yoho/internal/secrets"
)

// Roles orchestrates single-Server Compose projects under one rollout.
type Roles struct{ LockHeld bool }

// RolesFailure preserves the mixed-Server result for automation.
type RolesFailure struct {
	Err                error
	Changed, Unchanged []string
}

func (e *RolesFailure) Error() string { return "Roles rollout: " + e.Err.Error() }
func (e *RolesFailure) Unwrap() error { return e.Err }

func (r Roles) lock(ctx context.Context, d *plan.Deploy) (func() error, error) {
	if r.LockHeld {
		return func() error { return nil }, nil
	}
	u, e := AcquireLocks(ctx, d.Servers, d.App, d.Destination, LockInfo{Performer: d.Performer, Version: d.Version, Command: "roles rollout"}, nil)
	if e != nil {
		return nil, e
	}
	return func() error { return u(context.WithoutCancel(ctx)) }, nil
}

// generatedOwner prefers the first sorted Stateful consumer, then Server name.
func generatedOwner(d *plan.Deploy, key string) string {
	var consumers []string
	for _, svc := range sortedKeys(d.Ext) {
		ext := d.Ext[svc]
		uses := false
		for _, ref := range ext.Secrets {
			uses = uses || ref.Key == key
		}
		_, declares := ext.Generate[key]
		if uses || declares {
			hs := d.Roles.ServiceServers(svc)
			consumers = append(consumers, hs...)
			if ext.Stateful {
				if len(hs) > 0 {
					return hs[0]
				}
			}
		}
	}
	slices.Sort(consumers)
	if len(consumers) > 0 {
		return consumers[0]
	}
	return ""
}

func rolesGenerated(ctx context.Context, d *plan.Deploy, parts map[string]*plan.Deploy, write bool) ([]string, error) {
	kinds, e := generatedDecls(d)
	if e != nil {
		return nil, e
	}
	owners := maps.Clone(d.GeneratedOwners)
	if owners == nil {
		owners = map[string]string{}
	}
	dir := release.AppDir(d.App, d.Destination)
	for _, h := range d.Servers {
		catalog, e := h.Host.ReadFile(ctx, path.Join(dir, "generated-owners.json"), false)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
		if e == nil {
			var pinned map[string]string
			if e = json.Unmarshal(catalog, &pinned); e != nil {
				return nil, fmt.Errorf("invalid generated-secret ownership catalog: %w", e)
			}
			for key, owner := range pinned {
				if prev := owners[key]; prev != "" && prev != owner {
					return nil, fmt.Errorf("conflicting pinned owner for generated secret %s", key)
				}
				owners[key] = owner
			}
		}
		rel, e := CurrentRelease(ctx, h.Host, d.App, d.Destination)
		if e != nil {
			return nil, e
		}
		if rel != nil {
			for key, owner := range rel.GeneratedOwners {
				if prev := owners[key]; prev != "" && prev != owner {
					return nil, fmt.Errorf("conflicting pinned owner for generated secret %s", key)
				}
				owners[key] = owner
			}
		}
	}
	var values []string
	for _, key := range sortedKeys(kinds) {
		owner := owners[key]
		pinned := owner != ""
		// Adopt an existing value from Releases made before ownership metadata.
		if owner == "" {
			var existing string
			for _, name := range sortedKeys(parts) {
				b, e := parts[name].Servers[0].Host.ReadFile(ctx, path.Join(dir, "generated", key), false)
				if e != nil && !errors.Is(e, os.ErrNotExist) {
					return values, e
				}
				if len(b) > 0 {
					if existing != "" && existing != string(b) {
						return values, fmt.Errorf("generated secret %s differs across Servers; reconcile its existing owner explicitly", key)
					}
					existing = string(b)
					if owner == "" {
						owner = name
					}
				}
			}
			if owner == "" {
				owner = generatedOwner(d, key)
			}
			owners[key] = owner
		}
		op := parts[owner]
		if op == nil {
			return values, fmt.Errorf("pinned generated secret %s owner Server %s is absent; restore it before applying", key, owner)
		}
		var value string
		if pinned {
			b, e := op.Servers[0].Host.ReadFile(ctx, path.Join(dir, "generated", key), false)
			if e != nil || len(b) == 0 {
				return values, fmt.Errorf("pinned generated secret %s is missing on %s; restore it before applying", key, owner)
			}
		}
		if write {
			v, e := ensureGenerated(ctx, op.Servers[0].Host, dir, map[string]string{key: kinds[key]})
			if e != nil {
				return values, e
			}
			value = v[key]
		} else {
			b, e := op.Servers[0].Host.ReadFile(ctx, path.Join(dir, "generated", key), false)
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return values, e
			}
			value = string(b)
			if value == "" {
				value = "\x00will-be-generated:" + key
			}
		}
		values = append(values, value)
		for _, p := range parts {
			used := false
			for svc, ext := range p.Ext {
				m := p.ServiceSecrets[svc]
				if m == nil {
					m = map[string]string{}
				}
				for _, ref := range ext.Secrets {
					if ref.Key == key {
						used = true
						if _, ok := m[ref.Name]; !ok {
							m[ref.Name] = value
							if p.SecretRefs == nil {
								p.SecretRefs = map[string]string{}
							}
							if p.SecretRefs[key] == "" {
								p.SecretRefs[key] = "generated"
							}
						}
					}
				}
				p.ServiceSecrets[svc] = m
			}
			if used && write && p.Servers[0].Name != owner {
				if e := p.Servers[0].Host.WriteFile(ctx, path.Join(dir, "generated", key), []byte(value), 0600, false); e != nil {
					return values, e
				}
			}
		}
	}
	if write {
		catalog, e := json.Marshal(owners)
		if e != nil {
			return values, e
		}
		for _, p := range parts {
			if e = p.Servers[0].Host.WriteFile(ctx, path.Join(dir, "generated-owners.json"), catalog, 0600, false); e != nil {
				return values, e
			}
		}
	}
	d.GeneratedOwners = owners
	for _, p := range parts {
		p.GeneratedOwners = maps.Clone(owners)
		for svc, ext := range p.Ext {
			ext.Generate = nil
			p.Ext[svc] = ext
		}
		if !write {
			continue
		}
		for key, owner := range owners {
			if !secretNameRe.MatchString(key) {
				return values, fmt.Errorf("invalid stored generated secret name")
			}
			used := false
			for _, ext := range p.Ext {
				for _, ref := range ext.Secrets {
					used = used || ref.Key == key
				}
			}
			used = used && kinds[key] != ""
			if !used && p.Servers[0].Name != owner {
				if e := p.Servers[0].Host.Run(ctx, remote.Cmd{Script: "set -eu\nrm -f -- " + remote.Quote(path.Join(dir, "generated", key))}); e != nil {
					return values, e
				}
			}
		}
	}
	return values, nil
}

func roleParts(d *plan.Deploy) (map[string]*plan.Deploy, []string, error) {
	order, err := roles.Order(d.Roles)
	if err != nil {
		return nil, nil, err
	}
	parts := map[string]*plan.Deploy{}
	for _, name := range order {
		p, e := roles.Render(d, d.Roles, name, d.PrivateAddresses)
		if e != nil {
			return nil, nil, e
		}
		if e = validate(p); e != nil {
			return nil, nil, e
		}
		parts[name] = p
	}
	return parts, order, nil
}

func (r Roles) Deploy(ctx context.Context, d *plan.Deploy) (rel *release.Release, err error) {
	parts, order, err := roleParts(d)
	if err != nil {
		return nil, err
	}
	unlock, err := r.lock(ctx, d)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	if e := checkRolesOwnership(ctx, d); e != nil {
		return nil, e
	}
	for _, server := range order {
		if e := (Compose{}).Preflight(ctx, parts[server]); e != nil {
			return nil, e
		}
	}
	if d.OnService != nil {
		for _, server := range order {
			for _, svc := range sortedKeys(parts[server].Project.Services) {
				d.OnService(ctx, server+"/"+svc, "unchanged")
			}
		}
	}
	// Every participant must actually own its effective private address.
	for _, h := range d.Servers {
		used := false
		for _, c := range d.Roles.Connections {
			used = used || slices.Contains(d.Roles.ServiceServers(c.Service), h.Name)
			for _, svc := range c.Consumers {
				used = used || slices.Contains(d.Roles.ServiceServers(svc), h.Name)
			}
		}
		if used {
			if e := roles.CheckAddress(ctx, h.Host, d.PrivateAddresses[h.Name]); e != nil {
				return nil, e
			}
		}
	}
	// Expand protection before rollout; retain stale ports until old containers
	// have been removed. An interrupted rollout therefore remains protected.
	for _, name := range order {
		p := parts[name]
		rules, e := roles.FirewallRules(d.App, d.Destination, name, d.Roles, d.PrivateAddresses)
		if e != nil {
			return nil, e
		}
		expanded, e := roles.ExpandProtection(ctx, p.Servers[0].Host, d.App, d.Destination, rules)
		if e != nil {
			return nil, e
		}
		if e = roles.Protect(ctx, p.Servers[0].Host, d.App, d.Destination, expanded); e != nil {
			return nil, e
		}
	}

	values, err := rolesGenerated(ctx, d, parts, true)
	if err != nil {
		return nil, err
	}
	defer func() { err = secrets.RedactError(err, values...) }()
	var changed []string
	active := ""
	fail := func(e error) (*release.Release, error) {
		var detail *Failure
		if active != "" && errors.As(e, &detail) {
			for _, sf := range detail.Services {
				if sf.ProxyRoute == "switched" || sf.ProxyRoute == "not-applicable" && (sf.Convergence == "complete" || sf.Convergence == "ready") {
					changed = append(changed, active+"/"+sf.Service)
				}
			}
		}
		var unchanged []string
		for _, n := range order {
			for svc := range parts[n].Project.Services {
				key := n + "/" + svc
				if !slices.Contains(changed, key) {
					unchanged = append(unchanged, key)
				}
			}
		}
		return nil, &RolesFailure{e, slices.Clone(changed), unchanged}
	}
	if err = RunHook(ctx, d, d.Version, "pre-deploy", nil); err != nil {
		return fail(err)
	}
	if err = RunHook(ctx, d, d.Version, "pre-app-boot", nil); err != nil {
		return fail(err)
	}
	if e := rolesReleasePhase(ctx, d, parts); e != nil {
		return fail(e)
	}
	for _, name := range order {
		active = name
		p := parts[name]
		p.SkipReleaseCommands = true
		if len(d.Restart) > 0 {
			base, e := CurrentRelease(ctx, p.Servers[0].Host, d.App, d.Destination)
			if e != nil {
				return fail(e)
			}
			p.RestartFrom = base
		}
		p.Hook = func(ctx context.Context, hook string, env map[string]string) error {
			if hook == "pre-proxy-reboot" || hook == "post-proxy-reboot" {
				return RunHook(ctx, d, d.Version, hook, env)
			}
			return nil
		}
		for cn, c := range d.Roles.Connections {
			consumes := false
			for _, svc := range c.Consumers {
				_, ok := p.Project.Services[svc]
				consumes = consumes || ok
			}
			if !consumes {
				continue
			}
			for _, provider := range d.Roles.ServiceServers(c.Service) {
				if provider == name {
					continue
				}
				if e := roles.Ready(ctx, p.Servers[0].Host, d.PrivateAddresses[provider], c.Port); e != nil {
					return fail(stoppedOr(ctx, partHost(parts, provider), d, cn, c.Service, provider, fmt.Errorf("connection %s from %s: %w", c.Service, name, e)))
				}
			}
		}

		out := p.Out
		if out == nil {
			out = io.Discard
		}
		red := secrets.NewRedactor(out, values...)
		p.Out = red
		// Keep Server identity in the existing operation's Service state map.
		if d.OnService != nil {
			p.OnService = func(ctx context.Context, svc, state string) { d.OnService(ctx, name+"/"+svc, state) }
		}
		rel, err = (Compose{LockHeld: true}).Deploy(ctx, p)
		red.Flush()
		if err != nil {
			return fail(err)
		}
		for svc := range p.Project.Services {
			changed = append(changed, name+"/"+svc)
		}
	}
	// All old placements have now been removed; retire stale protection.
	for _, name := range order {
		rules, e := roles.FirewallRules(d.App, d.Destination, name, d.Roles, d.PrivateAddresses)
		if e != nil {
			return fail(e)
		}
		if e = roles.Protect(ctx, parts[name].Servers[0].Host, d.App, d.Destination, rules); e != nil {
			return fail(e)
		}
	}
	if err = RunHook(ctx, d, d.Version, "post-app-boot", nil); err != nil {
		return fail(err)
	}
	if err = RunHook(ctx, d, d.Version, "post-deploy", nil); err != nil {
		return fail(err)
	}
	if rel == nil {
		return nil, fmt.Errorf("Roles placement has no Services")
	}
	return rel, nil
}

func (Roles) Diff(ctx context.Context, d *plan.Deploy) ([]plan.Change, error) {
	if e := checkRolesOwnership(ctx, d); e != nil {
		return nil, e
	}
	parts, order, err := roleParts(d)
	if err != nil {
		return nil, err
	}
	if _, err = rolesGenerated(ctx, d, parts, false); err != nil {
		return nil, err
	}
	var out []plan.Change
	for _, name := range order {
		p := parts[name]
		changes, e := (Compose{}).Diff(ctx, p)
		if e != nil {
			return nil, e
		}
		out = append(out, changes...)
		rules, e := roles.FirewallRules(d.App, d.Destination, name, d.Roles, d.PrivateAddresses)
		if e != nil {
			return nil, e
		}
		persisted, e := p.Servers[0].Host.ReadFile(ctx, roles.FirewallPath(d.App, d.Destination), true)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return nil, e
		}
		if len(rules) > 0 || e == nil {
			action := plan.ActionNoop
			if string(persisted) != roles.FirewallScript(d.App, d.Destination, rules) || roles.CheckFirewall(ctx, p.Servers[0].Host, rules) != nil {
				action = plan.ActionUpdate
			}
			reasons := []string{"reconcile private connection ports and consumer firewall allowlist"}
			if len(rules) == 0 {
				reasons = []string{"remove stale Yoho-owned connection firewall rules"}
			}
			for _, n := range sortedKeys(d.Roles.Connections) {
				c := d.Roles.Connections[n]
				hs := d.Roles.ServiceServers(c.Service)
				switch {
				case c.Instances && slices.Contains(hs, name):
					reasons = append(reasons, fmt.Sprintf("connection %s: publish %s:%d:%d/tcp (instance %s)", n, d.PrivateAddresses[name], c.Port, c.Port, config.InstanceName(name, c.Service)))
				case len(hs) == 1 && hs[0] == name:
					reasons = append(reasons, fmt.Sprintf("connection %s: publish %s:%d:%d/tcp", n, d.PrivateAddresses[name], c.Port, c.Port))
				}
			}
			out = append(out, plan.Change{Kind: "firewall", Name: d.App + "/" + d.Destination, Server: name, Action: action, Reasons: reasons})
		}
	}
	return annotateProviderSwitch(ctx, d, out)
}

func (r Roles) Rollback(ctx context.Context, d *plan.Deploy, version string) (rel *release.Release, err error) {
	unlock, err := r.lock(ctx, d)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	hosts := map[string]plan.NamedHost{}
	for _, h := range d.Servers {
		hosts[h.Name] = h
	}
	previous := d.Roles
	metadata := ""
	savedServices := map[string][]string{}
	for i, h := range d.Servers {
		old, e := readRelease(ctx, h.Host, path.Join(release.Dir(d.App, d.Destination, version), "release.json"))
		if e != nil {
			return nil, fmt.Errorf("Server %s lacks target Release %s; rollback requires the target Server inventory: %w", h.Name, version, e)
		}
		if old.Roles == nil {
			return nil, fmt.Errorf("Release %s predates Roles on %s; migrate explicitly", version, h.Name)
		}
		if old.Status != "deployed" {
			return nil, fmt.Errorf("Release %s on %s is not a successful Release", version, h.Name)
		}
		meta, e := json.Marshal([]any{old.Roles, old.Connections, old.GeneratedOwners})
		if e != nil {
			return nil, e
		}
		if metadata != "" && metadata != string(meta) {
			return nil, fmt.Errorf("target Release placement metadata differs across Servers; rollback refused")
		}
		metadata = string(meta)
		for _, owner := range old.GeneratedOwners {
			if hosts[owner].Host == nil {
				return nil, fmt.Errorf("target Release requires missing generated-secret owner Server %s", owner)
			}
		}
		for _, role := range old.Roles {
			for _, server := range role.Servers {
				if hosts[server].Host == nil {
					return nil, fmt.Errorf("target Release requires missing Server %s; restore its Destination inventory before rollback", server)
				}
			}
		}
		if i == 0 {
			previous.Roles = old.Roles
			previous.Connections = old.Connections
		}
		savedPlans, e := h.Host.ReadFile(ctx, path.Join(release.Dir(d.App, d.Destination, version), "plan.json"), false)
		if e != nil {
			return nil, e
		}
		var recorded []servicePlan
		if e = json.Unmarshal(savedPlans, &recorded); e != nil {
			return nil, e
		}
		for _, sp := range recorded {
			savedServices[h.Name] = append(savedServices[h.Name], sp.Name)
		}
		saved, e := h.Host.ReadFile(ctx, path.Join(release.Dir(d.App, d.Destination, version), "compose.yaml"), false)
		if e != nil {
			return nil, e
		}
		if e = checkOwnership(ctx, h.Host, d.App, d.Destination, recorded); e != nil {
			return nil, e
		}
		subnet, exists, e := currentSubnet(ctx, h.Host, d.App, d.Destination)
		if e != nil {
			return nil, e
		}
		if e = checkSubnetChange(release.ProjectName(d.App, d.Destination), saved, subnet, exists); e != nil {
			return nil, e
		}
		stateful := map[string]bool{}
		for _, sp := range recorded {
			stateful[sp.Name] = sp.Stateful
		}
		cur, e := CurrentRelease(ctx, h.Host, d.App, d.Destination)
		if e != nil {
			return nil, e
		}
		if cur != nil {
			for svc := range old.Images {
				if stateful[svc] {
					if _, present := cur.Images[svc]; !present {
						return nil, fmt.Errorf("rollback would move Stateful Service %s; use service move", svc)
					}
				}
			}
		}
	}
	order, e := roles.Order(previous)
	if e != nil {
		return nil, e
	}
	for _, h := range d.Servers {
		used := false
		for _, c := range previous.Connections {
			used = used || slices.Contains(previous.ServiceServers(c.Service), h.Name)
			for _, svc := range c.Consumers {
				used = used || slices.Contains(previous.ServiceServers(svc), h.Name)
			}
		}
		if used {
			if e := roles.CheckAddress(ctx, h.Host, d.PrivateAddresses[h.Name]); e != nil {
				return nil, e
			}
		}
	}
	for _, name := range order {
		rules, e := roles.FirewallRules(d.App, d.Destination, name, previous, d.PrivateAddresses)
		if e != nil {
			return nil, e
		}
		expanded, e := roles.ExpandProtection(ctx, hosts[name].Host, d.App, d.Destination, rules)
		if e != nil {
			return nil, e
		}
		if e = roles.Protect(ctx, hosts[name].Host, d.App, d.Destination, expanded); e != nil {
			return nil, e
		}
	}
	if err = RunHook(ctx, d, version, "pre-deploy", nil); err != nil {
		return nil, err
	}
	if err = RunHook(ctx, d, version, "pre-app-boot", nil); err != nil {
		return nil, err
	}
	var changed []string
	failure := func(e error) *RolesFailure {
		var unchanged []string
		for server, svcs := range savedServices {
			for _, svc := range svcs {
				key := server + "/" + svc
				if !slices.Contains(changed, key) {
					unchanged = append(unchanged, key)
				}
			}
		}
		return &RolesFailure{Err: e, Changed: slices.Clone(changed), Unchanged: unchanged}
	}
	for _, name := range order {
		for cn, c := range previous.Connections {
			consumes := false
			for _, svc := range c.Consumers {
				consumes = consumes || slices.Contains(previous.ServiceServers(svc), name)
			}
			if !consumes {
				continue
			}
			for _, provider := range previous.ServiceServers(c.Service) {
				if provider == name {
					continue
				}
				if e := roles.Ready(ctx, hosts[name].Host, d.PrivateAddresses[provider], c.Port); e != nil {
					return nil, failure(stoppedOr(ctx, hosts[provider].Host, d, cn, c.Service, provider, e))
				}
			}
		}
		p := *d
		p.Servers = []plan.NamedHost{hosts[name]}
		p.Hook = func(ctx context.Context, hook string, env map[string]string) error {
			if hook == "pre-proxy-reboot" || hook == "post-proxy-reboot" {
				return RunHook(ctx, d, version, hook, env)
			}
			return nil
		}
		p.Ext = maps.Clone(d.Ext)
		if d.OnService != nil {
			p.OnService = func(ctx context.Context, svc, state string) { d.OnService(ctx, name+"/"+svc, state) }
		}
		rel, err = (Compose{LockHeld: true}).Rollback(ctx, &p, version)
		if err != nil {
			return nil, failure(err)
		}
		for _, svc := range savedServices[name] {
			changed = append(changed, name+"/"+svc)
		}
	}
	for _, name := range order {
		rules, e := roles.FirewallRules(d.App, d.Destination, name, previous, d.PrivateAddresses)
		if e != nil {
			return nil, failure(e)
		}
		if e = roles.Protect(ctx, hosts[name].Host, d.App, d.Destination, rules); e != nil {
			return nil, failure(e)
		}
	}
	if err = RunHook(ctx, d, version, "post-app-boot", nil); err != nil {
		return nil, err
	}
	if err = RunHook(ctx, d, version, "post-deploy", nil); err != nil {
		return nil, err
	}
	return rel, nil
}
func (Roles) Releases(ctx context.Context, d *plan.Deploy) ([]release.Release, error) {
	var out []release.Release
	for _, h := range d.Servers {
		p := *d
		p.Servers = []plan.NamedHost{h}
		rels, e := (Compose{}).Releases(ctx, &p)
		if e != nil {
			return nil, e
		}
		out = append(out, rels...)
	}
	slices.SortStableFunc(out, func(a, b release.Release) int {
		if !a.DeployedAt.Equal(b.DeployedAt) {
			if a.DeployedAt.After(b.DeployedAt) {
				return -1
			}
			return 1
		}
		if a.Server < b.Server {
			return -1
		}
		if a.Server > b.Server {
			return 1
		}
		return 0
	})
	return out, nil
}

// GeneratedOwner exposes deterministic shared-secret ownership to rotation.
func GeneratedOwner(d *plan.Deploy, key string) string {
	if owner := d.GeneratedOwners[key]; owner != "" {
		return owner
	}
	return generatedOwner(d, key)
}

func checkRolesOwnership(ctx context.Context, d *plan.Deploy) error {
	if !d.ServiceMove {
		for _, h := range d.Servers {
			cur, e := CurrentRelease(ctx, h.Host, d.App, d.Destination)
			if e != nil {
				return e
			}
			if cur == nil {
				continue
			}
			if cur.Roles != nil {
				previous := d.Roles
				previous.Roles = cur.Roles
				for svc, ext := range d.Ext {
					if ext.Stateful && !slices.Equal(previous.ServiceServers(svc), d.Roles.ServiceServers(svc)) {
						return fmt.Errorf("Stateful Service %s changed ownership; use yoho service move SERVICE --to SERVER", svc)
					}
				}
			}
			for svc, ext := range d.Ext {
				if !ext.Stateful {
					continue
				}
				if _, wasHere := cur.Images[svc]; wasHere && !slices.Contains(d.Roles.ServiceServers(svc), h.Name) {
					return fmt.Errorf("Stateful Service %s moved from %s; use yoho service move SERVICE --to SERVER", svc, h.Name)
				}
			}
		}
	}

	return nil
}
