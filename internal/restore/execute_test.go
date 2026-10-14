package restore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/history"
	"gitlocal/internal/storage"
	"gitlocal/internal/workspace"
)

func TestExecuteAndRecovery(t *testing.T) {
	for _, mode := range []string{"execute", "resume", "rollback", "stage-rollback"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			repo, r, w := fixture(t)
			root := repo.Root
			write(t, root, "notes/a", "source")
			write(t, root, "notes/sub/b", "recreate")
			track(t, r, root, true, "notes")
			source := save(t, r, root)
			write(t, root, "notes/a", "baseline")
			remove(t, root, "notes/sub")
			write(t, root, "notes/c", "delete")
			head := save(t, r, root)
			write(t, root, "notes/unknown", "untouched")
			// Capture unsaved bytes in the recovery snapshot, not merely the baseline.
			write(t, root, "notes/a", "dirty preimage")
			mainBefore := durable(t, filepath.Join(root, ".git/objects"))
			indexBefore, err := os.ReadFile(filepath.Join(root, ".git/index"))
			if err != nil {
				t.Fatal(err)
			}
			stage := "install:0"
			if mode == "stage-rollback" {
				stage = "stage:2"
			}
			var f fault
			if mode != "execute" {
				f = func(at string) error {
					if at == stage {
						return errors.New("injected interruption")
					}
					return nil
				}
			}
			result, err := execute(ctx, r, root, "", source, []string{"notes"}, true, f)
			if mode != "execute" {
				if err == nil {
					t.Fatal("fault missed")
				}
				pending, e := configtx.Pending(ctx, r, root)
				if e != nil || !reflect.DeepEqual(pending, []string{result.TransactionID}) {
					t.Fatal(pending, e)
				}
				if _, e = history.Commit(ctx, r, root, "blocked"); e == nil {
					t.Fatal("pending restore did not block commit")
				}
				rollback := strings.Contains(mode, "rollback")
				result, err = Recover(ctx, r, root, result.TransactionID, rollback)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = Recover(ctx, r, root, result.TransactionID, rollback); err != nil {
					t.Fatal("repeated recovery", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			want := "source"
			if strings.Contains(mode, "rollback") {
				want = "dirty preimage"
			}
			bytes, err := os.ReadFile(filepath.Join(root, "notes/a"))
			if err != nil || string(bytes) != want {
				t.Fatal(string(bytes), err)
			}
			bytes, err = os.ReadFile(filepath.Join(root, "notes/unknown"))
			if err != nil || string(bytes) != "untouched" {
				t.Fatal("unknown sibling", err)
			}
			if strings.Contains(mode, "rollback") {
				if _, err = os.Stat(filepath.Join(root, "notes/sub")); !os.IsNotExist(err) {
					t.Fatal("created directory survived rollback", err)
				}
				bytes, err = os.ReadFile(filepath.Join(root, "notes/c"))
				if err != nil || string(bytes) != "delete" {
					t.Fatal(err)
				}
			} else {
				bytes, err = os.ReadFile(filepath.Join(root, "notes/sub/b"))
				if err != nil || string(bytes) != "recreate" {
					t.Fatal(err)
				}
				if _, err = os.Stat(filepath.Join(root, "notes/c")); !os.IsNotExist(err) {
					t.Fatal("deletion failed", err)
				}
			}
			got, err := history.Resolve(ctx, r, root, "", "latest")
			if err != nil || got.ID != head {
				t.Fatal("user head moved", got.ID, err)
			}
			if !reflect.DeepEqual(mainBefore, durable(t, filepath.Join(root, ".git/objects"))) {
				t.Fatal("main objects mutated")
			}
			indexAfter, e := os.ReadFile(filepath.Join(root, ".git/index"))
			if e != nil || string(indexAfter) != string(indexBefore) {
				t.Fatal("main index mutated", e)
			}
			if _, err = r.History(ctx, w.Store, nil, "rev-parse", "--verify", RecoveryRef(w.WorkspaceID, result.TransactionID)); err != nil {
				t.Fatal("missing recovery ref", err)
			}
			pending, e := configtx.Pending(ctx, r, root)
			if e != nil || len(pending) != 0 {
				t.Fatal(pending, e)
			}
			entries, e := os.ReadDir(filepath.Join(root, "notes"))
			if e != nil {
				t.Fatal(e)
			}
			for _, entry := range entries {
				if storage.IsTempName(entry.Name()) {
					t.Fatal("staging file survived", entry.Name())
				}
			}
		})
	}
}

func TestRestoreMissingSelectedFileNeedsNoOverwrite(t *testing.T) {
	t.Parallel()
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "saved")
	track(t, r, root, true, "notes/a")
	save(t, r, root)
	remove(t, root, "notes/a")
	before := durable(t, root)
	plan, err := Plan(ctx, r, root, "", "latest", []string{"notes/a"}, false)
	if err != nil || len(plan.Operations()) != 1 || plan.Operations()[0].Action != "create" || !plan.Operations()[0].Dirty {
		t.Fatal(plan, err)
	}
	if !reflect.DeepEqual(before, durable(t, root)) {
		t.Fatal("dry-run changed repository")
	}
	result, err := Execute(ctx, r, root, "", "latest", []string{"notes/a"}, false)
	if err != nil || !result.Changed {
		t.Fatal(result, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "notes/a"))
	if err != nil || string(data) != "saved" {
		t.Fatal(string(data), err)
	}
}

func TestCaptureFailureNeverChangesDestinations(t *testing.T) {
	t.Parallel()
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "source")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	write(t, root, "notes/a", "baseline")
	save(t, r, root)
	before := durable(t, filepath.Join(root, "notes"))
	for _, at := range []string{"before-capture:0", "capture:0", "before-manifest-blob", "manifest-blob", "recovery-tree", "recovery-commit", "before-recovery-ref", "recovery-ref", "before-journal"} {
		_, err := execute(ctx, r, root, "", source, []string{"notes"}, false, func(stage string) error {
			if stage == at {
				return fmt.Errorf("injected failure %s", at)
			}
			return nil
		})
		if err == nil {
			t.Fatal("fault missed", at)
		}
		if !reflect.DeepEqual(before, durable(t, filepath.Join(root, "notes"))) {
			t.Fatal("capture failure changed destinations", at)
		}
		ids, e := configtx.Pending(ctx, r, root)
		if e != nil || len(ids) != 0 {
			t.Fatal("pre-journal failure left pending", ids, e)
		}
	}
}

func TestRecoveryPreservesExternalChanges(t *testing.T) {
	// A branch switch alone no longer blocks recovery (GL-010); see
	// TestRecoveryAfterMainCommitIndexAndBranchChange.
	for _, kind := range []string{"edit", "owned", "symlink", "configuration", "tampered-journal"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			repo, r, w := fixture(t)
			root := repo.Root
			write(t, root, "notes/a", "source")
			track(t, r, root, true, "notes")
			source := save(t, r, root)
			write(t, root, "notes/a", "baseline")
			save(t, r, root)
			result, err := execute(ctx, r, root, "", source, []string{"notes"}, false, func(at string) error {
				if at == "journal" {
					return errors.New("stop")
				}
				return nil
			})
			if err == nil {
				t.Fatal("fault missed")
			}
			switch kind {
			case "edit":
				write(t, root, "notes/a", "external")
			case "owned":
				repo.Git("add", "-f", "notes/a")
			case "symlink":
				remove(t, root, "notes")
				if err = os.Symlink(t.TempDir(), filepath.Join(root, "notes")); err != nil {
					t.Skip(err)
				}
			case "configuration":
				p := filepath.Join(w.Common, "info/exclude")
				b, e := os.ReadFile(p)
				if e != nil {
					t.Fatal(e)
				}
				if e = os.WriteFile(p, append(b, []byte("\n# change\n")...), 0600); e != nil {
					t.Fatal(e)
				}
			case "tampered-journal":
				meta, e := openMeta(w.Common)
				if e != nil {
					t.Fatal(e)
				}
				defer meta.Close()
				j, e := readJournal(meta, journalPath(result.TransactionID))
				if e != nil {
					t.Fatal(e)
				}
				j.Manifest.Plan.Operations[0].Path = "app.txt"
				if e = writeJournal(meta, j); e != nil {
					t.Fatal(e)
				}
			}
			before := durable(t, root)
			for _, rollback := range []bool{false, true} {
				if _, err = Recover(ctx, r, root, result.TransactionID, rollback); err == nil {
					t.Fatal("external change accepted")
				}
			}
			if !reflect.DeepEqual(before, durable(t, root)) {
				t.Fatal("recovery changed external state")
			}
		})
	}
}

func TestCancellationAfterJournal(t *testing.T) {
	t.Parallel()
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "source")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	write(t, root, "notes/a", "baseline")
	save(t, r, root)
	canceled, cancel := context.WithCancel(ctx)
	defer cancel()
	result, err := execute(canceled, r, root, "", source, []string{"notes"}, false, func(at string) error {
		if at == "journal" {
			cancel()
		}
		return nil
	})
	if err == nil {
		t.Fatal("cancellation missed")
	}
	if _, err = Recover(ctx, r, root, result.TransactionID, false); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionStopsOnLastMomentChanges(t *testing.T) {
	for _, kind := range []string{"edit", "branch", "symlink", "write-denied"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			repo, r, _ := fixture(t)
			root := repo.Root
			write(t, root, "notes/a", "source")
			track(t, r, root, true, "notes")
			source := save(t, r, root)
			write(t, root, "notes/a", "baseline")
			save(t, r, root)
			external := t.TempDir()
			write(t, external, "a", "outside")
			result, err := execute(ctx, r, root, "", source, []string{"notes"}, false, func(at string) error {
				if at != "before-install:0" {
					return nil
				}
				switch kind {
				case "edit":
					write(t, root, "notes/a", "external")
				case "branch":
					repo.Git("switch", "-q", "-c", "other")
				case "symlink":
					remove(t, root, "notes")
					return os.Symlink(external, filepath.Join(root, "notes"))
				case "write-denied":
					return os.ErrPermission
				}
				return nil
			})
			if err == nil {
				t.Fatal("unsafe final write accepted")
			}
			b, e := os.ReadFile(filepath.Join(external, "a"))
			if e != nil || string(b) != "outside" {
				t.Fatal("wrote through ancestor symlink", e)
			}
			if kind == "edit" {
				b, e = os.ReadFile(filepath.Join(root, "notes/a"))
				if e != nil || string(b) != "external" {
					t.Fatal("external edit overwritten", e)
				}
			}
			if kind == "write-denied" {
				if _, err = Recover(ctx, r, root, result.TransactionID, false); err != nil {
					t.Fatal("write-denied retry", err)
				}
			}
		})
	}
}

func TestInterruptedPartialStagingIsPreserved(t *testing.T) {
	t.Parallel()
	repo, r, w := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "source")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	write(t, root, "notes/a", "baseline")
	save(t, r, root)
	crash(t, repo.RunnerEnv(), root, source, "", "before-stage-write:0")
	ids, err := configtx.Pending(ctx, r, root)
	if err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	meta, err := openMeta(w.Common)
	if err != nil {
		t.Fatal(err)
	}
	defer meta.Close()
	j, err := readJournal(meta, journalPath(ids[0]))
	if err != nil {
		t.Fatal(err)
	}
	stage := j.Manifest.Stages[0].Apply
	before := durable(t, filepath.Join(root, "notes"))
	if _, err = Recover(ctx, r, root, ids[0], false); err == nil {
		t.Fatal("unrecognized partial stage was accepted")
	}
	if !reflect.DeepEqual(before, durable(t, filepath.Join(root, "notes"))) {
		t.Fatal("partial stage or destination changed")
	}
	// Simulate explicit user reconciliation by removing the known empty artifact.
	b, err := os.ReadFile(filepath.Join(root, stage))
	if err != nil || len(b) != 0 {
		t.Fatal("expected empty interrupted stage", err)
	}
	if err = os.Remove(filepath.Join(root, stage)); err != nil {
		t.Fatal(err)
	}
	if _, err = Recover(ctx, r, root, ids[0], false); err != nil {
		t.Fatal(err)
	}
}

func TestBinaryModesAndSymlinkExecution(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX link/mode execution")
	}
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "source")
	if err := os.Chmod(filepath.Join(root, "notes/a"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../outside", filepath.Join(root, "notes/link")); err != nil {
		t.Skip(err)
	}
	binary := bytes.Repeat([]byte{0, 255, 1, 2}, 5<<20)
	if err := os.WriteFile(filepath.Join(root, "notes/binary"), binary, 0600); err != nil {
		t.Fatal(err)
	}
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	write(t, root, "notes/a", "preimage")
	if err := os.Chmod(filepath.Join(root, "notes/a"), 0640); err != nil {
		t.Fatal(err)
	}
	remove(t, root, "notes/link")
	write(t, root, "notes/link", "regular")
	write(t, root, "notes/binary", "small")
	save(t, r, root)
	old, err := os.Open(filepath.Join(root, "notes/a"))
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if _, err = Execute(ctx, r, root, "", source, []string{"notes"}, false); err != nil {
		t.Fatal(err)
	}
	oldBytes := make([]byte, 8)
	if _, err = old.Read(oldBytes); err != nil || string(oldBytes) != "preimage" {
		t.Fatal("existing inode truncated", string(oldBytes), err)
	}
	fi, err := os.Stat(filepath.Join(root, "notes/a"))
	if err != nil || fi.Mode().Perm() != 0740 {
		t.Fatal("permissions", fi, err)
	}
	target, err := os.Readlink(filepath.Join(root, "notes/link"))
	if err != nil || target != "../../outside" {
		t.Fatal("link target", target, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "notes/binary"))
	if err != nil || !bytes.Equal(data, binary) {
		t.Fatal("streamed binary changed", err)
	}
}

func TestImportedRestoreRecoveryWithoutBaseline(t *testing.T) {
	t.Parallel()
	repo, r, sourceWorkspace := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "source")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	destination := filepath.Join(t.TempDir(), "linked")
	repo.Git("worktree", "add", "-q", "-b", "linked", destination)
	if _, err := workspace.Init(ctx, r, destination); err != nil {
		t.Fatal(err)
	}
	write(t, destination, "notes/unknown", "keep")
	track(t, r, destination, true, "notes")
	result, err := execute(ctx, r, destination, sourceWorkspace.WorkspaceID, source, []string{"notes"}, false, func(at string) error {
		if at == "install:0" {
			return errors.New("stop after import")
		}
		return nil
	})
	if err == nil {
		t.Fatal("fault missed")
	}
	if _, err = Recover(ctx, r, root, result.TransactionID, true); err == nil {
		t.Fatal("recovery used the source workspace as destination")
	}
	if _, err = Recover(ctx, r, destination, result.TransactionID, true); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(destination, "notes/a")); !os.IsNotExist(err) {
		t.Fatal("rollback failed to restore absent preimage", err)
	}
	if _, err = Execute(ctx, r, destination, sourceWorkspace.WorkspaceID, source, []string{"notes"}, false); err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(filepath.Join(destination, "notes/a"))
	if err != nil || string(bytes) != "source" {
		t.Fatal(string(bytes), err)
	}
	bytes, err = os.ReadFile(filepath.Join(destination, "notes/unknown"))
	if err != nil || string(bytes) != "keep" {
		t.Fatal("unknown import sibling", err)
	}
}
