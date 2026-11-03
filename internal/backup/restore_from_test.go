package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/config"
)

func TestCheckManifestOwner(t *testing.T) {
	for _, tc := range []struct {
		name              string
		m                 Manifest
		app, dest, source string
		ok                bool
		wantSub           string
	}{
		{"own Backup", Manifest{App: "shop", Destination: "production"}, "shop", "production", "production", true, ""},
		{"from another Destination", Manifest{App: "shop", Destination: "production"}, "shop", "staging", "production", true, ""},
		{"other Destination without --from", Manifest{App: "shop", Destination: "production"}, "shop", "staging", "staging", false, "pass --from production"},
		{"--from names another source", Manifest{App: "shop", Destination: "qa"}, "shop", "staging", "production", false, "shop/qa, not shop/production"},
		{"other App", Manifest{App: "blog", Destination: "production"}, "shop", "production", "production", false, "another App"},
		{"other App with --from", Manifest{App: "blog", Destination: "production"}, "shop", "staging", "production", false, "another App"},
	} {
		err := checkManifestOwner(tc.m, tc.app, tc.dest, tc.source)
		if tc.ok != (err == nil) || (err != nil && !strings.Contains(err.Error(), tc.wantSub)) {
			t.Errorf("%s: err = %v", tc.name, err)
		}
	}
}

// restoreFromRig restores a Backup taken by staging into production (the
// Destination the fixtures deploy), so the isolation fixtures apply unchanged.
func restoreFromRig(m Manifest, copyGenerated bool) (*fakeHost, RestoreOptions) {
	h := &fakeHost{respond: dockerResponder(nil)}
	data := "/var/lib/yoho/backups/shop/production/restore-20261008T030000Z/data"
	mj, _ := json.Marshal(m)
	h.files = map[string][]byte{
		data + "/manifest.json":                           mj,
		data + "/yoho-generated/PG":                       []byte("staging-secret-value"),
		"/var/lib/yoho/apps/shop/production/generated/PG": []byte("production-secret-value"),
	}
	return h, RestoreOptions{
		App: "shop", Destination: "production", From: "staging", CopyGenerated: copyGenerated,
		Host: h, Confirm: true,
		ID:       "yoho-shop-staging-20261001T030000Z.tar.gz",
		Target:   config.BackupTarget{Type: "archive", Repository: "/srv/b"},
		Services: map[string]config.ServiceBackup{"db": {Volumes: []string{"pgdata"}}},
		Now:      func() time.Time { return time.Date(2026, 10, 8, 3, 0, 0, 0, time.UTC) },
	}
}

func stagingManifest() Manifest {
	return Manifest{
		App: "shop", Destination: "staging", Project: "yoho-shop-staging",
		GeneratedSecrets: []string{"PG"},
		Services:         map[string]ManifestService{"db": {Volumes: map[string]string{"pgdata": "db-pgdata.tar.gz"}}},
	}
}

func TestRestoreFromKeepsGeneratedSecretsByDefault(t *testing.T) {
	h, o := restoreFromRig(stagingManifest(), false)
	var out bytes.Buffer
	o.Out = &out
	res, err := Restore(context.Background(), o)
	if err != nil {
		t.Fatalf("%v\n%s", err, h.scripts())
	}
	if h.index("/srv/b/yoho-shop-staging-20261001T030000Z.tar.gz") < 0 {
		t.Fatalf("source Backup not fetched\n%s", h.scripts())
	}
	if h.index("-exec rm -rf {} +") < 0 || len(res.Volumes) != 1 {
		t.Fatalf("volume not restored: %+v", res)
	}
	if got := string(h.files["/var/lib/yoho/apps/shop/production/generated/PG"]); got != "production-secret-value" {
		t.Fatalf("target generated secret overwritten: %q", got)
	}
	if h.index("#writefile /var/lib/yoho/apps/shop/production/generated/") >= 0 {
		t.Fatal("generated secret written without CopyGenerated")
	}
	if strings.Join(res.GeneratedSkipped, ",") != "PG" || len(res.Generated) != 0 {
		t.Fatalf("skipped %v generated %v", res.GeneratedSkipped, res.Generated)
	}
	if strings.Contains(out.String(), "secret-value") || strings.Contains(h.scripts(), "secret-value") {
		t.Fatal("secret value logged")
	}
}

func TestRestoreFromCopiesGeneratedSecretsWhenAsked(t *testing.T) {
	h, o := restoreFromRig(stagingManifest(), true)
	res, err := Restore(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(h.files["/var/lib/yoho/apps/shop/production/generated/PG"]); got != "staging-secret-value" {
		t.Fatalf("generated secret not copied: %q", got)
	}
	if strings.Join(res.GeneratedChanged, ",") != "PG" || len(res.GeneratedSkipped) != 0 {
		t.Fatalf("changed %v skipped %v", res.GeneratedChanged, res.GeneratedSkipped)
	}
}

func TestRestoreFromRefusesForeignBackupsBeforeStopping(t *testing.T) {
	other := stagingManifest()
	other.App = "blog"
	wrongDest := stagingManifest()
	wrongDest.Destination = "qa"
	for name, m := range map[string]Manifest{"other App": other, "other source": wrongDest} {
		h, o := restoreFromRig(m, true)
		_, err := Restore(context.Background(), o)
		if err == nil {
			t.Fatalf("%s: restore accepted", name)
		}
		if h.index("docker stop") >= 0 || h.index("-exec rm -rf {} +") >= 0 || h.index("#writefile /var/lib/yoho/apps/") >= 0 {
			t.Fatalf("%s: changed the App before refusing\n%s", name, h.scripts())
		}
	}
	// An archive named for another Destination than --from is never fetched.
	h, o := restoreFromRig(stagingManifest(), false)
	o.ID = "yoho-shop-production-20261001T030000Z.tar.gz"
	if _, err := Restore(context.Background(), o); err == nil || !strings.Contains(err.Error(), "does not belong") {
		t.Fatalf("err = %v", err)
	}
	if h.index("cp '/srv/b/") >= 0 {
		t.Fatal("fetched an archive of another Destination")
	}
	// Without From, another Destination's Backup is refused with a hint.
	h, o = restoreFromRig(stagingManifest(), false)
	o.From = ""
	o.ID = "yoho-shop-production-20261001T030000Z.tar.gz"
	if _, err := Restore(context.Background(), o); err == nil || !strings.Contains(err.Error(), "--from staging") {
		t.Fatalf("err = %v", err)
	}
	if h.index("docker stop") >= 0 {
		t.Fatal("stopped the App")
	}
}

func TestListFromUsesSourceIdentityAndLocalFiles(t *testing.T) {
	h := &fakeHost{respond: func(context.Context, string) (string, error) { return "[]", nil }}
	_, err := List(context.Background(), ListOptions{
		App: "shop", Destination: "production", From: "staging", Host: h,
		Target: config.BackupTarget{Repository: "/srv/restic"}, Password: secretPW,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := h.scripts()
	if !strings.Contains(s, "destination=staging") || strings.Contains(s, "destination=production") {
		t.Fatalf("list must select the source Destination's snapshots:\n%s", s)
	}
	if !strings.Contains(s, "/var/lib/yoho/backups/shop/production/.restic-cache") {
		t.Fatalf("list must use the local Destination's spool:\n%s", s)
	}
}

func TestDumpOnlyGenerated(t *testing.T) {
	m := Manifest{Services: map[string]ManifestService{
		"pg":    {Dump: "pg.dump"},
		"mysql": {Dump: "mysql.dump", Volumes: map[string]string{"data": "mysql-data.tar"}},
		"vol":   {Volumes: map[string]string{"data": "vol-data.tar"}},
	}}
	owners := map[string][]string{
		"POSTGRES_PASSWORD": {"pg", "web"}, // web is not backed up: pg decides
		"MYSQL_PASSWORD":    {"mysql"},     // its volumes restore the roles
		"VOL_KEY":           {"vol"},
		"APP_KEY":           {"web"},       // owner not in the Backup
		"SHARED":            {"pg", "vol"}, // one owner restores volumes
	}
	names := []string{"APP_KEY", "MYSQL_PASSWORD", "POSTGRES_PASSWORD", "SHARED", "UNKNOWN", "VOL_KEY"}
	got := dumpOnlyGenerated(m, names, owners)
	if len(got) != 1 || got["POSTGRES_PASSWORD"] != "pg" {
		t.Fatalf("got %v", got)
	}
	if got := dumpOnlyGenerated(m, names, nil); len(got) != 0 {
		t.Fatalf("no owners: %v", got)
	}
}
