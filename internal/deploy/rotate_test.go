package deploy

import (
	"context"
	"encoding/hex"
	"path"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

func TestChangedServicesDetectsValueAndMembership(t *testing.T) {
	secret := "super-secret-value-XYZ"
	prev := map[string]map[string]string{
		"web": {"SECRET_KEY_BASE": secret, "DATABASE_PASSWORD": "pg"},
		"db":  {"POSTGRES_PASSWORD": "pg"},
		"api": {"TOKEN": "same"},
	}
	next := map[string]map[string]string{
		"web": {"SECRET_KEY_BASE": "rotated-value", "DATABASE_PASSWORD": "pg"},
		"db":  {"POSTGRES_PASSWORD": "pg"},
		"api": {"TOKEN": "same"},
	}
	got := ChangedServices(prev, next)
	if strings.Join(got, ",") != "web" {
		t.Fatalf("changed = %v, want [web]", got)
	}
	if strings.Contains(strings.Join(got, ","), secret) || strings.Contains(strings.Join(got, ","), "rotated") {
		t.Fatal("secret value leaked into changed-Service names")
	}
	if got := ChangedServices(prev, prev); len(got) != 0 {
		t.Fatalf("unchanged set: %v", got)
	}
	next["db"] = map[string]string{"POSTGRES_PASSWORD": "pg", "EXTRA": "1"}
	got = ChangedServices(prev, next)
	if strings.Join(got, ",") != "db,web" {
		t.Fatalf("membership change: %v", got)
	}
	delete(next, "api")
	got = ChangedServices(prev, next)
	if !strings.Contains(strings.Join(got, ","), "api") {
		t.Fatalf("removed Service: %v", got)
	}
}

func TestChangedServicesFromAuditIgnoresValues(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	prev := map[string]release.SecretAudit{
		"web/SECRET_KEY_BASE":  {Ref: "op://vault/x", Fingerprint: secrets.Fingerprint(key, "old")},
		"db/POSTGRES_PASSWORD": {Ref: "generated", Fingerprint: secrets.Fingerprint(key, "pg")},
	}
	next := map[string]release.SecretAudit{
		"web/SECRET_KEY_BASE":  {Ref: "op://vault/x", Fingerprint: secrets.Fingerprint(key, "new")},
		"db/POSTGRES_PASSWORD": {Ref: "generated", Fingerprint: secrets.Fingerprint(key, "pg")},
	}
	got := ChangedServicesFromAudit(prev, next)
	if strings.Join(got, ",") != "web" {
		t.Fatalf("audit changed = %v", got)
	}
	for _, name := range got {
		if strings.Contains(name, "old") || strings.Contains(name, "new") || strings.Contains(name, "pg") {
			t.Fatalf("audit result leaked a value: %v", got)
		}
	}
}

func TestDetectChangedServicesUsesAudit(t *testing.T) {
	root := withRoot(t)
	ctx := context.Background()
	h := &remote.Local{}
	d := testDeploy(t, h, nil)
	dir := release.AppDir(d.App, d.Destination)
	key, err := ensureHMACKey(ctx, h, dir)
	if err != nil {
		t.Fatal(err)
	}
	generated, err := ensureGenerated(ctx, h, dir, map[string]string{"POSTGRES_PASSWORD": "password32"})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := serviceSecrets(d, generated, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	cur := &release.Release{
		App: d.App, Destination: d.Destination, Version: "v1", Status: "deployed",
		Secrets: secretAudit(d, svc, generated, key),
	}
	got, err := DetectChangedServices(ctx, d, cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("same values: %v (root %s)", got, root)
	}
	d.ServiceSecrets["web"]["SECRET_KEY_BASE"] = "rotated-secret-value"
	got, err = DetectChangedServices(ctx, d, cur)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "web" {
		t.Fatalf("rotated web: %v", got)
	}
	joined := strings.Join(got, ",")
	if strings.Contains(joined, "rotated") || strings.Contains(joined, webSecret) || strings.Contains(joined, "pg-value") {
		t.Fatal("secret value leaked from DetectChangedServices")
	}
	if _, err := hex.DecodeString(hex.EncodeToString(key)); err != nil {
		t.Fatal(err)
	}
}

func TestDetectChangedServicesFallsBackToGeneration(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &remote.Local{}
	d := testDeploy(t, h, nil)
	dir := release.AppDir(d.App, d.Destination)
	generated, err := ensureGenerated(ctx, h, dir, map[string]string{"POSTGRES_PASSWORD": "password32"})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := serviceSecrets(d, generated, t.Logf)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeSecrets(ctx, h, path.Join(dir, "secrets", "G1"), svc, map[string]bool{"cloudflared": true}); err != nil {
		t.Fatal(err)
	}
	cur := &release.Release{App: d.App, Destination: d.Destination, Version: "v1", Status: "deployed", SecretsGeneration: "G1"}
	got, err := DetectChangedServices(ctx, d, cur)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("same generation: %v", got)
	}
	d.ServiceSecrets["cloudflared"]["TUNNEL_TOKEN"] = "new-tunnel-token-value"
	got, err = DetectChangedServices(ctx, d, cur)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "cloudflared" {
		t.Fatalf("env secret: %v", got)
	}
	if strings.Contains(strings.Join(got, ","), "new-tunnel") || strings.Contains(strings.Join(got, ","), tunnelSecret) {
		t.Fatal("secret value leaked from generation fallback")
	}
}

func TestRestartSelected(t *testing.T) {
	if !restartSelected(nil, "web") || !restartSelected(nil, "db") {
		t.Fatal("empty Restart restarts every Service")
	}
	if !restartSelected([]string{"web"}, "web") || restartSelected([]string{"web"}, "db") {
		t.Fatal("named Restart must be exact")
	}
}

func TestDetectChangedServicesMissingGeneration(t *testing.T) {
	withRoot(t)
	h := &remote.Local{}
	d := testDeploy(t, h, nil)
	if _, err := ensureGenerated(context.Background(), h, release.AppDir(d.App, d.Destination), map[string]string{"POSTGRES_PASSWORD": "password32"}); err != nil {
		t.Fatal(err)
	}
	cur := &release.Release{App: d.App, Destination: d.Destination, Version: "v1", Status: "deployed", SecretsGeneration: "GONE"}
	if _, err := DetectChangedServices(context.Background(), d, cur); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("want missing generation error, got %v", err)
	}
}
