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
		Network: &deploy.NetworkHint{Network: "shop_default", SuggestedSubnet: "10.200.3.0/24", Snippet: deploy.SnippetFor("10.200.3.0/24")}}
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
