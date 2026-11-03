package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/yoho-build/yoho/internal/audit"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/secrets"
	"github.com/yoho-build/yoho/internal/version"
)

func init() { extraCommands = append(extraCommands, auditCmd) }

// auditedCommands are the commands that change the Server or run code on
// it. Each writes a started and a finished record to the Destination's
// audit log (internal/audit). Read-only commands and plan are not logged.
var auditedCommands = []string{
	"deploy", "apply", "rollback", "remove",
	"app exec", "app run",
	"backup run", "backup restore", "backup drill",
	"secrets rotate",
	"lock acquire", "lock release",
	"proxy boot", "proxy upgrade",
}

// auditSkipFlags are global flags that say nothing about the invocation.
var auditSkipFlags = map[string]bool{"config": true, "destination": true, "json": true, "verbose": true, "secrets-retries": true}

// auditSecretsTimeout bounds resolving secrets just to redact an exec argv.
const auditSecretsTimeout = 20 * time.Second

// auditWrap wraps the RunE of every audited command below root.
func auditWrap(g *globals, root *cobra.Command) {
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.HasParent() && c.RunE != nil {
			path := strings.TrimPrefix(c.CommandPath(), root.Name()+" ")
			for _, name := range auditedCommands {
				if name == path {
					inner := c.RunE
					c.RunE = func(cmd *cobra.Command, args []string) error {
						s := g.auditStart(cmd, args)
						err := inner(cmd, args)
						s.finish(err)
						return err
					}
					break
				}
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// auditRun is one audited command: the open Server connection and the
// record being written. A nil *auditRun does nothing.
type auditRun struct {
	a     *app
	h     remote.Host
	rec   audit.Record
	begin time.Time
	red   *audit.Redactor
	dead  bool // an append failed: stop trying and stop warning
}

// auditStart loads the App, writes the "started" record and returns the run
// to finish. It never fails: whatever goes wrong, the command itself runs
// (and reports its own load errors).
func (g *globals) auditStart(cmd *cobra.Command, args []string) *auditRun {
	if f := cmd.Flags().Lookup("dry-run"); f != nil && f.Value.String() == "true" {
		return nil // changes nothing
	}
	a, err := g.loadApp(cmd)
	if err != nil || a.scoped || len(a.dest.Servers) == 0 {
		return nil // scoped access: the Gate writes its own audit
	}
	name := a.dest.Servers[0]
	srv := a.cfg.Servers[name]
	h, err := remote.NewSSH(name, srv.SSH, srv.Sudo, a.sshOptions())
	if err != nil {
		return nil
	}
	r := &auditRun{a: a, h: h, begin: time.Now(), red: audit.NewRedactor()}
	command := strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()+" ")
	argvCmd := command == "app exec" || command == "app run"
	if argvCmd {
		r.red = a.auditRedactor(cmd.Context())
	}
	r.rec = audit.Record{
		Time: r.begin.UTC(), Run: audit.NewRun(), Phase: audit.Started, Performer: performer(),
		Command: command, App: a.cfg.App, Destination: a.destName,
		Version: a.auditVersion(cmd, command, args),
		Args:    auditArgs(cmd, command, args, r.red),
		Yoho:    Version, Via: audit.ViaCLI,
	}
	r.append(cmd.Context(), r.rec)
	return r
}

// finish writes the "finished" record for err and closes the connection.
func (r *auditRun) finish(err error) {
	if r == nil {
		return
	}
	defer r.h.Close()
	rec := r.rec
	rec.Time, rec.Phase = time.Now().UTC(), audit.Finished
	code := 0
	if err != nil {
		code = 1
		var ee *exitError
		if errors.As(err, &ee) {
			code = ee.code
		}
		rec.Error = r.red.String(err.Error())
	}
	rec.Exit = &code
	rec.Seconds = time.Since(r.begin).Round(time.Millisecond).Seconds()
	// The command may have been cancelled (Ctrl-C): still record the end.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	r.append(ctx, rec)
}

// append writes rec; a failure warns once and disables the rest.
func (r *auditRun) append(ctx context.Context, rec audit.Record) {
	if r.dead {
		return
	}
	if err := audit.Append(ctx, r.h, rec); err != nil {
		r.dead = true
		r.a.ui.Warn("audit log not written (the command goes on): %s", firstLine(r.red.String(err.Error())))
	}
}

// auditRedactor masks the Destination's secret values in an exec argv. It
// resolves them once, bounded by auditSecretsTimeout; when that fails the
// pattern-based redaction still applies.
func (a *app) auditRedactor(ctx context.Context) *audit.Redactor {
	r := audit.NewRedactor()
	keys, err := secrets.FileKeys(a.dir, a.destName)
	if err != nil || (len(keys) == 0 && len(a.cfg.Secrets.Values) == 0) {
		return r
	}
	ctx, cancel := context.WithTimeout(ctx, auditSecretsTimeout)
	defer cancel()
	store, err := secrets.Load(ctx, a.dir, a.destName, a.cfg.Secrets, secrets.LoadOptions{})
	if err != nil {
		a.ui.Warn("audit log: secrets not resolved, the command is redacted by pattern only")
		return r
	}
	return r.WithStore(store.RedactString)
}

// auditVersion is the Version a command works on, when it names one.
func (a *app) auditVersion(cmd *cobra.Command, command string, args []string) string {
	switch command {
	case "deploy", "apply":
		if f := cmd.Flags().Lookup("version"); f != nil && f.Value.String() != "" {
			return f.Value.String()
		}
		if v, err := version.FromGit(a.dir); err == nil {
			return v
		}
	case "rollback":
		if len(args) == 1 {
			return args[0]
		}
	}
	return ""
}

// auditArgs summarizes an invocation without secret values: the flags that
// were set, the positional arguments, and for app exec/run the Service and
// argv. Everything passes through red.
func auditArgs(cmd *cobra.Command, command string, args []string, red *audit.Redactor) map[string]any {
	out := map[string]any{}
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if auditSkipFlags[f.Name] {
			return
		}
		out[f.Name] = auditFlagValue(f, red)
	})
	switch command {
	case "app exec", "app run":
		if len(args) > 0 {
			for k, v := range audit.ArgvArgs(args[0], red.Argv(args[1:])) {
				out[k] = v
			}
		}
	default:
		if len(args) > 0 {
			out["args"] = red.Argv(args)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func auditFlagValue(f *pflag.Flag, red *audit.Redactor) any {
	switch f.Value.Type() {
	case "bool":
		b, _ := strconv.ParseBool(f.Value.String())
		return b
	case "int", "int64":
		n, _ := strconv.Atoi(f.Value.String())
		return n
	case "stringSlice", "stringArray":
		if sv, ok := f.Value.(pflag.SliceValue); ok {
			return red.Argv(sv.GetSlice())
		}
	}
	return red.String(f.Value.String())
}

func auditCmd(g *globals) *cobra.Command {
	var n int
	var since string
	c := &cobra.Command{
		Use:   "audit [-n 50] [--since DURATION] [--json]",
		Short: "Show who ran what on this Destination (deploy, apply, rollback, exec, backup, ...)",
		Long: `Show the Destination's audit log, newest last: who ran which mutating
command, with which Version, how it ended and how long it took.

The log is <root>/audit/<app>/<destination>.log on the (first) Server,
one JSON line per record: a "started" record before the command changes
anything and a "finished" record after it. A command that was killed shows
as "unfinished". For app exec and app run the command is recorded after the
secrets redactor; secret values are never recorded. Members of the Gate
are in 'yoho access audit'.

--since takes a duration (36h, 7d) or a date (2026-11-03 or RFC 3339).
--json prints the records themselves, one per line.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			if n < 1 || n > 10000 {
				return a.fail(&hintError{"-n must be between 1 and 10000", ""})
			}
			var cutoff time.Time
			if since != "" {
				if cutoff, err = parseSince(since, time.Now()); err != nil {
					return a.fail(&hintError{err.Error(), "use a duration like 36h or 7d, or a date like 2026-11-03"})
				}
			}
			name := a.dest.Servers[0]
			srv := a.cfg.Servers[name]
			h, err := remote.NewSSH(name, srv.SSH, srv.Sudo, a.sshOptions())
			if err != nil {
				return a.fail(&hintError{err.Error(), "check `ssh " + srv.SSH + "` works without a password"})
			}
			defer h.Close()
			entries, err := audit.Read(cmd.Context(), h, a.cfg.App, a.destName, 20000)
			if err != nil {
				return a.fail(&hintError{err.Error(), hintFor(err, "check that the Server is reachable")})
			}
			runs := audit.Select(audit.Pair(entries), cutoff, n)
			if g.json {
				w := cmd.OutOrStdout()
				for _, r := range runs {
					if r.Started != nil {
						fmt.Fprintln(w, r.Started.Raw)
					}
					if r.Finished != nil {
						fmt.Fprintln(w, r.Finished.Raw)
					}
				}
				return nil
			}
			if len(runs) == 0 {
				a.ui.Info("no audit records for %s/%s", a.cfg.App, a.destName)
				return nil
			}
			rows := make([][]string, 0, len(runs))
			for _, r := range runs {
				first := r.First()
				dur := ""
				if d, ok := r.Duration(); ok {
					dur = d.Round(time.Second).String()
					if d < time.Second {
						dur = d.Round(time.Millisecond).String()
					}
				}
				rows = append(rows, []string{
					first.Time.Local().Format("2006-01-02 15:04:05"), first.Performer, auditCommandText(first),
					shortVersion(first.Version), audit.Truncate(r.Result(), 70), dur,
				})
			}
			a.ui.Table([]string{"TIME", "BY", "COMMAND", "VERSION", "RESULT", "DURATION"}, rows)
			return nil
		},
	}
	c.Flags().IntVarP(&n, "lines", "n", 50, "Show the last N commands")
	c.Flags().StringVar(&since, "since", "", "Only commands since DURATION ago (36h, 7d) or a date")
	return c
}

// auditCommandText is the command with the Service and argv of an exec/run.
func auditCommandText(e audit.Entry) string {
	s := e.Command
	if svc, ok := e.Args["service"].(string); ok {
		s += " " + svc
		if argv, ok := e.Args["argv"].([]any); ok {
			parts := make([]string, 0, len(argv))
			for _, a := range argv {
				parts = append(parts, fmt.Sprint(a))
			}
			s += " -- " + audit.Truncate(strings.Join(parts, " "), 60)
		}
	}
	return s
}

// parseSince is a cutoff time: now minus a duration ("36h", "7d", "2w"), or
// a date (YYYY-MM-DD, local midnight) or RFC 3339 time.
func parseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if len(s) > 1 {
		if n, err := strconv.Atoi(s[:len(s)-1]); err == nil && n >= 0 {
			switch s[len(s)-1] {
			case 'd':
				return now.Add(-time.Duration(n) * 24 * time.Hour), nil
			case 'w':
				return now.Add(-time.Duration(n) * 7 * 24 * time.Hour), nil
			}
		}
	}
	if d, err := time.ParseDuration(s); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("cannot read --since %q", s)
}
