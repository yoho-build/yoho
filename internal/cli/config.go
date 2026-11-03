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
	"github.com/yoho-build/yoho/internal/templates"
	"github.com/yoho-build/yoho/internal/ui"
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
	c := &cobra.Command{Use: "secrets", Short: "Inspect or rotate secrets (values are never printed unless revealed on a terminal)"}
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
			step.Done(plural(len(s.Keys()), "key", "keys"))
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
	c.AddCommand(secretsRotateCmd(g))
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
	var tmpl []string
	var list bool
	c := &cobra.Command{
		Use:   "init",
		Short: "Create yoho.yml, .yoho/secrets and sample hooks; --template adds ready-to-deploy Services",
		Long: `Create yoho.yml, .yoho/secrets and sample hooks in the current directory.

--template NAME[,NAME...] also writes compose Services (` + strings.Join(templates.Names(), ", ") + `)
with stateful, generated secrets, Backups and healthchecks set, and no
published ports. An existing compose file gets the Services appended; a
Service or volume name that already exists is refused and nothing is
written. With --template an existing Yoho file is left unchanged.
--list-templates prints the templates.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			u := g.ui(cmd)
			if list {
				listTemplates(g, u)
				return nil
			}
			var names []string
			if cmd.Flags().Changed("template") {
				var err error
				if names, err = templates.Parse(tmpl); err != nil {
					var te *templates.Error
					if errors.As(err, &te) {
						u.Error(te, te.Hint)
						return &silentError{te}
					}
					return err
				}
			}
			wd, err := os.Getwd()
			if err != nil {
				return err
			}
			return runInit(u, wd, names)
		},
	}
	c.Flags().StringSliceVar(&tmpl, "template", nil, "Add compose Services from templates: "+strings.Join(templates.Names(), ", "))
	c.Flags().BoolVar(&list, "list-templates", false, "List the Service templates and exit")
	return c
}

func listTemplates(g *globals, u *ui.UI) {
	var rows [][]string
	for _, t := range templates.List() {
		if g.json {
			u.Event("template", "", map[string]any{"name": t.Name, "image": t.Image, "description": t.Description, "connect": t.Connect, "secrets": jsonList(t.Secrets)})
			continue
		}
		rows = append(rows, []string{t.Name, t.Image, t.Description})
	}
	if !g.json {
		u.Table([]string{"TEMPLATE", "IMAGE", "DESCRIPTION"}, rows)
	}
}

func runInit(u *ui.UI, wd string, names []string) error {
	yohoPath, findErr := config.Find(wd)
	haveYoho := findErr == nil
	if haveYoho && len(names) == 0 {
		return fmt.Errorf("%s already exists", filepath.Base(yohoPath))
	}
	// Merge first: a refused template leaves every file as it was.
	var composeRel string
	var merged *templates.Result
	if len(names) > 0 {
		var err error
		composeRel, err = initComposeFile(wd, yohoPath, haveYoho)
		if err != nil {
			return err
		}
		existing, err := os.ReadFile(filepath.Join(wd, composeRel))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		merged, err = templates.Merge(existing, names)
		if err != nil {
			var te *templates.Error
			if errors.As(err, &te) {
				err = fmt.Errorf("%s: %w", composeRel, te)
				u.Error(err, te.Hint)
				return &silentError{err}
			}
			return err
		}
		// Merge saw only the edited file. Another compose entry or a
		// Destination overlay declaring the same name would merge into the
		// template's Service, so check every file the Yoho file deploys.
		if haveYoho {
			if err := templateCollisions(wd, yohoPath, composeRel, merged); err != nil {
				var he *hintError
				if errors.As(err, &he) {
					u.Error(err, he.hint)
					return &silentError{err}
				}
				return err
			}
		}
	}
	name := strings.ToLower(filepath.Base(wd))
	name = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, name)
	name = strings.TrimLeft(name, "-")
	if name == "" {
		name = "app"
	}
	if name[0] < 'a' || name[0] > 'z' {
		name = `"` + name + `"` // a name like 001 must stay a YAML string
	}
	files := map[string]string{
		"yoho.yml":           fmt.Sprintf(initYoho, name),
		".yoho/secrets":      initSecrets,
		".yoho/hooks/README": "Executable scripts named after a hook run on your machine: pre-connect, pre-build, pre-deploy, post-deploy, pre-backup, post-backup, ...\nEnv: YOHO_APP, YOHO_DESTINATION, YOHO_VERSION, YOHO_HOSTS, YOHO_COMMAND, YOHO_PERFORMER.\n",
	}
	if merged != nil {
		// Every template is a Stateful Service: suggest a Backup next to it.
		files["yoho.yml"] += "\n" + templates.BackupsExample("production")
	}
	for _, rel := range []string{"yoho.yml", ".yoho/secrets", ".yoho/hooks/README"} {
		p := filepath.Join(wd, rel)
		if rel == "yoho.yml" && haveYoho {
			u.Warn("%s exists, left unchanged", filepath.Base(yohoPath))
			continue
		}
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
	if merged == nil {
		u.Info("Next: edit yoho.yml, add x-yoho to compose services, then `yoho config check`")
		return nil
	}
	p := filepath.Join(wd, composeRel)
	mode := os.FileMode(0o644)
	verb := "created"
	if st, err := os.Stat(p); err == nil {
		mode = st.Mode().Perm()
		verb = "updated"
	}
	if err := writeFileAtomic(p, merged.Compose, mode); err != nil {
		return err
	}
	u.Info("%s %s", verb, composeRel)
	var keys []string
	for _, n := range names {
		t, _ := templates.Lookup(n)
		keys = append(keys, t.Secrets...)
		u.Event("template_added", fmt.Sprintf("  %s (%s): %s", n, t.Image, t.Connect), map[string]any{
			"template": n, "compose": composeRel, "image": t.Image, "connect": t.Connect, "secrets": t.Secrets,
		})
	}
	if len(keys) > 0 {
		u.Info("Generated on the Server at the first deploy: %s. List them in x-yoho.secrets of each Service that connects.", strings.Join(keys, ", "))
	}
	if haveYoho {
		ex := templates.BackupsExample("production")
		u.Event("backups_example", "Back them up: add a backups block to "+filepath.Base(yohoPath)+" (uncomment and adjust the Destination), for example:\n"+strings.TrimRight(ex, "\n"),
			map[string]any{"file": filepath.Base(yohoPath), "yaml": ex})
	} else {
		u.Info("yoho.yml has a commented-out backups example for these Services; uncomment it to back them up")
	}
	u.Info("Next: edit yoho.yml (servers, destinations), then `yoho config check`")
	return nil
}

// templateCollisions refuses template Services and volumes whose names are
// already declared in a compose file other than composeRel: the Yoho file's
// other compose entries and every Destination's compose overlays (and the
// compose list a yoho.<destination> file sets).
func templateCollisions(wd, yohoPath, composeRel string, merged *templates.Result) error {
	cfg, err := config.Load(yohoPath)
	if err != nil {
		return err
	}
	var d composefile.Declared
	if err := composefile.DeclaredNames(&d, wd, cfg.Compose, ""); err != nil {
		return err
	}
	for _, dest := range sortedKeys(cfg.Destinations) {
		files := cfg.Compose
		if dc, _, _, err := config.LoadForDestination(yohoPath, dest); err == nil {
			files = dc.Compose
		}
		if err := composefile.DeclaredNames(&d, wd, files, dest); err != nil {
			return err
		}
	}
	target := filepath.Clean(filepath.Join(wd, composeRel))
	other := func(files []string) string {
		for _, f := range files {
			if filepath.Clean(f) != target {
				if rel, err := filepath.Rel(wd, f); err == nil {
					return rel
				}
				return f
			}
		}
		return ""
	}
	for _, kind := range []struct {
		what  string
		names []string
		in    map[string][]string
	}{{"Service", merged.Services, d.Services}, {"volume", merged.Volumes, d.Volumes}} {
		for _, n := range kind.names {
			if f := other(kind.in[n]); f != "" {
				return &hintError{
					msg:  fmt.Sprintf("%s %q is already declared in %s; the template would merge into it", kind.what, n, f),
					hint: "rename or remove " + n + " in " + f + ", or add the template to that file by hand (`yoho init --list-templates`); nothing was written",
				}
			}
		}
	}
	return nil
}

// initComposeFile picks the compose file templates go into: the Yoho file's
// first compose entry, else the first default name that exists, else
// compose.yaml.
func initComposeFile(wd, yohoPath string, haveYoho bool) (string, error) {
	if haveYoho {
		cfg, err := config.Load(yohoPath)
		if err != nil {
			return "", err
		}
		if len(cfg.Compose) > 0 {
			rel := cfg.Compose[0]
			if filepath.IsAbs(rel) {
				return filepath.Rel(wd, rel)
			}
			return rel, nil // Find looks only in wd, so relative to the Yoho file is relative to wd
		}
	}
	for _, n := range composefile.DefaultFiles {
		if _, err := os.Stat(filepath.Join(wd, n)); err == nil {
			return n, nil
		}
	}
	return "compose.yaml", nil
}

// writeFileAtomic replaces p through a temporary file in the same directory.
func writeFileAtomic(p string, data []byte, mode os.FileMode) error {
	t, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(t.Name())
	if _, err := t.Write(data); err != nil {
		t.Close()
		return err
	}
	if err := t.Chmod(mode); err != nil {
		t.Close()
		return err
	}
	if err := t.Close(); err != nil {
		return err
	}
	return os.Rename(t.Name(), p)
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
	findings = append(findings, composefile.CheckCloudflareSSH(a.destName, a.dest, r.Project)...)
	findings = append(findings, composefile.CheckBackupCoverage(a.destName, a.cfg.Backups, r)...)
	for _, warning := range config.RcloneWarnings(a.cfg) {
		findings = append(findings, composefile.Finding{Level: composefile.LevelWarning, Message: warning})
	}
	findings = append(findings, composefile.CheckServerBuild(r, a.dir, a.cfg.Builder)...)
	if t := a.proxyConfig().Tunnel; t != nil && t.TokenSecret == "" {
		for _, name := range sortedKeys(r.Ext) {
			if p := r.Ext[name].Proxy; p != nil && len(p.Hosts) > 0 {
				findings = append(findings, composefile.Finding{Level: composefile.LevelWarning, Service: name,
					Message: "Quick Tunnel requests use a *.trycloudflare.com Host and won't match proxy.hosts; leave hosts empty or set proxy.tunnel.token_secret"})
			}
		}
	}
	px := a.proxyConfig()
	httpOff, httpsOff := portDisabled(px.HTTPPort), portDisabled(px.HTTPSPort)
	for _, name := range sortedKeys(r.Ext) {
		p := r.Ext[name].Proxy
		if p == nil || p.CustomDomainsAllow() == "" {
			continue
		}
		if px.Tunnel != nil {
			findings = append(findings, composefile.Finding{Level: composefile.LevelWarning, Service: name,
				Message: "custom_domains issues Let's Encrypt certificates at the Proxy; behind a Cloudflare Tunnel leave tls and custom_domains off and terminate TLS at Cloudflare"})
		}
		if httpOff && httpsOff {
			findings = append(findings, composefile.Finding{Level: composefile.LevelWarning, Service: name,
				Message: "custom_domains needs a published Proxy port for ACME (HTTP-01 on 80 or TLS-ALPN on 443); http_port and https_port are both 0"})
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
	step.Done(fmt.Sprintf("%s, %s, runtime %s", ui.Count(services, "service", ""), ui.Count(keyCount, "secret key", ""), a.dest.Runtime))
	a.ui.Check(true, 0, offline, services, keyCount)
	return nil
}

func portDisabled(p *int) bool {
	return p != nil && *p == 0
}
