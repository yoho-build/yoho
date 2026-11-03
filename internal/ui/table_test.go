package ui

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// A column named like an envelope field must not overwrite the envelope
// (or be overwritten by it).
func TestTableJSONReservedKeys(t *testing.T) {
	var b bytes.Buffer
	u := New(&b, JSON, false)
	u.Table([]string{"PHASE", "TIME", "EVENT", "BACKUP ID"}, [][]string{{"download", "1.2s", "x", "abc"}})
	line := strings.TrimSpace(b.String())
	var m map[string]any
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("%v: %s", err, line)
	}
	if m["event"] != "row" || m["row_time"] != "1.2s" || m["row_event"] != "x" || m["phase"] != "download" || m["backup_id"] != "abc" {
		t.Fatalf("row = %s", line)
	}
	if tm, _ := m["time"].(string); !strings.Contains(tm, "T") {
		t.Fatalf("envelope time lost: %s", line)
	}
}
