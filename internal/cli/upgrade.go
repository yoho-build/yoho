package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/yoho-build/yoho/internal/selfupdate"
)

func init() { extraCommands = append(extraCommands, upgradeCmd) }

func upgradeCmd(g *globals) *cobra.Command { return newUpgradeCmd(g, selfupdate.Upgrade) }

func newUpgradeCmd(g *globals, run func(context.Context, string) (selfupdate.Result, error)) *cobra.Command {
	return &cobra.Command{
		Use:     "upgrade",
		Aliases: []string{"update"},
		Short:   "Update the local Yoho CLI to the latest stable release",
		Long:    "Update the running Yoho executable to the latest stable GitHub release. Verifies SHA-256 checksums and signed build provenance before replacing the binary. Does not require a Yoho file. Package manager installations must be updated with their package manager.",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			output := g.ui(cmd)
			output.Info("Checking for the latest stable Yoho release...")
			result, err := run(cmd.Context(), Version)
			if err != nil {
				output.Error(err, "The existing Yoho executable has not been replaced.")
				return &silentError{err}
			}
			message := fmt.Sprintf("Yoho %s is already up to date.", result.Version)
			if result.Updated {
				message = fmt.Sprintf("Updated Yoho %s → %s (%s)", result.Previous, result.Version, result.Path)
			}
			output.Event("upgrade", message, map[string]any{"previous": result.Previous, "version": result.Version, "path": result.Path, "updated": result.Updated})
			return nil
		},
	}
}
