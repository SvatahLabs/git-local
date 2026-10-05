// Package doctor checks initialized local history without changing project state.
package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/guard"
	"gitlocal/internal/history"
	"gitlocal/internal/hook"
	"gitlocal/internal/overlay"
	"gitlocal/internal/restore"
	"gitlocal/internal/selection"
)

type Finding struct {
	Check       string `json:"check"`
	Code        string `json:"code,omitempty"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	Severity    string `json:"severity"`
	Message     string `json:"message"`
	Action      string `json:"action,omitempty"`
}
type Report struct {
	SchemaVersion int       `json:"schemaVersion"`
	RepositoryID  string    `json:"repositoryId,omitempty"`
	WorkspaceID   string    `json:"workspaceId,omitempty"`
	Findings      []Finding `json:"findings"`
}

func (r Report) HasIssues() bool {
	for _, f := range r.Findings {
		if f.Severity == "error" {
			return true
		}
	}
	return false
}
func (r Report) Human() string {
	var out strings.Builder
	for _, f := range r.Findings {
		fmt.Fprintf(&out, "%s %s: %s\n", strings.ToUpper(f.Severity), f.Check, f.Message)
		if f.Action != "" {
			fmt.Fprintf(&out, "  Action: %s\n", f.Action)
		}
	}
	if !r.HasIssues() {
		out.WriteString("Doctor found no blocking issues.\n")
	}
	return out.String()
}
func (r Report) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }
func (r *Report) add(check, severity, message, action string) {
	r.Findings = append(r.Findings, Finding{Check: check, Severity: severity, Message: message, Action: action})
}
func (r *Report) addWorkspace(id, code, message, action string) {
	r.Findings = append(r.Findings, Finding{Check: "workspace " + id, Code: code, WorkspaceID: id, Severity: "warning", Message: message, Action: action})
}

// Run returns a report even when an initialized store is malformed, so the CLI
// can give an actionable diagnosis. Cancellation remains an operation error.
func Run(ctx context.Context, r *gitexec.Runner, cwd string) (Report, error) {
	result := Report{SchemaVersion: 1, Findings: []Finding{}}
	if err := r.CheckCapabilities(ctx); err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.add("git", "error", err.Error(), "Install Git 2.50.1 or later and rerun git local doctor.")
		return result, nil
	}
	result.add("git", "ok", "required local Git behavior is available", "")
	err := configtx.ReadDiagnostic(ctx, r, cwd, func(d configtx.DiagnosticState) error {
		result.RepositoryID = d.Workspace.RepositoryID
		result.WorkspaceID = d.Workspace.WorkspaceID
		result.add("configuration", "ok", "repository identity, workspace schema and isolated store configuration are valid", "")
		if d.InventoryErr != nil {
			result.add("workspaces", "error", d.InventoryErr.Error(), "Inspect worktree registrations and identity files; use git local init or workspace retire as appropriate.")
		} else {
			for _, w := range d.Workspaces {
				switch w.State {
				case "missing":
					result.addWorkspace(w.ID, "workspace-missing", "active workspace has no Git registration", "After removing its worktree, run git local workspace retire "+w.ID+" from an active checkout.")
				case "moved":
					result.addWorkspace(w.ID, "workspace-moved", "workspace root moved", "Run git local init from the moved checkout to reconcile its registration.")
				case "unavailable":
					result.addWorkspace(w.ID, "workspace-unavailable", "registered workspace is unavailable", "Restore access to that checkout before changing its selection.")
				}
			}
			result.add("workspaces", "ok", fmt.Sprintf("%d workspace records inspected", len(d.Workspaces)), "")
		}
		if d.PendingErr != nil {
			result.add("transactions", "error", d.PendingErr.Error(), "Inspect the transactions directory; run git local init for an unfinished initialization.")
		} else if len(d.Pending) > 0 {
			for _, id := range d.Pending {
				action := "Run git local recover " + id + " --resume or --rollback."
				for _, journal := range d.Journals {
					if journal.ID == id && (journal.Kind == "restore" || journal.Kind == "seed") {
						action = "From the original destination workspace, run git local recover " + id + " --resume or --rollback."
						break
					}
				}
				result.add("transaction "+id, "error", "unfinished transaction requires explicit recovery", action)
			}
		} else {
			result.add("transactions", "ok", "no unfinished transactions", "")
		}
		if d.JournalErr != nil {
			result.add("journals", "error", d.JournalErr.Error(), "Inspect unexpected transaction metadata; do not delete recovery records blindly.")
		}
		journalsHealthy := d.JournalErr == nil
		for _, j := range d.Journals {
			problem := j.Err
			if problem == nil && j.Kind == "restore" {
				problem = restore.InspectJournal(ctx, r, d.Workspace, d.Selection, d.ConfigDigest, j.ID, j.Completed, d.Sources)
			}
			if problem != nil {
				journalsHealthy = false
				result.add("journal "+j.ID, "error", problem.Error(), "Inspect this journal and its recovery ref before attempting recovery.")
			}
		}
		if journalsHealthy {
			result.add("journals", "ok", fmt.Sprintf("%d transaction journals inspected", len(d.Journals)), "")
		}
		if d.ConfigErr != nil {
			result.add("exclusions", "error", d.ConfigErr.Error(), "Reconcile the managed info/exclude block and workspace selections manually.")
		} else {
			result.add("exclusions", "ok", "managed exclusions and workspace selection inputs are consistent", "")
		}
		if d.IgnoreErr != nil {
			result.add("selection and ignores", "error", d.IgnoreErr.Error(), "Resolve selected path conflicts or overriding Git ignore rules before the next mutation.")
		} else {
			result.add("selection and ignores", "ok", "available active selections are safe and effectively ignored", "")
		}
		for _, warning := range d.IgnoreWarnings {
			result.add("selection and ignores", "warning", warning, "")
		}
		owner, ownerErr := selection.InspectOwnership(ctx, r, d.Workspace.Root, d.Workspace.Admin)
		if ownerErr == nil {
			ownerErr = owner.Check(d.Selection)
		}
		if ownerErr != nil {
			result.add("main ownership", "error", ownerErr.Error(), "Untrack or exclude main-owned selected paths.")
		} else {
			result.add("main ownership", "ok", "main HEAD and index do not own the current selection", "")
		}
		storeErr := gitexec.ValidateStore(d.Workspace.Store)
		if storeErr != nil {
			result.add("history store", "error", storeErr.Error(), "Restore a supported private store configuration from a trusted backup.")
		}
		if storeErr == nil {
			if _, err := r.History(ctx, d.Workspace.Store, nil, "fsck", "--full", "--strict", "--no-reflogs", "--no-progress"); err != nil {
				result.add("objects", "error", err.Error(), "Inspect the private store and a trusted backup; do not prune objects automatically.")
			} else {
				result.add("objects", "ok", "private object graph passes Git integrity checks", "")
			}
			total := 0
			historyHealthy := true
			for _, id := range d.Sources {
				n, e := history.AuditWorkspaceLocked(ctx, r, d.Workspace, id)
				total += n
				if e != nil {
					historyHealthy = false
					result.add("history "+id, "error", e.Error(), "Inspect this workspace history and its private-store backup.")
				}
			}
			if historyHealthy && len(d.Sources) > 0 {
				result.add("history", "ok", fmt.Sprintf("%d user snapshots checked across %d workspaces", total, len(d.Sources)), "")
			}
		}
		if ownerErr == nil {
			if e := owner.Revalidate(ctx, r, d.Workspace.Root, d.Workspace.Admin); e != nil {
				result.add("main ownership", "error", e.Error(), "Retry after Git operations finish.")
			}
		}
		if e := d.Recheck(); e != nil {
			result.add("configuration", "error", e.Error(), "Retry after concurrent configuration changes finish.")
		}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		result.add("configuration", "error", err.Error(), "Run git local init for an incomplete initialization, or inspect schema and identity files before retrying.")
	}
	if err == nil {
		snapshot, e := configtx.LoadGuardSnapshot(ctx, r, cwd)
		if e != nil {
			result.add("guards", "error", e.Error(), "Inspect selection metadata and retry.")
		} else {
			hasOverlays, overlayErr := overlay.HasAny(ctx, snapshot.Common)
			if overlayErr != nil {
				result.add("overlays", "error", overlayErr.Error(), "Inspect overlay registrations before retrying.")
			} else if hasOverlays {
				if e := overlay.AuditInstallation(ctx, r, cwd, snapshot.Common); e != nil {
					result.add("overlays", "error", e.Error(), "Restore the required overlay filter, managed attributes and executable before staging or checking out files.")
				} else {
					result.add("overlays", "ok", "active overlay filter installation is intact", "")
				}
			}
			if len(snapshot.Selections) == 0 && !hasOverlays {
				return result, nil
			}
			status, e := hook.Inspect(ctx, r, cwd)
			if e != nil {
				result.add("guards", "error", e.Error(), "Inspect the configured hooks directory.")
			} else {
				for _, name := range []string{"pre-commit", "pre-push"} {
					if !status.Installed[name] {
						result.add("guards", "warning", name+" guard is not installed in the current hooks directory", "Run git local hook install.")
					}
				}
				if len(snapshot.Selections) > 0 {
					missing := []string{}
					for _, name := range []string{"post-commit", "post-checkout", "pre-rebase", "post-rewrite"} {
						if !status.Installed[name] {
							missing = append(missing, name)
						}
					}
					if len(missing) > 0 {
						result.add("automatic snapshots", "warning", "Git event hooks are missing: "+strings.Join(missing, ", "), "Run git local auto install; session-end hooks can also call git local commit --if-changed.")
					}
				}
				if len(status.Previous) > 0 {
					result.add("guards", "warning", "core.hooksPath changed; hooks remain at "+strings.Join(status.Previous, ", "), "Reinstall git-local hooks in the current hooks directory.")
				}
			}
			leaks, e := guard.AuditUpstream(ctx, r, cwd)
			if e != nil {
				result.add("upstream history", "error", e.Error(), "Inspect local branches with upstreams.")
			} else {
				for _, leak := range leaks {
					if leak.OverlayName != "" {
						result.add("upstream history", "warning", fmt.Sprintf("overlay %q on %q occurs in commit %s (workspace %s)", leak.OverlayName, leak.Path, leak.Commit, leak.WorkspaceID), "Rewrite the branch to remove the overlay before pushing.")
					} else {
						result.add("upstream history", "warning", fmt.Sprintf("selected path %q occurs in commit %s (workspace %s)", leak.Path, leak.Commit, leak.WorkspaceID), "Rewrite the branch before pushing, or remove the selection if this path is intentionally mainline.")
					}
				}
			}
		}
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	return result, nil
}
