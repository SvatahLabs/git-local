package gitexec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"gitlocal/internal/storage"
)

const StoreConfig = "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = true\n\tlogallrefupdates = false\n\tfsync = loose-object,reference\n\tfsyncMethod = fsync\n[gc]\n\tauto = 0\n[maintenance]\n\tauto = false\n"

// InitBare is only for new disposable or journal-owned staging directories.
func (r *Runner) InitBare(ctx context.Context, path string) error {
	if _, err := os.Lstat(path); err == nil {
		return errors.New("refusing to initialize over an existing directory")
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := r.run(ctx, filepath.Dir(path), true, nil, nil, nil, "init", "--bare", "--object-format=sha1", "--template="+filepath.Join(r.private, "template"), "--initial-branch=main", path); err != nil {
		return err
	}
	dir, err := storage.OpenDir(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err = dir.AtomicWrite("config", []byte(StoreConfig), 0600); err != nil {
		return err
	}
	return ValidateStore(path)
}

// storeWalks counts full store walks, for tests.
var storeWalks atomic.Int64

// The v1 store uses one exact config rather than accepting includes, remotes or
// subtly executable settings. Future extensions must explicitly version this.
// ValidateStore also walks every file in the store.
func ValidateStore(path string) error {
	dir, err := storage.OpenDir(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := validateStoreSettings(dir); err != nil {
		return err
	}
	return walkStore(path, dir)
}

// checkStore validates a store before each runner command. The config and
// redirection files are checked every time; the walk of every file, which
// grows with the store, runs once per store for this runner (one command).
func (r *Runner) checkStore(path string) error {
	r.mu.Lock()
	walked := r.walked[path]
	r.mu.Unlock()
	if !walked {
		if err := ValidateStore(path); err != nil {
			return err
		}
		r.mu.Lock()
		if r.walked == nil {
			r.walked = map[string]bool{}
		}
		r.walked[path] = true
		r.mu.Unlock()
		return nil
	}
	dir, err := storage.OpenDir(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return validateStoreSettings(dir)
}

// WalkStoresAgain makes the next command on each store walk it again. A
// long-running command (auto watch) calls it for each unit of work, so a
// store changed after it started is still checked in full.
func (r *Runner) WalkStoresAgain() {
	r.mu.Lock()
	r.walked = nil
	r.mu.Unlock()
}

func validateStoreSettings(dir *storage.Dir) error {
	config, err := dir.ReadFile("config", 64<<10)
	if err != nil {
		return err
	}
	if string(config) != StoreConfig {
		return errors.New("unexpected history configuration; refusing storage redirection or unapproved settings")
	}
	for _, p := range []string{"objects/info/alternates", "objects/info/http-alternates", "shallow", "info/grafts", "refs/replace", "commondir", "gitdir"} {
		if _, err := dir.Lstat(p); err == nil {
			return fmt.Errorf("unsupported history state %s", p)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func walkStore(path string, dir *storage.Dir) error {
	storeWalks.Add(1)
	// Walk without following links, including descendants of objects and refs.
	return filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlinked history state: %s", p)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if err = storage.ValidateInfo(fi); err != nil {
			return err
		}
		if !d.IsDir() && !fi.Mode().IsRegular() {
			return errors.New("unsupported history file type")
		}
		if fi.Mode().IsRegular() {
			rel, e := filepath.Rel(path, p)
			if e != nil {
				return e
			}
			if e = dir.CheckFile(rel); e != nil {
				return e
			}
		}
		if strings.HasSuffix(p, ".promisor") {
			return errors.New("promisor history state is unsupported")
		}
		return nil
	})
}
