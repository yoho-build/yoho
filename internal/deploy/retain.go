package deploy

import "github.com/yoho-build/yoho/internal/release"

// keepFailedReleases is how many of the newest failed Release records are
// kept for diagnostics beyond the retained successful ones.
const keepFailedReleases = 1

// RetainedReleases returns the versions of rels (newest first) that pruning
// keeps: the newest `retain` Releases that are not failed, the Release that
// `current` names, and the newest failed record. A run of failed deploys must
// not push the last good Releases (rollback targets) out of retention.
func RetainedReleases(rels []release.Release, retain int, current string) map[string]bool {
	keep := map[string]bool{}
	good, failed := 0, 0
	for _, rel := range rels {
		switch {
		case rel.Version == current:
			keep[rel.Version] = true
			if rel.Status != "failed" {
				good++
			}
		case rel.Status == "failed":
			if failed < keepFailedReleases {
				keep[rel.Version] = true
				failed++
			}
		case good < retain:
			keep[rel.Version] = true
			good++
		}
	}
	return keep
}
