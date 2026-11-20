package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

func TestCandidateWaitRequiresRunningVersionedService(t *testing.T) {
	for _, scenario := range []string{"candidate", "old and candidate", "old web", "release command", "lowercase oneoff", "created", "exited", "wrong App", "wrong Destination", "wrong Service", "wrong project", "empty", "duplicate", "docker error"} {
		t.Run(scenario, func(t *testing.T) {
			row := map[string]any{"id": "new-web", "state": "running", "project": "yoho-shop-production", "app": "shop", "destination": "production", "service": "web", "version": "new", "oneoff": "False"}
			switch scenario {
			case "old web":
				row["version"] = "old"
			case "release command":
				row["oneoff"] = "True"
			case "lowercase oneoff":
				row["oneoff"] = "true"
			case "created", "exited":
				row["state"] = scenario
			case "wrong App":
				row["app"] = "other"
			case "wrong Destination":
				row["destination"] = "staging"
			case "wrong Service":
				row["service"] = "worker"
			case "wrong project":
				row["project"] = "other"
			}
			b, err := json.Marshal(row)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "empty" {
				b = nil
			}
			if scenario == "duplicate" {
				b = append(append(b, '\n'), b...)
			}
			if scenario == "old and candidate" {
				old := []byte(`{"id":"old-web","state":"running","project":"yoho-shop-production","app":"shop","destination":"production","service":"web","version":"old","oneoff":"False"}`)
				b = append(append(old, '\n'), b...)
			}
			dir := t.TempDir()
			rows, query := filepath.Join(dir, "rows.jsonl"), filepath.Join(dir, "query.txt")
			if err := os.WriteFile(rows, b, 0600); err != nil {
				t.Fatal(err)
			}
			err = assertions(t, `fixture=$1; query=$2; fault=$3; docker() { printf '%s\n' "$@" > "$query"; [[ $fault != 'docker error' ]] || return 1; cat "$fixture"; }; candidate_started new`, rows, query, scenario)
			want := scenario == "candidate" || scenario == "old and candidate"
			if (err == nil) != want {
				t.Fatalf("%s: %v", scenario, err)
			}
			args, err := os.ReadFile(query)
			if err != nil {
				t.Fatal(err)
			}
			for _, filter := range []string{"ps", "status=running", "label=com.docker.compose.project=yoho-shop-production", "label=com.docker.compose.service=web", "label=yoho.app=shop", "label=yoho.destination=production", "label=yoho.service=web", "label=yoho.version=new"} {
				if !strings.Contains("\n"+string(args), "\n"+filter+"\n") {
					t.Fatalf("missing Docker query filter %q: %s", filter, args)
				}
			}
		})
	}
}

func TestDockerFaultResultUsesRecordedServiceChanges(t *testing.T) {
	for _, tc := range []struct {
		name, outcome string
		code          int
		changed, want bool
	}{
		{"unchanged failed", "failed", 1, false, true},
		{"changed failed", "failed", 1, true, false},
		{"changed partial", "partial", 13, true, true},
		{"unchanged partial", "partial", 13, false, false},
		{"changed unknown", "unknown", 14, true, true},
		{"unchanged unknown", "unknown", 14, false, true},
		{"incorrect partial code", "partial", 1, true, false},
		{"incorrect unknown code", "unknown", 1, false, false},
		{"pre-mutation code", "failed", 12, false, false},
		{"false success", "success", 0, true, false},
		{"missing record", "failed", 1, false, false},
		{"missing services", "failed", 1, false, false},
		{"wrong operation", "failed", 1, false, false},
		{"missing finished", "failed", 1, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := "attempted"
			if tc.changed {
				state = "changed"
			}
			record := map[string]any{"id": "op1", "services": map[string]string{"worker": state}}
			if tc.name == "missing services" {
				delete(record, "services")
			}
			if tc.name == "wrong operation" {
				record["id"] = "other"
			}
			b, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name == "missing record" {
				b = nil
			}
			dir := t.TempDir()
			recordFile, eventsFile := filepath.Join(dir, "operation.json"), filepath.Join(dir, "events.jsonl")
			if err := os.WriteFile(recordFile, b, 0600); err != nil {
				t.Fatal(err)
			}
			events := []byte(`{"event":"operation_started","operation_id":"op1"}` + "\n")
			finished, err := json.Marshal(map[string]any{"event": "finished", "operation_id": "op1", "outcome": tc.outcome})
			if err != nil {
				t.Fatal(err)
			}
			if tc.name != "missing finished" {
				events = append(events, finished...)
			}
			if err := os.WriteFile(eventsFile, events, 0600); err != nil {
				t.Fatal(err)
			}
			err = assertions(t, `docker_fault_result "$1" "$2" "$3"`, eventsFile, recordFile, strconv.Itoa(tc.code))
			if (err == nil) != tc.want {
				t.Fatalf("%s: %v", tc.name, err)
			}
		})
	}
}
