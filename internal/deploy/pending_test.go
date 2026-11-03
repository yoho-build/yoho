package deploy

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

func writeGenerated(t *testing.T, name, value string) string {
	t.Helper()
	dir := filepath.Join(release.AppDir("shop", "production"), "generated")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const pendingMsg = "pending rotation of POSTGRES_PASSWORD (generated/POSTGRES_PASSWORD.next): the database may already use the new value; run `yoho secrets rotate --generated POSTGRES_PASSWORD` to finish it"

func TestPendingRotations(t *testing.T) {
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}}
	ctx := context.Background()
	if keys, err := PendingRotations(ctx, h, "shop", "production"); err != nil || len(keys) != 0 {
		t.Fatalf("no App directory: %v %v", keys, err)
	}
	writeGenerated(t, "POSTGRES_PASSWORD", "old")
	writeGenerated(t, "POSTGRES_PASSWORD.swapped", "fp")
	writeGenerated(t, "OTHER.next.ab12CD", "temp") // ensureFileOnce temp file
	if keys, err := PendingRotations(ctx, h, "shop", "production"); err != nil || len(keys) != 0 {
		t.Fatalf("no .next: %v %v", keys, err)
	}
	writeGenerated(t, "POSTGRES_PASSWORD.next", "new")
	writeGenerated(t, "A_KEY.next", "new")
	keys, err := PendingRotations(ctx, h, "shop", "production")
	if err != nil || !slices.Equal(keys, []string{"A_KEY", "POSTGRES_PASSWORD"}) {
		t.Fatalf("keys %v %v", keys, err)
	}
	err = CheckPendingRotations(ctx, h, "shop", "production", []string{"A_KEY"})
	var pe *PendingRotationError
	if !errors.As(err, &pe) || err.Error() != pendingMsg {
		t.Fatalf("err %v", err)
	}
	if err := CheckPendingRotations(ctx, h, "shop", "production", []string{"A_KEY", "POSTGRES_PASSWORD"}); err != nil {
		t.Fatalf("allowed keys: %v", err)
	}
	if strings.Contains(err.Error(), "new") && strings.Contains(err.Error(), "old") {
		t.Fatal("value leaked")
	}
}

func TestDeployAndRollbackRefusePendingRotation(t *testing.T) {
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}, respond: respond(nil)}
	ctx := context.Background()
	if _, err := (Compose{}).Deploy(ctx, testDeploy(t, h, io.Discard)); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	writeGenerated(t, "POSTGRES_PASSWORD.next", "new-value")
	h.scripts = nil
	_, err := (Compose{}).Deploy(ctx, testDeploy(t, h, io.Discard))
	var pe *PendingRotationError
	var f *Failure
	if !errors.As(err, &pe) || errors.As(err, &f) || err.Error() != pendingMsg {
		t.Fatalf("deploy err %v", err)
	}
	if strings.Contains(h.all(), "compose") {
		t.Errorf("refused deploy ran compose:\n%s", h.all())
	}
	// The lock is released after the refusal.
	if _, err := os.Stat(LockDir("shop", "production")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("lock left behind: %v", err)
	}
	_, err = (Compose{}).Rollback(ctx, testDeploy(t, h, io.Discard), "v1")
	if !errors.As(err, &pe) || errors.As(err, &f) {
		t.Fatalf("rollback err %v", err)
	}
	// rotate --generated rolls out under its own lock with the key it rotates.
	if _, err := (Compose{Rotating: []string{"POSTGRES_PASSWORD"}}).Deploy(ctx, testDeploy(t, h, io.Discard)); errors.As(err, &pe) {
		t.Fatalf("Rotating key refused: %v", err)
	}
}

func TestDeployLockedIsARefusal(t *testing.T) {
	withRoot(t)
	h := &dockerHost{local: &remote.Local{}}
	ctx := context.Background()
	unlock, err := AcquireLock(ctx, h, "shop", "production", LockInfo{Performer: "carol", Command: "secrets rotate"})
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(ctx)
	for _, run := range []func() error{
		func() error { _, err := (Compose{}).Deploy(ctx, testDeploy(t, h, nil)); return err },
		func() error { _, err := (Compose{}).Rollback(ctx, testDeploy(t, h, nil), "v1"); return err },
	} {
		err := run()
		var le *LockedError
		var f *Failure
		if !errors.As(err, &le) || errors.As(err, &f) || strings.HasPrefix(err.Error(), "deploy phase") {
			t.Fatalf("err %v", err)
		}
	}
}

func TestRotationSwapped(t *testing.T) {
	withRoot(t)
	ctx := context.Background()
	h := &rotHost{}
	p := writeGenerated(t, "API_TOKEN", "old-token-value")
	g := &GeneratedRotation{Key: "API_TOKEN", Kind: "password32", Service: "web", Strategy: RotateNone, Consumers: []string{"web"}}
	if err := RotateGeneratedSecret(ctx, h, "shop", "production", "alice", g, nil, nil); err != nil {
		t.Fatal(err)
	}
	marker, err := os.ReadFile(p + ".swapped")
	if err != nil {
		t.Fatalf("no swap marker: %v", err)
	}
	val, _ := os.ReadFile(p)
	keyHex, _ := os.ReadFile(filepath.Join(release.AppDir("shop", "production"), "hmac.key"))
	hk := mustHex(t, strings.TrimSpace(string(keyHex)))
	fp := secrets.Fingerprint(hk, string(val))
	if strings.TrimSpace(string(marker)) != fp || strings.Contains(string(marker), string(val)) {
		t.Fatalf("marker %q, want fingerprint %s", marker, fp)
	}
	oldRel := &release.Release{Secrets: map[string]release.SecretAudit{"web/API_TOKEN": {Fingerprint: secrets.Fingerprint(hk, "old-token-value")}}}
	// Killed after the swap: the current Release still has the old value.
	if ok, err := RotationSwapped(ctx, h, "shop", "production", oldRel, "API_TOKEN"); err != nil || !ok {
		t.Fatalf("swapped = %v %v", ok, err)
	}
	// A pending .next takes precedence (the database step resumes).
	writeGenerated(t, "API_TOKEN.next", "another")
	if ok, _ := RotationSwapped(ctx, h, "shop", "production", oldRel, "API_TOKEN"); ok {
		t.Fatal(".next present: want false")
	}
	os.Remove(p + ".next")
	// Rolled out since (plain rotate or deploy): the marker is stale.
	newRel := &release.Release{Secrets: map[string]release.SecretAudit{"web/API_TOKEN": {Fingerprint: fp}}}
	if ok, err := RotationSwapped(ctx, h, "shop", "production", newRel, "API_TOKEN"); err != nil || ok {
		t.Fatalf("rolled out: %v %v", ok, err)
	}
	if _, err := os.Stat(p + ".swapped"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale marker kept")
	}
	if ok, _ := RotationSwapped(ctx, h, "shop", "production", oldRel, "API_TOKEN"); ok {
		t.Fatal("no marker: want false")
	}
	// A marker that does not match generated/<KEY> is stale too.
	writeGenerated(t, "API_TOKEN.swapped", "0000000000000000\n")
	if ok, _ := RotationSwapped(ctx, h, "shop", "production", oldRel, "API_TOKEN"); ok {
		t.Fatal("mismatched marker: want false")
	}
	writeGenerated(t, "API_TOKEN.swapped", fp+"\n")
	if err := ClearRotationSwapped(ctx, h, "shop", "production", []string{"API_TOKEN"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p + ".swapped"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("marker not cleared")
	}
}

func TestDiffAnnotatesDefaultLogging(t *testing.T) {
	h, _, root := deployed(t)
	// A Release deployed before v0.6.0 has no logging on its Services.
	p := filepath.Join(root, "apps", "shop", "production", "current", "compose.yaml")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	for _, s := range doc["services"].(map[string]any) {
		delete(s.(map[string]any), "logging")
	}
	out, _ := yaml.Marshal(doc)
	if err := os.WriteFile(p, out, 0o600); err != nil {
		t.Fatal(err)
	}
	cs, err := Compose{}.Diff(context.Background(), testDeploy(t, h, io.Discard))
	if err != nil {
		t.Fatal(err)
	}
	c := byKey(cs)["service db"]
	want := "config changed: logging (Yoho's default log rotation since v0.6.0; set `logging` on the Service to keep your own)"
	if c.Action != plan.ActionReplace || len(c.Reasons) != 1 || c.Reasons[0] != want {
		t.Fatalf("db %+v", c)
	}
}

func TestAnnotateLoggingKeys(t *testing.T) {
	def := map[string]any{"driver": "json-file", "options": map[string]any{"max-size": "10m", "max-file": "3"}}
	if got := AnnotateLoggingKeys([]string{"environment", "logging"}, def); got[0] != "environment" || got[1] != DefaultLoggingNote {
		t.Errorf("default: %v", got)
	}
	own := map[string]any{"driver": "json-file", "options": map[string]any{"max-size": "50m"}}
	if got := AnnotateLoggingKeys([]string{"logging"}, own); got[0] != "logging" {
		t.Errorf("own logging annotated: %v", got)
	}
	if got := AnnotateLoggingKeys([]string{"logging"}, nil); got[0] != "logging" {
		t.Errorf("removed logging annotated: %v", got)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
