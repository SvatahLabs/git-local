package history

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/selection"
	"gitlocal/internal/workspace"
)

type DiffResult struct {
	Status StatusResult
	Patch  string
}

func treeFromFiles(ctx context.Context, g historyGit, store string, files map[string]fileValue, conflicts []string) (string, error) {
	root := &directory{}
	for path, v := range files {
		if !conflicted(path, conflicts) {
			root.add(path, v.Mode, v.ID)
		}
	}
	return root.write(ctx, g, store)
}

// terminalText preserves diff layout while making terminal controls inert.
func terminalText(text string) string {
	var out strings.Builder
	for _, c := range text {
		if c != '\n' && c != '\t' && (unicode.IsControl(c) || unicode.In(c, unicode.Cf)) {
			fmt.Fprintf(&out, "\\u%04x", c)
		} else {
			out.WriteRune(c)
		}
	}
	return out.String()
}
func Diff(ctx context.Context, r *gitexec.Runner, cwd string) (DiffResult, error) {
	return diff(ctx, r, cwd, nil)
}
func diff(ctx context.Context, r *gitexec.Runner, cwd string, f func(string, string) error) (result DiffResult, err error) {
	err = configtx.ReadSelection(ctx, r, cwd, func(w workspace.Context, s selection.State, warnings []string, recheck func() error) (err error) {
		temporary, err := r.TemporaryHistory(ctx, w.Store)
		if err != nil {
			return err
		}
		defer func() {
			if cleanupErr := temporary.Close(); cleanupErr != nil {
				if err == nil {
					err = fmt.Errorf("remove temporary diff content: %w", cleanupErr)
				} else {
					err = fmt.Errorf("%w; temporary diff cleanup failed: %v", err, cleanupErr)
				}
			}
		}()
		inspection, err := inspect(ctx, r, w, s, warnings, recheck, temporary, temporary.Path, true)
		if err != nil {
			return err
		}
		result.Status = inspection.status
		if err = diffPoint(ctx, f, "diff-captured", temporary.Path); err != nil {
			return err
		}
		before, err := treeFromFiles(ctx, temporary, temporary.Path, inspection.before, inspection.conflicts)
		if err != nil {
			return err
		}
		after, err := treeFromFiles(ctx, temporary, temporary.Path, inspection.after, inspection.conflicts)
		if err != nil {
			return err
		}
		out, err := temporary.History(ctx, temporary.Path, nil, "diff-tree", "--no-commit-id", "-r", "-p", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--full-index", "--src-prefix=a/", "--dst-prefix=b/", before, after, "--")
		if err != nil {
			return err
		}
		if err = diffPoint(ctx, f, "diff-rendered", temporary.Path); err != nil {
			return err
		}
		if !utf8.Valid(out) {
			var summary strings.Builder
			summary.WriteString("Non-UTF-8 content detected; patch omitted.\n")
			for _, file := range result.Status.Files {
				if file.Code != "C" {
					fmt.Fprintf(&summary, "%s %q\n", file.Code, file.Path)
				}
			}
			result.Patch = summary.String()
		} else {
			result.Patch = terminalText(string(out))
		}
		return nil
	})
	return result, err
}
func (d DiffResult) Human() string {
	var out strings.Builder
	for _, s := range d.Status.Selection {
		fmt.Fprintf(&out, "Selection %s %s %q\n", s.Code, s.Kind, s.Path)
	}
	for _, f := range d.Status.Files {
		if f.Code == "C" {
			fmt.Fprintf(&out, "Conflict %q (omitted from patch)\n", f.Path)
		}
	}
	out.WriteString(d.Patch)
	if out.Len() == 0 {
		out.WriteString("No local changes.\n")
	}
	return out.String()
}

func diffPoint(ctx context.Context, f func(string, string) error, stage, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f != nil {
		return f(stage, path)
	}
	return nil
}
