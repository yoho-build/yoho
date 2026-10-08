// Package cli wires Yoho's commands.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

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
	pf.StringVarP(&g.destination, "destination", "d", "", "Destination (default: the only one)")
	pf.BoolVar(&g.json, "json", false, "Emit NDJSON events instead of human output")
	pf.BoolVarP(&g.verbose, "verbose", "v", false, "Stream remote command output")

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
	cfg      *config.Config
	destName string
	dest     config.Destination
	ui       *ui.UI
}

func (g *globals) load(cmd *cobra.Command) (*app, error) {
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
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if errs := config.Validate(cfg); len(errs) > 0 {
		return nil, fmt.Errorf("invalid %s:\n  %w", filepath.Base(path), errors.Join(errs...))
	}
	name, err := cfg.DestName(g.destination)
	if err != nil {
		return nil, fmt.Errorf("%w\nhint: pass -d <destination>", err)
	}
	dest, err := cfg.Dest(name)
	if err != nil {
		return nil, err
	}
	a := &app{g: g, dir: filepath.Dir(path), path: path, cfg: cfg, destName: name, dest: dest, ui: g.ui(cmd)}
	release.Root = a.root()
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
	return composefile.Load(ctx, a.dir, a.cfg.Compose, a.dest.Env, a.project())
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

func (a *app) loadSecrets(ctx context.Context) (*secrets.Store, error) {
	return secrets.Load(ctx, a.dir, a.destName, a.cfg.Secrets, secrets.LoadOptions{
		Warn: func(msg string) { a.ui.Warn("%s", msg) },
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
