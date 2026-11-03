package deploy

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/release"
)

func TestLostDefaultLogging(t *testing.T) {
	cur := []byte(`services:
  web:
    image: a
    logging: {driver: json-file, options: {max-size: "10m", max-file: "3"}}
  db:
    image: b
    logging: {driver: json-file, options: {max-size: "10m", max-file: "3"}}
  own:
    image: c
    logging: {driver: local}
  added:
    image: d
    logging: {driver: json-file, options: {max-size: "10m", max-file: "3"}}
`)
	old := []byte(`services:
  web: {image: a}
  db: {image: b}
  own: {image: c}
`)
	if got := LostDefaultLogging(cur, old); !reflect.DeepEqual(got, []string{"db", "web"}) {
		t.Fatalf("got %v", got)
	}
	if got := LostDefaultLogging(cur, cur); len(got) != 0 {
		t.Fatalf("same compose: %v", got)
	}
}

func TestRotatedSince(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	log := `{"time":"2026-11-01T09:00:00Z","key":"OLD","fingerprint":"o1"}
not json
{"time":"2026-11-01T11:00:00Z","key":"DB_PASSWORD","fingerprint":"f1"}
{"time":"2026-11-01T12:00:00Z","key":"DB_PASSWORD","fingerprint":"f2"}
{"time":"2026-11-01T12:00:00Z","key":"KEPT","fingerprint":"k2"}
`
	recs := ParseRotations(log)
	if len(recs) != 4 {
		t.Fatalf("records %+v", recs)
	}
	target := &release.Release{DeployedAt: t0, Secrets: map[string]release.SecretAudit{
		"db/DB_PASSWORD": {Ref: "generated", Fingerprint: "f0"}, "web/DB_PASSWORD": {Ref: "generated", Fingerprint: "f0"},
		"db/KEPT": {Ref: "generated", Fingerprint: "k2"},
	}}
	got := RotatedSince(target, recs)
	if len(got) != 1 || !strings.HasPrefix(got[0], "DB_PASSWORD was rotated after this Release (2026-11-01T12:00:00Z); with --with-release-secrets its Services start with the old value the database no longer accepts. Run `yoho secrets rotate --generated DB_PASSWORD` after the rollback") {
		t.Fatalf("got %q", got)
	}
	if got := RotatedSince(&release.Release{DeployedAt: t0.Add(3 * time.Hour)}, recs); len(got) != 0 {
		t.Fatalf("Release after the rotations, no fingerprints: %q", got)
	}
	// Rolled back to once already: DeployedAt is newer, the value still old.
	again := &release.Release{DeployedAt: t0.Add(3 * time.Hour), Secrets: target.Secrets}
	if got := RotatedSince(again, recs); len(got) != 1 || !strings.HasPrefix(got[0], "DB_PASSWORD was rotated") {
		t.Fatalf("re-recorded Release: %q", got)
	}
}
