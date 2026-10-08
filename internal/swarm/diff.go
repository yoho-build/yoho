package swarm

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// genSegment strips the secrets-generation directory from compiled paths so
// a plan compares stable env_file locations. Deploy names that directory
// with the current time; the file inside it does not.
var genSegment = regexp.MustCompile(`(/secrets/)[^/\s'"]+/`)

func init() {
	var _ plan.Planner = Runtime{}
}

// Diff implements plan.Planner. It reads the current Release and the
// Destination's hmac key and generated secrets. It does not write secrets,
// files, or Swarm objects. A missing hmac key, or no current Release, is a
// first deploy: every Service and route is a create.
func (Runtime) Diff(ctx context.Context, d *plan.Deploy) ([]plan.Change, error) {
	if err := validate(d); err != nil {
		return nil, err
	}
	h := d.Servers[0].Host
	dir := release.AppDir(d.App, d.Destination)
	key, haveKey, err := readHMAC(ctx, h, dir)
	if err != nil {
		return nil, err
	}
	generated, err := loadGenerated(ctx, h, dir, d)
	if err != nil {
		return nil, err
	}
	_, pending, err := mergeSecrets(d, generated)
	if err != nil {
		return nil, err
	}
	if !haveKey {
		return firstChanges(d, pending), nil
	}
	c, err := compileDesired(ctx, d, key, generated)
	if err != nil {
		return nil, err
	}
	curYAML, err := readCurrentStack(ctx, h, d.App, d.Destination)
	if err != nil {
		return nil, err
	}
	if curYAML == nil {
		return firstChanges(d, pending), nil
	}
	return compareStack(d, curYAML, c, pending)
}

// compileDesired compiles the stack the way Deploy would, using secret
// values already on the Server. The generation directory is a placeholder;
// comparisons ignore that path segment.
func compileDesired(ctx context.Context, d *plan.Deploy, key []byte, generated map[string]string) (*compiled, error) {
	svcSecrets, _, err := mergeSecrets(d, generated)
	if err != nil {
		return nil, err
	}
	pin, err := checkSwarm(ctx, d)
	if err != nil {
		return nil, err
	}
	return compile(d, compileInput{
		PinHost:       pin,
		GenerationDir: release.SecretsDir(d.App, d.Destination, "planned"),
		SvcSecrets:    svcSecrets,
		HMACKey:       key,
		DockerVersion: deploy.DockerServerVersion(ctx, d.Servers[0].Host),
	})
}

func firstChanges(d *plan.Deploy, pending map[string][]string) []plan.Change {
	srv := d.Servers[0].Name
	var out []plan.Change
	for _, name := range sortedKeys(d.Project.Services) {
		out = append(out, plan.Change{
			Kind: "service", Name: name, Server: srv, Action: plan.ActionCreate,
			Reasons: willGenerate(pending[name]),
		})
	}
	for _, name := range sortedKeys(d.Project.Services) {
		if d.Ext[name].Proxy == nil {
			continue
		}
		out = append(out, plan.Change{
			Kind: "route", Name: deploy.RouteName(d.App, d.Destination, name), Action: plan.ActionCreate,
		})
	}
	return out
}

func compareStack(d *plan.Deploy, curYAML []byte, c *compiled, pending map[string][]string) ([]plan.Change, error) {
	var curDoc map[string]any
	if err := yaml.Unmarshal(curYAML, &curDoc); err != nil {
		return nil, fmt.Errorf("parse current stack: %w", err)
	}
	curSvcs := serviceMaps(curDoc)
	desSvcs := serviceMaps(c.Doc)
	byName := map[string]servicePlan{}
	for _, sp := range c.Plan.Services {
		byName[sp.Name] = sp
	}
	names := map[string]bool{}
	for n := range curSvcs {
		names[n] = true
	}
	for n := range desSvcs {
		names[n] = true
	}
	srv := d.Servers[0].Name
	var out []plan.Change
	curRoute := map[string]bool{}
	desRoute := map[string]bool{}
	for _, name := range sortedKeys(names) {
		curS, inC := curSvcs[name]
		desS, inD := desSvcs[name]
		switch {
		case inD && !inC:
			out = append(out, plan.Change{
				Kind: "service", Name: name, Server: srv, Action: plan.ActionCreate,
				Reasons: willGenerate(pending[name]),
			})
		case inC && !inD:
			out = append(out, plan.Change{Kind: "service", Name: name, Server: srv, Action: plan.ActionDelete})
		default:
			// Desired is the in-memory document. The current Release is the
			// encoded stack file, which escapes `$`. Round-trip the desired
			// Service through that encoding before comparing.
			deployed, err := asDeployed(desS)
			if err != nil {
				return nil, err
			}
			reasons, err := diffReasons(curS, deployed)
			if err != nil {
				return nil, fmt.Errorf("service %s: %w", name, err)
			}
			if extra := willGenerate(pending[name]); len(extra) > 0 {
				reasons = append(reasons, extra...)
			}
			ch := plan.Change{Kind: "service", Name: name, Server: srv, Action: plan.ActionNoop, Reasons: reasons}
			if len(reasons) > 0 {
				ch.Action = plan.ActionUpdate
				if byName[name].Stateful {
					ch.Action = plan.ActionReplace
					ch.Downtime = true
				}
			}
			out = append(out, ch)
		}
		if inC && onProxyNet(curS) {
			curRoute[name] = true
		}
		if inD && byName[name].Proxy != nil {
			desRoute[name] = true
		}
	}
	routeNames := map[string]bool{}
	for n := range curRoute {
		routeNames[n] = true
	}
	for n := range desRoute {
		routeNames[n] = true
	}
	for _, name := range sortedKeys(routeNames) {
		route := deploy.RouteName(d.App, d.Destination, name)
		switch {
		case desRoute[name] && !curRoute[name]:
			out = append(out, plan.Change{Kind: "route", Name: route, Action: plan.ActionCreate})
		case curRoute[name] && !desRoute[name]:
			out = append(out, plan.Change{Kind: "route", Name: route, Action: plan.ActionDelete})
		}
	}
	return out, nil
}

// asDeployed returns the Service as `docker stack deploy` would read it
// back from the stack file.
func asDeployed(svc map[string]any) (map[string]any, error) {
	b, err := encode(map[string]any{"services": map[string]any{"s": svc}})
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	m, ok := serviceMaps(doc)["s"]
	if !ok {
		return nil, errors.New("round-trip dropped the service")
	}
	return m, nil
}

func diffReasons(cur, des map[string]any) ([]string, error) {
	a, err := normService(cur)
	if err != nil {
		return nil, err
	}
	b, err := normService(des)
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for k := range a {
		keys[k] = true
	}
	for k := range b {
		keys[k] = true
	}
	var reasons, cfg []string
	secretsChanged := false
	for _, k := range sortedKeys(keys) {
		if canon(a[k]) == canon(b[k]) {
			continue
		}
		switch k {
		case "image":
			reasons = append(reasons, fmt.Sprintf("image %s → %s", asString(a[k]), asString(b[k])))
		case "secrets":
			secretsChanged = true
		default:
			cfg = append(cfg, k)
		}
	}
	if secretsChanged {
		reasons = append(reasons, "secrets changed")
	}
	if len(cfg) > 0 {
		reasons = append(reasons, "config changed: "+strings.Join(cfg, ", "))
	}
	return reasons, nil
}

func normService(s map[string]any) (map[string]any, error) {
	if s == nil {
		s = map[string]any{}
	}
	raw, err := yaml.Marshal(s)
	if err != nil {
		return nil, err
	}
	raw = []byte(genSegment.ReplaceAllString(string(raw), "${1}"))
	var out map[string]any
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	if out == nil {
		out = map[string]any{}
	}
	return out, nil
}

func canon(v any) string {
	if v == nil {
		return ""
	}
	b, err := yaml.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if v == nil {
		return ""
	}
	return strings.TrimSpace(canon(v))
}

func willGenerate(names []string) []string {
	if len(names) == 0 {
		return nil
	}
	return []string{"will be generated"}
}

func serviceMaps(doc map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for name, v := range mapOf(doc, "services") {
		m, _ := v.(map[string]any)
		if m == nil {
			m = map[string]any{}
		}
		out[name] = m
	}
	return out
}

func onProxyNet(svc map[string]any) bool {
	_, ok := mapOf(svc, "networks")[proxy.Network]
	return ok
}

// mergeSecrets is the read-only counterpart of deploy.ServiceSecrets.
// Generated values that are not on the Server yet are reported as pending
// (container secret names) instead of failing; Deploy would create them.
func mergeSecrets(d *plan.Deploy, generated map[string]string) (map[string]map[string]string, map[string][]string, error) {
	if d.Project == nil {
		return nil, nil, errors.New("no compose project")
	}
	out := map[string]map[string]string{}
	pending := map[string][]string{}
	for _, svc := range sortedKeys(d.Project.Services) {
		m := map[string]string{}
		for k, v := range d.ServiceSecrets[svc] {
			m[k] = v
		}
		ext := d.Ext[svc]
		var pend []string
		for _, ref := range ext.Secrets {
			if _, ok := m[ref.Name]; ok {
				continue
			}
			v, ok := generated[ref.Key]
			if !ok {
				pend = append(pend, ref.Name)
				continue
			}
			m[ref.Name] = v
		}
		for _, name := range sortedKeys(ext.Generate) {
			if _, ok := m[name]; ok {
				continue
			}
			v, ok := generated[name]
			if !ok {
				pend = append(pend, name)
				continue
			}
			m[name] = v
		}
		if len(m) > 0 {
			out[svc] = m
		}
		if len(pend) > 0 {
			sort.Strings(pend)
			pending[svc] = pend
		}
	}
	return out, pending, nil
}

func loadGenerated(ctx context.Context, h remote.Host, dir string, d *plan.Deploy) (map[string]string, error) {
	kinds, err := deploy.GeneratedDecls(d)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, name := range sortedKeys(kinds) {
		b, ok, err := readOptional(ctx, h, path.Join(dir, "generated", name))
		if err != nil {
			return nil, err
		}
		if ok {
			out[name] = string(b)
		}
	}
	return out, nil
}

func readHMAC(ctx context.Context, h remote.Host, dir string) ([]byte, bool, error) {
	b, ok, err := readOptional(ctx, h, path.Join(dir, "hmac.key"))
	if err != nil || !ok {
		return nil, false, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(b)))
	if err != nil || len(key) < 16 {
		return nil, false, fmt.Errorf("invalid hmac.key in %s", dir)
	}
	return key, true, nil
}

func readCurrentStack(ctx context.Context, h remote.Host, app, dest string) ([]byte, error) {
	dir := release.AppDir(app, dest)
	out, err := h.Output(ctx, remote.Cmd{Script: "readlink " + remote.Quote(path.Join(dir, "current")) + " 2>/dev/null || true"})
	if err != nil {
		return nil, err
	}
	ver := path.Base(strings.TrimSpace(out))
	if ver == "" || ver == "." {
		return nil, nil
	}
	b, ok, err := readOptional(ctx, h, path.Join(release.Dir(app, dest, ver), "compose.yaml"))
	if err != nil || !ok {
		return nil, err
	}
	return b, nil
}

func readOptional(ctx context.Context, h remote.Host, p string) ([]byte, bool, error) {
	b, err := h.ReadFile(ctx, p, false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return b, true, nil
}
