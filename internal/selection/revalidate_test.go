package selection

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/storage"
	"github.com/SvatahLabs/git-local/internal/testutil"
)

// Evidence the same command captured moments earlier (snapshot, track, a
// restore plan, doctor) differs only because a Git command ran meanwhile, so
// Revalidate says to retry even after that command has released its locks;
// evidence a journal recorded says so only while Git is busy.
func TestRevalidateSuggestsRetryForEvidenceOfTheSameCommand(t *testing.T) {
	t.Parallel()
	repo := testutil.New(t)
	repo.Commit()
	r, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	admin := filepath.Join(repo.Root, ".git")
	owner, err := InspectOwnership(ctx, r, repo.Root, admin)
	if err != nil {
		t.Fatal(err)
	}
	repo.Git("commit", "-q", "--allow-empty", "-m", "moved")
	err = owner.Revalidate(ctx, r, repo.Root, admin)
	if !errors.Is(err, storage.ErrConflict) || !strings.Contains(err.Error(), "main HEAD or index changed; retry after Git operations finish") {
		t.Fatalf("revalidate of this command's evidence after a commit: %v", err)
	}
	err = owner.RevalidateRecorded(ctx, r, repo.Root, admin)
	if !errors.Is(err, storage.ErrConflict) || !strings.Contains(err.Error(), "main HEAD or index changed") || strings.Contains(err.Error(), "retry") {
		t.Fatalf("revalidate of recorded evidence after a commit: %v", err)
	}
}

// OwnedError keeps its message and conflict, and Fact drops the advice.
func TestOwnedErrorFactOmitsAdvice(t *testing.T) {
	t.Parallel()
	err := (Ownership{Paths: []string{"notes/a"}}).CheckPaths([]string{"notes/a"})
	var owned *OwnedError
	if !errors.Is(err, storage.ErrConflict) || !errors.As(err, &owned) {
		t.Fatalf("owned path refusal: %v", err)
	}
	if want := `selected path "notes/a" is owned by the main index or HEAD; untrack or exclude it`; !strings.HasSuffix(err.Error(), want) {
		t.Fatalf("owned path message: %v", err)
	}
	if fact := owned.Fact(); strings.Contains(fact, "untrack") || !strings.HasSuffix(fact, `selected path "notes/a" is owned by the main index or HEAD`) {
		t.Fatalf("owned path fact: %q", fact)
	}
}
