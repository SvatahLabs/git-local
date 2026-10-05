// Package doctor checks initialized local history without changing project state.
package doctor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/exclude"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/guard"
	"gitlocal/internal/history"
	"gitlocal/internal/hook"
	"gitlocal/internal/overlay"
	"gitlocal/internal/restore"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
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
	// Pending journals are listed without a session, which an initialization
	// or encryption journal refuses, so every kind gets its next command.
	pending, pendingErr := configtx.PendingJournals(ctx, r, cwd)
	pendingReported := false
	reportPending := func() {
		pendingReported = true
		if pendingErr != nil {
			result.add("transactions", "error", pendingErr.Error(), "Inspect the transactions directory.")
			return
		}
		if len(pending) == 0 {
			result.add("transactions", "ok", "no unfinished transactions", "")
			return
		}
		for _, p := range pending {
			action, reason := p.Next()
			result.add("transaction "+p.ID, "error", "unfinished "+p.Kind+" transaction: "+reason, action)
		}
	}
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
		reportPending()
		if d.PendingErr != nil && pendingErr == nil && len(pending) == 0 {
			result.add("transactions", "error", d.PendingErr.Error(), "Inspect the transactions directory.")
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
		if d.IgnoreOutdated {
			result.add("selection and ignores", "warning", "managed ignore block was written by an earlier release: it may not ignore restore and seed staging files ("+exclude.StagePattern+"), or may not match core.ignorecase", "Run any git local track or untrack; the next selection change rewrites the block.")
		}
		if stages, e := leftoverStages(ctx, r, d.Workspace); e != nil {
			result.add("staging files", "error", e.Error(), "Inspect the checkout for .gitlocal-tmp-* files.")
		} else {
			for _, stage := range stages {
				result.add("staging files", "warning", fmt.Sprintf("leftover restore or seed staging file %q may hold private content", stage), "Check for an unfinished transaction first (git local recover). Otherwise inspect the file and delete it; never commit it.")
			}
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
		if paths, e := sensitiveKindMismatches(d.Workspace); e != nil {
			result.add("encryption", "error", e.Error(), "Inspect the sensitive-file policy.")
		} else {
			for _, problem := range paths {
				result.add("encryption", "error", problem+"; files it should cover were saved unencrypted, and snapshots that include them are refused until the policy is corrected", "Keep the private store and its backups private. Changing an existing sensitive-file policy isn't supported yet; until then, git local untrack the affected path to keep taking snapshots of other files.")
			}
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
		if !pendingReported && (pendingErr != nil || len(pending) > 0) {
			reportPending()
		}
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
					if leak.Staging {
						result.add("upstream history", "warning", fmt.Sprintf("restore or seed staging file %q occurs in commit %s", leak.Path, leak.Commit), "Rewrite the branch to remove the file before pushing.")
					} else if leak.OverlayName != "" {
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

// sensitiveKindMismatches describes rules that don't protect what the checkout
// holds: a file rule naming a directory, or a rule spelled differently from the
// on-disk path. Earlier releases recorded both, which left the files stored as
// plaintext.
func sensitiveKindMismatches(w workspace.Context) ([]string, error) {
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return nil, err
	}
	defer common.Close()
	var policy secureblob.Policy
	if err := common.ReadJSON(secureblob.PolicyFile, &policy); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	files, err := storage.OpenDir(w.Root)
	if err != nil {
		return nil, err
	}
	defer files.Close()
	var problems []string
	for _, rule := range policy.Sensitive {
		if storage.ValidateSelectionPath(rule.Path) != nil {
			continue
		}
		if spelled := onDiskSpelling(files, rule.Path); spelled != "" {
			problems = append(problems, fmt.Sprintf("sensitive path %q is spelled %q on disk", rule.Path, spelled))
			continue
		}
		if rule.Kind != "file" {
			continue
		}
		if info, err := files.Lstat(filepath.FromSlash(rule.Path)); err == nil && info.IsDir() {
			problems = append(problems, fmt.Sprintf("sensitive path %q is recorded as a file but is a directory", rule.Path))
		}
	}
	return problems, nil
}

// onDiskSpelling returns the checkout's spelling of path when some component
// exists only under a different alias spelling (case or normalization). It
// returns "" when the path matches exactly or doesn't exist.
func onDiskSpelling(files *storage.Dir, path string) string {
	parts := strings.Split(path, "/")
	spelled := make([]string, 0, len(parts))
	differs := false
	for i, part := range parts {
		entries, err := files.ReadDir(filepath.FromSlash(strings.Join(parts[:i], "/")))
		if err != nil {
			return ""
		}
		match := ""
		for _, entry := range entries {
			if entry.Name() == part {
				match = part
				break
			}
			if storage.AliasKey(entry.Name()) == storage.AliasKey(part) {
				match = entry.Name()
			}
		}
		if match == "" {
			return ""
		}
		differs = differs || match != part
		spelled = append(spelled, match)
		if match != part {
			// Keep reading below the on-disk spelling.
			parts[i] = match
		}
	}
	if !differs {
		return ""
	}
	return strings.Join(spelled, "/")
}

// leftoverStages lists untracked restore and seed staging files in this
// checkout, including ignored ones, which an interrupted operation can leave.
func leftoverStages(ctx context.Context, r *gitexec.Runner, w workspace.Context) ([]string, error) {
	// An ignore pattern rather than a pathspec: the runner forces literal
	// pathspecs, and only this pattern is applied, so ignored stages are listed.
	b, err := r.Main(ctx, w.Root, w.Admin, "ls-files", "-z", "--others", "--ignored", "--full-name", "--exclude="+exclude.StagePattern)
	if err != nil {
		return nil, err
	}
	var stages []string
	for _, name := range strings.Split(string(b), "\x00") {
		if name != "" {
			stages = append(stages, name)
		}
	}
	// A stage that was force-added is in the index, which is the riskier state.
	b, err = r.Main(ctx, w.Root, w.Admin, "ls-files", "-z", "--cached", "--full-name")
	if err != nil {
		return nil, err
	}
	for _, name := range strings.Split(string(b), "\x00") {
		if configtx.StagingPath(name) {
			stages = append(stages, name+" (staged in the index)")
		}
	}
	return stages, nil
}
