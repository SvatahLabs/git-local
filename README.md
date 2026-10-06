# Git Local

An external Git command for maintaining independent local history of selected
project files without adding that history to the project's commits or pushes.

**Status:** T01–T12 implemented. Initialization, selection, snapshots, inspection,
workspace lifecycle, historical selection, restore, explicit transaction recovery,
and read-only diagnostics are available. Native platform qualification and release
packaging remain T13–T14 work.

## Development

Requires Go 1.26 or later and Git 2.50.1 or later. Users of a compiled executable
need Git but no Go runtime. Initialization additionally checks required Git behavior
using disposable local fixtures; it performs no external network operations.

```sh
go test -race -parallel 4 -timeout 20m ./...
go vet ./...
go build -o bin/git-local ./cmd/git-local
./bin/git-local --help
./bin/git-local --version
```

On macOS, [build a universal local installer](project/specifications/proposal/MAC_INSTALLER.md)
with `scripts/build-macos-installer.sh`. It installs the CLI at
`/usr/local/bin/git-local` and leaves all repository history untouched.
For standalone macOS, Linux and Windows binaries, see the
[binary distribution guide](project/specifications/proposal/RELEASE_BINARIES.md).

Place the built `git-local` executable on `PATH`, then run inside a checkout:

```sh
git local init
git local track notes
git local overlay add debug-port -- config/app.conf
git local overlay status
git local overlay show debug-port
git local overlay remove debug-port
git local untrack notes/private.txt
git local commit -m "Save local notes"
git local commit -m "Save my notes" --writer session-42 -- notes/my-file
git local commit --if-changed
git local status
git local diff
git local diff <older-snapshot-id> <newer-snapshot-id>
git local log
git local log --stat
git local log --branch main
git local log --main-commit <full-mainline-commit-id>
git local log --writer session-42 --path notes/my-file --provenance
git local show <snapshot-id>:notes/a
git local grep 'search text'
git local workspace list
git local workspace remove /path/to/linked-worktree
git local workspace prune --dry-run
git local workspace prune
git local hook install
git local auto install
git local auto status
git local reset --hard HEAD~1
git local restore --source latest --dry-run -- notes
git local restore --source <snapshot-id> -- notes
git local recover --list
git local recover <transaction-id> --abandon
git local doctor
git local doctor --json
git local workspace list --json
git local show latest:notes/my-file --json
git local commit -m "Agent snapshot" --writer session-42 --json -- notes/my-file
git local backup keygen /secure/location/gitlocal.key
git local backup push backup --key-file /secure/location/gitlocal.key
git local encrypt keygen /secure/location/gitlocal-files.key
git local encrypt enable --key-file /secure/location/gitlocal-files.key -- notes/private.txt
git local encrypt migrate --key-file /secure/location/gitlocal-files.key -- notes/private.txt
git local encrypt rebind-key /secure/new-location/gitlocal-files.key
git local completion bash
```

An interrupted transaction blocks git-local commands and the commit and push
guards until it is resolved. `git local doctor` lists every pending journal
(configuration, seed, restore, encryption migration and initialization) with the
command that resolves it. Use `recover <id> --resume` or `--rollback` first (for
an encryption migration, `encrypt recover`). If neither can finish, for example
because a file was edited after the interruption,
`git local recover <id> --abandon` gives the transaction up: working files and
selections stay as they are, a configuration transaction's managed ignore block
is replaced by the one for the current selections (lines outside it are kept),
staging files whose content local
history already holds are removed, and any other journal-reserved bytes are
moved to `.git/gitlocal/quarantine/<id>/` with a `manifest.json` describing
them. An abandoned encryption migration moves its prepared and replaced stores
there instead of deleting them; before the policy is published, the original
store must still be in service, and after it, completed restore and seed
journals are first updated for the encrypted history. The quarantine is never deleted automatically
and isn't included in backups; delete it once you've checked its contents. The
abandoned journal is archived with outcome `abandoned`, and a restore keeps its
recovery ref. Run `git local doctor` afterwards to check the result.

Sensitive-file encryption can be enabled before the first private snapshot.
First `track` the path, create a separate 32-byte key outside every checkout,
then run `encrypt enable` with that selected path. Covered snapshot contents are
encrypted in the private store; `show`, `diff`, restore and linked-worktree seed
require the key. `encrypt enable` refuses any existing private objects or refs,
including unreachable objects. If private snapshots already exist, use
`encrypt migrate` with the same selected paths. Migration rewrites all active
and retained workspace snapshot histories in a separate encrypted store, then
publishes the new store and policy under the common lock. It also converts
completed seed journals and restore recovery anchors, including sensitive
preimages. Unknown private refs and invalid recovery records stop migration
before publication. If the swap is
interrupted, run
`git local encrypt recover <transaction-id> --resume` or `--rollback`;
rollback is available only before policy publication. Keep the external key
safe and available to every worktree. Migration rewrites snapshot commits, so
previous snapshot IDs change; use `git local log` to find their replacements.

A sensitive path may be a selected file or directory, or any file or directory
inside a selected directory. `enable` reads its kind from the checkout, so a
sensitive subdirectory covers everything below it. `migrate` reads it from the
saved snapshots first: a path with saved files below it is a directory even if
it was since deleted or replaced, and a saved path may be missing from the
checkout. Otherwise the path must exist with its exact on-disk spelling: a
missing path, or one that differs only in case or Unicode normalization, is
refused. Migration fails if a file would match a sensitive path only by
spelling or kind. `doctor` reports a policy from an earlier release that
recorded a directory as a file or used a different spelling; files it should
have covered were saved unencrypted. **Changing an existing policy isn't
supported yet.** Until it is, a snapshot skips each file that matches a
sensitive path only by spelling or kind and prints a warning naming it: no new
plaintext is written, the skipped file keeps its last saved version (or stays
absent if it was never saved), and every other file is saved as usual. Changes
to skipped files go unsaved until the policy is corrected. Untracking a skipped
file, or renaming it to the sensitive path's exact spelling, drops its old
entry; the renamed file is then saved encrypted. Restoring over unsaved changes
to a skipped file is refused, because the recovery copy restore keeps would be
plaintext; move the file aside first.
If the key is moved, `encrypt rebind-key` accepts its new absolute path only
when the replacement bytes match the stored key fingerprint.

For encrypted remote backup, add a named SSH or HTTPS Git remote on a host
different from `origin`, generate and preserve a key outside the checkout, and
run `backup push`. The printed `refs/gitlocal/backups/...` ref identifies that
backup. In a fresh mainline clone with the same named backup remote and key:

```sh
git local backup restore backup "$BACKUP_REF" --key-file /secure/location/gitlocal.key
git local hook install
git local restore --source latest --dry-run -- notes
git local restore --source latest -- notes
```

If that backup contains sensitive-file encryption, also keep its separate
file key and pass `--file-key /secure/location/gitlocal-files.key` when its
path differs in the new clone. Adoption checks the key fingerprint, encrypted
snapshots, and completed recovery records before publishing private state.
If the original file-key path is still valid, `--file-key` may be omitted.

If publication is interrupted, run `git local backup recover` in that clone.
The encrypted archive contains committed private snapshots, not unsaved
working files. Restore refuses a checkout with existing Git Local state or a
mainline-owned selected path. Active tracked-file overlays require their
source clone's filter setup, so backup restore preserves their saved versions
but deactivates them in the new clone. Inspect a saved version with
`git local overlay show <name>`; after editing the tracked file to the desired
local content, run `git local overlay add <name> -- <file>` before staging it. See the
[backup contract](project/specifications/proposal/BACKUP_STATUS.md).

On Windows, build as `bin/git-local.exe`. `git -C <checkout> local init` and
invocation from a subdirectory are supported. Initialization is idempotent; rerun
it from the original checkout to resume an interrupted initialization. If that
checkout was removed, `git local recover <workspace-id> --abandon` (doctor shows
the ID) discards its journal from any other checkout, along with the
registration it had published. Abandon is refused while a checkout still holds
that workspace identity, for example after the repository directory moved; run
`git local init` in the moved checkout instead, which finishes the
initialization where it is now. Init checks every other registered worktree
before it writes its journal, so a failed check leaves nothing behind. Unknown
or conflicting state is preserved and reported instead of overwritten.
With `auto install` in the primary checkout, Git's post-checkout hook snapshots
its latest selected files and seeds each new linked worktree with that selection
and file contents. The primary checkout must already be initialized. If hooks
are unavailable, or for a worktree created with `--no-checkout`, run
`init --from main` in the new worktree after a local snapshot exists. Use a
workspace UUID for another source, or `--paths` to copy a subset. Workspace
bootstrap uses its existing
initialization journal; selection, ignore rules, and copied files use one seed
transaction, recoverable with `git local recover <id> --resume|--rollback`.

```sh
git -C <new-worktree> local init --from main
# Or, in another new worktree:
git -C <another-new-worktree> local init --from <workspace-uuid> --paths notes/a notes/b
```

State lives in `<git-common-dir>/gitlocal/`, with a workspace identity file in
each checkout's administrative directory. Linked worktrees have independent
identities and retain common-store records after removal. Initialization leaves
main-repository content, index, refs, objects and exclusions unchanged.
Selections from all worktrees contribute to the shared `info/exclude`, so a path
selected in one worktree may also be ignored by ordinary Git in another.
The managed block also ignores `.gitlocal-tmp-*` at any depth: restore and
linked-worktree seed stage each file under that name beside its target, and an
interrupted operation keeps partial stages, which may hold decrypted content.
The block, with that rule, stays after every selection is untracked.
`doctor` lists leftover stages, and warns when a block written by an earlier
release lacks that rule; the next `track` or `untrack` adds it.
The generated rules follow the checkout's `core.ignorecase` (true by default on
macOS and Windows), so a path you `untrack` inside a tracked directory stays
visible to Git whatever its letter case. Earlier releases ignored such paths
when `core.ignorecase` was true, which hid them from `git status` and let
`git clean -X` delete them. Doctor reports an excluded path that the managed
rules still ignore as an error, and warns about a block generated for the
other setting; the next `track` or `untrack` rewrites it.
`git add -f` can bypass that ignore rule. Run `git local hook install` to add
pre-commit and pre-push guards on macOS, Linux or Git for Windows. Native Windows
hook execution still awaits CI qualification. The guards reject additions or
changes to paths selected by any active or retired workspace, and to
`.gitlocal-tmp-*` staging files, and detect saved
overlay patches that entered the index or a pushed commit without the filter,
including a single hunk of a multi-hunk patch staged on its own;
staged deletions remain allowed. Paths that can never be selected, such as
`aux.c`, `nul.txt`, names with a backslash or a trailing dot or space, or
non-UTF-8 names, pass the guards unless the spelling Windows would resolve them
to (backslash as separator, trailing dots and spaces dropped) aliases a
selected path. Existing shell hooks keep their contents and run after the
managed guard block. Use `git local hook status` to inspect protection and
`git local hook uninstall` to remove only git-local's blocks. Either command
accepts `--pre-commit` or `--pre-push` to select one guard. Installation uses
Git's effective `core.hooksPath` directory. In a repository with no git-local
state (no `<git-common-dir>/gitlocal` directory), `guard staged` and `guard push`
succeed without output, so a hook directory shared with such repositories
doesn't block their commits or pushes; partial or damaged state still fails
closed.
Git's default hook directory is shared by linked worktrees, so one
installation covers them. `hook install` and `auto install` refuse a hooks directory inside any
worktree (for example `core.hooksPath=.githooks` or husky's `.husky`), where the
scripts can be committed and would carry this machine's binary path, and leave
it unchanged. They also refuse a directory outside the repository, such as a
global `core.hooksPath` shared by other repositories, unless
`--allow-shared-hooks-dir` is given, and refuse to modify hook files that Git
tracks wherever the directory is. With an existing hook manager, call
`git local guard staged` from its pre-commit hook and `git local guard push
"$1"` (passing the hook's standard input) from its pre-push hook instead.
`hook status` and `auto status` report a worktree or shared hooks directory
and any tracked hook files.
On a new branch, the pre-push guard excludes commits reachable from known
remote-tracking branches; without those refs it scans the branch's full ancestry.
Hooks protect against accidental leaks;
`--no-verify` on both commit and push bypasses them. A fresh clone needs its
own hook installation. Server-side protection cannot see local selections.
`workspace prune --dry-run` previews workspaces that are both unregistered and
unavailable; `workspace prune` retires those records while preserving history.
For session-end automation, `git local commit --if-changed` uses an automatic
message when none is supplied and stays quiet when there is nothing to save.
`git local log --branch <name>` filters by the branch recorded when each local
snapshot was made, even if that branch is later renamed or deleted.
`log --main-commit <full-id>` finds snapshots made while that exact mainline
commit was checked out. Both filters can be combined; `-n` limits matching
snapshots after filtering.
`show <snapshot>:<repository-relative-path>` prints recorded bytes without
touching the worktree. `diff <older> <newer>` compares two reachable local
snapshots; `diff` without selectors still compares the worktree with the latest
snapshot. `log --stat` reports recorded file changes. `grep <literal-pattern>`
searches the newest 20 snapshots by default; use `--snapshots` and
`--max-matches` for bounded searches. It reports binary and oversized files it
skips. These commands also accept `--workspace <uuid>` when reading retained
workspace history (`diff` requires two selectors with that flag).
Run `git local auto install` to add managed post-commit, post-checkout,
pre-rebase and post-rewrite hooks. They snapshot selected files only when their
content or selection changed. The hooks preserve existing shell hooks and seed
new worktrees from the primary checkout on their first post-checkout event.
On Windows, these commands use Git for Windows' POSIX shell and a quoted `.exe`
path; native Windows hook execution still awaits CI qualification.
When Git removes a worktree directly, the next installed post-event hook
retires eligible missing records while keeping their history. Ambiguous or
unidentified registrations require `git local workspace prune` after repair. Use
`git local workspace remove <path>` to save selected files, ask Git to remove
the linked worktree, and retire it immediately. If that snapshot skipped a file
(see the sensitive-file policy note above), removal is refused unless you pass
`--force`, because Git would delete the skipped file's unsaved changes. A
`--no-checkout` worktree is
seeded after its first later checkout event; `git local init --from main` is
available when no such event occurs. A failed pre-rebase snapshot
stops the rebase; a failed post-event snapshot prints a warning after the Git
operation. `git local auto uninstall` removes only git-local's event blocks.
For a session-end hook, call `git local commit --if-changed` from the checkout.
To capture changes between Git events, run `git local auto watch` in that
checkout. It saves an initial snapshot, then checks every five minutes by
default; `--interval 30s` changes the period. This is a foreground process for
one worktree. Stop it with Ctrl-C, or stop its service if you run it under a
process supervisor. It holds no permanent watcher state, and simultaneous
event hooks use the same snapshot lock. `--json` returns one summary object
when the watcher stops.
Direct `git reset` does not trigger these hooks. Use
`git local reset --hard [revision]` to save changed selected files before
resetting the main index and worktree. The command resolves the revision and
checks configured checkout filters before the snapshot. If capture fails, it
does not run the reset. It accepts only `--hard` and an optional revision;
ordinary `git reset` remains a direct Git operation without pre-reset capture.
The wrapper refuses third-party checkout filters used by target files because
a hard reset would execute them. When an overlay is active, its managed filter must pass the
installation audit before the reset proceeds.
With `--json`, the result reports the resolved commit, snapshot ID, whether
the snapshot changed, and whether Git completed the reset.

For a tracked text file, `overlay add` saves its complete working-file difference
from the current index as a named, versioned local patch. Stage ordinary edits
first if they should remain mainline. Git's local clean/smudge filter then
removes the patch when staging and reapplies it on checkout. Overlapping edits
fail with a conflict instead of silently choosing a version. Overlays require
Git with a POSIX shell for the filter command (Git for Windows supplies one),
a regular UTF-8 file of at most 1 MiB, and no existing line-ending, ident or
content filter on that path. Native Windows overlay behavior still awaits CI
qualification.

**`git archive` applies active overlays.** Git runs the smudge filter for
archives too, so a plain `git archive HEAD` puts your local patch in the
archive. Export without overlays by disabling the filter for that command:

```sh
git -c filter.gitlocal-overlay.process= -c filter.gitlocal-overlay.required=false archive HEAD -o release.tar
```

The commit and push guards don't cover archives. See the
[overlay implementation contract](project/specifications/proposal/OVERLAY_PLAN.md)
for remaining broader qualification. `overlay remove`
leaves the working file and saved versions intact; the former patch becomes an
ordinary edit that Git can stage.

The filter driver lives in `.git/gitlocal/overlay-filter.gitconfig`, included
from the local Git config through the relative path
`gitlocal/overlay-filter.gitconfig`, so it keeps working when the checkout is
moved or mounted at another path. `filter.gitlocal-overlay.required=true` is
also set directly in the local config: if the include file goes missing, `git
add` of an overlaid file fails instead of staging the patch. The last `overlay
remove` unsets both.

`git local doctor` verifies that active overlays still have their managed
attributes, relative include, required Git filter, and executable. If it reports
a damaged installation, restore those parts before staging or checking out
affected files; an `overlay add` update also refuses a changed active filter.
`git local init` repairs the shared filter installation without creating an
overlay version: it rewrites an absolute include written by an earlier release
(also after the checkout moved), recreates a missing include or filter
configuration, adds the direct `required` key, and checks every overlaid path
with `git check-attr`, undoing its changes if Git wouldn't apply the filter.
Until then an unmoved earlier install keeps working: doctor warns and points to
`git local init`, and `reset --hard` accepts it. An absolute include that no
longer resolves, or one naming another repository's filter configuration, is
refused.
After writing its attributes, `overlay add` asks Git (`git check-attr`)
whether every overlaid path now uses the filter, and rolls back if not, for
example when a later line in `.git/info/attributes` or a `.gitattributes` file
sets `-filter` for that path. Paths with spaces, non-ASCII or control
characters are quoted the way Git parses them; a filter executable whose path
contains a control character is refused.

If a pull fails because the upstream changed the overlay's lines, run
`git local overlay status` to see the conflicting upstream blob and whether Git
left the working file missing. `git local overlay show <name>` prints the saved
content (`--version N` selects an older version). To resolve, inspect any
unstaged work, run `git local overlay remove <name>`, restore the clean tracked
file with `git restore -- <file>`, complete the pull, edit the file to the local
value you now want, and run `git local overlay add <name> -- <file>`. The saved
versions remain available throughout; `git restore` discards unstaged edits in
that file, so preserve any unrelated edits first.

Exit codes: `0` success, `1` operation failure/cancellation, `2` invalid usage,
`3` conflict or recovery required. Alternate-index/object/configuration injection
contexts, including `git -c ... local`, are rejected.

See the [public command reference](project/specifications/proposal/COMMANDS.md)
for every flag, machine-readable output, binary behavior, limits, and complete
restore examples. `doctor` checks health without replaying or repairing anything;
see the [T12 diagnostic contract](project/specifications/proposal/DOCTOR_STATUS.md).

Native platform qualification is still pending. See the
[foundation status](project/specifications/proposal/FOUNDATION_STATUS.md) for
filesystem admission rules, durability limits and validation coverage.

The Go module path remains `gitlocal` until the published repository is chosen.
Dependencies are Cobra plus Go's platform syscall and Unicode libraries.

## Design

- [Proposal](project/specifications/proposal/README.md): product contract and scope.
- [Technical design](project/specifications/proposal/DESIGN.md): storage and algorithms.
- [Adversarial review](project/specifications/proposal/ADVERSARIAL_REVIEW.md): original
  Git behavior probes and design corrections.
- [Implementation plan](project/specifications/proposal/IMPLEMENTATION_PLAN.md): task
  dependencies and acceptance gates.
- [Feature roadmap](project/specifications/proposal/FEATURE_ROADMAP.md): requested
  overlays, automation, browsing, backup, writer scope and encryption gates.
- [Foundation status](project/specifications/proposal/FOUNDATION_STATUS.md): implemented
  T01–T04 behavior, test coverage and remaining platform qualification.

See the [T05 selection contract](project/specifications/proposal/SELECTION_STATUS.md)
for exclusion precedence, re-tracking, schema compatibility and ownership checks.

See the [T06 configuration contract](project/specifications/proposal/CONFIGURATION_STATUS.md)
for shared ignore rules, preservation of user rules, unavailable worktrees and recovery.

See the [T07 snapshot contract](project/specifications/proposal/SNAPSHOT_STATUS.md)
for raw capture, no-change detection, publication and isolation checks.

See the [T08 inspection contract](project/specifications/proposal/INSPECTION_STATUS.md)
for porcelain v1, temporary diff storage, conflict reporting and log scope.

See the [T09 lifecycle contract](project/specifications/proposal/LIFECYCLE_STATUS.md)
for retirement, retained-source logs and historical path selection.

See the [T10 restore planning contract](project/specifications/proposal/RESTORE_PLAN_STATUS.md)
for exact restore scope, overwrite rules and read-only guarantees.

See the [T11 restore and recovery contract](project/specifications/proposal/RESTORE_STATUS.md)
for journaled execution, interruption handling and platform limits.

See the [T12 diagnostic contract](project/specifications/proposal/DOCTOR_STATUS.md)
for read-only health checks and actionable recovery findings.
