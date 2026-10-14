package restore

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
)

// Real process death deliberately bypasses defers and exercises ambiguous durable
// publication, not merely error returns from the live executor.
func TestRestoreCrashHelper(t *testing.T) {
	root := os.Getenv("GITLOCAL_TEST_RESTORE_ROOT")
	if root == "" {
		t.Skip("subprocess helper")
	}
	r, err := gitexec.New(os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	cut := os.Getenv("GITLOCAL_TEST_RESTORE_CUT")
	f := func(at string) error {
		if at == cut {
			os.Exit(86)
		}
		return nil
	}
	if id := os.Getenv("GITLOCAL_TEST_RESTORE_ID"); id != "" {
		_, err = recoverRestore(ctx, r, root, id, true, f)
	} else {
		_, err = execute(ctx, r, root, "", os.Getenv("GITLOCAL_TEST_RESTORE_SOURCE"), []string{"notes"}, false, f)
	}
	t.Fatalf("crash point %s was not reached: %v", cut, err)
}
func crash(t *testing.T, env []string, root, source, id, cut string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRestoreCrashHelper$", "-test.timeout=3m")
	cmd.Env = append(env, "GITLOCAL_TEST_RESTORE_ROOT="+root, "GITLOCAL_TEST_RESTORE_SOURCE="+source, "GITLOCAL_TEST_RESTORE_CUT="+cut, "GITLOCAL_TEST_RESTORE_ID="+id)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 86 {
		t.Fatalf("crash %s: %v\n%s", cut, err, out)
	}
}
func TestRestoreProcessDeathBoundaries(t *testing.T) {
	cuts := []string{"before-capture:0", "capture:0", "before-manifest-blob", "manifest-blob", "before-recovery-tree", "recovery-tree", "before-recovery-commit", "recovery-commit", "before-recovery-ref", "recovery-ref", "before-journal", "journal", "before-directory:0", "directory:0", "before-stage:0", "stage-write:0", "stage:0", "before-install:0", "install:0", "before-progress:0", "progress:0", "before-install:1", "install:1", "stage:2", "install:2", "before-outcome", "outcome", "before-archive", "archived"}
	template, source := faultTemplate(t)
	for _, cut := range cuts {
		t.Run(cut, func(t *testing.T) {
			t.Parallel()
			repo, r := cloneRepo(t, template)
			root := repo.Root
			crash(t, repo.RunnerEnv(), root, source, "", cut)
			pending, err := configtx.Pending(ctx, r, root)
			if err != nil {
				t.Fatal(err)
			}
			expected := "baseline"
			if len(pending) > 0 {
				if len(pending) != 1 {
					t.Fatal(pending)
				}
				if _, err = Recover(ctx, r, root, pending[0], false); err != nil {
					t.Fatal(err)
				}
				if _, err = Recover(ctx, r, root, pending[0], false); err != nil {
					t.Fatal(err)
				}
				expected = "source"
			} else if cut == "archived" {
				expected = "source"
			}
			data, err := os.ReadFile(filepath.Join(root, "notes/a"))
			if err != nil || string(data) != expected {
				t.Fatal(string(data), err)
			}
			if expected == "source" {
				if _, err = os.Stat(filepath.Join(root, "notes/b")); !os.IsNotExist(err) {
					t.Fatal("deletion", err)
				}
				data, err = os.ReadFile(filepath.Join(root, "notes/z/new"))
				if err != nil || string(data) != "new" {
					t.Fatal("creation", err)
				}
			}
		})
	}
}
func TestRollbackProcessDeathBoundaries(t *testing.T) {
	template, source := faultTemplate(t)
	for _, cut := range []string{"before-rollback-intent", "rollback-intent", "install:0", "before-stage-cleanup:2", "stage-cleanup:2", "before-remove-directory:0", "remove-directory:0", "outcome", "archived"} {
		t.Run(cut, func(t *testing.T) {
			t.Parallel()
			repo, r := cloneRepo(t, template)
			root := repo.Root
			result, err := execute(ctx, r, root, "", source, []string{"notes"}, false, func(at string) error {
				if at == "stage:2" {
					return errors.New("stop")
				}
				return nil
			})
			if err == nil {
				t.Fatal("fault missed")
			}
			crash(t, repo.RunnerEnv(), root, source, result.TransactionID, cut)
			if _, err = Recover(ctx, r, root, result.TransactionID, true); err != nil {
				t.Fatal(err)
			}
			if _, err = Recover(ctx, r, root, result.TransactionID, true); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(filepath.Join(root, "notes/a"))
			if err != nil || string(data) != "baseline" {
				t.Fatal(string(data), err)
			}
			data, err = os.ReadFile(filepath.Join(root, "notes/b"))
			if err != nil || string(data) != "delete" {
				t.Fatal(string(data), err)
			}
			if _, err = os.Stat(filepath.Join(root, "notes/z")); !os.IsNotExist(err) {
				t.Fatal("directory rollback", err)
			}
			if cut != "archived" {
				if _, err = Recover(ctx, r, root, result.TransactionID, false); err == nil || !strings.Contains(err.Error(), "different outcome") {
					t.Fatal("opposite recovery accepted", err)
				}
			}
		})
	}
}
