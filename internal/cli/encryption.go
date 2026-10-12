package cli

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"gitlocal/internal/encryption"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
)

func encryptionCommand() *cobra.Command {
	command := &cobra.Command{Use: "encrypt", Short: "Configure sensitive-file encryption for local history"}
	command.AddCommand(&cobra.Command{Use: "keygen <absolute-path>", Short: "Create an external owner-private sensitive-file key", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 1 || !filepath.IsAbs(args[0]) {
			return usageError{errors.New("encrypt keygen requires one absolute key path outside the checkout")}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			if err := encryption.Keygen(c.Context(), r, cwd, args[0]); err != nil {
				return err
			}
			if setJSON(c, map[string]any{"path": args[0]}) {
				return nil
			}
			_, err := fmt.Fprintf(c.OutOrStdout(), "Created sensitive-file key at %s. Keep it outside the clone.\n", args[0])
			return err
		})
	}})
	var keyFile string
	enable := &cobra.Command{Use: "enable --key-file <absolute-path> -- <selected-paths...>", Short: "Enable encryption in a private store with no history", Args: func(_ *cobra.Command, args []string) error {
		if len(args) == 0 || !filepath.IsAbs(keyFile) {
			return usageError{errors.New("encrypt enable requires --key-file <absolute-path> and selected paths")}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			policy, err := encryption.EnableFresh(c.Context(), r, cwd, keyFile, args)
			if err != nil {
				return err
			}
			if setJSON(c, map[string]any{"keyFile": policy.KeyFile, "keyFingerprint": policy.KeyFingerprint, "sensitive": policy.Sensitive}) {
				return nil
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Enabled sensitive-file encryption for %d path rule(s).\n", len(policy.Sensitive))
			return err
		})
	}}
	enable.Flags().StringVar(&keyFile, "key-file", "", "Absolute path to an external 32-byte sensitive-file key")
	command.AddCommand(enable)
	var migrateKey string
	migrate := &cobra.Command{Use: "migrate --key-file <absolute-path> -- <selected-paths...>", Short: "Encrypt existing private snapshot history, or correct the policy in service", Args: func(_ *cobra.Command, args []string) error {
		if len(args) == 0 || !filepath.IsAbs(migrateKey) {
			return usageError{errors.New("encrypt migrate requires --key-file <absolute-path> and selected paths")}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			id, err := encryption.Migrate(c.Context(), r, cwd, migrateKey, args)
			if err != nil {
				return err
			}
			if setJSON(c, map[string]any{"transactionId": id}) {
				return nil
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Encrypted existing local history (transaction %s).\n", id)
			return err
		})
	}}
	migrate.Flags().StringVar(&migrateKey, "key-file", "", "Absolute path to an external 32-byte sensitive-file key")
	command.AddCommand(migrate)
	var resume, rollback bool
	recoverCommand := &cobra.Command{Use: "recover <transaction-id>", Short: "Resolve an interrupted encryption migration", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 1 || resume == rollback {
			return usageError{errors.New("encrypt recover requires one transaction ID and exactly one of --resume or --rollback")}
		}
		if !storage.ValidUUID(args[0]) {
			return usageError{errors.New("invalid transaction ID")}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			direction := "resume"
			if rollback {
				direction = "rollback"
			}
			if err := encryption.RecoverMigration(c.Context(), r, cwd, args[0], direction); err != nil {
				return recoveryKeyError(err, args[0], "before the policy is published, git local encrypt recover "+args[0]+" --rollback works without the key")
			}
			if setJSON(c, map[string]any{"transactionId": args[0], "direction": direction}) {
				return nil
			}
			outcome := "resumed"
			if rollback {
				outcome = "rolled back"
			}
			_, err := fmt.Fprintf(c.OutOrStdout(), "Encryption migration %s %s.\n", args[0], outcome)
			return err
		})
	}}
	recoverCommand.Flags().BoolVar(&resume, "resume", false, "Complete the encrypted-store publication")
	recoverCommand.Flags().BoolVar(&rollback, "rollback", false, "Restore the original store before policy publication")
	command.AddCommand(recoverCommand)
	command.AddCommand(&cobra.Command{Use: "rebind-key <absolute-path>", Short: "Point an existing policy at the same key in a new location", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 1 || !filepath.IsAbs(args[0]) {
			return usageError{errors.New("encrypt rebind-key requires one absolute key path outside every checkout")}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			policy, err := encryption.Rebind(c.Context(), r, cwd, args[0])
			if err != nil {
				return err
			}
			if setJSON(c, map[string]any{"keyFile": policy.KeyFile, "keyFingerprint": policy.KeyFingerprint}) {
				return nil
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Rebound sensitive-file key to %s.\n", policy.KeyFile)
			return err
		})
	}})
	return command
}
