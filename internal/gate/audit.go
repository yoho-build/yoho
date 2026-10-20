package gate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/yoho-build/yoho/internal/release"
)

// AuditEntry is one line of audit.log.
type AuditEntry struct {
	Time        time.Time      `json:"time"`
	Member      string         `json:"member"`
	Key         string         `json:"key,omitempty"`    // SSH key fingerprint from the forced command
	Client      string         `json:"client,omitempty"` // SSH_CONNECTION client address
	App         string         `json:"app"`
	Destination string         `json:"destination"`
	Verb        string         `json:"verb"`
	Args        map[string]any `json:"args,omitempty"` // redacted: never compose bodies or secret values
	Exit        int            `json:"exit"`
	Error       string         `json:"error,omitempty"`
	Seconds     float64        `json:"seconds"`
}

// MaxAuditBytes is the size at which an audit log is rotated: it becomes
// <log>.1 (replacing the previous one) and a new log starts, so a Member
// probing the Gate cannot fill the disk.
var MaxAuditBytes int64 = 16 << 20

// ServerAuditPath logs requests that name no App Destination with scoped
// access (probes, typos): <root>/gate-audit.log.
func ServerAuditPath() string { return filepath.Join(release.Root, "gate-audit.log") }

// AppendAudit appends e to the audit log at p (0600, created as needed). One
// write per line, O_APPEND, so concurrent Gate runs do not interleave lines.
func AppendAudit(p string, e AuditEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() && fi.Size() >= MaxAuditBytes {
		if err := os.Rename(p, p+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// truncate shortens s for audit and error lines.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
