package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

// LabelOneOff marks a container started by `yoho app run`.
const LabelOneOff = "yoho.oneoff"

// CurrentService is what `yoho app run` needs to know about a Service of
// the current Release.
type CurrentService struct {
	Name     string
	Stateful bool
	Proxied  bool
}

// ErrNoCurrentRelease means the Destination has no current Release.
var ErrNoCurrentRelease = errors.New("no current Release")

// CurrentServices reads the current Release's Version and Services (its
// plan.json) on h.
func CurrentServices(ctx context.Context, h remote.Host, app, dest string) (string, []CurrentService, error) {
	dir := release.AppDir(app, dest)
	out, err := h.Output(ctx, remote.Cmd{Script: "readlink " + remote.Quote(path.Join(dir, "current")) + " 2>/dev/null || true"})
	if err != nil {
		return "", nil, fmt.Errorf("read current Release: %w", err)
	}
	ver := path.Base(strings.TrimSpace(out))
	if ver == "" || ver == "." || ver == "/" || !versionRe.MatchString(ver) {
		return "", nil, ErrNoCurrentRelease
	}
	b, err := h.ReadFile(ctx, path.Join(release.Dir(app, dest, ver), "plan.json"), false)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, fmt.Errorf("Release %s has no plan.json; deploy again first", ver)
		}
		return "", nil, err
	}
	var plans []servicePlan
	if err := json.Unmarshal(b, &plans); err != nil {
		return "", nil, fmt.Errorf("parse plan.json of Release %s: %w", ver, err)
	}
	svcs := make([]CurrentService, 0, len(plans))
	for _, sp := range plans {
		svcs = append(svcs, CurrentService{Name: sp.Name, Stateful: sp.Stateful, Proxied: sp.Proxy != nil})
	}
	return ver, svcs, nil
}

// OneOffScript runs command in a new, removed-on-exit container of service
// from the compiled compose of Release version: `docker compose run --rm
// --no-deps`, so the Service's image, secrets files, env, networks and
// volumes apply, no ports are published (no --service-ports), no
// dependency starts and the Proxy never routes to it. tty requests a
// pseudo-terminal (no -T). Compose labels it as a one-off
// (com.docker.compose.oneoff), which cutover and orphan removal skip.
func OneOffScript(app, dest, version, service string, tty bool, command []string) string {
	dir := release.AppDir(app, dest)
	cc := composeCLI{project: release.ProjectName(app, dest), dir: dir, file: path.Join(release.Dir(app, dest, version), "compose.yaml")}
	args := []string{"run", "--rm", "--no-deps", "--label", LabelOneOff + "=run"}
	if !tty {
		args = append(args, "-T")
	}
	args = append(append(args, "--", service), command...)
	return cc.cmd(args...)
}
