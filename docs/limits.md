# Limits

Fixed limits that a large repository or history can reach. Each fails with a
clear error rather than truncating or guessing.

| What | Limit | When it's reached |
| --- | --- | --- |
| Buffered Git output (blob contents, patches, config) | 16 MiB | "Git output exceeded safety limit". `show` and `diff` print at most 16 MiB; use `restore` for a larger file. Listings that grow with the repository or history (index, tree, ref and ancestry listings, staged and pushed path names) and file contents that restore writes aren't capped. |
| Managed ignore rules in `.git/info/exclude` | 256 KiB | `track`/`untrack` fail with "managed ignore rules exceed the size limit". A path untracked inside a tracked directory costs about L×L+21×L bytes for a name of length L, so roughly fifty sixty-character names can be untracked in one directory. |
| Snapshot metadata kept inline | 1 MiB | Larger changed-path lists and digest maps are stored in their own blobs (up to 256 MiB each); git-local 0.1.4 can't read such snapshots. |
| Transaction journals | Restore: 16 MiB. Configuration and seed: 1 MiB. Encryption migration: 16 MiB. Each is written and read back with the same limit. | The transaction is refused before anything is written, naming the cause. |
| Other metadata files (workspace and overlay records, quarantine manifests, the sensitive-file policy) | 1 MiB, written and read back with the same limit | Writing is refused before anything changes, naming the file. |
| Seeding a worktree (`init --from` or automatic seeding) | A seed journal whose file list doesn't fit in 1 MiB keeps the list beside it (`transactions/seed-files/<id>.json`, up to 64 MiB, about 250,000 files) | Beyond that the seed is refused before anything is written; seed a subset with `init --from <source> --paths <path>...`. |
| `log` | 1,000 entries per call | `-n` above 1,000 is refused. |
| `grep` | 1–100 snapshots, 1–1,000 matches, 1 MiB per file, 64 MiB per search | Files and snapshots beyond the budget are reported as not searched. |
| Overlay files | UTF-8 text, at most 1 MiB | `overlay add` refuses. |
| Restore | time grows linearly with the restored file count, roughly half a second per file on a busy machine | A directory of a few thousand files takes tens of minutes. |

## Platform and environment

- Git 2.50.1 or later. Inherited alternate-index, object-directory and
  configuration-injection contexts (`GIT_INDEX_FILE`, `GIT_CONFIG_*`,
  `git -c … local`) are refused.
- Overlays need a POSIX shell for Git's filter (Git for Windows has one), no
  `core.autocrlf` (`true` or `input`), and no other line-ending, ident or
  content filter on the file.
- `git archive` applies active overlays; see the
  [user guide](guide.md#overlays) for the command that exports without them.
- Windows isn't qualified: it hasn't run on native hardware or CI. 8.3-style
  short names (`PROGRA~1`) can't be selected there. Its key-file protection has
  known defects, so sensitive-file and backup keys aren't supported on Windows
  until they are fixed.
- The Intel macOS build has only run under Rosetta on Apple silicon; the test
  suite hasn't run on an Intel Mac.
- Only APFS and a Linux container's overlay filesystem have been tested. Other
  local filesystems are untested, and network filesystems aren't qualified.
- A snapshot packs the private store once it holds 6,700 loose objects or
  256 MiB of them; `git local maintenance` repacks it fully (`doctor`
  reports the store's size).
- Mixed `core.ignorecase` settings across worktrees can over-ignore paths
  (never leak them); `git local doctor` warns, naming each worktree and its
  value.

## Backup remotes

Over HTTPS, backup push and fetch use only these settings from your global and
system Git config: credential helpers, `credential.useHttpPath` and
`credential.username` (each also as `credential.<url>.*`), and
`http.sslCAInfo`, `http.sslCAPath` and `http.proxy` (each also as
`http.<url>.*`). Repository config is ignored, and conditional includes
(`includeIf`) are evaluated outside the repository, so settings they include
only for it aren't used. Redirects aren't followed, and Git doesn't prompt.
Credential helpers reach Git as `-c` options, so a helper written inline with a
secret in it shows in the process list; use a helper program instead.

Over SSH, askpass programs are disabled (`SSH_ASKPASS_REQUIRE=never`), but ssh
can still ask on the terminal to accept an unknown host key or for a key's
passphrase. For unattended backups, use an ssh-agent and accept the host key
beforehand.
