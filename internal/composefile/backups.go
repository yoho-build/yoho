package composefile

import (
	"fmt"
	"slices"
	"strings"

	"github.com/yoho-build/yoho/internal/config"
)

// UncoveredStateful returns the Stateful Services of r that no Backup job of
// destName covers, sorted. A job covers a Service it names in services, or,
// without services, every Service with x-yoho.backup (as `yoho backup run`
// selects them).
func UncoveredStateful(destName string, b config.BackupsConfig, r *Result) []string {
	if r == nil || r.Project == nil {
		return nil
	}
	var out []string
	for _, name := range r.Project.ServiceNames() {
		ext := r.Ext[name]
		if !ext.Stateful {
			continue
		}
		covered := false
		for _, j := range b.Jobs {
			if j.Destination != destName {
				continue
			}
			if len(j.Services) == 0 && ext.Backup != nil || slices.Contains(j.Services, name) {
				covered = true
				break
			}
		}
		if !covered {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// CheckBackupCoverage warns once when Stateful Services of the Destination
// have no Backup job covering them.
func CheckBackupCoverage(destName string, b config.BackupsConfig, r *Result) []Finding {
	names := UncoveredStateful(destName, b, r)
	if len(names) == 0 {
		return nil
	}
	who, its := "Stateful Service "+names[0]+" has", "its"
	if len(names) > 1 {
		who, its = "Stateful Services "+strings.Join(names, ", ")+" have", "their"
	}
	var noExt []string
	for _, n := range names {
		if r.Ext[n].Backup == nil {
			noExt = append(noExt, n)
		}
	}
	hint := "add x-yoho.backup to the Service and a backups job for destination " + destName + " in the Yoho file (`yoho init --template postgres` prints an example; see the Backups docs), then `yoho schedule install`"
	if len(noExt) == 0 {
		hint = "add a backups job for destination " + destName + " in the Yoho file (see the Backups docs), then `yoho schedule install`"
	}
	return []Finding{{
		Level:   LevelWarning,
		Service: names[0],
		Message: fmt.Sprintf("%s no Backup job on Destination %s; %s data exists only on the Server. Hint: %s", who, destName, its, hint),
	}}
}
