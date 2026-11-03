package cli

import (
	"strings"
	"testing"

	"github.com/yoho-build/yoho/internal/secrets"
)

// restore --from loads the source's Backup Target secrets; the restored
// Destination's own secrets can show up in the output too, so both redact.
func TestRestoreFromRedactsBothStores(t *testing.T) {
	j := &backupJob{
		store: secrets.New(map[string]string{"BACKUP_PASSWORD": "source-backup-pass-123"}),
		local: secrets.New(map[string]string{"API_KEY": "target-api-key-456"}),
	}
	var b strings.Builder
	w, flush := j.redact(&b)
	w.Write([]byte("pw source-backup-pass-123 key target-api-key-456\n"))
	flush()
	if out := b.String(); strings.Contains(out, "source-backup-pass-123") || strings.Contains(out, "target-api-key-456") {
		t.Fatalf("leaked: %q", out)
	}
	j.local = nil
	b.Reset()
	w, flush = j.redact(&b)
	w.Write([]byte("pw source-backup-pass-123\n"))
	flush()
	if strings.Contains(b.String(), "source-backup-pass-123") {
		t.Fatalf("leaked without local: %q", b.String())
	}
}
