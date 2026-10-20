package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/compose-spec/compose-go/v2/types"

	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// Checks of the Server's Docker state that the compose project alone
// cannot decide: names shared with other Apps, images and volumes that
// already exist.

// collisions refuses an App Destination whose names another App
// Destination on this Server also uses: the compose project (shop/prod-web
// and shop-prod/web are both yoho-shop-prod-web) or a built image (shop/prod
// with Service web-api and shop-prod/web with Service api both build
// yoho/shop-prod-web-api; the other side's Services come from its Releases).
func collisions(app, dest string, built []string) error {
	root := filepath.Join(release.Root, "apps")
	apps, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	project := release.ProjectName(app, dest)
	for _, a := range apps {
		dests, err := os.ReadDir(filepath.Join(root, a.Name()))
		if err != nil {
			continue
		}
		for _, d := range dests {
			if (a.Name() == app && d.Name() == dest) || ValidAppDest(a.Name(), d.Name()) != nil {
				continue
			}
			other := a.Name() + "/" + d.Name()
			if release.ProjectName(a.Name(), d.Name()) == project {
				return denied(fmt.Sprintf("%s/%s and %s share the compose project %s, so their containers would mix", app, dest, other, project),
					"the operator renames one of them")
			}
			theirs := builtNames(a.Name(), d.Name())
			for _, svc := range built {
				if name := strings.ToLower(app + "-" + dest + "-" + svc); theirs[name] {
					return denied(fmt.Sprintf("the image Yoho builds for Service %s of %s/%s has the same name as one %s builds (%s)", svc, app, dest, other, name),
						"rename the Service, or the operator renames one of the Apps")
				}
			}
		}
	}
	return nil
}

// builtNames are the image names (<app>-<dest>-<service>, lowercased)
// of the Services an App Destination's Releases built.
func builtNames(app, dest string) map[string]bool {
	out := map[string]bool{}
	files, _ := filepath.Glob(filepath.Join(release.AppDir(app, dest), "releases", "*", "release.json"))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var rel struct {
			Built []string `json:"built"`
		}
		if json.Unmarshal(b, &rel) != nil {
			continue
		}
		for _, svc := range rel.Built {
			out[strings.ToLower(app+"-"+dest+"-"+svc)] = true
		}
	}
	return out
}

// ownerLabels is "<yoho.app>|<yoho.destination>" of an object.
func ownerLabels(app, dest string) string { return app + "|" + dest }

// imageInfo is what checkImages needs of a local image ("" when absent).
func (c *call) imageInfo(ctx context.Context, ref string) (exists bool, owner string, err error) {
	out, err := c.host.Output(ctx, remote.Cmd{Script: "docker image inspect --format " +
		remote.Quote(`{{index .Config.Labels "`+deploy.LabelApp+`"}}|{{index .Config.Labels "`+deploy.LabelDestination+`"}}|`) +
		" " + remote.Quote(ref) + " 2>/dev/null || true"})
	if err != nil || strings.TrimSpace(out) == "" {
		return false, "", err
	}
	a, rest, _ := strings.Cut(out, "|")
	d, _, _ := strings.Cut(rest, "|")
	if a != "" || d != "" {
		owner = ownerLabels(a, d)
	}
	return true, owner, nil
}

// checkImages refuses image tags on the Server that belong to another App
// Destination (labels yoho.app/yoho.destination): a built tag another
// build already holds (loading would retag it), and any other image the
// project names.
func (c *call) checkImages(ctx context.Context, pol Policy, p *types.Project) error {
	refs := []string{}
	for _, svc := range sortedKeys(pol.Built) {
		refs = append(refs, pol.Built[svc])
	}
	for _, img := range containerImages(p) {
		if !pol.built(img) {
			refs = append(refs, img)
		}
	}
	seen := map[string]bool{}
	for _, ref := range refs {
		if seen[ref] {
			continue
		}
		seen[ref] = true
		if err := c.checkOwner(ctx, ref); err != nil {
			return err
		}
	}
	return nil
}

// checkOwner refuses ref when the local image carries another App
// Destination's labels.
func (c *call) checkOwner(ctx context.Context, ref string) error {
	exists, owner, err := c.imageInfo(ctx, ref)
	if err != nil {
		return err
	}
	if own := ownerLabels(c.req.App, c.req.Destination); exists && owner != "" && owner != own {
		return denied(fmt.Sprintf("image %s on the Server belongs to %s, not %s/%s", ref, strings.ReplaceAll(owner, "|", "/"), c.req.App, c.req.Destination),
			"the operator renames one of the Apps")
	}
	return nil
}

// pullImages puts every image the project runs, other than the built ones,
// on the Server before compose runs (Harden sets pull_policy: never). Refs
// under an operator-allowed registry (limits.registries) are pulled with
// the Server's `docker login` when missing; all others are pulled
// anonymously every time, with an empty Docker config, so the Server's
// credentials never fetch a private image for a Member, and a local image
// (another App's build, or one named by bare ID) is used only if anyone
// could pull it.
func (c *call) pullImages(ctx context.Context, pol Policy, p *types.Project) error {
	seen := map[string]bool{}
	for _, img := range containerImages(p) {
		if seen[img] || pol.built(img) {
			continue
		}
		seen[img] = true
		if slices.ContainsFunc(pol.Limits.Registries, func(r string) bool { return HasRepoPrefix(img, r) }) {
			exists, _, err := c.imageInfo(ctx, img)
			if err != nil {
				return err
			}
			if !exists {
				fmt.Fprintf(c.fw, "pulling %s\n", img)
				if err := c.host.Run(ctx, remote.Cmd{Script: "docker pull -q " + remote.Quote(img) + " >/dev/null", Stderr: c.fw}); err != nil {
					return fmt.Errorf("pull %s: %w", img, err)
				}
			}
		} else {
			fmt.Fprintf(c.fw, "pulling %s (anonymously)\n", img)
			if err := c.host.Run(ctx, remote.Cmd{Script: anonymousPull(img), Stderr: io.Discard}); err != nil {
				return &remote.GateError{
					Message: "image " + img + " cannot be pulled anonymously",
					Hint:    "use a public image, build the Service, or ask the operator to allow its registry with limits.registries",
					Code:    "denied",
				}
			}
		}
		if err := c.checkOwner(ctx, img); err != nil {
			return err
		}
	}
	return nil
}

// anonymousPull pulls ref with an empty Docker client config: no registry
// credentials.
func anonymousPull(ref string) string {
	return "set -eu\nd=$(mktemp -d)\ntrap 'rm -rf \"$d\"' EXIT\nDOCKER_CONFIG=\"$d\" docker pull -q " + remote.Quote(ref) + " >/dev/null\n"
}

// containerImages lists the images of every Service and pre_start
// container.
func containerImages(p *types.Project) []string {
	var out []string
	for _, name := range sortedKeys(p.Services) {
		s := p.Services[name]
		if s.Image != "" {
			out = append(out, s.Image)
		}
		for _, h := range s.PreStart {
			if h.Image != "" {
				out = append(out, h.Image)
			}
		}
	}
	return out
}

// checkVolumes refuses named volumes and networks that already exist but
// are not this compose project's plain local ones: a volume another App
// created (or one with driver options, which can bind any Server path)
// would otherwise be mounted under this App's name.
func (c *call) checkVolumes(ctx context.Context, p *types.Project) error {
	project := c.project()
	for _, k := range sortedKeys(p.Volumes) {
		v := p.Volumes[k]
		name := v.Name
		if bool(v.External) || name == "" {
			continue
		}
		out, err := c.host.Output(ctx, remote.Cmd{Script: "docker volume inspect --format " +
			remote.Quote(`{{.Driver}}|{{index .Labels "com.docker.compose.project"}}|{{len .Options}}`) + " " + remote.Quote(name) + " 2>/dev/null || true"})
		if err != nil {
			return err
		}
		if out == "" {
			continue
		}
		if f := strings.Split(out, "|"); len(f) != 3 || f[0] != "local" || f[1] != project || f[2] != "0" {
			return denied(fmt.Sprintf("volume %s already exists and is not a plain volume of %s (driver|project|options: %s)", name, project, out),
				"use another volume name, or ask the operator to remove the volume")
		}
	}
	for _, k := range sortedKeys(p.Networks) {
		n := p.Networks[k]
		if bool(n.External) || n.Name == "" {
			continue
		}
		out, err := c.host.Output(ctx, remote.Cmd{Script: "docker network inspect --format " +
			remote.Quote(`{{index .Labels "com.docker.compose.project"}}`) + " " + remote.Quote(n.Name) + " 2>/dev/null || true"})
		if err != nil {
			return err
		}
		if out != "" && out != project {
			return denied(fmt.Sprintf("network %s already exists and belongs to %s, not %s", n.Name, out, project), "use another network name")
		}
	}
	return nil
}

// ownContainers lists the containers of the compose project and refuses
// when any belongs to another App Destination (a project name collision).
// Each entry is "<id> <service> <state>".
func (c *call) ownContainers(ctx context.Context) ([][3]string, error) {
	out, err := c.host.Output(ctx, remote.Cmd{Script: "docker ps -a --no-trunc --filter " + remote.Quote("label=com.docker.compose.project="+c.project()) +
		" --format " + remote.Quote(`{{.ID}}|{{.Label "`+deploy.LabelApp+`"}}|{{.Label "`+deploy.LabelDestination+`"}}|{{.Label "com.docker.compose.service"}}|{{.State}}`)})
	if err != nil {
		return nil, err
	}
	own := ownerLabels(c.req.App, c.req.Destination)
	var res [][3]string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(l), "|")
		if len(f) != 5 {
			continue
		}
		if ownerLabels(f[1], f[2]) != own {
			return nil, denied(fmt.Sprintf("compose project %s has containers of %s/%s, not only of %s/%s", c.project(), f[1], f[2], c.req.App, c.req.Destination),
				"the operator renames one of the Apps")
		}
		res = append(res, [3]string{f[0], f[3], f[4]})
	}
	return res, nil
}

// reservedPrefixes are the image repository prefixes of Yoho builds on
// this Server: the local "yoho/" and the Registry's.
func reservedPrefixes(s *Settings) []string {
	out := []string{"yoho/"}
	reg := normalizeHost(strings.TrimSuffix(build.ImageName(s.Registry, "a", "", "x", "v"), "a-x:v"))
	if reg != "yoho/" {
		out = append(out, reg)
	}
	return out
}
