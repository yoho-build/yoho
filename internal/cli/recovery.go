package cli

import (
	"fmt"
	"time"

	"github.com/yoho-build/yoho/internal/backup"
	"github.com/yoho-build/yoho/internal/ui"
)

// reportRecovery reports only facts this command observes. A restore cannot
// measure provisioning or image reconstruction, or prove ingress and workers
// healthy. The recovery harness supplies those facts after the final apply.
func reportRecovery(u *ui.UI, entry *backup.Entry, elapsed time.Duration) {
	u.Event("recovery", fmt.Sprintf("Recovery point %s; data restore %.3fs; needs old Server: false. Full recovery time, image source and Scheduled Jobs: verify after `yoho apply`.", entry.Time.UTC().Format(time.RFC3339Nano), elapsed.Seconds()), map[string]any{
		"backup_id":                  entry.ID,
		"recovery_point":             entry.Time.UTC().Format(time.RFC3339Nano),
		"data_restore_seconds":       elapsed.Seconds(),
		"full_recovery_seconds":      nil,
		"image_source":               nil,
		"needs_old_server":           false,
		"scheduled_jobs_reinstalled": nil,
	})
}
