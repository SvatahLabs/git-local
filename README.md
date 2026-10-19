# Git Local

An external Git command for maintaining independent local history of selected
project files without adding that history to the project's commits or pushes.

Select files that belong to you rather than the project (notes, local
configuration, scratch scripts, agent session files), and git-local snapshots
them into a private store inside `.git`, with its own log, diff, show and
restore. Guards keep the selected files out of mainline commits and pushes,
overlays keep a local patch on a tracked file, and history can be encrypted and
backed up to a separate remote.

**Status:** pre-release. Selection, snapshots, inspection, restore, journaled
recovery, worktrees, guards and automatic snapshots, overlays, encryption at
rest, encrypted backups, maintenance and removal are implemented and tested on
macOS on Apple silicon. CI for Linux, macOS and Windows is configured but hasn't
run yet. The Intel macOS build has only run under Rosetta, and Windows has
never run natively, so neither is qualified.
See the [changelog](CHANGELOG.md) for what changed and the known limitations.

## Install

git-local needs Git 2.50.1 or later; a built executable needs no Go runtime.
Repositories may use either Git ref backend, `files` or `reftable` (Git 3.0's
default).
Put `git-local` on `PATH` and check it with `git local --version`. See
[installing and removing git-local](docs/install.md) for release binaries, the
macOS package, building from source, and uninstalling.

## Quick start

```sh
git local init                        # once per checkout
git local track notes                 # select files or directories
git local commit -m "Save my notes"   # snapshot the selection
git local log
git local diff
git local show latest:notes/todo.md
git local restore --source latest --dry-run -- notes
git local hook install                # keep selected files out of commits and pushes
git local auto install                # snapshot on Git events
git local doctor
```

## Documentation

- [User guide](docs/guide.md): daily use, recovery, encryption, backups,
  worktrees, guards, automatic snapshots, overlays, maintenance and removal.
- [Command reference](docs/commands.md): every command and flag, JSON output
  and exit codes.
- [Installing and removing git-local](docs/install.md).
- [Limits](docs/limits.md): size limits and platform constraints.
- [Changelog](CHANGELOG.md).

## Development

Requires Go 1.26 or later and Git 2.50.1 or later. Initialization checks
required Git behavior using disposable local fixtures; it performs no external
network operations.

```sh
go test -race -parallel 4 -timeout 20m ./...
go vet ./...
go build -o bin/git-local ./cmd/git-local
./bin/git-local --version
```

The version lives in the `VERSION` file and the minimum Git version in
`gitexec.MinimumGit`; the release scripts and CI read them from there.
The Go module path remains `gitlocal` until the published repository is chosen.
Dependencies are Cobra plus Go's platform syscall and Unicode libraries.
