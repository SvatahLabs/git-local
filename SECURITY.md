# Security policy

## Report a vulnerability

Do not open a public issue for a security problem.

Report it privately on GitHub:
[Report a vulnerability](https://github.com/SvatahLabs/git-local/security/advisories/new).

Include:

- the git-local version (`git local --version`) and your operating system
- the steps to reproduce the problem
- what an attacker can read, change or delete

You get a first reply within 7 days.

## Supported versions

Only the latest release gets security fixes.

## Scope

In scope:

- selected files reaching a mainline commit or push past the guards
- private history, keys or backups readable by another user or remote
- restore or recovery writing outside the selected paths

Out of scope:

- Windows key-file protection, a known limitation (see the
  [changelog](CHANGELOG.md#known-limitations))
- problems that need an attacker who already controls your user account
