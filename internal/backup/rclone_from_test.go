package backup

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
)

const persistentRcloneDir = "/var/lib/yoho/backups/shop/production/.rclone/"

// assertTempRclone checks that restore --from used the source's rclone
// config only from a private file under tempRe, removed it afterwards, and
// never wrote or mounted this Destination's persistent rclone config.
func assertTempRclone(t *testing.T, h *fakeHost, tempRe *regexp.Regexp, secret string) {
	t.Helper()
	var conf string
	for p, b := range h.files {
		if strings.HasPrefix(p, persistentRcloneDir) {
			t.Fatalf("wrote the persistent rclone config %s", p)
		}
		if string(b) == secret {
			conf = p
		}
	}
	if !tempRe.MatchString(conf) {
		t.Fatalf("source config written to %q, want a staging path like %s", conf, tempRe)
	}
	if m := h.modes[conf]; m != 0o600 {
		t.Fatalf("temporary rclone config mode %o", m)
	}
	dir := strings.TrimSuffix(conf, "/rclone.conf")
	s := fixturePath(h.scripts())
	if strings.Contains(s, persistentRcloneDir) {
		t.Fatalf("scripts reference the persistent rclone config:\n%s", s)
	}
	for _, want := range []string{
		"-v '" + dir + ":/yoho-rclone'",
		"yoho_secret '" + conf + "'",
		"rm -rf '" + dir + "'",
		"rc ",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("scripts lack %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, rcloneSecretToken) {
		t.Fatal("rclone secret in a script")
	}
	// The temporary copy is removed after the last rclone call.
	last, rm := -1, -1
	for i, c := range h.cmds {
		if strings.Contains(c.Script, "rc() {") {
			last = i
		}
		if strings.Contains(fixturePath(c.Script), "rm -rf '"+dir+"'") {
			rm = i
		}
	}
	if rm < last {
		t.Fatalf("temporary rclone config removed before the last rclone call (%d < %d)", rm, last)
	}
}

func TestListFromUsesTemporaryRcloneConfig(t *testing.T) {
	h := rcloneHost()
	secret := rcloneSecret("prod")
	if _, err := List(context.Background(), ListOptions{
		App: "shop", Destination: "staging", From: "production", Host: h,
		Target: rcloneTarget(), Password: "pw", RcloneConfig: secret,
	}); err != nil {
		t.Fatalf("%v\n%s", err, h.scripts())
	}
	// Restoring into staging: the source (production) config must not be
	// seeded into staging's persistent dir either.
	for p := range h.files {
		if strings.Contains(p, "/staging/.rclone/") {
			t.Fatalf("seeded staging's persistent rclone config %s", p)
		}
	}
	// Reuse the production-relative assertion by restoring into production
	// from staging as well.
	h = rcloneHost()
	if _, err := List(context.Background(), ListOptions{
		App: "shop", Destination: "production", From: "staging", Host: h,
		Target: rcloneTarget(), Password: "pw", RcloneConfig: secret,
	}); err != nil {
		t.Fatalf("%v\n%s", err, h.scripts())
	}
	assertTempRclone(t, h, regexp.MustCompile(`^/var/lib/yoho/backups/shop/production/check-[0-9a-f]{32}/\.rclone/rclone\.conf$`), secret)
}

func TestRestoreFromUsesTemporaryRcloneConfig(t *testing.T) {
	h, o := restoreFromRig(stagingManifest(), false)
	secret := rcloneSecret("staging")
	o.Target = config.BackupTarget{Type: "archive", Repository: "od:backups/shop", RcloneConfigSecret: "RCLONE_CONF"}
	o.RcloneConfig = secret
	if _, err := Restore(context.Background(), o); err != nil {
		t.Fatalf("%v\n%s", err, h.scripts())
	}
	if h.index("rc copyto 'od:backups/shop/yoho-shop-staging-20261001T030000Z.tar.gz'") < 0 {
		t.Fatalf("source Backup not fetched over rclone\n%s", h.scripts())
	}
	assertTempRclone(t, h, regexp.MustCompile(`^/var/lib/yoho/backups/shop/production/restore-20261008T030000Z/\.rclone/rclone\.conf$`), secret)
}

func TestRestoreFromRequiresSourceRcloneConfig(t *testing.T) {
	h, o := restoreFromRig(stagingManifest(), false)
	o.Target = config.BackupTarget{Type: "archive", Repository: "od:backups/shop", RcloneConfigSecret: "RCLONE_CONF"}
	if _, err := Restore(context.Background(), o); err == nil || !strings.Contains(err.Error(), "RCLONE_CONF") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(h.scripts(), persistentRcloneDir) {
		t.Fatal("fell back to the persistent rclone config")
	}
}
