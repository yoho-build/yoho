package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/yoho-build/yoho/internal/build"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/remote"
)

// minContainerVersion is the oldest Apple container release `yoho builder
// setup` accepts; older installs are offered an upgrade.
const minContainerVersion = "1.5.0"

const containerReleaseAPI = "https://api.github.com/repos/apple/container/releases/latest"

func init() {
	extraCommands = append(extraCommands, builderCmd)
}

func builderCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "builder", Short: "Inspect and set up the local Builder (Apple container on Apple silicon Macs)"}
	c.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show the build engine Yoho will use and the state of Apple container",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return builderStatus(cmd, g)
		},
	})
	var yes bool
	setup := &cobra.Command{
		Use:   "setup",
		Short: "Install or upgrade Apple container, Rosetta and the builder VM, then smoke-test a linux/amd64 build",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return builderSetup(cmd, g, yes)
		},
	}
	setup.Flags().BoolVarP(&yes, "yes", "y", false, "Run every step without asking (including sudo installer)")
	c.AddCommand(setup)
	c.AddCommand(&cobra.Command{
		Use:   "check",
		Short: "Check that a remote Builder can ship images straight to each Server, and print setup steps for the ones it cannot",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return builderCheck(cmd, g)
		},
	})
	return c
}

// builderCheck probes, from the remote Builder, every Server of the
// Destination (the check behind direct ship). It never installs keys or
// changes the Builder; failures come with commands the operator runs.
func builderCheck(cmd *cobra.Command, g *globals) error {
	a, err := g.load(cmd)
	if err != nil {
		return err
	}
	u, b := a.ui, a.cfg.Builder
	if b.Location != "remote" {
		u.Info("Direct ship only applies to remote Builders (builder.location: remote); this App builds with location %s.", orDefault(b.Location, "local"))
		return nil
	}
	ctx := cmd.Context()
	if b.Direct.Mode() == config.DirectOff {
		u.Warn("builder.direct is false: deploys ship through this machine whatever this check finds")
	}
	step := u.Step("builder", "Connect %s", b.Remote)
	bh, err := remote.NewSSH("builder", b.Remote, false, remote.SSHOptions{})
	if err == nil {
		_, err = bh.Output(ctx, remote.Cmd{Script: "true"})
	}
	if err != nil {
		step.Fail(err, "check `"+sshCommand(b.Remote)+"` works without a password")
		return &silentError{err}
	}
	defer bh.Close()
	step.Done()

	rows := [][]string{}
	var details, hints []string
	failed := 0
	for _, name := range a.dest.Servers {
		srv := a.cfg.Servers[name]
		row := []string{name, "-", "ok"}
		h, err := remote.NewSSH(name, srv.SSH, srv.Sudo, a.sshOptions())
		if err != nil {
			return err
		}
		hop, hasHop := h.Hop(ctx)
		if hasHop {
			row[1] = hop.Target()
		}
		if perr := build.DirectProbe(ctx, bh, h); perr != nil {
			failed++
			row[2] = "error"
			details = append(details, fmt.Sprintf("%s: %v", name, perr))
			hints = append(hints, directSetupHints(name, b.Remote, srv.SSH, hop, hasHop)...)
		}
		h.Close()
		rows = append(rows, row)
	}
	u.Table([]string{"SERVER", "HOP TARGET", "RESULT"}, rows)
	for _, d := range details {
		u.Warn("%s", d)
	}
	if failed == 0 {
		u.Info("The Builder %s can ship images directly to every Server.", b.Remote)
		return nil
	}
	for _, h := range hints {
		u.Info("%s", h)
	}
	err = fmt.Errorf("direct ship is not ready for %d of %d Server(s)", failed, len(rows))
	u.Error(err, "")
	return &silentError{err}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// sshCommand is the ssh command line that reaches target ([user@]host[:port]).
func sshCommand(target string) string {
	user, host, port, err := remote.ParseTarget(target)
	if err != nil {
		return "ssh " + remote.Quote(target)
	}
	cmd := "ssh "
	if port != 0 {
		cmd += "-p " + strconv.Itoa(port) + " "
	}
	if user != "" {
		host = user + "@" + host
	}
	return cmd + host
}

// directSetupHints are the commands the operator runs to let the Builder ssh
// to one Server without a prompt. Yoho prints them and never runs them: it
// does not create, copy or install keys. builder and server are the
// targets as the operator reaches them; hop is what the Builder connects to.
// A Server without a hop (hasHop false, Cloudflare Tunnel) cannot use direct ship.
func directSetupHints(name, builder, server string, hop remote.Hop, hasHop bool) []string {
	if !hasHop {
		return []string{"", name + ": reached through a Cloudflare Tunnel, which the Builder lacks, so it cannot use direct ship; images for it ship through this machine."}
	}
	b := sshCommand(builder)
	scan := "ssh-keyscan -p " + strconv.Itoa(hop.Port) + " " + hop.Host
	known := hop.Host
	if hop.Port != 22 {
		known = "[" + hop.Host + "]:" + strconv.Itoa(hop.Port)
	}
	lines := []string{"",
		"To let the Builder reach " + name + " (" + hop.Target() + ") without a prompt, run these yourself (Yoho never installs keys):",
		"  1. Create a key on the Builder if it has none:",
		"       " + b + " " + remote.Quote(`mkdir -p -m 700 ~/.ssh && { test -f ~/.ssh/id_ed25519 || ssh-keygen -t ed25519 -N "" -f ~/.ssh/id_ed25519; }`),
		"  2. Authorize it on " + name + ":",
		"       " + b + " 'cat ~/.ssh/id_ed25519.pub' | " + sshCommand(server) + " 'mkdir -p ~/.ssh && cat >> ~/.ssh/authorized_keys'",
		"  3. Check the host key of " + hop.Host + " against the fingerprint your machine trusts, then add it to the Builder's known_hosts:",
		"       ssh-keygen -l -F " + remote.Quote(known) + "   # on your machine: the fingerprint you trust",
		"       " + b + " " + remote.Quote(scan+" 2>/dev/null | ssh-keygen -lf -") + "   # must match",
		"       " + b + " " + remote.Quote(scan+" >> ~/.ssh/known_hosts"),
		"  4. Run `yoho builder check` again.",
	}
	return lines
}

// builderConfig is the Yoho file's builder section, or defaults when there
// is no Yoho file here.
func builderConfig(cmd *cobra.Command, g *globals) (config.Builder, string) {
	a, err := g.load(cmd)
	if err != nil {
		return config.Builder{}, "no Yoho file; defaults"
	}
	return a.cfg.Builder, filepath.Base(a.path)
}

func cmdOutput(ctx context.Context, name string, args ...string) (string, error) {
	var out bytes.Buffer
	c := exec.CommandContext(ctx, name, args...)
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	return strings.TrimSpace(out.String()), err
}

func osBuildExec(ctx context.Context, c build.Command) error {
	x := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	x.Env = append(os.Environ(), c.Env...)
	x.Stdin, x.Stdout, x.Stderr = c.Stdin, c.Stdout, c.Stderr
	return x.Run()
}

var versionRE = regexp.MustCompile(`\d+\.\d+\.\d+`)

// containerVersion parses `container --version` ("container CLI version 1.0.0 ...").
func containerVersion(ctx context.Context) (string, error) {
	out, err := cmdOutput(ctx, "container", "--version")
	if err != nil {
		return "", err
	}
	v := versionRE.FindString(out)
	if v == "" {
		return "", fmt.Errorf("unrecognized version %q", out)
	}
	return v, nil
}

// versionLess compares dotted numeric versions (1.10.0 is newer than 1.5.0).
func versionLess(a, b string) bool { return build.VersionLess(a, b) }

// containerStatusValue is the "container" row: installed version, latest
// release when GitHub answered, and an upgrade hint when the install is older.
func containerStatusValue(installed string, installedOK bool, latest string, latestOK bool) string {
	cv := "not installed"
	if installedOK {
		cv = installed
	}
	if !latestOK {
		if installedOK && versionLess(installed, minContainerVersion) {
			cv += " (older than " + minContainerVersion + "; run `yoho builder setup`)"
		}
		return cv + " · latest unknown (offline?)"
	}
	cv += " · latest " + latest
	if installedOK && versionLess(installed, latest) {
		cv += "; run `yoho builder setup` to upgrade (needs sudo)"
	} else if installedOK && versionLess(installed, minContainerVersion) {
		cv += " (older than " + minContainerVersion + "; run `yoho builder setup`)"
	}
	return cv
}

type containerRelease struct {
	Version string
	PkgURL  string
	PkgName string
}

// latestContainerRelease asks GitHub for the newest signed installer.
// Short timeout: status must work offline.
func latestContainerRelease(ctx context.Context) (containerRelease, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, containerReleaseAPI, nil)
	if err != nil {
		return containerRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return containerRelease{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return containerRelease{}, fmt.Errorf("GitHub API: %s", resp.Status)
	}
	var r struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&r); err != nil {
		return containerRelease{}, err
	}
	rel := containerRelease{Version: strings.TrimPrefix(r.Tag, "v")}
	for _, a := range r.Assets {
		if strings.HasSuffix(a.Name, "-installer-signed.pkg") {
			rel.PkgName, rel.PkgURL = a.Name, a.URL
		}
	}
	if rel.PkgURL == "" {
		return rel, fmt.Errorf("release %s has no signed installer", r.Tag)
	}
	return rel, nil
}

func rosettaInstalled(ctx context.Context) bool {
	if exec.CommandContext(ctx, "/usr/bin/pgrep", "-q", "oahd").Run() == nil {
		return true
	}
	return exec.CommandContext(ctx, "/usr/bin/arch", "-x86_64", "/usr/bin/true").Run() == nil
}

// builderVMStatus is "running", "stopped" or an error description.
func builderVMStatus(ctx context.Context) string {
	out, err := cmdOutput(ctx, "container", "builder", "status")
	switch {
	case strings.Contains(out, "not running"):
		return "stopped"
	case err == nil && strings.Contains(out, "running"):
		// ID IMAGE STATE IP CPUS MEMORY
		lines := strings.Split(out, "\n")
		if f := strings.Fields(lines[len(lines)-1]); len(f) >= 6 {
			return "running · " + f[4] + " CPUs, " + strings.Join(f[5:], " ")
		}
		return "running"
	case err != nil:
		return "unknown (" + firstLine(out) + ")"
	}
	return firstLine(out)
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}

func builderStatus(cmd *cobra.Command, g *globals) error {
	ctx := cmd.Context()
	u := g.ui(cmd)
	cfg, src := builderConfig(cmd, g)
	engine, reason := build.ResolveEngine(cfg, runtime.GOOS, runtime.GOARCH, exec.LookPath,
		func() error { return build.ContainerStatus(ctx, osBuildExec) })
	rows := [][]string{
		{"config", src},
		{"engine", engine},
		{"reason", reason},
	}
	if runtime.GOOS == "darwin" {
		ver, verr := containerVersion(ctx)
		latest, lerr := latestContainerRelease(ctx)
		rows = append(rows, []string{"container", containerStatusValue(ver, verr == nil, latest.Version, lerr == nil)})
		if verr == nil {
			svc := "running"
			if err := build.ContainerStatus(ctx, osBuildExec); err != nil {
				svc = "stopped (container system start)"
			}
			rows = append(rows, []string{"system service", svc}, []string{"builder VM", builderVMStatus(ctx)})
		}
		if runtime.GOARCH == "arm64" {
			r := "installed"
			if !rosettaInstalled(ctx) {
				r = "missing (needed for linux/amd64 builds; run `yoho builder setup`)"
			}
			rows = append(rows, []string{"rosetta", r})
		}
	}
	u.Table([]string{"ITEM", "VALUE"}, rows)
	return nil
}

// confirm asks a yes/no question on the terminal; --yes answers yes.
func confirm(cmd *cobra.Command, yes bool, question string) (bool, error) {
	if yes {
		return true, nil
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, errors.New("not a terminal; rerun with --yes to accept: " + question)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s [y/N] ", question)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes", nil
}

// interactive runs a command attached to the terminal (sudo prompts).
func interactive(ctx context.Context, argv ...string) error {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stderr, os.Stderr
	return c.Run()
}

func runStep(ctx context.Context, w io.Writer, argv ...string) error {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...)
	c.Stdout, c.Stderr = w, w
	return c.Run()
}

func builderSetup(cmd *cobra.Command, g *globals, yes bool) error {
	ctx := cmd.Context()
	u := g.ui(cmd)
	fail := func(s interface{ Fail(error, string) }, err error, hint string) error {
		s.Fail(err, hint)
		return &silentError{err}
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		err := fmt.Errorf("Apple container needs an Apple silicon Mac (this is %s/%s)", runtime.GOOS, runtime.GOARCH)
		u.Info("%v; Yoho builds with Docker here.", err)
		return nil
	}

	// 1. Apple container itself.
	step := u.Step("", "Check Apple container (>= %s)", minContainerVersion)
	ver, verr := containerVersion(ctx)
	if verr == nil && !versionLess(ver, minContainerVersion) {
		step.Done(ver)
	} else {
		have := "not installed"
		if verr == nil {
			have = ver
		}
		rel, err := latestContainerRelease(ctx)
		if err != nil {
			return fail(step, err, "download the signed .pkg from https://github.com/apple/container/releases and install it")
		}
		step.Done(have + ", latest " + rel.Version)
		q := fmt.Sprintf("Install Apple container %s (download %s, then `sudo installer -pkg ... -target /`)?", rel.Version, rel.PkgName)
		if verr == nil {
			q = fmt.Sprintf("Upgrade Apple container %s -> %s (`container system stop`, download %s, then `sudo installer -pkg ... -target /`)?", ver, rel.Version, rel.PkgName)
		}
		ok, err := confirm(cmd, yes, q)
		if err != nil || !ok {
			if err == nil {
				err = errors.New("declined")
			}
			s := u.Step("", "Install Apple container %s", rel.Version)
			return fail(s, err, "install it yourself from "+rel.PkgURL+", then rerun `yoho builder setup`")
		}
		s := u.Step("", "Install Apple container %s", rel.Version)
		if err := installContainer(ctx, rel, verr == nil, s.Output()); err != nil {
			return fail(s, err, "install it yourself from "+rel.PkgURL)
		}
		s.Done()
	}

	// 2. Rosetta runs linux/amd64 build steps on Apple silicon.
	step = u.Step("", "Check Rosetta")
	if rosettaInstalled(ctx) {
		step.Done("installed")
	} else {
		step.Done("missing")
		ok, err := confirm(cmd, yes, "Install Rosetta (`sudo softwareupdate --install-rosetta --agree-to-license`)?")
		s := u.Step("", "Install Rosetta")
		if err != nil || !ok {
			if err == nil {
				err = errors.New("declined")
			}
			return fail(s, err, "linux/amd64 images need Rosetta: softwareupdate --install-rosetta --agree-to-license")
		}
		if err := interactive(ctx, "sudo", "softwareupdate", "--install-rosetta", "--agree-to-license"); err != nil {
			return fail(s, err, "run `softwareupdate --install-rosetta --agree-to-license` yourself")
		}
		s.Done()
	}

	// 3. System service.
	step = u.Step("", "Start container system service")
	if build.ContainerStatus(ctx, osBuildExec) == nil {
		step.Done("already running")
	} else if err := runStep(ctx, step.Output(), "container", "system", "start", "--enable-kernel-install"); err != nil {
		return fail(step, err, "run `container system start` and follow its prompts")
	} else {
		step.Done()
	}

	// 4. Builder VM sized to half the CPUs and a quarter of RAM.
	cpus, mem := builderResources(ctx)
	step = u.Step("", "Start builder VM (%d CPUs, %s)", cpus, mem)
	if st := builderVMStatus(ctx); strings.HasPrefix(st, "running") {
		step.Done("already " + st + "; `container builder stop` then rerun to resize")
	} else if err := runStep(ctx, step.Output(), "container", "builder", "start", "--cpus", strconv.Itoa(cpus), "--memory", mem); err != nil {
		return fail(step, err, "run `container builder start` to see the error")
	} else {
		step.Done()
	}

	// 5. Smoke build: a linux/amd64 RUN step must see x86_64.
	step = u.Step("", "Smoke-build a linux/amd64 image")
	arch, err := smokeBuild(ctx, step.Output())
	if err != nil {
		return fail(step, err, "check `container builder status` and Rosetta; set builder.engine: docker to keep using Docker")
	}
	if arch != "x86_64" {
		err := fmt.Errorf("linux/amd64 build ran as %q, want x86_64", arch)
		return fail(step, err, "install Rosetta and restart the builder: container builder stop && yoho builder setup")
	}
	step.Done("uname -m = x86_64")
	u.Info("Apple container is ready; Yoho builds locally with it (builder.engine: auto).")
	return nil
}

func installContainer(ctx context.Context, rel containerRelease, running bool, w io.Writer) error {
	dir, err := os.MkdirTemp("", "yoho-container-pkg-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	pkg := filepath.Join(dir, rel.PkgName)
	if err := download(ctx, rel.PkgURL, pkg); err != nil {
		return err
	}
	// installer verifies too; fail early with a clear message.
	if out, err := cmdOutput(ctx, "/usr/sbin/pkgutil", "--check-signature", pkg); err != nil || !strings.Contains(out, "Status: signed") {
		return fmt.Errorf("%s is not signed: %s", rel.PkgName, firstLine(out))
	}
	if running {
		// Upgrades require the old services stopped.
		if err := runStep(ctx, w, "container", "system", "stop"); err != nil {
			return fmt.Errorf("container system stop: %w", err)
		}
	}
	if err := interactive(ctx, "sudo", "/usr/sbin/installer", "-pkg", pkg, "-target", "/"); err != nil {
		return fmt.Errorf("installer: %w", err)
	}
	return nil
}

func download(ctx context.Context, url, dst string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: %s", url, resp.Status)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// builderResources is half the CPUs (>= 2) and a quarter of RAM (>= 2 GiB).
func builderResources(ctx context.Context) (int, string) {
	cpus := runtime.NumCPU() / 2
	if cpus < 2 {
		cpus = 2
	}
	memMiB := int64(2048)
	if out, err := cmdOutput(ctx, "/usr/sbin/sysctl", "-n", "hw.memsize"); err == nil {
		if b, err := strconv.ParseInt(out, 10, 64); err == nil && b/4/(1<<20) > memMiB {
			memMiB = b / 4 / (1 << 20)
		}
	}
	return cpus, strconv.FormatInt(memMiB, 10) + "M"
}

// smokeBuild builds a throwaway linux/amd64 image and returns what `uname -m`
// printed inside the build.
func smokeBuild(ctx context.Context, w io.Writer) (string, error) {
	dir, err := os.MkdirTemp("", "yoho-smoke-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	df := "FROM alpine\nRUN echo \"yoho-arch=$(uname -m)\"\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(df), 0o644); err != nil {
		return "", err
	}
	const ref = "yoho/builder-smoke:latest"
	var out bytes.Buffer
	err = runStep(ctx, io.MultiWriter(&out, w), "container", "build", "--progress", "plain", "--no-cache", "--platform", "linux/amd64", "--tag", ref, dir)
	_ = exec.CommandContext(ctx, "container", "image", "delete", ref).Run()
	if err != nil {
		return "", err
	}
	m := regexp.MustCompile(`yoho-arch=(\S+)`).FindAllStringSubmatch(out.String(), -1)
	for _, x := range m {
		if !strings.Contains(x[1], "$(") {
			return x[1], nil
		}
	}
	return "", errors.New("build output did not show uname -m")
}
