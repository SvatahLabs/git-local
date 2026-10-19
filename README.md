# Git Local

**Private, versioned history for the files you and your coding agents keep
out of Git.**

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
run yet. The Intel macOS build has only run under Rosetta, so it isn't
qualified. **Windows is experimental:** its builds are published but have never
run natively, it has known defects (among them key-file protection, so don't
keep sensitive-file or backup keys there), and it isn't supported for 1.0.
See the [changelog](CHANGELOG.md) for what changed and the known limitations.

## For coding agents

Agent instructions and session files (`CLAUDE.local.md`, `AGENTS.md` notes,
local prompts, scratch plans, handovers) usually belong to one developer, not
to the project, so they stay untracked and their history is lost. With
`git local track CLAUDE.local.md .agents/` they keep their own snapshots in
`.git` and can be diffed and restored like any other history. With the
guards (`git local hook install`) they can't reach a mainline commit or push
by accident, and with automatic snapshots (`git local auto install`) each new
worktree an agent creates is seeded with them. An agent uses the same verbs
it uses for Git (`git local status`, `log`, `diff`, `restore`), and every
command takes `--json` for scripts.

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

Requires Go 1.26.9 or later and Git 2.50.1 or later. Initialization checks
required Git behavior using disposable local fixtures; it performs no external
network operations.

```sh
go test -race -parallel 4 -timeout 30m ./...
go vet ./...
go build -o bin/git-local ./cmd/git-local
./bin/git-local --version
```

The version lives in the `VERSION` file and the minimum Git version in
`gitexec.MinimumGit`; the release scripts and CI read them from there.
The Go module path is `github.com/SvatahLabs/git-local`.
Dependencies are Cobra plus Go's platform syscall and Unicode libraries.

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE).
