package composefile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/config"
)

func TestBackupCoverage(t *testing.T) {
	dir := t.TempDir()
	compose := `services:
  web:
    image: nginx
  db:
    image: postgres
    x-yoho:
      stateful: true
      backup: {dump: ["pg_dump"]}
  cache:
    image: valkey
    x-yoho:
      stateful: true
`
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := Load(context.Background(), dir, nil, nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	none := config.BackupsConfig{}
	if got := UncoveredStateful("production", none, r); strings.Join(got, ",") != "cache,db" {
		t.Fatalf("uncovered %v", got)
	}
	fs := CheckBackupCoverage("production", none, r)
	if len(fs) != 1 || fs[0].Level != LevelWarning || !strings.Contains(fs[0].Message, "Stateful Services cache, db have no Backup job on Destination production") ||
		!strings.Contains(fs[0].Message, "add x-yoho.backup") {
		t.Fatalf("findings %+v", fs)
	}
	// A job of another Destination does not count; a default job covers
	// Services with x-yoho.backup; a named Service is covered.
	b := config.BackupsConfig{Jobs: map[string]config.BackupJob{
		"staging": {Destination: "staging", Target: "t"},
		"nightly": {Destination: "production", Target: "t"},
	}}
	if got := UncoveredStateful("production", b, r); strings.Join(got, ",") != "cache" {
		t.Fatalf("uncovered %v", got)
	}
	b.Jobs["cache"] = config.BackupJob{Destination: "production", Target: "t", Services: []string{"cache"}}
	if fs := CheckBackupCoverage("production", b, r); len(fs) != 0 {
		t.Fatalf("findings %+v", fs)
	}
	if got := UncoveredStateful("staging", b, r); strings.Join(got, ",") != "cache" {
		t.Fatalf("staging uncovered %v", got)
	}
}
