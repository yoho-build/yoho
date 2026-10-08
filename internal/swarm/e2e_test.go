package swarm

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-dev/yoho/internal/config"
	"github.com/yoho-dev/yoho/internal/plan"
	"github.com/yoho-dev/yoho/internal/proxy"
	"github.com/yoho-dev/yoho/internal/remote"
)

const e2eCompose = `
services:
  web:
    image: yoho-swarme2e-web:${TAG}
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://127.0.0.1/"]
      interval: 1s
      timeout: 2s
      retries: 3
    stop_grace_period: 5s
    deploy:
      replicas: 2
  redis:
    image: redis:7-alpine
    command: ["redis-server", "--appendonly", "yes"]
    volumes:
      - data:/data
volumes:
  data:
`

func sh(t *testing.T, script string) string {
	t.Helper()
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", script, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestE2ELocalSwarm deploys a tiny stack twice to the LOCAL Docker (a
// single-node Swarm, initialized and left again if it was not one) and
// checks the start-first rolling update keeps serving through the Proxy.
func TestE2ELocalSwarm(t *testing.T) {
	if os.Getenv("YOHO_E2E") != "1" {
		t.Skip("YOHO_E2E=1 not set")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("no local docker")
	}
	ctx := context.Background()
	if st := sh(t, "docker info -f '{{.Swarm.LocalNodeState}}'"); st == "inactive" {
		sh(t, "docker swarm init >/dev/null 2>&1 || docker swarm init --advertise-addr 127.0.0.1 >/dev/null")
		t.Cleanup(func() {
			exec.Command("docker", "swarm", "leave", "--force").Run()
			exec.Command("docker", "network", "rm", "docker_gwbridge").Run()
		})
	}
	hadProxy := exec.Command("docker", "container", "inspect", proxy.ContainerName).Run() == nil
	hadNet := exec.Command("docker", "network", "inspect", proxy.Network).Run() == nil
	stack := "yoho-swarme2e-test"
	t.Cleanup(func() {
		exec.Command("docker", "stack", "rm", stack).Run()
		exec.Command("docker", "stack", "rm", ReleaseStack(stack)).Run()
		// Task containers go away asynchronously; volumes are free after.
		for i := 0; i < 60; i++ {
			out, _ := exec.Command("docker", "ps", "-aq", "--filter", "label=com.docker.stack.namespace="+stack).Output()
			if strings.TrimSpace(string(out)) == "" {
				break
			}
			time.Sleep(time.Second)
		}
		if !hadProxy {
			exec.Command("docker", "rm", "-f", proxy.ContainerName).Run()
			exec.Command("docker", "volume", "rm", proxy.Volume).Run()
		}
		if !hadNet {
			exec.Command("docker", "network", "rm", proxy.Network).Run()
		}
		exec.Command("sh", "-c", "docker secret rm $(docker secret ls -q --filter label=yoho.app=swarme2e) >/dev/null 2>&1; docker volume rm yoho-swarme2e-test_data >/dev/null 2>&1; docker image rm yoho-swarme2e-web:v1 yoho-swarme2e-web:v2 >/dev/null 2>&1").Run()
	})
	sh(t, "docker tag nginx:alpine yoho-swarme2e-web:v1 && docker tag nginx:alpine yoho-swarme2e-web:v2")
	withRoot(t)

	port := 18089
	zero := 0
	h := &remote.Local{HostName: "local"}
	mk := func(tag string) *plan.Deploy {
		return &plan.Deploy{
			App: "swarme2e", Destination: "test", Version: tag, Performer: "e2e",
			Servers: []plan.NamedHost{{Name: "local", Server: config.Server{SSH: "127.0.0.1"}, Host: h}},
			Project: loadE2E(t, tag),
			Ext: map[string]config.ServiceExt{
				"web": {
					Proxy:          &config.ServiceProxy{Port: 80, HealthPath: "/"},
					Secrets:        config.SecretRefs{{Name: "API_KEY", Key: "API_KEY"}},
					ReleaseCommand: []string{"sh", "-c", "nslookup redis >/dev/null && echo migrated"},
				},
				"redis": {Stateful: true, Generate: map[string]string{"REDIS_PASSWORD": "hex32"}},
			},
			ServiceSecrets: map[string]map[string]string{"web": {"API_KEY": "e2e-" + tag}},
			Proxy:          config.ProxyConfig{HTTPPort: &port, HTTPSPort: &zero},
			RetainReleases: 2,
			Out:            testWriter{t},
		}
	}

	rel, err := Runtime{}.Deploy(ctx, mk("v1"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("deployed %s", rel.Version)
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if err := get200(url); err != nil {
		t.Fatalf("v1 not serving: %v", err)
	}
	cid := sh(t, "docker ps -q --filter label=com.docker.swarm.service.name="+stack+"_web | head -n 1")
	if got := sh(t, "docker exec "+cid+" cat /run/secrets/API_KEY"); got != "e2e-v1" {
		t.Errorf("secret = %q", got)
	}
	if got := sh(t, "docker inspect -f '{{json .Config.Env}}' "+cid); strings.Contains(got, "e2e-v1") || !strings.Contains(got, "API_KEY_FILE=/run/secrets/API_KEY") {
		t.Errorf("env = %s", got)
	}
	redisID := sh(t, "docker service inspect -f '{{.ID}}' "+stack+"_redis")

	// Second deploy: new image tag and rotated secret. Hammer the Proxy
	// meanwhile; start-first must not drop a request.
	var ok, bad atomic.Int64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if get200(url) == nil {
				ok.Add(1)
			} else {
				bad.Add(1)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	rel, err = Runtime{}.Deploy(ctx, mk("v2"))
	close(stop)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("v2: %d ok, %d failed requests during the update", ok.Load(), bad.Load())
	if bad.Load() > 0 {
		t.Errorf("%d requests failed during the start-first update", bad.Load())
	}
	if got := sh(t, "docker service inspect -f '{{.Spec.TaskTemplate.ContainerSpec.Image}}|{{.Spec.UpdateConfig.Order}}' "+stack+"_web"); got != "yoho-swarme2e-web:v2|start-first" {
		t.Errorf("web spec = %s", got)
	}
	if got := sh(t, "docker service inspect -f '{{.Spec.UpdateConfig.Order}}|{{json .Spec.TaskTemplate.Placement.Constraints}}' "+stack+"_redis"); !strings.HasPrefix(got, "stop-first|") || !strings.Contains(got, "node.hostname ==") {
		t.Errorf("redis spec = %s", got)
	}
	if id := sh(t, "docker service inspect -f '{{.ID}}' "+stack+"_redis"); id != redisID {
		t.Error("redis service was recreated")
	}
	cid = sh(t, "docker ps -q --filter label=com.docker.swarm.service.name="+stack+"_web | head -n 1")
	if got := sh(t, "docker exec "+cid+" cat /run/secrets/API_KEY"); got != "e2e-v2" {
		t.Errorf("rotated secret = %q", got)
	}

	// Rollback to v1 reuses v1's (retained) secret.
	if _, err := (Runtime{}).Rollback(ctx, mk("v1"), "v1"); err != nil {
		t.Fatal(err)
	}
	if got := sh(t, "docker service inspect -f '{{.Spec.TaskTemplate.ContainerSpec.Image}}' "+stack+"_web"); got != "yoho-swarme2e-web:v1" {
		t.Errorf("after rollback web image = %s", got)
	}
}

func loadE2E(t *testing.T, tag string) *types.Project {
	t.Helper()
	return loadProjectNamed(t, strings.ReplaceAll(e2eCompose, "${TAG}", tag), "yoho-swarme2e-test")
}

func get200(url string) error {
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
