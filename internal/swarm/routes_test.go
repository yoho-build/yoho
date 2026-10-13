package swarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

type routeGuardHost struct {
	remote.Host
	mutations []string
}

func (h *routeGuardHost) Run(ctx context.Context, c remote.Cmd) error {
	if !strings.HasPrefix(c.Script, "rm -rf ") || !strings.Contains(c.Script, "/lock'") {
		h.mutations = append(h.mutations, c.Script)
	}
	return h.Host.Run(ctx, c)
}
func (h *routeGuardHost) WriteFile(ctx context.Context, p string, b []byte, m os.FileMode, sudo bool) error {
	h.mutations = append(h.mutations, "write "+p)
	return h.Host.WriteFile(ctx, p, b, m, sudo)
}

func TestSwarmRoutePreflightBeforeAllMutations(t *testing.T) {
	for _, op := range []string{"deploy", "same-version", "plan", "rollback"} {
		for _, kind := range []string{"own rename", "catchall rename", "third-node catchall rename", "foreign hostname", "third-node conflict", "third-node query failure", "ownership query failure", "same-name foreign VIP", "same-name foreign task", "third-node foreign task", "ambiguous stack owner", "desired duplicates", "legacy rollback"} {
			t.Run(op+"/"+kind, func(t *testing.T) {
				withRoot(t)
				cause := errors.New("third Server Proxy unavailable")
				var guards []*routeGuardHost
				var bases []*fakeHost
				var queried []int
				for i := 0; i < 3; i++ {
					server := i
					queried = append(queried, 0)
					sim := newSim()
					base := &fakeHost{name: []string{"manager", "worker", "third"}[i], local: &remote.Local{}, respond: func(script string) (string, error) {
						if strings.Contains(script, "kamal-proxy list --json") {
							queried[server]++
							if strings.HasPrefix(kind, "third-node") && server != 2 {
								return "{}", nil
							}
							if kind == "third-node query failure" {
								return "", cause
							}
							switch kind {
							case "same-name foreign VIP", "ownership query failure":
								return `{"shop-production-web":{"hosts":["shop.example.com"],"targets":["foreign_web:3000"]}}`, nil
							case "catchall rename", "third-node catchall rename":
								return `{"shop-production-oldweb":{"hosts":["*"],"targets":["old_web:3000"]}}`, nil
							case "same-name foreign task", "third-node foreign task":
								return `{"shop-production-web":{"hosts":["shop.example.com"],"targets":["tasks.foreign_web:3000"]}}`, nil
							case "foreign hostname":
								return `{"other-production-web":{"hosts":["shop.example.com"],"targets":["foreign_web:3000"]}}`, nil
							default:
								return `{"shop-production-oldweb":{"hosts":["shop.example.com"],"targets":["old_web:3000"]}}`, nil
							}
						}
						if kind == "ownership query failure" && strings.Contains(script, "docker service inspect -f") && strings.Contains(script, "foreign_web") {
							return "", cause
						}
						if kind == "same-name foreign VIP" && strings.Contains(script, "docker service inspect -f") && strings.Contains(script, "foreign_web") {
							return "other|production", nil
						}
						if (kind == "same-name foreign task" || kind == "third-node foreign task") && strings.Contains(script, "docker service inspect -f") && strings.Contains(script, "'foreign_web'") && !strings.Contains(script, "tasks.foreign_web") {
							return "other|production", nil
						}
						if kind == "ambiguous stack owner" && strings.Contains(script, "docker service ls --filter") {
							return "shop-production|other", nil
						}
						return sim.respond(script)
					}}
					bases = append(bases, base)
					guards = append(guards, &routeGuardHost{Host: base})
				}
				d := testDeploy(t, guards[0], nil)
				if strings.Contains(kind, "catchall rename") {
					e := d.Ext["web"]
					e.Proxy = &config.ServiceProxy{}
					d.Ext["web"] = e
				}
				d.Servers = []plan.NamedHost{{Name: "manager", Host: guards[0]}, {Name: "worker", Host: guards[1]}, {Name: "third", Host: guards[2]}}
				hooks := 0
				d.Hook = func(context.Context, string, map[string]string) error { hooks++; return nil }
				if kind == "desired duplicates" {
					e := d.Ext["worker"]
					e.Proxy = &config.ServiceProxy{Hosts: []string{"other.example.com,shop.example.com"}}
					d.Ext["worker"] = e
				}
				// Saved rollback routes deliberately differ from local config.
				relDir := release.Dir(d.App, d.Destination, "saved")
				if err := os.MkdirAll(relDir, 0o700); err != nil {
					t.Fatal(err)
				}
				p := stackPlan{Stack: release.ProjectName(d.App, d.Destination), Services: []servicePlan{{Name: "web", Mode: "replicated", Replicas: 1, Proxy: d.Ext["web"].Proxy}}}
				if kind == "desired duplicates" {
					p.Services = append(p.Services, servicePlan{Name: "worker", Mode: "replicated", Replicas: 1, Proxy: d.Ext["worker"].Proxy})
				}
				if kind == "legacy rollback" {
					p.Services[0].Mode = ""
					p.Services[0].Replicas = 0
				}
				yaml := []byte("services:\n  web:\n    image: shop-web:saved\n")
				if err := os.WriteFile(filepath.Join(relDir, "compose.yaml"), yaml, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := deploy.WriteJSON(context.Background(), bases[0], filepath.Join(relDir, "plan.json"), p); err != nil {
					t.Fatal(err)
				}
				rel := &release.Release{App: d.App, Destination: d.Destination, Runtime: "swarm", Version: "saved", Status: "deployed", ComposeSHA256: deploy.SHA256Hex(yaml)}
				if err := deploy.WriteJSON(context.Background(), bases[0], filepath.Join(relDir, "release.json"), rel); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("releases/saved", filepath.Join(release.AppDir(d.App, d.Destination), "current")); err != nil {
					t.Fatal(err)
				}
				if op == "same-version" {
					d.Version = "saved"
				}
				if op == "rollback" {
					e := d.Ext["web"]
					e.Proxy = &config.ServiceProxy{Hosts: []string{"unrelated.example.com"}}
					d.Ext["web"] = e
				}
				before := probeAppFiles(t)
				var err error
				switch op {
				case "deploy", "same-version":
					_, err = (Runtime{}).Deploy(context.Background(), d)
				case "plan":
					_, err = (Runtime{}).Diff(context.Background(), d)
				case "rollback":
					_, err = (Runtime{}).Rollback(context.Background(), d, "saved")
				}
				if err == nil {
					t.Fatal("preflight accepted blocked route")
				}
				if (kind == "third-node query failure" || kind == "ownership query failure") && !errors.Is(err, cause) {
					t.Fatalf("lost query cause: %v", err)
				}
				var conflict *proxy.RouteConflictError
				if kind == "own rename" || strings.Contains(kind, "catchall rename") || kind == "foreign hostname" || kind == "third-node conflict" || kind == "desired duplicates" || kind == "legacy rollback" {
					if !errors.As(err, &conflict) {
						t.Fatalf("not a route conflict: %v", err)
					}
				}
				var owner *release.OwnershipError
				if strings.Contains(kind, "foreign VIP") || strings.Contains(kind, "foreign task") || kind == "ambiguous stack owner" {
					if !errors.As(err, &owner) {
						t.Fatalf("ownership guard lost: %v", err)
					}
				}
				if strings.HasPrefix(kind, "third-node") && !reflect.DeepEqual(queried, []int{1, 1, 1}) {
					t.Fatalf("not every Destination Server checked: %v", queried)
				}
				for i, h := range guards {
					if len(h.mutations) != 0 {
						t.Fatalf("Server %d changed before preflight: %v", i, h.mutations)
					}
					if strings.Contains(bases[i].all(), "'remove'") {
						t.Fatal("route destructively removed")
					}
				}
				if hooks != 0 {
					t.Fatal("Hook ran before preflight")
				}
				if after := probeAppFiles(t); !reflect.DeepEqual(before, after) {
					t.Fatalf("retained/generated/release files changed: before=%v after=%v", before, after)
				}
				if op != "rollback" && kind == "desired duplicates" {
					for _, base := range bases {
						if len(base.scripts) != 0 {
							t.Fatalf("desired duplicate read Server: %s", base.all())
						}
					}
				}
			})
		}
	}
}

func TestSwarmRoutePreflightAllowedAndCanceled(t *testing.T) {
	for _, kind := range []string{"same-name update", "no proxy", "scaled zero", "missing Proxy", "global", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			withRoot(t)
			queries := 0
			h := newFake(newSim())
			respond := h.respond
			h.respond = func(script string) (string, error) {
				if strings.Contains(script, "kamal-proxy list --json") {
					queries++
					if kind == "no proxy" || kind == "scaled zero" {
						return "", errors.New("must not query")
					}
					if kind == "missing Proxy" {
						return "null", nil
					}
					return `{"shop-production-web":{"hosts":["shop.example.com"],"targets":["yoho-shop-production_web:3000"]}}`, nil
				}
				return respond(script)
			}
			d := testDeploy(t, h, nil)
			p := stackPlan{Services: []servicePlan{{Name: "web", Mode: "replicated", Replicas: 1, Proxy: d.Ext["web"].Proxy}}}
			if kind == "no proxy" {
				p.Services[0].Proxy = nil
			}
			if kind == "scaled zero" {
				p.Services[0].Replicas = 0
			}
			if kind == "global" {
				p.Services[0].Mode = "global"
				p.Services[0].Replicas = 0
			}
			if kind == "legacy" {
				p.Services[0].Mode = ""
				p.Services[0].Replicas = 0
			}
			routes, err := planRoutes(d.App, d.Destination, p)
			if err != nil {
				t.Fatal(err)
			}
			if err := preflightRoutes(context.Background(), d, routes); err != nil {
				t.Fatal(err)
			}
			if (kind == "no proxy" || kind == "scaled zero") && queries != 0 {
				t.Fatal("unneeded Proxy query")
			}
			if (kind == "global" || kind == "legacy") && queries != 1 {
				t.Fatal("global/legacy route omitted")
			}
		})
	}
	withRoot(t)
	h := newFake(newSim())
	d := testDeploy(t, h, nil)
	routes, err := proxy.ProjectRoutes(d.App, d.Destination, d.Project, d.Ext, "swarm")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := preflightRoutes(ctx, d, routes); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestSwarmDesiredRoutesMatchReplicaModes(t *testing.T) {
	withRoot(t)
	d := testDeploy(t, newFake(newSim()), nil)
	svc := d.Project.Services["web"]
	zero := 0
	for _, mode := range []string{"replicated", "global", "replicated-job", "global-job"} {
		svc.Deploy = &types.DeployConfig{Mode: mode, Replicas: &zero}
		d.Project.Services["web"] = svc
		routes, err := proxy.ProjectRoutes(d.App, d.Destination, d.Project, d.Ext, "swarm")
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		if mode == "global" {
			want = 1
		}
		if len(routes) != want {
			t.Fatalf("mode %s: %v", mode, routes)
		}
	}
}
