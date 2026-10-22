package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/secrets"
)

func configCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "Inspect and check the Yoho file and compose files"}
	var offline bool
	check := &cobra.Command{
		Use:   "check",
		Short: "Validate config and secret references (resolves secrets only with secrets.nonempty; --offline never does)",
		Long: `Validate the Yoho file, compose files, x-yoho and secret references.

It opens no connection and runs no Hook. Secret names are checked against the
keys in .yoho/secrets without running any line. With secrets.nonempty set it
also resolves secrets, which runs $(...) lines and password managers; --offline
(or YOHO_OFFLINE=1) skips that, so the command is safe in CI.

Exit codes: 0 no errors, 1 findings or a failed secrets.nonempty check, 2 the
Yoho or compose files could not be loaded (unreadable, parse error, unknown
Destination).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !cmd.Flags().Changed("offline") && os.Getenv("YOHO_OFFLINE") == "1" {
				offline = true
			}
			return runConfigCheck(cmd, g, offline)
		},
	}
	check.Flags().BoolVar(&offline, "offline", false, "Never resolve secrets (no $(...) lines or password managers); also YOHO_OFFLINE=1")
	c.AddCommand(check)
	c.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the resolved Yoho file (with defaults) as JSON",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			overlay := a.overlay
			if overlay == "" {
				overlay = "-"
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "# destination: %s (overlay: %s)\n", a.destName, overlay)
			enc := json.NewEncoder(cmd.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(a.cfg)
		},
	})
	return c
}

func schemaCmd(g *globals) *cobra.Command {
	var ext bool
	c := &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema of the Yoho file (or of x-yoho with --ext)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var b []byte
			var err error
			if ext {
				b, err = config.ServiceExtSchema()
			} else {
				b, err = config.Schema()
			}
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(append(b, '\n'))
			return err
		},
	}
	c.Flags().BoolVar(&ext, "ext", false, "Schema of the compose x-yoho extension")
	return c
}

func secretsCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "secrets", Short: "Inspect secrets (values are never printed unless revealed on a terminal)"}
	c.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Resolve secrets and list keys, sources and lengths",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			step := a.ui.Step("", "Resolve secrets for %s", a.destName)
			s, err := a.loadSecrets(cmd.Context(), step)
			if err != nil {
				step.Fail(err, secretsHint(err, "check .yoho/secrets and that op / bw / bws are signed in"))
				return &silentError{err}
			}
			step.Done(fmt.Sprintf("%d keys", len(s.Keys())))
			var rows [][]string
			for _, e := range secrets.Describe(s, nil) {
				rows = append(rows, []string{e.Key, e.Ref, fmt.Sprintf("%d", e.Length)})
			}
			a.ui.Table([]string{"KEY", "SOURCE", "LENGTH"}, rows)
			return nil
		},
	})
	var reveal bool
	p := &cobra.Command{
		Use:   "print KEY...",
		Short: "Print secret values (requires --reveal and a terminal)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !reveal {
				return errors.New("refusing to print secret values without --reveal")
			}
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			s, err := a.loadSecrets(cmd.Context(), nil)
			if err != nil {
				return err
			}
			return secrets.Reveal(os.Stdout, s, args, term.IsTerminal(int(os.Stdout.Fd())))
		},
	}
	p.Flags().BoolVar(&reveal, "reveal", false, "Confirm printing values")
	c.AddCommand(p)
	return c
}

const initYoho = `# yaml-language-server: $schema=https://yohobuild.com/schema/yoho.schema.json
app: %s

servers:
  primary:
    ssh: yoho@203.0.113.10      # [user@]host[:port], uses ~/.ssh/config
    # root: /home/yoho/yoho     # state dir when sudo is unavailable (default /var/lib/yoho)

destinations:
  production:
    servers: [primary]
  # staging:
  #   servers: [primary]
  # Optional overlay yoho.staging.yml is deep-merged over this file for that Destination.

builder:
  location: local               # local | remote | server

proxy:
  http_port: 80                 # 0 = not published (e.g. Cloudflare Tunnel only)
  https_port: 443
`

const initSecrets = `# Yoho secrets for every Destination. Trusted shell: $(...) runs locally at deploy.
# Override per Destination in .yoho/secrets.<destination>. Never commit values.
#
# SECRET_KEY_BASE=$(op read "op://Production/myapp/secret_key_base")
# STRIPE_KEY=$(bw get password stripe-live)
`

func initCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Create yoho.yml, .yoho/secrets and sample hooks in the current directory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			u := g.ui(cmd)
			if p, err := config.Find(wd); err == nil {
				return fmt.Errorf("%s already exists", filepath.Base(p))
			}
			name := strings.ToLower(filepath.Base(wd))
			name = strings.Map(func(r rune) rune {
				if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
					return r
				}
				return '-'
			}, name)
			files := map[string]string{
				"yoho.yml":           fmt.Sprintf(initYoho, name),
				".yoho/secrets":      initSecrets,
				".yoho/hooks/README": "Executable scripts named after a hook run on your machine: pre-connect, pre-build, pre-deploy, post-deploy, pre-backup, post-backup, ...\nEnv: YOHO_APP, YOHO_DESTINATION, YOHO_VERSION, YOHO_HOSTS, YOHO_COMMAND, YOHO_PERFORMER.\n",
			}
			for _, rel := range []string{"yoho.yml", ".yoho/secrets", ".yoho/hooks/README"} {
				p := filepath.Join(wd, rel)
				if _, err := os.Stat(p); err == nil {
					u.Warn("%s exists, left unchanged", rel)
					continue
				}
				if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
					return err
				}
				mode := os.FileMode(0o644)
				if strings.HasPrefix(rel, ".yoho/secrets") {
					mode = 0o600
				}
				if err := os.WriteFile(p, []byte(files[rel]), mode); err != nil {
					return err
				}
				u.Info("created %s", rel)
			}
			if err := ensureGitignore(wd, ".yoho/secrets*"); err != nil {
				return err
			}
			u.Info("Next: edit yoho.yml, add x-yoho to compose services, then `yoho config check`")
			return nil
		},
	}
}

func ensureGitignore(dir, line string) error {
	p := filepath.Join(dir, ".gitignore")
	b, _ := os.ReadFile(p)
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == line {
			return nil
		}
	}
	if len(b) > 0 && b[len(b)-1] != '\n' {
		b = append(b, '\n')
	}
	return os.WriteFile(p, append(b, []byte(line+"\n")...), 0o644)
}

// Exit codes of `config check`.
const (
	exitFindings = 1
	exitLoad     = 2
)

// runConfigCheck is `yoho config check`. It never connects to a Server.
func runConfigCheck(cmd *cobra.Command, g *globals, offline bool) error {
	a, err := g.load(cmd)
	if err != nil {
		g.ui(cmd).Error(err, "")
		g.ui(cmd).Check(false, 1, offline, 0, 0)
		return &exitError{exitLoad, &silentError{err}}
	}
	errCount, services, keyCount := 0, 0, 0
	step := a.ui.Step("", "Check %s and compose for %s", filepath.Base(a.path), a.destName)
	fail := func(code int, err error, hint string) error {
		step.Fail(err, hint)
		a.ui.Check(false, errCount, offline, services, keyCount)
		return &exitError{code, &silentError{err}}
	}
	r, err := a.compose(cmd.Context())
	if err != nil {
		errCount = 1
		code := exitLoad
		var extErr *composefile.ExtError
		if errors.As(err, &extErr) {
			code = exitFindings
		}
		return fail(code, err, "fix the compose file, then run `yoho config check` again")
	}
	services = len(r.Project.Services)
	keys, err := a.secretKeys(r)
	if err != nil {
		errCount = 1
		return fail(exitLoad, err, "")
	}
	keyCount = len(keys)
	findings := composefile.Check(r, a.dest.Runtime, keys)
	findings = append(findings, composefile.CheckServerBuild(r, a.dir, a.cfg.Builder)...)
	if t := a.proxyConfig().Tunnel; t != nil && t.TokenSecret == "" {
		for _, name := range sortedKeys(r.Ext) {
			if p := r.Ext[name].Proxy; p != nil && len(p.Hosts) > 0 {
				findings = append(findings, composefile.Finding{Level: composefile.LevelWarning, Service: name,
					Message: "Quick Tunnel requests use a *.trycloudflare.com Host and won't match proxy.hosts; leave hosts empty or set proxy.tunnel.token_secret"})
			}
		}
	}
	for _, f := range findings {
		if f.Level == composefile.LevelError {
			errCount++
		}
		a.ui.Finding(f.Level, f.Service, f.Message)
	}
	if errCount > 0 {
		return fail(exitFindings, fmt.Errorf("%d error(s)", errCount), "")
	}
	if len(a.cfg.Secrets.Nonempty) > 0 {
		if offline {
			a.ui.Warn("secrets.nonempty not verified (--offline)")
		} else if _, err := a.loadSecrets(cmd.Context(), step); err != nil {
			errCount = 1
			return fail(exitFindings, err, secretsHint(err, "check the declared source keys in .yoho/secrets and secrets.values"))
		}
	}
	step.Done(fmt.Sprintf("%d services, %d secret keys, runtime %s", services, keyCount, a.dest.Runtime))
	a.ui.Check(true, 0, offline, services, keyCount)
	return nil
}
