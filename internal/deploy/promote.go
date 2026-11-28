package deploy

import (
	"context"
	"fmt"
	"slices"

	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
)

// CurrentConnections returns the Connections the current Release on the
// first of d's Servers with a Roles Release records; nil when none.
func CurrentConnections(ctx context.Context, d *plan.Deploy) (*release.Release, error) {
	for _, h := range d.Servers {
		rel, err := CurrentRelease(ctx, h.Host, d.App, d.Destination)
		if err != nil {
			return nil, err
		}
		if rel != nil && rel.Roles != nil {
			return rel, nil
		}
	}
	return nil, nil
}

// annotateProviderSwitch points a Connection whose provider moves from one
// Stateful Service to another at `yoho db promote`: a plain apply switches
// the Connection without fencing the old primary or promoting the standby.
func annotateProviderSwitch(ctx context.Context, d *plan.Deploy, out []plan.Change) ([]plan.Change, error) {
	cur, err := CurrentConnections(ctx, d)
	if err != nil || cur == nil {
		return out, err
	}
	for _, n := range sortedKeys(d.Roles.Connections) {
		c := d.Roles.Connections[n]
		old, ok := cur.Connections[n]
		if !ok || old.Service == c.Service || !d.Ext[old.Service].Stateful || !d.Ext[c.Service].Stateful {
			continue
		}
		reason := fmt.Sprintf("connection %s: provider %s → %s; for a database switchover use `yoho db promote %s`", n, old.Service, c.Service, n)
		servers := d.Roles.ServiceServers(c.Service)
		attached := false
		for i := range out {
			ch := &out[i]
			if ch.Kind == "service" && ch.Name == c.Service && ch.Action != plan.ActionNoop && slices.Contains(servers, ch.Server) {
				ch.Reasons = append(ch.Reasons, reason)
				attached = true
			}
		}
		if attached {
			continue
		}
		for i := range out {
			ch := &out[i]
			if ch.Kind == "firewall" && slices.Contains(servers, ch.Server) {
				ch.Reasons = append(ch.Reasons, reason)
			}
		}
	}
	return out, nil
}
