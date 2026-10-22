package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/ui"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

func TestRouteConflictHintHumanAndNDJSON(t *testing.T) {
	conflict := &proxy.RouteConflictError{Desired: "shop-production-newweb", Existing: "shop-production-web", Host: "shop.example.com", Path: "/"}
	for _, err := range []error{fmt.Errorf("plan: %w", conflict), &deploy.Failure{Cause: conflict, Message: conflict.Error(), Phase: "preflight-routes"}} {
		for _, mode := range []ui.Mode{ui.Human, ui.JSON} {
			var out bytes.Buffer
			ui.New(&out, mode, false).Step("primary", "Preflight Proxy routes").Fail(err, hintFor(err, "default hint"))
			for _, want := range []string{"shop-production-newweb", "shop-production-web", "different hostname", "verify ownership", "maintenance window", "interrupted", "never remove another App"} {
				if !strings.Contains(out.String(), want) {
					t.Fatalf("missing actionable failure detail %q: %s", want, out.String())
				}
			}
			if mode == ui.JSON {
				for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
					var event map[string]any
					if e := json.Unmarshal([]byte(line), &event); e != nil {
						t.Fatalf("invalid NDJSON %q: %v", line, e)
					}
				}
			}
		}
	}
}

func TestHintForOwnership(t *testing.T) {
	err := fmt.Errorf("wrapped: %w", &release.OwnershipError{Msg: "collision"})
	if got := hintFor(err, "check that Docker is running"); !strings.Contains(got, "rename the App or Destination") {
		t.Errorf("hint %q", got)
	}
	if got := hintFor(fmt.Errorf("boom"), "check that Docker is running"); got != "check that Docker is running" {
		t.Errorf("hint %q", got)
	}
}

func TestHintForGateError(t *testing.T) {
	err := fmt.Errorf("rollback: %w", &remote.GateError{Message: "no", Hint: "ask the operator to roll back"})
	if got := hintFor(err, "see `yoho releases`"); got != "ask the operator to roll back" {
		t.Fatalf("hint %q", got)
	}
}

func TestPoolExhaustedHintAndFacts(t *testing.T) {
	err := &deploy.Failure{Cause: fmt.Errorf("x"), Message: "Docker has no free address pool", Phase: "create-network",
		Network: &deploy.NetworkHint{Network: "shop_default", Default: true, SuggestedSubnet: "10.200.3.0/24", Key: "default"}}
	for _, mode := range []ui.Mode{ui.Human, ui.JSON} {
		var out bytes.Buffer
		ui.New(&out, mode, false).Step("primary", "Deploy").Fail(err, hintFor(err, "default hint"))
		if !strings.Contains(out.String(), "10.200.3.0/24") || strings.Contains(out.String(), "default hint") {
			t.Fatalf("hint lacks subnet: %s", out.String())
		}
	}
	var out bytes.Buffer
	ui.New(&out, ui.JSON, false).Step("primary", "Deploy").Fail(err, hintFor(err, "d"))
	var ev map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		_ = json.Unmarshal([]byte(line), &ev)
	}
	facts := ev["failure_facts"].(map[string]any)["network"].(map[string]any)
	if facts["suggested_subnet"] != "10.200.3.0/24" {
		t.Fatalf("facts %v", ev)
	}
}

func TestSecretsHintPerClass(t *testing.T) {
	def := "default secrets hint"
	got := map[string]string{}
	for name, class := range map[string]secrets.ErrClass{"timeout": secrets.ApprovalTimeout, "signin": secrets.NotSignedIn, "notfound": secrets.NotFound, "other": secrets.Other} {
		got[name] = secretsHint(fmt.Errorf("secret A: %w", &secrets.ProviderError{Class: class, Provider: "op"}), def)
	}
	if got["other"] != def || secretsHint(fmt.Errorf("plain"), def) != def {
		t.Fatalf("other must keep the default: %v", got)
	}
	seen := map[string]bool{def: true}
	for _, k := range []string{"timeout", "signin", "notfound"} {
		if seen[got[k]] {
			t.Fatalf("%s hint is not distinct: %q", k, got[k])
		}
		seen[got[k]] = true
	}
	if !strings.Contains(got["timeout"], "approval") || strings.Contains(got["timeout"], "inferred") {
		t.Fatalf("timeout hint %q", got["timeout"])
	}
	inferred := secretsHint(&secrets.ProviderError{Class: secrets.ApprovalTimeout, Provider: "op", Inferred: true}, def)
	if !strings.Contains(inferred, "inferred from the long wait") {
		t.Fatalf("inferred hint %q", inferred)
	}
}

func TestSecretsFailureJSONCodeAndWaitEvent(t *testing.T) {
	var out bytes.Buffer
	u := ui.New(&out, ui.JSON, false)
	step := u.Step("", "Resolve secrets")
	step.Waiting("waiting for secrets provider approval… (op)", map[string]any{"provider": "op", "key": "A", "elapsed_ms": int64(3100)})
	err := fmt.Errorf("secret A: %w", &secrets.ProviderError{Class: secrets.ApprovalTimeout, Provider: "op"})
	step.Fail(err, secretsHint(err, "d"))
	events := map[string]map[string]any{}
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var ev map[string]any
		if e := json.Unmarshal([]byte(line), &ev); e != nil {
			t.Fatal(e)
		}
		events[ev["event"].(string)] = ev
	}
	if strings.Contains(out.String(), "op://") {
		t.Fatalf("a provider ref leaked into the events: %s", out.String())
	}
	w := events["step_wait"]
	if w == nil || w["provider"] != "op" || w["step"] != "Resolve secrets" || w["elapsed_ms"] != float64(3100) {
		t.Fatalf("step_wait %v", w)
	}
	if f := events["step_fail"]; f == nil || f["code"] != "secrets_approval_timeout" || f["hint"] == "" {
		t.Fatalf("step_fail %v", f)
	}
}

func TestResolveSecretsRetriesDefaultDependsOnTTY(t *testing.T) {
	cases := []struct {
		flag int
		env  string
		tty  bool
		want int
	}{
		{-1, "", true, 1}, {-1, "", false, 0}, {-1, "2", false, 2}, {0, "2", true, 0}, {3, "", false, 3}, {-1, "junk", true, 1},
	}
	for _, c := range cases {
		if got := resolveSecretsRetries(c.flag, c.env, c.tty); got != c.want {
			t.Errorf("%+v: got %d", c, got)
		}
	}
}

func TestPoolHintForNonDefaultNetworks(t *testing.T) {
	mk := func(n *deploy.NetworkHint) error { return &deploy.Failure{Cause: fmt.Errorf("x"), Network: n} }
	shared := hintFor(mk(&deploy.NetworkHint{Network: "yoho", SuggestedSubnet: "10.200.3.0/24"}), "d")
	if !strings.Contains(shared, "docker network create --subnet 10.200.3.0/24 yoho") || strings.Contains(shared, "destinations.<name>.subnet") {
		t.Errorf("shared network hint: %q", shared)
	}
	named := hintFor(mk(&deploy.NetworkHint{Network: "shop_backend", Key: "backend", SuggestedSubnet: "10.200.3.0/24"}), "d")
	if !strings.Contains(named, "networks.backend.ipam") || !strings.Contains(named, "  backend:\n") {
		t.Errorf("named network hint: %q", named)
	}
	if h := hintFor(mk(&deploy.NetworkHint{SuggestedSubnet: "10.200.3.0/24"}), "d"); !strings.Contains(h, "10.200.3.0/24") {
		t.Errorf("unknown network hint: %q", h)
	}
	if h := hintFor(&deploy.SubnetChangeError{Project: "p", Old: "", New: "10.200.3.0/24"}, "d"); h == "d" {
		t.Error("subnet change needs its own hint")
	}
}
