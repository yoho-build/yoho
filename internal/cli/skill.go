package cli

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/yoho-dev/yoho/skills"
)

func init() {
	extraCommands = append(extraCommands, skillCmd)
}

// skillDirs maps --agent values to project-local skill directories.
var skillDirs = map[string]string{
	"claude": filepath.Join(".claude", "skills", "yoho"),
	"codex":  filepath.Join(".agents", "skills", "yoho"),
}

func skillCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "skill", Short: "Install the Yoho agent skill for coding agents"}

	var dir, agent string
	install := &cobra.Command{
		Use:   "install",
		Short: "Write the embedded skill to .claude/skills/yoho and/or .agents/skills/yoho",
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
				dest := filepath.Join(dir, skillDirs[t])
				n, err := writeSkill(dest)
				if err != nil {
					return err
				}
				if g.json {
					b, _ := json.Marshal(map[string]any{"event": "skill_installed", "agent": t, "path": dest, "files": n})
					fmt.Fprintln(out, string(b))
				} else {
					fmt.Fprintf(out, "installed skill for %s: %s (%d files)\n", t, dest, n)
				}
			}
			return nil
		},
	}
	install.Flags().StringVar(&dir, "dir", ".", "Project directory to install into")
	install.Flags().StringVar(&agent, "agent", "all", "Agent to install for: claude, codex or all")

	print := &cobra.Command{
		Use:   "print",
		Short: "Print the skill's SKILL.md",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			b, err := skills.FS.ReadFile("yoho/SKILL.md")
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

// writeSkill copies the embedded skill into dest and returns the file count.
func writeSkill(dest string) (int, error) {
	n := 0
	err := fs.WalkDir(skills.FS, "yoho", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.FromSlash(path.Clean(p[len("yoho"):]))
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
