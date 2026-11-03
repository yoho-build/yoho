package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/internal/railway"
)

func migrateDraftCmd(g *globals) *cobra.Command {
	var from, out, service, project string
	c := &cobra.Command{
		Use:   "draft",
		Short: "Draft compose.yaml and yoho.yml from a Railway inventory or railway.json",
		Long: `Read a names-only inventory JSON, or one service's railway.json / railway.toml,
and print a compose file plus a Yoho file. The inventory lists variable names,
never values. railway.json and railway.toml are Railway's deprecated
config-as-code files (one service's build and deploy); Yoho does not read
.railway/railway.ts and does not call Railway.

Secret values are refused. With --out, compose.yaml and yoho.yml are written
only when those names do not already exist. .yoho/secrets is not written.

Nothing is deployed and no Server is contacted.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runMigrateDraft(cmd, g, from, out, service, project)
		},
	}
	c.Flags().StringVar(&from, "from", "", "Names-only inventory JSON, or railway.json / railway.toml")
	c.Flags().StringVar(&out, "out", "", "Write compose.yaml and yoho.yml into this directory (neither file may already exist)")
	c.Flags().StringVar(&service, "service", "", "Service name for a one-service railway.json or railway.toml (default web)")
	c.Flags().StringVar(&project, "project", "", "App name when the file has none (default app), or override the inventory project")
	return c
}

func runMigrateDraft(cmd *cobra.Command, g *globals, from, out, service, project string) error {
	u := g.ui(cmd)
	if from == "" {
		err := &hintError{"--from is required", "pass a names-only inventory JSON, or a railway.json / railway.toml"}
		u.Error(err, err.hint)
		return &silentError{err}
	}
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	d, err := railway.Draft(railway.Request{Filename: from, Data: data, Service: service, Project: project})
	if err != nil {
		var ie *railway.InputError
		if errors.As(err, &ie) {
			u.Error(ie, ie.Hint)
			return &silentError{ie}
		}
		return err
	}
	if out != "" {
		if err := writeDraftFiles(out, d); err != nil {
			var ie *railway.InputError
			if errors.As(err, &ie) {
				u.Error(ie, ie.Hint)
				return &silentError{ie}
			}
			return err
		}
	}
	u.Event("migrate_draft", "", map[string]any{
		"services": d.Services, "volumes": d.Volumes, "secret_keys": d.SecretKeys,
		"generated": d.Generated, "warnings": d.Warnings, "secret_lines": d.SecretLines,
		"compose": d.Compose, "yoho": d.Yoho, "out": out,
	})
	u.Event("migrate_rotation_checklist", "", map[string]any{"secrets": rotationItems(d.Rotation)})
	if g.json {
		return nil
	}
	if out == "" {
		fmt.Fprint(cmd.OutOrStdout(), d.Compose)
		fmt.Fprint(cmd.OutOrStdout(), "\n")
		fmt.Fprint(cmd.OutOrStdout(), d.Yoho)
		printRotationChecklist(cmd.ErrOrStderr(), d.Rotation)
		return nil
	}
	u.Info("wrote %s", filepath.Join(out, "compose.yaml"))
	u.Info("wrote %s", filepath.Join(out, "yoho.yml"))
	u.Info("Add to .yoho/secrets (references only; do not paste values):")
	for _, line := range d.SecretLines {
		u.Info("%s", line)
	}
	u.Info("Review the files. Nothing was deployed.")
	printRotationChecklist(cmd.OutOrStdout(), d.Rotation)
	return nil
}

// rotationItems keeps the JSON field an array when the checklist is empty.
func rotationItems(items []railway.RotationItem) []railway.RotationItem {
	if items == nil {
		return []railway.RotationItem{}
	}
	return items
}

// printRotationChecklist ends a human draft: every secret name that lived
// at Railway, and who uses it. Names only.
func printRotationChecklist(w io.Writer, items []railway.RotationItem) {
	if len(items) == 0 {
		return
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Rotation checklist (these credentials were readable at Railway):")
	for _, it := range items {
		fmt.Fprintf(w, "  [ ] %s (used by %s): %s\n", it.Key, strings.Join(it.Services, ", "), it.Action)
	}
	fmt.Fprintln(w, "Rotate before pointing traffic at Yoho; after an incident see the runbook in docs/site/secrets.md.")
}

// writeDraftFiles creates compose.yaml and yoho.yml. It refuses when either
// name already exists, including as a symlink, and it does not write a secrets file.
func writeDraftFiles(dir string, d *railway.Result) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	compose := filepath.Join(dir, "compose.yaml")
	yoho := filepath.Join(dir, "yoho.yml")
	for _, p := range []string{compose, yoho} {
		if _, err := os.Lstat(p); err == nil {
			return &railway.InputError{Message: p + " already exists", Hint: "choose another --out directory; draft does not overwrite"}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.WriteFile(compose, []byte(d.Compose), 0o644); err != nil {
		return err
	}
	return os.WriteFile(yoho, []byte(d.Yoho), 0o644)
}
