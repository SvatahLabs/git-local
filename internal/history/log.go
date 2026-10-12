package history

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type LogEntry struct {
	ID, Timestamp, Message, Stat string
	Writer                       string
	ChangedPaths                 []string
	ShowProvenance               bool
	StatData                     *StatSummary
}
type LogFilter struct {
	Branch         string
	MainCommit     string
	WithStat       bool
	Writer         string
	Path           string
	WithProvenance bool
}

func Log(ctx context.Context, r *gitexec.Runner, cwd string, limit int) ([]LogEntry, error) {
	return LogWorkspace(ctx, r, cwd, "", limit)
}
func LogWorkspace(ctx context.Context, r *gitexec.Runner, cwd, source string, limit int) ([]LogEntry, error) {
	return LogFiltered(ctx, r, cwd, source, limit, LogFilter{})
}

// ErrInvalidLogFilter marks a malformed --branch or --main-commit value.
var ErrInvalidLogFilter = errors.New("invalid log filter")

// LogFiltered matches the branch and mainline commit recorded when each local
// snapshot was created. Branch names are historical, not current ref targets.
func LogFiltered(ctx context.Context, r *gitexec.Runner, cwd, source string, limit int, filter LogFilter) ([]LogEntry, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("log limit must be between 1 and 1000")
	}
	filter.MainCommit = strings.ToLower(filter.MainCommit)
	if filter.MainCommit != "" && !validOID(filter.MainCommit) {
		return nil, fmt.Errorf("%w: mainline commit must be a full 40- or 64-character hexadecimal object ID", ErrInvalidLogFilter)
	}
	if err := ValidateWriter(filter.Writer); err != nil {
		return nil, err
	}
	if filter.Path != "" {
		if err := storage.ValidateSelectionPath(filter.Path); err != nil {
			return nil, err
		}
	}
	if filter.Branch != "" && !strings.HasPrefix(filter.Branch, "refs/heads/") {
		filter.Branch = "refs/heads/" + filter.Branch
	}
	result := []LogEntry{}
	outputBytes := 0
	err := configtx.ReadSelection(ctx, r, cwd, func(w workspace.Context, _ selection.State, _ []string, recheck func() error) error {
		if filter.Branch != "" {
			if _, err := r.Main(ctx, w.Root, w.Admin, "check-ref-format", filter.Branch); err != nil {
				var ge *gitexec.Error
				if errors.As(err, &ge) && ge.Code == 1 {
					return fmt.Errorf("%w: invalid branch name %q", ErrInvalidLogFilter, strings.TrimPrefix(filter.Branch, "refs/heads/"))
				}
				return fmt.Errorf("invalid branch name: %w", err)
			}
		}
		id, err := sourceWorkspace(w, source)
		if err != nil {
			return err
		}
		w.WorkspaceID = id
		first, err := head(ctx, r, w.Store, Ref(w.WorkspaceID))
		if err != nil {
			return err
		}
		id = first
		seen := map[string]bool{}
		for id != "" && len(result) < limit {
			if seen[id] {
				return errors.New("cycle in snapshot history")
			}
			seen[id] = true
			_, metadata, e := readPrevious(ctx, r, w.Store, id, w.RepositoryID, w.WorkspaceID)
			if e != nil {
				return e
			}
			data, err := r.History(ctx, w.Store, nil, "cat-file", "commit", id)
			if err != nil {
				return err
			}
			header, message, ok := strings.Cut(string(data), "\n\n")
			if !ok || len(message) > 64<<10+1 {
				return errors.New("invalid snapshot commit message")
			}
			parent, stamp := "", ""
			for _, line := range strings.Split(header, "\n") {
				if strings.HasPrefix(line, "parent ") {
					p := strings.TrimPrefix(line, "parent ")
					if parent != "" || !validOID(p) {
						return errors.New("nonlinear or invalid snapshot history")
					}
					parent = p
				}
				if strings.HasPrefix(line, "committer ") {
					fields := strings.Fields(line)
					if len(fields) < 3 {
						return errors.New("invalid snapshot timestamp")
					}
					seconds, e := strconv.ParseInt(fields[len(fields)-2], 10, 64)
					if e != nil {
						return errors.New("invalid snapshot timestamp")
					}
					stamp = time.Unix(seconds, 0).UTC().Format(time.RFC3339)
				}
			}
			if stamp == "" {
				return errors.New("missing snapshot timestamp")
			}
			matchesPath := filter.Path == ""
			for _, changed := range metadata.ChangedPaths {
				if changed == filter.Path {
					matchesPath = true
					break
				}
			}
			if (filter.Branch == "" || metadata.MainBranch == filter.Branch) && (filter.MainCommit == "" || metadata.MainHead == filter.MainCommit) && (filter.Writer == "" || metadata.Writer == filter.Writer) && matchesPath {
				entry := LogEntry{ID: id, Timestamp: stamp, Message: strings.TrimSuffix(message, "\n"), Writer: metadata.Writer, ChangedPaths: metadata.ChangedPaths, ShowProvenance: filter.WithProvenance}
				if filter.WithStat {
					var stat StatSummary
					if metadata.SchemaVersion == 2 {
						entry.Stat, stat, err = snapshotStatPlain(ctx, r, w, id, parent)
					} else {
						entry.Stat, stat, err = snapshotStat(ctx, r, w.Store, id)
					}
					entry.StatData = &stat
					if err != nil {
						return err
					}
				}
				outputBytes += len(entry.ID) + len(entry.Timestamp) + len(strconv.Quote(entry.Message)) + len(entry.Stat) + 3
				if filter.WithProvenance {
					outputBytes += len(entry.Writer) + 25
					for _, path := range entry.ChangedPaths {
						outputBytes += len(strconv.Quote(path)) + 12
					}
				}
				if outputBytes > maxBrowseOutput {
					return errors.New("log output exceeds 16 MiB safety limit")
				}
				result = append(result, entry)
			}
			id = parent
		}
		if err = recheck(); err != nil {
			return err
		}
		current, err := head(ctx, r, w.Store, Ref(w.WorkspaceID))
		if err != nil {
			return err
		}
		if current != first {
			return fmt.Errorf("%w: snapshot history changed during inspection", storage.ErrConflict)
		}
		return nil
	})
	return result, err
}
func FormatLog(entries []LogEntry) string {
	var out strings.Builder
	for _, entry := range entries {
		fmt.Fprintf(&out, "%s %s %q\n", entry.ID, entry.Timestamp, entry.Message)
		if entry.ShowProvenance {
			writer := entry.Writer
			if writer == "" {
				writer = "unattributed"
			}
			fmt.Fprintf(&out, "  writer: %s\n", writer)
			for _, path := range entry.ChangedPaths {
				fmt.Fprintf(&out, "  changed: %q\n", path)
			}
		}
		out.WriteString(entry.Stat)
	}
	if len(entries) == 0 {
		out.WriteString("No local snapshots.\n")
	}
	return out.String()
}
