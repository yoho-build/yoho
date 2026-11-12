package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// PreviewRecord authorizes destructive teardown of this exact App Destination.
// Written under the Destination lock before building, so failed first deploys
// can also be torn down. Version is the last attempted Version.
type PreviewRecord struct {
	App         string   `json:"app"`
	Name        string   `json:"name"`
	Destination string   `json:"destination"`
	Project     string   `json:"project"`
	Hosts       []string `json:"hosts"`
	Services    []string `json:"services"`
	Version     string   `json:"version"`
}

func (p PreviewRecord) Validate(app, dest string) error {
	n, err := config.PreviewName(p.Name)
	if err != nil || n != p.Name || p.App != app || p.Destination != dest || dest != "preview-"+n || p.Project != release.ProjectName(app, dest) {
		return fmt.Errorf("refusing: invalid preview marker for %s/%s", app, dest)
	}
	return nil
}

func ReadPreview(ctx context.Context, h remote.Host, app, dest string) (*PreviewRecord, error) {
	f := remote.Quote(path.Join(release.AppDir(app, dest), "preview.json"))
	out, err := h.Output(ctx, remote.Cmd{Script: "set -eu\nif [ -L " + f + " ]; then exit 1; fi\nif [ -f " + f + " ]; then cat " + f + "; else printf missing; fi"})
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "missing" {
		return nil, nil
	}
	var p PreviewRecord
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return nil, fmt.Errorf("invalid preview marker: %w", err)
	}
	if err := p.Validate(app, dest); err != nil {
		return nil, err
	}
	return &p, nil
}

// MarkPreview never adopts an unmarked deployed Destination or Docker project.
// The caller holds the Destination lock.
func MarkPreview(ctx context.Context, h remote.Host, p PreviewRecord) error {
	if err := p.Validate(p.App, p.Destination); err != nil {
		return err
	}
	old, err := ReadPreview(ctx, h, p.App, p.Destination)
	if err != nil {
		return err
	}
	if old == nil {
		dir := remote.Quote(release.AppDir(p.App, p.Destination))
		out, err := h.Output(ctx, remote.Cmd{Script: "set -eu\nif [ -e " + dir + "/current ] || [ -d " + dir + "/releases ]; then echo deployed; fi"})
		if err != nil {
			return err
		}
		inv, err := InspectRemove(ctx, h, RemoveOptions{App: p.App, Destination: p.Destination, Services: p.Services})
		if err != nil {
			return err
		}
		if strings.TrimSpace(out) != "" || len(inv.Containers)+len(inv.Volumes)+len(inv.Networks)+len(inv.Routes) > 0 {
			return fmt.Errorf("refusing to adopt unmarked Destination %s as a preview", p.Destination)
		}
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	f := path.Join(release.AppDir(p.App, p.Destination), "preview.json")
	if err := h.WriteFile(ctx, f+".tmp", b, 0o600, false); err != nil {
		return err
	}
	return h.Run(ctx, remote.Cmd{Script: "set -eu\nmv -f " + remote.Quote(f+".tmp") + " " + remote.Quote(f)})
}

// RemovePreview uses the same exact-label removal as `yoho remove`. It removes
// the marker last, so an interrupted teardown can safely be retried.
// The caller holds the Destination lock and has checked configured collisions.
func RemovePreview(ctx context.Context, h remote.Host, app, dest string, sudo bool) (*PreviewRecord, error) {
	p, err := ReadPreview(ctx, h, app, dest)
	if err != nil {
		return nil, err
	}
	if p == nil {
		// A genuinely absent preview is success; unmarked resources are refused.
		inv, err := InspectRemove(ctx, h, RemoveOptions{App: app, Destination: dest})
		if err != nil {
			return nil, err
		}
		if len(inv.Containers)+len(inv.Volumes)+len(inv.Networks)+len(inv.Routes) > 0 {
			return nil, fmt.Errorf("refusing to remove %s without a preview marker", dest)
		}
		// AcquireLock may have created the directory. Existing Releases are not ours.
		dir := remote.Quote(release.AppDir(app, dest))
		out, err := h.Output(ctx, remote.Cmd{Script: "set -eu\nfind " + dir + " -mindepth 1 -maxdepth 1 ! -name lock ! -name .yoho-app.json -print"})
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(out) != "" {
			return nil, fmt.Errorf("refusing to remove %s without a preview marker", dest)
		}
		return nil, nil
	}
	o := RemoveOptions{App: app, Destination: dest, Volumes: true, Services: p.Services, Sudo: sudo}
	inv, err := InspectRemove(ctx, h, o)
	if err != nil {
		return nil, err
	}
	if err := RemoveRoutes(ctx, h, inv.Routes); err != nil {
		return nil, err
	}
	for _, script := range []string{RemoveContainersScript(o), RemoveNetworksScript(o), RemoveVolumesScript(o)} {
		if err := h.Run(ctx, remote.Cmd{Script: script}); err != nil {
			return nil, err
		}
	}
	if err := RemoveAppDir(ctx, h, o); err != nil {
		return nil, err
	}
	return p, nil
}
