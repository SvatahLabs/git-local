package restore

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/testutil"
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

// cloneRepo copies template, a fixture repository (see copyTree), to a new
// temporary directory and opens a runner on the copy.
func cloneRepo(t *testing.T, template *testutil.Repo) (*testutil.Repo, *gitexec.Runner) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(root, "repo")
	copyTree(t, template.Root, root)
	copy := &testutil.Repo{T: t, Root: root, Env: template.Env, GitPath: template.GitPath}
	g, err := gitexec.New(copy.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { g.Close() })
	return copy, g
}

// faultTemplate is the repository every fault and crash case starts from:
// notes tracked, a source snapshot with notes/a and notes/z/new, then a
// baseline snapshot that changes notes/a, deletes notes/z and adds notes/b.
// It returns the template and the source snapshot.
func faultTemplate(t *testing.T) (*testutil.Repo, string) {
	t.Helper()
	repo, r, _ := fixture(t)
	root := repo.Root
	write(t, root, "notes/a", "source")
	write(t, root, "notes/z/new", "new")
	track(t, r, root, true, "notes")
	source := save(t, r, root)
	write(t, root, "notes/a", "baseline")
	remove(t, root, "notes/z")
	write(t, root, "notes/b", "delete")
	save(t, r, root)
	return repo, source
}

func TestEveryRestoreFaultIsResolvable(t *testing.T) {
	template, source := faultTemplate(t)
	// Record the points on a copy, keeping the template unchanged.
	clone := func(t *testing.T) (string, *gitexec.Runner) {
		t.Helper()
		repo, g := cloneRepo(t, template)
		return repo.Root, g
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
				// want checks the files: the source after resume, the
				// baseline after rollback or a stop before the journal.
				want := func(how string, restored bool) {
					t.Helper()
					expected := map[string]string{"notes/a": "baseline", "notes/b": "delete", "notes/z/new": ""}
					if restored {
						expected = map[string]string{"notes/a": "source", "notes/b": "", "notes/z/new": "new"}
					}
					for name, content := range expected {
						b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
						if content == "" && !os.IsNotExist(err) || content != "" && (err != nil || string(b) != content) {
							t.Fatalf("after %s, %s: %q %v", how, name, b, err)
						}
					}
				}
				if len(pending) == 0 {
					want("a stop before the journal", point == "archived")
					return
				}
				// Recovery in the requested direction must finish; it doesn't
				// fall back to abandon (GL-123). Two stops have a documented
				// way out instead: partial bytes of an interrupted stage
				// write are preserved (they could hold an edit) until the
				// staging file is moved aside, and a restore that committed
				// can only be resumed. Each is accepted only at its own
				// points, so a regression that hits it elsewhere fails.
				how := "resume"
				if rollback {
					how = "rollback"
				}
				_, err = Recover(ctx, r, root, result.TransactionID, rollback)
				if err != nil && strings.HasPrefix(point, "before-stage-write:") && strings.Contains(err.Error(), "staging file, which Git ignores") {
					stages, _ := filepath.Glob(filepath.Join(root, "notes", "*", ".gitlocal-tmp-*"))
					more, _ := filepath.Glob(filepath.Join(root, "notes", ".gitlocal-tmp-*"))
					stages = append(stages, more...)
					aside := t.TempDir()
					for i, stage := range stages {
						if !strings.Contains(err.Error(), filepath.ToSlash(strings.TrimPrefix(stage, root+string(filepath.Separator)))) {
							continue
						}
						if e := os.Rename(stage, filepath.Join(aside, fmt.Sprint(i))); e != nil {
							t.Fatal(e)
						}
					}
					how += " after moving the staging file aside"
					_, err = Recover(ctx, r, root, result.TransactionID, rollback)
				} else if err != nil && rollback && (point == "outcome" || point == "before-archive") && strings.Contains(err.Error(), "already completed with a different outcome") {
					how = "resume"
					_, err = Recover(ctx, r, root, result.TransactionID, false)
				}
				if err != nil {
					t.Fatalf("%s failed: %v", how, err)
				}
				t.Logf("resolved by %s", how)
				want(how, strings.HasPrefix(how, "resume"))
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
