package cli

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yoho-build/yoho/skills"
)

func init() {
	extraCommands = append(extraCommands, skillCmd)
}

// skillDirs maps --agent values to project-local skills directories.
var skillDirs = map[string]string{
	"claude": filepath.Join(".claude", "skills"),
	"codex":  filepath.Join(".agents", "skills"),
}

func skillCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "skill", Short: "Install the Yoho agent skills for coding agents"}

	var dir, agent string
	install := &cobra.Command{
		Use:   "install",
		Short: "Write all bundled skills to .claude/skills/<name> and/or .agents/skills/<name>",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var targets []string
			switch agent {
			case "claude", "codex":
				targets = []string{agent}
			case "all", "":
				targets = []string{"claude", "codex"}
			default:
				return fmt.Errorf("unknown --agent %q (want claude, codex or all)", agent)
			}
			out := cmd.OutOrStdout()
			for _, t := range targets {
				for _, name := range skills.Names() {
					dest := filepath.Join(dir, skillDirs[t], name)
					n, err := writeSkill(name, dest)
					if err != nil {
						return err
					}
					if g.json {
						b, _ := json.Marshal(map[string]any{"event": "skill_installed", "agent": t, "skill": name, "path": dest, "files": n})
						fmt.Fprintln(out, string(b))
					} else {
						fmt.Fprintf(out, "installed skill %s for %s: %s (%d files)\n", name, t, dest, n)
					}
				}
			}
			return nil
		},
	}
	install.Flags().StringVar(&dir, "dir", ".", "Project directory to install into")
	install.Flags().StringVar(&agent, "agent", "all", "Agent to install for: claude, codex or all")

	print := &cobra.Command{
		Use:   "print [SKILL]",
		Short: "Print a skill's SKILL.md (default: yoho)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "yoho"
			if len(args) == 1 {
				name = args[0]
			}
			if !slices.Contains(skills.Names(), name) {
				return fmt.Errorf("unknown skill %q (available: %s)", name, strings.Join(skills.Names(), ", "))
			}
			b, err := skills.FS.ReadFile(name + "/SKILL.md")
			if err != nil {
				return err
			}
			_, err = cmd.OutOrStdout().Write(b)
			return err
		},
	}
	c.AddCommand(install, print)
	return c
}

// writeSkill copies the embedded skill name into dest and returns the file count.
func writeSkill(name, dest string) (int, error) {
	n := 0
	err := fs.WalkDir(skills.FS, name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.FromSlash(path.Clean(p[len(name):]))
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := skills.FS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(target, b, 0o644); err != nil {
			return err
		}
		n++
		return nil
	})
	return n, err
}
