# Limits

Fixed limits that a large repository or history can reach. Each fails with a
clear error rather than truncating or guessing.

| What | Limit | When it's reached |
| --- | --- | --- |
| Buffered Git output (blob contents, patches, config) | 16 MiB | "Git output exceeded safety limit". `show` and `diff` print at most 16 MiB; use `restore` for a larger file. Listings that grow with the repository or history (index, tree, ref and ancestry listings, staged and pushed path names) and file contents that restore writes aren't capped. |
| Managed ignore rules in `.git/info/exclude` | 256 KiB | `track`/`untrack` fail with "managed ignore rules exceed the size limit". A path untracked inside a tracked directory costs about L×L+21×L bytes for a name of length L, so roughly fifty sixty-character names can be untracked in one directory. |
| Snapshot metadata kept inline | 1 MiB | Larger changed-path lists and digest maps are stored in their own blobs (up to 256 MiB each); git-local 0.1.4 can't read such snapshots. |
| Transaction journals | 16 MiB | The transaction is refused. |
| `log` | 1,000 entries per call | `-n` above 1,000 is refused. |
| `grep` | 1–100 snapshots, 1–1,000 matches, 1 MiB per file, 64 MiB per search | Files and snapshots beyond the budget are reported as not searched. |
| Overlay files | UTF-8 text, at most 1 MiB | `overlay add` refuses. |
| Restore | grows with the square of the restored file count | A directory of a few hundred files takes minutes. |

## Platform and environment

- Git 2.50.1 or later. Inherited alternate-index, object-directory and
  configuration-injection contexts (`GIT_INDEX_FILE`, `GIT_CONFIG_*`,
  `git -c … local`) are refused.
- Overlays need a POSIX shell for Git's filter (Git for Windows has one), no
  `core.autocrlf` (`true` or `input`), and no other line-ending, ident or
  content filter on the file.
- `git archive` applies active overlays; see the
  [user guide](guide.md#overlays) for the command that exports without them.
- Windows hasn't been qualified on native hardware or CI. 8.3-style short
  names (`PROGRA~1`) can't be selected there.
- Network filesystems aren't qualified.
- The private store isn't packed or pruned automatically; run
  `git local maintenance` (`doctor` reports the store's size).
- Mixed `core.ignorecase` settings across worktrees can over-ignore paths
  (never leak them).

## Backups over HTTPS

Backup push and fetch use only these settings from your global and system Git
config: credential helpers, `credential.useHttpPath`,
`credential.<url>.username`, `http.sslCAInfo`, `http.sslCAPath` and
`http.proxy` (each also as `http.<url>.*`). Redirects aren't followed, and
neither Git nor ssh prompts. These settings reach Git as `-c` options, so a
credential helper written inline with a secret in it shows in the process list;
use a helper program instead.
