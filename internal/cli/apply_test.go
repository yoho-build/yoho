package cli

import (
	"bytes"
	"context"
	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/secrets"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/ui"
)

func TestConfirmApply(t *testing.T) {
	a := &app{g: &globals{}, ui: ui.Discard()}
	if err := a.confirmApply(strings.NewReader("yes\n"), false, false); err == nil || !strings.Contains(err.Error(), "--auto-approve") {
		t.Errorf("no TTY must refuse: %v", err)
	}
	if err := a.confirmApply(strings.NewReader(""), false, true); err != nil {
		t.Errorf("auto-approve: %v", err)
	}
	if err := a.confirmApply(strings.NewReader("yes\n"), true, false); err != nil {
		t.Errorf("yes: %v", err)
	}
	for _, in := range []string{"y\n", "no\n", "Yes\n", ""} {
		if err := a.confirmApply(strings.NewReader(in), true, false); err == nil {
			t.Errorf("%q must not be accepted", in)
		}
	}
	j := &app{g: &globals{json: true}, ui: ui.Discard()}
	if err := j.confirmApply(strings.NewReader("yes\n"), true, false); err == nil || !strings.Contains(err.Error(), "--json") {
		t.Errorf("json without auto-approve: %v", err)
	}
}

func TestApplyPrecheckRefusesWithoutTTY(t *testing.T) {
	old := isTTY
	isTTY = func() bool { return false }
	defer func() { isTTY = old }()
	a := &app{g: &globals{}, ui: ui.Discard()}
	if err := a.confirmPrecheck(applyOptions{}); err == nil {
		t.Error("want refusal")
	}
	if err := a.confirmPrecheck(applyOptions{AutoApprove: true}); err != nil {
		t.Error(err)
	}
}

func TestPlanRejectsInvalidProxyHealthProbeBeforeSSH(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	for _, runtime := range []string{"compose", "swarm"} {
		for _, settings := range []string{"health_port: 0", "health_timeout: 0s", "health_timeout: ''"} {
			t.Run(runtime+settings, func(t *testing.T) {
				src := "services:\n  web:\n    image: nginx\n    healthcheck: {test: ['CMD', 'true']}\n    x-yoho: {proxy: {" + settings + "}}\n"
				c := planCmd(&globals{configPath: appDir(t, runtime, src), json: true})
				var out bytes.Buffer
				c.SetOut(&out)
				c.SetErr(&out)
				c.SetArgs([]string{"--version", "v1"})
				c.SilenceErrors = true
				c.SilenceUsage = true
				err := c.ExecuteContext(context.Background())
				if err == nil || !strings.Contains(out.String(), "x-yoho.proxy.health_") || strings.Contains(out.String(), "Connect") {
					t.Fatalf("error %v, output %s", err, out.String())
				}
			})
		}
	}
}

func TestNewDeployPreservesProxyHealthProbeForPlan(t *testing.T) {
	src := "services:\n  web:\n    image: nginx\n    x-yoho: {proxy: {port: 3000, health_port: 9090, health_timeout: 750ms, deploy_timeout: 47, drain_timeout: 19}}\n"
	path := appDir(t, "compose", src)
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := composefile.Load(context.Background(), filepath.Dir(path), nil, nil, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{g: &globals{}, cfg: cfg, destName: "production", dest: cfg.Destinations["production"], ui: ui.Discard()}
	s := &session{r: r, ver: "v1", store: secrets.New(nil)}
	for _, forPlan := range []bool{true, false} {
		d, err := a.newDeploy(s, nil, forPlan)
		if err != nil {
			t.Fatal(err)
		}
		p := d.Ext["web"].Proxy
		if p.Port != 3000 || p.HealthPort == nil || *p.HealthPort != 9090 || p.HealthTimeout == nil || *p.HealthTimeout != "750ms" || p.DeployTimeout != 47 || p.DrainTimeout != 19 {
			t.Fatalf("mapped Proxy %+v", p)
		}
	}
}
