package configtx

import (
	"context"

	"gitlocal/internal/gitexec"
)

type PruneResult struct {
	Candidates []string
	Retired    []string
	Warnings   []string
}

// Prune retires only active workspace records with no Git registration and no
// available checkout. Retirement preserves their history. Each candidate is
// revalidated by Retire under the mutation lock before its rules change.
func Prune(ctx context.Context, r *gitexec.Runner, cwd string, dryRun bool) (PruneResult, error) {
	result := PruneResult{Candidates: []string{}, Retired: []string{}, Warnings: []string{}}
	s, err := openMode(ctx, r, cwd, true)
	if err != nil {
		return result, err
	}
	pending, err := s.pending()
	if err == nil && len(pending) > 0 {
		err = pendingError(pending[0])
	}
	if err == nil {
		var entries []WorkspaceInfo
		entries, err = s.inventory(ctx)
		if err == nil {
			for _, entry := range entries {
				if entry.State == "missing" && !entry.Registered && !entry.Available {
					result.Candidates = append(result.Candidates, entry.ID)
				}
			}
		}
	}
	s.close()
	if err != nil || dryRun {
		return result, err
	}
	for _, id := range result.Candidates {
		retired, e := Retire(ctx, r, cwd, id)
		result.Warnings = append(result.Warnings, retired.Warnings...)
		if e != nil {
			return result, e
		}
		if retired.Changed {
			result.Retired = append(result.Retired, id)
		}
	}
	return result, nil
}
