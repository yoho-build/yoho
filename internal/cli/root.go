// Package cli wires Yoho's commands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/secrets"
	"github.com/yoho-build/yoho/internal/ui"
)

// Version is set at build time with -ldflags "-X .../cli.Version=...".
var Version = "dev"

type globals struct {
	configPath  string
	destination string
	json        bool
	verbose     bool
	// secretsRetries is --secrets-retries; negative means unset.
	secretsRetries int
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	g := &globals{}
	root := &cobra.Command{
		Use:   "yoho",
		Short: "Deploy Docker Compose apps to your own servers with zero downtime",
		Long: `Yoho deploys Docker Compose (or Swarm) apps to servers over SSH:
builds images locally, ships them without a registry, switches traffic with
kamal-proxy, keeps secrets in your password manager, and backs up volumes.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&g.configPath, "config", "c", "", "Yoho file (default: yoho.{yml,yaml,toml,json,jsonc} in the current directory)")
	pf.StringVarP(&g.destination, "destination", "d", "", "Destination (default: $YOHO_DESTINATION, then production, then the only one)")
	pf.BoolVar(&g.json, "json", false, "Emit NDJSON events instead of human output")
	pf.BoolVarP(&g.verbose, "verbose", "v", false, "Stream remote command output")
	pf.IntVar(&g.secretsRetries, "secrets-retries", -1, "Times to rerun a secrets provider call whose approval prompt timed out (default: $YOHO_SECRETS_RETRIES, then 1 on a terminal, else 0)")

	root.AddCommand(
		initCmd(g),
		configCmd(g),
		schemaCmd(g),
		secretsCmd(g),
		versionCmd(),
	)
	for _, add := range extraCommands {
		root.AddCommand(add(g))
	}

	ctx, cancel := signalContext()
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		var silent *silentError
		if !errors.As(err, &silent) {
			fmt.Fprintln(os.Stderr, "Error:", err)
		}
		return 1
	}
	return 0
}

// extraCommands lets other files register commands.
var extraCommands []func(*globals) *cobra.Command

// silentError is returned when the UI already reported the failure.
type silentError struct{ err error }

func (e *silentError) Error() string { return e.err.Error() }
func (e *silentError) Unwrap() error { return e.err }

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the Yoho version",
		Run:   func(cmd *cobra.Command, _ []string) { fmt.Fprintln(cmd.OutOrStdout(), Version) },
	}
}

func (g *globals) ui(cmd *cobra.Command) *ui.UI {
	mode := ui.Human
	if g.json {
		mode = ui.JSON
	}
	return ui.New(cmd.OutOrStdout(), mode, g.verbose)
}

// app is a loaded App: Yoho file plus selected Destination.
type app struct {
	g        *globals
	dir      string
	path     string
	overlay  string
	cfg      *config.Config
	destName string
	dest     config.Destination
	ui       *ui.UI
	// scoped: the Destination is reached through the Gate (ADR 0011).
	scoped bool
}

// load loads the App for cmd. With scoped access only the commands the Gate
// serves run; the rest fail before connecting.
func (g *globals) load(cmd *cobra.Command) (*app, error) {
	a, err := g.loadApp(cmd)
	if err != nil {
		return nil, err
	}
	if a.scoped && !scopedCommand(cmd) {
		return nil, a.fail(&hintError{
			fmt.Sprintf("`%s` is not available with scoped access to %s/%s", cmd.CommandPath(), a.cfg.App, a.destName),
			"scoped access serves deploy, rollback, releases, app ps/logs/stats, tui and ui; ask the operator for the rest",
		})
	}
	return a, nil
}

// loadOperator loads the App for operator-only commands (`yoho access`),
// which always use full SSH whatever the Destination's access says.
func (g *globals) loadOperator(cmd *cobra.Command) (*app, error) {
	a, err := g.loadApp(cmd)
	if err != nil {
		return nil, err
	}
	a.scoped = false
	return a, nil
}

func (g *globals) loadApp(cmd *cobra.Command) (*app, error) {
	path := g.configPath
	if path == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		if path, err = config.Find(wd); err != nil {
			return nil, fmt.Errorf("%w\nhint: run `yoho init` to create a Yoho file", err)
		}
	}
	selected := g.destination
	if selected == "" {
		selected = os.Getenv("YOHO_DESTINATION")
	}
	cfg, name, overlay, err := config.LoadForDestination(path, selected)
	if err != nil {
		if destSelectErr(err) {
			return nil, fmt.Errorf("%w\nhint: pass -d <destination>", err)
		}
		return nil, err
	}
	if errs := config.Validate(cfg); len(errs) > 0 {
		return nil, fmt.Errorf("invalid %s:\n  %w", filepath.Base(path), errors.Join(errs...))
	}
	dest, err := cfg.Dest(name)
	if err != nil {
		return nil, err
	}
	a := &app{g: g, dir: filepath.Dir(path), path: path, overlay: overlay, cfg: cfg, destName: name, dest: dest, ui: g.ui(cmd)}
	release.Root = a.root()
	if a.scoped, err = scopedAccess(dest, os.Getenv); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *app) root() string {
	if len(a.dest.Servers) > 0 {
		if s, ok := a.cfg.Servers[a.dest.Servers[0]]; ok && s.Root != "" {
			return s.Root
		}
	}
	return release.DefaultRoot
}

func (a *app) project() string { return release.ProjectName(a.cfg.App, a.destName) }

func (a *app) compose(ctx context.Context) (*composefile.Result, error) {
	r, err := composefile.Load(ctx, a.dir, a.cfg.Compose, a.dest.Env, a.project(), a.destName)
	if err != nil {
		return nil, err
	}
	for _, key := range a.cfg.Secrets.Nonempty {
		for _, name := range sortedKeys(r.Ext) {
			if _, generated := r.Ext[name].Generate[key]; generated {
				return nil, fmt.Errorf("secrets.nonempty: source key %s is generated on the Server; only locally resolved keys may be declared", key)
			}
		}
	}
	return r, nil
}

func destSelectErr(err error) bool {
	s := err.Error()
	return strings.Contains(s, "unknown Destination") || strings.Contains(s, "choose one with -d")
}

// secretKeys lists every key a Service may reference: file keys, provider
// values for this Destination, and keys generated on the Server.
func (a *app) secretKeys(r *composefile.Result) ([]string, error) {
	keys, err := secrets.FileKeys(a.dir, a.destName)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, k := range keys {
		seen[k] = true
	}
	for k, v := range a.cfg.Secrets.Values {
		if len(v.Destinations) == 0 || contains(v.Destinations, a.destName) {
			seen[k] = true
		}
	}
	if r != nil {
		for _, ext := range r.Ext {
			for k := range ext.Generate {
				seen[k] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// resolveSecretsRetries picks the retry count: the flag, then the
// environment, then one retry on a terminal (someone can approve again) and
// none otherwise, so agents and CI never wait twice silently.
func resolveSecretsRetries(flag int, env string, tty bool) int {
	if flag >= 0 {
		return flag
	}
	if n, err := strconv.Atoi(strings.TrimSpace(env)); err == nil && n >= 0 {
		return n
	}
	if tty {
		return 1
	}
	return 0
}

// loadSecrets resolves the Destination's secrets. step, when not nil, is the
// "Resolve secrets" step that shows a provider approval wait.
func (a *app) loadSecrets(ctx context.Context, step *ui.Step) (*secrets.Store, error) {
	// Only opt-in contracts need compose inspection here. Commands that never
	// resolve secrets retain their existing offline/retained-state behavior.
	if len(a.cfg.Secrets.Nonempty) > 0 {
		if _, err := a.compose(ctx); err != nil {
			return nil, err
		}
	}
	return secrets.Load(ctx, a.dir, a.destName, a.cfg.Secrets, secrets.LoadOptions{
		Warn:    func(msg string) { a.ui.Warn("%s", msg) },
		Retries: resolveSecretsRetries(a.g.secretsRetries, os.Getenv("YOHO_SECRETS_RETRIES"), stdinIsTTY()),
		OnWait: func(ev secrets.WaitEvent) {
			msg := "waiting for secrets provider approval… (" + ev.Provider + ")"
			fields := map[string]any{"provider": ev.Provider, "ref": ev.Ref, "elapsed_ms": ev.Elapsed.Milliseconds()}
			if step != nil {
				step.Waiting(msg, fields)
			} else {
				a.ui.Waiting(msg, fields)
			}
		},
	})
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
