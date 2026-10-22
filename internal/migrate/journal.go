// Package migrate runs the one-shot cutover from another platform (ADR
// 0012): preflight, stop the old containers by saved ID, prove the claimed
// data is free, repoint Cloudflare Tunnel ingress, and undo all of it.
//
// It is not internal/deploy's cutover (the zero-downtime Proxy switch
// inside a deploy). Every step is idempotent and recorded in the Journal,
// <app dir>/cutover.json, which never holds secret values.
package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"time"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Journal states.
const (
	StateStarted    = "started"
	StateDone       = "done"
	StateRolledBack = "rolled_back"
)

// Container is an old container stopped by the cutover.
type Container struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Journal is the durable state of one cutover.
type Journal struct {
	ID          string `json:"id"`
	App         string `json:"app"`
	Destination string `json:"destination"`
	From        string `json:"from,omitempty"`
	State       string `json:"state"`

	StartedAt time.Time  `json:"started_at"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
	// Stopped is written before the first docker stop.
	Stopped     []Container `json:"stopped"`
	ClaimsClear bool        `json:"claims_clear,omitempty"`
	// Version of the Release the cutover deployed, when one was deployed.
	Version  string `json:"version,omitempty"`
	Deployed bool   `json:"deployed,omitempty"`

	// Hostnames repointed, the configuration before the switch (written
	// before the PUT) and a hash of the configuration read back after it.
	Hostnames       []string        `json:"hostnames,omitempty"`
	TunnelBefore    json.RawMessage `json:"tunnel_before,omitempty"`
	TunnelAfterHash string          `json:"tunnel_after_hash,omitempty"`

	DoneAt       *time.Time `json:"done_at,omitempty"`
	RolledBackAt *time.Time `json:"rolled_back_at,omitempty"`
}

// NewJournal starts a Journal for app/destination.
func NewJournal(app, destination, from string, now time.Time) *Journal {
	now = now.UTC()
	return &Journal{
		ID: "cutover-" + now.Format("20060102T150405Z"), App: app, Destination: destination,
		From: from, State: StateStarted, StartedAt: now, Stopped: []Container{},
	}
}

// IngressSwitched reports whether the ingress switch was confirmed.
func (j *Journal) IngressSwitched() bool { return j.TunnelAfterHash != "" }

// Downtime is the time from stopping the old containers to done.
func (j *Journal) Downtime() time.Duration {
	if j.StoppedAt == nil || j.DoneAt == nil {
		return 0
	}
	return j.DoneAt.Sub(*j.StoppedAt)
}

// JournalPath is the Journal of app/destination on the Server.
func JournalPath(app, destination string) string {
	return path.Join(release.AppDir(app, destination), "cutover.json")
}

// ReadJournal returns the Journal, or nil when there is none.
func ReadJournal(ctx context.Context, h remote.Host, app, destination string) (*Journal, error) {
	data, err := h.ReadFile(ctx, JournalPath(app, destination), false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read cutover journal: %w", err)
	}
	var j Journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("cutover journal %s is not valid JSON: %w", JournalPath(app, destination), err)
	}
	return &j, nil
}

// Save writes the Journal (0600).
func (j *Journal) Save(ctx context.Context, h remote.Host) error {
	data, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		return err
	}
	if err := h.WriteFile(ctx, JournalPath(j.App, j.Destination), append(data, '\n'), 0o600, false); err != nil {
		return fmt.Errorf("write cutover journal: %w", err)
	}
	return nil
}

// Action is what a deploy does about a configured cutover.
type Action int

const (
	// Skip: the cutover is done; the deploy is a normal deploy.
	Skip Action = iota
	// Start a new cutover.
	Start
	// Resume continues an interrupted cutover from its Journal.
	Resume
)

// InProgressError refuses a deploy while a cutover is unfinished.
type InProgressError struct{ Journal *Journal }

func (e *InProgressError) Error() string {
	return fmt.Sprintf("cutover %s on %s is unfinished (started %s); undo it with `yoho rollback --cutover` or continue with `yoho deploy --cutover-resume`",
		e.Journal.ID, e.Journal.Destination, e.Journal.StartedAt.Format(time.RFC3339))
}

// Next decides what a deploy does given the Journal (nil: none).
func Next(j *Journal, resume bool) (Action, error) {
	switch {
	case j == nil || j.State == StateRolledBack:
		if resume {
			return Skip, errors.New("there is no unfinished cutover to resume; run without --cutover-resume")
		}
		return Start, nil
	case j.State == StateDone:
		return Skip, nil
	case resume:
		return Resume, nil
	default:
		return Skip, &InProgressError{j}
	}
}
