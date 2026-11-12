package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
)

func init() { extraCommands = append(extraCommands, previewCmd) }

func previewCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "preview", Short: "Deploy and tear down ephemeral Destinations"}
	var name string
	var o applyOptions
	up := &cobra.Command{Use: "up", Short: "Build and deploy a preview from previews.from", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.loadPreview(cmd, name)
			if err != nil {
				return err
			}
			return a.deploy(cmd.Context(), o)
		}}
	up.Flags().StringVar(&name, "name", "", "Preview name (for example pr-123)")
	up.Flags().StringVar(&o.Version, "version", "", "Version to deploy (default: git SHA)")
	up.Flags().BoolVar(&o.SkipBuild, "skip-build", false, "Use images already built for this preview Version")
	addLockWaitFlag(up)
	var yes bool
	down := &cobra.Command{Use: "down", Short: "Remove a marked preview, including its volumes", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) (err error) {
			u := g.ui(cmd)
			defer func() {
				if err != nil {
					u.Finished(err, "")
				}
			}()
			a, err := g.loadPreview(cmd, name)
			if err != nil {
				return err
			}
			u = a.ui
			if err := checkRemoveConfirm(removeFlags{yes: yes}, a.cfg.App, isTerminal(cmd.InOrStdin()), g.json); err != nil {
				return a.fail(err)
			}
			if !yes {
				a.ui.Info("Remove preview %s and all its volumes?", a.previewName)
				if err := askRemove(cmd.InOrStdin(), cmd.OutOrStdout(), removeFlags{}, a.cfg.App, 0); err != nil {
					return err
				}
			}
			ctx := cmd.Context()
			hosts, closeHosts, err := a.connect(ctx)
			if err != nil {
				return err
			}
			defer closeHosts()
			h := hosts[0]
			step := a.ui.Step(h.Name, "Remove preview %s and its volumes", a.previewName)
			if _, err := deploy.CheckPreviewDir(ctx, h.Host, a.cfg.App, a.destName); err != nil {
				step.Fail(err, "only marked previews or empty preview directories can be removed")
				return &silentError{err}
			}
			unlock, err := a.acquireLock(ctx, h.Host, deploy.LockInfo{Command: "preview down"})
			if err != nil {
				step.Fail(err, hintFor(err, ""))
				return &silentError{err}
			}
			defer func() { _ = unlock(context.WithoutCancel(ctx)) }()
			p, err := deploy.RemovePreview(ctx, h.Host, a.cfg.App, a.destName, h.Server.Sudo)
			if err != nil {
				step.Fail(err, "only marked previews can be removed; rerun to finish an interrupted teardown")
				return &silentError{err}
			}
			hostsOut := []string{}
			if p == nil {
				step.Done("preview is already absent")
			} else {
				hostsOut = p.Hosts
				step.Done()
			}
			a.ui.Event("preview.down", "", map[string]any{"name": a.previewName, "hosts": hostsOut})
			a.ui.Finished(nil, "Preview "+a.previewName+" removed")
			return nil
		}}
	down.Flags().StringVar(&name, "name", "", "Preview name (for example pr-123)")
	down.Flags().BoolVarP(&yes, "yes", "y", false, "Confirm removal of the preview and its volumes")
	addLockWaitFlag(down)
	list := &cobra.Command{Use: "list", Short: "List preview markers on the template's Servers", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.loadPreviewTemplate(cmd)
			if err != nil {
				return err
			}
			hosts, closeHosts, err := a.connect(cmd.Context())
			if err != nil {
				return err
			}
			defer closeHosts()
			var rows [][]string
			for _, h := range hosts {
				previews, err := deploy.ListPreviews(cmd.Context(), h.Host, a.cfg.App)
				if err != nil {
					return err
				}
				for _, p := range previews {
					if _, configured := a.cfg.Destinations[p.Destination]; configured {
						continue
					}
					rows = append(rows, []string{p.Name, h.Name, strings.Join(p.Hosts, ", "), p.Version})
				}
			}
			a.ui.Table([]string{"NAME", "SERVER", "HOSTS", "VERSION"}, rows)
			return nil
		}}
	c.AddCommand(up, down, list)
	return c
}

// Load the template's Yoho overlay before deriving the runtime Destination.
// previews.from in the effective configuration selects the template.
func (g *globals) loadPreviewTemplate(cmd *cobra.Command) (*app, error) {
	// Start with the same selected Destination overlay as ordinary loading;
	// the previews block can be supplied or corrected there.
	initial := *g
	if initial.destination == "" && os.Getenv("YOHO_DESTINATION") == "" {
		file, err := g.yohoFile()
		if err != nil {
			return nil, err
		}
		base, err := config.Load(file)
		if err != nil {
			return nil, err
		}
		if base.Previews != nil {
			// Keep ordinary overlay loading available to correct an invalid base ref.
			if _, exists := base.Destinations[base.Previews.From]; exists {
				initial.destination = base.Previews.From
			}
		}
	}
	selected, err := initial.loadApp(cmd)
	if err != nil {
		return nil, err
	}
	settings := selected.cfg.Previews
	if settings == nil {
		return nil, errors.New("previews is required; configure previews.from and previews.host")
	}
	cfg, name, overlay, err := config.LoadForDestination(selected.path, settings.From)
	if err != nil {
		return nil, err
	}
	// Preserve the selected overlay's preview settings while applying the
	// template overlay to its Servers, compose inputs and Destination settings.
	cfg.Previews = settings
	if errs := config.Validate(cfg); len(errs) != 0 {
		return nil, fmt.Errorf("invalid preview template: %w", errors.Join(errs...))
	}
	dest, err := cfg.Dest(name)
	if err != nil {
		return nil, err
	}
	a := *selected
	a.g = g
	a.cfg, a.destName, a.dest, a.overlay = cfg, name, dest, overlay
	if a.scoped, err = scopedAccess(dest, os.Getenv); err != nil {
		return nil, err
	}
	if a.scoped && !scopedCommand(cmd) {
		return nil, a.fail(errors.New("preview commands require operator access"))
	}
	release.Root = a.root()
	return &a, nil
}

func (g *globals) loadPreview(cmd *cobra.Command, raw string) (*app, error) {
	a, err := g.loadPreviewTemplate(cmd)
	if err != nil {
		return nil, err
	}
	n, err := config.PreviewName(raw)
	if err != nil {
		return nil, err
	}
	dest := "preview-" + n
	if _, exists := a.cfg.Destinations[dest]; exists {
		return nil, fmt.Errorf("preview %q collides with configured Destination %s", n, dest)
	}
	a.derivePreview(n)
	return a, nil
}

func (a *app) derivePreview(n string) {
	a.templateName, a.previewName = a.destName, n
	a.destName, a.dest = "preview-"+n, config.PreviewDestination(a.dest)
	a.cfg.Backups = config.BackupsConfig{}
}

// Refuse host reuse before rewriting the template extensions. Read every
// Destination's effective compose configuration, including its own overlays.
func (a *app) checkPreviewHostCollisions(ctx context.Context, template *composefile.Result) error {
	configured := map[string]string{}
	collect := func(dest string, r *composefile.Result) error {
		for name, svc := range r.Project.AllServices() {
			ext, err := config.DecodeServiceExt(svc.Extensions["x-yoho"])
			if err != nil {
				return fmt.Errorf("check configured hosts for %s/%s: %w", dest, name, err)
			}
			if ext.Proxy != nil {
				for _, host := range ext.Proxy.Hosts {
					configured[strings.ToLower(strings.TrimSuffix(host, "."))] = dest + "/" + name
				}
			}
		}
		return nil
	}
	for _, dest := range sortedKeys(a.cfg.Destinations) {
		cfg, _, _, err := config.LoadForDestination(a.path, dest)
		if err != nil {
			return fmt.Errorf("check configured hosts for Destination %s: %w", dest, err)
		}
		d, err := cfg.Dest(dest)
		if err != nil {
			return err
		}
		r, err := composefile.Load(ctx, a.dir, cfg.Compose, d.Env, release.ProjectName(cfg.App, dest), dest)
		if err != nil {
			return fmt.Errorf("check configured hosts for Destination %s: %w", dest, err)
		}
		if err := collect(dest, r); err != nil {
			return err
		}
	}
	for name, ext := range template.Ext {
		if ext.Proxy == nil {
			continue
		}
		host, err := a.cfg.Previews.RenderHost(a.previewName, name)
		if err != nil {
			return err
		}
		if owner, exists := configured[host]; exists {
			return fmt.Errorf("preview hostname %s is already configured on %s; change previews.host or the preview name", host, owner)
		}
	}
	return nil
}

func (a *app) preparePreviewCompose(r *composefile.Result) error {
	count := 0
	for _, e := range r.Ext {
		if e.Proxy != nil && len(e.Proxy.Hosts) > 0 {
			count++
		}
	}
	if count > 1 && !strings.Contains(a.cfg.Previews.Host, "{service}") {
		return errors.New("previews.host needs {service} for multiple proxied Services; use {service}-{name}.preview.example.com")
	}
	for name, e := range r.Ext {
		e.Backup = nil
		if e.Proxy != nil {
			p := *e.Proxy
			// Catch-all/custom-domain routes would steal traffic from the template.
			if len(p.Hosts) == 0 {
				return fmt.Errorf("preview Service %s needs explicit proxy.hosts on its template", name)
			}
			h, err := a.cfg.Previews.RenderHost(a.previewName, name)
			if err != nil {
				return err
			}
			p.Hosts = []string{h}
			e.Proxy = &p
		}
		r.Ext[name] = e
	}
	// A preview must have independent named volumes and networks. Compose-go
	// already derives ordinary resource names using the preview project name.
	for name, v := range r.Project.Volumes {
		if v.Driver != "" && v.Driver != "local" || len(v.DriverOpts) != 0 {
			return fmt.Errorf("preview volume %s uses a non-default driver or driver_opts; shared storage is not supported", name)
		}
		if v.External {
			return fmt.Errorf("preview volume %s is external; use a project-owned volume in the template", name)
		}
		v.Name = a.project() + "_" + name
		r.Project.Volumes[name] = v
	}
	for name, n := range r.Project.Networks {
		if len(n.Ipam.Config) != 0 {
			return fmt.Errorf("preview network %s has fixed IPAM config; use automatic allocation", name)
		}
		if !n.External {
			n.Name = a.project() + "_" + name
			r.Project.Networks[name] = n
		}
	}
	for name, svc := range r.Project.Services {
		for network, attachment := range svc.Networks {
			if attachment != nil && (attachment.Ipv4Address != "" || attachment.Ipv6Address != "") {
				return fmt.Errorf("preview Service %s network %s has a static IP address; use automatic allocation", name, network)
			}
		}
		if svc.ContainerName != "" {
			return fmt.Errorf("preview Service %s has container_name; remove it in the template overlay", name)
		}
		for _, v := range svc.Volumes {
			if v.Type == "bind" && !v.ReadOnly {
				return fmt.Errorf("preview Service %s has a writable bind mount; use a project-owned volume", name)
			}
		}
	}
	return nil
}

func (a *app) previewRecord(s *session) deploy.PreviewRecord {
	p := deploy.PreviewRecord{App: a.cfg.App, Name: a.previewName, Destination: a.destName, Project: a.project(), Version: s.ver, Hosts: []string{}}
	for _, name := range sortedKeys(s.r.Ext) {
		e := s.r.Ext[name]
		p.Services = append(p.Services, name)
		if e.Proxy != nil {
			p.Hosts = append(p.Hosts, e.Proxy.Hosts...)
		}
	}
	return p
}
