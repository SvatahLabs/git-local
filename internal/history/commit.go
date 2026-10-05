package history

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"unicode/utf8"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/secureblob"
	"gitlocal/internal/selection"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

type Result struct {
	ID           string
	Changed      bool
	Warnings     []string
	Subprocesses uint64
}
type fault func(string) error

func checkpoint(ctx context.Context, f fault, at string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if f != nil {
		return f(at)
	}
	return nil
}
func sameRules(a, b selection.State) bool {
	return reflect.DeepEqual(a.Include, b.Include) && reflect.DeepEqual(a.Exclude, b.Exclude)
}
func ValidateMessage(message string) error {
	if strings.TrimSpace(message) == "" || !utf8.ValidString(message) || strings.ContainsRune(message, 0) || len(message) > 64<<10 {
		return errors.New("commit message must be nonempty UTF-8 without NUL, at most 64 KiB")
	}
	return nil
}
func Commit(ctx context.Context, r *gitexec.Runner, cwd, message string) (Result, error) {
	return commitWithOptions(ctx, r, cwd, message, "", nil, nil)
}
func CommitWithWriter(ctx context.Context, r *gitexec.Runner, cwd, message, writer string) (Result, error) {
	return commitWithOptions(ctx, r, cwd, message, writer, nil, nil)
}
func commit(ctx context.Context, r *gitexec.Runner, cwd, message string, f fault) (result Result, err error) {
	return commitWithOptions(ctx, r, cwd, message, "", nil, f)
}

// CommitScoped captures only requested selected paths. Files outside the scope
// retain their previous snapshot bytes, even when other writers have edited
// them in the same checkout. Writer is an optional caller-supplied session ID.
func CommitScoped(ctx context.Context, r *gitexec.Runner, cwd, message, writer string, paths []string) (Result, error) {
	if len(paths) == 0 {
		return Result{}, errors.New("scoped commit requires at least one path")
	}
	return commitWithOptions(ctx, r, cwd, message, writer, paths, nil)
}

func commitWithOptions(ctx context.Context, r *gitexec.Runner, cwd, message, writer string, requested []string, f fault) (result Result, err error) {
	start := r.SubprocessCount()
	defer func() { result.Subprocesses = r.SubprocessCount() - start }()
	if err = ValidateMessage(message); err != nil {
		return result, err
	}
	if err = ValidateWriter(writer); err != nil {
		return result, err
	}
	result.Warnings, err = configtx.WithSelection(ctx, r, cwd, func(w workspace.Context, state selection.State, recheck func() error) error {
		secure, e := loadSecure(w)
		if e != nil {
			return e
		}
		previous, e := head(ctx, r, w.Store, Ref(w.WorkspaceID))
		if e != nil {
			return e
		}
		previousTree := ""
		var old Metadata
		if previous != "" {
			previousTree, old, e = readPrevious(ctx, r, w.Store, previous, w.RepositoryID, w.WorkspaceID)
			if e != nil {
				return e
			}
		}
		oldFiles, e := readFiles(ctx, r, w.Store, previousTree)
		if e != nil {
			return e
		}
		if secure == nil && old.SchemaVersion == 2 {
			return fmt.Errorf("%w: encrypted snapshot requires a sensitive-file policy", storage.ErrConflict)
		}
		if secure != nil {
			if old.SchemaVersion == 2 && old.EncryptedKeyFingerprint != secure.policy.KeyFingerprint {
				return fmt.Errorf("%w: sensitive-file key differs from prior snapshot", storage.ErrConflict)
			}
			for path := range oldFiles {
				if secure.covers(path) && old.EncryptedDigests[path] == "" {
					return fmt.Errorf("%w: plaintext history for %q requires encryption migration", storage.ErrConflict, path)
				}
			}
			for path := range old.EncryptedDigests {
				if _, ok := oldFiles[path]; !ok {
					return fmt.Errorf("%w: encrypted snapshot metadata references missing file %q", storage.ErrConflict, path)
				}
				if !secure.covers(path) {
					return fmt.Errorf("%w: encrypted history for %q is outside the sensitive-file policy", storage.ErrConflict, path)
				}
			}
		}
		engine, e := selection.New(r, w.Root, w.Admin)
		if e != nil {
			return e
		}
		defer engine.Close()
		var scope []string
		if requested != nil {
			scope, e = normalizeScope(engine, cwd, state, requested)
			if e != nil {
				return e
			}
		}
		plan, e := engine.Validate(ctx, state)
		if e != nil {
			return e
		}
		if e = checkpoint(ctx, f, "validated"); e != nil {
			return e
		}
		files, e := storage.OpenDir(w.Root)
		if e != nil {
			return e
		}
		defer files.Close()
		observations := map[string]storage.Observation{}
		nextFiles := map[string]fileValue{}
		nextDigests := map[string]string{}
		if scope != nil {
			for path, file := range oldFiles {
				if !inScope(scope, path) {
					nextFiles[path] = file
					if digest := old.EncryptedDigests[path]; digest != "" {
						nextDigests[path] = digest
					}
				}
			}
		}
		entries := scopeEntries(plan.Entries, scope)
		plainRegular := 0
		for _, entry := range entries {
			if entry.Mode&os.ModeSymlink == 0 && !secure.covers(entry.Path) {
				plainRegular++
			}
		}
		var batch *blobBatch
		if plainRegular >= 16 {
			batch, e = newBlobBatch()
			if e != nil {
				return e
			}
			defer batch.close()
		}
		flush := func() error {
			if batch != nil {
				return batch.flush(ctx, r, w.Store, nextFiles)
			}
			return nil
		}
		for _, entry := range entries {
			if e = checkpoint(ctx, f, "before-file"); e != nil {
				return e
			}
			var id string
			mode := "100644"
			if entry.Mode&os.ModeSymlink != 0 {
				mode = "120000"
			} else if entry.Mode&0111 != 0 {
				mode = "100755"
			}
			if secure.covers(entry.Path) {
				if e = flush(); e != nil {
					return e
				}
				identity := secure.identity(w, entry.Path, mode)
				var digest string
				if mode == "120000" {
					target, err := files.ReadLink(entry.Path)
					if err != nil {
						return err
					}
					if target != entry.LinkTarget {
						return fmt.Errorf("%w: link changed at %q", storage.ErrConflict, entry.Path)
					}
					digest, e = secureblob.Digest(strings.NewReader(target), secure.key, identity)
					if e != nil {
						return e
					}
					if oldFile, ok := oldFiles[entry.Path]; ok && oldFile.Mode == mode && old.EncryptedDigests[entry.Path] == digest {
						id = oldFile.ID
					} else {
						var encryptedDigest string
						id, encryptedDigest, e = writeEncryptedBlob(ctx, r, w.Store, strings.NewReader(target), secure.key, identity)
						if e != nil {
							return e
						}
						if encryptedDigest != digest {
							return fmt.Errorf("%w: symlink changed during encryption", storage.ErrConflict)
						}
					}
				} else {
					var observation storage.Observation
					observation, e = files.StreamStable(entry.Path, func(input io.Reader) error {
						var err error
						digest, err = secureblob.Digest(input, secure.key, identity)
						return err
					})
					if e != nil {
						return e
					}
					if observation.Mode() != entry.Mode {
						return fmt.Errorf("%w: mode changed at %q", storage.ErrConflict, entry.Path)
					}
					observations[entry.Path] = observation
					if oldFile, ok := oldFiles[entry.Path]; ok && oldFile.Mode == mode && old.EncryptedDigests[entry.Path] == digest {
						id = oldFile.ID
					} else {
						var encryptedDigest string
						_, e = files.StreamStable(entry.Path, func(input io.Reader) error {
							var err error
							id, encryptedDigest, err = writeEncryptedBlob(ctx, r, w.Store, input, secure.key, identity)
							return err
						})
						if e != nil {
							return e
						}
						if encryptedDigest != digest {
							return fmt.Errorf("%w: file changed during encryption at %q", storage.ErrConflict, entry.Path)
						}
					}
				}
				nextDigests[entry.Path] = digest
				nextFiles[entry.Path] = fileValue{mode, id}
				if e = checkpoint(ctx, f, "file"); e != nil {
					return e
				}
				continue
			}
			if entry.Mode&os.ModeSymlink != 0 {
				if e = flush(); e != nil {
					return e
				}
				target, err := files.ReadLink(entry.Path)
				if err != nil {
					return err
				}
				if target != entry.LinkTarget {
					return fmt.Errorf("%w: link changed at %q", storage.ErrConflict, entry.Path)
				}
				id, e = hash(ctx, r, w.Store, strings.NewReader(target))
			} else {
				if batch != nil {
					observation, err := batch.stage(files, entry.Path, mode, entry.Mode)
					if err == nil {
						observations[entry.Path] = observation
						if batch.ready() {
							if err := flush(); err != nil {
								return err
							}
						}
						if e = checkpoint(ctx, f, "file"); e != nil {
							return e
						}
						continue
					}
					if !errors.Is(err, errBlobBatchTooLarge) {
						return err
					}
					if e = flush(); e != nil {
						return e
					}
				}
				var observation storage.Observation
				observation, e = files.StreamStable(entry.Path, func(input io.Reader) error { var err error; id, err = hash(ctx, r, w.Store, input); return err })
				if e == nil {
					if observation.Mode() != entry.Mode {
						return fmt.Errorf("%w: mode changed at %q", storage.ErrConflict, entry.Path)
					}
					observations[entry.Path] = observation
				}
			}
			if e != nil {
				return e
			}
			nextFiles[entry.Path] = fileValue{mode, id}
			if e = checkpoint(ctx, f, "file"); e != nil {
				return e
			}
		}
		if e = flush(); e != nil {
			return e
		}
		if e := validateMergedFiles(nextFiles, state); e != nil {
			return e
		}
		root := &directory{}
		for path, file := range nextFiles {
			root.add(path, file.Mode, file.ID)
		}
		filesTree, e := root.write(ctx, r, w.Store)
		if e != nil {
			return e
		}
		if e = checkpoint(ctx, f, "files-tree"); e != nil {
			return e
		}
		stable := func() error {
			if err := recheck(); err != nil {
				return err
			}
			now, err := engine.Validate(ctx, state)
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(scopeEntries(plan.Entries, scope), scopeEntries(now.Entries, scope)) || !reflect.DeepEqual(scopeMissing(plan.Missing, scope), scopeMissing(now.Missing, scope)) || scope == nil && !reflect.DeepEqual(plan.Directories, now.Directories) {
				return fmt.Errorf("%w: selected paths changed during capture", storage.ErrConflict)
			}
			for path, observation := range observations {
				if err = observation.Recheck(files, path); err != nil {
					return err
				}
			}
			return plan.Ownership.Revalidate(ctx, r, w.Root, w.Admin)
		}
		if previous != "" && filesTree == previousTree && sameRules(state, old.Selection) && (secure == nil || old.SchemaVersion == 2) {
			if e = stable(); e != nil {
				return e
			}
			current, err := head(ctx, r, w.Store, Ref(w.WorkspaceID))
			if err != nil {
				return err
			}
			if current != previous {
				return fmt.Errorf("%w: history changed during capture", storage.ErrConflict)
			}
			result.ID = previous
			return nil
		}
		metadata := Metadata{SchemaVersion: 1, Kind: "user", RepositoryID: w.RepositoryID, WorkspaceID: w.WorkspaceID, Selection: state, MainHead: plan.Ownership.Evidence.Head, Writer: writer, Scope: scope, ChangedPaths: changedPaths(oldFiles, nextFiles)}
		if secure != nil {
			metadata.SchemaVersion = 2
			metadata.EncryptedKeyFingerprint = secure.policy.KeyFingerprint
			metadata.EncryptedDigests = nextDigests
		}
		branch, branchErr := r.Main(ctx, w.Root, w.Admin, "symbolic-ref", "--quiet", "HEAD")
		if branchErr == nil {
			metadata.MainBranch = strings.TrimSuffix(string(branch), "\n")
		} else {
			var ge *gitexec.Error
			if !errors.As(branchErr, &ge) || ge.Code != 1 {
				return branchErr
			}
		}
		data, e := json.Marshal(metadata)
		if e != nil {
			return e
		}
		data = append(data, '\n')
		if len(data) > 1<<20 {
			return errors.New("snapshot metadata exceeds size limit")
		}
		metadataID, e := hash(ctx, r, w.Store, bytes.NewReader(data))
		if e != nil {
			return e
		}
		wrapper, e := tree(ctx, r, w.Store, []treeEntry{{"040000", "tree", filesTree, "files"}, {"100644", "blob", metadataID, "metadata.json"}})
		if e != nil {
			return e
		}
		args := []string{"commit-tree", wrapper}
		if previous != "" {
			args = append(args, "-p", previous)
		}
		id, e := oid(r.History(ctx, w.Store, strings.NewReader(message), args...))
		if e != nil {
			return e
		}
		if e = checkpoint(ctx, f, "commit-object"); e != nil {
			return e
		}
		if e = checkpoint(ctx, f, "before-publish"); e != nil {
			return e
		}
		if e = stable(); e != nil {
			return e
		}
		if e = advance(ctx, r, w.Store, Ref(w.WorkspaceID), id, previous); e != nil {
			return e
		}
		result.ID = id
		result.Changed = true
		if e = checkpoint(ctx, f, "published"); e != nil {
			return fmt.Errorf("snapshot %s was committed; reporting was interrupted: %w", id, e)
		}
		return nil
	})
	return result, err
}
