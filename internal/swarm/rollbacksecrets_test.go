package swarm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
)

func readText(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestSwarmRollbackUsesCurrentSecrets(t *testing.T) {
	root := withRoot(t)
	fastPolling(t)
	ctx := context.Background()
	h := newFake(newSim())
	appDir := filepath.Join(root, "apps", "shop", "production")
	stack := "yoho-shop-production"

	d := testDeploy(t, h, nil)
	d.RetainReleases = 5
	d.ServiceSecrets = map[string]map[string]string{"web": {"SECRET_KEY_BASE": "skb-old"}, "cloudflared": {"TUNNEL_TOKEN": "tok-old"}}
	rel1, err := Runtime{}.Deploy(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "generated", "POSTGRES_PASSWORD"), []byte("pg-new"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.Version = "v2"
	d.ServiceSecrets = map[string]map[string]string{"web": {"SECRET_KEY_BASE": "skb-new"}, "cloudflared": {"TUNNEL_TOKEN": "tok-new"}}
	if _, err := (Runtime{}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	key, err := deploy.EnsureHMACKey(ctx, h, appDir)
	if err != nil {
		t.Fatal(err)
	}

	rb := testDeploy(t, h, nil)
	rb.RetainReleases = 5
	got, err := Runtime{}.Rollback(ctx, rb, "v1")
	if err != nil {
		t.Fatalf("%v\n%s", err, h.all())
	}
	if got.RollbackSecrets != deploy.RollbackSecretsCurrent || got.BaseSecretsGeneration != rel1.SecretsGeneration || got.BaseComposeSHA256 != rel1.ComposeSHA256 {
		t.Fatalf("record %+v", got)
	}
	relDir := filepath.Join(appDir, "releases", "v1")
	stackYAML := readText(t, filepath.Join(relDir, "compose.yaml"))
	skbNew, skbOld := SecretName(stack, "SECRET_KEY_BASE", "skb-new", key), SecretName(stack, "SECRET_KEY_BASE", "skb-old", key)
	pgNew := SecretName(stack, "POSTGRES_PASSWORD", "pg-new", key)
	if !strings.Contains(stackYAML, skbNew) || strings.Contains(stackYAML, skbOld) || !strings.Contains(stackYAML, pgNew) {
		t.Errorf("stack file not re-pointed at the current secrets:\n%s", stackYAML)
	}
	if strings.Contains(stackYAML, rel1.SecretsGeneration+"/") || !strings.Contains(stackYAML, got.SecretsGeneration+"/cloudflared.env") {
		t.Errorf("env_file not re-pointed:\n%s", stackYAML)
	}
	if env := readText(t, filepath.Join(appDir, "secrets", got.SecretsGeneration, "cloudflared.env")); !strings.Contains(env, "tok-new") {
		t.Errorf("cloudflared.env = %q", env)
	}
	if indexOf(h, "docker stack deploy", "releases/v1/compose.yaml") < 0 || indexOf(h, "docker secret inspect", skbNew) < 0 {
		t.Errorf("scripts:\n%s", h.all())
	}
	p, err := readPlan(ctx, h, relDir)
	if err != nil {
		t.Fatal(err)
	}
	// plan.json keeps the Release's own secrets too, so prune keeps them
	// for --with-release-secrets.
	if !slices.Contains(p.Secrets, skbNew) || !slices.Contains(p.Secrets, skbOld) {
		t.Errorf("plan secrets %v", p.Secrets)
	}
	if SHA256Hex := deploy.SHA256Hex([]byte(readText(t, filepath.Join(relDir, deploy.BaseComposeFile)))); SHA256Hex != rel1.ComposeSHA256 {
		t.Error("compose.release.yaml is not the Release as first deployed")
	}

	rb.RollbackReleaseSecrets = true
	own, err := Runtime{}.Rollback(ctx, rb, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if own.RollbackSecrets != deploy.RollbackSecretsRelease || own.SecretsGeneration != rel1.SecretsGeneration || own.ComposeSHA256 != rel1.ComposeSHA256 || own.BaseComposeSHA256 != "" {
		t.Fatalf("release-secrets record %+v", own)
	}
	if s := readText(t, filepath.Join(relDir, "compose.yaml")); !strings.Contains(s, skbOld) || strings.Contains(s, skbNew) {
		t.Errorf("own stack file:\n%s", s)
	}
}

func TestSwarmRollbackRefusesMissingSecret(t *testing.T) {
	withRoot(t)
	fastPolling(t)
	ctx := context.Background()
	h := newFake(newSim())
	d := testDeploy(t, h, nil)
	web := d.Ext["web"]
	web.Secrets = append(append(config.SecretRefs{}, web.Secrets...), config.SecretRef{Name: "LEGACY_KEY", Key: "LEGACY_KEY"})
	d.Ext["web"] = web
	d.ServiceSecrets["web"]["LEGACY_KEY"] = "legacy"
	if _, err := (Runtime{}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	d2 := testDeploy(t, h, nil)
	d2.Version = "v2"
	if _, err := (Runtime{}).Deploy(ctx, d2); err != nil {
		t.Fatal(err)
	}
	h.scripts = nil
	_, err := Runtime{}.Rollback(ctx, testDeploy(t, h, nil), "v1")
	var ms *deploy.MissingSecretsError
	if !errors.As(err, &ms) || !slices.Equal(ms.Missing, []string{"web/LEGACY_KEY"}) {
		t.Fatalf("err = %v", err)
	}
	if indexOf(h, "docker stack deploy") >= 0 {
		t.Error("a refused rollback deployed the stack")
	}
	rels, _ := Runtime{}.Releases(ctx, d2)
	if rels[0].Version != "v2" || rels[0].Status != "deployed" {
		t.Errorf("releases %+v", rels)
	}
}
