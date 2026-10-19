package qualification

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/SvatahLabs/git-local/internal/configtx"
	"github.com/SvatahLabs/git-local/internal/gitexec"
	"github.com/SvatahLabs/git-local/internal/history"
	"github.com/SvatahLabs/git-local/internal/testutil"
	"github.com/SvatahLabs/git-local/internal/workspace"
)

// Run explicitly with GITLOCAL_QUALIFY_PERFORMANCE=1. The 1-GiB case also
// requires GITLOCAL_QUALIFY_LARGE=1; neither belongs in the normal test loop.
func TestSnapshotPerformance(t *testing.T) {
	if os.Getenv("GITLOCAL_QUALIFY_PERFORMANCE") != "1" {
		t.Skip("opt-in disk and performance qualification")
	}
	for _, workload := range []struct {
		name        string
		files       int
		payloadSize int64
		large       bool
	}{
		{"1000-files-100MiB", 1000, 100 << 20, false},
		{"10000-files-1GiB", 10000, 1 << 30, true},
	} {
		t.Run(workload.name, func(t *testing.T) {
			if workload.large && os.Getenv("GITLOCAL_QUALIFY_LARGE") != "1" {
				t.Skip("set GITLOCAL_QUALIFY_LARGE=1 for the 1-GiB case")
			}
			ctx := context.Background()
			repo := testutil.New(t)
			repo.Commit()
			r, err := gitexec.New(repo.RunnerEnv())
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			w, err := workspace.Init(ctx, r, repo.Root)
			if err != nil {
				t.Fatal(err)
			}
			notes := filepath.Join(repo.Root, "notes")
			if err := os.Mkdir(notes, 0700); err != nil {
				t.Fatal(err)
			}
			beforeIndex := repo.Git("ls-files", "--stage", "-z")
			beforeHead := repo.Git("rev-parse", "HEAD")
			baseSize := workload.payloadSize / int64(workload.files)
			extra := workload.payloadSize % int64(workload.files)
			content := make([]byte, baseSize+1)
			for i := 0; i < workload.files; i++ {
				size := baseSize
				if int64(i) < extra {
					size++
				}
				if _, err := rand.Read(content[:size]); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(notes, fmt.Sprintf("%06d.bin", i)), content[:size], 0600); err != nil {
					t.Fatal(err)
				}
			}
			beforeStore, err := directoryBytes(w.Store)
			if err != nil {
				t.Fatal(err)
			}
			trackStart := time.Now()
			if _, err := configtx.Change(ctx, r, repo.Root, true, "notes"); err != nil {
				t.Fatal(err)
			}
			trackElapsed := time.Since(trackStart)
			commitStart := time.Now()
			result, err := history.Commit(ctx, r, repo.Root, "performance qualification")
			commitElapsed := time.Since(commitStart)
			if err != nil || !result.Changed {
				t.Fatalf("snapshot failed: %+v %v", result, err)
			}
			afterStore, err := directoryBytes(w.Store)
			if err != nil {
				t.Fatal(err)
			}
			if got := repo.Git("ls-files", "--stage", "-z"); !bytes.Equal(got, beforeIndex) {
				t.Fatal("performance run changed main index")
			}
			if got := repo.Git("rev-parse", "HEAD"); !bytes.Equal(got, beforeHead) {
				t.Fatal("performance run changed main HEAD")
			}
			selfRSS, childRSS, err := peakRSSBytes()
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("files=%d payload_bytes=%d track_elapsed=%s snapshot_elapsed=%s snapshot_git_processes=%d peak_self_rss_bytes=%d peak_child_rss_bytes=%d private_store_growth_bytes=%d", workload.files, workload.payloadSize, trackElapsed, commitElapsed, result.Subprocesses, selfRSS, childRSS, afterStore-beforeStore)
		})
	}
}

func directoryBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()
		return nil
	})
	return total, err
}
