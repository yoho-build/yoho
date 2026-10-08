package config

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
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
	}
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
	if c.Transport.Mode == "registry" && c.Registry == nil {
		add("transport.mode registry requires a registry section")
	}
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
	}
	for _, n := range sortedKeys(c.Backups.Jobs) {
		j := c.Backups.Jobs[n]
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
