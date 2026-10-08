package deploy

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

var _ plan.Planner = Compose{}

// planGenDir stands in for the secrets generation directory while planning;
// the real one is only known at deploy time. Old compose files are rewritten
// to the same placeholder before comparing.
const planGenDir = "/__yoho_secrets_generation__"

// planKey is the fingerprint key when the Server has no hmac.key yet (first
// deploy): every fingerprint differs from what is deployed, which is correct.
var planKey = []byte("yoho-plan-placeholder-key-000000")

// container is the part of `docker inspect` that Diff needs.
type container struct {
	Name  string `json:"Name"`
	State struct {
		Running bool `json:"Running"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
}

func (c container) service() string { return c.Config.Labels["com.docker.compose.service"] }

// projectContainers lists the compose project's containers (one-off release
// command containers excluded). Read-only.
func projectContainers(ctx context.Context, h remote.Host, project string) ([]container, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: "ids=$(docker ps -aq --filter " + remote.Quote("label=com.docker.compose.project="+project) + ")\n" +
		`[ -z "$ids" ] || docker inspect $ids`})
	if err != nil {
		return nil, fmt.Errorf("list project containers: %w", err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	var all []container
	if err := json.Unmarshal([]byte(out), &all); err != nil {
		return nil, fmt.Errorf("parse docker inspect: %w", err)
	}
	var cs []container
	for _, c := range all {
		if strings.EqualFold(c.Config.Labels["com.docker.compose.oneoff"], "true") {
			continue
		}
		cs = append(cs, c)
	}
	return cs, nil
}

// currentRelease loads the compiled state of the Release `current` points at.
type currentState struct {
	rel      *release.Release
	services map[string]map[string]any // normalized service entries
	plans    map[string]servicePlan
}

func loadCurrent(ctx context.Context, h remote.Host, app, dest string) *currentState {
	dir := release.AppDir(app, dest)
	link, err := h.Output(ctx, remote.Cmd{Script: "readlink " + remote.Quote(path.Join(dir, "current")) + " 2>/dev/null || true"})
	if err != nil || strings.TrimSpace(link) == "" {
		return nil
	}
	relDir := path.Join(dir, "releases", path.Base(strings.TrimSpace(link)))
	rel, err := readRelease(ctx, h, path.Join(relDir, "release.json"))
	if err != nil {
		return nil
	}
	raw, err := h.ReadFile(ctx, path.Join(relDir, "compose.yaml"), false)
	if err != nil {
		return nil
	}
	if rel.SecretsGeneration != "" {
		raw = bytes.ReplaceAll(raw, []byte(release.SecretsDir(app, dest, rel.SecretsGeneration)), []byte(planGenDir))
	}
	st := &currentState{rel: rel, plans: map[string]servicePlan{}}
	if st.services, err = serviceEntries(raw); err != nil {
		return nil
	}
	if pj, err := h.ReadFile(ctx, path.Join(relDir, "plan.json"), false); err == nil {
		var ps []servicePlan
		if json.Unmarshal(pj, &ps) == nil {
			for _, p := range ps {
				st.plans[p.Name] = p
			}
		}
	}
	return st
}

// serviceEntries parses a compiled compose file into per-Service maps with
// the fields that change on every Version or are compared separately
// (image, yoho.version, yoho.secrets, YOHO_VERSION) removed.
func serviceEntries(raw []byte) (map[string]map[string]any, error) {
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	for _, s := range doc.Services {
		delete(s, "image")
		if l, ok := s["labels"].(map[string]any); ok {
			delete(l, LabelVersion)
			delete(l, LabelSecrets)
			if len(l) == 0 {
				delete(s, "labels")
			}
		}
		if e, ok := s["environment"].(map[string]any); ok {
			delete(e, "YOHO_VERSION")
			if len(e) == 0 {
				delete(s, "environment")
			}
		}
	}
	return doc.Services, nil
}

func changedKeys(a, b map[string]any) []string {
	var keys []string
	seen := map[string]bool{}
	for k, v := range a {
		seen[k] = true
		if w, ok := b[k]; !ok || !reflect.DeepEqual(v, w) {
			keys = append(keys, k)
		}
	}
	for k := range b {
		if !seen[k] {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

// Diff implements plan.Planner. It never writes to the Server.
func (c Compose) Diff(ctx context.Context, d *plan.Deploy) ([]plan.Change, error) {
	if err := validate(d); err != nil {
		return nil, err
	}
	srv := d.Servers[0]
	h := srv.Host
	dir := release.AppDir(d.App, d.Destination)
	project := release.ProjectName(d.App, d.Destination)
	var changes []plan.Change
	add := func(kind, name string, a plan.Action, downtime bool, reasons ...string) {
		changes = append(changes, plan.Change{Kind: kind, Name: name, Server: srv.Name, Action: a, Reasons: reasons, Downtime: downtime})
	}

	// Secrets: the audit key and generated values as they exist today.
	var key []byte
	if b, err := h.ReadFile(ctx, path.Join(dir, "hmac.key"), false); err == nil {
		if k, err := hex.DecodeString(strings.TrimSpace(string(b))); err == nil && len(k) >= 16 {
			key = k
		}
	}
	kinds, err := generatedDecls(d)
	if err != nil {
		return nil, err
	}
	generated := map[string]string{}
	pending := map[string]bool{}
	for _, name := range sortedKeys(kinds) {
		if b, err := h.ReadFile(ctx, path.Join(dir, "generated", name), false); err == nil && len(b) > 0 {
			generated[name] = string(b)
			continue
		}
		generated[name] = "\x00will-be-generated:" + name
		pending[name] = true
	}
	svcSecrets, err := serviceSecrets(d, generated, func(string, ...any) {})
	if err != nil {
		return nil, err
	}
	fpKey := key
	if fpKey == nil {
		fpKey = planKey
	}
	composeYAML, plans, err := compile(d, srv.Name, planGenDir, svcSecrets, fpKey, DockerServerVersion(ctx, h))
	if err != nil {
		return nil, err
	}
	want, err := serviceEntries(composeYAML)
	if err != nil {
		return nil, err
	}
	wantFP := map[string]string{}
	{
		var doc struct {
			Services map[string]struct {
				Labels map[string]string `yaml:"labels"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(composeYAML, &doc); err == nil {
			for n, s := range doc.Services {
				wantFP[n] = s.Labels[LabelSecrets]
			}
		}
	}

	conts, err := projectContainers(ctx, h, project)
	if err != nil {
		return nil, err
	}
	cur := loadCurrent(ctx, h, d.App, d.Destination)

	// Proxy container.
	if needsProxy(plans) {
		need, why, err := proxy.NeedsBoot(ctx, h, d.Proxy)
		if err != nil {
			return nil, err
		}
		if need {
			act := plan.ActionUpdate
			if strings.Contains(why, "missing") {
				act = plan.ActionCreate
			}
			add("proxy", proxy.ContainerName, act, false, why)
		}
	}

	// Generated secrets that do not exist yet.
	for _, name := range sortedKeys(pending) {
		add("secret", name, plan.ActionCreate, false, "generated on the Server ("+kinds[name]+")")
	}

	// Services in compose.
	byService := map[string][]container{}
	for _, ct := range conts {
		byService[ct.service()] = append(byService[ct.service()], ct)
	}
	for _, sp := range plans {
		cs := byService[sp.Name]
		if len(cs) == 0 {
			rs := []string{"not deployed"}
			if sp.Image != "" {
				rs = []string{"image " + sp.Image}
			}
			add("service", sp.Name, plan.ActionCreate, false, rs...)
			continue
		}
		var reasons []string
		running := 0
		for _, ct := range cs {
			if ct.State.Running {
				running++
			}
		}
		if sp.Image != "" {
			for _, ct := range cs {
				if ct.Config.Image != sp.Image {
					reasons = append(reasons, "image "+ct.Config.Image+" → "+sp.Image)
					break
				}
			}
		}
		secretsChanged := false
		for _, ct := range cs {
			if ct.Config.Labels[LabelSecrets] != wantFP[sp.Name] {
				secretsChanged = true
				break
			}
		}
		if wantFP[sp.Name] != "" && key == nil {
			secretsChanged = true
		}
		switch {
		case secretsChanged && servicePending(d, sp.Name, pending):
			reasons = append(reasons, "secrets will be generated")
		case secretsChanged:
			reasons = append(reasons, "secrets changed")
		}
		if cur != nil {
			if old, ok := cur.services[sp.Name]; ok {
				if keys := changedKeys(old, want[sp.Name]); len(keys) > 0 {
					reasons = append(reasons, "config changed: "+strings.Join(keys, ", "))
				}
			} else {
				reasons = append(reasons, "config changed: service is new in the current Release")
			}
			if op, ok := cur.plans[sp.Name]; ok && !samePlanProxy(op, sp) {
				reasons = append(reasons, "proxy settings changed")
			}
		}
		if running > 0 && running != sp.Replicas {
			reasons = append(reasons, fmt.Sprintf("replicas %d → %d", running, sp.Replicas))
		}
		if len(reasons) == 0 && running == 0 {
			add("service", sp.Name, plan.ActionUpdate, false, "not running")
			continue
		}
		switch {
		case len(reasons) == 0:
			add("service", sp.Name, plan.ActionNoop, false)
		case sp.Proxy != nil:
			add("service", sp.Name, plan.ActionUpdate, false, reasons...)
		default:
			add("service", sp.Name, plan.ActionReplace, true, reasons...)
		}
	}

	// Services running but no longer in compose.
	inCompose := map[string]bool{}
	for _, sp := range plans {
		inCompose[sp.Name] = true
	}
	for _, name := range sortedKeys(byService) {
		if !inCompose[name] {
			add("service", name, plan.ActionDelete, false, "no longer in the compose file")
		}
	}

	// Routes.
	routes, _ := proxy.List(ctx, h) // missing Proxy: no routes
	have := map[string]proxy.Route{}
	for _, r := range routes {
		have[r.Service] = r
	}
	wantRoutes := map[string]bool{}
	for _, sp := range plans {
		if sp.Proxy == nil {
			continue
		}
		name := RouteName(d.App, d.Destination, sp.Name)
		wantRoutes[name] = true
		r, ok := have[name]
		switch {
		case !ok:
			add("route", name, plan.ActionCreate, false, "hosts "+hostsLabel(sp.Proxy.Hosts))
		case !sameHosts(r.Hosts, sp.Proxy.Hosts):
			add("route", name, plan.ActionUpdate, false, "hosts "+hostsLabel(r.Hosts)+" → "+hostsLabel(sp.Proxy.Hosts))
		}
	}
	for _, r := range StaleRoutes(routes, d.App, d.Destination, wantRoutes) {
		add("route", r.Service, plan.ActionDelete, false, "no longer matches a proxied Service")
	}
	return changes, nil
}

func servicePending(d *plan.Deploy, svc string, pending map[string]bool) bool {
	ext := d.Ext[svc]
	for _, ref := range ext.Secrets {
		if pending[ref.Key] {
			return true
		}
	}
	for name := range ext.Generate {
		if pending[name] {
			return true
		}
	}
	return false
}

func samePlanProxy(a, b servicePlan) bool {
	a.Image, b.Image = "", ""
	a.ReleaseCommand, b.ReleaseCommand = nil, nil
	a.DependsOn, b.DependsOn = nil, nil
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func normHosts(hs []string) []string {
	var out []string
	for _, h := range hs {
		if h != "" && h != "*" {
			out = append(out, h)
		}
	}
	slices.Sort(out)
	return out
}

func sameHosts(a, b []string) bool { return slices.Equal(normHosts(a), normHosts(b)) }

func hostsLabel(hs []string) string {
	if n := normHosts(hs); len(n) > 0 {
		return strings.Join(n, ",")
	}
	return "*"
}

// StaleRoutes returns Proxy routes that belong to this App Destination (name
// prefix <app>-<destination>- and a target container of its compose project)
// but are not in keep.
func StaleRoutes(routes []proxy.Route, app, dest string, keep map[string]bool) []proxy.Route {
	prefix := RouteName(app, dest, "")
	owner := release.ProjectName(app, dest) + "-"
	var out []proxy.Route
	for _, r := range routes {
		if !strings.HasPrefix(r.Service, prefix) || keep[r.Service] {
			continue
		}
		if r.Target != "" && !strings.HasPrefix(r.Target, owner) {
			continue // another App's route that happens to share the prefix
		}
		out = append(out, r)
	}
	return out
}
