package cli

import (
	"fmt"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/spf13/cobra"
)

func maintenanceCommand() *cobra.Command {
	return &cobra.Command{Use: "maintenance", Short: "Pack the private history store and delete objects no snapshot or recovery ref uses", Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				result, err := history.Maintain(c.Context(), r, cwd)
				if setJSON(c, result) || err != nil {
					return err
				}
				_, err = fmt.Fprintf(c.OutOrStdout(), "Private history: %d loose and %d packed objects (%s) before, %d loose and %d packed (%s) after.\n",
					result.Before.LooseObjects, result.Before.PackedObjects, byteSize(result.Before.Bytes()),
					result.After.LooseObjects, result.After.PackedObjects, byteSize(result.After.Bytes()))
				return err
			})
		},
	}
}

// byteSize prints a size in the largest whole binary unit.
func byteSize(n int64) string {
	for _, unit := range []struct {
		name string
		size int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}} {
		if n >= unit.size {
			return fmt.Sprintf("%.1f %s", float64(n)/float64(unit.size), unit.name)
		}
	}
	return fmt.Sprintf("%d bytes", n)
}
