package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/gate"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func init() { extraCommands = append(extraCommands, accessCmd) }

// GateBinary is where `yoho access install` puts the root-owned yoho the
// Gate's sudoers rule allows.
const GateBinary = "/usr/local/bin/yoho"

// sudoersPath is the Gate's sudoers drop-in (no dot: sudo skips such names).
const sudoersPath = "/etc/sudoers.d/yoho-gate"

// accessCmd groups the operator commands of scoped access (ADR 0011). They
// always use the operator path (full SSH with sudo), never the Gate.
func accessCmd(g *globals) *cobra.Command {
	c := &cobra.Command{
		Use:   "access",
		Short: "Share one App Destination with Members through the Gate (operator)",
		Long: `Members (teammates, CI) reach one App Destination through the Gate: a
restricted SSH key of the yoho-gate user whose forced command runs
'yoho gate'. They can deploy, roll back and read status, logs, stats and
Releases; they get no shell, no Docker and no secret values.

  yoho access install                  once per Server: yoho-gate user, sudoers, binary
  yoho access secrets push             store secrets and settings for scoped deploys
  yoho access add alice --verbs all    grant verbs on this App Destination
  yoho access key add alice alice.pub  authorize alice's public key

Members set 'access: scoped' on the Destination (or YOHO_ACCESS=scoped)
and use the usual commands.`,
	}
	c.AddCommand(accessInstallCmd(g), accessAddCmd(g), accessRmCmd(g), accessLsCmd(g), accessKeyCmd(g), accessSecretsCmd(g), accessAuditCmd(g))
	return c
}

// accessHost connects to the Destination's Server as the operator with
// sudo, and checks root is available: Gate state is root-owned.
func (a *app) accessHost(ctx context.Context) (*remote.SSH, error) {
	name := a.dest.Servers[0]
	srv := a.cfg.Servers[name]
	step := a.ui.Step(name, "Connect %s", srv.SSH)
	h, err := remote.NewSSH(name, srv.SSH, true, a.sshOptions())
	if err == nil {
		var id string
		id, err = h.Output(ctx, remote.Cmd{Script: "id -u", Sudo: true})
		if err == nil && strings.TrimSpace(id) != "0" {
			err = errors.New("not root")
		}
	}
	if err != nil {
		if h != nil {
			h.Close()
		}
		step.Fail(err, "`yoho access` needs root or passwordless sudo on the Server (ssh as root, or give "+srv.SSH+" sudo -n)")
		return nil, &silentError{err}
	}
	step.Done()
	return h, nil
}

// requireAppDir fails unless the operator deployed the App Destination.
func (a *app) requireAppDir(ctx context.Context, h remote.Host) error {
	dir := release.AppDir(a.cfg.App, a.destName)
	if err := h.Run(ctx, remote.Cmd{Script: "test -d " + remote.Quote(dir), Sudo: true}); err != nil {
		return a.fail(&hintError{a.cfg.App + "/" + a.destName + " is not deployed on " + h.Name() + " (" + dir + " is missing)", "deploy it once as the operator with `yoho deploy`"})
	}
	return nil
}

func readMembers(ctx context.Context, h remote.Host, app, dest string) (*gate.Members, error) {
	b, err := h.ReadFile(ctx, gate.MembersPath(app, dest), true)
	if errors.Is(err, os.ErrNotExist) {
		return &gate.Members{Members: map[string]gate.Member{}}, nil
	}
	if err != nil {
		return nil, err
	}
	return gate.ParseMembers(b)
}

// writeAccessFile writes a root-owned file in the access directory (0700).
func writeAccessFile(ctx context.Context, h remote.Host, p string, data []byte) error {
	dir := path.Dir(p)
	if err := h.Run(ctx, remote.Cmd{Script: "set -eu\nmkdir -p -m 0700 " + remote.Quote(dir) + "\nchown root:root " + remote.Quote(dir) + "\nchmod 0700 " + remote.Quote(dir), Sudo: true}); err != nil {
		return err
	}
	return h.WriteFile(ctx, p, data, 0o600, true)
}

func accessAddCmd(g *globals) *cobra.Command {
	var verbs string
	c := &cobra.Command{
		Use:   "add MEMBER",
		Short: "Grant a Member verbs on this App Destination",
		Long: "Grant a Member verbs on this App Destination. Verbs: " + strings.Join(gate.GrantableVerbs, ", ") +
			`; "all" is every verb, "read" the read-only ones (releases, ps, logs, stats, dash).
Running it again replaces the Member's verbs.`,
		Example: "  yoho access add alice --verbs all\n  yoho access add ci --verbs deploy,releases -d staging\n  yoho access add bob --verbs read",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.loadOperator(cmd)
			if err != nil {
				return err
			}
			member := args[0]
			if !gate.MemberRe.MatchString(member) {
				return a.fail(&hintError{fmt.Sprintf("invalid Member name %q", member), "lowercase letters, digits, '.', '_' and '-', e.g. alice or ci-github"})
			}
			vs, err := gate.ParseVerbs(verbs)
			if err != nil {
				return a.fail(&hintError{err.Error(), "e.g. --verbs all, --verbs read, --verbs deploy,logs"})
			}
			ctx := cmd.Context()
			h, err := a.accessHost(ctx)
			if err != nil {
				return err
			}
			defer h.Close()
			if err := a.requireAppDir(ctx, h); err != nil {
				return err
			}
			m, err := readMembers(ctx, h, a.cfg.App, a.destName)
			if err != nil {
				return a.fail(err)
			}
			m.Members[member] = gate.Member{Verbs: vs}
			b, err := m.Marshal()
			if err != nil {
				return a.fail(err)
			}
			if err := writeAccessFile(ctx, h, gate.MembersPath(a.cfg.App, a.destName), b); err != nil {
				return a.fail(err)
			}
			a.ui.Finished(nil, fmt.Sprintf("%s may %s on %s/%s\n  next: `yoho access key add %s KEY.pub` and `yoho access secrets push`", member, strings.Join(vs, ", "), a.cfg.App, a.destName, member))
			return nil
		},
	}
	c.Flags().StringVar(&verbs, "verbs", "all", "Comma-separated verbs, all, or read")
	return c
}

func accessRmCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "rm MEMBER",
		Short: "Revoke a Member's verbs on this App Destination (keys stay; see `access key rm`)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.loadOperator(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			h, err := a.accessHost(ctx)
			if err != nil {
				return err
			}
			defer h.Close()
			m, err := readMembers(ctx, h, a.cfg.App, a.destName)
			if err != nil {
				return a.fail(err)
			}
			if _, ok := m.Members[args[0]]; !ok {
				a.ui.Finished(nil, args[0]+" has no access to "+a.cfg.App+"/"+a.destName)
				return nil
			}
			delete(m.Members, args[0])
			b, err := m.Marshal()
			if err != nil {
				return a.fail(err)
			}
			if err := writeAccessFile(ctx, h, gate.MembersPath(a.cfg.App, a.destName), b); err != nil {
				return a.fail(err)
			}
			a.ui.Finished(nil, "Revoked "+args[0]+" on "+a.cfg.App+"/"+a.destName+" (takes effect on the next connection)")
			return nil
		},
	}
}

func accessLsCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List Members of this App Destination and their keys",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.loadOperator(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			h, err := a.accessHost(ctx)
			if err != nil {
				return err
			}
			defer h.Close()
			m, err := readMembers(ctx, h, a.cfg.App, a.destName)
			if err != nil {
				return a.fail(err)
			}
			keys, err := readGateKeys(ctx, h, a.gateUser())
			if err != nil {
				return a.fail(err)
			}
			var rows [][]string
			for _, name := range sortedKeys(m.Members) {
				var fps []string
				for _, k := range keys {
					if k.Member == name {
						fps = append(fps, k.Fingerprint)
					}
				}
				rows = append(rows, []string{name, strings.Join(m.Members[name].Verbs, ","), strings.Join(fps, " ")})
			}
			a.ui.Table([]string{"MEMBER", "VERBS", "KEYS"}, rows)
			return nil
		},
	}
}

// gateKey is one Yoho-managed line of the Gate user's authorized_keys.
type gateKey struct {
	Member      string
	Fingerprint string
	Blob        string // base64 key blob
	Line        string
}

var (
	keyTypes = []string{
		"ssh-ed25519", "sk-ssh-ed25519@openssh.com",
		"ecdsa-sha2-nistp256", "ecdsa-sha2-nistp384", "ecdsa-sha2-nistp521", "sk-ecdsa-sha2-nistp256@openssh.com",
		"ssh-rsa",
	}
	gateLineRe = regexp.MustCompile(`--member ([a-z0-9][a-z0-9._-]*) --key (SHA256:[A-Za-z0-9+/]+)`)
	safeRootRe = regexp.MustCompile(`^/[A-Za-z0-9/._-]*$`)
)

// parsePublicKey reads one OpenSSH public key line (type, base64 blob,
// optional comment) and returns type, blob and its SHA256 fingerprint.
func parsePublicKey(b []byte) (typ, blob, fp string, err error) {
	var line string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			if line != "" {
				return "", "", "", errors.New("the file has more than one key; add them one at a time")
			}
			line = l
		}
	}
	f := strings.Fields(line)
	if len(f) < 2 {
		return "", "", "", errors.New("not an OpenSSH public key (expected `TYPE BASE64 [comment]`, e.g. ~/.ssh/id_ed25519.pub)")
	}
	if !slices.Contains(keyTypes, f[0]) {
		return "", "", "", fmt.Errorf("key type %q is not supported (use ssh-ed25519); options before the key are not allowed", f[0])
	}
	raw, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil || len(raw) < 16 {
		return "", "", "", errors.New("the key's base64 blob is invalid")
	}
	// The blob starts with its own type: a length-prefixed string.
	if n := int(raw[0])<<24 | int(raw[1])<<16 | int(raw[2])<<8 | int(raw[3]); n+4 > len(raw) || string(raw[4:4+n]) != f[0] {
		return "", "", "", errors.New("the key blob does not match its type")
	}
	sum := sha256.Sum256(raw)
	return f[0], f[1], "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:]), nil
}

// gateKeyLine is the authorized_keys line of a Member key: restrict (no
// pty, forwarding or user rc) and a forced command that runs only the Gate.
// The Yoho root is not an argument: the Gate reads it from the root-owned
// GateConfigPath that `yoho access install` writes.
func gateKeyLine(member, typ, blob, fp string) string {
	cmd := "sudo -n " + GateBinary + " gate --member " + member + " --key " + fp
	return `restrict,command="` + cmd + `" ` + typ + " " + blob + " yoho-gate:" + member
}

func gateHome(ctx context.Context, h remote.Host, user string) (string, error) {
	out, err := h.Output(ctx, remote.Cmd{Script: "getent passwd " + remote.Quote(user) + " | cut -d: -f6", Sudo: true})
	if err != nil {
		return "", err
	}
	home := strings.TrimSpace(out)
	if home == "" || !safeRootRe.MatchString(home) {
		return "", &hintError{"the Gate user " + user + " does not exist on " + h.Name(), "run `yoho access install` first"}
	}
	return home, nil
}

func readGateKeys(ctx context.Context, h remote.Host, user string) ([]gateKey, error) {
	home, err := gateHome(ctx, h, user)
	if err != nil {
		return nil, err
	}
	b, err := h.ReadFile(ctx, path.Join(home, ".ssh", "authorized_keys"), true)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []gateKey
	for _, l := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		k := gateKey{Line: l}
		if m := gateLineRe.FindStringSubmatch(l); m != nil {
			k.Member, k.Fingerprint = m[1], m[2]
		}
		for _, t := range keyTypes {
			if i := strings.Index(l, `" `+t+" "); i >= 0 {
				if f := strings.Fields(l[i+2:]); len(f) >= 2 {
					k.Blob = f[1]
				}
			}
		}
		out = append(out, k)
	}
	return out, nil
}

// writeGateKeys writes authorized_keys root-owned (0644 in a 0755 root
// .ssh), so neither yoho-gate nor a Member can add keys or options.
func writeGateKeys(ctx context.Context, h remote.Host, user string, keys []gateKey) error {
	home, err := gateHome(ctx, h, user)
	if err != nil {
		return err
	}
	dir := path.Join(home, ".ssh")
	if err := h.Run(ctx, remote.Cmd{Script: "set -eu\ninstall -d -m 0755 -o root -g root " + remote.Quote(dir), Sudo: true}); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Managed by `yoho access key`. Each key runs only `yoho gate` for its Member.\n")
	for _, k := range keys {
		if strings.HasPrefix(k.Line, "#") {
			continue
		}
		b.WriteString(k.Line + "\n")
	}
	p := path.Join(dir, "authorized_keys")
	if err := h.WriteFile(ctx, p, []byte(b.String()), 0o644, true); err != nil {
		return err
	}
	return h.Run(ctx, remote.Cmd{Script: "chown root:root " + remote.Quote(p), Sudo: true})
}

func accessKeyCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "key", Short: "Authorize or remove Member SSH keys of the Gate user"}
	c.AddCommand(&cobra.Command{
		Use:     "add MEMBER PUBKEY_FILE",
		Short:   "Authorize a Member's public key (restricted to the Gate)",
		Example: "  yoho access key add alice alice.pub\n  ssh-keygen -t ed25519 -N '' -f ci && yoho access key add ci ci.pub",
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.loadOperator(cmd)
			if err != nil {
				return err
			}
			member := args[0]
			if !gate.MemberRe.MatchString(member) {
				return a.fail(&hintError{fmt.Sprintf("invalid Member name %q", member), "lowercase letters, digits, '.', '_' and '-'"})
			}
			raw, err := os.ReadFile(args[1])
			if err != nil {
				return a.fail(err)
			}
			if bytes.Contains(raw, []byte("PRIVATE KEY")) {
				return a.fail(&hintError{args[1] + " is a private key", "pass the .pub file"})
			}
			typ, blob, fp, err := parsePublicKey(raw)
			if err != nil {
				return a.fail(&hintError{args[1] + ": " + err.Error(), "pass an OpenSSH public key file such as ~/.ssh/id_ed25519.pub"})
			}
			ctx := cmd.Context()
			h, err := a.accessHost(ctx)
			if err != nil {
				return err
			}
			defer h.Close()
			keys, err := readGateKeys(ctx, h, a.gateUser())
			if err != nil {
				return a.fail(err)
			}
			// One key belongs to one Member: replace any line with this blob.
			keys = slices.DeleteFunc(keys, func(k gateKey) bool { return k.Blob == blob })
			keys = append(keys, gateKey{Member: member, Fingerprint: fp, Blob: blob, Line: gateKeyLine(member, typ, blob, fp)})
			if err := writeGateKeys(ctx, h, a.gateUser(), keys); err != nil {
				return a.fail(err)
			}
			srv := a.cfg.Servers[a.dest.Servers[0]]
			target, _ := remote.WithUser(srv.SSH, a.gateUser())
			a.ui.Finished(nil, fmt.Sprintf("Authorized %s for %s (%s)\n  the Member connects as %s; grant verbs with `yoho access add %s`", fp, member, typ, target, member))
			return nil
		},
	})
	c.AddCommand(&cobra.Command{
		Use:   "rm MEMBER",
		Short: "Remove every key of a Member",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := g.loadOperator(cmd)
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			h, err := a.accessHost(ctx)
			if err != nil {
				return err
			}
			defer h.Close()
			keys, err := readGateKeys(ctx, h, a.gateUser())
			if err != nil {
				return a.fail(err)
			}
			n := len(keys)
			keys = slices.DeleteFunc(keys, func(k gateKey) bool { return k.Member == args[0] })
			if err := writeGateKeys(ctx, h, a.gateUser(), keys); err != nil {
				return a.fail(err)
			}
			a.ui.Finished(nil, fmt.Sprintf("Removed %d key(s) of %s", n-len(keys), args[0]))
			return nil
		},
	})
	return c
}

func accessSecretsCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "secrets", Short: "Store secrets on the Server for scoped deploys"}
	c.AddCommand(&cobra.Command{
		Use:   "push",
		Short: "Resolve secrets locally and store them, with the Proxy and route settings, for scoped deploys",
		Long: `Resolve the Destination's secrets on this machine (like a deploy) and store
them on the Server as a new secrets generation, plus the operator settings
scoped deploys use instead of the Member's Yoho file: Proxy, Registry
(without its password), retained Releases, and the Proxy hostnames the App
may claim (today's x-yoho.proxy hosts). Members never see the values. Run it
again after changing secrets or hostnames.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.loadOperator(cmd)
			if err != nil {
				return err
			}
			return a.pushAccessSecrets(cmd.Context())
		},
	})
	return c
}

func (a *app) pushAccessSecrets(ctx context.Context) error {
	u := a.ui
	step := u.Step("", "Load compose")
	r, err := a.compose(ctx)
	if err != nil {
		step.Fail(err, "run `yoho config check`")
		return &silentError{err}
	}
	step.Done(fmt.Sprintf("%d services", len(r.Project.Services)))
	step = u.Step("", "Resolve secrets")
	store, err := a.loadSecrets(ctx)
	if err != nil {
		step.Fail(err, "check .yoho/secrets and that op / bw / bws are signed in")
		return &silentError{err}
	}
	svcSecrets, _, err := serviceSecrets(store, r)
	if err != nil {
		step.Fail(err, "")
		return &silentError{err}
	}
	n := 0
	for _, m := range svcSecrets {
		n += len(m)
	}
	step.Done(fmt.Sprintf("%d values for %d services", n, len(svcSecrets)))

	hosts, closeHosts, err := a.connect(ctx)
	if err != nil {
		return err
	}
	defer closeHosts()
	h := hosts[0]
	if err := a.requireAppDir(ctx, h.Host); err != nil {
		return err
	}
	asEnv := map[string]bool{}
	for svc, ext := range r.Ext {
		asEnv[svc] = ext.SecretsAsEnv
	}
	step = u.Step(h.Name, "Store secrets generation")
	gen, err := deploy.WriteSecretsGeneration(ctx, h.Host, a.cfg.App, a.destName, "access-push", svcSecrets, asEnv)
	if err != nil {
		step.Fail(err, "")
		return &silentError{err}
	}
	// Scoped deploys read this generation (not the newest one, which
	// every deploy replaces) until the next push.
	if err := h.Host.WriteFile(ctx, release.AccessSecretsFile(a.cfg.App, a.destName), []byte(gen+"\n"), 0o600, false); err != nil {
		step.Fail(err, "")
		return &silentError{err}
	}
	step.Done(gen)

	settings := gate.Settings{Proxy: a.proxyConfig(), RetainReleases: a.cfg.RetainReleases, Routes: gate.Routes(r.Ext), Server: h.Name}
	if settings.Routes == nil {
		settings.Routes = []string{}
	}
	if a.cfg.Registry != nil {
		reg := *a.cfg.Registry
		reg.PasswordSecret = ""
		settings.Registry = &reg
	}
	sh, err := a.accessHost(ctx)
	if err != nil {
		return err
	}
	defer sh.Close()
	// Limits are the operator's edits to settings.json; keep them.
	if old, err := sh.ReadFile(ctx, gate.SettingsPath(a.cfg.App, a.destName), true); err == nil {
		var prev struct {
			Limits json.RawMessage `json:"limits"`
		}
		if json.Unmarshal(old, &prev) == nil && len(prev.Limits) > 0 {
			if err := json.Unmarshal(prev.Limits, &settings.Limits); err != nil {
				return a.fail(fmt.Errorf("settings.json on the Server: limits: %w", err))
			}
		}
	}
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	step = u.Step(h.Name, "Store settings for scoped deploys")
	if err := writeAccessFile(ctx, sh, gate.SettingsPath(a.cfg.App, a.destName), b); err != nil {
		step.Fail(err, "")
		return &silentError{err}
	}
	step.Done("routes: " + strings.Join(displayRoutes(settings.Routes), ", "))
	u.Finished(nil, "Scoped deploys of "+a.cfg.App+"/"+a.destName+" now use secrets generation "+gen)
	return nil
}

func displayRoutes(r []string) []string {
	if len(r) == 0 {
		return []string{"none"}
	}
	out := make([]string, len(r))
	for i, h := range r {
		if h == "" {
			h = "(catch-all)"
		}
		out[i] = h
	}
	return out
}

func accessAuditCmd(g *globals) *cobra.Command {
	var lines int
	c := &cobra.Command{
		Use:   "audit",
		Short: "Show the last Gate requests of this App Destination (JSON lines)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.loadOperator(cmd)
			if err != nil {
				return err
			}
			if lines < 1 || lines > 100000 {
				return a.fail(&hintError{"--lines must be between 1 and 100000", ""})
			}
			ctx := cmd.Context()
			h, err := a.accessHost(ctx)
			if err != nil {
				return err
			}
			defer h.Close()
			p := gate.AuditPath(a.cfg.App, a.destName)
			return h.Run(ctx, remote.Cmd{Script: fmt.Sprintf("if [ -e %s ]; then tail -n %d %s; fi", remote.Quote(p), lines, remote.Quote(p)), Sudo: true, Stdout: cmd.OutOrStdout()})
		},
	}
	c.Flags().IntVarP(&lines, "lines", "n", 50, "Lines from the end")
	return c
}

func accessInstallCmd(g *globals) *cobra.Command {
	var binary string
	c := &cobra.Command{
		Use:   "install",
		Short: "Set up the Gate on the Server: yoho-gate user, sudoers rule, root-owned yoho binary",
		Long: `Set up the Gate on the Destination's Server (idempotent):

  - installs this yoho version as ` + GateBinary + ` (root-owned, 0755)
  - creates the Gate user (default yoho-gate, gate_user overrides): no
    password, shell /bin/sh, not in the docker group
  - writes ` + sudoersPath + ` so the Gate user may run only
    '` + GateBinary + ` gate ...' as root, checked with visudo -c
  - writes ` + GateConfigPath + ` (root-owned) with the Yoho root the Gate
    serves

Needs root or passwordless sudo.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.loadOperator(cmd)
			if err != nil {
				return err
			}
			return a.installGate(cmd.Context(), binary)
		},
	}
	c.Flags().StringVar(&binary, "binary", "", "Linux yoho binary to install (default: this version)")
	return c
}

func (a *app) installGate(ctx context.Context, binary string) error {
	u := a.ui
	user := a.gateUser()
	h, err := a.accessHost(ctx)
	if err != nil {
		return err
	}
	defer h.Close()

	step := u.Step(h.Name(), "Install %s", GateBinary)
	platform, err := build.ServerPlatform(ctx, h)
	if err != nil {
		step.Fail(err, "")
		return &silentError{err}
	}
	local, err := linuxBinary(ctx, binary, platform, step.Output())
	if err != nil {
		step.Fail(err, "pass --binary with a yoho binary for "+platform)
		return &silentError{err}
	}
	data, err := os.ReadFile(local)
	if err != nil {
		step.Fail(err, "")
		return &silentError{err}
	}
	sum := sha256.Sum256(data)
	want := fmt.Sprintf("%x", sum)
	shaCmd := "sha256sum " + remote.Quote(GateBinary) + " 2>/dev/null | cut -d' ' -f1"
	if got, _ := h.Output(ctx, remote.Cmd{Script: shaCmd, Sudo: true}); strings.TrimSpace(got) != want {
		if err := h.WriteFile(ctx, GateBinary, data, 0o755, true); err != nil {
			step.Fail(err, "")
			return &silentError{err}
		}
	}
	if err := h.Run(ctx, remote.Cmd{Script: "set -eu\nchown root:root " + remote.Quote(GateBinary) + "\nchmod 0755 " + remote.Quote(GateBinary), Sudo: true}); err != nil {
		step.Fail(err, "")
		return &silentError{err}
	}
	if got, _ := h.Output(ctx, remote.Cmd{Script: shaCmd, Sudo: true}); strings.TrimSpace(got) != want {
		err := errors.New("checksum mismatch after upload")
		step.Fail(err, "")
		return &silentError{err}
	}
	step.Done(Version)

	step = u.Step(h.Name(), "Gate user %s", user)
	q := remote.Quote(user)
	script := "set -eu\n" +
		"id -u " + q + " >/dev/null 2>&1 || useradd -m -s /bin/sh " + q + "\n" +
		// '*' disables password login without locking the account ('!'),
		// which OpenSSH would refuse even for keys.
		"usermod -p '*' -s /bin/sh " + q + "\n" +
		"if id -nG " + q + " | tr ' ' '\\n' | grep -qx docker; then gpasswd -d " + q + " docker >/dev/null; fi\n" +
		"home=$(getent passwd " + q + " | cut -d: -f6)\n" +
		"chown " + q + ": \"$home\"\nchmod 0750 \"$home\"\n" +
		"install -d -m 0755 -o root -g root \"$home/.ssh\"\n" +
		"[ -e \"$home/.ssh/authorized_keys\" ] || install -m 0644 -o root -g root /dev/null \"$home/.ssh/authorized_keys\"\n"
	if err := h.Run(ctx, remote.Cmd{Script: script, Sudo: true}); err != nil {
		step.Fail(err, "")
		return &silentError{err}
	}
	step.Done()

	step = u.Step(h.Name(), "sudoers %s", sudoersPath)
	rule := sudoersRule(user)
	tmp := sudoersPath + ".yoho-new" // the dot keeps sudo from reading it early
	script = "set -eu\numask 0337\nt=" + remote.Quote(tmp) + "\ntrap 'rm -f \"$t\"' EXIT\ncat > \"$t\"\nchown root:root \"$t\"\nchmod 0440 \"$t\"\n" +
		"visudo -cf \"$t\" >/dev/null\nmv -f \"$t\" " + remote.Quote(sudoersPath) + "\ntrap - EXIT\nvisudo -c >/dev/null\n"
	if err := h.Run(ctx, remote.Cmd{Script: script, Stdin: strings.NewReader(rule), Sudo: true}); err != nil {
		step.Fail(err, "check that sudo is installed and /etc/sudoers includes /etc/sudoers.d")
		return &silentError{err}
	}
	step.Done()
	step = u.Step(h.Name(), "Gate config %s", GateConfigPath)
	conf, err := json.Marshal(GateConfig{Root: a.root()})
	if err != nil {
		return err
	}
	script = "set -eu\numask 0022\nd=" + remote.Quote(path.Dir(GateConfigPath)) + "\nf=" + remote.Quote(GateConfigPath) + "\n" +
		"install -d -m 0755 -o root -g root \"$d\"\n" +
		"t=\"$f.yoho-new\"\ntrap 'rm -f \"$t\"' EXIT\ncat > \"$t\"\nchown root:root \"$t\"\nchmod 0644 \"$t\"\nmv -f \"$t\" \"$f\"\ntrap - EXIT\n"
	if err := h.Run(ctx, remote.Cmd{Script: script, Stdin: bytes.NewReader(conf), Sudo: true}); err != nil {
		step.Fail(err, "")
		return &silentError{err}
	}
	step.Done(a.root())
	u.Finished(nil, "Gate installed on "+h.Name()+"\n  next: `yoho access secrets push`, `yoho access add MEMBER`, `yoho access key add MEMBER KEY.pub`")
	return nil
}

// sudoersRule lets the Gate user run only the Gate binary, as root. The
// argument wildcard is safe: the forced command fixes the arguments, and
// the binary only serves the Gate under this subcommand.
// env_reset and secure_path keep the caller's environment (DOCKER_HOST,
// PATH) away from the root Gate whatever the Server's sudo defaults are.
func sudoersRule(user string) string {
	return "# Managed by yoho (`yoho access install`): Members run only the Gate.\n" +
		"Defaults:" + user + " !requiretty, !use_pty, env_reset\n" +
		"Defaults:" + user + " secure_path=\"" + remote.SafePath + "\"\n" +
		"Defaults:" + user + " env_keep = \"SSH_CONNECTION\"\n" +
		user + " ALL=(root) NOPASSWD: " + GateBinary + " gate *\n"
}
