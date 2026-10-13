package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
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
func (h *routeGuardHost) WriteFile(ctx context.Context, p string, b []byte, mode os.FileMode, sudo bool) error {
	h.mutations = append(h.mutations, "write "+p)
	return h.Host.WriteFile(ctx, p, b, mode, sudo)
}

func routeAppFiles(t *testing.T) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.Walk(release.Root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		if strings.Contains(p, "/lock/") {
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(p)
			files[p] = "link:" + target
			return err
		}
		b, err := os.ReadFile(p)
		files[p] = string(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestComposeRoutePreflightBeforeAllMutations(t *testing.T) {
	for _, operation := range []string{"deploy", "same-version", "plan", "rollback"} {
		for _, kind := range []string{"own rename", "catchall rename", "foreign hostname", "query failure", "malformed query", "ownership query failure", "same-name foreign owner", "ambiguous project owner", "desired duplicates"} {
			t.Run(operation+"/"+kind, func(t *testing.T) {
				withRoot(t)
				cause := errors.New("Proxy query unavailable")
				base := &dockerHost{local: &remote.Local{}, respond: func(script string) (string, error) {
					if strings.Contains(script, "kamal-proxy list --json") {
						switch kind {
						case "query failure":
							return "", cause
						case "malformed query":
							return "not json", nil
						case "same-name foreign owner", "ownership query failure":
							return `{"shop-production-web":{"hosts":["shop.example.com"],"targets":["foreign-web:3000"]}}`, nil
						case "catchall rename":
							return `{"shop-production-oldweb":{"hosts":["*"],"targets":["old-web:3000"]}}`, nil
						case "foreign hostname":
							return `{"other-production-web":{"hosts":["shop.example.com"],"targets":["foreign-web:3000"]}}`, nil
						default:
							return `{"shop-production-oldweb":{"hosts":["shop.example.com"],"targets":["old-web:3000"]}}`, nil
						}
					}
					if kind == "ownership query failure" && strings.Contains(script, "docker inspect -f") && strings.Contains(script, "foreign-web") {
						return "", cause
					}
					if kind == "same-name foreign owner" && strings.Contains(script, "docker inspect -f") && strings.Contains(script, "foreign-web") {
						return "other|production", nil
					}
					if kind == "ambiguous project owner" && strings.Contains(script, "docker ps -a --filter") {
						return "shop-production|other", nil
					}
					return "", nil
				}}
				h := &routeGuardHost{Host: base}
				d := testDeploy(t, h, nil)
				if kind == "catchall rename" {
					e := d.Ext["web"]
					e.Proxy = &config.ServiceProxy{}
					d.Ext["web"] = e
				}
				if operation == "same-version" {
					d.Version = "saved"
				}
				hooks := 0
				d.Hook = func(context.Context, string, map[string]string) error { hooks++; return nil }
				if kind == "desired duplicates" {
					e := d.Ext["cloudflared"]
					e.Proxy = &config.ServiceProxy{Hosts: []string{"other.example.com,shop.example.com"}}
					d.Ext["cloudflared"] = e
				}
				// Seed an existing record and current link. On rollback, local
				// config deliberately claims an unrelated hostname: only saved
				// plans may determine the rollback's desired route reservations.
				relDir := release.Dir(d.App, d.Destination, "saved")
				if err := os.MkdirAll(relDir, 0o700); err != nil {
					t.Fatal(err)
				}
				plans := []servicePlan{{Name: "web", Replicas: 1, Proxy: d.Ext["web"].Proxy}}
				if kind == "desired duplicates" {
					plans = append(plans, servicePlan{Name: "cloudflared", Replicas: 1, Proxy: d.Ext["cloudflared"].Proxy})
				}
				yaml := []byte("services:\n  web:\n    image: shop-web:saved\n  cloudflared:\n    image: cloudflare/cloudflared:latest\n")
				if err := writeReleaseFiles(context.Background(), base, relDir, yaml, plans); err != nil {
					t.Fatal(err)
				}
				rel := &release.Release{App: d.App, Destination: d.Destination, Runtime: "compose", Version: "saved", Status: "deployed", ComposeSHA256: sha256Hex(yaml)}
				if err := writeJSON(context.Background(), base, filepath.Join(relDir, "release.json"), rel); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("releases/saved", filepath.Join(release.AppDir(d.App, d.Destination), "current")); err != nil {
					t.Fatal(err)
				}
				if operation == "rollback" {
					e := d.Ext["web"]
					e.Proxy = &config.ServiceProxy{Hosts: []string{"unrelated.example.com"}}
					d.Ext["web"] = e
				}
				before := routeAppFiles(t)
				var err error
				switch operation {
				case "deploy", "same-version":
					_, err = (Compose{}).Deploy(context.Background(), d)
				case "plan":
					_, err = (Compose{}).Diff(context.Background(), d)
				case "rollback":
					_, err = (Compose{}).Rollback(context.Background(), d, "saved")
				}
				if err == nil {
					t.Fatal("preflight allowed a blocked route")
				}
				if (kind == "query failure" || kind == "ownership query failure") && !errors.Is(err, cause) {
					t.Fatalf("lost query cause: %v", err)
				}
				var conflict *proxy.RouteConflictError
				if kind == "own rename" || kind == "catchall rename" || kind == "foreign hostname" || kind == "desired duplicates" {
					if !errors.As(err, &conflict) {
						t.Fatalf("not a route conflict: %v", err)
					}
				}
				var owner *release.OwnershipError
				if (kind == "same-name foreign owner" || kind == "ambiguous project owner") && !errors.As(err, &owner) {
					t.Fatalf("ownership guard weakened: %v", err)
				}
				if hooks != 0 || len(h.mutations) != 0 {
					t.Fatalf("preflight mutated Services/files/jobs/Proxy: hooks=%d commands=%v", hooks, h.mutations)
				}
				if after := routeAppFiles(t); !reflect.DeepEqual(before, after) {
					t.Fatalf("App files changed: before=%v after=%v", before, after)
				}
				if strings.Contains(base.all(), "'remove'") {
					t.Fatal("preflight deleted a route")
				}
				if operation != "rollback" && kind == "desired duplicates" && len(base.scripts) != 0 {
					t.Fatalf("desired duplicate queried Server before local validation: %s", base.all())
				}
			})
		}
	}
}

func TestComposeRoutePreflightAllowedAndCanceled(t *testing.T) {
	for _, kind := range []string{"same-name update", "missing Proxy", "no proxy", "scaled zero", "catchall specific", "wildcard specific", "path route"} {
		t.Run(kind, func(t *testing.T) {
			withRoot(t)
			queries := 0
			h := &dockerHost{local: &remote.Local{}, respond: func(script string) (string, error) {
				if strings.Contains(script, "kamal-proxy list --json") {
					queries++
					switch kind {
					case "same-name update":
						return `{"shop-production-web":{"hosts":["shop.example.com"],"targets":["owned-web:3000"]}}`, nil
					case "catchall specific":
						return `{"other":{"hosts":[""]}}`, nil
					case "wildcard specific":
						return `{"other":{"hosts":["*.example.com"]}}`, nil
					case "path route":
						return `{"other":{"hosts":["shop.example.com"],"path_prefixes":["/api"]}}`, nil
					case "no proxy", "scaled zero":
						return "", errors.New("must not query Proxy")
					default:
						return "null", nil
					}
				}
				return "", nil
			}}
			d := testDeploy(t, h, nil)
			plans := []servicePlan{{Name: "web", Replicas: 1, Proxy: d.Ext["web"].Proxy}}
			if kind == "no proxy" {
				plans[0].Proxy = nil
			}
			if kind == "scaled zero" {
				plans[0].Replicas = 0
			}
			if err := checkOwnership(context.Background(), h, d.App, d.Destination, plans); err != nil {
				t.Fatal(err)
			}
			if (kind == "no proxy" || kind == "scaled zero") && queries != 0 {
				t.Fatal("unneeded Proxy query")
			}
		})
	}
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := checkOwnership(ctx, h, "shop", "production", []servicePlan{{Name: "web", Replicas: 1, Proxy: &config.ServiceProxy{}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
}

func TestRouteOwnershipDiscoveryLocalShell(t *testing.T) {
	dir := t.TempDir()
	shim := "#!/bin/sh\nset -eu\ncase \"$1\" in\nps)\n case \"$ROUTE_OWNER_TEST\" in daemon) exit 42;; vanished) exit 0;; *) printf 'id123|owned-web\\n';; esac;;\ninspect)\n case \"$ROUTE_OWNER_TEST\" in inspect) exit 43;; unlabeled) printf '|\\n';; foreign) printf 'other|production\\n';; *) printf 'shop|production\\n';; esac;;\n*) exit 99;;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(shim), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, mode := range []string{"owned", "foreign", "daemon", "inspect", "vanished", "unlabeled"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("ROUTE_OWNER_TEST", mode)
			err := CheckRouteOwnership(context.Background(), &remote.Local{}, proxy.Route{Service: "shop-production-web", Target: "owned-web:3000"}, "shop", "production")
			wantErr := mode == "foreign" || mode == "daemon" || mode == "inspect"
			if (err != nil) != wantErr {
				t.Fatalf("mode=%s err=%v", mode, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CheckRouteOwnership(ctx, &remote.Local{}, proxy.Route{Target: "owned-web:3000"}, "shop", "production"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestComposeDesiredDuplicateCompilation(t *testing.T) {
	withRoot(t)
	d := testDeploy(t, &dockerHost{local: &remote.Local{}}, nil)
	e := d.Ext["cloudflared"]
	e.Proxy = &config.ServiceProxy{Hosts: []string{"shop.example.com"}}
	d.Ext["cloudflared"] = e
	if err := validate(d); err == nil {
		t.Fatal("duplicate route hosts accepted")
	}
	if _, _, err := compile(d, "primary", planGenDir, nil, planKey, ""); err == nil {
		t.Fatal("compiler accepted duplicate desired route hosts")
	}
	// Retained legacy rows are still compared by their saved hosts.
	b, _ := json.Marshal([]servicePlan{{Name: "web", Replicas: 1, Proxy: &config.ServiceProxy{}}, {Name: "newweb", Replicas: 1, Proxy: &config.ServiceProxy{}}})
	var plans []servicePlan
	if err := json.Unmarshal(b, &plans); err != nil {
		t.Fatal(err)
	}
	if err := checkOwnership(context.Background(), d.Servers[0].Host, d.App, d.Destination, plans); err == nil {
		t.Fatal("saved catch-all duplicates accepted")
	}
}
