package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
)

// hintFor returns the failure hint for err: ownership collisions get their
// own, everything else the given default.
func hintFor(err error, def string) string {
	var conflict *proxy.RouteConflictError
	if errors.As(err, &conflict) {
		return proxy.RouteConflictHint
	}
	var oe *release.OwnershipError
	if errors.As(err, &oe) {
		return "rename the App or Destination (names join App and Destination with '-', so another App's resources collide)"
	}
	var me *deploy.MigrationPendingError
	if errors.As(err, &me) {
		return "rerun `yoho deploy` or `yoho apply` to finish the app_dir migration"
	}
	var ae *deploy.AppDirError
	if errors.As(err, &ae) && ae.Hint != "" {
		return ae.Hint
	}
	var lk *deploy.LockedError
	if errors.As(err, &lk) {
		return lockHint(lk)
	}
	var ms *deploy.MissingSecretsError
	if errors.As(err, &ms) {
		return deploy.MissingSecretsHint
	}
	var pr *deploy.PendingRotationError
	if errors.As(err, &pr) {
		return deploy.PendingRotationHint
	}
	var le *deploy.LogDriverError
	if errors.As(err, &le) {
		return deploy.LogDriverHint
	}
	var sc *deploy.SubnetChangeError
	if errors.As(err, &sc) {
		return subnetChangeHint(sc)
	}
	var df *deploy.Failure
	if errors.As(err, &df) && df.Network != nil {
		return poolHint(df.Network)
	}
	// The Gate knows why it refused; its hint beats the generic one.
	var ge *remote.GateError
	if errors.As(err, &ge) && ge.Hint != "" {
		return ge.Hint
	}
	return def
}

// lockHint is the next step when the Destination lock (or gate) is held:
// nothing was changed. Wait for the holder, or remove a stale lock left by
// a killed run, as the error message says.
func lockHint(le *deploy.LockedError) string {
	if le.Gate() {
		return "nothing was changed: another Yoho command holds the gate of this Destination; wait for it to finish and rerun. If none is running (a run was killed), remove " + le.Dir + " on the Server, then rerun"
	}
	if le.Manual() {
		return "nothing was changed: the Destination is locked by `yoho lock acquire`; ask the holder, run `yoho lock release` when it may proceed, or pass --lock-wait to wait for it"
	}
	h := "nothing was changed: another Yoho command holds the lock of this Destination; wait for it to finish and rerun (or pass --lock-wait). If none is running (a run was killed), the lock is stale: run `yoho lock release --force`, then rerun"
	if le.Holder.Command == "secrets rotate" {
		h += "; an interrupted `yoho secrets rotate --generated KEY` is finished by running it again"
	}
	return h
}

// poolHint explains Docker address pool exhaustion with the subnet found free
// on the Server. Only the compose default network has a Yoho setting; the
// shared Proxy network and named networks get their own remedy.
func poolHint(n *deploy.NetworkHint) string {
	free := "a free subnet is " + n.SuggestedSubnet
	switch {
	case n.Default:
		return "Docker has no free address pool; add networks.default.ipam.config[].subnet: " + n.SuggestedSubnet +
			" to compose, or set destinations.<name>.subnet\n" + deploy.SnippetFor(n.SuggestedSubnet)
	case n.Network == proxy.Network:
		return "Docker has no free address pool for the shared " + proxy.Network + " network; create it yourself on the Server, then rerun: docker network create --subnet " +
			n.SuggestedSubnet + " " + proxy.Network
	case n.Network != "":
		return "Docker has no free address pool for network " + n.Network + "; add networks." + n.Key + ".ipam.config[].subnet: " + n.SuggestedSubnet +
			" to compose (destinations.<name>.subnet covers only the default network)\n" + deploy.SnippetFor(n.SuggestedSubnet, n.Key)
	}
	return "Docker has no free address pool; " + free + ". Pin it on the network that failed with ipam.config[].subnet, or free up unused Docker networks"
}

// secretsHint maps a failed secrets resolution to its own hint; def is the
// caller's text for failures Yoho cannot place.
func secretsHint(err error, def string) string {
	var pe *secrets.ProviderError
	if !errors.As(err, &pe) {
		return def
	}
	switch pe.Class {
	case secrets.ApprovalTimeout:
		h := "the " + pe.Provider + " approval prompt was not answered in time; approve it when it appears and rerun (or pass --secrets-retries 1)"
		if pe.Inferred {
			h += " (inferred from the long wait: the provider gave no reason, so this may be another failure)"
		}
		if pe.Provider == "op" {
			h += "; to be asked less often see https://yohobuild.com/secrets#1password-approvals"
		}
		return h
	case secrets.NotSignedIn:
		return "sign in to " + pe.Provider + " (`op signin`, `bw unlock`, or set BWS_ACCESS_TOKEN) and rerun"
	case secrets.NotFound:
		return "check the ref in secrets.values or .yoho/secrets: " + pe.Provider + " could not find that item or field"
	}
	return def
}

// rolloutHint is hintFor for a failed rollout. A Proxy route conflict with
// another Destination of this App over a hostname the base compose files
// list gets its own: Destination overlays append to lists, so an overlay's
// x-yoho.proxy.hosts keeps the base file's hosts unless it says !override.
func (a *app) rolloutHint(ctx context.Context, err error, def string) string {
	h := hintFor(err, def)
	var c *proxy.RouteConflictError
	if !errors.As(err, &c) || c.Host == "" || !strings.HasPrefix(c.Existing, a.cfg.App+"-") || strings.HasPrefix(c.Existing, a.cfg.App+"-"+a.destName+"-") {
		return h
	}
	base, lerr := composefile.Load(ctx, a.dir, a.cfg.Compose, a.dest.Env, a.project(), "")
	if lerr != nil {
		return h
	}
	names := make([]string, 0, len(base.Ext))
	for n := range base.Ext {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if pr := base.Ext[n].Proxy; pr != nil && slices.Contains(pr.Hosts, c.Host) {
			return fmt.Sprintf("hostname %s comes from the base compose file (Service %s), and Destination overlays append to lists rather than replace them: in the %s overlay (e.g. compose.%s.yaml) write `hosts: !override [...]` under services.%s.x-yoho.proxy, then deploy again",
				c.Host, n, a.destName, a.destName, n)
		}
	}
	return h
}

func subnetChangeHint(e *deploy.SubnetChangeError) string {
	return "restore destinations.<name>.subnet (or the compose ipam) to " + strconv.Quote(e.Old) +
		", or stop the App on the Server with `docker compose -p " + e.Project + " down` (volumes are kept), then deploy again so the network is created with the new subnet"
}
