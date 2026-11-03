package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func stepClock(t *testing.T) {
	clock := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	old := now
	now = func() time.Time { clock = clock.Add(time.Minute); return clock }
	t.Cleanup(func() { now = old })
}

// Rotated provider and generated secrets stay rotated across a rollback;
// --with-release-secrets brings the Release's own values back.
func TestRollbackUsesCurrentSecrets(t *testing.T) {
	root := withRoot(t)
	stepClock(t)
	ctx := context.Background()
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	appDir := filepath.Join(root, "apps", "shop", "production")

	d := testDeploy(t, h, nil)
	d.RetainReleases = 5
	d.ServiceSecrets = map[string]map[string]string{"web": {"SECRET_KEY_BASE": "skb-old"}, "cloudflared": {"TUNNEL_TOKEN": "tok-old"}}
	rel1, err := Compose{}.Deploy(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	pgOld := readFile(t, filepath.Join(appDir, "generated", "POSTGRES_PASSWORD"))
	// A rotation: new provider values, a new generated value.
	if err := os.WriteFile(filepath.Join(appDir, "generated", "POSTGRES_PASSWORD"), []byte("pg-new"), 0o600); err != nil {
		t.Fatal(err)
	}
	d.Version = "v2"
	d.ServiceSecrets = map[string]map[string]string{"web": {"SECRET_KEY_BASE": "skb-new"}, "cloudflared": {"TUNNEL_TOKEN": "tok-new"}}
	rel2, err := Compose{}.Deploy(ctx, d)
	if err != nil {
		t.Fatal(err)
	}

	rb := testDeploy(t, h, nil)
	rb.RetainReleases = 5
	got, err := Compose{}.Rollback(ctx, rb, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if got.RollbackSecrets != RollbackSecretsCurrent || got.SecretsFrom != rel2.SecretsGeneration || got.BaseSecretsGeneration != rel1.SecretsGeneration ||
		got.SecretsGeneration == rel1.SecretsGeneration || got.SecretsGeneration == rel2.SecretsGeneration || got.BaseComposeSHA256 != rel1.ComposeSHA256 {
		t.Fatalf("rollback record %+v", got)
	}
	gen := filepath.Join(appDir, "secrets", got.SecretsGeneration)
	if v := readFile(t, filepath.Join(gen, "web", "SECRET_KEY_BASE")); v != "skb-new" {
		t.Errorf("web SECRET_KEY_BASE = %q, want the current value", v)
	}
	if v := readFile(t, filepath.Join(gen, "web", "DATABASE_PASSWORD")); v != "pg-new" {
		t.Errorf("web DATABASE_PASSWORD = %q, want the current generated value", v)
	}
	if v := readFile(t, filepath.Join(gen, "db", "POSTGRES_PASSWORD")); v != "pg-new" {
		t.Errorf("db POSTGRES_PASSWORD = %q", v)
	}
	if v := readFile(t, filepath.Join(gen, "cloudflared.env")); !strings.Contains(v, "tok-new") {
		t.Errorf("cloudflared.env = %q", v)
	}
	relDir := filepath.Join(appDir, "releases", "v1")
	compose := readFile(t, filepath.Join(relDir, "compose.yaml"))
	if !strings.Contains(compose, gen+"/web/SECRET_KEY_BASE") || !strings.Contains(compose, gen+"/cloudflared.env") || strings.Contains(compose, rel1.SecretsGeneration) {
		t.Errorf("compose.yaml not re-pointed:\n%s", compose)
	}
	if !strings.Contains(compose, "$$5") {
		t.Errorf("escaped $ lost:\n%s", compose)
	}
	if SHA256Hex([]byte(compose)) != got.ComposeSHA256 {
		t.Error("ComposeSHA256 does not match compose.yaml")
	}
	baseFile := readFile(t, filepath.Join(relDir, BaseComposeFile))
	if SHA256Hex([]byte(baseFile)) != rel1.ComposeSHA256 {
		t.Error("compose.release.yaml is not the Release as first deployed")
	}
	if got.Secrets["web/SECRET_KEY_BASE"].Fingerprint != rel2.Secrets["web/SECRET_KEY_BASE"].Fingerprint ||
		got.Secrets["web/SECRET_KEY_BASE"].Ref != "op://vault/shop/skb" || got.BaseSecrets["web/SECRET_KEY_BASE"].Fingerprint != rel1.Secrets["web/SECRET_KEY_BASE"].Fingerprint {
		t.Errorf("audit %+v base %+v", got.Secrets, got.BaseSecrets)
	}
	if strings.Contains(h.all(), "skb-new") || strings.Contains(h.all(), "pg-new") {
		t.Error("secret value in a docker command")
	}
	// The new label makes compose recreate the Services.
	if lbl := func(s string) string { i := strings.Index(s, "yoho.secrets: "); return s[i : i+30] }; lbl(compose) == lbl(baseFile) {
		t.Error("yoho.secrets label unchanged")
	}

	// Again: the current secrets are what v1 reads already.
	again, err := Compose{}.Rollback(ctx, rb, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if again.SecretsGeneration != got.SecretsGeneration || again.BaseComposeSHA256 != rel1.ComposeSHA256 {
		t.Errorf("repeated rollback %+v", again)
	}

	// --with-release-secrets: the Release's own generation and file.
	rb.RollbackReleaseSecrets = true
	own, err := Compose{}.Rollback(ctx, rb, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if own.RollbackSecrets != RollbackSecretsRelease || own.SecretsGeneration != rel1.SecretsGeneration || own.BaseComposeSHA256 != "" ||
		own.ComposeSHA256 != rel1.ComposeSHA256 || own.Secrets["web/SECRET_KEY_BASE"] != rel1.Secrets["web/SECRET_KEY_BASE"] {
		t.Fatalf("release-secrets record %+v", own)
	}
	if readFile(t, filepath.Join(relDir, "compose.yaml")) != baseFile {
		t.Error("compose.yaml is not the Release's own")
	}
	if v := readFile(t, filepath.Join(appDir, "secrets", own.SecretsGeneration, "web", "DATABASE_PASSWORD")); v != pgOld {
		t.Errorf("own generation DATABASE_PASSWORD = %q", v)
	}
}

func TestRollbackRefusesSecretsTheCurrentReleaseLacks(t *testing.T) {
	root := withRoot(t)
	stepClock(t)
	ctx := context.Background()
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	d := testDeploy(t, h, nil)
	web := d.Ext["web"]
	web.Secrets = append(append(config.SecretRefs{}, web.Secrets...), config.SecretRef{Name: "LEGACY_KEY", Key: "LEGACY_KEY"})
	d.Ext["web"] = web
	d.ServiceSecrets["web"]["LEGACY_KEY"] = "legacy"
	if _, err := (Compose{}).Deploy(ctx, d); err != nil {
		t.Fatal(err)
	}
	d2 := testDeploy(t, h, nil)
	d2.Version = "v2"
	if _, err := (Compose{}).Deploy(ctx, d2); err != nil {
		t.Fatal(err)
	}
	relDir := filepath.Join(root, "apps", "shop", "production", "releases", "v1")
	before := readFile(t, filepath.Join(relDir, "compose.yaml"))
	h.scripts = nil
	_, err := Compose{}.Rollback(ctx, testDeploy(t, h, nil), "v1")
	var ms *MissingSecretsError
	if !errors.As(err, &ms) || len(ms.Missing) != 1 || ms.Missing[0] != "web/LEGACY_KEY" || ms.Current != "v2" || !IsRefusal(err) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(h.all(), "'--scale'") || strings.Contains(h.all(), "kamal-proxy' 'deploy") {
		t.Error("a refused rollback started containers")
	}
	if readFile(t, filepath.Join(relDir, "compose.yaml")) != before {
		t.Error("a refused rollback changed compose.yaml")
	}
	if _, err := os.Stat(filepath.Join(relDir, BaseComposeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("a refused rollback wrote compose.release.yaml")
	}
	// The Release's own secrets still work.
	rb := testDeploy(t, h, nil)
	rb.RollbackReleaseSecrets = true
	if _, err := (Compose{}).Rollback(ctx, rb, "v1"); err != nil {
		t.Fatal(err)
	}
}

func TestPickCurrentAndFreshGeneration(t *testing.T) {
	target := map[string]release.SecretAudit{"api/TOKEN": {Ref: "op://v/i/token"}, "api/LOCAL": {}}
	cur := map[string]release.SecretAudit{"web/TOKEN": {Ref: "op://v/i/token"}, "worker/TOKEN": {Ref: "op://v/i/token"}, "web/LOCAL": {}}
	vals := map[string]map[string]string{"web": {"TOKEN": "t2", "LOCAL": "l"}, "worker": {"TOKEN": "t2"}}
	if v, from, ok := PickCurrent("api", "TOKEN", target, cur, vals); !ok || v != "t2" || !strings.HasSuffix(from, "/TOKEN") {
		t.Errorf("same ref: %q %q %v", v, from, ok)
	}
	if _, _, ok := PickCurrent("api", "LOCAL", target, cur, vals); ok {
		t.Error("a key without a provider reference matched another Service")
	}
	vals["worker"]["TOKEN"] = "t3"
	if _, _, ok := PickCurrent("api", "TOKEN", target, cur, vals); ok {
		t.Error("Services disagree: no match")
	}
	if g := FreshGeneration("a", "b"); g != "a" {
		t.Error(g)
	}
	if g := FreshGeneration("a", "a", "a-rollback"); g != "a-rollback-rollback" {
		t.Error(g)
	}
}
