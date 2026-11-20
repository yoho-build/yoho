package cli

import (
	"context"
	"fmt"
	"os/exec"
	"path"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/audit"
	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/plan"
	"github.com/yoho-build/yoho/internal/proxy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/roles"
	"github.com/yoho-build/yoho/internal/schedule"
	"github.com/yoho-build/yoho/internal/setup"
	"github.com/yoho-build/yoho/internal/transport"
)

func init() {
	extraCommands = append(extraCommands, doctorCmd)
}

const (
	severityError   = "error"
	severityWarning = "warning"
	severityInfo    = "info"

	// drillStaleAge is how old a successful restore drill may be before doctor warns.
	drillStaleAge = 90 * 24 * time.Hour

	gib = 1024 * 1024 * 1024
	mib = 1024 * 1024
)

type doctorFinding struct {
	Check    string
	Severity string
	Subject  string
	Message  string
	Hint     string
}

type doctorOptions struct {
	validationErr error
	maxAge        time.Duration
	connect       func(ctx context.Context, a *app) ([]plan.NamedHost, func(), error)
	localExec     transport.Exec
	now           func() time.Time
}

func doctorCmd(g *globals) *cobra.Command {
	var maxAge string
	c := &cobra.Command{
		Use:   "doctor",
		Short: "Check whether a Destination is production-safe (read-only)",
		Long: `Run a read-only health check for a Destination across config, Backups,
restore drills, firewall, exposed ports, disk space, Proxy, pending secret
rotations and image transport.

It never resolves secrets from a password manager and never changes the
Server, so it is safe to run from CI or a Scheduled Job.

Each finding has a severity (error, warning, or info) and a one-line fix
hint. With --json, findings are emitted as NDJSON "finding" events followed
by a final "doctor" summary event.

Exit codes: 0 no errors (warnings and info still exit 0), 1 one or more
error findings (or a load/connection failure).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var age time.Duration
			var err error
			if cmd.Flags().Changed("max-age") {
				age, err = time.ParseDuration(maxAge)
				if err != nil || age <= 0 {
					err = fmt.Errorf("--max-age must be a positive duration (e.g. 48h)")
				}
			}
			return runDoctor(cmd, g, doctorOptions{maxAge: age, validationErr: err})
		},
	}
	c.Flags().StringVar(&maxAge, "max-age", "", "Override the Backup freshness threshold for every Scheduled Job (e.g. 48h; default: derived from each job's schedule)")
	return c
}

func runDoctor(cmd *cobra.Command, g *globals, opts doctorOptions) error {
	a, err := g.load(cmd)
	if err != nil {
		u := g.ui(cmd)
		u.DoctorFinding("config", severityError, "", err.Error(), "check yoho.yml and pass -d <destination> if needed")
		u.DoctorSummary(false, 1, 0, 0, g.destination)
		return &exitError{1, &silentError{err}}
	}
	if opts.validationErr != nil {
		return emitDoctorResults(a, []doctorFinding{{Check: "config", Severity: severityError, Message: opts.validationErr.Error(), Hint: "pass --max-age with a positive duration (e.g. 48h)"}})
	}
	ctx := cmd.Context()
	if opts.now == nil {
		opts.now = time.Now
	}
	connectFn := opts.connect
	if connectFn == nil {
		connectFn = func(ctx context.Context, a *app) ([]plan.NamedHost, func(), error) {
			return a.connect(ctx)
		}
	}

	a.ui.Title("Doctor %s (%s)", a.cfg.App, a.destName)

	var findings []doctorFinding

	// 1. Offline config validity & Services with no Backup (Stateful Services first).
	r, cfgFindings := checkDoctorConfig(ctx, a)
	findings = append(findings, cfgFindings...)

	// Connect to the Destination's Servers for live checks.
	hosts, closeHosts, err := connectFn(ctx, a)
	if err != nil {
		findings = append(findings, doctorFinding{
			Check:    "connect",
			Severity: severityError,
			Subject:  a.destName,
			Message:  fmt.Sprintf("could not connect to Server(s): %v", err),
			Hint:     "verify SSH access and Docker on the Destination's Servers (`yoho setup`)",
		})
		return emitDoctorResults(a, findings)
	}
	defer closeHosts()

	needsProxy := appNeedsProxy(a, r)
	hasStateful := appHasStateful(r)
	pusshOK := transport.LocalPusshAvailable(ctx, opts.localExec)

	// 2. Backup freshness and last successful restore drill (first Server).
	if a.dest.Roles != nil {
		for _, h := range hosts {
			b, e := a.rolesJobsApp(ctx, hosts, h)
			if e != nil {
				findings = append(findings, doctorFinding{Check: "roles", Severity: severityError, Subject: h.Name, Message: e.Error(), Hint: "configure Backup jobs per owning Server"})
				continue
			}
			stateful := false
			if r != nil {
				for svc, ext := range r.Ext {
					stateful = stateful || ext.Stateful && slices.Contains(a.dest.ServiceServers(svc), h.Name)
				}
			}
			findings = append(findings, checkDoctorBackups(ctx, b, h, opts.maxAge, stateful, opts.now())...)
			findings = append(findings, checkDoctorPendingRotations(ctx, a, h)...)
		}
	} else if len(hosts) > 0 {
		h0 := hosts[0]
		findings = append(findings, checkDoctorBackups(ctx, a, h0, opts.maxAge, hasStateful, opts.now())...)
		findings = append(findings, checkDoctorPendingRotations(ctx, a, h0)...)
	}

	// 3–7. Per-Server checks: firewall & public ports, disk, Proxy, transport.
	for _, h := range hosts {
		if a.dest.Roles != nil {
			addresses := privateAddresses(hosts)
			rules, e := roles.FirewallRules(a.cfg.App, a.destName, h.Name, a.dest, addresses)
			if e == nil {
				e = roles.CheckFirewall(ctx, h.Host, rules)
			}
			if e == nil {
				e = roles.CheckBindings(ctx, h.Host, a.cfg.App, a.destName, h.Name, a.dest, addresses)
			}
			if e != nil {
				findings = append(findings, doctorFinding{Check: "roles", Severity: severityError, Subject: h.Name, Message: e.Error(), Hint: "run yoho apply to reconcile private port bindings and Docker firewall rules"})
			}
		}
		findings = append(findings, checkDoctorFirewall(ctx, a, h)...)
		findings = append(findings, checkDoctorPublishedPorts(ctx, a, h)...)
		findings = append(findings, checkDoctorDisk(ctx, a, h)...)
		if r == nil {
			findings = append(findings, doctorFinding{Check: "proxy", Severity: severityWarning, Subject: h.Name, Message: "Proxy check skipped because compose config failed", Hint: "fix config and rerun `yoho doctor`"})
		} else {
			findings = append(findings, checkDoctorProxy(ctx, a, h, needsProxy && a.serverHasProxy(h.Name, r.Ext))...)
		}
		findings = append(findings, checkDoctorTransport(ctx, a, h, pusshOK)...)
	}

	return emitDoctorResults(a, findings)
}

func emitDoctorResults(a *app, findings []doctorFinding) error {
	var errCount, warnCount, infoCount int
	for _, f := range findings {
		switch f.Severity {
		case severityError:
			errCount++
		case severityWarning:
			warnCount++
		default:
			infoCount++
		}
		a.ui.DoctorFinding(f.Check, f.Severity, f.Subject, f.Message, f.Hint)
	}
	ok := errCount == 0
	a.ui.DoctorSummary(ok, errCount, warnCount, infoCount, a.destName)
	if !ok {
		return &exitError{1, &silentError{fmt.Errorf("doctor found %d error(s)", errCount)}}
	}
	return nil
}

// checkDoctorConfig validates compose and Yoho config offline and reports
// Services with no Backup (Stateful Services first).
func checkDoctorConfig(ctx context.Context, a *app) (*composefile.Result, []doctorFinding) {
	var out []doctorFinding
	r, err := a.compose(ctx)
	if err != nil {
		out = append(out, doctorFinding{
			Check:    "config",
			Severity: severityError,
			Message:  err.Error(),
			Hint:     "fix the compose file and run `yoho config check`",
		})
		return nil, out
	}
	keys, err := a.secretKeys(r)
	if err != nil {
		out = append(out, doctorFinding{
			Check:    "config",
			Severity: severityError,
			Message:  err.Error(),
			Hint:     "check .yoho/secrets and secrets.values, then run `yoho config check`",
		})
	}

	cmd := &cobra.Command{Use: "doctor"}
	cmd.SetContext(ctx)
	raw := a.offlineConfigFindings(cmd, r, keys)
	if len(a.cfg.Secrets.Nonempty) > 0 {
		raw = append(raw, composefile.Finding{Level: composefile.LevelWarning, Message: "secrets.nonempty not verified (--offline)"})
	}
	var backupFindings, otherFindings []doctorFinding
	for _, f := range raw {
		sev := severityWarning
		hint := "review the warning and run `yoho config check`"
		if f.Level == composefile.LevelError {
			sev = severityError
			hint = "fix the error and run `yoho config check`"
		}
		msg := f.Message
		if before, after, ok := strings.Cut(msg, ". Hint: "); ok {
			msg = before
			hint = after
		}
		checkName := "config"
		if strings.Contains(msg, "no Backup job on Destination") {
			checkName = "backup"
		}
		df := doctorFinding{
			Check:    checkName,
			Severity: sev,
			Subject:  f.Service,
			Message:  msg,
			Hint:     hint,
		}
		if checkName == "backup" {
			backupFindings = append(backupFindings, df)
		} else {
			otherFindings = append(otherFindings, df)
		}
	}

	// Non-Stateful Services with x-yoho.backup configured but no covering Backup
	// job on this Destination come after Stateful Services.
	for _, name := range r.Project.ServiceNames() {
		ext := r.Ext[name]
		if ext.Stateful || ext.Backup == nil {
			continue
		}
		if !serviceHasBackupJob(a.destName, name, true, a.cfg.Backups) {
			backupFindings = append(backupFindings, doctorFinding{
				Check:    "backup",
				Severity: severityWarning,
				Subject:  name,
				Message:  fmt.Sprintf("Service %s declares x-yoho.backup but no Backup job covers it on Destination %s", name, a.destName),
				Hint:     fmt.Sprintf("add a backups.jobs entry for Destination %s in %s, then run `yoho schedule install -d %s`", a.destName, path.Base(a.path), a.destName),
			})
		}
	}

	// Non-Stateful, non-proxied Services publishing ports on 0.0.0.0/:: in compose.
	// (Stateful Services' public ports are already warned by composefile.Check.)
	for _, name := range r.Project.ServiceNames() {
		ext := r.Ext[name]
		if ext.Stateful || ext.Proxy != nil {
			continue
		}
		svc, err := r.Project.GetService(name)
		if err != nil {
			continue
		}
		pub := composefile.PublicPorts(svc)
		exposure := "on 0.0.0.0/:: outside the Proxy (bypasses ufw)"
		hint := "bind private ports to 127.0.0.1, or route HTTP traffic through x-yoho.proxy"
		if a.dest.Runtime == "swarm" {
			pub = composefile.PublishedPorts(svc)
			exposure = "on every node and interface via the Swarm ingress mesh (bypasses ufw)"
			hint = "remove published ports and use the stack network, or route HTTP traffic through x-yoho.proxy"
		}
		if len(pub) > 0 {
			otherFindings = append(otherFindings, doctorFinding{
				Check:    "ports",
				Severity: severityWarning,
				Subject:  name,
				Message:  fmt.Sprintf("Service %s publishes port(s) %s %s", name, strings.Join(pub, ", "), exposure),
				Hint:     hint,
			})
		}
	}

	// Report errors/warnings from config first, then backup coverage (Stateful first).
	out = append(out, otherFindings...)
	out = append(out, backupFindings...)
	if len(out) == 0 {
		out = append(out, doctorFinding{
			Check:    "config",
			Severity: severityInfo,
			Message:  fmt.Sprintf("config valid (%d service(s), %d secret key(s), runtime %s)", len(r.Project.Services), len(keys), a.dest.Runtime),
		})
	}
	return r, out
}

func serviceHasBackupJob(destName, svc string, hasExtBackup bool, b config.BackupsConfig) bool {
	for _, j := range b.Jobs {
		if j.Destination != destName {
			continue
		}
		if (len(j.Services) == 0 && hasExtBackup) || slices.Contains(j.Services, svc) {
			return true
		}
	}
	return false
}

func appNeedsProxy(a *app, r *composefile.Result) bool {
	if a.proxyConfig().Tunnel != nil {
		return true
	}
	if r == nil {
		return false
	}
	for _, ext := range r.Ext {
		if ext.Proxy != nil {
			return true
		}
	}
	return false
}

func appHasStateful(r *composefile.Result) bool {
	if r == nil {
		return false
	}
	for _, ext := range r.Ext {
		if ext.Stateful {
			return true
		}
	}
	return false
}

// checkDoctorBackups checks Scheduled Job Backup freshness against each job's
// schedule (or maxAge override) and reports the date of the last successful
// restore drill from the audit log.
func checkDoctorBackups(ctx context.Context, a *app, h plan.NamedHost, maxAge time.Duration, hasStateful bool, now time.Time) []doctorFinding {
	var out []doctorFinding

	// Collect configured Backup jobs for this Destination.
	var destJobs, scheduledJobs []string
	for _, name := range sortedKeys(a.cfg.Backups.Jobs) {
		j := a.cfg.Backups.Jobs[name]
		if j.Destination != a.destName {
			continue
		}
		destJobs = append(destJobs, name)
		if j.Schedule != "" {
			scheduledJobs = append(scheduledJobs, name)
		} else {
			out = append(out, doctorFinding{
				Check:    "backup",
				Severity: severityWarning,
				Subject:  name,
				Message:  fmt.Sprintf("Backup job %s has no schedule; Backups run only by hand", name),
				Hint:     fmt.Sprintf("set backups.jobs.%s.schedule and run `yoho schedule install -d %s`", name, a.destName),
			})
		}
	}

	sts, err := schedule.Status(ctx, h.Host, a.cfg.App, a.destName, a.scheduleMode())
	if err != nil {
		out = append(out, doctorFinding{
			Check:    "backup",
			Severity: severityWarning,
			Subject:  h.Name,
			Message:  fmt.Sprintf("could not read Scheduled Jobs: %v", err),
			Hint:     fmt.Sprintf("run `yoho schedule status -d %s` to inspect timers", a.destName),
		})
	} else {
		installed := map[string]bool{}
		for _, s := range sts {
			installed[s.Job] = true
		}
		for _, name := range scheduledJobs {
			if !installed[name] {
				out = append(out, doctorFinding{
					Check:    "backup",
					Severity: severityError,
					Subject:  name,
					Message:  fmt.Sprintf("Backup job %s (%s) is not installed as a Scheduled Job on %s", name, a.cfg.Backups.Jobs[name].Schedule, h.Name),
					Hint:     fmt.Sprintf("run `yoho schedule install -d %s`", a.destName),
				})
			}
		}

		for _, s := range sts {
			if s.TimerState == "" {
				out = append(out, doctorFinding{Check: "backup", Severity: severityWarning, Subject: s.Job, Message: "timer state unreadable", Hint: fmt.Sprintf("inspect `yoho schedule status -d %s` and systemctl on the Server", a.destName)})
			} else if s.TimerState != "active" {
				out = append(out, doctorFinding{
					Check:    "backup",
					Severity: severityError,
					Subject:  s.Job,
					Message:  fmt.Sprintf("Scheduled Job %s timer is %s (not active)", s.Job, s.TimerState),
					Hint:     fmt.Sprintf("run `yoho schedule install -d %s` to re-enable the timer", a.destName),
				})
			}
		}

		if len(sts) > 0 {
			clockOut, cerr := h.Host.Output(ctx, remote.Cmd{Script: "set -eu\ndate -u +%s.%N\n"})
			var serverNow time.Time
			if cerr == nil {
				serverNow, cerr = parseScheduleServerClock(clockOut)
			}
			if cerr != nil {
				out = append(out, doctorFinding{
					Check:    "backup",
					Severity: severityError,
					Subject:  h.Name,
					Message:  fmt.Sprintf("could not read Server clock for Backup freshness check: %v", cerr),
					Hint:     "check GNU date on the Server and rerun `yoho schedule status --max-age 48h`",
				})
			} else {
				for _, s := range sts {
					window := maxAge
					if window <= 0 {
						var unavailable bool
						var ageErr error
						window, unavailable, ageErr = scheduleMaxAge(ctx, h.Host, s.Schedule)
						if ageErr != nil {
							out = append(out, doctorFinding{Check: "backup", Severity: severityError, Subject: s.Job, Message: ageErr.Error(), Hint: "check the schedule with systemd-analyze calendar on the Server, or pass --max-age"})
							continue
						}
						if unavailable {
							out = append(out, doctorFinding{Check: "backup", Severity: severityInfo, Subject: s.Job, Message: "systemd-analyze unavailable on Server; using conservative 8d Backup freshness threshold", Hint: "install systemd-analyze or pass --max-age"})
						}
					}
					health := schedule.SuccessHealth(s.State, serverNow, window)
					switch health {
					case "healthy":
						age := serverNow.Sub(*s.State.LastSuccess)
						id := s.State.BackupID
						if id != "" {
							id = " (" + id + ")"
						}
						out = append(out, doctorFinding{
							Check:    "backup",
							Severity: severityInfo,
							Subject:  s.Job,
							Message:  fmt.Sprintf("newest successful Backup%s is %s old (within %s for schedule %q)", id, formatAge(age), formatAge(window), s.Schedule),
						})
					case "stale":
						age := serverNow.Sub(*s.State.LastSuccess)
						out = append(out, doctorFinding{
							Check:    "backup",
							Severity: severityError,
							Subject:  s.Job,
							Message:  fmt.Sprintf("newest successful Backup is %s old, older than %s for schedule %q", formatAge(age), formatAge(window), s.Schedule),
							Hint:     fmt.Sprintf("inspect `yoho schedule status -d %s` and run `yoho backup run %s -d %s`", a.destName, s.Job, a.destName),
						})
					case "never-success":
						out = append(out, doctorFinding{
							Check:    "backup",
							Severity: severityError,
							Subject:  s.Job,
							Message:  fmt.Sprintf("Scheduled Job %s has no successful Backup yet (schedule %q)", s.Job, s.Schedule),
							Hint:     fmt.Sprintf("run `yoho backup run %s -d %s` and inspect `yoho schedule status -d %s`", s.Job, a.destName, a.destName),
						})
					case "future-success":
						out = append(out, doctorFinding{
							Check:    "backup",
							Severity: severityError,
							Subject:  s.Job,
							Message:  fmt.Sprintf("Scheduled Job %s last_success timestamp is in the future relative to the Server clock", s.Job),
							Hint:     "check NTP / Server clock and schedule-state.json",
						})
					}
					if s.State.Error != "" {
						out = append(out, doctorFinding{
							Check:    "backup",
							Severity: severityWarning,
							Subject:  s.Job,
							Message:  fmt.Sprintf("Scheduled Job %s last run failed: %s", s.Job, s.State.Error),
							Hint:     fmt.Sprintf("inspect `yoho schedule status -d %s` and rerun `yoho backup run %s -d %s`", a.destName, s.Job, a.destName),
						})
					}
				}
			}
		}
	}

	// Last successful restore drill from the audit log.
	if hasStateful || len(destJobs) > 0 || len(sts) > 0 {
		out = append(out, checkDoctorDrill(ctx, a, h, now))
	}
	return out
}

func checkDoctorDrill(ctx context.Context, a *app, h plan.NamedHost, now time.Time) doctorFinding {
	authority, done, err := a.authority(ctx, h)
	if err != nil {
		return doctorFinding{Check: "drill", Severity: severityWarning, Subject: h.Name, Message: err.Error()}
	}
	defer done()
	entries, err := audit.ReadCommand(ctx, authority, a.cfg.App, a.destName, "backup drill")
	if err != nil {
		return doctorFinding{
			Check:    "drill",
			Severity: severityWarning,
			Subject:  h.Name,
			Message:  fmt.Sprintf("could not read audit log for last restore drill: %v", err),
			Hint:     fmt.Sprintf("run `yoho backup drill -d %s` to prove Backups restore", a.destName),
		}
	}
	var last time.Time
	for _, r := range audit.Pair(entries) {
		first := r.First()
		if dest, ok := a.cfg.Destinations[a.destName]; ok && dest.Roles != nil {
			job := ""
			if args, ok := first.Args["args"].([]any); ok && len(args) > 0 {
				job, _ = args[0].(string)
			}
			if job != "" {
				if _, ok := a.cfg.Backups.Jobs[job]; !ok {
					continue
				}
			} else if len(a.cfg.Backups.Jobs) == 0 {
				continue
			}
		}
		if first.Command != "backup drill" || r.Finished == nil {
			continue
		}
		if cleanup, _ := first.Args["cleanup"].(bool); cleanup {
			continue
		}
		if r.Finished.Exit == nil || *r.Finished.Exit != 0 {
			continue
		}
		if r.Finished.Time.After(last) {
			last = r.Finished.Time
		}
	}
	if last.IsZero() {
		return doctorFinding{
			Check:    "drill",
			Severity: severityWarning,
			Subject:  a.destName,
			Message:  "no successful restore drill recorded",
			Hint:     fmt.Sprintf("run `yoho backup drill -d %s` to prove Backups restore", a.destName),
		}
	}
	age := now.Sub(last)
	if age < 0 {
		age = 0
	}
	date := last.UTC().Format("2006-01-02")
	if age > drillStaleAge {
		return doctorFinding{
			Check:    "drill",
			Severity: severityWarning,
			Subject:  a.destName,
			Message:  fmt.Sprintf("last successful restore drill was on %s (%s ago)", date, formatAge(age)),
			Hint:     fmt.Sprintf("run `yoho backup drill -d %s` to verify Backups still restore", a.destName),
		}
	}
	return doctorFinding{
		Check:    "drill",
		Severity: severityInfo,
		Subject:  a.destName,
		Message:  fmt.Sprintf("last successful restore drill: %s (%s ago)", date, formatAge(age)),
	}
}

// scheduleMaxAge asks the Server's systemd to interpret its own calendar syntax.
// Only a missing systemd-analyze permits the conservative fallback.
func scheduleMaxAge(ctx context.Context, h remote.Host, onCalendar string) (time.Duration, bool, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: "set -eu\nLC_ALL=C; export LC_ALL\nif ! command -v systemd-analyze >/dev/null 2>&1; then echo yoho-systemd-analyze-unavailable; exit 0; fi\nsystemd-analyze calendar --iterations=2 " + remote.Quote(onCalendar) + "\n"})
	if err != nil {
		return 0, false, fmt.Errorf("could not evaluate Backup schedule %q: %w", onCalendar, err)
	}
	if strings.TrimSpace(out) == "yoho-systemd-analyze-unavailable" {
		return 8 * 24 * time.Hour, true, nil
	}
	var elapses []time.Time
	for _, line := range strings.Split(out, "\n") {
		label, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && label == "(in UTC)" && len(elapses) > 0 {
			t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", strings.TrimSpace(value))
			if err != nil {
				return 0, false, fmt.Errorf("could not parse systemd UTC elapse: %w", err)
			}
			elapses[len(elapses)-1] = t
			continue
		}
		if !ok || (label != "Next elapse" && !strings.HasPrefix(label, "Iter. #")) {
			continue
		}
		t, err := time.Parse("Mon 2006-01-02 15:04:05 MST", strings.TrimSpace(value))
		if err != nil {
			return 0, false, fmt.Errorf("could not parse systemd calendar elapse: %w", err)
		}
		elapses = append(elapses, t)
	}
	if len(elapses) != 2 || !elapses[1].After(elapses[0]) {
		return 0, false, fmt.Errorf("systemd calendar returned no two increasing elapses for %q", onCalendar)
	}
	return 2*elapses[1].Sub(elapses[0]) + time.Hour, false, nil
}

// checkDoctorFirewall checks ufw state using setup.CheckFirewall.
func checkDoctorFirewall(ctx context.Context, a *app, h plan.NamedHost) []doctorFinding {
	if a.cfg.Setup.Firewall != nil && !*a.cfg.Setup.Firewall {
		return []doctorFinding{{
			Check:    "firewall",
			Severity: severityInfo,
			Subject:  h.Name,
			Message:  "firewall check skipped (setup.firewall: false)",
		}}
	}
	ports := a.setupProxyPorts()
	if a.dest.Roles != nil {
		r, e := a.compose(ctx)
		if e != nil {
			return []doctorFinding{{Check: "firewall", Severity: severityError, Subject: h.Name, Message: e.Error()}}
		}
		if !a.serverHasProxy(h.Name, r.Ext) {
			ports = nil
		}
	}
	needed, detail, err := setup.CheckFirewall(ctx, h.Host, h.Server.Sudo, ports, a.cfg.Setup.AllowPorts)
	if err != nil {
		return []doctorFinding{{
			Check:    "firewall",
			Severity: severityWarning,
			Subject:  h.Name,
			Message:  fmt.Sprintf("firewall check failed: %v", err),
			Hint:     fmt.Sprintf("check ufw on %s or run `yoho setup --plan -d %s`", h.Name, a.destName),
		}}
	}
	if needed {
		return []doctorFinding{{
			Check:    "firewall",
			Severity: severityWarning,
			Subject:  h.Name,
			Message:  "firewall: " + detail,
			Hint:     fmt.Sprintf("run `yoho setup -d %s` to configure ufw", a.destName),
		}}
	}
	return []doctorFinding{{
		Check:    "firewall",
		Severity: severityInfo,
		Subject:  h.Name,
		Message:  "firewall: " + detail,
	}}
}

// checkDoctorPublishedPorts inspects running containers of this App on h for
// ports published on 0.0.0.0 or :: outside the Proxy.
func checkDoctorPublishedPorts(ctx context.Context, a *app, h plan.NamedHost) []doctorFinding {
	q := remote.Quote
	proj := a.project()
	script := "set -eu\n" +
		"{ docker ps --filter " + q("label=com.docker.compose.project="+proj) + " --format '{{.Names}}\t{{.Ports}}'\n" +
		"  docker ps --filter " + q("label=com.docker.stack.namespace="+proj) + " --format '{{.Names}}\t{{.Ports}}'\n" +
		"} | sort -u\n"
	if a.dest.Runtime == "swarm" {
		script = "set -eu\ndocker service ls --filter " + q("label=com.docker.stack.namespace="+proj) + " --format '{{.Name}}\t{{.Ports}}'\n"
	}
	out, err := h.Host.Output(ctx, remote.Cmd{Script: script})
	if err != nil {
		return []doctorFinding{{
			Check:    "ports",
			Severity: severityWarning,
			Subject:  h.Name,
			Message:  fmt.Sprintf("could not inspect published ports: %v", err),
			Hint:     fmt.Sprintf("run `yoho app ps -d %s` to inspect containers", a.destName),
		}}
	}
	var findings []doctorFinding
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name, ports, ok := strings.Cut(line, "\t")
		if !ok || name == proxy.ContainerName || strings.TrimSpace(ports) == "" {
			continue
		}
		pub := parseWildcardPublishedPorts(ports)
		hint := "remove published ports or bind to 127.0.0.1, or route HTTP traffic through x-yoho.proxy"
		exposure := "on 0.0.0.0/:: outside the Proxy (bypasses ufw)"
		if a.dest.Runtime == "swarm" {
			pub = nil
			for _, p := range strings.Split(ports, ",") {
				if strings.Contains(p, "->") {
					pub = append(pub, strings.TrimSpace(p))
				}
			}
			exposure = "on every node and interface via the Swarm ingress mesh (bypasses ufw)"
			hint = "remove published ports and use the stack network, or route HTTP traffic through x-yoho.proxy"
		}
		if len(pub) > 0 {
			findings = append(findings, doctorFinding{
				Check:    "ports",
				Severity: severityWarning,
				Subject:  h.Name + "/" + name,
				Message:  fmt.Sprintf("Service %s publishes %s %s", name, strings.Join(pub, ", "), exposure),
				Hint:     hint,
			})
		}
	}
	return findings
}

// parseWildcardPublishedPorts extracts published port mappings bound to
// 0.0.0.0 or :: from `docker ps --format '{{.Ports}}'`.
func parseWildcardPublishedPorts(ports string) []string {
	var out []string
	for _, part := range strings.Split(ports, ",") {
		p := strings.TrimSpace(part)
		if strings.HasPrefix(p, "0.0.0.0:") || strings.HasPrefix(p, ":::") || strings.HasPrefix(p, "[::]:") {
			out = append(out, p)
		}
	}
	return out
}

type doctorDisk struct {
	Label string
	Path  string
	Mount string
	Total uint64
	Used  uint64
	Avail uint64
}

// checkDoctorDisk checks free space on the Docker data root and Backup cache.
func checkDoctorDisk(ctx context.Context, a *app, h plan.NamedHost) []doctorFinding {
	backupCache := path.Join(release.BackupDir(a.cfg.App, a.destName), ".restic-cache")
	script := `set -eu
LC_ALL=C; export LC_ALL
disk() {
  d=$2
  while [ ! -e "$d" ] && [ "$d" != / ]; do d=$(dirname "$d"); done
  df -Pk "$d" 2>/dev/null | awk -v l="$1" -v p="$2" 'NR==2 {print l "\t" p "\t" $6 "\t" $2 "\t" $3 "\t" $4}' || :
}
dr=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || echo /var/lib/docker)
[ -n "$dr" ] || dr=/var/lib/docker
disk docker "$dr"
disk backup-cache ` + remote.Quote(backupCache) + `
`
	out, err := h.Host.Output(ctx, remote.Cmd{Script: script})
	if err != nil {
		return []doctorFinding{{
			Check:    "disk",
			Severity: severityWarning,
			Subject:  h.Name,
			Message:  fmt.Sprintf("could not check disk usage: %v", err),
			Hint:     "check `df -h` on " + h.Name,
		}}
	}

	var disks []doctorDisk
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(strings.TrimSpace(line), "\t")
		if len(f) != 6 {
			continue
		}
		total, _ := strconv.ParseUint(strings.TrimSpace(f[3]), 10, 64)
		used, _ := strconv.ParseUint(strings.TrimSpace(f[4]), 10, 64)
		avail, _ := strconv.ParseUint(strings.TrimSpace(f[5]), 10, 64)
		if total == 0 {
			continue
		}
		d := doctorDisk{
			Label: f[0], Path: f[1], Mount: f[2],
			Total: total * 1024, Used: used * 1024, Avail: avail * 1024,
		}
		merged := false
		for i := range disks {
			if disks[i].Mount == d.Mount {
				disks[i].Label += " & " + d.Label
				merged = true
				break
			}
		}
		if !merged {
			disks = append(disks, d)
		}
	}

	if len(disks) == 0 {
		return []doctorFinding{{
			Check:    "disk",
			Severity: severityWarning,
			Subject:  h.Name,
			Message:  "could not determine free disk on Docker data root or Backup cache",
			Hint:     "check `df -h` on " + h.Name,
		}}
	}

	var findings []doctorFinding
	for _, d := range disks {
		usedPct := 100.0
		if d.Avail > 0 {
			usedPct = float64(d.Used) / (float64(d.Used) + float64(d.Avail)) * 100
		}
		label := diskLabel(d.Label)
		sev := severityInfo
		hint := ""
		switch {
		case d.Avail == 0 || usedPct >= 95 || (d.Total >= 2*gib && d.Avail < 1*gib):
			sev = severityError
			hint = diskHint(d.Label)
		case usedPct >= 85 || (d.Total >= 10*gib && d.Avail < 5*gib):
			sev = severityWarning
			hint = diskHint(d.Label)
		}
		findings = append(findings, doctorFinding{
			Check:    "disk",
			Severity: sev,
			Subject:  h.Name,
			Message:  fmt.Sprintf("%s (mount %s): %s free of %s (%.0f%% used)", label, d.Mount, formatBytes(d.Avail), formatBytes(d.Total), usedPct),
			Hint:     hint,
		})
	}
	return findings
}

func diskLabel(raw string) string {
	switch raw {
	case "docker":
		return "Docker data root"
	case "backup-cache":
		return "Backup cache"
	case "docker & backup-cache":
		return "Docker data root and Backup cache"
	default:
		return raw
	}
}

func diskHint(raw string) string {
	if strings.Contains(raw, "backup-cache") && !strings.Contains(raw, "docker") {
		return "prune old Backups or clear the .restic-cache directory"
	}
	return "free disk space (`docker image prune` or expand the volume)"
}

// checkDoctorProxy checks whether the Proxy is running and compares its
// version against the pinned/configured image.
func checkDoctorProxy(ctx context.Context, a *app, h plan.NamedHost, needsProxy bool) []doctorFinding {
	st, err := proxy.Status(ctx, h.Host, a.proxyConfig())
	if err != nil {
		return []doctorFinding{{
			Check:    "proxy",
			Severity: severityError,
			Subject:  h.Name,
			Message:  fmt.Sprintf("could not inspect Proxy: %v", err),
			Hint:     fmt.Sprintf("run `yoho proxy status -d %s`", a.destName),
		}}
	}
	want := proxy.VersionLabel(st.Want)
	if !st.Exists || !st.Running {
		if !needsProxy {
			if st.Exists {
				return []doctorFinding{{
					Check:    "proxy",
					Severity: severityWarning,
					Subject:  h.Name,
					Message:  fmt.Sprintf("Proxy %s exists but is stopped (no proxied Services in this App)", proxy.ContainerName),
					Hint:     fmt.Sprintf("run `yoho proxy boot -d %s` or remove the stopped container", a.destName),
				}}
			}
			return []doctorFinding{{
				Check:    "proxy",
				Severity: severityInfo,
				Subject:  h.Name,
				Message:  "Proxy not running (no proxied Services in this App)",
			}}
		}
		state := "not installed"
		if st.Exists {
			state = "stopped"
		}
		return []doctorFinding{{
			Check:    "proxy",
			Severity: severityError,
			Subject:  h.Name,
			Message:  fmt.Sprintf("Proxy %s is %s (want %s)", proxy.ContainerName, state, want),
			Hint:     fmt.Sprintf("run `yoho proxy boot -d %s`", a.destName),
		}}
	}
	if st.Image == "" {
		return []doctorFinding{{
			Check:    "proxy",
			Severity: severityWarning,
			Subject:  h.Name,
			Message:  fmt.Sprintf("Proxy %s is running with an unknown image (want %s)", proxy.ContainerName, want),
			Hint:     fmt.Sprintf("run `yoho proxy upgrade -d %s`", a.destName),
		}}
	}
	if st.Image == st.Want {
		return []doctorFinding{{
			Check:    "proxy",
			Severity: severityInfo,
			Subject:  h.Name,
			Message:  fmt.Sprintf("Proxy running %s (up to date)", proxy.VersionLabel(st.Image)),
		}}
	}
	if proxy.CompareImages(st.Image, st.Want) > 0 {
		return []doctorFinding{{
			Check:    "proxy",
			Severity: severityInfo,
			Subject:  h.Name,
			Message:  fmt.Sprintf("Proxy running %s (newer than pinned %s)", proxy.VersionLabel(st.Image), want),
			Hint:     "upgrade Yoho or set proxy.image in yoho.yml",
		}}
	}
	return []doctorFinding{{
		Check:    "proxy",
		Severity: severityWarning,
		Subject:  h.Name,
		Message:  fmt.Sprintf("Proxy running %s; pinned version is %s", proxy.VersionLabel(st.Image), want),
		Hint:     fmt.Sprintf("run `yoho proxy upgrade -d %s`", a.destName),
	}}
}

// checkDoctorPendingRotations checks for interrupted generated-secret
// rotations (generated/<KEY>.next).
func checkDoctorPendingRotations(ctx context.Context, a *app, h plan.NamedHost) []doctorFinding {
	keys, err := deploy.PendingRotations(ctx, h.Host, a.cfg.App, a.destName)
	if err != nil {
		return []doctorFinding{{
			Check:    "secrets",
			Severity: severityWarning,
			Subject:  h.Name,
			Message:  fmt.Sprintf("could not check pending secret rotations: %v", err),
			Hint:     "check the App's generated/ directory on " + h.Name,
		}}
	}
	if len(keys) == 0 {
		return []doctorFinding{{
			Check:    "secrets",
			Severity: severityInfo,
			Subject:  h.Name,
			Message:  "no pending secret-rotation generation",
		}}
	}
	flags := make([]string, len(keys))
	for i, k := range keys {
		flags[i] = "--generated " + k
	}
	return []doctorFinding{{
		Check:    "secrets",
		Severity: severityError,
		Subject:  h.Name,
		Message:  deploy.PendingRotationWarning(keys),
		Hint:     fmt.Sprintf("run `yoho secrets rotate %s -d %s` to finish the rotation before deploying", strings.Join(flags, " "), a.destName),
	}}
}

// checkDoctorTransport reports which image transport path is in use (pussh or
// full archive) and why, reusing internal/transport detection.
func checkDoctorTransport(ctx context.Context, a *app, h plan.NamedHost, pusshOK bool) []doctorFinding {
	switch a.cfg.Builder.Location {
	case "server":
		return []doctorFinding{{
			Check:    "transport",
			Severity: severityInfo,
			Subject:  h.Name,
			Message:  "transport: built on Server (no image shipping)",
		}}
	case "remote":
		msg := "transport: remote Builder (direct ship auto)"
		switch a.cfg.Builder.Direct.Mode() {
		case config.DirectOn:
			msg = "transport: remote Builder (direct ship to Server)"
		case config.DirectOff:
			msg = "transport: remote Builder (relayed through local machine)"
		}
		return []doctorFinding{{
			Check:    "transport",
			Severity: severityInfo,
			Subject:  h.Name,
			Message:  msg,
		}}
	}

	engine, _ := build.ResolveEngine(a.cfg.Builder, runtime.GOOS, runtime.GOARCH, exec.LookPath, func() error {
		return exec.CommandContext(ctx, "container", "system", "status").Run()
	})
	if err := transport.ValidateEngineMode(engine, a.cfg.Transport.Mode); err != nil {
		return []doctorFinding{{Check: "transport", Severity: severityError, Subject: h.Name, Message: err.Error(), Hint: "set transport.mode: auto or load, or builder.engine: docker"}}
	}
	if engine == build.EngineContainer {
		return []doctorFinding{{Check: "transport", Severity: severityInfo, Subject: h.Name, Message: "transport: archive load (Apple container)"}}
	}
	mode := a.cfg.Transport.Mode
	if mode == "" {
		mode = "auto"
	}
	switch mode {
	case transport.MethodRegistry:
		return []doctorFinding{{
			Check:    "transport",
			Severity: severityInfo,
			Subject:  h.Name,
			Message:  "transport: registry (configured transport.mode: registry)",
		}}
	case transport.MethodLoad:
		return []doctorFinding{{
			Check:    "transport",
			Severity: severityInfo,
			Subject:  h.Name,
			Message:  "transport: full archive (docker save | docker load; configured transport.mode: load)",
			Hint:     "set transport.mode: auto to use pussh when available",
		}}
	}

	if err := transport.PusshTarget(h.Host); err != nil {
		sev := severityInfo
		if mode == transport.MethodPussh {
			sev = severityError
		}
		return []doctorFinding{{Check: "transport", Severity: sev, Subject: h.Name, Message: "transport: full archive (docker save | docker load); pussh unavailable: " + err.Error(), Hint: "use transport.mode: auto or load for a local Server"}}
	}
	containerdOK, err := transport.ServerContainerdImageStore(ctx, h.Host)
	if err != nil {
		return []doctorFinding{{
			Check:    "transport",
			Severity: severityWarning,
			Subject:  h.Name,
			Message:  fmt.Sprintf("could not inspect Server Docker image store: %v", err),
			Hint:     "check Docker on " + h.Name,
		}}
	}

	if pusshOK && containerdOK {
		return []doctorFinding{{
			Check:    "transport",
			Severity: severityInfo,
			Subject:  h.Name,
			Message:  "transport: pussh (layer delta push; local docker pussh plugin and Server containerd image store available)",
		}}
	}

	var reasons []string
	var hints []string
	if !pusshOK {
		reasons = append(reasons, "local `docker pussh` plugin not installed")
		hints = append(hints, "install docker-pussh locally")
	}
	if !containerdOK {
		reasons = append(reasons, "Server Docker does not use the containerd image store")
		hints = append(hints, fmt.Sprintf("run `yoho setup --containerd-image-store -d %s`", a.destName))
	}
	why := strings.Join(reasons, " and ")
	hint := strings.Join(hints, " and ") + " for faster layer-only pushes"

	if mode == transport.MethodPussh {
		return []doctorFinding{{
			Check:    "transport",
			Severity: severityError,
			Subject:  h.Name,
			Message:  fmt.Sprintf("transport: transport.mode is pussh but unavailable (%s)", why),
			Hint:     strings.Join(hints, " and ") + ", or set transport.mode: auto",
		}}
	}
	return []doctorFinding{{
		Check:    "transport",
		Severity: severityInfo,
		Subject:  h.Name,
		Message:  fmt.Sprintf("transport: full archive (docker save | docker load) because %s", why),
		Hint:     hint,
	}}
}

func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

func formatBytes(b uint64) string {
	switch {
	case b >= gib:
		return fmt.Sprintf("%.1f GiB", float64(b)/float64(gib))
	case b >= mib:
		return fmt.Sprintf("%.1f MiB", float64(b)/float64(mib))
	default:
		return fmt.Sprintf("%d KiB", b/1024)
	}
}
