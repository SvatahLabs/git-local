package cli

import (
	"errors"
	"fmt"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/restore"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/spf13/cobra"
)

func restoreCommand() *cobra.Command {
	var source, workspace string
	var dryRun, overwrite bool
	c := &cobra.Command{Use: "restore --source <snapshot> [--dry-run] [--overwrite] -- <paths...>", Short: "Restore selected paths with journaled recovery, or preview with --dry-run",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usageError{errors.New("restore requires explicit paths")}
			}
			if err := history.ValidateSelector(source); err != nil {
				return usageError{err}
			}
			if workspace != "" && !storage.ValidUUID(workspace) {
				return usageError{errors.New("invalid workspace UUID")}
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				if !dryRun {
					result, err := restore.Execute(c.Context(), r, cwd, workspace, source, args, overwrite)
					if setJSON(c, map[string]any{"transactionId": result.TransactionID, "recoverySnapshot": result.RecoverySnapshot, "changed": result.Changed}) {
						return err
					}
					if err != nil {
						return err
					}
					if !result.Changed {
						_, err = fmt.Fprintln(c.OutOrStdout(), "No changes needed.")
					} else {
						_, err = fmt.Fprintf(c.OutOrStdout(), "Restore transaction %s committed. Recovery snapshot: %s\n", result.TransactionID, result.RecoverySnapshot)
					}
					return err
				}
				result, err := restore.Plan(c.Context(), r, cwd, workspace, source, args, overwrite)
				if err != nil {
					return err
				}
				if setJSON(c, result) {
					return nil
				}
				_, err = fmt.Fprint(c.OutOrStdout(), result.Human())
				return err
			})
		},
	}
	c.Flags().StringVar(&source, "source", "", "Source snapshot: latest or a hexadecimal ID (required)")
	c.Flags().StringVar(&workspace, "workspace", "", "Source workspace UUID within this history store")
	c.Flags().BoolVar(&dryRun, "dry-run", false, "Show the plan without writing files")
	c.Flags().BoolVar(&overwrite, "overwrite", false, "Allow dirty recorded paths; safety conflicts still fail")
	return c
}
