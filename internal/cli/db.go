package cli

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/types"
	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/pgpromote"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/ui"
)

func init() { extraCommands = append(extraCommands, dbCmd) }

type promoteOptions struct {
	From           string
	OldPrimaryDown bool
	User           string
	MaxLag         string
	Timeout        time.Duration
	Version        string
	SkipBuild      bool
	Plan           bool
	AutoApprove    bool
}

const defaultMaxLag = "16MiB"

func dbCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "db", Short: "Database operations (promote a Postgres standby)"}
	var o promoteOptions
	promote := &cobra.Command{
		Use:   "promote [CONNECTION]",
		Short: "Promote a Postgres standby after you point the Connection at it",
		Long: `Promote a streaming Postgres standby to primary, for a planned switchover
or after the primary's Server is lost. It is config first: edit the
Connection in the Yoho file so its service is the standby and the old primary
is one of its consumers, then run this command. It reads the old primary from
the current Release, and then:

  1. checks, read-only, that the standby streams and lags at most --max-lag
  2. shows the plan and asks for confirmation (--plan stops here)
  3. fences the old primary with ` + "`yoho app stop`" + ` (it stays stopped)
  4. waits until the standby replayed everything the old primary wrote
  5. runs pg_promote() on the standby
  6. applies the Yoho file, which moves the Connection and its consumers

Every step is journaled on the new primary's Server; rerun the command to
resume after a failure. Volumes are never deleted and the Yoho file is never
rewritten. Postgres only, Roles Destinations only, no automatic failover
(ADR 0003, ADR 0021).

When the old primary's Server is lost, power it off in your provider's
console, remove it from the Destination, and pass --old-primary-down: the
standby is promoted at the position it received, and writes it did not
receive are lost.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			conn := ""
			if len(args) == 1 {
				conn = args[0]
			}
			return a.dbPromote(cmd.Context(), conn, o)
		},
	}
	promote.Flags().StringVar(&o.From, "from", "", "Old primary Service, when the current Release already names the new one")
	promote.Flags().BoolVar(&o.OldPrimaryDown, "old-primary-down", false, "The old primary's Server is powered off: promote without fencing it")
	promote.Flags().StringVar(&o.User, "user", "", "Postgres role for psql over the local socket (default: POSTGRES_USER, else postgres)")
	promote.Flags().StringVar(&o.MaxLag, "max-lag", defaultMaxLag, "Refuse when the standby's replay lags more than this many bytes (KiB, MiB, GiB suffixes)")
	promote.Flags().DurationVar(&o.Timeout, "timeout", 5*time.Minute, "How long to wait for the standby to replay the old primary's WAL")
	promote.Flags().StringVar(&o.Version, "version", "", "Version to apply after promoting (default: git SHA)")
	promote.Flags().BoolVar(&o.SkipBuild, "skip-build", false, "Apply with the images already on the Servers")
	promote.Flags().BoolVar(&o.Plan, "plan", false, "Show the promotion and apply plan without changing Servers")
	promote.Flags().BoolVar(&o.AutoApprove, "auto-approve", false, "Approve the displayed promotion")
	addLockWaitFlag(promote)
	c.AddCommand(promote)
	return c
}

// parseBytes reads 16777216, 16MiB, 512KiB or 1GiB.
func parseBytes(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	mult := uint64(1)
	for _, u := range []struct {
		suffix string
		n      uint64
	}{{"KiB", 1 << 10}, {"MiB", 1 << 20}, {"GiB", 1 << 30}, {"B", 1}} {
		if rest, ok := strings.CutSuffix(s, u.suffix); ok {
			s, mult = strings.TrimSpace(rest), u.n
			break
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q (use a number with KiB, MiB or GiB)", s)
	}
	return n * mult, nil
}

// promoteTarget is the promotion derived from the Release and the config.
type promoteTarget struct {
	Connection         string
	From, To           string
	FromServer         string
	ToServer           string
	Done               bool // the journal says promoted and the Release matches: resume at apply
	FromJournal, Moved bool
	Unclean            bool // the journal records an unclean fence
}

// derivePromotion finds which Connection's provider the operator moved:
// its provider in the current Releases (rels by Server) differs from the
// config. from names the old primary when the Releases already match.
func derivePromotion(dest config.Destination, rels map[string]*release.Release, journals map[string]*pgpromote.Journal, conn, from string) (promoteTarget, error) {
	var t promoteTarget
	if len(dest.Connections) == 0 {
		return t, fmt.Errorf("Destination has no Connections")
	}
	candidates := sortedKeys(dest.Connections)
	if conn != "" {
		if _, ok := dest.Connections[conn]; !ok {
			return t, fmt.Errorf("unknown Connection %s (Connections: %s)", conn, strings.Join(candidates, ", "))
		}
		candidates = []string{conn}
	}
	differs := map[string]string{}
	for _, n := range candidates {
		want := dest.Connections[n].Service
		seen := map[string]bool{}
		for _, server := range sortedKeys(rels) {
			rel := rels[server]
			if rel == nil {
				continue
			}
			if c, ok := rel.Connections[n]; ok && c.Service != want {
				seen[c.Service] = true
			}
		}
		if len(seen) > 1 {
			return t, fmt.Errorf("Servers' Releases disagree on the provider of Connection %s (%s); converge with `yoho apply` first", n, strings.Join(sortedKeys(seen), ", "))
		}
		for s := range seen {
			differs[n] = s
		}
	}
	name := conn
	switch {
	case len(differs) > 1:
		return t, fmt.Errorf("several Connections changed provider (%s); name the one to promote", strings.Join(sortedKeys(differs), ", "))
	case len(differs) == 1:
		for n := range differs {
			name = n
		}
		t.Moved = true
		if from != "" && from != differs[name] {
			return t, fmt.Errorf("--from %s, but the current Release's provider of Connection %s is %s", from, name, differs[name])
		}
		from = differs[name]
	default:
		if name == "" && len(candidates) == 1 {
			name = candidates[0]
		}
		if from == "" {
			var resumable []string
			for _, n := range candidates {
				if j := journals[n]; j != nil && j.To == dest.Connections[n].Service {
					resumable = append(resumable, n)
				}
			}
			if len(resumable) == 1 {
				name = resumable[0]
				j := journals[name]
				from, t.FromJournal = j.From, true
				t.Done = j.Promoted()
			}
		}
		if from == "" {
			label := name
			if label == "" {
				label = "<name>"
			}
			return t, fmt.Errorf("no Connection changed its provider\nhint: edit `connections.%s.service` to the standby and list the old primary in `consumers`, then rerun", label)
		}
		if name == "" {
			return t, fmt.Errorf("--from needs the CONNECTION to promote (Connections: %s)", strings.Join(candidates, ", "))
		}
	}
	t.Connection, t.From, t.To = name, from, dest.Connections[name].Service
	if t.From == t.To {
		return t, fmt.Errorf("Connection %s already names %s; --from must name the old primary", name, t.To)
	}
	to := dest.ServiceServers(t.To)
	if len(to) != 1 {
		return t, fmt.Errorf("new primary %s must run on exactly one Server, not %d", t.To, len(to))
	}
	t.ToServer = to[0]
	for _, server := range sortedKeys(rels) {
		if rel := rels[server]; rel != nil && rel.Roles != nil {
			if hs := (config.Destination{Roles: rel.Roles}).ServiceServers(t.From); len(hs) == 1 {
				t.FromServer = hs[0]
				break
			}
		}
	}
	if t.FromServer == "" {
		if hs := dest.ServiceServers(t.From); len(hs) == 1 {
			t.FromServer = hs[0]
		}
	}
	// Only when neither the Release nor the config places the old primary
	// any more (lost Server, apply failed after recording the new Release)
	// does a promoted journal of this very promotion name its Server. It
	// never overrides a placement the current state still records.
	if j := journals[name]; t.FromServer == "" && j != nil && j.Promoted() && j.Connection == name && j.From == t.From && j.To == t.To && j.FromServer != "" {
		t.FromServer = j.FromServer
	}
	if t.FromServer == "" {
		return t, fmt.Errorf("cannot tell which Server runs the old primary %s", t.From)
	}
	return t, nil
}

// postgresUser is --user, else POSTGRES_USER as the Service on server sees
// it: its per-Server x-yoho.tags environment, its Compose environment, else
// postgres. An x-yoho.secrets entry named POSTGRES_USER is never read here:
// secret names how psql resolves it inside the container
// (pgpromote.UserFromEnv or UserFromFile), so it is never in argv.
func postgresUser(flag string, svc types.ServiceConfig, ext config.ServiceExt, server string, srv config.Server) (user, secret string) {
	if flag != "" {
		return flag, ""
	}
	if v := config.ServerEnv(ext, server, srv)["POSTGRES_USER"]; v != "" {
		return v, ""
	}
	if v, ok := svc.Environment["POSTGRES_USER"]; ok && v != nil && *v != "" {
		return *v, ""
	}
	for _, ref := range ext.Secrets {
		if ref.Name == "POSTGRES_USER" {
			if ext.SecretsAsEnv {
				return "", pgpromote.UserFromEnv
			}
			return "", pgpromote.UserFromFile
		}
	}
	return "postgres", ""
}

// serviceVolumeNames are the Docker names of svc's named volumes.
func serviceVolumeNames(p *types.Project, project, service string) []string {
	var out []string
	for _, v := range p.Services[service].Volumes {
		if v.Type != types.VolumeTypeVolume || v.Source == "" {
			continue
		}
		name := project + "_" + v.Source
		if cfg, ok := p.Volumes[v.Source]; ok && cfg.Name != "" {
			name = cfg.Name
		}
		out = append(out, name)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (a *app) dbPromote(ctx context.Context, conn string, o promoteOptions) (err error) {
	applying := false
	defer func() {
		if err != nil && !applying {
			a.ui.Finished(classifyMutation(err, !preMutation(ctx)), "")
		}
	}()
	switch {
	case a.dest.Roles == nil:
		return &validationError{fmt.Errorf("db promote requires a Roles Destination with a Connection")}
	case a.dest.Runtime == "swarm":
		return &validationError{fmt.Errorf("db promote is not available on runtime swarm")}
	case a.scoped:
		return &validationError{fmt.Errorf("db promote is not available with scoped access; ask an Operator")}
	}
	maxLag, err := parseBytes(o.MaxLag)
	if err != nil {
		return &validationError{err}
	}
	ver, err := a.resolveDeployVersion(o.Version)
	if err != nil {
		return err
	}
	a.ui.Title("Promoting a standby of %s on %s", a.cfg.App, a.destName)
	s, err := a.openSession(ctx, ver, true, false)
	if err != nil {
		return err
	}
	defer s.close()
	hosts := map[string]plan.NamedHost{}
	rels := map[string]*release.Release{}
	for _, h := range s.hosts {
		hosts[h.Name] = h
		rel, e := deploy.CurrentRelease(ctx, h.Host, a.cfg.App, a.destName)
		if e != nil {
			return e
		}
		rels[h.Name] = rel
	}
	journals := map[string]*pgpromote.Journal{}
	for _, n := range sortedKeys(a.dest.Connections) {
		hs := a.dest.ServiceServers(a.dest.Connections[n].Service)
		if len(hs) != 1 || hosts[hs[0]].Host == nil {
			continue
		}
		j, e := pgpromote.ReadJournal(ctx, hosts[hs[0]].Host, a.cfg.App, a.destName, n)
		if e != nil {
			return e
		}
		journals[n] = j
	}
	t, err := derivePromotion(a.dest, rels, journals, conn, o.From)
	if err != nil {
		return &validationError{err}
	}
	if j := journals[t.Connection]; j != nil && j.From == t.From && j.To == t.To && j.Steps[pgpromote.StepFence] == "unclean" {
		t.Unclean = true
	}
	if err = a.checkPromotion(ctx, s, t, o); err != nil {
		return err
	}
	project := release.ProjectName(a.cfg.App, a.destName)
	newUser, newSecret := postgresUser(o.User, s.r.Project.Services[t.To], s.r.Ext[t.To], t.ToServer, a.cfg.Servers[t.ToServer])
	oldUser, oldSecret := postgresUser(o.User, s.r.Project.Services[t.From], s.r.Ext[t.From], t.FromServer, a.cfg.Servers[t.FromServer])
	popts := pgpromote.Options{
		App: a.cfg.App, Destination: a.destName, Connection: t.Connection,
		New:            pgpromote.Node{Server: t.ToServer, Service: t.To, Project: project, User: newUser, UserSecret: newSecret, Host: hosts[t.ToServer].Host},
		Old:            pgpromote.Node{Server: t.FromServer, Service: t.From, Project: project, User: oldUser, UserSecret: oldSecret},
		OldPrimaryDown: o.OldPrimaryDown,
		MaxLag:         maxLag,
		Timeout:        o.Timeout,
		Performer:      performer(),
	}
	// A connected old Server is always verified, even with
	// --old-primary-down (allowed there only after an unclean fence).
	if h, ok := hosts[t.FromServer]; ok {
		popts.Old.Host = h.Host
	}
	step := a.ui.Step(t.ToServer, "Check %s replicates %s", t.To, t.From)
	st, err := pgpromote.Preflight(ctx, popts)
	if err != nil {
		step.Fail(err, preflightHint(err))
		return &silentError{err}
	}
	switch {
	case st.Promoted:
		step.Done(t.To + " is already primary; resuming at apply")
	case st.Fenced:
		step.Done(t.From + " is already stopped")
	case o.OldPrimaryDown:
		step.Done("old primary down; not fenced")
	default:
		step.Done(fmt.Sprintf("streaming, %d bytes behind", st.Lag))
	}

	fence := plan.Change{Kind: "service", Name: t.From, Server: t.FromServer, Action: plan.ActionReplace, Downtime: true, Reasons: []string{"fence: stop (`yoho app stop`); it stays stopped until `yoho app start " + t.From + "`"}}
	switch {
	case o.OldPrimaryDown:
		fence.Action, fence.Downtime, fence.Reasons = plan.ActionNoop, false, []string{"old primary is down (--old-primary-down): not fenced"}
	case st.Fenced:
		fence.Action, fence.Downtime, fence.Reasons = plan.ActionNoop, false, []string{"already stopped"}
	}
	promote := plan.Change{Kind: "service", Name: t.To, Server: t.ToServer, Action: plan.ActionUpdate, Reasons: []string{"pg_promote()"}}
	if st.Promoted {
		promote.Action, promote.Reasons = plan.ActionNoop, []string{"already primary"}
	}
	rest, _, err := a.collectChanges(ctx, s, false)
	if err != nil {
		return err
	}
	a.ui.Plan(append([]plan.Change{fence, promote}, rest...))
	if o.Plan {
		return nil
	}
	if err = a.confirmApply(applyInput, isTTY(), o.AutoApprove); err != nil {
		return err
	}
	if err = a.runPromotion(ctx, s, t, popts); err != nil {
		return err
	}
	s.close()

	applying = true
	if err = a.apply(ctx, applyOptions{Version: ver, SkipBuild: o.SkipBuild, AutoApprove: true}); err != nil {
		a.ui.Warn("%s is primary and %s is stopped; rerun `yoho db promote %s` (it resumes at apply)", t.To, t.From, t.Connection)
		return err
	}
	// The journal is only a retry marker: once applied, a later switchover
	// with the same Services starts fresh.
	if e := a.deletePromotionJournal(ctx, t); e != nil {
		a.ui.Warn("could not delete the promotion journal on %s (%v); delete %s by hand", t.ToServer, e, pgpromote.JournalPath(a.cfg.App, a.destName, t.Connection))
	}
	a.promoteNextSteps(s, t, project)
	return nil
}

// deletePromotionJournal removes the journal after the follow-up apply.
func (a *app) deletePromotionJournal(ctx context.Context, t promoteTarget) error {
	srv := a.cfg.Servers[t.ToServer]
	h, err := connectServer(t.ToServer, srv.SSH, srv.Sudo, a.sshOptions())
	if err != nil {
		return err
	}
	defer h.Close()
	return pgpromote.DeleteJournal(ctx, h, a.cfg.App, a.destName, t.Connection)
}

// preflightHint names the socket and pg_hba only for psql failures.
func preflightHint(err error) string {
	var pe *pgpromote.PsqlError
	if errors.As(err, &pe) {
		return "promote uses the container's local socket; pg_hba needs a `local` rule for the role, or pass `--user`"
	}
	return ""
}

// checkPromotion refuses, before any change, a promotion that could leave
// two primaries or one Yoho cannot fence.
func (a *app) checkPromotion(ctx context.Context, s *session, t promoteTarget, o promoteOptions) error {
	conn := a.dest.Connections[t.Connection]
	if _, ok := s.r.Project.Services[t.To]; !ok {
		return &validationError{fmt.Errorf("new primary %s is not a Service in compose", t.To)}
	}
	if !s.r.Ext[t.To].Stateful {
		return &validationError{fmt.Errorf("new primary %s must be marked x-yoho.stateful", t.To)}
	}
	// Only an old primary still placed in the Destination restarts and
	// must come back as a standby; a lost Server's is gone from compose.
	if _, ok := s.r.Project.Services[t.From]; ok && len(a.dest.ServiceServers(t.From)) > 0 {
		if !s.r.Ext[t.From].Stateful {
			return &validationError{fmt.Errorf("old primary %s must be marked x-yoho.stateful", t.From)}
		}
		if !slices.Contains(conn.Consumers, t.From) {
			return &validationError{fmt.Errorf("old primary %s must be a consumer of Connection %s, so it restarts as a standby, never as a second primary\nhint: add %s to connections.%s.consumers", t.From, t.Connection, t.From, t.Connection)}
		}
	}
	inDest := slices.Contains(a.dest.Servers, t.FromServer)
	if !o.OldPrimaryDown {
		if !inDest {
			return &validationError{fmt.Errorf("Server %s of the old primary %s is not in the Destination, so it cannot be fenced\nhint: if Server %s is lost, power it off in your provider's console and confirm it stays off (detaching only its external network is not enough), then rerun with --old-primary-down", t.FromServer, t.From, t.FromServer)}
		}
		return nil
	}
	if !inDest {
		return nil
	}
	// --old-primary-down needs the old Server out of the Destination; the
	// one exception is accepting the loss after an unclean fence, when
	// nothing may run.
	if !t.Unclean {
		return &validationError{fmt.Errorf("--old-primary-down, but Server %s of the old primary %s is still in the Destination\nhint: if Server %s is lost, power it off and remove it from `servers` and the Destination (see \"Server a is lost\" in the Postgres replication guide); otherwise drop --old-primary-down so promote fences %s", t.FromServer, t.From, t.FromServer, t.From)}
	}
	for _, h := range s.hosts {
		if h.Name != t.FromServer {
			continue
		}
		ids, e := deploy.RunningContainers(ctx, h.Host, a.cfg.App, a.destName, t.From)
		if e != nil {
			return e
		}
		if len(ids) > 0 {
			return &validationError{fmt.Errorf("Server %s is reachable and %s is running; --old-primary-down would leave two primaries\nhint: drop --old-primary-down so promote fences %s", t.FromServer, t.From, t.From)}
		}
	}
	return nil
}

// runPromotion fences, waits and promotes under every Server's lock.
func (a *app) runPromotion(ctx context.Context, s *session, t promoteTarget, popts pgpromote.Options) (err error) {
	unlock, err := a.operationLocks(ctx, s.hosts, deploy.LockInfo{Command: "db promote"}, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unlock(context.WithoutCancel(ctx))) }()
	if err = beginOperation(ctx); err != nil {
		return err
	}
	popts.OperationID = operationID(ctx)
	steps := map[string]*ui.Step{}
	popts.Progress = func(name, state string, j *pgpromote.Journal) {
		fields := map[string]any{"connection": t.Connection, "from": t.From, "to": t.To, "step": name, "state": state}
		if j.TargetLSN != "" {
			fields["target_lsn"] = j.TargetLSN
		}
		a.ui.Event("db_promote", "", fields)
		a.progress(ctx, t.ToServer+"/promote/"+t.Connection+"/"+name, state)
		server := t.ToServer
		if name == pgpromote.StepFence {
			server = t.FromServer
		}
		switch state {
		case "running":
			steps[name] = a.ui.Step(server, "%s", promoteStepTitle(name, t))
		case "complete", "skipped":
			if st := steps[name]; st != nil {
				st.Done(state)
				delete(steps, name)
			}
		case "failed", "unclean":
			if st := steps[name]; st != nil {
				st.Fail(errors.New(name+" "+state), "")
				delete(steps, name)
			}
		}
	}
	if err = pgpromote.Run(ctx, popts); err != nil {
		return err
	}
	return nil
}

func promoteStepTitle(step string, t promoteTarget) string {
	switch step {
	case pgpromote.StepFence:
		return "Fence " + t.From
	case pgpromote.StepWait:
		return "Wait for " + t.To + " to replay " + t.From + "'s WAL"
	case pgpromote.StepPromote:
		return "Promote " + t.To
	}
	return step
}

func (a *app) promoteNextSteps(s *session, t promoteTarget, project string) {
	a.ui.Info("%s is the primary of Connection %s. %s stays stopped on Server %s.", t.To, t.Connection, t.From, t.FromServer)
	if _, ok := s.r.Project.Services[t.From]; ok {
		if vols := serviceVolumeNames(s.r.Project, project, t.From); len(vols) > 0 {
			a.ui.Info("To rebuild it as a standby, remove its old data on Server %s by hand (Yoho never deletes volumes):\n  docker volume rm %s\nthen run `yoho app start %s`; it clones %s.", t.FromServer, strings.Join(vols, " "), t.From, t.To)
		} else {
			a.ui.Info("To rebuild it as a standby, remove its old data on Server %s by hand, then run `yoho app start %s`.", t.FromServer, t.From)
		}
	}
	a.ui.Info("Move the Backup job of %s to %s.", t.From, t.To)
}
