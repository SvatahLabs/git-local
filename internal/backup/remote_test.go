package backup

import (
	"context"
	"testing"

	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

func TestEffectiveDestinationUsesGitExpandedFetchAndPushURLs(t *testing.T) {
	repo := testutil.New(t)
	repo.Commit()
	g, err := gitexec.New(repo.RunnerEnv())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	w, err := workspace.Init(context.Background(), g, repo.Root)
	if err != nil {
		t.Fatal(err)
	}
	repo.Git("remote", "add", "origin", "git@origin.example:project/source.git")
	repo.Git("remote", "set-url", "--push", "origin", "ssh://git@push-origin.example/project/source.git")
	repo.Git("remote", "add", "backup", "https://backup.example/project/archive.git")
	urls, err := EffectiveDestination(context.Background(), g, *w, "backup")
	if err != nil || len(urls) != 1 || urls[0] != "https://backup.example/project/archive.git" {
		t.Fatal(urls, err)
	}
	repo.Git("remote", "set-url", "backup", "git@push-origin.example:project/archive.git")
	if _, err := EffectiveDestination(context.Background(), g, *w, "backup"); err == nil {
		t.Fatal("destination on origin push host accepted")
	}
	repo.Git("remote", "set-url", "backup", "alias:project/archive.git")
	repo.Git("config", "url.ssh://git@origin.example/.insteadOf", "alias:")
	if _, err := EffectiveDestination(context.Background(), g, *w, "backup"); err == nil {
		t.Fatal("rewritten destination on origin host accepted")
	}
	if _, err := EffectiveDestination(context.Background(), g, *w, "origin"); err == nil {
		t.Fatal("origin was accepted as backup destination")
	}
}
