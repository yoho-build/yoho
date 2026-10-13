package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// restoreState records deployed objects, not the operator's current compose
// file. Stopping the whole owned App also catches workers and consumers whose
// dependency on restored data is not expressed with depends_on. Routes are
// deliberately left alone: stopped backends cannot serve partially restored
// data, and shared Proxy configuration belongs to other Apps too.
type restoreState struct {
	Runtime string
	Objects []restoreObject
}

type restoreObject struct {
	ID, Service            string
	Labels                 map[string]string
	Status                 string
	HasHealthcheck         bool
	Replicas               *int
	Ports                  map[string]json.RawMessage
	NetworkMode            string
	Networks               map[string]json.RawMessage
	Attachments            []struct{ Target string }
	Published              []json.RawMessage
	Constraints            []string
	Image, HealthcheckMode string
	Mounts                 []struct{ Type, Source string }
}

// Docker inspect uses missingkey=error; optional JSON keys must use index.
const restoreImageHealthFormat = `{{with index .Config "Healthcheck"}}{{with index . "Test"}}{{if ne (index . 0) "NONE"}}true{{else}}false{{end}}{{else}}false{{end}}{{else}}false{{end}}`
const restoreContainerFormat = `{"ID":{{json .Id}},"Service":{{json (index .Config.Labels "com.docker.compose.service")}},"Labels":{"yoho.app":{{json (index .Config.Labels "yoho.app")}},"yoho.destination":{{json (index .Config.Labels "yoho.destination")}},"com.docker.compose.project":{{json (index .Config.Labels "com.docker.compose.project")}}},"Status":{{json .State.Status}},"HasHealthcheck":` + restoreImageHealthFormat + `,"Ports":{{json (index .HostConfig "PortBindings")}},"NetworkMode":{{json .HostConfig.NetworkMode}},"Networks":{{json (index .NetworkSettings "Networks")}}}`
const restoreServiceFormat = `{"ID":{{json .Spec.Name}},"Service":{{json (index .Spec.Labels "yoho.service")}},"Labels":{"yoho.app":{{json (index .Spec.Labels "yoho.app")}},"yoho.destination":{{json (index .Spec.Labels "yoho.destination")}},"com.docker.stack.namespace":{{json (index .Spec.Labels "com.docker.stack.namespace")}}},"Replicas":{{json .Spec.Mode.Replicated.Replicas}},"Attachments":{{json .Spec.TaskTemplate.Networks}},"Published":{{json .Spec.EndpointSpec.Ports}},"HasHealthcheck":{{if .Spec.TaskTemplate.ContainerSpec.Healthcheck}}{{if .Spec.TaskTemplate.ContainerSpec.Healthcheck.Test}}{{if ne (index .Spec.TaskTemplate.ContainerSpec.Healthcheck.Test 0) "NONE"}}true{{else}}false{{end}}{{else}}false{{end}}{{else}}false{{end}},"Constraints":{{if .Spec.TaskTemplate.Placement}}{{json .Spec.TaskTemplate.Placement.Constraints}}{{else}}null{{end}},"Image":{{json .Spec.TaskTemplate.ContainerSpec.Image}},"HealthcheckMode":{{if .Spec.TaskTemplate.ContainerSpec.Healthcheck}}{{if .Spec.TaskTemplate.ContainerSpec.Healthcheck.Test}}{{if eq (index .Spec.TaskTemplate.ContainerSpec.Healthcheck.Test 0) "NONE"}}"disabled"{{else}}"enabled"{{end}}{{else}}"inherit"{{end}}{{else}}"inherit"{{end}}}`

const restoreVolumeServiceFormat = `{"ID":{{json .Spec.Name}},"Labels":{"yoho.app":{{json (index .Spec.Labels "yoho.app")}},"yoho.destination":{{json (index .Spec.Labels "yoho.destination")}},"com.docker.stack.namespace":{{json (index .Spec.Labels "com.docker.stack.namespace")}}},"Mounts":[{{range $i,$m := .Spec.TaskTemplate.ContainerSpec.Mounts}}{{if $i}},{{end}}{"Type":{{json $m.Type}},"Source":{{json $m.Source}}}{{end}}]}`

// restorePrerequisites reads the deployed Release, never today's local Compose.
// Only the transitive declared prerequisite closure may run during restore;
// all its objects must pass the same private-target preflight as restore roots.
func restorePrerequisites(ctx context.Context, h remote.Host, app, dest, runtime string, temporary []string) ([]string, map[string]bool, error) {
	if len(temporary) == 0 {
		return nil, nil, nil
	}
	type service struct {
		Name       string            `json:"name"`
		DependsOn  []string          `json:"depends_on"`
		Conditions map[string]string `json:"dependency_conditions"`
	}
	base := path.Join(release.AppDir(app, dest), "current")
	data, err := h.ReadFile(ctx, path.Join(base, "plan.json"), false)
	if err != nil {
		return nil, nil, fmt.Errorf("restore isolation: cannot verify deployed prerequisites: %w", err)
	}
	var services []service
	if runtime == "swarm" {
		var plan struct {
			Services []service `json:"services"`
		}
		if err := json.Unmarshal(data, &plan); err != nil {
			return nil, nil, fmt.Errorf("restore isolation: invalid deployed prerequisite plan: %w", err)
		}
		services = plan.Services
	} else if err := json.Unmarshal(data, &services); err != nil {
		return nil, nil, fmt.Errorf("restore isolation: invalid deployed prerequisite plan: %w", err)
	}
	byName := map[string]service{}
	for _, svc := range services {
		if svc.Name == "" {
			return nil, nil, errors.New("restore isolation: deployed prerequisite plan has an unnamed Service")
		}
		if _, exists := byName[svc.Name]; exists {
			return nil, nil, fmt.Errorf("restore isolation: deployed prerequisite plan repeats Service %s", svc.Name)
		}
		byName[svc.Name] = svc
	}
	// Compose preserves dependency conditions in the compiled manifest. Swarm
	// drops depends_on, so new Release plans explicitly retain the conditions;
	// dependency-bearing old Swarm plans cannot safely infer completion jobs.
	var compose struct {
		Services map[string]struct {
			DependsOn yaml.Node `yaml:"depends_on"`
		} `yaml:"services"`
	}
	composeRead := false
	condition := func(svc service, dep string) (string, error) {
		if runtime == "swarm" {
			c, ok := svc.Conditions[dep]
			if !ok {
				return "", fmt.Errorf("restore isolation: deployed Swarm plan lacks dependency conditions for %s -> %s; redeploy to refresh the Release metadata before restoring", svc.Name, dep)
			}
			return c, nil
		}
		if !composeRead {
			data, err := h.ReadFile(ctx, path.Join(base, "compose.yaml"), false)
			if err != nil {
				return "", fmt.Errorf("restore isolation: cannot verify deployed dependency conditions: %w", err)
			}
			if err := yaml.Unmarshal(data, &compose); err != nil {
				return "", fmt.Errorf("restore isolation: invalid deployed dependency conditions: %w", err)
			}
			composeRead = true
		}
		n := compose.Services[svc.Name].DependsOn
		if n.Kind == yaml.SequenceNode {
			for _, item := range n.Content {
				if item.Value == dep {
					return "service_started", nil
				}
			}
		} else if n.Kind == yaml.MappingNode {
			var deps map[string]struct {
				Condition string `yaml:"condition"`
			}
			if err := n.Decode(&deps); err != nil {
				return "", fmt.Errorf("restore isolation: invalid dependency conditions of %s: %w", svc.Name, err)
			}
			if d, ok := deps[dep]; ok {
				return d.Condition, nil
			}
		}
		return "", fmt.Errorf("restore isolation: deployed dependency metadata is inconsistent for %s -> %s", svc.Name, dep)
	}
	requireHealth := map[string]bool{}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var order []string
	var visit func(string) error
	visit = func(name string) error {
		if visiting[name] {
			return fmt.Errorf("restore isolation: cycle in deployed restore prerequisites at %s", name)
		}
		if visited[name] {
			return nil
		}
		svc, ok := byName[name]
		if !ok {
			return fmt.Errorf("restore isolation: restore prerequisite %s is absent from the deployed Release plan", name)
		}
		visiting[name] = true
		for _, dep := range svc.DependsOn {
			c, err := condition(svc, dep)
			if err != nil {
				return err
			}
			switch c {
			case "", "service_started":
			case "service_healthy":
				requireHealth[dep] = true
			default:
				return fmt.Errorf("restore isolation: prerequisite %s -> %s has unsupported condition %s; completion-only prerequisites require a controlled manual restore", name, dep, c)
			}
			if err := visit(dep); err != nil {
				return err
			}
		}
		visiting[name] = false
		visited[name] = true
		order = append(order, name)
		return nil
	}
	for _, name := range temporary {
		if err := visit(name); err != nil {
			return nil, nil, err
		}
	}
	return order, requireHealth, nil
}

func snapshotRestoreState(ctx context.Context, h remote.Host, project, app, dest, runtime string, selected, temporary []string, requireHealth map[string]bool) (*restoreState, error) {
	s := &restoreState{Runtime: runtime}
	list := "docker ps -aq --filter " + remote.Quote("label=com.docker.compose.project="+project)
	inspect, format := "docker inspect -f ", restoreContainerFormat
	if runtime == "swarm" {
		list = "docker service ls -q --filter " + remote.Quote("label=com.docker.stack.namespace="+project)
		inspect, format = "docker service inspect -f ", restoreServiceFormat
	}
	ids, err := h.Output(ctx, remote.Cmd{Script: "set -eu; " + list})
	if err != nil {
		return nil, fmt.Errorf("list restore isolation objects: %w", err)
	}
	for _, id := range strings.Fields(ids) {
		out, err := h.Output(ctx, remote.Cmd{Script: "set -eu; " + inspect + remote.Quote(format) + " " + remote.Quote(id)})
		if err != nil {
			return nil, fmt.Errorf("inspect restore isolation object %s: %w", id, err)
		}
		var o restoreObject
		if err := json.Unmarshal([]byte(out), &o); err != nil {
			return nil, fmt.Errorf("inspect restore isolation object %s: %w", id, err)
		}
		if o.ID == "" || o.Service == "" || o.Labels["yoho.app"] != app || o.Labels["yoho.destination"] != dest {
			return nil, fmt.Errorf("restore isolation: object %s does not have matching App %s Destination %s ownership labels; refusing to change it", id, app, dest)
		}
		if runtime == "swarm" {
			if o.Labels["com.docker.stack.namespace"] != project || o.ID != project+"_"+o.Service || o.Replicas == nil || *o.Replicas < 0 {
				return nil, fmt.Errorf("restore isolation: %s is not an owned replicated Swarm Service", id)
			}
		} else {
			if o.Labels["com.docker.compose.project"] != project {
				return nil, fmt.Errorf("restore isolation: %s is not in the App's compose project", id)
			}
			switch o.Status {
			case "running", "exited", "created":
			default:
				return nil, fmt.Errorf("restore isolation: container %s is %s; stabilize the App before restoring", id, o.Status)
			}
		}
		s.Objects = append(s.Objects, o)
	}
	for _, svc := range selected {
		if len(s.service(svc)) == 0 {
			return nil, fmt.Errorf("restore isolation: no deployed Service %s; deploy the App before restoring", svc)
		}
	}
	if len(temporary) > 0 {
		routes, err := proxy.List(ctx, h)
		if err != nil {
			return nil, fmt.Errorf("restore isolation: verify Proxy exposure: %w", err)
		}
		for _, svc := range temporary {
			for _, route := range routes {
				if route.Service == app+"-"+dest+"-"+svc {
					return nil, fmt.Errorf("restore isolation: dump/post_restore Service %s has Proxy route %s; isolate its exposure before restoring (route left unchanged)", svc, route.Service)
				}
			}
		}
	}
	var localNode, localHostname string
	if runtime == "swarm" && len(temporary) > 0 {
		out, err := h.Output(ctx, remote.Cmd{Script: "set -eu; node=$(docker info -f " + remote.Quote(`{{.Swarm.NodeID}}`) + "); docker node inspect -f " + remote.Quote(`{{.ID}}|{{.Description.Hostname}}`) + " \"$node\""})
		if err != nil {
			return nil, fmt.Errorf("restore isolation: inspect local Swarm node: %w", err)
		}
		parts := strings.Split(strings.TrimSpace(out), "|")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return nil, errors.New("restore isolation: cannot establish local Swarm node identity")
		}
		localNode, localHostname = parts[0], parts[1]
	}
	for _, svc := range temporary {
		objects := s.service(svc)
		if len(objects) != 1 || (runtime == "swarm" && *objects[0].Replicas > 1) {
			return nil, fmt.Errorf("restore isolation: dump/post_restore Service %s must have exactly one deployed container or at most one Swarm replica", svc)
		}
		o := objects[0]
		if runtime == "swarm" {
			pinned := false
			for _, constraint := range o.Constraints {
				key, value, ok := strings.Cut(constraint, "==")
				if ok && ((strings.TrimSpace(key) == "node.id" && strings.TrimSpace(value) == localNode) || (strings.TrimSpace(key) == "node.hostname" && strings.TrimSpace(value) == localHostname)) {
					pinned = true
				}
			}
			if !pinned {
				return nil, fmt.Errorf("restore isolation: temporary Swarm Service %s is not pinned to the restore Server; remote or movable targets require a controlled manual restore", svc)
			}
			if requireHealth[svc] && !o.HasHealthcheck && o.HealthcheckMode == "inherit" {
				if o.Image == "" {
					return nil, fmt.Errorf("restore isolation: cannot resolve inherited healthcheck of %s without its deployed image", svc)
				}
				out, err := h.Output(ctx, remote.Cmd{Script: "set -eu; docker image inspect -f " + remote.Quote(restoreImageHealthFormat) + " " + remote.Quote(o.Image)})
				if err != nil {
					return nil, fmt.Errorf("restore isolation: resolve inherited healthcheck of %s: %w", svc, err)
				}
				switch strings.TrimSpace(out) {
				case "true":
					o.HasHealthcheck = true
				case "false":
				default:
					return nil, fmt.Errorf("restore isolation: invalid image healthcheck metadata for %s", svc)
				}
			}
		}
		if requireHealth[svc] && !o.HasHealthcheck {
			return nil, fmt.Errorf("restore isolation: prerequisite %s requires service_healthy but its deployed object has no enabled healthcheck", svc)
		}
		if len(o.Ports) > 0 || len(o.Published) > 0 || o.NetworkMode == "host" || strings.HasPrefix(o.NetworkMode, "container:") {
			return nil, fmt.Errorf("restore isolation: dump/post_restore Service %s has published ports or a shared network namespace; isolate its exposure before restoring", svc)
		}
		networks := sortedKeys(o.Networks)
		for _, a := range o.Attachments {
			networks = append(networks, a.Target)
		}
		for _, n := range networks {
			// Only App-owned private networks are permitted during temporary
			// execution. In particular, never start a backend on the shared
			// yoho network while a Proxy or Tunnel can route to it.
			out, err := h.Output(ctx, remote.Cmd{Script: "set -eu; docker network inspect -f " + remote.Quote(`{{index .Labels "com.docker.compose.project"}}|{{index .Labels "com.docker.stack.namespace"}}|{{.Driver}}`) + " " + remote.Quote(n)})
			if err != nil {
				return nil, fmt.Errorf("inspect restore network %s: %w", n, err)
			}
			parts := strings.Split(strings.TrimSpace(out), "|")
			ownerIndex := 0
			if runtime == "swarm" {
				ownerIndex = 1
			}
			if len(parts) != 3 || parts[ownerIndex] != project || (parts[2] != "bridge" && parts[2] != "overlay") {
				return nil, fmt.Errorf("restore isolation: dump/post_restore Service %s uses network %s outside the App's private networks; isolate its Proxy/Tunnel/external access before restoring", svc, n)
			}
		}
	}
	return s, nil
}

// checkRestoreVolumeUsers refuses shared data belonging to unrelated Apps,
// even when those containers are currently stopped. Never stop foreign users
// just to make a destructive restore possible.
func checkRestoreVolumeUsers(ctx context.Context, h remote.Host, volume, project, app, dest, runtime string) error {
	if runtime == "swarm" {
		// Local docker ps cannot see remote tasks or zero-replica Services.
		// Inventory every Service spec on the manager before touching data.
		ids, err := h.Output(ctx, remote.Cmd{Script: "set -eu; docker service ls -q"})
		if err != nil {
			return fmt.Errorf("restore isolation: inspect cluster volume users: %w", err)
		}
		for _, id := range strings.Fields(ids) {
			out, err := h.Output(ctx, remote.Cmd{Script: "set -eu; docker service inspect -f " + remote.Quote(restoreVolumeServiceFormat) + " " + remote.Quote(id)})
			if err != nil {
				return fmt.Errorf("restore isolation: inspect cluster volume user %s: %w", id, err)
			}
			var o restoreObject
			if err := json.Unmarshal([]byte(out), &o); err != nil || o.ID == "" {
				return fmt.Errorf("restore isolation: invalid cluster volume metadata for %s", id)
			}
			for _, mount := range o.Mounts {
				if mount.Type != "volume" {
					continue
				}
				if strings.Contains(mount.Source, "{{") {
					return fmt.Errorf("restore isolation: cannot verify templated volume source of Service %s", o.ID)
				}
				if mount.Source == volume && (o.Labels["com.docker.stack.namespace"] != project || o.Labels["yoho.app"] != app || o.Labels["yoho.destination"] != dest) {
					return fmt.Errorf("restore isolation: volume %s is mounted by foreign Swarm Service %s; refusing to change shared data", volume, o.ID)
				}
			}
		}
	}
	projectLabel := "com.docker.compose.project"
	if runtime == "swarm" {
		projectLabel = "com.docker.stack.namespace"
	}
	format := `{{.ID}}|{{.Label "` + projectLabel + `"}}|{{.Label "yoho.app"}}|{{.Label "yoho.destination"}}`
	out, err := h.Output(ctx, remote.Cmd{Script: "set -eu; docker ps -a --filter " + remote.Quote("volume="+volume) + " --format " + remote.Quote(format)})
	if err != nil {
		return fmt.Errorf("inspect restore volume users: %w", err)
	}
	for _, row := range strings.Split(strings.TrimSpace(out), "\n") {
		if row == "" {
			continue
		}
		fields := strings.Split(row, "|")
		if len(fields) != 4 || fields[1] != project || fields[2] != app || fields[3] != dest {
			return fmt.Errorf("restore isolation: volume %s is also mounted by a container outside this owned App; refusing to change shared data", volume)
		}
	}
	return nil
}

func (s *restoreState) service(name string) []restoreObject {
	var objects []restoreObject
	for _, o := range s.Objects {
		if o.Service == name {
			objects = append(objects, o)
		}
	}
	return objects
}

// Bound each request separately and issue every stop/scale-zero request before
// waiting for convergence. One slow Service must not starve later isolation
// requests during the overall bounded failure cleanup.
var stopAttemptTimeout = 10 * time.Second
var stopWaitTimeout = 30 * time.Second

// Initial isolation honors Docker's configured shutdown grace and the caller's
// context. Strict per-object budgets are reserved for failure cleanup.
func (s *restoreState) stop(ctx context.Context, h remote.Host) error {
	return s.stopWithBounds(ctx, h, false)
}

func (s *restoreState) stopBounded(ctx context.Context, h remote.Host) error {
	return s.stopWithBounds(ctx, h, true)
}

func (s *restoreState) stopWithBounds(ctx context.Context, h remote.Host, bounded bool) error {
	var errs []error
	var waiting []restoreObject
	attemptLimit := stopAttemptTimeout
	if deadline, ok := ctx.Deadline(); ok && len(s.Objects) > 0 {
		budget := time.Until(deadline) / time.Duration(2*len(s.Objects))
		if budget > 0 && budget < attemptLimit {
			attemptLimit = budget
		}
	}
	for _, o := range s.Objects {
		attempt, cancel := ctx, func() {}
		if bounded {
			attempt, cancel = context.WithTimeout(ctx, attemptLimit)
		}
		var err error
		if s.Runtime == "swarm" {
			err = h.Run(attempt, remote.Cmd{Script: "set -eu; docker service scale " + remote.Quote(o.ID+"=0") + " --detach=true >/dev/null"})
			waiting = append(waiting, o)
		} else {
			err = h.Run(attempt, remote.Cmd{Script: "set -eu; docker stop " + remote.Quote(o.ID) + " >/dev/null"})
		}
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", o.ID, err))
		}
	}
	for _, o := range waiting {
		wait, cancel := ctx, func() {}
		if bounded {
			wait, cancel = context.WithTimeout(ctx, stopWaitTimeout)
		}
		err := waitServiceReplicas(wait, h, o.ID, 0)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("wait for stopped %s: %w", o.ID, err))
		}
	}
	return errors.Join(errs...)
}

func (s *restoreState) startTemporary(ctx context.Context, h remote.Host, project string, svcs []string) error {
	for _, svc := range svcs {
		o := s.service(svc)[0] // preflight requires exactly one object
		if s.Runtime == "swarm" {
			if err := scaleService(ctx, h, o.ID, 1); err != nil {
				return err
			}
		} else if err := h.Run(ctx, remote.Cmd{Script: "set -eu; docker start " + remote.Quote(o.ID) + " >/dev/null"}); err != nil {
			return err
		}
		cid := o.ID
		if s.Runtime == "swarm" {
			var err error
			cid, err = containerID(ctx, h, project, svc, s.Runtime)
			if err != nil {
				return err
			}
		}
		if err := waitReady(ctx, h, cid, readyTimeout); err != nil {
			return fmt.Errorf("restore prerequisite %s: %w", svc, err)
		}
	}
	return nil
}

func (s *restoreState) resume(ctx context.Context, h remote.Host) error {
	// Stop temporarily started targets that were previously inactive before
	// any consumer can resume. Keep previously running private targets alive:
	// their readiness was checked for restore commands and another restart
	// would unnecessarily discard that readiness.
	for _, o := range s.Objects {
		if s.Runtime == "swarm" {
			if *o.Replicas == 0 {
				if err := scaleService(ctx, h, o.ID, 0); err != nil {
					return err
				}
			}
		} else if o.Status != "running" {
			if err := h.Run(ctx, remote.Cmd{Script: "set -eu; docker stop " + remote.Quote(o.ID) + " >/dev/null"}); err != nil {
				return err
			}
		}
	}
	for _, o := range s.Objects {
		if s.Runtime == "swarm" {
			if *o.Replicas > 0 {
				if err := scaleService(ctx, h, o.ID, *o.Replicas); err != nil {
					return err
				}
			}
		} else if o.Status == "running" {
			if err := h.Run(ctx, remote.Cmd{Script: "set -eu; docker start " + remote.Quote(o.ID) + " >/dev/null"}); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}
