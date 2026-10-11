// Package cli owns the public command surface and exit classification.
package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"gitlocal/internal/automatic"
	"gitlocal/internal/backup"
	"gitlocal/internal/configtx"
	"gitlocal/internal/doctor"
	"gitlocal/internal/encryption"
	"gitlocal/internal/exclude"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/guard"
	"gitlocal/internal/history"
	"gitlocal/internal/hook"
	"gitlocal/internal/overlay"
	"gitlocal/internal/restore"
	"gitlocal/internal/seed"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type usageError struct{ error }
type guardRepositoryError struct{ error }

func (e guardRepositoryError) Unwrap() error { return e.error }

type guardViolation struct{ error }

// Version is replaced by release builds; source builds identify themselves as dev.
var Version = "dev"

// New constructs an independent command tree. Restore execution is journaled and explicitly recoverable.
func New(out, errOut io.Writer) *cobra.Command {
	root := &cobra.Command{Use: "git-local", Short: "Independent local history for selected project files",
		Version:       Version,
		Long:          "Independent local history for selected project files.\nInitialize a workspace, select local files, check health, and recover transactions.\nCommit, inspect changes, and browse independent local snapshots. Preview or execute explicit-path restores with journaled recovery.",
		SilenceErrors: true, SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{fmt.Errorf("unknown command %q", args[0])}
			}
			return c.Help()
		},
	}
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.PersistentFlags().Bool("json", false, "Emit a versioned JSON result or error")

	var seedFrom string
	var seedPaths []string
	init := &cobra.Command{Use: "init", Short: "Initialize or resume this workspace's independent history store",
		Args: func(c *cobra.Command, args []string) error {
			if c.Flags().Changed("from") && seedFrom == "" {
				return usageError{errors.New("--from requires main or a workspace UUID")}
			}
			if seedFrom != "" && seedFrom != "main" && !storage.ValidUUID(seedFrom) {
				return usageError{errors.New("--from requires main or a workspace UUID")}
			}
			if seedFrom == "" && (c.Flags().Changed("paths") || len(args) != 0) {
				return usageError{errors.New("init paths require --from")}
			}
			if seedFrom != "" && len(args) != 0 && !c.Flags().Changed("paths") {
				return usageError{errors.New("use --paths before seed path arguments")}
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			r, err := gitexec.New(os.Environ())
			if err != nil {
				return err
			}
			defer r.Close()
			if seedFrom != "" {
				result, err := seed.Run(c.Context(), r, cwd, seedFrom, append(seedPaths, args...))
				if setJSON(c, map[string]any{"workspaceId": result.WorkspaceID, "transactionId": result.TransactionID, "source": seedFrom}, result.Warnings...) {
					return err
				}
				for _, warning := range result.Warnings {
					fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
				}
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(c.OutOrStdout(), "Initialized and seeded local workspace %s from %s.\n", result.WorkspaceID, seedFrom)
				return err
			}
			w, err := workspace.Init(c.Context(), r, cwd)
			if err != nil {
				return err
			}
			// Init is the reconcile command: it also repairs the shared overlay
			// filter installation, without creating an overlay version.
			executable, err := os.Executable()
			if err != nil {
				return err
			}
			migrated, err := overlay.Migrate(c.Context(), r, w.Root, w.Common, executable)
			if migrated == nil {
				migrated = []string{}
			}
			if err != nil {
				err = fmt.Errorf("initialized local workspace %s, but the overlay filter needs repair: %w", w.WorkspaceID, err)
			}
			// It also rewrites a managed ignore block an earlier release wrote,
			// or adds one where there was none (a fresh or 0.1.4 repository).
			var refreshed bool
			previous, _ := os.ReadFile(w.Exclude)
			warnings := w.Warnings
			if err == nil {
				var refreshWarnings []string
				refreshed, refreshWarnings, err = configtx.RefreshIgnore(c.Context(), r, cwd)
				warnings = append(warnings, refreshWarnings...)
				if err != nil {
					err = fmt.Errorf("initialized local workspace %s, but the managed ignore block needs repair: %w", w.WorkspaceID, err)
				}
			}
			if setJSON(c, map[string]any{"workspaceId": w.WorkspaceID, "repositoryId": w.RepositoryID, "historyStore": w.Store, "overlayMigration": migrated, "ignoreBlockRewritten": refreshed}, warnings...) {
				return err
			}
			for _, warning := range warnings {
				fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
			}
			if err != nil {
				return err
			}
			if _, err = fmt.Fprintf(c.OutOrStdout(), "Initialized local workspace %s\nHistory store: %s\n", w.WorkspaceID, w.Store); err != nil {
				return err
			}
			if refreshed {
				change := "added"
				if bytes.Contains(previous, []byte(exclude.Begin)) {
					change = "rewritten for this release"
				}
				if _, err = fmt.Fprintf(c.OutOrStdout(), "Managed ignore block: %s.\n", change); err != nil {
					return err
				}
			}
			for _, change := range migrated {
				if _, err = fmt.Fprintf(c.OutOrStdout(), "Overlay filter: %s.\n", change); err != nil {
					return err
				}
			}
			return nil
		},
	}
	init.Flags().StringVar(&seedFrom, "from", "", "Seed from the latest snapshot of a workspace UUID or main checkout")
	init.Flags().StringArrayVar(&seedPaths, "paths", nil, "Seed a subset of source paths; repeat or add positional paths")
	root.AddCommand(init)
	root.AddCommand(encryptionCommand())

	backupCommand := &cobra.Command{Use: "backup", Short: "Back up or restore encrypted local history on a separate remote"}
	backupCommand.AddCommand(&cobra.Command{Use: "keygen <absolute-path>", Short: "Create a new external owner-private backup key", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 1 || !filepath.IsAbs(args[0]) {
			return usageError{errors.New("backup keygen requires one absolute path outside the checkout")}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			if err := backup.Keygen(c.Context(), r, cwd, args[0]); err != nil {
				return err
			}
			if setJSON(c, map[string]any{"path": args[0]}) {
				return nil
			}
			_, err := fmt.Fprintf(c.OutOrStdout(), "Created backup key at %s. Keep it outside the clone.\n", args[0])
			return err
		})
	}})
	var backupKeyFile string
	backupPush := &cobra.Command{Use: "push <remote>", Short: "Push one encrypted archive to a remote other than origin", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 1 || backupKeyFile == "" || !filepath.IsAbs(backupKeyFile) {
			return usageError{errors.New("backup push requires a remote and --key-file <absolute-path>")}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			result, err := backup.Push(c.Context(), r, cwd, args[0], backupKeyFile)
			if setJSON(c, map[string]any{"ref": result.Ref, "destinations": result.Destinations}) {
				return err
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Pushed encrypted backup %s to %d destination(s).\n", result.Ref, result.Destinations)
			return err
		})
	}}
	backupPush.Flags().StringVar(&backupKeyFile, "key-file", "", "Absolute path to an external 32-byte backup key")
	backupCommand.AddCommand(backupPush)
	var backupRestoreKeyFile, backupRestoreFileKey string
	backupRestore := &cobra.Command{Use: "restore <remote> <backup-ref>", Short: "Restore an encrypted backup into a fresh main checkout", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 2 || backupRestoreKeyFile == "" || !filepath.IsAbs(backupRestoreKeyFile) {
			return usageError{errors.New("backup restore requires a remote, backup ref, and --key-file <absolute-path>")}
		}
		if backupRestoreFileKey != "" && !filepath.IsAbs(backupRestoreFileKey) {
			return usageError{errors.New("--file-key requires an absolute path outside the checkout")}
		}
		if err := backup.ValidateBackupRef(args[1]); err != nil {
			return usageError{err}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			stage, err := backup.FetchStaged(c.Context(), r, cwd, args[0], args[1], backupRestoreKeyFile)
			if err != nil {
				return err
			}
			defer os.RemoveAll(stage)
			var fileKeys []string
			if backupRestoreFileKey != "" {
				fileKeys = append(fileKeys, backupRestoreFileKey)
			}
			info, err := backup.AdoptStaged(c.Context(), r, cwd, stage, fileKeys...)
			if err != nil {
				return err
			}
			if setJSON(c, map[string]any{"workspaceId": info.WorkspaceID, "repositoryId": info.RepositoryID, "ref": args[1], "archivedOverlays": info.ArchivedOverlays}) {
				return nil
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Restored encrypted backup into local workspace %s.\n", info.WorkspaceID)
			if err == nil && info.ArchivedOverlays > 0 {
				_, err = fmt.Fprintf(c.OutOrStdout(), "%d saved overlay(s) were deactivated; inspect with git local overlay show and reactivate before staging.\n", info.ArchivedOverlays)
			}
			return err
		})
	}}
	backupRestore.Flags().StringVar(&backupRestoreKeyFile, "key-file", "", "Absolute path to the external 32-byte backup key")
	backupRestore.Flags().StringVar(&backupRestoreFileKey, "file-key", "", "Matching external sensitive-file key for restored encrypted history")
	backupCommand.AddCommand(backupRestore)
	backupCommand.AddCommand(&cobra.Command{Use: "recover", Short: "Resume an interrupted encrypted backup restore", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 0 {
			return usageError{errors.New("backup recover takes no arguments")}
		}
		return nil
	}, RunE: func(c *cobra.Command, _ []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			info, err := backup.RecoverRestore(c.Context(), r, cwd)
			if err != nil {
				return err
			}
			if setJSON(c, map[string]any{"workspaceId": info.WorkspaceID, "repositoryId": info.RepositoryID, "archivedOverlays": info.ArchivedOverlays}) {
				return nil
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Recovered encrypted backup restore for local workspace %s.\n", info.WorkspaceID)
			if err == nil && info.ArchivedOverlays > 0 {
				_, err = fmt.Fprintf(c.OutOrStdout(), "%d saved overlay(s) were deactivated; inspect with git local overlay show and reactivate before staging.\n", info.ArchivedOverlays)
			}
			return err
		})
	}})
	root.AddCommand(backupCommand)

	for _, name := range []string{"track", "untrack"} {
		track := name == "track"
		var from, sourceWorkspace string
		short := "Select literal paths and maintain Git ignore rules"
		if !track {
			short = "Stop selecting paths; preserve files and existing history"
		}
		command := &cobra.Command{Use: name + " <paths...>", Short: short,
			Args: func(c *cobra.Command, args []string) error {
				if track && c.Flags().Changed("from") && from == "" {
					return usageError{errors.New("--from requires a snapshot selector")}
				}
				if len(args) == 0 {
					return usageError{errors.New("at least one literal path is required")}
				}
				if sourceWorkspace != "" && (from == "" || !storage.ValidUUID(sourceWorkspace)) {
					return usageError{errors.New("--workspace requires --from and a valid workspace UUID")}
				}
				if from != "" {
					if err := history.ValidateSelector(from); err != nil {
						return usageError{err}
					}
				}

				return nil
			},
			RunE: func(c *cobra.Command, args []string) error {
				cwd, err := os.Getwd()
				if err != nil {
					return err
				}
				r, err := gitexec.New(os.Environ())
				if err != nil {
					return err
				}
				defer r.Close()
				var result configtx.Result
				if from != "" {
					result, err = history.TrackFrom(c.Context(), r, cwd, sourceWorkspace, from, args...)
				} else {
					result, err = configtx.Change(c.Context(), r, cwd, track, args...)
				}
				if setJSON(c, map[string]any{"workspaceId": result.WorkspaceID, "transactionId": result.TransactionID, "changed": result.Changed, "paths": args}, result.Warnings...) {
					return err
				}
				for _, warning := range result.Warnings {
					fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
				}
				if err != nil {
					return err
				}
				if result.Changed {
					_, err = fmt.Fprintf(c.OutOrStdout(), "Selection updated for workspace %s.\n", result.WorkspaceID)
				} else {
					_, err = fmt.Fprintln(c.OutOrStdout(), "Selection unchanged.")
				}
				return err
			},
		}
		if track {
			command.Flags().StringVar(&from, "from", "", "Select historical path types from latest or a hexadecimal snapshot ID")
			command.Flags().StringVar(&sourceWorkspace, "workspace", "", "Source workspace UUID (requires --from)")
		}
		root.AddCommand(command)
	}

	var message string
	var ifChanged bool
	var commitWriter string
	commit := &cobra.Command{Use: "commit -m <message> [-- <paths...>]", Short: "Save all selected files or only explicit selected paths",
		Args: func(c *cobra.Command, args []string) error {
			if len(args) != 0 && c.ArgsLenAtDash() < 0 {
				return usageError{errors.New("commit paths require -- before the first path")}
			}
			if c.Flags().Changed("writer") && commitWriter == "" {
				return usageError{errors.New("--writer requires a nonempty session or process ID")}
			}
			if err := history.ValidateWriter(commitWriter); err != nil {
				return usageError{err}
			}
			if ifChanged && !c.Flags().Changed("message") {
				message = "Automatic local snapshot"
			}
			if err := history.ValidateMessage(message); err != nil {
				return usageError{err}
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			r, err := gitexec.New(os.Environ())
			if err != nil {
				return err
			}
			defer r.Close()
			var result history.Result
			if len(args) != 0 {
				result, err = history.CommitScoped(c.Context(), r, cwd, message, commitWriter, args)
			} else {
				result, err = history.CommitWithWriter(c.Context(), r, cwd, message, commitWriter)
			}
			if setJSON(c, map[string]any{"snapshotId": result.ID, "changed": result.Changed, "writer": commitWriter, "scope": append([]string{}, args...)}, result.Warnings...) {
				return err
			}
			for _, warning := range result.Warnings {
				fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
			}
			if err != nil {
				return err
			}
			if !result.Changed {
				if ifChanged {
					return nil
				}
				_, err = fmt.Fprintln(c.OutOrStdout(), "Nothing to save.")
			} else {
				_, err = fmt.Fprintf(c.OutOrStdout(), "Saved local snapshot %s.\n", result.ID)
			}
			return err
		},
	}
	commit.Flags().StringVarP(&message, "message", "m", "", "Snapshot message (required)")
	commit.Flags().BoolVar(&ifChanged, "if-changed", false, "Snapshot only when content or selection changed; use an automatic message if -m is omitted")
	commit.Flags().StringVar(&commitWriter, "writer", "", "Caller-supplied session or process ID attributed to changed paths")
	root.AddCommand(commit)

	var porcelain, zero bool
	status := &cobra.Command{Use: "status", Short: "Compare selected files with the latest local snapshot",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{errors.New("status accepts no paths")}
			}
			if porcelain != zero {
				return usageError{errors.New("porcelain v1 requires both --porcelain and -z")}
			}
			return nil
		},
		RunE: func(c *cobra.Command, _ []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				result, err := history.Status(c.Context(), r, cwd)
				if err != nil {
					return err
				}
				if setJSON(c, map[string]any{"files": result.Files, "selection": result.Selection}, result.Warnings...) {
					return inspectionConflicts(result)
				}
				for _, warning := range result.Warnings {
					fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
				}
				if porcelain {
					_, err = c.OutOrStdout().Write(result.Porcelain())
				} else {
					_, err = fmt.Fprint(c.OutOrStdout(), result.Human())
				}
				if err != nil {
					return err
				}
				return inspectionConflicts(result)
			})
		},
	}
	status.Flags().BoolVar(&porcelain, "porcelain", false, "Emit porcelain v1 records (requires -z)")
	status.Flags().BoolVarP(&zero, "null", "z", false, "NUL-terminate porcelain records")
	root.AddCommand(status)
	var diffWorkspace string
	diffCommand := &cobra.Command{Use: "diff [<before> <after>]", Short: "Compare the worktree with the latest snapshot, or two historical snapshots",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 && len(args) != 2 {
				return usageError{errors.New("diff requires zero or two snapshot selectors")}
			}
			if diffWorkspace != "" && (len(args) != 2 || !storage.ValidUUID(diffWorkspace)) {
				return usageError{errors.New("--workspace requires two snapshot selectors and a valid workspace UUID")}
			}
			for _, selector := range args {
				if err := history.ValidateSelector(selector); err != nil {
					return usageError{err}
				}
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				if len(args) == 2 {
					patch, err := history.DiffSnapshots(c.Context(), r, cwd, diffWorkspace, args[0], args[1])
					if err != nil {
						return err
					}
					if patch == "" {
						patch = "No differences between snapshots.\n"
					}
					if setJSON(c, map[string]any{"before": args[0], "after": args[1], "workspaceId": diffWorkspace, "patch": patch}) {
						return nil
					}
					_, err = fmt.Fprint(c.OutOrStdout(), patch)
					return err
				}
				result, err := history.Diff(c.Context(), r, cwd)
				if err != nil {
					return err
				}
				if setJSON(c, map[string]any{"files": result.Status.Files, "selection": result.Status.Selection, "patch": result.Patch}, result.Status.Warnings...) {
					return inspectionConflicts(result.Status)
				}
				for _, warning := range result.Status.Warnings {
					fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
				}
				if _, err = fmt.Fprint(c.OutOrStdout(), result.Human()); err != nil {
					return err
				}
				return inspectionConflicts(result.Status)
			})
		},
	}
	diffCommand.Flags().StringVar(&diffWorkspace, "workspace", "", "Source workspace UUID, including retained history (historical diff only)")
	root.AddCommand(diffCommand)
	var showWorkspace string
	showCommand := &cobra.Command{Use: "show <snapshot>:<path>", Short: "Print raw recorded file content without restoring it",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 {
				return usageError{errors.New("show requires <snapshot>:<repository-relative-path>")}
			}
			selector, path, ok := strings.Cut(args[0], ":")
			if !ok || path == "" {
				return usageError{errors.New("show requires <snapshot>:<repository-relative-path>")}
			}
			if err := history.ValidateSelector(selector); err != nil {
				return usageError{err}
			}
			if err := storage.ValidateSelectionPath(path); err != nil {
				return usageError{err}
			}
			if showWorkspace != "" && !storage.ValidUUID(showWorkspace) {
				return usageError{errors.New("invalid workspace UUID")}
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				selector, path, _ := strings.Cut(args[0], ":")
				data, err := history.Show(c.Context(), r, cwd, showWorkspace, selector, path)
				if err != nil {
					return err
				}
				if setJSON(c, map[string]any{"snapshot": selector, "workspaceId": showWorkspace, "path": path, "encoding": "base64", "contentBase64": base64.StdEncoding.EncodeToString(data), "size": len(data)}) {
					return nil
				}
				_, err = c.OutOrStdout().Write(data)
				return err
			})
		},
	}
	showCommand.Flags().StringVar(&showWorkspace, "workspace", "", "Source workspace UUID, including retained history")
	root.AddCommand(showCommand)
	var grepWorkspace string
	var grepSnapshots, grepMatches int
	grepCommand := &cobra.Command{Use: "grep <literal-pattern>", Short: "Search text in recent local snapshots without restoring files",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 || args[0] == "" || !utf8.ValidString(args[0]) || strings.ContainsRune(args[0], 0) {
				return usageError{errors.New("grep requires one nonempty literal pattern")}
			}
			if grepWorkspace != "" && !storage.ValidUUID(grepWorkspace) {
				return usageError{errors.New("invalid workspace UUID")}
			}
			if grepSnapshots < 1 || grepSnapshots > 100 || grepMatches < 1 || grepMatches > 1000 {
				return usageError{errors.New("grep limits must be 1–100 snapshots and 1–1000 matches")}
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				result, err := history.Grep(c.Context(), r, cwd, grepWorkspace, args[0], grepSnapshots, grepMatches)
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
	grepCommand.Flags().StringVar(&grepWorkspace, "workspace", "", "Source workspace UUID, including retained history")
	grepCommand.Flags().IntVarP(&grepSnapshots, "snapshots", "n", 20, "Search the newest 1–100 snapshots")
	grepCommand.Flags().IntVar(&grepMatches, "max-matches", 100, "Stop after 1–1000 matches")
	root.AddCommand(grepCommand)
	var limit int
	var logWorkspace string
	var logBranch string
	var logMainCommit string
	var logStat bool
	var logWriter, logPath string
	var logProvenance bool
	logCommand := &cobra.Command{Use: "log", Short: "List a workspace's local snapshots, newest first",
		Args: func(c *cobra.Command, args []string) error {
			if logWorkspace != "" && !storage.ValidUUID(logWorkspace) {
				return usageError{errors.New("invalid workspace UUID")}
			}
			if len(args) != 0 || limit < 1 || limit > 1000 {
				return usageError{errors.New("log accepts no paths; -n must be between 1 and 1000")}
			}
			if c.Flags().Changed("writer") && logWriter == "" {
				return usageError{errors.New("--writer requires a nonempty ID")}
			}
			if err := history.ValidateWriter(logWriter); err != nil {
				return usageError{err}
			}
			if logPath != "" {
				if err := storage.ValidateSelectionPath(logPath); err != nil {
					return usageError{err}
				}
			}
			return nil
		},
		RunE: func(c *cobra.Command, _ []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				entries, err := history.LogFiltered(c.Context(), r, cwd, logWorkspace, limit, history.LogFilter{Branch: logBranch, MainCommit: logMainCommit, WithStat: logStat, Writer: logWriter, Path: logPath, WithProvenance: logProvenance})
				if err != nil {
					return err
				}
				if jsonEnabled(c) {
					items := make([]map[string]any, 0, len(entries))
					for _, entry := range entries {
						item := map[string]any{"id": entry.ID, "timestamp": entry.Timestamp, "message": entry.Message, "writer": entry.Writer, "changedPaths": append([]string{}, entry.ChangedPaths...)}
						if logStat {
							item["stat"] = entry.StatData
						}
						items = append(items, item)
					}
					setJSON(c, map[string]any{"entries": items})
					return nil
				}
				if len(entries) == 0 && (logBranch != "" || logMainCommit != "" || logWriter != "" || logPath != "") {
					_, err = fmt.Fprintln(c.OutOrStdout(), "No matching local snapshots.")
					return err
				}
				_, err = fmt.Fprint(c.OutOrStdout(), history.FormatLog(entries))
				return err
			})
		},
	}
	logCommand.Flags().IntVarP(&limit, "max-count", "n", 20, "Maximum snapshots (1–1000)")
	logCommand.Flags().StringVar(&logWorkspace, "workspace", "", "Source workspace UUID, including retained history")
	logCommand.Flags().StringVar(&logBranch, "branch", "", "Snapshots recorded on this branch (short name or refs/heads/name)")
	logCommand.Flags().StringVar(&logMainCommit, "main-commit", "", "Snapshots recorded at this full mainline commit ID")
	logCommand.Flags().BoolVar(&logStat, "stat", false, "Show changed-file and line counts for each snapshot")
	logCommand.Flags().StringVar(&logWriter, "writer", "", "Only snapshots attributed to this caller-supplied writer ID")
	logCommand.Flags().StringVar(&logPath, "path", "", "Only snapshots that changed this exact repository-relative path")
	logCommand.Flags().BoolVar(&logProvenance, "provenance", false, "Show writer ID and changed paths for each snapshot")
	root.AddCommand(logCommand)
	root.AddCommand(restoreCommand())
	doctorCommand := &cobra.Command{Use: "doctor", Short: "Check local history, configuration and recovery state without changing them",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{errors.New("doctor accepts no arguments")}
			}
			return nil
		},
		RunE: func(c *cobra.Command, _ []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				report, err := doctor.Run(c.Context(), r, cwd)
				if setJSON(c, report) {
					if err != nil {
						return err
					}
					if report.HasIssues() {
						return storage.ErrConflict
					}
					return nil
				}
				if err != nil {
					return err
				}
				_, err = fmt.Fprint(c.OutOrStdout(), report.Human())
				if err != nil {
					return err
				}
				if report.HasIssues() {
					return storage.ErrConflict
				}
				return nil
			})
		},
	}
	root.AddCommand(doctorCommand)

	workspaceCommand := &cobra.Command{Use: "workspace", Short: "List local workspaces or retain a removed workspace's history"}
	workspaceCommand.AddCommand(&cobra.Command{Use: "list", Short: "List workspace identities, registration and availability",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{errors.New("workspace list accepts no arguments")}
			}
			return nil
		},
		RunE: func(c *cobra.Command, _ []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				entries, err := configtx.ListWorkspaces(c.Context(), r, cwd)
				if err != nil {
					return err
				}
				if setJSON(c, map[string]any{"workspaces": entries}) {
					return nil
				}
				for _, entry := range entries {
					if _, err = fmt.Fprintf(c.OutOrStdout(), "%s %s registered=%t available=%t locked=%t moved=%t root=%q\n", entry.ID, entry.State, entry.Registered, entry.Available, entry.Locked, entry.Moved, entry.Root); err != nil {
						return err
					}
				}
				return nil
			})
		},
	})
	workspaceCommand.AddCommand(&cobra.Command{Use: "retire <uuid>", Short: "Retain an unregistered workspace and reconcile shared ignore rules",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 1 || !storage.ValidUUID(args[0]) {
				return usageError{errors.New("provide one valid workspace UUID")}
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				result, err := configtx.Retire(c.Context(), r, cwd, args[0])
				if setJSON(c, map[string]any{"workspaceId": args[0], "state": "retained", "transactionId": result.TransactionID}, result.Warnings...) {
					return err
				}
				for _, warning := range result.Warnings {
					fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
				}
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(c.OutOrStdout(), "Workspace %s retained; history preserved.\n", args[0])
				return err
			})
		},
	})
	var removeWorktreeForce bool
	removeWorktree := &cobra.Command{Use: "remove <path>", Short: "Snapshot, remove, and retire a linked worktree", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 1 || args[0] == "" {
			return usageError{errors.New("workspace remove requires one linked worktree path")}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			result, err := automatic.RemoveWorktree(c.Context(), r, cwd, args[0], removeWorktreeForce)
			if setJSON(c, map[string]any{"workspaceId": result.WorkspaceID, "snapshotChanged": result.SnapshotChanged, "removed": result.Removed, "retired": result.Retired, "transactionId": result.TransactionID}, result.Warnings...) {
				return err
			}
			for _, warning := range result.Warnings {
				fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
			}
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Removed and retained workspace %s; history preserved.\n", result.WorkspaceID)
			return err
		})
	}}
	removeWorktree.Flags().BoolVar(&removeWorktreeForce, "force", false, "Allow Git to remove a dirty linked worktree after saving selected files")
	workspaceCommand.AddCommand(removeWorktree)
	var pruneDryRun bool
	prune := &cobra.Command{Use: "prune", Short: "Retire unregistered, unavailable workspaces while preserving history",
		Args: func(_ *cobra.Command, args []string) error {
			if len(args) != 0 {
				return usageError{errors.New("workspace prune accepts no arguments")}
			}
			return nil
		},
		RunE: func(c *cobra.Command, _ []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				result, err := configtx.Prune(c.Context(), r, cwd, pruneDryRun)
				if setJSON(c, map[string]any{"dryRun": pruneDryRun, "candidates": result.Candidates, "retired": result.Retired}, result.Warnings...) {
					return err
				}
				for _, warning := range result.Warnings {
					fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
				}
				if len(result.Candidates) == 0 && err == nil {
					_, err = fmt.Fprintln(c.OutOrStdout(), "No missing workspaces.")
					return err
				}
				ids := result.Retired
				if pruneDryRun {
					ids = result.Candidates
				}
				for _, id := range ids {
					if pruneDryRun {
						_, err = fmt.Fprintf(c.OutOrStdout(), "Would retire workspace %s; history preserved.\n", id)
					} else {
						_, err = fmt.Fprintf(c.OutOrStdout(), "Workspace %s retained; history preserved.\n", id)
					}
					if err != nil {
						return err
					}
				}
				return err
			})
		},
	}
	prune.Flags().BoolVar(&pruneDryRun, "dry-run", false, "List candidates without changing workspace records or ignores")
	workspaceCommand.AddCommand(prune)
	root.AddCommand(workspaceCommand)
	root.AddCommand(hookCommand())
	root.AddCommand(autoCommand())
	var resetHard bool
	reset := &cobra.Command{Use: "reset --hard [revision]", Short: "Save selected files, then hard-reset the main worktree",
		Args: func(_ *cobra.Command, args []string) error {
			if !resetHard || len(args) > 1 {
				return usageError{errors.New("reset requires --hard and at most one revision")}
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				revision := "HEAD"
				if len(args) == 1 {
					revision = args[0]
				}
				result, err := automatic.ResetHard(c.Context(), r, cwd, revision)
				if setJSON(c, map[string]any{"revision": revision, "commit": result.Commit, "snapshotId": result.SnapshotID, "snapshotChanged": result.SnapshotChanged, "reset": result.Reset}, result.Warnings...) {
					return err
				}
				for _, warning := range result.Warnings {
					fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
				}
				if err != nil {
					return err
				}
				if result.SnapshotChanged {
					if _, err := fmt.Fprintf(c.OutOrStdout(), "Saved local snapshot %s before reset.\n", result.SnapshotID); err != nil {
						return err
					}
				}
				_, err = fmt.Fprintf(c.OutOrStdout(), "Reset main worktree to %s.\n", result.Commit)
				return err
			})
		},
	}
	reset.Flags().BoolVar(&resetHard, "hard", false, "Reset index and working tree after saving selected files")
	root.AddCommand(reset)
	root.AddCommand(guardCommand())
	root.AddCommand(overlayCommand())
	var resume, rollback, abandon, list, discardRecovery bool
	recover := &cobra.Command{Use: "recover <transaction-id> --resume|--rollback|--abandon", Short: "Resume, roll back or abandon an interrupted configuration, seed, restore, encryption or initialization transaction",
		Args: func(_ *cobra.Command, args []string) error {
			if list {
				if len(args) != 0 || resume || rollback || abandon || discardRecovery {
					return usageError{errors.New("--list cannot be combined with an ID or recovery direction")}
				}
				return nil
			}
			directions := 0
			for _, set := range []bool{resume, rollback, abandon} {
				if set {
					directions++
				}
			}
			if len(args) != 1 || directions != 1 {
				return usageError{errors.New("provide a transaction ID and exactly one of --resume, --rollback or --abandon")}
			}
			if discardRecovery && !abandon {
				return usageError{errors.New("--discard-recovery requires --abandon")}
			}
			if !storage.ValidUUID(args[0]) {
				return usageError{errors.New("invalid transaction ID")}
			}
			return nil
		},
		RunE: func(c *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			r, err := gitexec.New(os.Environ())
			if err != nil {
				return err
			}
			defer r.Close()
			if list {
				// PendingJournals needs no session, so it also lists the
				// encryption and initialization journals that block one.
				pending, err := configtx.PendingJournals(c.Context(), r, cwd)
				if err != nil {
					return err
				}
				ids := []string{}
				transactions := []map[string]any{}
				for _, p := range pending {
					next, state := p.Next()
					// Scripts pass transactionIds to recover, so only valid
					// transaction IDs are listed there; an unknown or
					// unreadable entry appears only in transactions. Each
					// entry's next command says how to resolve it (an
					// encryption journal through encrypt recover).
					if storage.ValidUUID(p.ID) {
						ids = append(ids, p.ID)
					}
					transactions = append(transactions, map[string]any{"id": p.ID, "kind": p.Kind, "workspaceId": p.WorkspaceID, "root": p.Root, "state": state, "next": next})
				}
				if setJSON(c, map[string]any{"transactionIds": ids, "transactions": transactions}) {
					return nil
				}
				if len(pending) == 0 {
					_, err = fmt.Fprintln(c.OutOrStdout(), "No unfinished transactions.")
					return err
				}
				for _, p := range pending {
					next, state := p.Next()
					if _, err = fmt.Fprintf(c.OutOrStdout(), "%s %s: %s.\n  %s\n", p.ID, p.Kind, state, next); err != nil {
						return err
					}
				}
				return nil
			}
			if abandon {
				return abandonTransaction(c, r, cwd, args[0], discardRecovery)
			}
			kind, err := configtx.TransactionKind(c.Context(), r, cwd, args[0])
			if err != nil {
				return err
			}
			if kind == "seed" {
				result, err := seed.Recover(c.Context(), r, cwd, args[0], rollback)
				if setJSON(c, map[string]any{"transactionId": args[0], "kind": kind, "rolledBack": rollback, "workspaceId": result.WorkspaceID}, result.Warnings...) {
					return err
				}
				for _, warning := range result.Warnings {
					fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
				}
				if err != nil {
					return err
				}
				outcome := "committed"
				if rollback {
					outcome = "rolled back"
				}
				_, err = fmt.Fprintf(c.OutOrStdout(), "Seed transaction %s %s.\n", args[0], outcome)
				return err
			}
			if kind == "restore" {
				result, err := restore.Recover(c.Context(), r, cwd, args[0], rollback)
				if setJSON(c, map[string]any{"transactionId": args[0], "kind": kind, "rolledBack": rollback, "recoverySnapshot": result.RecoverySnapshot}) {
					return err
				}
				if err != nil {
					return err
				}
				outcome := "committed"
				if rollback {
					outcome = "rolled back"
				}
				_, err = fmt.Fprintf(c.OutOrStdout(), "Restore transaction %s %s.\n", args[0], outcome)
				return err
			}
			result, err := configtx.Recover(c.Context(), r, cwd, args[0], rollback)
			if setJSON(c, map[string]any{"transactionId": args[0], "kind": kind, "rolledBack": rollback, "workspaceId": result.WorkspaceID}, result.Warnings...) {
				return err
			}
			for _, warning := range result.Warnings {
				fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
			}
			if err != nil {
				return err
			}
			outcome := "committed"
			if rollback {
				outcome = "rolled back"
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Configuration transaction %s %s.\n", args[0], outcome)
			return err
		},
	}
	recover.Flags().BoolVar(&resume, "resume", false, "Finish the recorded transaction")
	recover.Flags().BoolVar(&rollback, "rollback", false, "Restore the recorded pre-transaction state")
	recover.Flags().BoolVar(&abandon, "abandon", false, "Give up the transaction, leaving files as they are and quarantining unpreserved bytes")
	recover.Flags().BoolVar(&list, "list", false, "List unfinished transaction IDs")
	recover.Flags().BoolVar(&discardRecovery, "discard-recovery", false, "With --abandon, also delete a recovery ref that keeps the previous file versions of a restore whose journal is quarantined")
	root.AddCommand(recover)
	return root
}

func hookCommand() *cobra.Command {
	hooks := &cobra.Command{Use: "hook", Short: "Manage selected-path commit and push guards"}
	for _, action := range []string{"install", "uninstall"} {
		action := action
		var preCommit, prePush, allowShared bool
		command := &cobra.Command{Use: action, Short: action + " selected-path guards",
			Args: func(_ *cobra.Command, args []string) error {
				if len(args) != 0 {
					return usageError{errors.New("hook " + action + " accepts no arguments")}
				}
				return nil
			},
			RunE: func(c *cobra.Command, _ []string) error {
				return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
					var paths, warnings []string
					var err error
					if action == "install" {
						executable, e := os.Executable()
						if e != nil {
							return e
						}
						paths, warnings, err = hook.Install(c.Context(), r, cwd, executable, preCommit, prePush, allowShared)
					} else {
						paths, warnings, err = hook.Uninstall(c.Context(), r, cwd, preCommit, prePush)
					}
					if setJSON(c, map[string]any{"action": action, "hookPaths": paths}, warnings...) {
						return err
					}
					for _, warning := range warnings {
						fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
					}
					if err != nil {
						return err
					}
					for _, p := range paths {
						fmt.Fprintf(c.OutOrStdout(), "%s guard at %s.\n", strings.Title(action)+"ed", p)
					}
					return nil
				})
			},
		}
		command.Flags().BoolVar(&preCommit, "pre-commit", false, "Select the pre-commit guard")
		command.Flags().BoolVar(&prePush, "pre-push", false, "Select the pre-push guard")
		if action == "install" {
			command.Flags().BoolVar(&allowShared, "allow-shared-hooks-dir", false, "Install into a hooks directory outside this repository, such as a global core.hooksPath")
			command.Flags().Bool("chain", false, "Deprecated: existing shell hooks are preserved automatically")
			_ = command.Flags().MarkHidden("chain")
		}
		hooks.AddCommand(command)
	}
	hooks.AddCommand(&cobra.Command{Use: "status", Short: "Show installed guards", Args: noArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				status, err := hook.Inspect(c.Context(), r, cwd)
				if err != nil {
					return err
				}
				if setJSON(c, map[string]any{"currentDirectory": status.Current, "installed": status.Installed, "installedByOtherRepository": append([]string{}, status.Others...), "disabled": append([]string{}, status.Disabled...), "previousDirectories": status.Previous, "location": status.Location.Kind, "worktree": status.Location.Worktree, "tracked": status.Tracked}) {
					return nil
				}
				names := []string{"pre-commit", "pre-push"}
				for _, name := range names {
					fmt.Fprintf(c.OutOrStdout(), "%s: %s (%s)\n", name, hookState(status, name), status.Current)
				}
				printHookLocation(c, status, names)
				for _, p := range status.Previous {
					fmt.Fprintf(c.OutOrStdout(), "previous installation: %s\n", p)
				}
				return nil
			})
		},
	})
	return hooks
}

func autoCommand() *cobra.Command {
	auto := &cobra.Command{Use: "auto", Short: "Capture local snapshots on Git events or a timer"}
	for _, action := range []string{"install", "uninstall"} {
		action := action
		var allowShared bool
		command := &cobra.Command{Use: action, Args: noArgs, Short: action + " automatic snapshot hooks", RunE: func(c *cobra.Command, _ []string) error {
			return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
				var paths, warnings []string
				var err error
				if action == "install" {
					executable, e := os.Executable()
					if e != nil {
						return e
					}
					paths, warnings, err = hook.InstallSnapshots(c.Context(), r, cwd, executable, allowShared)
				} else {
					paths, warnings, err = hook.UninstallSnapshots(c.Context(), r, cwd)
				}
				if setJSON(c, map[string]any{"action": action, "hookPaths": paths}, warnings...) {
					return err
				}
				for _, warning := range warnings {
					fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
				}
				if err != nil {
					return err
				}
				for _, path := range paths {
					if _, err := fmt.Fprintf(c.OutOrStdout(), "%s snapshot hook at %s.\n", strings.Title(action)+"ed", path); err != nil {
						return err
					}
				}
				return nil
			})
		}}
		if action == "install" {
			command.Flags().BoolVar(&allowShared, "allow-shared-hooks-dir", false, "Install into a hooks directory outside this repository, such as a global core.hooksPath")
		}
		auto.AddCommand(command)
	}
	auto.AddCommand(&cobra.Command{Use: "status", Args: noArgs, Short: "Show automatic snapshot hooks", RunE: func(c *cobra.Command, _ []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			status, err := hook.Inspect(c.Context(), r, cwd)
			if err != nil {
				return err
			}
			if setJSON(c, map[string]any{"currentDirectory": status.Current, "installed": status.Installed, "installedByOtherRepository": append([]string{}, status.Others...), "disabled": append([]string{}, status.Disabled...), "location": status.Location.Kind, "worktree": status.Location.Worktree, "tracked": status.Tracked}) {
				return nil
			}
			names := []string{"post-commit", "post-checkout", "pre-rebase", "post-rewrite"}
			for _, name := range names {
				if _, err := fmt.Fprintf(c.OutOrStdout(), "%s: %s (%s)\n", name, hookState(status, name), status.Current); err != nil {
					return err
				}
			}
			printHookLocation(c, status, names)
			return nil
		})
	}})
	var interval time.Duration
	watch := &cobra.Command{Use: "watch", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 0 || interval < automatic.MinWatchInterval || interval > automatic.MaxWatchInterval {
			return usageError{errors.New("auto watch accepts no arguments and requires --interval between 1s and 24h")}
		}
		return nil
	}, Short: "Capture changed selected files periodically until stopped", RunE: func(c *cobra.Command, _ []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			count := 0
			latest := ""
			var warnings []string
			err := automatic.Watch(c.Context(), r, cwd, interval, func(result history.Result) error {
				count++
				latest = result.ID
				if jsonEnabled(c) {
					warnings = append(warnings, result.Warnings...)
					return nil
				}
				for _, warning := range result.Warnings {
					fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
				}
				_, err := fmt.Fprintf(c.OutOrStdout(), "Saved local snapshot %s.\n", result.ID)
				return err
			}, func(conflict error) {
				warning := fmt.Sprintf("snapshot skipped: %v; retrying at the next check", conflict)
				if jsonEnabled(c) {
					warnings = append(warnings, warning)
				} else {
					fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
				}
			})
			if setJSON(c, map[string]any{"snapshots": count, "latestSnapshot": latest, "interval": interval.String()}, warnings...) {
				return err
			}
			return err
		})
	}}
	watch.Flags().DurationVar(&interval, "interval", 5*time.Minute, "Time between change checks (1s to 24h)")
	auto.AddCommand(watch)
	auto.AddCommand(&cobra.Command{Use: "event <name>", Hidden: true, Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 1 || !map[string]bool{"post-commit": true, "post-checkout": true, "pre-rebase": true, "post-rewrite": true}[args[0]] {
			return usageError{errors.New("invalid automatic snapshot event")}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		r, err := guardRunner()
		if err != nil {
			return err
		}
		defer r.Close()
		warnings, err := automatic.Event(c.Context(), r, cwd, args[0])
		for _, warning := range warnings {
			fmt.Fprintln(c.ErrOrStderr(), "git-local: warning:", warning)
		}
		return err
	}})
	return auto
}

func guardCommand() *cobra.Command {
	guards := &cobra.Command{Use: "guard", Short: "Check selected paths before commit or push"}
	guards.AddCommand(&cobra.Command{Use: "staged", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return guardRepositoryError{err}
		}
		index := os.Getenv("GIT_INDEX_FILE")
		r, err := guardRunner()
		if err != nil {
			return guardRepositoryError{err}
		}
		defer r.Close()
		if err := checkHookRepository(c.Context(), r, cwd); err != nil {
			return guardRepositoryError{err}
		}
		findings, err := guard.Staged(c.Context(), r, cwd, index)
		if err != nil {
			return guardRepositoryError{err}
		}
		if setJSON(c, map[string]any{"findings": guardFindingsJSON(findings)}) {
			if len(findings) > 0 {
				return guardViolation{errors.New("selected paths are staged")}
			}
			return nil
		}
		for _, f := range findings {
			if f.Staging {
				fmt.Fprintf(c.ErrOrStderr(), "git-local: refusing to commit %q: it is a git-local restore or seed staging file and may hold private content.\n  Unstage it: git restore --staged -- %s\n", f.Path, shellQuote(f.Path))
			} else if f.OverlayName != "" && f.OverlayReason == guard.OverlapReason {
				fmt.Fprintf(c.ErrOrStderr(), "git-local: refusing to commit %q: %s (overlay %q, workspace %s).\n  Re-staging can't help; save the overlay again against the current file: %s.\n", f.Path, f.OverlayReason, f.OverlayName, f.WorkspaceID, resaveSteps(f.OverlayName, f.Path))
			} else if f.OverlayName != "" {
				fmt.Fprintf(c.ErrOrStderr(), "git-local: refusing to commit %q: %s (overlay %q, workspace %s).\n  Re-stage with git add -- %s, or update the overlay after resolving the conflict.\n", f.Path, f.OverlayReason, f.OverlayName, f.WorkspaceID, shellQuote(f.Path))
			} else {
				fmt.Fprintf(c.ErrOrStderr(), "git-local: refusing to commit %q: selected for local history by workspace %s (%s).\n  Unstage it: git restore --staged -- %s\n", f.Path, f.WorkspaceID, f.WorkspaceRoot, shellQuote(f.Path))
			}
		}
		if len(findings) > 0 {
			return guardViolation{errors.New("selected paths are staged")}
		}
		return nil
	}})
	guards.AddCommand(&cobra.Command{Use: "push <remote>", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return usageError{errors.New("guard push requires a remote name")}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		cwd, err := os.Getwd()
		if err != nil {
			return guardRepositoryError{err}
		}
		r, err := guardRunner()
		if err != nil {
			return guardRepositoryError{err}
		}
		defer r.Close()
		if err := checkHookRepository(c.Context(), r, cwd); err != nil {
			return guardRepositoryError{err}
		}
		findings, err := guard.Push(c.Context(), r, cwd, args[0], c.InOrStdin())
		if err != nil {
			return guardRepositoryError{err}
		}
		if setJSON(c, map[string]any{"remote": args[0], "findings": guardFindingsJSON(findings)}) {
			if len(findings) > 0 {
				return guardViolation{errors.New("selected paths are present in pushed commits")}
			}
			return nil
		}
		for _, f := range findings {
			if f.Staging {
				fmt.Fprintf(c.ErrOrStderr(), "git-local: refusing to push commit %s: %q is a git-local restore or seed staging file and may hold private content.\n  Rewrite the commit to remove this path before pushing (for example, edit it with git rebase -i %s^).\n", f.Commit, f.Path, f.Commit)
			} else if f.OverlayName != "" {
				fmt.Fprintf(c.ErrOrStderr(), "git-local: refusing to push commit %s: %q %s (overlay %q, workspace %s).\n  Rewrite the commit to remove the overlay before pushing.\n", f.Commit, f.Path, f.OverlayReason, f.OverlayName, f.WorkspaceID)
			} else {
				fmt.Fprintf(c.ErrOrStderr(), "git-local: refusing to push commit %s: %q is selected for local history by workspace %s (%s).\n  Rewrite the commit to remove this path before pushing (for example, edit it with git rebase -i %s^).\n", f.Commit, f.Path, f.WorkspaceID, f.WorkspaceRoot, f.Commit)
			}
		}
		if len(findings) > 0 {
			return guardViolation{errors.New("selected paths are present in pushed commits")}
		}
		return nil
	}})
	return guards
}

func hookState(status hook.Status, name string) string {
	switch {
	case slices.Contains(status.Disabled, name):
		return "installed, but the hook file isn't executable, so Git skips it"
	case slices.Contains(status.Others, name):
		return "installed by another repository"
	case status.Installed[name]:
		return "installed"
	}
	return "missing"
}

// printHookLocation explains a hooks directory that install refuses by
// default. It suggests the flag that allows a shared one only while one of
// the named hooks is missing.
func printHookLocation(c *cobra.Command, status hook.Status, names []string) {
	missing := false
	for _, name := range names {
		missing = missing || !status.Installed[name]
	}
	switch {
	case status.Location.Kind == hook.LocationWorktree:
		fmt.Fprintf(c.OutOrStdout(), "hooks directory is inside the worktree %s; hook install refuses it because hook scripts there can be committed\n", status.Location.Worktree)
	case status.Location.Kind == hook.LocationShared && missing:
		fmt.Fprintf(c.OutOrStdout(), "hooks directory is shared: it is outside this repository and other repositories may use it; hook install needs --allow-shared-hooks-dir\n")
	case status.Location.Kind == hook.LocationShared:
		fmt.Fprintf(c.OutOrStdout(), "hooks directory is shared: it is outside this repository and other repositories may use it\n")
	}
	if len(status.Tracked) > 0 {
		fmt.Fprintf(c.OutOrStdout(), "tracked: %s\n", strings.Join(status.Tracked, ", "))
	}
}

// checkHookRepository refuses a guard run whose GIT_DIR names a repository
// other than the one the working directory belongs to, as with git
// --git-dir=A --work-tree=B. The guard finds its repository from the working
// directory, so it would otherwise check the wrong one.
func checkHookRepository(ctx context.Context, r *gitexec.Runner, cwd string) error {
	gitDir := os.Getenv("GIT_DIR")
	if gitDir == "" {
		return nil
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(cwd, gitDir)
	}
	b, err := r.Guard(ctx, cwd, "", "rev-parse", "--absolute-git-dir")
	found := strings.TrimSpace(string(b))
	if err == nil {
		want, wantErr := os.Stat(gitDir)
		got, gotErr := os.Stat(found)
		if wantErr == nil && gotErr == nil && os.SameFile(want, got) {
			return nil
		}
	} else {
		found = "no repository"
	}
	return fmt.Errorf("Git runs this hook for the repository at %s, but the working directory %s belongs to %s; run Git from that repository's worktree instead of with --git-dir and --work-tree", gitDir, cwd, found)
}

func guardRunner() (*gitexec.Runner, error) {
	env := []string{}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			env = append(env, entry)
		}
	}
	return gitexec.New(env)
}
func overlayCommand() *cobra.Command {
	command := &cobra.Command{Use: "overlay", Short: "Keep named local patches on tracked files out of mainline commits"}
	command.AddCommand(&cobra.Command{Use: "status", Short: "Inspect active patches and upstream conflicts", Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			statuses, err := overlay.Inspect(c.Context(), r, cwd)
			if err != nil {
				return err
			}
			if setJSON(c, map[string]any{"overlays": overlayStatusesJSON(statuses)}) {
				return nil
			}
			if len(statuses) == 0 {
				_, err = fmt.Fprintln(c.OutOrStdout(), "No active overlays.")
				return err
			}
			for _, s := range statuses {
				if _, err := fmt.Fprintf(c.OutOrStdout(), "%s v%d %q: index %s; working file %s", s.Name, s.Version, s.Path, s.Index, s.Working); err != nil {
					return err
				}
				if s.UpstreamRef != "" {
					if _, err := fmt.Fprintf(c.OutOrStdout(), "; upstream %s %s", s.UpstreamRef, s.Upstream); err != nil {
						return err
					}
					if s.UpstreamBlob != "" {
						if _, err := fmt.Fprintf(c.OutOrStdout(), " (blob %s)", s.UpstreamBlob); err != nil {
							return err
						}
					}
				}
				if s.Adopted > 0 {
					if _, err := fmt.Fprintf(c.OutOrStdout(), "\n  %d of %d hunks are already upstream, so the overlay is stale; save it again without them: %s", s.Adopted, s.Hunks, resaveSteps(s.Name, s.Path)); err != nil {
						return err
					}
				}
				if _, err := fmt.Fprintln(c.OutOrStdout()); err != nil {
					return err
				}
			}
			return nil
		})
	}})
	var showVersion uint32
	show := &cobra.Command{Use: "show <name>", Short: "Print a saved overlay version without changing files", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return usageError{errors.New("overlay show requires one name")}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			p, err := overlay.NewProcessor(c.Context(), r, cwd, "")
			if err != nil {
				return err
			}
			if p.WorkspaceID == "" {
				return errors.New("worktree needs git local init before showing an overlay")
			}
			v, err := overlay.LoadVersion(c.Context(), p.Common, p.WorkspaceID, args[0], showVersion)
			if err != nil {
				return err
			}
			if setJSON(c, map[string]any{"name": args[0], "version": v.Manifest.Version, "path": v.Manifest.Path, "encoding": "base64", "contentBase64": base64.StdEncoding.EncodeToString(v.Local), "size": len(v.Local)}) {
				return nil
			}
			_, err = c.OutOrStdout().Write(v.Local)
			return err
		})
	}}
	show.Flags().Uint32Var(&showVersion, "version", 0, "Saved version number (default: latest)")
	command.AddCommand(show)
	var removeWorkspace string
	remove := &cobra.Command{Use: "remove <name>", Short: "Deactivate a patch while keeping its file and versions", Args: func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return usageError{errors.New("overlay remove requires one name")}
		}
		if removeWorkspace != "" && !storage.ValidUUID(removeWorkspace) {
			return usageError{errors.New("invalid workspace UUID")}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			m, err := overlay.RemoveFor(c.Context(), r, cwd, removeWorkspace, args[0])
			if err != nil {
				return err
			}
			if setJSON(c, map[string]any{"name": m.Name, "path": m.Path, "version": m.Version, "state": "removed"}) {
				return nil
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Removed overlay %s for %s; working file and saved versions remain.\n", m.Name, m.Path)
			return err
		})
	}}
	remove.Flags().StringVar(&removeWorkspace, "workspace", "", "Remove an overlay of this retained workspace UUID (a removed worktree)")
	command.AddCommand(remove)
	command.AddCommand(&cobra.Command{Use: "add <name> -- <file>", Short: "Save a versioned local patch for a tracked text file", Args: func(c *cobra.Command, args []string) error {
		if len(args) != 2 || c.Flags().ArgsLenAtDash() != 1 {
			return usageError{errors.New("overlay add requires a name and -- <file>")}
		}
		return nil
	}, RunE: func(c *cobra.Command, args []string) error {
		return inspectCommand(c, func(r *gitexec.Runner, cwd string) error {
			executable, err := os.Executable()
			if err != nil {
				return err
			}
			m, err := overlay.Add(c.Context(), r, cwd, args[0], args[1], executable)
			if err != nil {
				return err
			}
			if setJSON(c, map[string]any{"name": m.Name, "path": m.Path, "version": m.Version, "state": "active"}) {
				return nil
			}
			_, err = fmt.Fprintf(c.OutOrStdout(), "Saved overlay %s version %d for %s.\n", m.Name, m.Version, m.Path)
			return err
		})
	}})
	command.AddCommand(&cobra.Command{Use: "filter-process", Hidden: true, Args: noArgs, RunE: func(c *cobra.Command, _ []string) error {
		if jsonEnabled(c) {
			return usageError{errors.New("overlay filter-process is a Git protocol command and does not accept --json")}
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		r, err := guardRunner()
		if err != nil {
			return err
		}
		defer r.Close()
		processor, err := overlay.NewProcessor(c.Context(), r, cwd, overlay.IndexFromEnvironment())
		if err != nil {
			return err
		}
		return overlay.ServeFilter(c.Context(), c.InOrStdin(), c.OutOrStdout(), c.ErrOrStderr(), processor.Transform)
	}})
	return command
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// resaveSteps tells how to save an overlay again against the current file.
func resaveSteps(name, path string) string {
	return fmt.Sprintf("git local overlay remove %s, edit %s to the local value you want, then git local overlay add %s -- %s", name, path, name, shellQuote(path))
}

func Execute(ctx context.Context, args []string, out, errOut io.Writer) int {
	completion := len(args) > 0 && (args[0] == cobra.ShellCompRequestCmd || args[0] == cobra.ShellCompNoDescRequestCmd)
	// A completion request carries the command line being completed, which
	// may include --json; the shell still needs plain completion lines.
	jsonMode := !completion && wantsJSON(args)
	commandOut, commandErr, capturedOut, capturedErr := newJSONBuffers(jsonMode, out, errOut)
	state := &jsonState{}
	if ctx.Err() != nil {
		if jsonMode {
			if err := writeJSON(out, "git local", "canceled", ctx.Err(), state, "", ""); err != nil {
				fmt.Fprintf(errOut, "git local: write JSON response: %v\n", err)
			}
		} else {
			fmt.Fprintln(errOut, "git local: canceled")
		}
		return 1
	}
	c := New(commandOut, commandErr)
	c.SetArgs(args)
	c.InitDefaultHelpCmd()
	c.InitDefaultCompletionCmd()
	enforceUsage(c)
	// Cobra adds its hidden completion request commands only while executing,
	// so they skip the unknown-command check. A help topic must name a command.
	var findErr error
	topic := args
	for len(topic) > 0 && (topic[0] == "--json" || strings.HasPrefix(topic[0], "--json=")) {
		topic = topic[1:]
	}
	switch {
	case completion:
	case len(topic) > 0 && topic[0] == "help":
		var found *cobra.Command
		var rest []string
		found, rest, findErr = c.Find(topic[1:])
		if findErr == nil && len(rest) > 0 && found.HasSubCommands() {
			findErr = unknownCommand(found, rest[0])
		}
	default:
		_, _, findErr = c.Find(args)
	}
	if findErr != nil {
		if jsonMode {
			if err := writeJSON(out, "git local", "usage", findErr, state, "", ""); err != nil {
				fmt.Fprintf(errOut, "git local: write JSON response: %v\n", err)
				return 1
			}
		} else {
			fmt.Fprintf(errOut, "git local: %v\n", findErr)
		}
		return 2
	}
	command, err := c.ExecuteContextC(jsonContext(ctx, state))
	code, kind := 0, ""
	var usage usageError
	var repositoryError guardRepositoryError
	var violation guardViolation
	if err != nil {
		switch {
		case errors.As(err, &usage):
			code, kind = 2, "usage"
		case errors.Is(err, context.Canceled):
			code, kind = 1, "canceled"
		case errors.Is(err, storage.ErrConflict), errors.As(err, &repositoryError):
			code, kind = 3, "conflict"
		case errors.As(err, &violation):
			code, kind = 1, "guard_violation"
		default:
			code, kind = 1, "operation"
		}
	}
	if jsonMode {
		if writeErr := writeJSON(out, jsonCommand(command), kind, err, state, capturedOut.String(), capturedErr.String()); writeErr != nil {
			fmt.Fprintf(errOut, "git local: write JSON response: %v\n", writeErr)
			return 1
		}
	} else if err != nil {
		fmt.Fprintf(errOut, "git local: %v\n", err)
	}
	return code
}

// enforceUsage makes every command honor the usage exit code: a group
// command given an unknown subcommand, and any failed argument check, report
// a usageError instead of printing help or a plain error.
func enforceUsage(c *cobra.Command) {
	if c.HasSubCommands() && !c.Runnable() {
		c.Args = cobra.ArbitraryArgs
		c.RunE = func(c *cobra.Command, args []string) error {
			if len(args) != 0 {
				return unknownCommand(c, args[0])
			}
			return c.Help()
		}
	} else if validate := c.Args; validate != nil {
		c.Args = func(c *cobra.Command, args []string) error {
			err := validate(c, args)
			var usage usageError
			if err != nil && !errors.As(err, &usage) {
				return usageError{err}
			}
			return err
		}
	}
	for _, child := range c.Commands() {
		enforceUsage(child)
	}
}

// noArgs rejects arguments as invalid usage.
func noArgs(c *cobra.Command, args []string) error {
	if len(args) != 0 {
		return usageError{fmt.Errorf("%s accepts no arguments", strings.Replace(c.CommandPath(), "git-local", "git local", 1))}
	}
	return nil
}

func unknownCommand(c *cobra.Command, name string) error {
	message := fmt.Sprintf("unknown command %q for %q", name, strings.Replace(c.CommandPath(), "git-local", "git local", 1))
	if suggestions := c.SuggestionsFor(name); len(suggestions) != 0 {
		message += "; did you mean " + strings.Join(suggestions, " or ") + "?"
	}
	return usageError{errors.New(message)}
}

func inspectCommand(c *cobra.Command, fn func(*gitexec.Runner, string) error) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	r, err := gitexec.New(os.Environ())
	if err != nil {
		return err
	}
	defer r.Close()
	return fn(r, cwd)
}
func inspectionConflicts(s history.StatusResult) error {
	for _, f := range s.Files {
		if f.Code == "C" {
			return fmt.Errorf("%w: selected paths have conflicts; resolve or untrack them", storage.ErrConflict)
		}
	}
	return nil
}

// abandonTransaction gives up a transaction that neither resume nor rollback
// can finish. Each kind's handler takes the repository lock and runs its own
// safety checks.
func abandonTransaction(c *cobra.Command, r *gitexec.Runner, cwd, id string, discardRecovery bool) error {
	kind, err := configtx.JournalKind(c.Context(), r, cwd, id)
	if errors.Is(err, os.ErrNotExist) {
		// A restore killed before writing its journal leaves only its
		// recovery ref.
		kind = "recovery-ref"
		err = nil
	}
	if err != nil {
		return err
	}
	var result configtx.AbandonResult
	switch kind {
	case "recovery-ref":
		result, err = restore.AbandonOrphanRef(c.Context(), r, cwd, id, discardRecovery)
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no transaction %s", id)
		}
	case "config", "seed":
		result, err = configtx.Abandon(c.Context(), r, cwd, id)
	case "restore":
		result, err = restore.Abandon(c.Context(), r, cwd, id)
	case "encrypt":
		result, err = encryption.AbandonMigration(c.Context(), r, cwd, id)
	case "init":
		result.TransactionID, result.Kind = id, kind
		result.Removed, err = workspace.AbandonInit(c.Context(), r, cwd, id)
	default:
		err = fmt.Errorf("transaction %s has unknown kind %q", id, kind)
	}
	quarantined := []map[string]string{}
	for _, entry := range result.Quarantined {
		quarantined = append(quarantined, map[string]string{"file": entry.File, "source": entry.Source, "root": entry.Root, "kind": entry.Kind, "reason": entry.Reason})
	}
	removed := result.Removed
	if removed == nil {
		removed = []string{}
	}
	if setJSON(c, map[string]any{"transactionId": id, "kind": kind, "abandoned": err == nil, "quarantine": result.Quarantine, "quarantined": quarantined, "removed": removed, "excludeRewritten": result.ExcludeRewritten}, result.Warnings...) {
		return err
	}
	for _, warning := range result.Warnings {
		fmt.Fprintln(c.ErrOrStderr(), "Warning:", warning)
	}
	if err != nil {
		return err
	}
	out := c.OutOrStdout()
	for _, name := range result.Removed {
		message := "Removed staging file %s; local history holds its content.\n"
		if kind == "init" {
			message = "Removed unpublished initialization metadata %s.\n"
		} else if kind == "recovery-ref" {
			message = "Removed recovery ref %s; its restore stopped before writing a journal and changed no files.\n"
		}
		if _, err := fmt.Fprintf(out, message, name); err != nil {
			return err
		}
	}
	for _, entry := range result.Quarantined {
		if _, err := fmt.Fprintf(out, "Quarantined %s as %s (%s).\n", entry.Source, entry.File, entry.Reason); err != nil {
			return err
		}
	}
	if result.Quarantine != "" {
		if _, err := fmt.Fprintf(out, "Quarantine: %s (kept until you delete it).\n", result.Quarantine); err != nil {
			return err
		}
	}
	if result.ExcludeRewritten {
		_, err = fmt.Fprintf(out, "Abandoned %s transaction %s. Files were left as they are, but the managed block in info/exclude was rewritten for the current selections; run git local doctor to check the result.\n", kind, id)
		return err
	}
	_, err = fmt.Fprintf(out, "Abandoned %s transaction %s. Files were left as they are; run git local doctor to check the result.\n", kind, id)
	return err
}
