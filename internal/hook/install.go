package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

const metadata = "gitlocal/hook-install.json"

type Record struct {
	Name, Directory string
	Existed         bool
	Mode            uint32
}
type Manifest struct{ Records []Record }
type Status struct {
	Current   string
	Installed map[string]bool
	Previous  []string
	Location  Location
	// Tracked names hook files in Current that the current worktree tracks.
	Tracked []string
}

var snapshotEvents = []string{"post-commit", "post-checkout", "pre-rebase", "post-rewrite"}

func validHookName(name string) bool {
	switch name {
	case "pre-commit", "pre-push", "post-commit", "post-checkout", "pre-rebase", "post-rewrite":
		return true
	}
	return false
}

func quotedForOS(s, goos string) string {
	if goos == "windows" {
		// Git for Windows evaluates hooks with its POSIX shell. Its shell
		// accepts drive paths with forward slashes and preserves quoted spaces.
		s = strings.ReplaceAll(s, "\\", "/")
	}
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
func markers(name string) (string, string) {
	return "# >>> git-local " + name + " >>>\n", "# <<< git-local " + name + " <<<\n"
}
func block(name, executable string) []byte {
	return blockForOS(name, executable, runtime.GOOS)
}
func blockForOS(name, executable, goos string) []byte {
	start, end := markers(name)
	check := "-x"
	if goos == "windows" {
		// NTFS does not expose POSIX executable bits. A missing .exe still
		// fails explicitly; attempting a non-executable file fails the hook.
		check = "-f"
	}
	quotedExecutable := quotedForOS(executable, goos)
	if name == "post-commit" || name == "post-checkout" || name == "post-rewrite" {
		return []byte(start + "if [ ! " + check + " " + quotedExecutable + " ]; then\n" +
			"  echo 'git-local: installed binary is missing; reinstall git-local hooks' >&2\n" +
			"else\n" +
			"  " + quotedExecutable + " auto event " + name + " || echo 'git-local: automatic event failed; run git local doctor or git local workspace prune' >&2\n" +
			"fi\n" + end)
	}
	script := start + "if [ ! " + check + " " + quotedExecutable + " ]; then\n" +
		"  echo 'git-local: installed binary is missing; reinstall git-local hooks' >&2\n" +
		"  exit 2\nfi\n"
	if name == "pre-push" {
		script += "gitlocal_push_input=$(mktemp \"${TMPDIR:-/tmp}/gitlocal-push.XXXXXX\") || exit 2\n" +
			"cat > \"$gitlocal_push_input\" || { rm -f \"$gitlocal_push_input\"; exit 2; }\n" +
			quotedExecutable + " guard push \"$1\" < \"$gitlocal_push_input\"\n" +
			"gitlocal_push_status=$?\n" +
			"if [ \"$gitlocal_push_status\" -ne 0 ]; then rm -f \"$gitlocal_push_input\"; exit \"$gitlocal_push_status\"; fi\n" +
			"exec < \"$gitlocal_push_input\"\nrm -f \"$gitlocal_push_input\"\n"
	} else if name == "pre-commit" {
		script += quotedExecutable + " guard staged || exit $?\n"
	} else if name == "pre-rebase" {
		script += quotedExecutable + " auto event " + name + " || exit $?\n"
	}
	return []byte(script + end)
}
func managed(data []byte, name string) bool {
	start, end := markers(name)
	return bytes.Contains(data, []byte(start)) && bytes.Contains(data, []byte(end))
}
func insert(data, addition []byte) ([]byte, error) {
	if len(data) == 0 {
		return append([]byte("#!/bin/sh\n"), addition...), nil
	}
	if !bytes.HasPrefix(data, []byte("#!")) {
		return nil, errors.New("existing hook needs a POSIX shell shebang")
	}
	i := bytes.IndexByte(data, '\n')
	if i < 0 {
		return nil, errors.New("existing hook has no shebang newline")
	}
	shebang := string(data[:i])
	if !strings.Contains(shebang, "sh") && !strings.Contains(shebang, "bash") && !strings.Contains(shebang, "zsh") {
		return nil, errors.New("existing hook is not a shell script")
	}
	out := append([]byte{}, data[:i+1]...)
	out = append(out, addition...)
	out = append(out, data[i+1:]...)
	return out, nil
}
func remove(data []byte, name string) ([]byte, error) {
	start, end := markers(name)
	a := bytes.Index(data, []byte(start))
	b := bytes.Index(data, []byte(end))
	if a < 0 || b < a {
		return nil, fmt.Errorf("managed block missing from %s", name)
	}
	b += len(end)
	out := append([]byte{}, data[:a]...)
	out = append(out, data[b:]...)
	return out, nil
}
func load(common *storage.Dir) (Manifest, error) {
	var m Manifest
	err := common.ReadJSON(metadata, &m)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return m, err
	}
	seen := map[string]bool{}
	for _, record := range m.Records {
		if !validHookName(record.Name) || !filepath.IsAbs(record.Directory) || filepath.Clean(record.Directory) != record.Directory || strings.ContainsAny(record.Directory, "\x00\r\n") || record.Mode > 0777 {
			return Manifest{}, errors.New("invalid hook installation record")
		}
		key := record.Directory + "\x00" + record.Name
		if seen[key] {
			return Manifest{}, errors.New("duplicate hook installation record")
		}
		seen[key] = true
	}
	return m, nil
}
func save(common *storage.Dir, m Manifest) error {
	if len(m.Records) == 0 {
		err := common.Remove(metadata)
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return common.AtomicWrite(metadata, append(b, '\n'), 0600)
}
func open(ctx context.Context, r *gitexec.Runner, cwd string) (*workspace.Context, *storage.Dir, *storage.Lock, error) {
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return nil, nil, nil, err
	}
	if _, err = configtx.CheckStagedPaths(ctx, r, cwd, nil); err != nil {
		return nil, nil, nil, err
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return nil, nil, nil, err
	}
	lock, err := common.Lock(ctx, "gitlocal/locks/repository.lock", false)
	if err != nil {
		common.Close()
		return nil, nil, nil, err
	}
	return w, common, lock, nil
}
func directory(ctx context.Context, r *gitexec.Runner, w *workspace.Context) (string, error) {
	return r.HooksPath(ctx, w.Root)
}
func names(preCommit, prePush bool) []string {
	if !preCommit && !prePush {
		preCommit, prePush = true, true
	}
	var n []string
	if preCommit {
		n = append(n, "pre-commit")
	}
	if prePush {
		n = append(n, "pre-push")
	}
	return n
}

// Install adds the guards to Git's effective hooks directory. A directory
// inside a worktree is always refused; one outside the repository's common
// directory needs allowShared.
//
// A hook whose record and file disagree is reinstalled: a hook file that was
// moved away or lost its block gets a new block and a fresh record, and a
// block with no record in this repository's own hooks directory is adopted (a
// crash between the writes, or a copy of this repository's hooks).
func Install(ctx context.Context, r *gitexec.Runner, cwd, executable string, preCommit, prePush, allowShared bool) ([]string, []string, error) {
	return installNamed(ctx, r, cwd, executable, names(preCommit, prePush), allowShared)
}

func InstallSnapshots(ctx context.Context, r *gitexec.Runner, cwd, executable string, allowShared bool) ([]string, []string, error) {
	return installNamed(ctx, r, cwd, executable, snapshotEvents, allowShared)
}

func installNamed(ctx context.Context, r *gitexec.Runner, cwd, executable string, selected []string, allowShared bool) ([]string, []string, error) {
	var warnings []string
	w, common, lock, err := open(ctx, r, cwd)
	if err != nil {
		return nil, nil, err
	}
	defer common.Close()
	defer lock.Close()
	dir, err := directory(ctx, r, w)
	if err != nil {
		return nil, nil, err
	}
	location, err := locate(ctx, r, w, dir)
	if err != nil {
		return nil, nil, err
	}
	if err := checkLocation(location, dir, allowShared); err != nil {
		return nil, nil, err
	}
	tracked, err := trackedHooks(ctx, r, w, dir, selected)
	if err != nil {
		return nil, nil, err
	}
	if len(tracked) > 0 {
		return nil, nil, fmt.Errorf("%w: hook files %s in %s are tracked by Git; installing would commit this machine's binary path. Call `git local guard staged` and `git local guard push \"$1\"` from those hooks instead", storage.ErrConflict, strings.Join(tracked, ", "), dir)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, nil, err
	}
	d, err := storage.OpenDir(dir)
	if err != nil {
		return nil, nil, err
	}
	defer d.Close()
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, nil, err
	}
	if !filepath.IsAbs(executable) || strings.ContainsAny(executable, "\x00\r\n") {
		return nil, nil, errors.New("invalid git-local executable path")
	}
	m, err := load(common)
	if err != nil {
		return nil, nil, err
	}
	previous := Manifest{Records: append([]Record{}, m.Records...)}
	installed := []string{}
	legacyBackup := false
	type plannedHook struct {
		name             string
		updated, before  []byte
		mode, beforeMode os.FileMode
		beforeExists     bool
	}
	plans := []plannedHook{}
	for _, name := range selected {
		info, e := d.Lstat(name)
		exists := e == nil
		if e != nil && !os.IsNotExist(e) {
			return nil, nil, e
		}
		var old []byte
		if exists {
			if !info.Mode().IsRegular() {
				return nil, nil, fmt.Errorf("%w: %s is not a regular file", storage.ErrConflict, name)
			}
			old, e = d.ReadFile(name, 1<<20)
			if e != nil {
				return nil, nil, e
			}
		}
		before, beforeInfo, beforeExists := bytes.Clone(old), info, exists
		if name == "pre-commit" && bytes.HasPrefix(old, []byte("#!/bin/sh\n# git-local managed pre-commit hook\n")) {
			backupInfo, backupErr := d.Lstat("pre-commit.git-local-original")
			if backupErr == nil {
				if !backupInfo.Mode().IsRegular() {
					return nil, nil, fmt.Errorf("%w: old hook backup is not a regular file", storage.ErrConflict)
				}
				old, e = d.ReadFile("pre-commit.git-local-original", 1<<20)
				if e != nil {
					return nil, nil, e
				}
				info, exists, legacyBackup = backupInfo, true, true
			} else if os.IsNotExist(backupErr) {
				old, info, exists = nil, nil, false
			} else {
				return nil, nil, backupErr
			}
		}
		var record *Record
		for i := range m.Records {
			if m.Records[i].Name == name && m.Records[i].Directory == dir {
				record = &m.Records[i]
				break
			}
		}
		if managed(old, name) && record == nil && location.Kind == LocationShared {
			return nil, nil, fmt.Errorf("%w: unmanaged record for %s", storage.ErrConflict, name)
		}
		if managed(old, name) {
			old, e = remove(old, name)
			if e != nil {
				return nil, nil, e
			}
			if record == nil {
				// An unrecorded block in this repository's own directory:
				// what remains without it is the hook that existed before.
				existed := !bytes.Equal(old, []byte("#!/bin/sh\n"))
				m.Records = append(m.Records, Record{name, dir, existed, mode(info, existed)})
			}
		} else if record != nil {
			// The hook file was replaced or moved away since installation:
			// what is there now is the hook to restore on uninstall.
			record.Existed, record.Mode = exists, mode(info, exists)
		} else {
			m.Records = append(m.Records, Record{name, dir, exists, mode(info, exists)})
		}
		updated, e := insert(old, block(name, executable))
		if e != nil {
			return nil, nil, e
		}
		plan := plannedHook{name: name, updated: updated, before: before, mode: modeForInstall(info, exists), beforeExists: beforeExists}
		if beforeExists {
			plan.beforeMode = beforeInfo.Mode().Perm()
		}
		plans = append(plans, plan)
	}
	rollback := func(written int) error {
		var rollbackErr error
		for i := written - 1; i >= 0; i-- {
			p := plans[i]
			var e error
			if p.beforeExists {
				e = d.AtomicWrite(p.name, p.before, p.beforeMode)
			} else {
				e = d.Remove(p.name)
			}
			if e != nil {
				rollbackErr = errors.Join(rollbackErr, e)
			}
		}
		return errors.Join(rollbackErr, save(common, previous))
	}
	// Record the hooks before writing them, so a crash in between leaves
	// records whose blocks are missing, which install and uninstall repair.
	if err := save(common, m); err != nil {
		return nil, nil, errors.Join(err, save(common, previous))
	}
	for i, p := range plans {
		if err := d.AtomicWrite(p.name, p.updated, p.mode); err != nil {
			return nil, nil, errors.Join(err, rollback(i))
		}
		installed = append(installed, filepath.Join(dir, p.name))
	}
	if legacyBackup {
		if err := d.Remove("pre-commit.git-local-original"); err != nil {
			return installed, warnings, err
		}
	}
	return installed, warnings, nil
}
func mode(info os.FileInfo, exists bool) uint32 {
	if exists {
		return uint32(info.Mode().Perm())
	}
	return 0
}
func modeForInstall(info os.FileInfo, exists bool) os.FileMode {
	if exists {
		return info.Mode().Perm() | 0111
	}
	return 0700
}

// Uninstall removes the guards' managed blocks. A record whose hooks
// directory, hook file or block is gone is dropped, and an unrecorded block in
// the current hooks directory is removed unless the directory is shared.
func Uninstall(ctx context.Context, r *gitexec.Runner, cwd string, preCommit, prePush bool) ([]string, []string, error) {
	return uninstallNamed(ctx, r, cwd, names(preCommit, prePush))
}

func UninstallSnapshots(ctx context.Context, r *gitexec.Runner, cwd string) ([]string, []string, error) {
	return uninstallNamed(ctx, r, cwd, snapshotEvents)
}

func uninstallNamed(ctx context.Context, r *gitexec.Runner, cwd string, selected []string) ([]string, []string, error) {
	w, common, lock, err := open(ctx, r, cwd)
	if err != nil {
		return nil, nil, err
	}
	defer common.Close()
	defer lock.Close()
	m, err := load(common)
	if err != nil {
		return nil, nil, err
	}
	current, err := directory(ctx, r, w)
	if err != nil {
		return nil, nil, err
	}
	currentLocation, err := locate(ctx, r, w, current)
	if err != nil {
		return nil, nil, err
	}
	wanted := map[string]bool{}
	for _, n := range selected {
		wanted[n] = true
	}
	records := append([]Record{}, m.Records...)
	if currentLocation.Kind != LocationShared {
		// Adopt blocks a crash left unrecorded in this repository's own
		// hooks directory.
		for _, name := range selected {
			recorded := false
			for _, record := range records {
				recorded = recorded || record.Name == name && record.Directory == current
			}
			data, e := os.ReadFile(filepath.Join(current, name))
			if !recorded && e == nil && managed(data, name) {
				out, e := remove(data, name)
				if e != nil {
					return nil, nil, e
				}
				existed := !bytes.Equal(out, []byte("#!/bin/sh\n"))
				info, e := os.Lstat(filepath.Join(current, name))
				if e != nil {
					return nil, nil, e
				}
				records = append(records, Record{name, current, existed, mode(info, existed)})
			}
		}
	}
	kept := []Record{}
	removed := []string{}
	var warnings []string
	for i, record := range records {
		if !wanted[record.Name] {
			kept = append(kept, record)
			continue
		}
		dropped, err := uninstallRecord(record)
		if err != nil {
			// Keep the records not yet handled, so a rerun can finish.
			m.Records = append(kept, records[i:]...)
			return removed, warnings, errors.Join(err, save(common, m))
		}
		if dropped != "" {
			warnings = append(warnings, dropped)
			continue
		}
		removed = append(removed, filepath.Join(record.Directory, record.Name))
	}
	m.Records = kept
	return removed, warnings, save(common, m)
}

// uninstallRecord removes one managed block. When the hooks directory, the
// hook file or the block is gone, it changes nothing and explains why the
// record is dropped.
func uninstallRecord(record Record) (string, error) {
	path := filepath.Join(record.Directory, record.Name)
	d, err := storage.OpenDir(record.Directory)
	if os.IsNotExist(err) {
		return fmt.Sprintf("%s no longer exists; dropped its record", path), nil
	}
	if err != nil {
		return "", err
	}
	defer d.Close()
	data, err := d.ReadFile(record.Name, 1<<20)
	if os.IsNotExist(err) {
		return fmt.Sprintf("%s no longer exists; dropped its record", path), nil
	}
	if err != nil {
		return "", err
	}
	if !managed(data, record.Name) {
		return fmt.Sprintf("%s no longer holds a git-local block; left it as it is", path), nil
	}
	out, err := remove(data, record.Name)
	if err != nil {
		return "", err
	}
	if record.Existed {
		return "", d.AtomicWrite(record.Name, out, os.FileMode(record.Mode))
	}
	if bytes.Equal(out, []byte("#!/bin/sh\n")) {
		return "", d.Remove(record.Name)
	}
	return "", d.AtomicWrite(record.Name, out, 0700)
}

func Inspect(ctx context.Context, r *gitexec.Runner, cwd string) (Status, error) {
	w, err := workspace.Discover(ctx, r, cwd)
	if err != nil {
		return Status{}, err
	}
	current, err := directory(ctx, r, w)
	if err != nil {
		return Status{}, err
	}
	common, err := storage.OpenDir(w.Common)
	if err != nil {
		return Status{}, err
	}
	defer common.Close()
	m, err := load(common)
	if err != nil {
		return Status{}, err
	}
	location, err := locate(ctx, r, w, current)
	if err != nil {
		return Status{}, err
	}
	tracked, err := trackedHooks(ctx, r, w, current, append([]string{"pre-commit", "pre-push"}, snapshotEvents...))
	if err != nil {
		return Status{}, err
	}
	s := Status{Current: current, Installed: map[string]bool{}, Location: location, Tracked: tracked}
	for _, record := range m.Records {
		if record.Directory == current {
			data, e := os.ReadFile(filepath.Join(record.Directory, record.Name))
			s.Installed[record.Name] = e == nil && managed(data, record.Name)
		} else {
			s.Previous = append(s.Previous, filepath.Join(record.Directory, record.Name))
		}
	}
	return s, nil
}
