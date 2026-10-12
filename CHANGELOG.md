# Changelog

All notable changes to git-local. Versions follow `major.minor.patch`; a
rebuilt distributable always gets a new patch version.

## 1.0.0 (unreleased)

The first release meant for general use. It replaces the 0.1.4 test builds.

### Supported platforms

- macOS (Apple silicon and Intel), Linux and Windows (amd64 and arm64), with
  Git 2.50.1 or later. Building from source needs Go 1.26 or later.
- The test suite runs on macOS. The CI matrix covers Linux and Windows but
  hasn't run yet, and Windows hasn't been qualified on native hardware; see
  Known limitations.

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
- Backups made by 0.1.4 (archive format 1) are refused: they aren't bound to
  their ref. Make a new backup with `git local backup push` after upgrading.
- Snapshots whose metadata exceeds 1 MiB are written in a form 0.1.4 can't
  read.
- If a sensitive-file policy from 0.1.4 recorded a directory as a file rule,
  snapshots that would save the files below it are refused until you correct
  the policy with `git local encrypt migrate`; `git local doctor` prints the
  command.

### Behavior changes

- `overlay add` and `doctor` refuse `core.autocrlf=input` as well as `true`
  (common as a macOS global setting), and `git local reset --hard` refuses in
  a repository with overlays while it is set; run
  `git config core.autocrlf false` in the repository.
- On Windows, 8.3-style short names (`PROGRA~1`) are refused in selections and
  restore paths.
- Exit codes follow the documented contract: 0 success, 1 failure or
  cancellation, 2 invalid usage (including a mistyped subcommand or extra
  arguments), 3 conflict or recovery required; `auto watch` exits 3 after five
  snapshot conflicts in a row. SIGTERM and SIGHUP stop `auto watch` cleanly
  (exit 0); other commands exit 1 `canceled`.
- `show` and `diff` print at most 16 MiB; larger Git output that isn't a
  listing is refused with "Git output exceeded safety limit".
- HTTPS backup remotes use the credential helpers, `credential.useHttpPath`,
  `credential.<url>.username`, `http.sslCAInfo`, `http.sslCAPath` and
  `http.proxy` (and their `http.<url>.*` forms) from your global and system
  Git config; repository config is ignored and redirects aren't followed.

### Added

- `git local deinit` removes git-local from a repository and its worktrees
  (overlays, hooks, managed ignore and attribute blocks, the required filter,
  workspace identities) and keeps the private history unless you pass
  `--remove-history`.
- `git local maintenance` packs the private history store and deletes objects
  no snapshot or recovery ref uses; `doctor` reports the store's size.
- `git local encrypt migrate` corrects an existing sensitive-file policy with
  the same key.
- `git local recover <id> --abandon` gives up a transaction that can neither
  resume nor roll back, quarantining what it held.
- `overlay remove --workspace <uuid>` for overlays of retired worktrees.
- Shell completion completes `git local …` as well as `git-local …` in bash
  and zsh.
- `--version` shows the source commit of release builds.

### Fixed

About a hundred defects found in pre-release reviews, among them:

- Leaks: sensitive subdirectories of a tracked directory stored in plaintext;
  overlay content exported by `git archive`; staging files neither ignored nor
  guarded; guard misses of partially staged overlay patches.
- Recovery: every interrupted transaction (configuration, seed, restore,
  encryption migration, initialization) can be resumed, rolled back or
  abandoned without editing internal files.
- Scale: large index and tree listings, large snapshot metadata, long exclude
  patterns, per-commit process spawning in guards and doctor, and quadratic
  restore re-verification.
- Backups: archives are bound to their ref, cross-OS restore works, leftover
  private files no longer make backups unrestorable, HTTPS remotes
  authenticate, and remote host checks canonicalize addresses.
- Security: `golang.org/x/text` is updated to v0.39.0; earlier versions could
  loop forever normalizing an invalid path name (GO-2026-5970).
- Platforms and layouts: relative and bare worktree layouts, shared and
  tracked hook directories, macOS case-insensitive key paths, `F_FULLFSYNC`
  for directory syncs.

### Known limitations

- **Windows** has never run on native hardware or CI. Read-only targets,
  8.3 short names and key-file ACLs are implemented but unverified.
- **Restore** time grows with the square of the number of restored files:
  restoring a directory of a few hundred files takes minutes.
- **Maintenance** isn't automatic: run `git local maintenance` now and then.
- **Limits:** see [docs/limits.md](docs/limits.md); among them managed ignore
  rules up to 256 KiB, journals up to 16 MiB, `show`/`diff` output up to
  16 MiB, and `log` up to 1,000 entries per call.
- Mixed `core.ignorecase` across worktrees over-ignores (never leaks).
- The macOS installer isn't signed or notarized yet.
