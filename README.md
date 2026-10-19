# Git Local

**Private, versioned history for the files you and your coding agents keep
out of Git.**

`git local` is a Git command. It keeps a separate history for files that you
choose. That history lives inside `.git`. It never goes into your project's
commits or pushes.

## Why use it

Coding agents and developers create files that should not be committed:

- agent instructions, such as `CLAUDE.local.md` and local `AGENTS.md` notes
- plans, prompts and handover notes
- local configuration and `.env` files
- scratch scripts and personal notes

These files are usually ignored by Git. Ignored files have no history. If an
agent overwrites or deletes one, the old version is gone.

`git local` fixes this:

- **History.** Save snapshots of the selected files. View the log, diff two
  versions, print an old version, and restore it.
- **Guards.** Git hooks stop a selected file from being committed or pushed
  by mistake.
- **Automatic snapshots.** Git hooks take a snapshot on commit, checkout,
  rebase and rewrite.
- **Worktrees.** Agents often work in a new Git worktree. A new worktree gets
  a copy of the selected files from the main checkout.
- **Writer IDs.** Each snapshot can record who made it, for example
  `--writer agent-1`. `log --writer` filters by it.
- **JSON output.** Every command accepts `--json`, so scripts and agents can
  read the result.
- **Encryption and backup.** Encrypt sensitive files in the history. Back up
  the history, encrypted, to a separate Git remote.

## Install

Requirements:

- Git 2.50.1 or later
- macOS or Linux (Windows builds exist but are experimental)

From a release:

1. Download the binary for your system and `SHA256SUMS` from the
   [releases page](https://github.com/SvatahLabs/git-local/releases).
2. Check the download:
   `shasum -a 256 -c --ignore-missing SHA256SUMS`
3. Install it on your `PATH` as `git-local`:
   `install -m 755 git-local_v1.0.0_darwin_arm64 /usr/local/bin/git-local`
4. On macOS, remove the download quarantine flag:
   `xattr -d com.apple.quarantine /usr/local/bin/git-local`
5. Check it: `git local --version`

From source (needs Go 1.26.9 or later):

```sh
git clone https://github.com/SvatahLabs/git-local.git
cd git-local
go build -o bin/git-local ./cmd/git-local
install -m 755 bin/git-local /usr/local/bin/git-local
```

More options, including the macOS package, checking release signatures, and
uninstalling: [docs/install.md](docs/install.md).

## How to use

Set up a repository once:

```sh
git local init                       # create the private history store
git local track CLAUDE.local.md      # select a file
git local track .agents/ notes/      # select directories
git local hook install               # block selected files from commits and pushes
git local auto install               # take snapshots on Git events
```

Save and inspect history:

```sh
git local status                     # what changed since the last snapshot
git local commit -m "Before refactor"
git local commit -m "Session end" --if-changed --writer agent-1
git local log
git local diff
git local show latest:CLAUDE.local.md
```

Restore a file:

```sh
git local restore --source latest --dry-run -- CLAUDE.local.md   # preview
git local restore --source latest -- CLAUDE.local.md
```

Check health and recover:

```sh
git local doctor                     # read-only health check
git local recover --list             # list interrupted operations
```

Stop using it in a repository:

```sh
git local deinit                     # removes setup, keeps the private history
```

### Use with coding agents

- Add `git local commit -m "<summary>" --if-changed --writer <agent-name>` to
  the agent's session-end or pre-compaction hook.
- Tell the agent to run `git local status` and `git local log` before editing
  its notes, and `git local restore` to undo a bad edit.
- Use `--json` when the agent or a script reads the output.
- Run `git local auto install` in the main checkout so each new worktree
  starts with the selected files.

## Documentation

- [User guide](docs/guide.md): daily use, recovery, encryption, backups,
  worktrees, guards, automatic snapshots, overlays, maintenance and removal
- [Command reference](docs/commands.md): every command and flag, JSON output
  and exit codes
- [Install and remove](docs/install.md)
- [Limits](docs/limits.md): size limits, platforms and filesystems
- [Changelog](CHANGELOG.md): changes and known limitations

## Status

Version 1.0.0 is not released yet.

- Tested on macOS on Apple silicon.
- CI builds and tests on Linux, macOS and Windows.
- The Intel macOS build has only run under Rosetta. It is not qualified.
- Windows is experimental. Its tests pass in CI on Windows Server. It has not
  been used on desktop Windows and has known defects. Do not keep encryption
  or backup keys on Windows.

## How to develop

Requirements: Go 1.26.9 or later and Git 2.50.1 or later.

```sh
git clone https://github.com/SvatahLabs/git-local.git
cd git-local
go build -o bin/git-local ./cmd/git-local
./bin/git-local --version
go vet ./...
go test -race -parallel 4 -timeout 30m ./...
```

Notes:

- The tests create temporary repositories. They make no network calls.
- The full test suite takes about 15 to 20 minutes.
- The version is in `VERSION`. The minimum Git version is in
  `gitexec.MinimumGit`. Scripts and CI read both from there.
- Release builds: `go run ./scripts/release-binaries.go` and
  `scripts/build-macos-installer.sh`. See
  [docs/install.md](docs/install.md#build-from-source).
- The Go module path is `github.com/SvatahLabs/git-local`.
- Dependencies: Cobra, plus Go's `x/sys` and `x/text`.

## License

Apache License 2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
