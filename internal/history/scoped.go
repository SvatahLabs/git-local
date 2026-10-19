package history

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/SvatahLabs/git-local/internal/selection"
	"github.com/SvatahLabs/git-local/internal/storage"
)

// Writer IDs are caller claims, not filesystem author attestations. A session
// runner should pass a stable ID shared by its scoped snapshots.
func ValidateWriter(writer string) error {
	if len(writer) > 128 {
		return errors.New("writer ID must be at most 128 characters")
	}
	for _, c := range writer {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._:@/-", c) {
			continue
		}
		return errors.New("writer ID may contain only letters, digits, . _ : @ / and -")
	}
	return nil
}

func normalizeScope(engine *selection.Engine, cwd string, state selection.State, args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, errors.New("scoped commit requires at least one path")
	}
	unique := map[string]bool{}
	for _, arg := range args {
		path, err := engine.Normalize(cwd, arg)
		if err != nil {
			return nil, err
		}
		if !state.Selected(path) {
			return nil, fmt.Errorf("path %q is outside the current local selection", path)
		}
		unique[path] = true
	}
	paths := make([]string, 0, len(unique))
	for path := range unique {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	result := []string{}
	for _, path := range paths {
		if len(result) == 0 || !under(result[len(result)-1], path) {
			result = append(result, path)
		}
	}
	return result, nil
}

func inScope(scope []string, path string) bool {
	if scope == nil {
		return true
	}
	for _, root := range scope {
		if under(root, path) {
			return true
		}
	}
	return false
}

func scopeEntries(entries []selection.Entry, scope []string) []selection.Entry {
	if scope == nil {
		return entries
	}
	result := []selection.Entry{}
	for _, entry := range entries {
		if inScope(scope, entry.Path) {
			result = append(result, entry)
		}
	}
	return result
}

func scopeMissing(rules []selection.Rule, scope []string) []selection.Rule {
	if scope == nil {
		return rules
	}
	result := []selection.Rule{}
	for _, rule := range rules {
		if inScope(scope, rule.Path) {
			result = append(result, rule)
		}
	}
	return result
}

func changedPaths(before, after map[string]fileValue) []string {
	set := map[string]bool{}
	for path, old := range before {
		if next, ok := after[path]; !ok || next != old {
			set[path] = true
		}
	}
	for path, next := range after {
		if old, ok := before[path]; !ok || old != next {
			set[path] = true
		}
	}
	result := make([]string, 0, len(set))
	for path := range set {
		result = append(result, path)
	}
	sort.Strings(result)
	return result
}

func validateMergedFiles(files map[string]fileValue, state selection.State) error {
	aliases := map[string]string{}
	// A directory shared by many files is checked once.
	checked := map[string]bool{}
	for path := range files {
		if err := storage.ValidateSelectionPath(path); err != nil {
			return err
		}
		if !state.Selected(path) {
			return fmt.Errorf("%w: carried-forward path %q is no longer selected; include its parent in the scope or save a full snapshot", storage.ErrConflict, path)
		}
		parts := strings.Split(path, "/")
		for i := range parts {
			component := strings.Join(parts[:i+1], "/")
			if checked[component] {
				continue
			}
			checked[component] = true
			key := storage.AliasKey(component)
			if other, ok := aliases[key]; ok && other != component {
				return fmt.Errorf("%w: ambiguous scoped snapshot paths %q and %q", storage.ErrConflict, other, component)
			}
			aliases[key] = component
		}
	}
	for path := range files {
		parts := strings.Split(path, "/")
		for i := 1; i < len(parts); i++ {
			if _, ok := files[strings.Join(parts[:i], "/")]; ok {
				return fmt.Errorf("%w: scoped snapshot has a file/directory conflict at %q; include the parent path", storage.ErrConflict, path)
			}
		}
	}
	return nil
}
