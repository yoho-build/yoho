package cli

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/yoho-build/yoho/internal/dash"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
	"github.com/yoho-build/yoho/internal/tui"
	"github.com/yoho-build/yoho/internal/webui"
)

func init() { extraCommands = append(extraCommands, tuiCmd, uiCmd) }

// dashSource connects to every Server of the Destination and returns the
// read-only collector the dashboards share.
func (a *app) dashSource(cmd *cobra.Command, interval time.Duration) (*dash.Live, func(), error) {
	if a.scoped {
		return a.scopedDashSource(cmd, interval)
	}
	hosts, closeHosts, err := a.connect(cmd.Context())
	if err != nil {
		return nil, nil, err
	}
	src := &dash.Source{
		App: a.cfg.App, Destination: a.destName, Runtime: a.dest.Runtime, Root: a.root(),
		Releases: func(ctx context.Context, h remote.Host) ([]release.Release, error) {
			return deploy.ListReleases(ctx, h, a.cfg.App, a.destName)
		},
	}
	if src.Runtime == "" {
		src.Runtime = "compose"
	}
	for _, h := range hosts {
		src.Targets = append(src.Targets, dash.Target{Name: h.Name, SSH: h.Server.SSH, Host: h.Host})
	}
	// Keep a little under the refresh interval so each tick collects anew.
	return &dash.Live{Src: src, MaxAge: interval * 4 / 5}, closeHosts, nil
}

func checkInterval(d time.Duration) error {
	if d < time.Second {
		return &hintError{"--interval must be at least 1s", "e.g. --interval 5s"}
	}
	return nil
}

func tuiCmd(g *globals) *cobra.Command {
	var interval time.Duration
	var lines int
	c := &cobra.Command{
		Use:   "tui",
		Short: "Live terminal dashboard of the Destination's Servers, Services and Releases",
		Long: `Live, read-only terminal dashboard of the Destination: host facts (load,
memory, disk, Docker), the App's containers with health and resource usage,
the Proxy, Release history, and container logs.

Everything is read over SSH; nothing is installed or left running on the
Servers. Keys: tab/←→ switch Server, ↑↓ select, d details, enter logs,
v disk (App directory and volume sizes), r refresh, q quit. Details show CPU, memory, network and disk I/O, and PIDs;
esc returns to the overview.`,
		Example: "  yoho tui\n  yoho tui -d staging --interval 10s",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			if err := checkInterval(interval); err != nil {
				return a.fail(err)
			}
			if g.json || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
				return a.fail(&hintError{"yoho tui needs an interactive terminal", "use `yoho ui` for a browser, or `yoho app ps` / `yoho --json app stats` in scripts"})
			}
			live, closeHosts, err := a.dashSource(cmd, interval)
			if err != nil {
				return err
			}
			defer closeHosts()
			return tui.Run(cmd.Context(), live, os.Stdin, os.Stdout, tui.Options{Interval: interval, Lines: lines})
		},
	}
	c.Flags().DurationVar(&interval, "interval", 5*time.Second, "Refresh interval")
	c.Flags().IntVarP(&lines, "lines", "n", 500, "Log lines to show")
	return c
}

func uiCmd(g *globals) *cobra.Command {
	var interval time.Duration
	var port int
	var noOpen bool
	c := &cobra.Command{
		Use:   "ui",
		Short: "Local web dashboard of the Destination's Servers, Services and Releases",
		Long: `Serve a read-only web dashboard of the Destination on 127.0.0.1: host facts
(load, memory, disk, Docker), the App's containers with health and resource
usage, the Proxy, Release history, and container logs.

The dashboard runs on this machine and reads the Servers over SSH; nothing is
installed or left running on them. It listens on loopback only and is opened
with a one-time login link (printed, and opened in your browser unless
--no-open). Stop it with Ctrl-C.`,
		Example: "  yoho ui\n  yoho ui -d staging --port 7300 --no-open",
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.load(cmd)
			if err != nil {
				return err
			}
			if err := checkInterval(interval); err != nil {
				return a.fail(err)
			}
			if port < 0 || port > 65535 {
				return a.fail(&hintError{fmt.Sprintf("--port %d is out of range", port), "use 0 for a free port, or 1-65535"})
			}
			ln, err := webui.Listen(port)
			if err != nil {
				return a.fail(&hintError{"listen on 127.0.0.1: " + err.Error(), "pick another --port, or 0 for a free one"})
			}
			defer ln.Close()
			live, closeHosts, err := a.dashSource(cmd, interval)
			if err != nil {
				return err
			}
			defer closeHosts()
			srv := webui.New(live, webui.Meta{Version: Version, Interval: int(interval / time.Second)}, webui.Assets())
			url := srv.LoginURL(ln.Addr())
			a.ui.Info("Yoho UI for %s · %s on http://%s (read-only, Ctrl-C to stop)", a.cfg.App, a.destName, ln.Addr())
			a.ui.Info("Open (one-time link): %s", url)
			if !noOpen && !g.json {
				cleanup, err := openBrowser(url)
				if err != nil {
					a.ui.Warn("cannot open a browser (%v); open the link above", err)
				}
				defer cleanup()
			}
			return srv.Serve(cmd.Context(), ln)
		},
	}
	c.Flags().DurationVar(&interval, "interval", 5*time.Second, "Browser refresh interval")
	c.Flags().IntVar(&port, "port", 0, "Port on 127.0.0.1 (0 picks a free port)")
	c.Flags().BoolVar(&noOpen, "no-open", false, "Do not open a browser")
	return c
}

// openBrowser opens url through a private redirect file, so the one-time
// login code is never in a process argv where other local users can read
// it. The returned cleanup removes the file; it is also removed after a
// minute, once the browser has read it.
func openBrowser(url string) (func(), error) {
	noop := func() {}
	var name string
	switch runtime.GOOS {
	case "darwin":
		name = "open"
	case "linux", "freebsd", "openbsd", "netbsd":
		name = "xdg-open"
	default:
		return noop, errors.New("unsupported OS " + runtime.GOOS)
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		return noop, err
	}
	dir, err := os.MkdirTemp("", "yoho-ui-") // 0700
	if err != nil {
		return noop, err
	}
	var once sync.Once
	cleanup := func() { once.Do(func() { os.RemoveAll(dir) }) }
	file := filepath.Join(dir, "open.html")
	u := html.EscapeString(url)
	page := `<!doctype html><meta charset="utf-8"><meta name="referrer" content="no-referrer">` +
		`<meta http-equiv="refresh" content="0;url=` + u + `"><title>Yoho</title><a href="` + u + `">Open Yoho</a>`
	if err := os.WriteFile(file, []byte(page), 0o600); err != nil {
		cleanup()
		return noop, err
	}
	c := exec.Command(bin, file)
	if err := c.Start(); err != nil {
		cleanup()
		return noop, err
	}
	go func() {
		c.Wait()
		time.Sleep(time.Minute)
		cleanup()
	}()
	return cleanup, nil
}
