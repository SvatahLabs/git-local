package cli

import (
	"fmt"

	"github.com/spf13/cobra"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/teardown"
)

func deinitCommand() *cobra.Command {
	var removeHistory, unignore bool
	command := &cobra.Command{Use: "deinit", Short: "Remove git-local from this repository and its worktrees, keeping the private history", Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				result, err := teardown.Deinit(c.Context(), r, cwd, removeHistory, unignore)
				if setJSON(c, result, result.Warnings...) {
					return err
				}
				for _, warning := range result.Warnings {
					fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
				}
				if err != nil {
					return err
				}
				out := c.OutOrStdout()
				for _, name := range result.Overlays {
					fmt.Fprintf(out, "Removed overlay %s; its file keeps the local content as an ordinary change.\n", name)
				}
				for _, path := range result.Hooks {
					fmt.Fprintf(out, "Removed git-local from hook %s.\n", path)
				}
				if result.KeptIgnored {
					fmt.Fprintln(out, "Replaced the managed block in .git/info/exclude with ordinary lines under \"# kept by git local deinit\", so selected files stay ignored; delete those lines to stop ignoring them.")
				} else if result.Exclude {
					fmt.Fprintln(out, "Removed the managed block from .git/info/exclude; selected files now show as untracked, so git add -A would stage them and git clean -fd would delete them.")
				}
				for _, path := range result.Identities {
					fmt.Fprintf(out, "Removed workspace identity %s.\n", path)
				}
				switch {
				case result.Archive != "":
					_, err = fmt.Fprintf(out, "Kept the private history and state in the common Git directory as %s; delete it when you no longer need it.\n", result.Archive)
				case result.Removed:
					_, err = fmt.Fprintln(out, "Deleted the private history and state.")
				default:
					_, err = fmt.Fprintln(out, "git-local isn't set up in this repository.")
				}
				return err
			})
		},
	}
	command.Flags().BoolVar(&unignore, "unignore", false, "Stop ignoring the selected files instead of keeping their lines in .git/info/exclude")
	command.Flags().BoolVar(&removeHistory, "remove-history", false, "Delete the private history instead of keeping it (refused while a quarantine or restore recovery ref exists)")
	return command
}
