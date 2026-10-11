//go:build !windows

package hook_test

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestInstallKeepsHookModeAndRefusesOtherInterpreters: install must not make
// a disabled hook executable, must report it, and must not insert its POSIX
// block into a hook another interpreter runs.
func TestInstallKeepsHookModeAndRefusesOtherInterpreters(t *testing.T) {
	binary := testBinary(t)
	repo, run := hookRepo(t, binary, nil)
	hooks := filepath.Join(repo.Root, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	preCommit := filepath.Join(hooks, "pre-commit")

	disabled := []byte("#!/bin/sh\necho disabled hook\n")
	if err := os.WriteFile(preCommit, disabled, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(preCommit, 0644); err != nil {
		t.Fatal(err)
	}
	if out := mustRun(t, run, "hook", "install", "--pre-commit"); !strings.Contains(out, "isn't executable, so Git won't run it") {
		t.Fatalf("install into a non-executable hook gave no warning:\n%s", out)
	}
	info, err := os.Stat(preCommit)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("install changed a disabled hook's mode to %o", info.Mode().Perm())
	}
	if !hasBlock(readHook(t, preCommit), "pre-commit") {
		t.Fatal("install skipped the disabled hook")
	}
	if out := mustRun(t, run, "hook", "status"); !strings.Contains(out, "pre-commit: installed, but the hook file isn't executable") {
		t.Fatalf("status didn't report the disabled hook:\n%s", out)
	}
	if out := mustRun(t, run, "hook", "status", "--json"); !strings.Contains(out, `"disabled":["pre-commit"]`) {
		t.Fatalf("JSON status didn't list the disabled hook:\n%s", out)
	}
	mustRun(t, run, "hook", "uninstall")
	if data := readHook(t, preCommit); !bytes.Equal(data, disabled) {
		t.Fatalf("uninstall: %q", data)
	}
	if info, err := os.Stat(preCommit); err != nil || info.Mode().Perm() != 0644 {
		t.Fatalf("uninstall mode: %v %v", info.Mode(), err)
	}

	for _, shebang := range []string{"#!/usr/bin/env pwsh", "#!/usr/bin/fish", "#!/bin/csh -f", "#!/usr/bin/env xonsh", "#!/usr/bin/env python3", "#!/bin/busybox awk", "#!/usr/bin/env -u sh python3", "#!/usr/bin/env VAR=sh"} {
		other := []byte(shebang + "\necho other interpreter\n")
		if err := os.WriteFile(preCommit, other, 0700); err != nil {
			t.Fatal(err)
		}
		out, err := run("hook", "install")
		if err == nil || !strings.Contains(out, "git local guard staged") || !strings.Contains(out, "not a POSIX shell") {
			t.Fatalf("%s: install was not refused with guidance: %v\n%s", shebang, err, out)
		}
		if data := readHook(t, preCommit); !bytes.Equal(data, other) {
			t.Fatalf("%s: refused install changed the hook: %q", shebang, data)
		}
		if _, err := os.Stat(filepath.Join(hooks, "pre-push")); !os.IsNotExist(err) {
			t.Fatalf("%s: refused install wrote pre-push: %v", shebang, err)
		}
	}

	shells := []string{"#!/bin/sh", "#!/bin/bash -e", "#!/usr/bin/env bash", "#!/usr/bin/env -S bash -e", "#!/bin/dash", "#!/bin/ksh", "#!/bin/zsh", "#!/bin/ash", "#!/bin/mksh", "#!/bin/busybox sh", "#!/usr/bin/env -S -u NAME sh"}
	if runtime.GOOS != "linux" {
		shells = append(shells, "#!/usr/bin/env -u NAME sh", "#!/usr/bin/env --unset=NAME sh", "#!/usr/bin/env VAR=x sh", "#!/usr/bin/env -i PATH=/bin:/usr/bin sh")
	}
	for _, shebang := range shells {
		shell := []byte(shebang + "\necho shell hook\n")
		if err := os.WriteFile(preCommit, shell, 0700); err != nil {
			t.Fatal(err)
		}
		mustRun(t, run, "hook", "install", "--pre-commit")
		if data := readHook(t, preCommit); !hasBlock(data, "pre-commit") || !bytes.HasPrefix(data, []byte(shebang+"\n")) {
			t.Fatalf("%s: install: %q", shebang, data)
		}
		mustRun(t, run, "hook", "uninstall", "--pre-commit")
		if data := readHook(t, preCommit); !bytes.Equal(data, shell) {
			t.Fatalf("%s: uninstall: %q", shebang, data)
		}
	}
}
