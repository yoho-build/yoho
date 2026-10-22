package deploy

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

func TestEarlySwitchOnlyOnFirstDeploy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		old         []string
		routeExists bool
		want        bool
	}{
		{name: "first deploy", want: true},
		{name: "route exists", routeExists: true},
		{name: "old containers without route", old: []string{"old"}},
		{name: "update", old: []string{"old"}, routeExists: true},
	} {
		if got := earlySwitch(&stagedProxy{old: tc.old}, tc.routeExists); got != tc.want {
			t.Errorf("%s: earlySwitch=%t, want %t", tc.name, got, tc.want)
		}
	}
}

// routeFixture deploys proxied Services a and b (independent, so one level,
// prepared a then b). routes is the Proxy's route list, old gives running
// containers per Service, and failB makes b's readiness fail.
func routeFixture(t *testing.T, routes string, listErr error, old map[string]string, failB bool) (*dockerHost, *bytes.Buffer, error) {
	t.Helper()
	h := &dockerHost{local: &remote.Local{}}
	h.respond = func(s string) (string, error) {
		switch {
		case strings.Contains(s, "kamal-proxy list --json"):
			return routes, listErr
		case strings.Contains(s, "kamal-proxy' 'deploy'"):
			return "", nil
		}
		for _, name := range []string{"a", "b"} {
			switch {
			case strings.Contains(s, "status=$(docker inspect") && strings.Contains(s, "'ps' '-q' '"+name+"'"):
				return old[name], nil
			case strings.Contains(s, "'--scale' '"+name+"="):
				return strings.TrimSpace(old[name] + "\nnew-" + name), nil
			case strings.Contains(s, "set -- 'new-"+name+"'"):
				if name == "b" && failB {
					return "", errors.New("b unhealthy")
				}
				return "", nil
			case strings.Contains(s, "{{.Name}}") && strings.Contains(s, "'new-"+name+"'"):
				return "/new-" + name, nil
			}
		}
		return "", nil
	}
	var out bytes.Buffer
	r := runner{h: h, out: &out, server: "s1", app: "shop", dest: "prod", cc: composeCLI{project: "p", dir: "/app", file: "/c.yaml"}}
	plans := []servicePlan{
		{Name: "a", Replicas: 1, Proxy: proxyWithDefaults(&config.ServiceProxy{Hosts: []string{"a.example.com"}})},
		{Name: "b", Replicas: 1, Proxy: proxyWithDefaults(&config.ServiceProxy{Hosts: []string{"b.example.com"}})},
	}
	return h, &out, r.cutover(context.Background(), plans)
}

func scriptIndex(h *dockerHost, sub string) int {
	return slices.IndexFunc(h.scripts, func(s string) bool { return strings.Contains(s, sub) })
}

func TestFirstDeploySwitchesRouteWhenServiceIsReady(t *testing.T) {
	h, out, err := routeFixture(t, "null", nil, nil, false)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	switchA, startB := scriptIndex(h, "kamal-proxy' 'deploy' 'shop-prod-a'"), scriptIndex(h, "'--scale' 'b=1'")
	if switchA < 0 || startB < 0 || switchA > startB {
		t.Fatalf("a's route did not go live before b started (switch %d, start %d):\n%s", switchA, startB, h.all())
	}
	if n := strings.Count(h.all(), "kamal-proxy' 'deploy' 'shop-prod-a'"); n != 1 {
		t.Fatalf("a switched %d times", n)
	}
	if !strings.Contains(out.String(), "[s1] route shop-prod-a is live for a.example.com, ") {
		t.Fatalf("no live route line:\n%s", out)
	}
}

func TestFirstDeployFailureKeepsLiveRoutes(t *testing.T) {
	h, out, err := routeFixture(t, "null", nil, nil, true)
	var failure *Failure
	if !errors.As(err, &failure) {
		t.Fatalf("no failure facts: %v", err)
	}
	if !strings.Contains(failure.Message, "Proxy routes already live: shop-prod-a") || !strings.Contains(out.String(), "Proxy routes already live on the new containers: shop-prod-a") {
		t.Fatalf("live route not reported: %v\n%s", err, out)
	}
	facts := map[string]string{}
	for _, f := range failure.Services {
		facts[f.Service] = f.ProxyRoute
	}
	if facts["a"] != "switched" || facts["b"] != "not-attempted" {
		t.Fatalf("facts %+v", failure.Services)
	}
	all := h.all()
	if strings.Contains(all, "docker rm -f 'new-a'") {
		t.Fatal("live route's containers removed")
	}
	if !strings.Contains(all, "docker rm -f 'new-b'") {
		t.Fatal("unswitched candidate kept")
	}
	if strings.Contains(all, "kamal-proxy' 'deploy' 'shop-prod-b'") {
		t.Fatal("failed Service switched")
	}
}

// Updates and unknown route lists keep switching after every Service is ready.
func TestRouteSwitchWaitsForAllServicesOnUpdate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		routes  string
		listErr error
		old     map[string]string
	}{
		{name: "existing routes", routes: `{"shop-prod-a":{"hosts":["a.example.com"],"targets":["old-a:80"]},"shop-prod-b":{"hosts":["b.example.com"],"targets":["old-b:80"]}}`, old: map[string]string{"a": "old-a", "b": "old-b"}},
		{name: "old containers without route", routes: "null", old: map[string]string{"a": "old-a", "b": "old-b"}},
		{name: "route without old containers", routes: `{"shop-prod-a":{"hosts":["a.example.com"],"targets":["gone:80"]},"shop-prod-b":{"hosts":["b.example.com"],"targets":["gone:80"]}}`},
		{name: "route list fails", listErr: errors.New("proxy down")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, out, err := routeFixture(t, tc.routes, tc.listErr, tc.old, false)
			if err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			lastReady, firstSwitch := scriptIndex(h, "set -- 'new-b'"), scriptIndex(h, "kamal-proxy' 'deploy'")
			if lastReady < 0 || firstSwitch < lastReady {
				t.Fatalf("route switched before every Service was ready:\n%s", h.all())
			}
			if strings.Contains(out.String(), " is live for ") {
				t.Fatalf("early switch reported:\n%s", out)
			}
			_, _, err = routeFixture(t, tc.routes, tc.listErr, tc.old, true)
			if err == nil || strings.Contains(err.Error(), "already live") {
				t.Fatalf("update failure: %v", err)
			}
		})
	}
}
