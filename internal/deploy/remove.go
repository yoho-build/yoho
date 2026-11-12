package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/safepath"
)

// `yoho remove` takes an App Destination off a Server: its containers,
// Proxy routes, networks and App directory (Releases, secrets, files). It
// selects Docker objects only by exact label values of this App
// Destination (the compose project or swarm stack label, or the yoho.app
// and yoho.destination pair), never by name prefix, so it never matches
// another project such as yoho-<app>-<destination>-drill-* or
// yoho-<app>-<destination>2. The shared yoho network and the Proxy stay.
// Volumes are removed only with RemoveOptions.Volumes. Backups (the local
// spool <root>/backups/<app>/<destination> and every Backup Target) are
// never touched.

// Docker labels selecting the objects of a compose project or swarm stack.
const (
	composeProjectLabel = "com.docker.compose.project"
	stackNamespaceLabel = "com.docker.stack.namespace"
)

// RemoveOptions selects what `yoho remove` removes on one Server.
type RemoveOptions struct {
	App, Destination string
	// Swarm: objects carry the stack label (and the -release stack of
	// release commands) instead of the compose project label.
	Swarm bool
	// Volumes also removes the App Destination's named volumes.
	Volumes bool
	// Services are Service names known from the local compose files; the
	// Services of every Release on the Server are added. Only Proxy routes
	// named after one of them are removed.
	Services []string
	// Sudo removes the App directory with sudo (Server.sudo): the scoped
	// access directory inside it is root-owned.
	Sudo bool
}

func (o RemoveOptions) project() string { return release.ProjectName(o.App, o.Destination) }

// selectors are the label filters of the App Destination's objects.
func (o RemoveOptions) selectors() []string {
	if o.Swarm {
		return []string{"label=" + stackNamespaceLabel + "=" + o.project(), "label=" + stackNamespaceLabel + "=" + o.project() + "-release"}
	}
	return []string{"label=" + composeProjectLabel + "=" + o.project()}
}

// RemoveInventory is what remove finds on one Server.
type RemoveInventory struct {
	Containers []string `json:"containers"`
	Routes     []string `json:"routes"`
	Networks   []string `json:"networks"`
	Volumes    []string `json:"volumes"`
	Secrets    []string `json:"secrets,omitempty"` // swarm
	// AppDir is the App directory when it exists on this Server.
	AppDir string `json:"app_dir,omitempty"`
}

// listScript prints one object name per line for each selector, prefixed
// with kind and a tab. ls -q style listing by exact label only.
func listScript(o RemoveOptions) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	for _, sel := range o.selectors() {
		f := remote.Quote(sel)
		b.WriteString("docker ps -a --filter " + f + " --format 'container\t{{.Names}}\t{{.Label \"" + LabelApp + "\"}}|{{.Label \"" + LabelDestination + "\"}}'\n")
		b.WriteString("docker network ls --filter " + f + " --format 'network\t{{.Name}}'\n")
		b.WriteString("docker volume ls --filter " + f + " --format 'volume\t{{.Name}}'\n")
	}
	// A scoped App's own Proxy network (ADR 0011) is labelled with the App
	// Destination rather than the project.
	b.WriteString("docker network ls " + remote.QuoteArgs(scopedNetworkFilters(o)...) + " --format 'network\t{{.Name}}'\n")
	if o.Swarm {
		b.WriteString("docker secret ls " + remote.QuoteArgs(ownerFilters(o)...) + " --format 'secret\t{{.Name}}' 2>/dev/null || true\n")
	}
	return b.String()
}

func ownerFilters(o RemoveOptions) []string {
	return []string{"--filter", "label=" + LabelApp + "=" + o.App, "--filter", "label=" + LabelDestination + "=" + o.Destination}
}

func scopedNetworkFilters(o RemoveOptions) []string {
	return append(ownerFilters(o), "--filter", "label="+LabelProxyNetwork+"=1")
}

// InspectRemove lists what remove would remove on h, read-only. It refuses
// (an *release.OwnershipError) when a container of the project belongs to
// another App Destination.
func InspectRemove(ctx context.Context, h remote.Host, o RemoveOptions) (*RemoveInventory, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: listScript(o)})
	if err != nil {
		return nil, fmt.Errorf("list the App's Docker objects on %s: %w", h.Name(), err)
	}
	inv := &RemoveInventory{}
	add := func(list *[]string, v string) {
		if v != "" && !slices.Contains(*list, v) {
			*list = append(*list, v)
		}
	}
	for _, l := range lines(out) {
		f := strings.Split(l, "\t")
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "container":
			if len(f) > 2 {
				a, d, _ := strings.Cut(f[2], "|")
				if (owner{app: a, dest: d}).foreign(o.App, o.Destination) {
					return nil, &release.OwnershipError{Msg: fmt.Sprintf("project %s on %s runs container %s of App %s Destination %s; not removing it. Rename the App or Destination", o.project(), h.Name(), f[1], a, d)}
				}
			}
			add(&inv.Containers, f[1])
		case "network":
			if f[1] != proxy.Network {
				add(&inv.Networks, f[1])
			}
		case "volume":
			add(&inv.Volumes, f[1])
		case "secret":
			add(&inv.Secrets, f[1])
		}
	}
	routes, err := removableRoutes(ctx, h, o)
	if err != nil {
		return nil, err
	}
	inv.Routes = routes
	dir := release.AppDir(o.App, o.Destination)
	if ok, err := h.Output(ctx, remote.Cmd{Script: "if [ -e " + remote.Quote(dir) + " ] || [ -L " + remote.Quote(dir) + " ]; then echo yes; fi"}); err != nil {
		return nil, err
	} else if ok == "yes" {
		inv.AppDir = dir
	}
	return inv, nil
}

// releaseServices returns the Service names recorded in the plan.json of
// every Release of the App Destination on h (compose: a list of Services;
// swarm: an object with services).
func releaseServices(ctx context.Context, h remote.Host, app, dest string) ([]string, error) {
	glob := remote.Quote(path.Join(release.AppDir(app, dest), "releases")) + "/*/plan.json"
	out, err := h.Output(ctx, remote.Cmd{Script: "for f in " + glob + "; do [ -f \"$f\" ] && cat \"$f\" && echo; done; true"})
	if err != nil {
		return nil, fmt.Errorf("read Release plans: %w", err)
	}
	var names []string
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			return names, nil // a damaged plan: keep what was read
		}
		type svc struct {
			Name string `json:"name"`
		}
		var list []svc
		var obj struct {
			Services []svc `json:"services"`
		}
		if bytes.HasPrefix(bytes.TrimSpace(raw), []byte("[")) {
			_ = json.Unmarshal(raw, &list)
		} else if json.Unmarshal(raw, &obj) == nil {
			list = obj.Services
		}
		for _, s := range list {
			if s.Name != "" && !slices.Contains(names, s.Name) {
				names = append(names, s.Name)
			}
		}
	}
	return names, nil
}

// removableRoutes are the Proxy routes on h named after a Service of this
// App Destination (RouteName) whose targets are not another App
// Destination's containers. Without a Proxy there are none.
func removableRoutes(ctx context.Context, h remote.Host, o RemoveOptions) ([]string, error) {
	running, err := h.Output(ctx, remote.Cmd{Script: "docker container inspect -f '{{.State.Running}}' " + proxy.ContainerName + " 2>/dev/null || true"})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(running) != "true" {
		return nil, nil
	}
	known, err := releaseServices(ctx, h, o.App, o.Destination)
	if err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for _, s := range append(known, o.Services...) {
		want[RouteName(o.App, o.Destination, s)] = true
	}
	routes, err := proxy.List(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("list Proxy routes on %s: %w", h.Name(), err)
	}
	var out []string
	for _, rt := range routes {
		if !want[rt.Service] {
			continue
		}
		if !o.Swarm {
			if _, foreign := routeOwner(ctx, h, rt, o.App, o.Destination); foreign {
				continue
			}
		}
		out = append(out, rt.Service)
	}
	slices.Sort(out)
	return out, nil
}

// RemoveRoutes deletes the named Proxy routes on h.
func RemoveRoutes(ctx context.Context, h remote.Host, routes []string) error {
	for _, r := range routes {
		if err := proxy.Remove(ctx, h, r); err != nil {
			return err
		}
	}
	return nil
}

// RemoveContainersScript stops and removes every container of the compose
// project (including `compose run` one-offs), selected by the exact project
// label.
func RemoveContainersScript(o RemoveOptions) string {
	rmFlags := "-f"
	if o.Volumes {
		rmFlags = "-fv"
	}
	var b strings.Builder
	b.WriteString("set -eu\n")
	for _, sel := range o.selectors() {
		b.WriteString("ids=$(docker ps -aq --filter " + remote.Quote(sel) + ")\n" +
			"if [ -n \"$ids\" ]; then docker stop $ids >/dev/null; docker rm " + rmFlags + " $ids >/dev/null; fi\n")
	}
	return b.String()
}

// RemoveNetworksScript removes the App Destination's networks: the
// project's own (by exact label) and a scoped App's Proxy network after
// disconnecting the Proxy from it. Never the shared yoho network.
func RemoveNetworksScript(o RemoveOptions) string {
	var b strings.Builder
	b.WriteString("set -eu\nyoho_netrm() {\n" +
		"  name=$(docker network inspect -f '{{.Name}}' \"$1\" 2>/dev/null) || return 0\n" +
		"  [ \"$name\" = " + remote.Quote(proxy.Network) + " ] && return 0\n" +
		"  docker network disconnect -f \"$1\" " + proxy.ContainerName + " >/dev/null 2>&1 || true\n" +
		"  docker network rm \"$1\" >/dev/null\n}\n")
	for _, sel := range o.selectors() {
		b.WriteString("for n in $(docker network ls -q --filter " + remote.Quote(sel) + "); do yoho_netrm \"$n\"; done\n")
	}
	b.WriteString("for n in $(docker network ls -q " + remote.QuoteArgs(scopedNetworkFilters(o)...) + "); do yoho_netrm \"$n\"; done\n")
	return b.String()
}

// RemoveVolumesScript removes the App Destination's named volumes, by the
// exact project or stack label. External volumes carry no such label and
// stay.
func RemoveVolumesScript(o RemoveOptions) string {
	var b strings.Builder
	b.WriteString("set -eu\n")
	for _, sel := range o.selectors() {
		b.WriteString("for v in $(docker volume ls -q --filter " + remote.Quote(sel) + "); do docker volume rm \"$v\" >/dev/null; done\n")
	}
	return b.String()
}

// RemoveAppDirScript deletes the canonical App directory: a real
// directory, or the trusted app_dir link plus the contents of its target
// when the target carries this App Destination's ownership marker. Then
// the App's apps/<app> directory when empty and the Destination's
// coordination directory unless a migration journal or a held gate is in
// it. It also drops the lock that lives inside the App directory.
func RemoveAppDirScript(o RemoveOptions) string {
	dir := release.AppDir(o.App, o.Destination)
	q := remote.Quote(dir)
	meta := remote.Quote(safepath.MetaDir(o.App, o.Destination))
	return "set -eu\n" +
		"if [ -L " + q + " ]; then\n" +
		"  t=$(readlink " + q + ")\n" +
		"  case \"$t\" in /*) ;; *) echo 'App directory link is not absolute; remove '" + q + "' by hand' >&2; exit 1;; esac\n" +
		"  if [ -f \"$t/" + safepath.MarkerName + "\" ] && [ \"$(cat \"$t/" + safepath.MarkerName + "\")\" = " + remote.Quote(safepath.Marker(o.App, o.Destination)) + " ]; then\n" +
		"    find \"$t\" -mindepth 1 -maxdepth 1 -exec rm -rf {} +\n" +
		"    rmdir \"$t\" 2>/dev/null || true\n" +
		"  else\n" +
		"    echo \"app_dir $t has no ownership marker of " + o.App + "/" + o.Destination + "; left in place\" >&2\n" +
		"  fi\n" +
		"  rm -f " + q + "\n" +
		"elif [ -d " + q + " ]; then\n" +
		"  rm -rf " + q + "\n" +
		"fi\n" +
		"rmdir " + remote.Quote(path.Dir(dir)) + " 2>/dev/null || true\n" +
		"if [ -d " + meta + " ] && [ ! -e " + meta + "/migrate.json ] && [ ! -e " + meta + "/gate ]; then rm -rf " + meta + "; fi\n" +
		"rmdir " + remote.Quote(path.Dir(safepath.MetaDir(o.App, o.Destination))) + " " + remote.Quote(path.Dir(path.Dir(safepath.MetaDir(o.App, o.Destination)))) + " 2>/dev/null || true\n"
}

// RemoveAppDir runs RemoveAppDirScript on h.
func RemoveAppDir(ctx context.Context, h remote.Host, o RemoveOptions) error {
	if err := h.Run(ctx, remote.Cmd{Script: RemoveAppDirScript(o), Sudo: o.Sudo}); err != nil {
		return fmt.Errorf("remove App directory on %s: %w", h.Name(), err)
	}
	return nil
}
