package cli

import (
	"encoding/json"
	"errors"
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
	var only, exclude []string
	var noMigrateRefs bool
	install := &cobra.Command{
		Use:   "install",
		Short: "Write bundled skills to .claude/skills/<name> and/or .agents/skills/<name>",
		Long: `Write the bundled skills into the project for Claude Code and Codex.

--only and --exclude choose skills (see "yoho skill list"). An existing layout
is respected: when .claude/skills (or .agent/skills, if present) is a symlink
to .agents/skills, or a skill folder is a symlink to the real one, the skill is
written once. Installing never deletes skills that are already there.

The yoho skill ships references/migrate-*.md. They are skipped with
--no-migrate-refs, or when --only names no yoho-migrate-* skill.`,
		Args: cobra.NoArgs,
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
			names, err := selectSkills(skills.Names(), only, exclude)
			if err != nil {
				return err
			}
			skipMigrate := noMigrateRefs
			if len(only) > 0 && !slices.ContainsFunc(names, func(n string) bool { return strings.HasPrefix(n, "yoho-migrate-") }) {
				skipMigrate = true
			}
			dirs := make([]skillTarget, 0, 3)
			for _, t := range targets {
				dirs = append(dirs, skillTarget{t, filepath.Join(dir, skillDirs[t])})
			}
			if agent == "all" || agent == "" {
				if fi, err := os.Stat(filepath.Join(dir, ".agent", "skills")); err == nil && fi.IsDir() {
					dirs = append(dirs, skillTarget{"agent", filepath.Join(dir, ".agent", "skills")})
				}
			}
			out := cmd.OutOrStdout()
			emit := func(event, human string, fields map[string]any) {
				if g.json {
					fields["event"] = event
					fields["project"] = false
					b, _ := json.Marshal(fields)
					fmt.Fprintln(out, string(b))
				} else {
					fmt.Fprintln(out, human)
				}
			}
			written := map[string]string{} // real path -> first dest
			for _, t := range dirs {
				for _, name := range names {
					dest := filepath.Join(t.dir, name)
					real := realPath(dest)
					if first, ok := written[real]; ok {
						emit("skill_linked", fmt.Sprintf("skill %s for %s is the same folder as %s: not written again", name, t.agent, first),
							map[string]any{"agent": t.agent, "skill": name, "path": dest, "target": first})
						continue
					}
					written[real] = dest
					n, skipped, err := writeSkill(name, dest, skipMigrate)
					if err != nil {
						return err
					}
					emit("skill_installed", fmt.Sprintf("installed skill %s for %s: %s (%d files)", name, t.agent, dest, n),
						map[string]any{"agent": t.agent, "skill": name, "path": dest, "files": n})
					if skipped > 0 {
						emit("skill_skipped", fmt.Sprintf("skipped %d references/migrate-* files of skill %s", skipped, name),
							map[string]any{"agent": t.agent, "skill": name, "path": dest, "skipped": skipped, "reason": "migrate references"})
					}
				}
			}
			return nil
		},
	}
	install.Flags().StringVar(&dir, "dir", ".", "Project directory to install into")
	install.Flags().StringVar(&agent, "agent", "all", "Agent to install for: claude, codex or all")
	install.Flags().StringSliceVar(&only, "only", nil, "Install only these skills (comma-separated)")
	install.Flags().StringSliceVar(&exclude, "exclude", nil, "Install every skill except these (comma-separated)")
	install.Flags().BoolVar(&noMigrateRefs, "no-migrate-refs", false, "Skip the yoho skill's references/migrate-*.md files")

	list := &cobra.Command{
		Use:   "list",
		Short: "List the bundled skills with their descriptions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var rows [][]string
			for _, name := range skills.Names() {
				d, err := skills.Describe(name)
				if err != nil {
					return err
				}
				rows = append(rows, []string{name, d, fmt.Sprint(skills.FileCount(name))})
			}
			if g.json {
				for _, r := range rows {
					b, _ := json.Marshal(map[string]any{"event": "skill", "skill": r[0], "description": r[1], "files": skills.FileCount(r[0])})
					fmt.Fprintln(cmd.OutOrStdout(), string(b))
				}
				return nil
			}
			g.ui(cmd).Table([]string{"NAME", "DESCRIPTION", "FILES"}, rows)
			return nil
		},
	}

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
	c.AddCommand(install, list, print)
	return c
}

type skillTarget struct{ agent, dir string }

// selectSkills applies --only / --exclude to all. The two are exclusive and
// every name must be a bundled skill.
func selectSkills(all, only, exclude []string) ([]string, error) {
	if len(only) > 0 && len(exclude) > 0 {
		return nil, errors.New("--only and --exclude are mutually exclusive")
	}
	for _, n := range slices.Concat(only, exclude) {
		if !slices.Contains(all, n) {
			return nil, fmt.Errorf("unknown skill %q (available: %s)", n, strings.Join(all, ", "))
		}
	}
	var out []string
	for _, n := range all {
		if len(only) > 0 && !slices.Contains(only, n) || slices.Contains(exclude, n) {
			continue
		}
		out = append(out, n)
	}
	return out, nil
}

// realPath resolves symlinks in p even when its tail does not exist yet, so
// two paths that end in the same folder compare equal.
func realPath(p string) string {
	p, _ = filepath.Abs(p)
	rest := ""
	for cur := p; ; cur = filepath.Dir(cur) {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		if filepath.Dir(cur) == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
	}
}

// writeSkill copies the embedded skill name into dest and returns the file
// count and the number of references/migrate-* files skipped.
func writeSkill(name, dest string, skipMigrate bool) (int, int, error) {
	n, skipped := 0, 0
	err := fs.WalkDir(skills.FS, name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel := filepath.FromSlash(path.Clean(p[len(name):]))
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if skipMigrate && path.Dir(p) == name+"/references" && strings.HasPrefix(path.Base(p), "migrate-") {
			skipped++
			return nil
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
	return n, skipped, err
}
