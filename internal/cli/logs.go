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

func logsCmd(g *globals) *cobra.Command {
	var follow, all bool
	var tail int
	c := &cobra.Command{
		Use: "logs [SERVICE...]", Short: "Show logs of one or more Services",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			swarm := a.dest.Runtime == "swarm"
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
				err := a.gateCall(cmd.Context(), "logs", gate.LogsArgs{ServicesArgs: gate.ServicesArgs{Services: svcs}, Lines: tail, Follow: follow}, nil, cmd.OutOrStdout(), nil)
				if follow && cmd.Context().Err() != nil {
					return nil // Ctrl-C ends a follow
				}
				return err
			}
			if swarm {
				s := fmt.Sprintf("docker service logs --tail %d", tail)
				if follow {
					s += " -f"
				}
				return a.remoteRun(cmd, s+" "+remote.Quote(a.project()+"_"+svcs[0])+" 2>&1")
			}
			s := fmt.Sprintf("docker compose -p %s logs --tail %d", remote.Quote(a.project()), tail)
			if follow {
				s += " -f"
			}
			if len(svcs) > 0 {
				s += " " + remote.QuoteArgs(svcs...)
			}
			return a.remoteRun(cmd, s+" 2>&1")
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "Follow")
	c.Flags().IntVarP(&tail, "lines", "n", 100, "Lines from the end")
	c.Flags().BoolVar(&all, "all", false, "Every Service, interleaved (compose only)")
	return c
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
