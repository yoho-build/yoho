package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/composefile"
	"github.com/yoho-build/yoho/internal/config"
	"github.com/yoho-build/yoho/internal/deploy"
	"github.com/yoho-build/yoho/internal/release"
	"github.com/yoho-build/yoho/internal/remote"
)

func init() { extraCommands = append(extraCommands, previewCmd) }

func previewCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "preview", Short: "Deploy and tear down ephemeral Destinations"}
	var name string
	c.PersistentFlags().StringVar(&name, "name", "", "Preview name (for example pr-123)")
	var o applyOptions
	up := &cobra.Command{Use: "up", Short: "Build and deploy a preview from previews.from", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.loadPreview(cmd, name)
			if err != nil {
				return err
			}
			return a.deploy(cmd.Context(), o)
		}}
	up.Flags().StringVar(&o.Version, "version", "", "Version to deploy (default: git SHA)")
	up.Flags().BoolVar(&o.SkipBuild, "skip-build", false, "Use images already built for this preview Version")
	addLockWaitFlag(up)
	var yes bool
	down := &cobra.Command{Use: "down", Short: "Remove a marked preview, including its volumes", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a, err := g.loadPreview(cmd, name)
			if err != nil {
				return err
			}
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
				glob := remote.Quote(path.Join(release.Root, "apps", a.cfg.App)) + "/preview-*/preview.json"
				out, err := h.Host.Output(cmd.Context(), remote.Cmd{Script: "set -eu\nfor f in " + glob + "; do [ -f \"$f\" ] || continue; [ ! -L \"$f\" ] || exit 1; cat \"$f\"; printf '\\n'; done"})
				if err != nil {
					return err
				}
				dec := json.NewDecoder(strings.NewReader(out))
				for {
					var p deploy.PreviewRecord
					if err := dec.Decode(&p); err == io.EOF {
						break
					} else if err != nil {
						return err
					}
					if err := p.Validate(a.cfg.App, "preview-"+p.Name); err != nil {
						return err
					}
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
// --destination and YOHO_DESTINATION never select a preview template.
func (g *globals) loadPreviewTemplate(cmd *cobra.Command) (*app, error) {
	file, err := g.yohoFile()
	if err != nil {
		return nil, err
	}
	base, err := config.Load(file)
	if err != nil {
		return nil, err
	}
	if base.Previews == nil {
		return nil, errors.New("previews is required; configure previews.from and previews.host")
	}
	if errs := config.ValidatePreviews(base); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	local := *g
	local.destination = base.Previews.From
	a, err := local.load(cmd)
	if err != nil {
		return nil, err
	}
	if a.scoped {
		return nil, errors.New("previews require operator access")
	}
	a.g = g
	return a, nil
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
		if v.External {
			return fmt.Errorf("preview volume %s is external; use a project-owned volume in the template", name)
		}
		v.Name = a.project() + "_" + name
		r.Project.Volumes[name] = v
	}
	for name, n := range r.Project.Networks {
		if !n.External {
			n.Name = a.project() + "_" + name
			r.Project.Networks[name] = n
		}
	}
	for name, svc := range r.Project.Services {
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
