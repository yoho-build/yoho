package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func assertions(t *testing.T, script string, args ...string) error {
	t.Helper()
	for _, bin := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skip(bin + " unavailable")
		}
	}
	cmd := exec.Command("bash", append([]string{"-euo", "pipefail", "-c", `. "$1"; shift; server_run() { "$@"; }; ` + script, "assertions", "./resilience-assertions.sh"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Logf("assertion rejected input: %s", out)
	}
	return err
}

func TestRecordsValidationRejectsIncompleteState(t *testing.T) {
	for _, fault := range []string{"valid", "missing releases", "empty releases", "empty current", "missing current", "bad release", "missing operations", "empty operations", "bad operation", "wrong operation ID", "missing referenced operation"} {
		t.Run(fault, func(t *testing.T) {
			root := t.TempDir()
			base := filepath.Join(root, "apps/shop/production")
			write := func(name, body string) {
				p := filepath.Join(base, name)
				if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			release := `{"app":"shop","destination":"production","version":"v1","operation_id":"op1"}`
			write("releases/v1/release.json", release)
			if err := os.Symlink("releases/v1", filepath.Join(base, "current")); err != nil {
				t.Fatal(err)
			}
			write("operations/op1.json", `{"id":"op1","started_at":"2026-11-20T00:00:00Z","state":"finished","outcome":"success","services":{}}`)
			// Keep current independently readable when testing inventory:
			// otherwise a dangling current link could hide a vacuous loop.
			if fault == "missing releases" || fault == "empty releases" || fault == "bad release" {
				if err := os.Remove(filepath.Join(base, "current")); err != nil {
					t.Fatal(err)
				}
				write("current/release.json", release)
			}
			switch fault {
			case "missing releases":
				os.RemoveAll(filepath.Join(base, "releases"))
			case "empty releases":
				os.RemoveAll(filepath.Join(base, "releases/v1"))
			case "empty current":
				os.WriteFile(filepath.Join(base, "current/release.json"), nil, 0600)
			case "missing current":
				os.Remove(filepath.Join(base, "current"))
			case "bad release":
				write("releases/v1/release.json", "{")
			case "missing operations":
				os.RemoveAll(filepath.Join(base, "operations"))
			case "empty operations":
				os.Remove(filepath.Join(base, "operations/op1.json"))
			case "bad operation":
				write("operations/op1.json", "{")
			case "wrong operation ID":
				write("operations/op1.json", `{"id":"wrong","state":"running","started_at":"now","services":{}}`)
			case "missing referenced operation":
				write("releases/v1/release.json", `{"app":"shop","destination":"production","version":"v1","operation_id":"absent"}`)
			}
			err := assertions(t, `ROOT=$1; records_valid`, root)
			if (err == nil) != (fault == "valid") {
				t.Fatalf("%s: %v", fault, err)
			}
		})
	}
}

func TestRecoveryEventValidation(t *testing.T) {
	for _, fault := range []string{"valid", "absent", "duplicate", "missing backup_id", "wrong backup_id", "missing recovery_point", "bad recovery_point", "invalid calendar", "missing data_restore_seconds", "negative duration", "string duration", "missing full_recovery_seconds", "missing image_source", "missing scheduled_jobs_reinstalled", "invented independence"} {
		t.Run(fault, func(t *testing.T) {
			event := map[string]any{"schema_version": 1, "event": "recovery", "backup_id": "abc", "recovery_point": "2026-11-20T00:00:00.123Z", "data_restore_seconds": 1.5, "full_recovery_seconds": nil, "image_source": nil, "scheduled_jobs_reinstalled": nil}
			switch fault {
			case "wrong backup_id":
				event["backup_id"] = "other"
			case "bad recovery_point":
				event["recovery_point"] = "not a timestamp"
			case "invalid calendar":
				event["recovery_point"] = "2026-99-99T00:00:00Z"
			case "negative duration":
				event["data_restore_seconds"] = -1
			case "string duration":
				event["data_restore_seconds"] = "1"
			case "invented independence":
				event["needs_old_server"] = false
			}
			if len(fault) > 8 && fault[:8] == "missing " {
				delete(event, fault[8:])
			}
			b, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			if fault == "absent" {
				b = []byte(`{"event":"finished","outcome":"success"}`)
			}
			if fault == "duplicate" {
				b = append(append(b, '\n'), b...)
			}
			file := filepath.Join(t.TempDir(), "restore.jsonl")
			if err := os.WriteFile(file, b, 0600); err != nil {
				t.Fatal(err)
			}
			err = assertions(t, `recovery_event "$1" abc`, file)
			if (err == nil) != (fault == "valid") {
				t.Fatalf("%s: %v", fault, err)
			}
		})
	}
}
