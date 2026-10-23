package gate

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/composefile"
)

const project = "yoho-shop-production"

func load(t *testing.T, yml string) *composefile.Result {
	t.Helper()
	r, err := composefile.LoadResolved(context.Background(), []byte(yml), t.TempDir(), project)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return r
}

func TestPolicyAllowsPlainApp(t *testing.T) {
	r := load(t, `
services:
  web:
    image: yoho/shop-production-web:abc
    environment: {A: "1"}
    volumes: [data:/data, {type: tmpfs, target: /tmp}]
    security_opt: [no-new-privileges:true]
    cap_drop: [ALL]
    ports: ["8443:443"]
    tmpfs: [/run]
    shm_size: 64m
    ulimits: {nofile: {soft: 1024, hard: 65536}}
    x-yoho: {proxy: {hosts: [shop.example.com]}, secrets: [DB_URL]}
  db:
    image: postgres:17
    volumes: [pg:/var/lib/postgresql/data]
    container_name: yoho-shop-production-db
    x-yoho: {stateful: true}
volumes: {data: {}, pg: {}}
configs: {motd: {content: hello}}
`)
	pol := Policy{Project: project, Built: map[string]string{"web": "yoho/shop-production-web:abc"}, Routes: []string{"shop.example.com"},
		Reserved: []string{"yoho/"}, Limits: Limits{Ports: []int{8443}}}
	if err := pol.Check(r.Project, r.Ext); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyRefuses(t *testing.T) {
	pol := Policy{Project: project, Built: map[string]string{"web": "yoho/shop-production-web:abc"}, Routes: []string{"shop.example.com", ""},
		Reserved: []string{"yoho/", "ghcr.io/acme/"}, Limits: Limits{Ports: []int{8080}}}
	cases := []struct{ name, yml, want string }{
		{"privileged", "services: {a: {image: x, privileged: true}}", "privileged is not allowed"},
		{"network host", "services: {a: {image: x, network_mode: host}}", `network_mode "host"`},
		{"network container", "services: {a: {image: x, network_mode: \"container:other\"}}", `network_mode "container:other"`},
		{"network service", "services: {a: {image: x, network_mode: \"service:b\"}, b: {image: x}}", `network_mode "service:b"`},
		{"pid host", "services: {a: {image: x, pid: host}}", `pid "host"`},
		{"ipc host", "services: {a: {image: x, ipc: host}}", `ipc "host"`},
		{"uts host", "services: {a: {image: x, uts: host}}", `uts "host"`},
		{"userns host", "services: {a: {image: x, userns_mode: host}}", `userns_mode "host"`},
		{"cgroup host", "services: {a: {image: x, cgroup: host}}", `cgroup "host"`},
		{"cgroup_parent", "services: {a: {image: x, cgroup_parent: /x}}", "cgroup_parent"},
		{"cap_add", "services: {a: {image: x, cap_add: [NET_ADMIN]}}", "cap_add is not allowed (NET_ADMIN)"},
		{"devices", "services: {a: {image: x, devices: [\"/dev/sda:/dev/sda\"]}}", "devices are not allowed"},
		{"device cgroup rules", "services: {a: {image: x, device_cgroup_rules: [\"c 1:3 mr\"]}}", "device_cgroup_rules"},
		{"gpus", "services: {a: {image: x, gpus: all}}", "gpus are not allowed"},
		{"seccomp unconfined", "services: {a: {image: x, security_opt: [seccomp=unconfined]}}", `security_opt "seccomp=unconfined"`},
		{"apparmor unconfined", "services: {a: {image: x, security_opt: [\"apparmor:unconfined\"]}}", `security_opt "apparmor:unconfined"`},
		{"label disable", "services: {a: {image: x, security_opt: [\"label:disable\"]}}", `security_opt "label:disable"`},
		{"seccomp file", "services: {a: {image: x, security_opt: [seccomp=/etc/x.json]}}", "security_opt"},
		{"volumes_from", "services: {a: {image: x, volumes_from: [b]}, b: {image: x}}", "volumes_from"},
		{"docker socket", "services: {a: {image: x, volumes: [\"/var/run/docker.sock:/var/run/docker.sock\"]}}", "bind mount /var/run/docker.sock"},
		{"containerd socket", "services: {a: {image: x, volumes: [\"/run/containerd/containerd.sock:/c.sock\"]}}", "bind mount /run/containerd"},
		{"bind root", "services: {a: {image: x, volumes: [\"/:/host\"]}}", "bind mount /:/host"},
		{"bind other app", "services: {a: {image: x, volumes: [\"/var/lib/yoho/apps/other/production/secrets:/s:ro\"]}}", "bind mount /var/lib/yoho/apps/other"},
		{"oom shield", "services: {a: {image: x, oom_score_adj: -1000}}", "oom_score_adj"},
		{"api socket", "services: {a: {image: x, use_api_socket: true}}", "use_api_socket"},
		{"runtime", "services: {a: {image: x, runtime: runsc}}", `runtime "runsc"`},
		{"container_name", "services: {a: {image: x, container_name: postgres}}", `container_name "postgres" must start with yoho-shop-production-`},
		{"external_links", "services: {a: {image: x, external_links: [other_db]}}", "external_links"},
		{"reserved label", "services: {a: {image: x, labels: {com.docker.compose.project: other}}}", "label com.docker.compose.project is reserved"},
		{"yoho label", "services: {a: {image: x, labels: {yoho.app: other}}}", "label yoho.app is reserved"},
		{"external volume", "services: {a: {image: x, volumes: [v:/v]}}\nvolumes: {v: {external: true, name: yoho-other-production_data}}", "external volumes"},
		{"volume driver_opts bind", "services: {a: {image: x, volumes: [v:/v]}}\nvolumes: {v: {driver_opts: {type: none, o: bind, device: /}}}", "driver_opts are not allowed"},
		{"volume driver", "services: {a: {image: x, volumes: [v:/v]}}\nvolumes: {v: {driver: rexray}}", `driver "rexray"`},
		{"volume name", "services: {a: {image: x, volumes: [v:/v]}}\nvolumes: {v: {name: other_data}}", `name "other_data" must start with yoho-shop-production_`},
		{"external network", "services: {a: {image: x, networks: [n]}}\nnetworks: {n: {external: true, name: yoho-other-production_default}}", "external network"},
		{"host network driver", "services: {a: {image: x, networks: [n]}}\nnetworks: {n: {driver: macvlan}}", `driver "macvlan"`},
		{"network name", "services: {a: {image: x, networks: [n]}}\nnetworks: {n: {name: other_default}}", `name "other_default"`},
		{"top-level secret file", "services: {a: {image: x, secrets: [s]}}\nsecrets: {s: {file: /etc/shadow}}", "top-level secrets are not allowed"},
		{"config file", "services: {a: {image: x, configs: [c]}}\nconfigs: {c: {file: /etc/shadow}}", "only inline content"},
		{"config environment", "services: {a: {image: x}}\nconfigs: {c: {environment: HOME}}", "only inline content"},
		{"privileged hook", "services: {a: {image: x, post_start: [{command: [id], privileged: true}]}}", "privileged lifecycle hooks"},
		{"privileged pre_start", "services: {a: {image: x, pre_start: [{image: y, privileged: true}]}}", "pre_start[0]: privileged"},
		{"route host", "services: {a: {image: x, x-yoho: {proxy: {hosts: [bank.example.com]}}}}", "Proxy host bank.example.com is not allowed"},
		{"shared yoho network", "services: {a: {image: x, networks: [default, yoho]}}\nnetworks: {yoho: {external: true}}", `external network "yoho"`},
		{"other app local image", "services: {a: {image: \"yoho/otherapp-production-web:leak\"}}", "Yoho-built image"},
		{"other app registry image", "services: {a: {image: ghcr.io/acme/other-production-web:v1}}", "Yoho-built image"},
		{"own old local image", "services: {a: {image: yoho/shop-production-web:old}}", "Yoho-built image"},
		{"pre_start other app image", "services: {a: {image: x, pre_start: [{image: \"yoho/other-production-web:v\"}]}}", "pre_start[0]: image"},
		{"image volume", "services: {a: {image: x, volumes: [{type: image, source: \"yoho/other-production-web:v\", target: /o}]}}", "image volume /o"},
		{"random host port", "services: {a: {image: x, ports: [\"80\"]}}", "random host port"},
		{"unlisted host port", "services: {a: {image: x, ports: [\"22:22\"]}}", "host port 22 is not allowed"},
		{"port range", "services: {a: {image: x, ports: [\"8080-8081:80\"]}}", "host port 8081"},
		{"shm", "services: {a: {image: x, shm_size: 8g}}", "shm_size"},
		{"tmpfs short", "services: {a: {image: x, tmpfs: [\"/t:size=64g\"]}}", "tmpfs /t:size=64g"},
		{"tmpfs long", "services: {a: {image: x, volumes: [{type: tmpfs, target: /t, tmpfs: {size: 64g}}]}}", "tmpfs /t: size"},
		{"ulimit unlimited", "services: {a: {image: x, ulimits: {memlock: -1}}}", "ulimit memlock"},
		{"ulimit huge", "services: {a: {image: x, ulimits: {nofile: {soft: 1024, hard: 99999999}}}}", "ulimit nofile"},
		{"pids unlimited", "services: {a: {image: x, pids_limit: -1}}", "pids_limit -1"},
		{"pids huge", "services: {a: {image: x, pids_limit: 1000000}}", "pids_limit 1000000"},
		{"no services", "services: {}", "no Services"},
		{"network_mode bridge", "services: {a: {image: x, network_mode: bridge}}", `network_mode "bridge"`},
		{"network_mode default", "services: {a: {image: x, network_mode: default}}", `network_mode "default"`},
		{"default network on docker0", "services: {a: {image: x}}\nnetworks: {default: {name: bridge, external: true}}", "external network"},
		{"pull_policy always", "services: {a: {image: x, pull_policy: always}}", `pull_policy "always"`},
		{"pull_policy build", "services: {a: {image: x, pull_policy: build}}", `pull_policy "build"`},
		{"storage_opt", "services: {a: {image: x, storage_opt: {size: 1t}}}", "storage_opt"},
		{"mem_limit", "services: {a: {image: x, mem_limit: 16g}}", "mem_limit"},
		{"deploy memory", "services: {a: {image: x, deploy: {resources: {limits: {memory: 16g}}}}}", "deploy.resources.limits.memory"},
		{"memswap", "services: {a: {image: x, memswap_limit: -1}}", "memswap_limit"},
		{"cpus", "services: {a: {image: x, cpus: 8}}", "cpus 8"},
		{"deploy cpus", "services: {a: {image: x, deploy: {resources: {limits: {cpus: '8'}}}}}", "deploy.resources.limits.cpus"},
		{"cpu_quota", "services: {a: {image: x, cpu_quota: 900000}}", "cpu_quota"},
		{"scale", "services: {a: {image: x, scale: 9}}", "9 replicas"},
		{"deploy replicas", "services: {a: {image: x, deploy: {replicas: 9}}}", "9 replicas"},
		{"total containers", "services: {a: {image: x, scale: 4}, b: {image: x, scale: 4}, c: {image: x, scale: 4}, d: {image: x, scale: 4}, e: {image: x, scale: 1}}", "17 containers"},
		{"reserved prefix uppercase host", "services: {a: {image: GHCR.IO/acme/other-production-web:v1}}", "Yoho-built image"},
		{"reserved prefix docker.io", "services: {a: {image: docker.io/yoho/other-production-web:v1}}", "Yoho-built image"},
		{"reservation devices", "services: {a: {image: x, deploy: {resources: {reservations: {devices: [{capabilities: [gpu]}]}}}}}", "deploy.resources devices"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := load(t, c.yml)
			err := pol.Check(r.Project, r.Ext)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestPolicyBuiltImageAndRoutes(t *testing.T) {
	r := load(t, "services: {web: {image: yoho/other-production-web:abc, x-yoho: {proxy: {}}}}")
	pol := Policy{Project: project, Built: map[string]string{"web": "yoho/shop-production-web:abc", "gone": "yoho/shop-production-gone:abc"}}
	err := pol.Check(r.Project, r.Ext)
	for _, want := range []string{`image "yoho/other-production-web:abc" must be yoho/shop-production-web:abc`, "built Service gone is not in the project", "catch-all Proxy route"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want %q in %v", want, err)
		}
	}
}

func TestPolicyBuiltImageVolumeAndEmpty(t *testing.T) {
	r := load(t, "services: {web: {image: yoho/shop-production-web:abc, volumes: [{type: image, source: \"yoho/shop-production-web:abc\", target: /assets}]}}")
	pol := Policy{Project: project, Built: map[string]string{"web": "yoho/shop-production-web:abc"}, Reserved: []string{"yoho/"}}
	if err := pol.Check(r.Project, r.Ext); err != nil {
		t.Fatal(err)
	}
	empty := load(t, "services: {}")
	if err := (Policy{Project: project, Limits: Limits{AllowEmpty: true}}).Check(empty.Project, empty.Ext); err != nil {
		t.Fatal(err)
	}
}

// Fractional CPU limits compare in thousandths (compose keeps float32).
func TestPolicyFractionalCPUs(t *testing.T) {
	pol := Policy{Project: project, Limits: Limits{MaxCPUs: 0.1}}
	for _, yml := range []string{
		"services: {a: {image: x, cpus: 0.1}}",
		"services: {a: {image: x, deploy: {resources: {limits: {cpus: '0.1'}}}}}",
	} {
		r := load(t, yml)
		if err := pol.Check(r.Project, r.Ext); err != nil {
			t.Errorf("%s: %v", yml, err)
		}
		pol.Harden(r.Project)
		if err := pol.CheckCompiled(r.Project); err != nil {
			t.Errorf("%s hardened: %v", yml, err)
		}
	}
	r := load(t, "services: {a: {image: x, cpus: 0.2}}")
	if err := pol.Check(r.Project, r.Ext); err == nil {
		t.Error("cpus 0.2 above 0.1 passed")
	}
}

func TestHarden(t *testing.T) {
	r := load(t, `
services:
  a: {image: x, tmpfs: [/run, "/t:mode=1777"], volumes: [{type: tmpfs, target: /tmp}], pre_start: [{image: y}]}
  b: {image: x, cap_drop: [ALL], deploy: {resources: {limits: {memory: 64m, pids: 50}}}}
`)
	Policy{}.Harden(r.Project)
	a, b := r.Project.Services["a"], r.Project.Services["b"]
	if a.PullPolicy != "never" || a.MemLimit != DefaultMaxMemoryBytes || a.CPUS != DefaultMaxCPUs || a.PreStart[0].PullPolicy != "never" {
		t.Errorf("a: pull %q mem %d cpus %g", a.PullPolicy, a.MemLimit, a.CPUS)
	}
	if !slices.Contains(a.CapDrop, "NET_RAW") || a.PidsLimit != DefaultPidsLimit {
		t.Errorf("a: cap_drop %v pids %d", a.CapDrop, a.PidsLimit)
	}
	if pre := a.PreStart[0]; pre.MemLimit != DefaultMaxMemoryBytes || pre.CPUS != DefaultMaxCPUs {
		t.Errorf("pre_start: mem %d cpus %g", pre.MemLimit, pre.CPUS)
	}
	if !slices.Contains(a.PreStart[0].CapDrop, "NET_RAW") {
		t.Errorf("pre_start cap_drop %v", a.PreStart[0].CapDrop)
	}
	if got := strings.Join(a.Tmpfs, " "); got != "/run:size=1073741824 /t:mode=1777,size=1073741824" {
		t.Errorf("tmpfs %q", got)
	}
	if a.Volumes[0].Tmpfs == nil || a.Volumes[0].Tmpfs.Size != DefaultMaxTmpfsBytes {
		t.Errorf("tmpfs volume %+v", a.Volumes[0])
	}
	if lim := b.Deploy.Resources.Limits; b.MemLimit != 64<<20 || lim.MemoryBytes != 64<<20 || b.CPUS != DefaultMaxCPUs || lim.NanoCPUs.Value() != DefaultMaxCPUs || lim.Pids != 50 {
		t.Errorf("b: mem %d/%d cpus %g/%g pids %d", b.MemLimit, lim.MemoryBytes, b.CPUS, lim.NanoCPUs.Value(), lim.Pids)
	}
	// What Harden writes must load again (compose refuses differing
	// top-level and deploy limits).
	out, err := r.Project.MarshalYAML()
	if err != nil {
		t.Fatal(err)
	}
	load(t, string(out))
	if slices.Contains(b.CapDrop, "NET_RAW") || b.PidsLimit != 50 {
		t.Errorf("b: cap_drop %v pids %d", b.CapDrop, b.PidsLimit)
	}
}

func TestCheckRaw(t *testing.T) {
	for _, c := range []struct{ yml, want string }{
		{"include: [other.yaml]\nservices: {a: {image: x}}", "include is not allowed"},
		{"services: {a: {image: x, extends: {file: /etc/x.yml, service: b}}}", "service a: extends"},
		{"services: {a: {image: x, env_file: [/root/.env]}}", "service a: env_file"},
		{"services: {a: {image: x, label_file: /etc/l}}", "service a: label_file"},
		{"services: {a: {image: x, build: .}}", "service a: build"},
	} {
		if err := CheckRaw([]byte(c.yml)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", c.yml, c.want, err)
		}
	}
	if err := CheckRaw([]byte("services: {a: {image: x}}")); err != nil {
		t.Fatal(err)
	}
}

func TestRoutes(t *testing.T) {
	r := load(t, "services: {a: {image: x, x-yoho: {proxy: {hosts: [B.example.com, a.example.com]}}}, b: {image: x, x-yoho: {proxy: {}}}, c: {image: x}}")
	got := strings.Join(Routes(r.Ext), ",")
	if got != ",a.example.com,b.example.com" {
		t.Fatalf("routes %q", got)
	}
	cd := load(t, "services: {web: {image: x, x-yoho: {proxy: {tls: true, custom_domains: {allow: /allow}}}}}")
	if got := Routes(cd.Ext); len(got) != 1 || got[0] != "custom_domains:/allow" {
		t.Fatalf("custom_domains routes %#v", got)
	}
}

func TestPolicyCustomDomainsCatchAll(t *testing.T) {
	yml := "services: {a: {image: x, x-yoho: {proxy: {tls: true, custom_domains: {allow: /allow}}}}}"
	r := load(t, yml)
	// A plain catch-all grant, or another allow endpoint, is not enough:
	// the Member could point the Proxy's check anywhere.
	for _, routes := range [][]string{{"shop.example.com"}, {"shop.example.com", ""}, {CustomDomainsRoute("http://169.254.169.254/")}} {
		deny := Policy{Project: project, Routes: routes}
		if err := deny.Check(r.Project, r.Ext); err == nil || !strings.Contains(err.Error(), "custom_domains with allow /allow is not allowed") {
			t.Fatalf("routes %v: %v", routes, err)
		}
	}
	allow := Policy{Project: project, Routes: []string{"shop.example.com", CustomDomainsRoute("/allow")}}
	if err := allow.Check(r.Project, r.Ext); err != nil {
		t.Fatal(err)
	}
}
