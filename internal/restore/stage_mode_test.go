package restore

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// A process killed after the last stage write but before its chmod leaves
// complete bytes at the 0600 creation mode. Recovery must recognize them.
func TestRecoveryRecognizesCompleteStageBeforeChmod(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows reports writable files as 0666 regardless of creation mode")
	}
	for _, perm := range []os.FileMode{0644, 0755} {
		for _, rollback := range []bool{false, true} {
			t.Run(fmt.Sprintf("%o/rollback=%v", perm, rollback), func(t *testing.T) {
				t.Parallel()
				repo, r, w := fixture(t)
				root := repo.Root
				target := filepath.Join(root, "notes", "a")
				write(t, root, "notes/a", "source")
				if err := os.Chmod(target, perm); err != nil {
					t.Fatal(err)
				}
				track(t, r, root, true, "notes")
				source := save(t, r, root)
				write(t, root, "notes/a", "baseline")
				if err := os.Chmod(target, perm); err != nil {
					t.Fatal(err)
				}
				save(t, r, root)
				crash(t, repo.RunnerEnv(), root, source, "", "stage-write:0")
				meta, err := openMeta(w.Common)
				if err != nil {
					t.Fatal(err)
				}
				defer meta.Close()
				entries, err := os.ReadDir(filepath.Join(w.Common, "gitlocal", "transactions"))
				if err != nil {
					t.Fatal(err)
				}
				var id string
				for _, entry := range entries {
					if entry.Name() != "completed" {
						id = entry.Name()[len("restore-") : len(entry.Name())-len(".json")]
					}
				}
				j, err := readJournal(meta, journalPath(id))
				if err != nil {
					t.Fatal(err)
				}
				stage := filepath.Join(root, j.Manifest.Stages[0].Apply)
				if info, err := os.Lstat(stage); err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("expected a complete 0600 stage: %v %v", info, err)
				}
				if _, err := Recover(ctx, r, root, id, rollback); err != nil {
					t.Fatalf("recovery: %v", err)
				}
				want := "source"
				if rollback {
					want = "baseline"
				}
				data, err := os.ReadFile(target)
				info, statErr := os.Lstat(target)
				if err != nil || statErr != nil || string(data) != want || info.Mode().Perm() != perm {
					t.Fatalf("restored file: %q %v %v %v", data, info, err, statErr)
				}
				for _, stages := range j.Manifest.Stages {
					for _, name := range []string{stages.Apply, stages.Rollback} {
						if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
							t.Fatalf("stage %s remains: %v", name, err)
						}
					}
				}
			})
		}
	}
}
