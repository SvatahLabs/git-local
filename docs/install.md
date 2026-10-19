# Installing and removing git-local

git-local is a single executable, `git-local`. Git finds it on `PATH`, so
`git local <command>` and `git-local <command>` are the same. It needs Git
2.50.1 or later; it doesn't need Go.

## Install a release binary

Each release has six CGO-free binaries (macOS, Linux and Windows on amd64 and
arm64) with a `release.json` manifest and a `SHA256SUMS` file. The Intel macOS
build has only run under Rosetta, and the Windows builds have never run
natively, so neither is qualified yet; see the
[changelog](../CHANGELOG.md#known-limitations). Check the
download, then put it on `PATH` under the name `git-local` (`git-local.exe` on
Windows) with execute permission. `SHA256SUMS` lists every artifact, so
`--ignore-missing` skips the ones you didn't download:

```sh
shasum -a 256 -c --ignore-missing SHA256SUMS   # macOS; sha256sum -c --ignore-missing SHA256SUMS on Linux
install -m 755 git-local_v1.0.0_darwin_arm64 /usr/local/bin/git-local
git local --version
```

`--version` names the release and the source commit it was built from.

## Install the macOS package

The macOS package installs a universal (Apple silicon and Intel) executable at
`/usr/local/bin/git-local`. It installs nothing else: no Git, no shell
configuration, no hooks, and it runs no repository commands.

```sh
shasum -a 256 -c GitLocal-1.0.0-macos-universal.pkg.sha256
pkgutil --payload-files GitLocal-1.0.0-macos-universal.pkg
sudo installer -pkg GitLocal-1.0.0-macos-universal.pkg -target /
git local --version
```

The package isn't signed or notarized yet. A later version replaces it (package
identifier `org.gitlocal.cli`). Use `git-local --help` for help: Git may route
`git local --help` to its manual-page lookup.

## Build from source

With Go 1.26.9 or later, from a checkout:

```sh
go build -o bin/git-local ./cmd/git-local
./bin/git-local --version   # "dev", and the commit when Go recorded one
```

On Windows, build `bin/git-local.exe`.

Release artifacts come from two scripts; both take the version from the
`VERSION` file and refuse to overwrite an existing artifact, so a released
version is never rebuilt:

- `go run ./scripts/release-binaries.go` builds the six binaries into
  `dist/binaries/v<version>/` (`-host-only` builds only this machine's, `-out`
  picks another new directory) and smoke-tests the native one in a disposable
  repository with no Go on `PATH`. `-smoke-existing <dir>` repeats that check
  for a built directory after verifying its checksum.
- `scripts/build-macos-installer.sh` builds the macOS package into `dist/`
  (`GITLOCAL_DIST_DIR` changes that, `GITLOCAL_GO_BIN` names the Go binary).

## Set up a checkout

Run `git local init` in each checkout you use (or `git local init --from
main` in a new linked worktree to seed it), then `git local track <path>` and
`git local commit -m <message>`. Opt in to the guards that keep selected files
out of mainline commits and pushes with `git local hook install`, and to
automatic snapshots with `git local auto install`. Teammates need nothing
unless they want their own local history. See the [user guide](guide.md).

## Remove git-local from a repository

```sh
git local deinit
```

`deinit` removes everything git-local added to the repository and its
worktrees: overlays (each file keeps its local content as an ordinary change),
the guard and snapshot hooks, the overlay filter and its include, the managed
blocks in `.git/info/attributes` and `.git/info/exclude`, and each worktree's
identity file. Afterwards Git behaves as if git-local was never installed,
even without the executable, except that the selected files stay ignored by
ordinary `.git/info/exclude` lines between `# kept by git local deinit` and
`# end of lines kept by git local deinit`; delete them, or run
`deinit --unignore` (also after an earlier deinit), to have Git see those files.

It keeps the private history and state as
`<git-common-dir>/gitlocal-removed-<id>`; delete that directory when you no
longer need it. `deinit --remove-history` deletes it instead, but refuses while
a quarantine or a restore recovery ref would be lost. `deinit` also refuses
while a transaction is pending, while an overlay's file holds changes the
overlay doesn't, or when an overlay belongs to a worktree that is missing.
Encryption and backup keys outside the repository are never touched. If
`deinit` is interrupted, run it again.

Run `deinit` before removing the executable: an overlay's filter, left
installed, makes Git fail on that file once `git-local` is gone.

## Remove the program

Delete `git-local` from `PATH`. For the macOS package:

```sh
sudo rm /usr/local/bin/git-local
sudo pkgutil --forget org.gitlocal.cli
```

`pkgutil --forget` only drops the installer receipt. Removing the program
doesn't remove any repository's local history; use `deinit` for that.
