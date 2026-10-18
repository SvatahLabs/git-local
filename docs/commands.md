# Command reference

Run `git local <command>` from an initialized checkout, or put `git-local` on
`PATH` and invoke it directly. `git -C <checkout> local ...` and invocation
from a subdirectory are supported. `--help` is available at the root and on
each command. Path arguments are literal paths relative to the invocation
directory; glob expansion is not performed by Git Local. Use `--` before
restore paths, especially names beginning with `-`.
`git-local --version` identifies the executable build.
Every public command accepts `--json` before or after its subcommand (and before
any `--` path separator). JSON mode
prints one versioned object on stdout, including on usage, conflict, guard and
operation errors. It does not mix human diagnostics into the JSON stream.

| Command | Arguments and flags | Result |
| --- | --- | --- |
| `init` | optional `--from main\|<workspace-uuid>` and `--paths <paths...>` | Initialize or resume; with `--from`, seed from the source's latest saved selection and files. Without `--from`, also repairs the shared overlay filter installation (absolute or missing include, missing filter config or direct required key) without a new overlay version; JSON `overlayMigration` lists the changes. It also adds a missing managed ignore block or rewrites an outdated one (JSON `ignoreBlockRewritten`). |
| `track` | `<paths...>`; optional `--from latest\|<hex-snapshot-id>` and `--workspace <uuid>` (requires `--from`) | Select local paths or reactivate historical path types. |
| `untrack` | `<paths...>` | Stop selecting paths while preserving files and saved history. |
| `commit` | `-m, --message <message>`; optional `--if-changed`, `--writer <id>`, `-- <paths...>` | Snapshot all selected paths or only explicit selected paths; `--writer` attributes changed files to a caller-supplied ID. |
| `auto install` / `auto uninstall` / `auto status` | install: optional `--allow-shared-hooks-dir` | Manage post-commit, post-checkout, pre-rebase and post-rewrite snapshot hooks while preserving existing shell hooks; post-checkout seeds new linked worktrees from the primary checkout. `auto status --json` has the same fields as `hook status` except `previousDirectories`. |
| `auto watch` | optional `--interval <1s..24h>` (default 5m) | Run a foreground timer for the current worktree; capture immediately and again when selected files change; stop with Ctrl-C, SIGTERM or SIGHUP (a second signal ends it at once). A conflicting capture is skipped with a warning and retried at the next check; five conflicts in a row exit 3. In JSON mode, return one summary when stopped, with skipped captures as warnings. |
| `overlay add` | `<name> -- <tracked-file>` | Save a versioned local patch and install the required Git clean/smudge filter for that path. Refused while the effective `core.autocrlf` (system and global configuration included) is `true` or `input`, or a `text`, `eol`, `ident` or filter attribute (system attributes included) applies. Works from a working directory reached through a symlink. |
| `overlay status` | none | Report active patch applicability, working-file presence, and conflicts with the branch upstream. An overlay whose hunks the upstream version already carries is reported as stale, with the remove, edit and add steps; JSON adds `adoptedHunks` and `hunks`. |
| `overlay show` | `<name> [--version N]` | Print saved patch content without changing the checkout; 0 selects the latest version. |
| `overlay remove` | `<name>`; optional `--workspace <uuid>` | Deactivate a patch, preserving the working file and all saved versions; restore owned Git attributes and config after the last active overlay. The file's times are renewed so Git reads it again and shows its local content as a change. `--workspace` removes an overlay of a retained workspace (a removed linked worktree) from any checkout; an active workspace's overlay is removed only from its own worktree. |
| `status` | no paths; optional paired `--porcelain -z` (`-z` is `--null`) | Compare selected files and rules with the latest snapshot. |
| `diff` | no arguments, or `<older-snapshot> <newer-snapshot>` with optional `--workspace <uuid>` | Show current selected-file changes, or compare two reachable saved snapshots without touching the worktree. |
| `log` | no paths; `-n, --max-count <1..1000>` (default 20); optional `--workspace <uuid>`, `--branch <name>`, `--main-commit <full-id>`, `--stat`, `--writer <id>`, `--path <repository-relative-file>`, `--provenance` | List snapshots; filter by mainline context, attributed writer or exact changed file, and optionally show provenance. |
| `show` | `<snapshot>:<repository-relative-path>`; optional `--workspace <uuid>` | Print raw bytes of one recorded historical file. |
| `grep` | `<literal-pattern>`; optional `--workspace <uuid>`, `-n, --snapshots <1..100>` (default 20), `--max-matches <1..1000>` (default 100) | Search text across reachable snapshots, reporting skipped binary/oversized files and truncation. |
| `restore` | `--source latest\|<hex-snapshot-id>` and `-- <paths...>` required; optional `--workspace <uuid>`, `--dry-run`, `--overwrite` | Preview or execute an explicit-path restore. |
| `reset --hard` | `--hard` required; optional `[revision]` (default `HEAD`) | Snapshot selected files, then run `git reset --hard` on the current worktree. Refused before the snapshot when the target commit contains a selected path or a staging file, a target file uses a third-party checkout filter, or an active overlay's filter fails its audit; a failed snapshot cancels the reset. JSON returns `revision`, `commit`, `snapshotId`, `snapshotChanged` and `reset`. |
| `workspace list` | none | Show registration and availability of workspaces. |
| `workspace retire` | `<uuid>` | Retain a removed workspace's history and reconcile shared ignores. |
| `workspace remove` | `<path>`; optional `--force` | Snapshot the selected files of an available linked worktree, ask Git to remove it (`--force` lets Git remove a dirty one), and retire its workspace. If the snapshot is refused, the worktree is kept, even with `--force`. JSON returns `workspaceId`, `snapshotChanged`, `removed`, `retired` and `transactionId`. |
| `workspace prune` | optional `--dry-run` | Retire every active workspace that is both unregistered and unavailable; keep history. |
| `guard staged` | none | Reject staged additions and changes to selected paths; allow deletions. |
| `guard push` | `<remote>`; pre-push ref updates on stdin | Reject outgoing commits that add or change selected paths. |
| `hook install` / `hook uninstall` | optional `--pre-commit`, `--pre-push`; default both; install also `--allow-shared-hooks-dir` | Add or remove managed guard blocks in Git's effective hook directory. Install refuses a directory inside a worktree, and one outside the repository unless `--allow-shared-hooks-dir` is given. In a shared directory, blocks another repository installed for the same binary are left as they are, with a warning; a different binary's block is refused. A hook file that no longer matches its record (moved away, block removed, interrupted install) is reinstalled with a fresh record; an unrecorded block in the repository's own hooks directory is adopted. Uninstall drops records whose directory, file or block is gone (with a warning), removes unrecorded blocks from the repository's own hooks directory, and warns when it removes blocks from a shared directory. Warnings go to stderr, or `warnings` in JSON. |
| `hook status` | none | Show guard state in the current and previous hook directories, and whether the current directory is inside a worktree, shared, or holds tracked hook files. A guard counts as installed when its hook file holds a managed block, whoever wrote it; in a shared directory, a block this repository has no record of is reported as `installed by another repository` (JSON `installedByOtherRepository`). JSON also returns `currentDirectory`, `installed` (whether each hook holds a managed block), `disabled` (installed hooks without execute permission, which Git skips; not reported on Windows), `previousDirectories` (recorded hook files in earlier hook directories), `location` (`repository`, `worktree` or `shared`), `worktree` (when `location` is `worktree`, the worktree that holds the directory) and `tracked` (hook files the worktree tracks). |
| `encrypt keygen` | `<absolute-path>` outside every checkout and the common Git directory | Create a new owner-private 32-byte sensitive-file key; never overwrite an existing file. |
| `encrypt enable` | `--key-file <absolute-path> -- <selected-paths...>` | Turn on sensitive-file encryption for the given selected paths while the private store has no objects or refs; refused once history exists (use `encrypt migrate`). JSON returns `keyFile`, `keyFingerprint` and `sensitive`. |
| `encrypt migrate` | `--key-file <absolute-path> -- <selected-paths...>` | Encrypt existing private history for the first time, or correct the policy in service with the same key file (the new rules must keep every existing rule). Rewrites every active and retained workspace's snapshots in a separate encrypted store, then publishes it with the policy; snapshot IDs change. JSON returns `transactionId`. |
| `encrypt recover` | `<transaction-id>` and exactly one of `--resume` or `--rollback` | Finish or undo an interrupted encryption migration; `--rollback` works only before the new policy is published. |
| `encrypt rebind-key` | `<absolute-path>` outside every checkout | Point the policy at its key in a new location; refused unless the file matches the stored key fingerprint. |
| `backup keygen` | `<absolute-path>` outside the checkout | Create a new owner-private 32-byte backup key; never overwrite an existing file. |
| `backup push` | `<remote> --key-file <absolute-path>` | Push a unique encrypted private-history archive ref to a named SSH or HTTPS remote on a different host from `origin`. |
| `backup restore` | `<remote> <refs/gitlocal/backups/...> --key-file <absolute-path>`; optional `--file-key <absolute-path>` | Fetch and authenticate one archive, then adopt it into an uninitialized main checkout. For a backup with sensitive-file encryption, `--file-key` names the matching key when its recorded path isn't valid here. |
| `backup recover` | none | Resume publication after an interrupted `backup restore` in the same checkout. |
| `recover --list` | none | List every pending journal (configuration, seed, restore, encryption migration and initialization), even while one of them blocks other commands, with its kind, state and next command. JSON adds a `transactions` array of `{id, kind, workspaceId, root, state, next}` beside `transactionIds`. |
| `recover` | `<transaction-id>` and exactly one of `--resume`, `--rollback` or `--abandon` | Explicitly complete, undo or give up an interrupted transaction. `--abandon` also accepts encryption migration IDs. It also accepts the ID of a restore recovery ref that has no journal (a restore killed before writing one), and deletes the ref; doctor lists such refs. A ref whose restore journal an encryption abandon quarantined keeps that restore's previous file versions, so deleting it also needs `--discard-recovery`. For a seed, pending or completed, whose file list beside its journal is missing or damaged, `--abandon` moves the journal into the quarantine without the list. |
| `doctor` | no paths | Read-only health and recovery diagnosis. Also reports recovery refs with no journal, stale overlays, `core.autocrlf` set under active overlays, a missing managed ignore block, a seed whose file list is missing or damaged, and seed file lists no journal names. |
| `maintenance` | none | Pack the private history store and delete objects no workspace ref, retained workspace or restore recovery ref reaches, under the exclusive repository lock, then checks the store (`fsck`) under the shared lock, so guards and the overlay filter don't wait for the check; refused while any transaction is pending. Reports loose and packed object counts and sizes before and after (JSON `before`, `after`). Never automatic. |
| `deinit` | optional `--remove-history` | Remove git-local from the repository and every worktree: overlays (files keep their local content), guard and snapshot hooks, the required filter and include, the managed `info/attributes` and `info/exclude` blocks, and worktree identity files. Refused, before anything changes, while a transaction is pending, an overlay's working file holds changes the overlay doesn't (or the overlay no longer applies), an overlay belongs to a missing or unavailable worktree, the managed `info/exclude` block was edited, the current worktree was never initialized (it names the worktrees to run it from, by their recorded roots, leaving out those whose identity belongs to an earlier private state), the current worktree's identity file can't be read (it names the content to restore and the worktrees to run it from instead), or a hook record is damaged. Also removes the overlay filter an interrupted `overlay add` left (refused while the managed `info/attributes` block holds a line git-local didn't write). Without the overlay installation record, or with one it can't read, it removes the filter as git-local's and warns. Refused from a worktree whose identity belongs to an earlier private state than the live one (delete that identity file). The private state is set aside under the exclusive lock after a final check; if another command installed something meanwhile, deinit refuses and a rerun removes it. Warns about sensitive paths that are no longer ignored. Keeps the private state as `<common-git-dir>/gitlocal-removed-<id>`; `--remove-history` deletes it, refused while a quarantine, a `refs/gitlocal/recovery/` ref or a retired workspace's history exists. Each step can be rerun; running it again finishes an interrupted deinit, names state an earlier run set aside, removes a private state directory holding only its lock, or nothing, and every identity file that doesn't belong to the live state. |
| `completion` | `bash`, `zsh`, `fish`, or `powershell`; optional `--no-descriptions` | Print the corresponding shell completion script; `--no-descriptions` leaves out the description shown beside each completion. |

`log --branch` accepts a short branch name or a `refs/heads/` ref. It matches
the name recorded when the local snapshot was created, including after a rename
or deletion. `log --main-commit` requires a full 40- or 64-character hexadecimal
mainline commit ID and matches the exact recorded HEAD, including snapshots
made on different branches at that commit. Filters can be combined and `-n`
limits matching entries after filtering. Changing branches without changing
selected content does not create a new local snapshot.

`commit -- <paths...>` resolves literal paths relative to the invocation
directory. Each path must be within the current selection; a directory path
includes its selected descendants. The snapshot retains previously saved bytes
outside the requested paths, even when those working files have newer unsaved
edits. Deletions within the scope are recorded. Adding a new selection can be
recorded without capturing its out-of-scope file bytes; saved files that are
no longer selected (untracked or excluded since) are dropped from the new
snapshot, as a full snapshot would drop them, and earlier snapshots keep them. `--writer` is a caller-supplied
session/process label, not a filesystem authorship attestation; omit it when
the writer is unknown. New snapshots store the exact paths whose recorded
bytes or modes changed. `log --writer` filters by that label; `log --path`
filters by one exact repository-relative changed path; `--provenance` prints
the label and changed paths. Older snapshots have no writer/path attribution.

Historical `show`, two-selector `diff`, `log --stat`, and `grep` read the
private store without restoring files. Selectors are `latest` or hexadecimal
snapshot IDs/prefixes and must be reachable from the chosen workspace's user
head; Git revision expressions and arbitrary refs are rejected. `show` paths
are exact repository-relative paths. A single `show` or diff output is limited
to 16 MiB. Grep matches literal UTF-8 text, scans at most 100 snapshots and
64 MiB of file content, and skips files larger than 1 MiB or with binary bytes.
It reports skipped files and when its match/output limit truncates results.

`latest` resolves within the selected source workspace, not globally. Snapshot
IDs can be hexadecimal prefixes of 4–64 characters; full IDs are reported by
`commit` and `log`. A source workspace
defaults to the current workspace; `--workspace` selects a known active or
retained source in the same common store. `track --from` changes selection
only; use `restore` to copy saved bytes. `restore` never infers an all-files
scope: it acts only within the explicitly requested paths. `--overwrite`
allows existing dirty recorded destinations; missing recorded files can be
recreated without it. Ownership, unselected paths, unknown
siblings, unsafe filesystem entries, and other safety conflicts still fail.
Successful restore creates a recovery snapshot and keeps the user snapshot head
unchanged, so restored differences may remain visible in `status`.
`init --from main` resolves `main` to the primary checkout's initialized
workspace. `--paths` is only valid with `--from`; repeat it or follow its first
value with more positional paths. Seeding refuses an existing destination
selection or local snapshot and never overwrites destination files: each file
is published with a no-replace rename, so one created after the existence
check is kept and the seed stops with a conflict; rolling it back leaves that
file (and a seed-created directory holding it) in place. An empty selection
written by an earlier release (schema 1) counts as empty. A worktree of a bare
repository has no primary checkout, so `--from main` is refused there.
Workspace bootstrap uses the existing idempotent init journal. The copied selection,
shared ignore update, and created files share one seed recovery journal, so an
interruption is resumed or rolled back with `recover` from that destination.
With `auto install`, a new linked worktree's first post-checkout event snapshots
the primary checkout before invoking this seed operation. A worktree created
with `--no-checkout` needs a later checkout event or manual `init --from main`.
`workspace remove <path>` snapshots selected files before asking Git to remove
an available linked worktree, then retires its record. Its warnings include the
snapshot's; if the snapshot is refused, the worktree is kept, even with
`--force`. A direct
`git worktree remove` has no hook; the next installed post-event hook retires
the missing workspace, or `workspace prune` can be run immediately.
Windows seeding rejects executable and symlink source entries before writing
the seed journal, matching the platform's restore limits.

Encrypted backup requires an external key and a separately hosted named Git
remote. `backup push` prints the exact ref required by `backup restore`.
Restore preserves existing user ignore bytes, refuses any selected path already
owned by the main index or HEAD, and never overwrites an existing Git Local
store. Its journal is separate from `recover` transactions; use
`backup recover` after an interruption. The archive contains committed local
snapshots, not unsaved working files. Reinstall hooks in the new clone and use
the ordinary explicit-path `restore` command to materialize saved files.
`backup restore` and `backup recover` report `archivedOverlays` in JSON output
when saved active overlay registrations were deactivated during adoption.
Active overlays are included as saved versions in encrypted backups. Restore
deactivates their registrations because the source filter setup is machine-
local; inspect them with `overlay show`, then explicitly `overlay add` after
editing the tracked file in the new clone.

## Machine output and exit status

`--json` uses this envelope for every public command:

```json
{"schemaVersion":1,"command":"git local commit","ok":true,"data":{"snapshotId":"...","changed":true,"writer":"agent-a","scope":["notes/a"]}}
```

An error sets `ok:false` and adds `error:{"code":"usage|conflict|guard_violation|canceled|operation","message":"..."}`. If an operation produced a partial transaction result, `data` accompanies the error. Optional `warnings` is an array of strings. Exit status remains `0` for success, `1` for operation failure, a guard violation (`guard_violation`) or cancellation, `2` for usage, and `3` for conflicts, including a guard that can't check the repository (`conflict`). `doctor --json` and `restore --dry-run --json` place their existing versioned report or plan inside `data`. `restore --json` also works for execution and returns transaction/recovery IDs.

Result fields use lower camel case. `init` returns workspace/repository IDs and
the history-store path; `track`/`untrack` return workspace and transaction IDs,
`changed`, and requested paths; `commit` returns `snapshotId`, `changed`,
`writer`, and `scope`. `status` returns `files` and `selection` records; `diff`
returns `patch` and, for a worktree diff, those records too. `show` and
`overlay show` return `contentBase64`, `encoding:"base64"`, and `size` so
arbitrary file bytes remain valid JSON. `grep` returns typed matches and skip
counts; `log` returns entries with snapshot IDs, timestamps, messages, writers
and changed paths. `log --stat` adds typed file/line/binary counts and per-path
entries. Workspace, hook, automatic-snapshot, overlay and guard
commands return lists, states or findings rather than formatted lines. Binary
Git filter protocol commands remain hidden and do not accept JSON mode.

`status --porcelain -z` requires both flags. It emits no header and one
NUL-terminated record per change, with no output for an unchanged workspace.
Records are `F<TAB>code<TAB>path` for files and
`S<TAB>code<TAB>kind<TAB>path` for selections. Warnings and errors go to stderr.

`restore --dry-run --json` includes a deterministic JSON plan in `data` with
`schemaVersion: 1`, source/destination identities, explicit requests, planned
directories and operations. It is a preview, never an execution token; a real
restore replans and revalidates. `doctor --json` emits a diagnostic report
with `schemaVersion: 1` and a `findings` array.
Human `status`, `diff`, `log`, `restore`, and `doctor` output is for reading, not
for parsing as a stable wire format.

Exit `0` means success, including a no-change result or warnings without
blocking findings. Exit `1` means operation failure, a guard violation
(`guard staged` or `guard push` found a selected path; JSON
`error.code: "guard_violation"`), or cancellation (Ctrl-C, SIGTERM or SIGHUP;
a second signal ends the process without waiting). Exit `2`
means invalid command/flags/arguments, including an unknown subcommand of a
command group (such as `hook instal`), extra arguments, and a malformed
transaction or workspace UUID; JSON reports `error.code: "usage"`. Exit `3`
means conflict or explicit recovery is required. A guard that can't check the
repository (for example damaged git-local state, or a `GIT_DIR` that names
another repository) also exits `3`, with `error.code: "conflict"`. Human
`status` and `diff` can display conflict records before returning 3; human `doctor` prints its report before returning 3 for
error findings.
An incomplete transaction blocks mutations until `recover` is explicitly run.
`doctor` reports every pending journal, including encryption migration and
initialization journals, with the next command and the reason.
`recover <id> --abandon` is the last resort when neither direction can finish.
It leaves working files and selections unchanged; for a configuration journal
it replaces the managed `info/exclude` block (whichever block the transaction
left) with the block for the live selections, keeping user bytes outside it,
and warns instead if the block matches none the transaction wrote. It removes reserved
staging files whose bytes match an image that local history holds, and copies
any other reserved bytes into `.git/gitlocal/quarantine/<id>/` (with
`manifest.json`) before removing them. For an encryption migration it renames
the prepared, replaced and record stores into the quarantine; before policy
publication it requires the original store to be in service, and after it, it
first applies the prepared completed-journal rewrites (refusing if they no
longer match) so restore and seed journals describe the encrypted history. The journal is
archived under `transactions/completed/` with `outcome: "abandoned"`; a restore
keeps its recovery ref. Quarantines are kept until the user deletes them and
are excluded from backups. JSON output: `transactionId`, `kind`, `abandoned`,
`quarantine` (absolute path or empty), `quarantined` (`file`, `source`, `root`,
`kind`, `reason`), `removed`, and `excludeRewritten` (whether abandon rewrote the
managed `info/exclude` block).
An initialization journal is named by the workspace ID it was creating. It can
be abandoned only when that checkout's Git administrative directory no longer
exists and no administrative directory of the repository (main or
`worktrees/*`) holds that workspace identity; a moved checkout resumes with
`init` instead, and doctor names that. Abandon removes the registration (and, for a repository that was never
published, the empty store skeleton) only when it matches the journal, and
archives the journal as `transactions/completed/init-<workspace-id>.json`.
`init` validates other registered worktrees before writing its journal.
`doctor --json` includes `code: "workspace-missing"` and the affected
`workspaceId` for an unregistered worktree. Scripts can use those fields to
identify candidates for `workspace retire`; retirement itself remains explicit.
`workspace prune --dry-run` previews the same candidates without changing state;
`workspace prune` revalidates and retires each candidate through the ordinary
journaled retirement operation.

Selections from all workspaces in one repository contribute to the shared
`<git-common-dir>/info/exclude`. Selecting a path in one worktree can hide it
from ordinary Git status in another worktree. `git add -f` can override an
ignore rule. `hook install` adds opt-in pre-commit and pre-push checks for
active and retained workspace selections on macOS and Linux. On Windows it
writes them for Git for Windows' POSIX shell, but running them under Git for
Windows hasn't been qualified natively. It uses Git's
effective `core.hooksPath`, creating the directory when needed.
The default hook directory is shared by linked worktrees. `hook install` and `auto install` refuse a hooks directory inside any
worktree (for example `core.hooksPath=.githooks` or husky's `.husky`), where the
scripts can be committed and would carry this machine's binary path, and leave
it unchanged. They also refuse a directory outside the repository, such as a
global `core.hooksPath` shared by other repositories, unless
`--allow-shared-hooks-dir` is given, and refuse to modify hook files that Git
tracks. With an existing hook manager, call
`git local guard staged` from its pre-commit hook and `git local guard push
"$1"` (passing the hook's standard input) from its pre-push hook instead.
`hook status` and `auto status` report a worktree or shared hooks directory
and any tracked hook files.
Existing shell hooks run after git-local's managed block, and uninstall
restores their original bytes. On a new branch, the pre-push guard skips commits
reachable from the remote's known remote-tracking branches and scans the rest
of the ancestry; without such refs it scans the full ancestry. `--no-verify` on both commit and push bypasses these
machine-local guards; they do not replace main ownership checks.

Git Local snapshots raw regular-file bytes and selected symlink targets; it
does not normalize text. Human `diff` summarizes Git-detected binary changes
without printing binary payloads, and falls back to a path summary for invalid
UTF-8 patches. Diagnostic errors do not include selected file contents.
Empty directories, arbitrary file ownership/ACLs, and unsupported filesystem
entry types are outside the snapshot contract. Buffered Git subprocess output
is bounded to 16 MiB: a larger blob read, patch or other query fails with "Git
output exceeded safety limit". Listings that grow with the repository
or its history (index, tree, ref and ancestry listings, staged and pushed path
names, batch object checks) are unbounded, and large regular
blobs stream. Log display
is limited to 1,000 entries per call. Restore streams large regular blobs, but
native platforms and large workloads aren't qualified yet.

## Worked restore examples

For a clean checkout, `git local init`, `git local track notes/a`, and
`git local commit -m "first"` save the initial bytes. Copy the printed snapshot
ID. After editing and saving a newer snapshot, preview with
`git local restore --source <first-id> --dry-run -- notes/a`, then run
`git local restore --source <first-id> -- notes/a`. `git local status` shows
the restored bytes as a change from the newer head until another snapshot is
committed.

To recover from a removed linked worktree, first run `git local workspace list`
to identify its UUID (or keep the UUID printed by its earlier `init`). After
`git worktree remove` unregisters it, run
`git local workspace retire <removed-uuid>` from a surviving checkout. Use
`git local log --workspace <removed-uuid>` to find a saved snapshot. In the
surviving checkout, run
`git local track --from <saved-id> --workspace <removed-uuid> notes/export`,
preview with
`git local restore --source <saved-id> --workspace <removed-uuid> --dry-run --json -- notes/export`,
then execute the same restore without `--dry-run`. This workflow depends
on the original common history store still existing; unsaved worktree edits
cannot be recovered from it.

Generate completion with `git local completion bash` (or `zsh`, `fish`,
`powershell`) and install the printed script using that shell's normal setup.
The bash script completes both `git-local` and `git local`: it defines
`_git_local` for Git's bash completion. The zsh script's `_git-local` serves the
`_git` completion bundled with zsh, and its `_git_local` serves Git's own
`git-completion.zsh` (installed as `_git` by some Git distributions), so both
forms complete with either. Load the scripts after Git's completion. With Git's
`git-completion.zsh`, source the zsh script from your `.zshrc` after `compinit`
rather than installing it in `fpath`: zsh loads an `fpath` file only at the
first `git-local <TAB>`, and until then `git local <TAB>` finds no `_git_local`.
