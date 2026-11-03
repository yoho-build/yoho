package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/dash"
	"github.com/yoho-build/yoho/internal/gate"
	"github.com/yoho-build/yoho/internal/remote"
)

// hintError is a failure found before connecting, with the next step for
// the user.
type hintError struct{ msg, hint string }

func (e *hintError) Error() string { return e.msg }

// fail reports err (and its hint) through the UI, so --json gets an event.
func (a *app) fail(err error) error {
	hint := ""
	var he *hintError
	if errors.As(err, &he) {
		hint = he.hint
	}
	a.ui.Error(err, hint)
	return &silentError{err}
}

// serviceNames lists the Services Yoho deploys (active compose Services,
// sorted) without the strict checks of a deploy, for diagnostic commands.
func (a *app) serviceNames(ctx context.Context) ([]string, error) {
	return composefile.ServiceNames(ctx, a.dir, a.cfg.Compose, a.dest.Env, a.project(), a.destName)
}

// checkServices rejects args that are not in names.
func checkServices(names, args []string) error {
	for _, s := range args {
		if contains(names, s) {
			continue
		}
		if len(names) == 0 {
			return noServicesError()
		}
		return &hintError{
			fmt.Sprintf("unknown Service %s; Services: %s", s, strings.Join(names, ", ")),
			"use a Service name from the compose files (profile-disabled Services are not deployed)",
		}
	}
	return nil
}

func noServicesError() error {
	return &hintError{"the App has no Services Yoho deploys", "add a Service to the compose files, or check its profiles"}
}

// logsServices resolves `app logs` arguments against names, the App's
// Services. It returns the Services to show; nil means every Service
// (--all, compose only). One Service is implied; several must be named.
func logsServices(names, args []string, all, swarm bool) ([]string, error) {
	example := "web"
	if len(names) > 0 {
		example = names[0]
	}
	switch {
	case all && swarm:
		return nil, &hintError{"swarm: --all is not supported, `docker service logs` shows one Service", "name one Service, e.g. `yoho app logs " + example + "`"}
	case all && len(args) > 0:
		return nil, &hintError{"--all and SERVICE arguments are mutually exclusive", "drop --all to show only " + strings.Join(args, ", ")}
	case all:
		return nil, nil
	}
	if err := checkServices(names, args); err != nil {
		return nil, err
	}
	if swarm && len(args) > 1 {
		return nil, &hintError{"swarm: name exactly one Service, `docker service logs` shows one", "run `yoho app logs SERVICE` once per Service"}
	}
	if len(args) > 0 {
		return args, nil
	}
	switch len(names) {
	case 0:
		return nil, noServicesError()
	case 1:
		return names, nil
	}
	if swarm {
		return nil, &hintError{"the App has several Services; name one: " + strings.Join(names, ", "), "e.g. `yoho app logs " + example + "`"}
	}
	return nil, &hintError{"the App has several Services; name one or more: " + strings.Join(names, ", "), "e.g. `yoho app logs " + example + "`, or --all for every Service"}
}

// logsOptions checks the `app logs` flags. -n 0 shows no history (with -f,
// only new lines), -n -1 every line. Without -n, --since and --grep show
// every line they select, like kamal; otherwise 100 lines.
func logsOptions(o dash.LogsOptions, linesSet, swarm bool) (dash.LogsOptions, error) {
	if err := dash.CheckTime(o.Since); err != nil {
		return o, &hintError{"--since: " + err.Error(), dash.TimeHint}
	}
	if err := dash.CheckTime(o.Until); err != nil {
		return o, &hintError{"--until: " + err.Error(), dash.TimeHint}
	}
	if err := o.Check(); err != nil {
		return o, &hintError{err.Error(), "e.g. `yoho app logs web --grep ERROR -i`"}
	}
	if swarm && o.Until != "" {
		return o, &hintError{"swarm: --until is not supported, `docker service logs` has no --until", "use --since, or compose"}
	}
	if o.Lines < dash.AllLines {
		return o, &hintError{"-n must be a number of lines, or -1 for every line", "e.g. `-n 200`, `-n 0` for no history (with -f, only new lines) or `-n -1`"}
	}
	if !linesSet && (o.Since != "" || o.Grep != "") {
		o.Lines = dash.AllLines
	}
	return o, nil
}

func logsCmd(g *globals) *cobra.Command {
	var all bool
	o := dash.LogsOptions{Lines: 100}
	c := &cobra.Command{
		Use: "logs [SERVICE...]", Short: "Show logs of one or more Services",
		Long: `Show logs of one or more Services, like docker compose logs.

--since and --until take what docker takes: a duration back from now (30s,
10m, 2h) or a timestamp (2026-11-03T15:04:05Z, 2026-11-03, Unix seconds).
--grep keeps lines containing a fixed string, filtered on the Server with
grep; -i ignores case and -E makes it an extended regular expression. With
--grep (and not -f), -n counts matching lines.

-n 0 shows no history: with -f, only new lines. -n -1 shows every line.
Without -n, --since and --grep show every line they select; add -n to bound
the output, e.g. --since 10m -n 200.`,
		Example: "  yoho app logs web -f\n  yoho app logs web --since 1h --grep ERROR -i\n  yoho app logs worker --since 2026-11-03T09:00:00Z --until 2026-11-03T10:00:00Z -t\n  yoho app logs --all -n 20",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			swarm := a.dest.Runtime == "swarm"
			opts, err := logsOptions(o, cmd.Flags().Changed("lines"), swarm)
			if err != nil {
				return a.fail(err)
			}
			follow := opts.Follow
			names, derr := a.serviceNames(cmd.Context())
			if derr != nil {
				if !all && len(args) == 0 {
					return a.fail(&hintError{"cannot list the App's Services: " + derr.Error(), "name a Service, e.g. `yoho app logs web`"})
				}
				// Logs are diagnostic: a compose file Yoho cannot read must not
				// hide them, so named Services pass through unchecked.
				if len(args) > 0 {
					a.ui.Warn("cannot list the App's Services, not checking %s: %v", strings.Join(args, ", "), derr)
				}
				names = args
			}
			svcs, err := logsServices(names, args, all, swarm)
			if err != nil {
				return a.fail(err)
			}
			if a.scoped {
				args := gate.LogsArgs{ServicesArgs: gate.ServicesArgs{Services: svcs}, Lines: gateLines(opts.Lines), Follow: follow, Timestamps: opts.Timestamps, LogFilter: opts.LogFilter}
				err := a.gateCall(cmd.Context(), "logs", args, nil, cmd.OutOrStdout(), nil)
				if follow && cmd.Context().Err() != nil {
					return nil // Ctrl-C ends a follow
				}
				return err
			}
			if follow && opts.Grep != "" {
				err = a.followGrep(cmd, appFollowGrepScript(a.project(), svcs, swarm, opts))
			} else {
				err = a.remoteRun(cmd, appLogsScript(a.project(), svcs, swarm, opts))
			}
			if follow && cmd.Context().Err() != nil {
				return nil // Ctrl-C ends a follow
			}
			var ee *remote.ExitError
			if opts.Regex && errors.As(err, &ee) && ee.Code == dash.ExitBadPattern && strings.Contains(ee.Stderr, "invalid --grep pattern") {
				// grep's own message is already on stderr.
				return a.fail(&hintError{"invalid --grep pattern", badPatternHint})
			}
			return err
		},
	}
	f := c.Flags()
	f.BoolVarP(&o.Follow, "follow", "f", false, "Follow")
	f.IntVarP(&o.Lines, "lines", "n", 100, "Lines from the end (0: none, -1: all); with --grep, matching lines")
	f.BoolVar(&all, "all", false, "Every Service, interleaved (compose only)")
	f.StringVar(&o.Since, "since", "", "Only logs since a duration back (10m, 2h) or a timestamp (2026-11-03T15:04:05Z)")
	f.StringVar(&o.Until, "until", "", "Only logs before a duration back or a timestamp (compose only)")
	f.BoolVarP(&o.Timestamps, "timestamps", "t", false, "Show timestamps")
	f.StringVar(&o.Grep, "grep", "", "Only lines containing this text, filtered on the Server")
	f.BoolVarP(&o.IgnoreCase, "ignore-case", "i", false, "Match --grep ignoring case")
	f.BoolVarP(&o.Regex, "regex", "E", false, "Treat --grep as an extended regular expression")
	return c
}

// badPatternHint is the hint for a --grep -E pattern the Server's grep
// refuses.
const badPatternHint = "-E takes an extended regular expression, as the Server's grep reads it; drop -E to match the text as is"

// gateLines is LogsOptions.Lines as gate.LogsArgs.Lines. Every line is
// MaxLogLines, which Gates of every version read as their bound.
func gateLines(n int) *int {
	if n < 0 {
		n = gate.MaxLogLines
	}
	return gate.LinesArg(n)
}

// appLogsScript is the `app logs` script for svcs (nil: every Service).
func appLogsScript(project string, svcs []string, swarm bool, o dash.LogsOptions) string {
	if swarm {
		return dash.LogsScript("docker service logs", remote.Quote(project+"_"+svcs[0]), o)
	}
	return dash.LogsScript("docker compose -p "+remote.Quote(project)+" logs", remote.QuoteArgs(svcs...), o)
}

func appFollowGrepScript(project string, svcs []string, swarm bool, o dash.LogsOptions) string {
	if swarm {
		return dash.FollowGrepScript("docker service logs", remote.Quote(project+"_"+svcs[0]), o)
	}
	return dash.FollowGrepScript("docker compose -p "+remote.Quote(project)+" logs", remote.QuoteArgs(svcs...), o)
}

// followGrep runs a dash.FollowGrepScript: the script travels as an
// argument and ssh's stdin stays open until this command ends, so the
// Server stops docker and grep when we exit or the connection drops.
func (a *app) followGrep(cmd *cobra.Command, script string) error {
	ctx := cmd.Context()
	srv := a.cfg.Servers[a.dest.Servers[0]]
	h, err := remote.NewSSH(a.dest.Servers[0], srv.SSH, srv.Sudo, remote.SSHOptions{})
	if err != nil {
		return err
	}
	defer h.Close()
	// An OS pipe, not io.Pipe: ssh reads it directly, so when the script
	// ends on its own (a bad pattern, docker failing) nothing waits for
	// stdin to close before RunTerminal returns.
	stdin, hold, err := os.Pipe()
	if err != nil {
		return err
	}
	defer stdin.Close()
	stop := context.AfterFunc(ctx, func() { hold.Close() })
	defer stop()
	defer hold.Close()
	return h.RunTerminal(ctx, script, false, stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// exitNoContainers is the exit code of statsScript when the App has no
// running containers to show.
const exitNoContainers = 43

const statsFormat = `table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.MemPerc}}\t{{.NetIO}}\t{{.BlockIO}}\t{{.PIDs}}`

// statsScript finds the App's running containers (all, or of services) and
// execs docker stats on them. Under swarm only tasks on this node are found.
func statsScript(project string, services []string, swarm, stream, jsonOut bool) string {
	var find string
	switch {
	case !swarm:
		find = "docker compose -p " + remote.Quote(project) + " ps -q"
		if len(services) > 0 {
			find += " " + remote.QuoteArgs(services...)
		}
	case len(services) == 0:
		find = "docker ps -q --filter " + remote.Quote("label=com.docker.stack.namespace="+project)
	default:
		// Repeated label filters AND together: query each Service and union.
		var qs []string
		for _, s := range services {
			qs = append(qs, "docker ps -q --filter "+remote.Quote("label=com.docker.swarm.service.name="+project+"_"+s))
		}
		find = "{ " + strings.Join(qs, "; ") + "; } | sort -u"
	}
	flags, format := "", statsFormat
	if !stream {
		flags = " --no-stream"
	}
	if jsonOut {
		format = "{{json .}}"
	}
	script := "set -eu\nids=$(" + find + ")\n" +
		"if [ -z \"$ids\" ]; then exit " + strconv.Itoa(exitNoContainers) + "; fi\n"
	stats := "docker stats" + flags + " --format " + remote.Quote(format) + " $ids"
	if !stream {
		return script + "exec " + stats + "\n"
	}
	// Some Docker clients cancel collection on SIGINT but keep rendering when
	// given container IDs. SSH forwards Ctrl-C to the remote PTY, not our local
	// signalContext. Keep a shell waiting so it can end this read-only client
	// even when Docker ignores cancellation. Never signal the containers.
	return script + `stats_pid=
trap 'if [ -n "$stats_pid" ]; then kill -KILL "$stats_pid" 2>/dev/null || :; wait "$stats_pid" 2>/dev/null || :; fi' 0
trap 'exit 130' INT
trap 'exit 129' HUP
trap 'exit 143' TERM
` + stats + ` </dev/null &
stats_pid=$!
code=0
wait "$stats_pid" || code=$?
stats_pid=
exit "$code"
`
}

func statsCmd(g *globals) *cobra.Command {
	var noStream bool
	c := &cobra.Command{
		Use: "stats [SERVICE...]", Short: "Show CPU, memory, network and disk I/O of the App's containers",
		Long: `Show CPU, memory, network and disk I/O of the App's containers.

In a terminal this is docker's live view (Ctrl-C to quit). Otherwise it prints
one snapshot and exits: with --no-stream, --json, when stdin or stdout is not a
terminal, or when CI is set or TERM=dumb. Scripts and coding agents should use
--no-stream (table) or --json (one docker JSON object per container).`,
		Example: "  yoho app stats\n  yoho app stats web --no-stream\n  yoho --json app stats",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			names, derr := a.serviceNames(ctx)
			switch {
			case derr != nil && len(args) > 0:
				a.ui.Warn("cannot list the App's Services, not checking %s: %v", strings.Join(args, ", "), derr)
			case derr != nil:
				// No SERVICE means every Service: nothing to check.
			case len(names) == 0:
				return a.fail(noServicesError())
			default:
				if err := checkServices(names, args); err != nil {
					return a.fail(err)
				}
			}
			swarm := a.dest.Runtime == "swarm"
			live := liveStats(noStream, g.json, cmd.InOrStdin(), cmd.OutOrStdout(), os.Getenv)
			if a.scoped {
				return a.scopedStats(cmd, args, live)
			}
			name := a.dest.Servers[0]
			if swarm {
				a.ui.Info("swarm: only tasks running on %s (the Destination's first Server)", name)
			}
			srv := a.cfg.Servers[name]
			h, err := remote.NewSSH(name, srv.SSH, srv.Sudo, a.sshOptions())
			if err != nil {
				return err
			}
			defer h.Close()
			var stdin io.Reader
			if live {
				stdin = cmd.InOrStdin()
			}
			err = h.RunTerminal(ctx, statsScript(a.project(), args, swarm, live, g.json), live, stdin, cmd.OutOrStdout(), cmd.ErrOrStderr())
			var ee *remote.ExitError
			switch {
			case err == nil:
				return nil
			case errors.As(err, &ee) && ee.Code == exitNoContainers:
				return a.fail(&hintError{"no running containers for " + a.cfg.App + " on " + name, "check `yoho app ps`"})
			case live && (ctx.Err() != nil || errors.As(err, &ee) && ee.Code == 130):
				// Ctrl-C ends the live view.
				return nil
			}
			return err
		},
	}
	c.Flags().BoolVar(&noStream, "no-stream", false, "Print one snapshot instead of a live view")
	return c
}

// liveStats reports whether stats should run docker's live view: only for a
// human at a terminal, never in CI, dumb terminals, --no-stream or --json.
func liveStats(noStream, jsonOut bool, in, out any, getenv func(string) string) bool {
	if noStream || jsonOut || getenv("CI") != "" || getenv("TERM") == "dumb" {
		return false
	}
	return isTerminal(in) && isTerminal(out)
}

// isTerminal reports whether f is a terminal file.
func isTerminal(f any) bool {
	file, ok := f.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}
