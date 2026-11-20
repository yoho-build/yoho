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

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
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
	hosts := slices.Clone(d.Servers)
	slices.SortFunc(hosts, func(a, b plan.NamedHost) int {
		if a.Name < b.Name {
			return -1
		}
		if a.Name > b.Name {
			return 1
		}
		return 0
	})
	var unlocks []func(context.Context) error
	done := func() error {
		var errs []error
		for i := len(unlocks) - 1; i >= 0; i-- {
			errs = append(errs, unlocks[i](context.WithoutCancel(ctx)))
		}
		return errors.Join(errs...)
	}
	for _, h := range hosts {
		u, err := AcquireLock(ctx, h.Host, d.App, d.Destination, LockInfo{Performer: d.Performer, Version: d.Version, Command: "roles rollout"})
		if err != nil {
			return nil, errors.Join(err, done())
		}
		unlocks = append(unlocks, u)
	}
	return done, nil
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
	kinds, err := generatedDecls(d)
	if err != nil {
		return nil, err
	}
	var values []string
	for _, key := range sortedKeys(kinds) {
		owner := parts[generatedOwner(d, key)]
		if owner == nil {
			return nil, fmt.Errorf("no owner for generated secret %s", key)
		}
		dir := release.AppDir(d.App, d.Destination)
		var value string
		if write {
			v, e := ensureGenerated(ctx, owner.Servers[0].Host, dir, map[string]string{key: kinds[key]})
			if e != nil {
				return values, e
			}
			value = v[key]
		} else {
			b, e := owner.Servers[0].Host.ReadFile(ctx, path.Join(dir, "generated", key), false)
			if e != nil && !errors.Is(e, os.ErrNotExist) {
				return values, e
			}
			value = string(b)
		}
		if value == "" {
			if write {
				continue
			}
			value = "\x00will-be-generated:" + key
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
						}
					}
				}
				if _, ok := ext.Generate[key]; ok {
					used = true
					if _, ok := m[key]; !ok {
						m[key] = value
					}
				}
				p.ServiceSecrets[svc] = m
			}
			if used && write && p.Servers[0].Name != owner.Servers[0].Name {
				if err := p.Servers[0].Host.WriteFile(ctx, path.Join(dir, "generated", key), []byte(value), 0600, false); err != nil {
					return values, err
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
	// Ordinary config edits must never silently create a second data copy.
	if !d.ServiceMove {
		for _, h := range d.Servers {
			cur, e := CurrentRelease(ctx, h.Host, d.App, d.Destination)
			if e != nil {
				return nil, e
			}
			if cur == nil {
				continue
			}
			if cur.Roles != nil {
				previous := d.Roles
				previous.Roles = cur.Roles
				for svc, ext := range d.Ext {
					if ext.Stateful && !slices.Equal(previous.ServiceServers(svc), d.Roles.ServiceServers(svc)) {
						return nil, fmt.Errorf("Stateful Service %s changed ownership; use yoho service move SERVICE --to SERVER", svc)
					}
				}
			}
			for svc, ext := range d.Ext {
				if !ext.Stateful {
					continue
				}
				if _, wasHere := cur.Images[svc]; wasHere && !slices.Contains(d.Roles.ServiceServers(svc), h.Name) {
					return nil, fmt.Errorf("Stateful Service %s moved from %s; use yoho service move SERVICE --to SERVER", svc, h.Name)
				}
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
	for _, name := range order {
		active = name
		p := parts[name]
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
		for _, c := range d.Roles.Connections {
			provider := d.Roles.ServiceServers(c.Service)[0]
			if provider == name {
				continue
			}
			for _, svc := range c.Consumers {
				if _, ok := p.Project.Services[svc]; ok {
					if e := roles.Ready(ctx, p.Servers[0].Host, d.PrivateAddresses[provider], c.Port); e != nil {
						return fail(fmt.Errorf("connection %s from %s: %w", c.Service, name, e))
					}
					break
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
				if len(hs) == 1 && hs[0] == name {
					reasons = append(reasons, fmt.Sprintf("connection %s: publish %s:%d:%d/tcp", n, d.PrivateAddresses[name], c.Port, c.Port))
				}
			}
			out = append(out, plan.Change{Kind: "firewall", Name: d.App + "/" + d.Destination, Server: name, Action: action, Reasons: reasons})
		}
	}
	return out, nil
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
	for i, h := range d.Servers {
		old, e := readRelease(ctx, h.Host, path.Join(release.Dir(d.App, d.Destination, version), "release.json"))
		if e != nil {
			return nil, e
		}
		if old.Roles == nil {
			return nil, fmt.Errorf("Release %s predates Roles on %s; migrate explicitly", version, h.Name)
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
	for _, name := range order {
		for _, c := range previous.Connections {
			provider := previous.ServiceServers(c.Service)[0]
			if provider == name {
				continue
			}
			for _, svc := range c.Consumers {
				if slices.Contains(previous.ServiceServers(svc), name) {
					if e := roles.Ready(ctx, hosts[name].Host, d.PrivateAddresses[provider], c.Port); e != nil {
						return nil, &RolesFailure{Err: e, Changed: changed}
					}
					break
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
			return nil, &RolesFailure{Err: err, Changed: changed}
		}
		changed = append(changed, name)
	}
	for _, name := range order {
		rules, e := roles.FirewallRules(d.App, d.Destination, name, previous, d.PrivateAddresses)
		if e != nil {
			return nil, &RolesFailure{Err: e, Changed: changed}
		}
		if e = roles.Protect(ctx, hosts[name].Host, d.App, d.Destination, rules); e != nil {
			return nil, &RolesFailure{Err: e, Changed: changed}
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
func GeneratedOwner(d *plan.Deploy, key string) string { return generatedOwner(d, key) }
