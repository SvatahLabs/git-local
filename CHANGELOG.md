# Changelog

All notable changes to git-local. Versions follow `major.minor.patch`; a
rebuilt distributable always gets a new patch version.

## 1.0.0 (unreleased)

The first release meant for general use. It replaces the 0.1.4 test builds.

### Supported platforms

- Release builds for macOS (Apple silicon and Intel), Linux and Windows
  (amd64 and arm64); each needs Git 2.50.1 or later. Building from source
  needs Go 1.26 or later.
- The test suite runs on macOS on Apple silicon. CI for Linux, macOS and
  Windows is configured but hasn't run yet.
- The Intel macOS build has only run under Rosetta on Apple silicon; the test
  suite hasn't run on an Intel Mac.
- Windows isn't qualified: it has never run natively, and whether it stays in
  1.0 is still to be decided. See Known limitations.

### Upgrading from 0.1.4

- Upgrade every worktree and every machine that shares a repository at once,
  and don't downgrade afterwards. This version's managed `info/exclude` block
  starts with `.gitlocal-tmp-*`; once it rewrites the block (`init`, `track`,
  `untrack`), 0.1.4's `commit` fails with "managed ignore block was edited".
- Run `git local init` once in each repository: it migrates overlays installed
  by 0.1.4 and refreshes an outdated ignore block.
- Run `git local hook install` and `git local auto install` again after
  upgrading the binary, so the hooks call it and their managed blocks are
  current.
- `hook install` and `auto install` now refuse some hook setups that 0.1.4
  accepted (see Behavior changes). For a hooks directory inside a worktree
  (husky's `.husky`, `core.hooksPath=.githooks`), tracked hook files, or a
  hook that isn't a POSIX shell script, call `git local guard staged` and
  `git local guard push "$1"` (for snapshots, `git local commit --if-changed`)
  from your hooks instead; for a shared or global `core.hooksPath`, pass
  `--allow-shared-hooks-dir`.
- Backups made by 0.1.4 (archive format 1) are refused: they aren't bound to
  their ref. Make a new backup with `git local backup push` after upgrading.
- Snapshots whose metadata exceeds 1 MiB are written in a form 0.1.4 can't
  read.
- Seeding a worktree with a large selection keeps the seed's file list in
  `.git/gitlocal/transactions/seed-files/`, which 0.1.4 takes for an unknown
  pending transaction (it then refuses every command) and refuses in backups.
- If a sensitive-file policy from 0.1.4 recorded a directory as a file rule,
  or a sensitive path spelled differently from the file on disk (letter case
  or Unicode normalization), snapshots that would save the files it should
  cover are refused until you correct the policy with
  `git local encrypt migrate`; `git local doctor` prints the command.

### Behavior changes

- `status` and snapshots hash files in process and skip files whose content
  the previous snapshot already holds, instead of starting a Git process per
  selected file: `status` on 20,000 files went from about 400 s to seconds.
- A scoped snapshot (`commit -- <paths>`) after an `untrack` or `exclude`
  drops the no-longer-selected saved files instead of refusing until a full
  snapshot; earlier snapshots keep them. Older builds refuse to read such a
  snapshot, so upgrade every worktree together.

- `overlay add` and `doctor` refuse `core.autocrlf=input` as well as `true`
  (common as a macOS global setting), and `git local reset --hard` refuses in
  a repository with overlays while it is set; run
  `git config core.autocrlf false` in the repository.
- On Windows, 8.3-style short names (`PROGRA~1`) are refused in selections and
  restore paths.
- Exit codes follow the documented contract: 0 success, 1 failure, guard
  violation or cancellation, 2 invalid usage (including a mistyped subcommand
  or extra arguments), 3 conflict or recovery required, including a guard that
  can't check the repository; `auto watch` exits 3 after five snapshot
  conflicts in a row. SIGTERM and SIGHUP stop `auto watch` cleanly
  (exit 0); other commands exit 1 `canceled`.
- `show` and `diff` print at most 16 MiB; larger Git output that isn't a
  listing is refused with "Git output exceeded safety limit".
- HTTPS backup remotes use the credential helpers, `credential.useHttpPath`
  and `credential.username` (and their `credential.<url>.*` forms), and
  `http.sslCAInfo`, `http.sslCAPath` and `http.proxy` (and their
  `http.<url>.*` forms) from your global and system Git config; repository
  config is ignored, conditional includes (`includeIf`) are evaluated outside
  the repository, and redirects aren't followed.
- `hook install` and `auto install` refuse a hooks directory inside a worktree
  (for example husky's `.husky` or `core.hooksPath=.githooks`) and hook files
  Git tracks, and refuse a directory outside the repository, such as a shared
  or global `core.hooksPath`, unless `--allow-shared-hooks-dir` is given.
- They add their block to an existing hook only when its first line runs a
  POSIX shell (`sh`, `bash`, `dash`, `ksh`, `zsh`, `ash` or `mksh`, directly,
  through `busybox` or through `env`); any other hook, including one with no
  `#!` line, is refused. On Linux, `env` options or `NAME=value` assignments
  in that line need `env -S`. A hook disabled by removing its execute bits
  stays disabled, with a warning.

### Added

- `git local deinit` removes git-local from a repository and its worktrees
  (overlays, hooks, managed ignore and attribute blocks, the required filter,
  workspace identities) and keeps the private history unless you pass
  `--remove-history`. Every check that can refuse runs before its first
  change, and a rerun finishes an interrupted one; until it has, `init` in a
  worktree still holding an old identity refuses without creating anything
  and names `deinit`.
- `git local maintenance` packs the private history store and deletes objects
  no snapshot or recovery ref uses; `doctor` reports the store's size. A
  snapshot also packs the store once it holds 6,700 loose objects or 256 MiB
  of them, so it no longer grows until every command slows down.
- `git local encrypt migrate` corrects an existing sensitive-file policy with
  the same key.
- `git local recover <id> --abandon` gives up a transaction that can neither
  resume nor roll back, quarantining what it held.
- `overlay remove --workspace <uuid>` for overlays of retired worktrees.
- `hook install --allow-shared-hooks-dir` and
  `auto install --allow-shared-hooks-dir` install into a hooks directory
  outside the repository, such as a global `core.hooksPath`. A checkout in a
  repository git-local isn't initialized in is then silent; before, every
  checkout there warned "automatic event failed".
- Shell completion completes `git local …` as well as `git-local …` in bash,
  and in zsh with zsh's bundled `_git` completion or Git's own
  `git-completion.zsh`.
- `--version` shows the source commit of release builds.

### Fixed

About a hundred defects found in pre-release reviews, among them:

- Leaks: sensitive subdirectories of a tracked directory stored in plaintext;
  overlay content exported by `git archive`; staging files neither ignored nor
  guarded; guard misses of partially staged overlay patches.
- Recovery: every interrupted transaction (configuration, seed, restore,
  encryption migration, initialization) can be resumed, rolled back or
  abandoned without editing internal files, also a seed whose file list was
  lost; the messages name what blocks recovery and the way out. A seed
  stopped by a commit or another change to main in its worktree can be
  resumed or rolled back (it could only be abandoned).
- A `git local init` in another worktree while `deinit` removed the
  identity files that don't belong to the live state could lose its new
  identity once the private state was set aside. Init and deinit now take
  `<common-git-dir>/gitlocal-identity.lock` for that step; deinit removes it
  last, and an init that waited for it stops and is run again.
- Scale: large index and tree listings, large snapshot metadata, long exclude
  patterns, per-commit process spawning in guards and doctor, and restore
  time, which grew with the square of the restored file count and is now
  linear: each file's step re-checks that file and the cheap global state
  (main HEAD, branch and index file), everything is re-checked at each
  phase boundary and at the end, and main ownership whenever that state
  changed. The branch and both snapshot heads are read with Git only when a
  file that can change them changed (HEAD, the refs, the configuration, the
  private store's refs), which halves the Git processes per restored file. Restore and seeding no longer check each file against every path
  in the main index, so their time per file doesn't grow with the size of
  the mainline repository. Seeding no longer lists the main index and starts
  Git processes for each file, sensitive files and symlinks included, syncs
  each file's directory once, and reads the workspaces' configuration again
  only when it changed (a selection of thousands of rules made each file
  cost about 0.1 s); it is about six times faster.
- Seeding a worktree from a selection of more than a few thousand files
  failed ("configuration transaction exceeds metadata size limit"); a seed
  journal now keeps a long file list beside it, and every journal kind is
  written and read back with the same limit (encryption-migration journals
  were written without one and read back with 1 MiB).
- Backups: archives are bound to their ref, a backup made on another OS is no
  longer refused (tested only by simulation on POSIX), leftover private files
  no longer make backups unrestorable, HTTPS remotes authenticate, remote
  host checks canonicalize addresses, and a failed push or fetch names its
  cause (certificate, authentication, connection or proxy, or the remote's
  refusal) instead of one generic message, without printing credentials.
- Restore and seeding trust a file's unchanged stat only once its timestamp
  is at least 20 ms older than when it was read. Linux stamps files from a
  coarse kernel clock, so a same-size rewrite within the same tick (of
  `selection.json`, HEAD or the index) could go unseen until the next full
  check.
- Output limits: a Git command whose output passes its limit is stopped
  instead of running to the end, and `check-ignore` keeps a long ignore rule
  once rather than once per matching path.
- Security: `golang.org/x/text` is updated to v0.39.0; earlier versions could
  loop forever normalizing an invalid path name (GO-2026-5970).
- Platforms and layouts: relative and bare worktree layouts, shared and
  tracked hook directories, macOS case-insensitive key paths, `F_FULLFSYNC`
  for directory syncs.

### Known limitations

- **Windows** isn't qualified: it has never run on native hardware or CI.
  Read-only targets and 8.3 short names are implemented but unverified.
  Key-file protection has known defects: a new key file is restricted only
  after it is created, so another user can open it in between; loading doesn't
  refuse a key that other users can write or whose access list they can
  change; and keys created by earlier releases are refused on load with no fix
  offered. Until these are fixed, Windows isn't supported for sensitive-file
  or backup keys.
  A command that waits for the repository lock while `deinit` removes the
  private state takes the lock and goes on instead of stopping, and `deinit`
  then fails; a rerun finishes it.
- **Restore** takes about a fifth of a second per restored file on a lightly
  loaded machine (it runs about a dozen Git processes per file, hashing and
  reading each file's content), also in a repository whose main index holds
  90,000 paths.
- **Seeding** a worktree takes about 12 ms per seeded file on a busy machine,
  most of it spent making each file durable.
- **Maintenance:** snapshots pack loose objects; run `git local maintenance`
  now and then to repack the store fully.
- **Limits:** see [docs/limits.md](docs/limits.md); among them managed ignore
  rules up to 256 KiB, restore and encryption-migration journals up to 16 MiB,
  configuration and seed journals up to 1 MiB (a seed's file list up to
  64 MiB beside its journal), `show`/`diff` output up to 16 MiB, and `log` up
  to 1,000 entries per call.
- Mixed `core.ignorecase` across worktrees over-ignores (never leaks).
- The macOS installer isn't signed or notarized yet.
