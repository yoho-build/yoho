package gate

import (
	"context"
	"fmt"
	"io"
	"slices"

	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/secrets"
)

// PlanArgs are what a deploy of Version would send, without images: the
// Member's resolved compose project and the Services built for it.
type PlanArgs struct {
	Version string   `json:"version"`
	Compose string   `json:"compose"`
	Built   []string `json:"built,omitempty"`
}

// composeDiff is deploy.Compose.Diff; tests replace it to watch what plan
// hands the runtime.
var composeDiff = func(ctx context.Context, d *plan.Deploy) ([]plan.Change, error) {
	return deploy.Compose{}.Diff(ctx, d)
}

// planVerb shows what a deploy through this Gate would change. It vets and
// hardens the compose exactly as deployVerb does, so the plan compares
// like with like (issue #65), then asks the runtime for a diff. It is
// read-only: no pull, no image load, no lock, no rollout, no Release or
// vetted record. Only the changes go back, never the compose or secrets.
func planVerb(ctx context.Context, c *call) (any, error) {
	var a PlanArgs
	if err := c.args(&a); err != nil {
		return nil, err
	}
	c.audit["version"], c.audit["built"] = a.Version, a.Built
	c.audit["compose_sha256"] = deploy.SHA256Hex([]byte(a.Compose))
	if !versionRe.MatchString(a.Version) {
		return nil, invalid(fmt.Sprintf("invalid version %q", a.Version))
	}
	if err := checkServices(a.Built); err != nil {
		return nil, err
	}
	s, err := c.settings()
	if err != nil {
		return nil, err
	}
	if err := collisions(c.req.App, c.req.Destination, a.Built); err != nil {
		return nil, err
	}
	r, pol, err := c.vet(ctx, s, []byte(a.Compose), a.Version, a.Built)
	if err != nil {
		return nil, err
	}
	pol.Harden(r.Project)
	if err := c.checkImages(ctx, pol, r.Project); err != nil {
		return nil, err
	}
	if err := c.checkVolumes(ctx, r.Project); err != nil {
		return nil, err
	}
	svcSecrets, gen, err := storedSecrets(c.req.App, c.req.Destination, r.Ext)
	if err != nil {
		return nil, err
	}
	c.audit["secrets_generation"] = gen
	var values []string
	for _, m := range svcSecrets {
		for _, v := range m {
			values = append(values, v)
		}
	}
	d := &plan.Deploy{
		App: c.req.App, Destination: c.req.Destination, Version: a.Version, Performer: c.performer(""),
		Servers: []plan.NamedHost{c.namedHost(s)}, Project: r.Project, Ext: r.Ext, Built: slices.Sorted(slices.Values(a.Built)),
		ServiceSecrets: svcSecrets, Registry: s.Registry, Proxy: s.Proxy, RetainReleases: s.RetainReleases, Out: io.Discard,
		Scoped: c.scoped(nil),
	}
	changes, err := composeDiff(ctx, d)
	if err != nil {
		return nil, secrets.RedactError(err, values...)
	}
	if changes == nil {
		changes = []plan.Change{}
	}
	return changes, nil
}
