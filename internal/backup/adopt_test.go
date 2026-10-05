package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gitlocal/internal/configtx"
	"gitlocal/internal/gitexec"
	"gitlocal/internal/history"
	"gitlocal/internal/selection"
	"gitlocal/internal/testutil"
	"gitlocal/internal/workspace"
)

func TestAdoptStagedFreshCloneAndInterruptedRecovery(t *testing.T) {
	for _, failAt := range []string{"", "journal", "exclude", "state", "identity"} {
		t.Run("interrupt-"+failAt, func(t *testing.T) {
			source := testutil.New(t)
			source.Commit()
			g, err := gitexec.New(source.RunnerEnv())
			if err != nil {
				t.Fatal(err)
			}
			defer g.Close()
			original, err := workspace.Init(context.Background(), g, source.Root)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(source.Root, "secret.local"), []byte("private backup snapshot\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := configtx.Change(context.Background(), g, source.Root, true, "secret.local"); err != nil {
				t.Fatal(err)
			}
			if _, err := history.Commit(context.Background(), g, source.Root, "private backup snapshot"); err != nil {
				t.Fatal(err)
			}
			key, err := GenerateKey()
			if err != nil {
				t.Fatal(err)
			}
			var encrypted bytes.Buffer
			if err := configtx.ReadSelection(context.Background(), g, source.Root, func(w workspace.Context, _ selection.State, _ []string, recheck func() error) error {
				if err := PackEncrypted(context.Background(), filepath.Join(w.Common, "gitlocal"), w.Admin, &encrypted, key); err != nil {
					return err
				}
				return recheck()
			}); err != nil {
				t.Fatal(err)
			}
			stage, err := DecryptStaged(context.Background(), bytes.NewReader(encrypted.Bytes()), key)
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(stage)
			if failAt == "" {
				blocked := filepath.Join(t.TempDir(), "blocked-clone")
				cmd := exec.Command(source.GitPath, "clone", "-q", "--no-local", source.Root, blocked)
				cmd.Env = source.Env
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("clone conflict checkout: %v %s", err, output)
				}
				if err := os.WriteFile(filepath.Join(blocked, "secret.local"), []byte("leaked"), 0600); err != nil {
					t.Fatal(err)
				}
				source.At(blocked, "add", "-f", "secret.local")
				if _, err := AdoptStaged(context.Background(), g, blocked, stage); err == nil || !strings.Contains(err.Error(), "mainline Git") {
					t.Fatalf("mainline-owned selected path accepted: %v", err)
				}
				if _, err := os.Stat(filepath.Join(blocked, ".git", "gitlocal")); !os.IsNotExist(err) {
					t.Fatalf("conflicting checkout changed: %v", err)
				}
			}
			clone := filepath.Join(t.TempDir(), "clone")
			cmd := exec.Command(source.GitPath, "clone", "-q", "--no-local", source.Root, clone)
			cmd.Env = source.Env
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("clone: %v %s", err, output)
			}
			excludePath := filepath.Join(clone, ".git", "info", "exclude")
			oldExclude, err := os.ReadFile(excludePath)
			if err != nil {
				t.Fatal(err)
			}
			fault := func(step string) error {
				if step == failAt {
					return errors.New("simulated interruption")
				}
				return nil
			}
			info, err := adoptStaged(context.Background(), g, clone, stage, fault)
			if failAt == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("restore interruption was ignored")
				}
				if _, err := adoptStaged(context.Background(), g, clone, stage, nil); err == nil {
					t.Fatal("pending restore was overwritten")
				}
				if failAt == "journal" {
					if err := os.WriteFile(filepath.Join(clone, "secret.local"), []byte("newly staged leak"), 0600); err != nil {
						t.Fatal(err)
					}
					source.At(clone, "add", "-f", "secret.local")
					if _, err := RecoverRestore(context.Background(), g, clone); err == nil || !strings.Contains(err.Error(), "mainline Git") {
						t.Fatalf("recovery ignored a newly staged selected path: %v", err)
					}
					source.At(clone, "rm", "--cached", "-f", "secret.local")
				}
				info, err = RecoverRestore(context.Background(), g, clone)
				if err != nil {
					t.Fatal(err)
				}
			}
			if info.RepositoryID != original.RepositoryID || info.WorkspaceID != original.WorkspaceID {
				t.Fatalf("restored identity: %+v", info)
			}
			if _, err := workspace.Init(context.Background(), g, clone); err != nil {
				t.Fatal("restored workspace did not initialize", err)
			}
			log, err := history.Log(context.Background(), g, clone, 10)
			if err != nil || len(log) != 1 {
				t.Fatalf("restored private history: %v %+v", err, log)
			}
			exclude, err := os.ReadFile(excludePath)
			if err != nil || !bytes.Contains(exclude, []byte("/secret.local")) || !bytes.Contains(exclude, oldExclude) {
				t.Fatalf("restored ignore rules: %v %q", err, exclude)
			}
			if _, err := os.Stat(filepath.Join(clone, ".git", restoreJournalName)); !os.IsNotExist(err) {
				t.Fatalf("restore journal remained: %v", err)
			}
			if _, err := AdoptStaged(context.Background(), g, clone, stage); err == nil || !strings.Contains(err.Error(), "already contains") {
				t.Fatalf("existing state overwritten: %v", err)
			}
		})
	}
}
