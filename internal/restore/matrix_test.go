package restore

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/testutil"
)

// copyTree copies a fixture repository, so each matrix case starts from the
// same saved state without rebuilding it through Git.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0700)
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		default:
			in, err := os.Open(path)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		}
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEveryRestoreFaultIsResolvable(t *testing.T) {
	repo, r, _ := fixture(t)
	template := repo.Root
	write(t, template, "notes/a", "source")
	write(t, template, "notes/z/new", "new")
	track(t, r, template, true, "notes")
	source := save(t, r, template)
	write(t, template, "notes/a", "baseline")
	remove(t, template, "notes/z")
	write(t, template, "notes/b", "delete")
	save(t, r, template)
	// Record the points on a copy, keeping the template unchanged.
	clone := func(t *testing.T) (string, *gitexec.Runner) {
		t.Helper()
		root, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		root = filepath.Join(root, "repo")
		copyTree(t, template, root)
		copy := &testutil.Repo{T: t, Root: root, Env: repo.Env, GitPath: repo.GitPath}
		g, err := gitexec.New(copy.RunnerEnv())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { g.Close() })
		return root, g
	}
	root, g := clone(t)
	var points []string
	if _, err := execute(ctx, g, root, "", source, []string{"notes"}, false, func(name string) error { points = append(points, name); return nil }); err != nil {
		t.Fatal(err)
	}
	journaled := false
	for _, point := range points {
		journaled = journaled || point == "journal"
		directions := []bool{false, true}
		if !journaled {
			// No journal exists yet, so there is nothing to resume or roll back.
			directions = directions[:1]
		}
		for _, rollback := range directions {
			name := point + map[bool]string{false: "/resume", true: "/rollback"}[rollback]
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				root, r := clone(t)
				result, err := execute(ctx, r, root, "", source, []string{"notes"}, false, func(name string) error {
					if name == point {
						return errors.New("injected interruption")
					}
					return nil
				})
				if err == nil {
					t.Fatalf("fault %s missed", point)
				}
				pending, err := configtx.Pending(ctx, r, root)
				if err != nil {
					t.Fatal(err)
				}
				if len(pending) == 0 {
					return
				}
				how := "resume"
				if rollback {
					how = "rollback"
				}
				if _, err := Recover(ctx, r, root, result.TransactionID, rollback); err != nil {
					how = "abandon"
					if _, err := Abandon(ctx, r, root, result.TransactionID); err != nil {
						t.Fatalf("recovery and abandon both failed: %v", err)
					}
				}
				t.Logf("resolved by %s", how)
				if pending, err := configtx.Pending(ctx, r, root); err != nil || len(pending) != 0 {
					t.Fatalf("pending after %s: %v %v", how, pending, err)
				}
				if _, err := configtx.LoadGuardSnapshot(ctx, r, root); err != nil {
					t.Fatalf("guards blocked after %s: %v", how, err)
				}
			})
		}
	}
}
