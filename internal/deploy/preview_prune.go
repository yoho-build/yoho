package deploy

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// PreviewPruneOptions selects previews to remove.
type PreviewPruneOptions struct {
	OlderThan         time.Duration // >0: prune previews older than this duration
	HasKeep           bool          // true when --keep was explicitly specified
	Keep              int           // >=0: protect the N newest previews
	IncludeUnknownAge bool          // with OlderThan: also prune records without timestamps
	Now               time.Time     // reference clock; defaults to previewNow()
}

// PreviewPruneCandidate is a preview selected for removal along with why it matched.
type PreviewPruneCandidate struct {
	Record PreviewRecord
	Reason string
}

// PreviewPrunePlan partitions active previews into removed, kept, and skipped-unknown-age.
type PreviewPrunePlan struct {
	Remove            []PreviewPruneCandidate
	Keep              []PreviewRecord
	SkippedUnknownAge []PreviewRecord
}

// SelectPrunePreviews chooses previews to remove according to opts.
// Records are ordered newest-first by ActivityTime (LastDeployedAt, falling
// back to CreatedAt), with unknown timestamps sorted last. When both Keep and
// OlderThan are set, the N newest previews are always protected and OlderThan
// applies to the rest.
func SelectPrunePreviews(records []PreviewRecord, opts PreviewPruneOptions) PreviewPrunePlan {
	now := opts.Now
	if now.IsZero() {
		now = previewNow()
	}
	sorted := slices.Clone(records)
	slices.SortFunc(sorted, comparePreviewsNewestFirst)

	var plan PreviewPrunePlan
	for i, r := range sorted {
		if opts.HasKeep && i < opts.Keep {
			plan.Keep = append(plan.Keep, r)
			continue
		}
		if opts.OlderThan > 0 {
			ts := r.ActivityTime()
			if ts.IsZero() {
				if opts.IncludeUnknownAge {
					plan.Remove = append(plan.Remove, PreviewPruneCandidate{
						Record: r,
						Reason: "unknown age (--include-unknown-age)",
					})
				} else {
					plan.SkippedUnknownAge = append(plan.SkippedUnknownAge, r)
					plan.Keep = append(plan.Keep, r)
				}
				continue
			}
			age := now.Sub(ts)
			if age >= opts.OlderThan {
				plan.Remove = append(plan.Remove, PreviewPruneCandidate{
					Record: r,
					Reason: fmt.Sprintf("older than %s", FormatPreviewAge(opts.OlderThan)),
				})
			} else {
				plan.Keep = append(plan.Keep, r)
			}
			continue
		}
		if opts.HasKeep {
			plan.Remove = append(plan.Remove, PreviewPruneCandidate{
				Record: r,
				Reason: fmt.Sprintf("beyond newest %d", opts.Keep),
			})
			continue
		}
		plan.Keep = append(plan.Keep, r)
	}
	return plan
}

func comparePreviewsNewestFirst(a, b PreviewRecord) int {
	if c := compareTimeNewestFirst(a.ActivityTime(), b.ActivityTime()); c != 0 {
		return c
	}
	if c := compareTimeNewestFirst(a.CreatedAt, b.CreatedAt); c != 0 {
		return c
	}
	return strings.Compare(a.Name, b.Name)
}

func compareTimeNewestFirst(a, b time.Time) int {
	az, bz := a.IsZero(), b.IsZero()
	if az != bz {
		if az {
			return 1 // unknown sorts after known
		}
		return -1
	}
	if a.After(b) {
		return -1
	}
	if b.After(a) {
		return 1
	}
	return 0
}

// CheckMaxActive returns an error when creating targetDest would exceed
// maxActive. Updating a preview that is already active is always allowed.
func CheckMaxActive(active []PreviewRecord, targetDest string, maxActive int) error {
	if maxActive <= 0 {
		return nil
	}
	for _, p := range active {
		if p.Destination == targetDest {
			return nil
		}
	}
	if len(active) >= maxActive {
		return fmt.Errorf("preview limit reached: %d active previews (previews.max_active is %d)", len(active), maxActive)
	}
	return nil
}

// ParsePreviewDuration parses a positive duration such as "7d", "2w", "48h", or "30m".
func ParsePreviewDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if len(s) > 1 {
		unit := s[len(s)-1]
		if unit == 'd' || unit == 'w' {
			n, err := strconv.Atoi(s[:len(s)-1])
			if err != nil || n <= 0 {
				return 0, fmt.Errorf("invalid duration %q: must be positive (for example 7d, 48h, 2w)", s)
			}
			if unit == 'd' {
				return time.Duration(n) * 24 * time.Hour, nil
			}
			return time.Duration(n) * 7 * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid duration %q: must be positive (for example 7d, 48h, 2w)", s)
	}
	return d, nil
}

// FormatPreviewAge formats a non-negative age compactly (for example "<1m", "15m", "6h", "7d").
func FormatPreviewAge(d time.Duration) string {
	if d < 0 {
		return "<1m"
	}
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	}
}
