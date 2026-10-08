package swarm

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
)

func TestDiffFirstDeploy(t *testing.T) {
	withRoot(t)
	h := newFake(newSim())
	d := testDeploy(t, h, nil)
	changes, err := Runtime{}.Diff(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}
	if stringsContainDocker(h, "stack deploy") {
		t.Fatal("diff must not deploy")
	}
	got := changeKeys(changes)
	want := []string{
		"service cloudflared create",
		"service db create will be generated",
		"service web create will be generated",
		"service worker create",
		"route shop-production-web create",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestDiffComparesCurrent(t *testing.T) {
	withRoot(t)
	h := newFake(newSim())
	d := testDeploy(t, h, nil)
	key := bytesKey()
	writeHMAC(t, key)
	const generated = "generated-db-password"
	writeGenerated(t, "POSTGRES_PASSWORD", generated)
	svc, pending, err := mergeSecrets(d, map[string]string{"POSTGRES_PASSWORD": generated})
	if err != nil || len(pending) != 0 {
		t.Fatalf("merge %v %v", pending, err)
	}
	c, err := compile(d, compileInput{
		PinHost: "node-1", GenerationDir: release.SecretsDir(d.App, d.Destination, "20260101T000000Z-v1"),
		SvcSecrets: svc, HMACKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	putCurrent(t, c.YAML)

	ctx := context.Background()
	changes, err := Runtime{}.Diff(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, ch := range changes {
		if ch.Action != plan.ActionNoop {
			t.Fatalf("expected noop, got %+v\nall %v", ch, changeKeys(changes))
		}
	}
	if routeChanges(changes) != 0 {
		t.Fatalf("unexpected routes %v", changeKeys(changes))
	}

	db := d.Project.Services["db"]
	db.Image = "postgres:18"
	d.Project.Services["db"] = db
	worker := d.Project.Services["worker"]
	worker.Image = "shop-worker:v1"
	d.Project.Services["worker"] = worker
	d.ServiceSecrets["web"]["SECRET_KEY_BASE"] = "rotated-secret-value"
	delete(d.Project.Services, "cloudflared")
	delete(d.Ext, "cloudflared")

	changes, err = Runtime{}.Diff(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]plan.Change{}
	for _, ch := range changes {
		by[ch.Kind+"/"+ch.Name] = ch
	}
	if by["service/db"].Action != plan.ActionReplace || !by["service/db"].Downtime || !reflect.DeepEqual(by["service/db"].Reasons, []string{"image postgres:17 → postgres:18"}) {
		t.Errorf("db %+v", by["service/db"])
	}
	if by["service/worker"].Action != plan.ActionUpdate || !reflect.DeepEqual(by["service/worker"].Reasons, []string{"image shop-web:v1 → shop-worker:v1"}) {
		t.Errorf("worker %+v", by["service/worker"])
	}
	if by["service/web"].Action != plan.ActionUpdate || !reflect.DeepEqual(by["service/web"].Reasons, []string{"secrets changed"}) {
		t.Errorf("web %+v", by["service/web"])
	}
	if by["service/cloudflared"].Action != plan.ActionDelete {
		t.Errorf("cloudflared %+v", by["service/cloudflared"])
	}
	if by["service/worker"].Downtime {
		t.Error("worker update has no downtime")
	}

	d.Ext["web"] = config.ServiceExt{
		Secrets: d.Ext["web"].Secrets, ReleaseCommand: d.Ext["web"].ReleaseCommand,
	}
	changes, err = Runtime{}.Diff(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	var deleted bool
	for _, ch := range changes {
		if ch.Kind == "route" && ch.Name == "shop-production-web" && ch.Action == plan.ActionDelete {
			deleted = true
		}
	}
	if !deleted {
		t.Fatalf("route not removed: %v", changeKeys(changes))
	}

	os.Remove(filepath.Join(release.AppDir("shop", "production"), "generated", "POSTGRES_PASSWORD"))
	changes, err = Runtime{}.Diff(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	by = map[string]plan.Change{}
	for _, ch := range changes {
		by[ch.Kind+"/"+ch.Name] = ch
	}
	if by["service/db"].Action != plan.ActionReplace || !contains(by["service/db"].Reasons, "will be generated") {
		t.Errorf("pending db %+v", by["service/db"])
	}
}

func TestRegistryAuth(t *testing.T) {
	local := stackPlan{Services: []servicePlan{{Image: "shop-web:v1"}, {Image: "cloudflare/cloudflared:latest"}}}
	if registryAuth(&plan.Deploy{}, local) {
		t.Fatal("local images")
	}
	if !registryAuth(&plan.Deploy{Registry: &config.Registry{Server: "ghcr.io", Username: "me"}}, local) {
		t.Fatal("configured registry")
	}
	local.Services[0].Image = "ghcr.io/acme/shop:v1"
	if !registryAuth(&plan.Deploy{}, local) {
		t.Fatal("registry-looking image")
	}
}

func bytesKey() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = 0x2a
	}
	return k
}

func writeHMAC(t *testing.T, key []byte) {
	t.Helper()
	dir := release.AppDir("shop", "production")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hmac.key"), []byte(hex.EncodeToString(key)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeGenerated(t *testing.T, name, value string) {
	t.Helper()
	dir := filepath.Join(release.AppDir("shop", "production"), "generated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func putCurrent(t *testing.T, yaml []byte) {
	t.Helper()
	dir := release.Dir("shop", "production", "v1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), yaml, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(release.AppDir("shop", "production"), "current")
	_ = os.Remove(link)
	if err := os.Symlink("releases/v1", link); err != nil {
		t.Fatal(err)
	}
}

func changeKeys(cs []plan.Change) []string {
	var out []string
	for _, c := range cs {
		s := c.Kind + " " + c.Name + " " + string(c.Action)
		if len(c.Reasons) > 0 {
			s += " " + c.Reasons[0]
		}
		out = append(out, s)
	}
	return out
}

func routeChanges(cs []plan.Change) int {
	n := 0
	for _, c := range cs {
		if c.Kind == "route" {
			n++
		}
	}
	return n
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func stringsContainDocker(h *fakeHost, sub string) bool {
	return indexOf(h, sub) >= 0
}
