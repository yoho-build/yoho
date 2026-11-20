package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yoho-build/yoho/internal/backup"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/ui"
)

func TestRecoveryReport(t *testing.T) {
	for _, jsonMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "human", true: "json"}[jsonMode], func(t *testing.T) {
			var out bytes.Buffer
			mode := ui.Human
			if jsonMode {
				mode = ui.JSON
			}
			u := ui.New(&out, mode, false)
			entry := &backup.Entry{ID: "selected", Time: time.Date(2026, 11, 20, 1, 2, 3, 0, time.UTC)}
			reportRecovery(u, entry, 1500*time.Millisecond)
			if !jsonMode {
				if !strings.Contains(out.String(), "2026-11-20T01:02:03Z") || !strings.Contains(out.String(), "1.500s") {
					t.Fatal(out.String())
				}
				return
			}
			var event map[string]any
			if err := json.Unmarshal(out.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if event["event"] != "recovery" || event["backup_id"] != "selected" || event["data_restore_seconds"] != 1.5 {
				t.Fatal(event)
			}
			if _, ok := event["needs_old_server"]; ok {
				t.Fatal("restore cannot prove independence from the old Server")
			}
			for _, key := range []string{"full_recovery_seconds", "image_source", "scheduled_jobs_reinstalled"} {
				if v, ok := event[key]; !ok || v != nil {
					t.Fatalf("unobserved %s: %v", key, event)
				}
			}
		})
	}
}

func TestRestoreReportRefusesCleanup(t *testing.T) {
	c := backupRestoreCmd(&globals{})
	c.SetArgs([]string{"--cleanup", "--report"})
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "--report") {
		t.Fatalf("cleanup/report: %v", err)
	}
}

func TestQuickstartBackupRemainsOptIn(t *testing.T) {
	cfg, err := config.Load("../../examples/quickstart/yoho.yml")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Backups.Jobs) != 0 || len(cfg.Backups.Targets) != 0 {
		t.Fatal("quickstart must deploy without Backup storage or RESTIC_PASSWORD")
	}
}
