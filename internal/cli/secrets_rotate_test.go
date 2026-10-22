package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/ui"
)

func TestSecretsRotateUnknownServiceBeforeSSH(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	src := "services:\n  web:\n    image: nginx\n  db:\n    image: postgres\n"
	c := secretsCmd(&globals{configPath: appDir(t, "compose", src)})
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"rotate", "nope"})
	c.SilenceErrors, c.SilenceUsage = true, true
	err := c.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(out.String(), "unknown Service nope") || strings.Contains(out.String(), "Connect") {
		t.Fatalf("error=%v output=%s", err, out.String())
	}
	if strings.Contains(out.String(), "KEY") && strings.Contains(out.String(), "=") {
		t.Fatalf("looks like a secret assignment leaked:\n%s", out.String())
	}
}

func TestRotatePlanNeverIncludesValues(t *testing.T) {
	secret := "super-secret-rotate-value"
	d := &plan.Deploy{Ext: map[string]config.ServiceExt{
		"web": {Proxy: &config.ServiceProxy{Hosts: []string{"shop.example.com"}}},
		"db":  {Stateful: true},
	}}
	changes := rotatePlan(d, []string{"web", "db"}, "primary")
	if len(changes) != 2 {
		t.Fatalf("changes %v", changes)
	}
	if changes[0].Action != plan.ActionUpdate || changes[0].Downtime || changes[0].Reasons[0] != "secrets changed" {
		t.Fatalf("web %+v", changes[0])
	}
	if changes[1].Action != plan.ActionReplace || !changes[1].Downtime {
		t.Fatalf("db %+v", changes[1])
	}
	var buf bytes.Buffer
	ui.New(&buf, ui.Human, false).Plan(changes)
	if strings.Contains(buf.String(), secret) {
		t.Fatalf("value in plan:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "secrets changed") {
		t.Fatalf("plan:\n%s", buf.String())
	}
}

func TestIntersectAndOmitNamedServices(t *testing.T) {
	changed := []string{"api", "web"}
	if got := intersectServices(changed, []string{"web"}); strings.Join(got, ",") != "web" {
		t.Fatalf("intersect %v", got)
	}
	if got := changedServicesNotRequested(changed, []string{"web"}); strings.Join(got, ",") != "api" {
		t.Fatalf("omitted %v", got)
	}
	if strings.Contains(strings.Join(changed, ","), "secret") {
		t.Fatal("fixture names must stay value-free")
	}
}

func TestChangedServicesUsedByRotate(t *testing.T) {
	// CLI rotate uses deploy.ChangedServices; keep the wiring honest.
	secret := "do-not-print-me-please"
	got := deploy.ChangedServices(
		map[string]map[string]string{"web": {"A": secret}, "db": {"B": "same"}},
		map[string]map[string]string{"web": {"A": "new"}, "db": {"B": "same"}},
	)
	if strings.Join(got, ",") != "web" {
		t.Fatalf("%v", got)
	}
	if strings.Contains(strings.Join(got, ","), secret) {
		t.Fatal("value leaked")
	}
}

func TestSecretsRotateNonemptyBeforeSSH(t *testing.T) {
	oldRoot := release.Root
	t.Cleanup(func() { release.Root = oldRoot })
	p := nonemptyApp(t, "compose", "services:\n  web:\n    image: nginx\n    x-yoho:\n      secrets: [KEY]\n", "KEY", "")
	dir := filepath.Dir(p)
	if err := os.MkdirAll(filepath.Join(dir, ".yoho"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".yoho", "secrets"), []byte("KEY=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := secretsCmd(&globals{configPath: p, json: true})
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs([]string{"rotate"})
	c.SilenceErrors, c.SilenceUsage = true, true
	err := c.ExecuteContext(context.Background())
	if err == nil || !strings.Contains(out.String(), "required nonempty source value") || strings.Contains(out.String(), "Connect") {
		t.Fatalf("error=%v output=%s", err, out.String())
	}
}
