# Security Policy

## Supported Versions

`quai` is currently in `0.1.x` alpha development. Security fixes are applied to `main` and the latest release tag.

| Version | Supported |
|---|---|
| `main` / `0.1.x` | Yes |

## Reporting a Vulnerability

Please do not open a public GitHub issue for security vulnerabilities.

Instead, report vulnerabilities privately using GitHub Security Advisories on this repository:

- Go to `https://github.com/coding-pr0jects/quai/security/advisories/new`
- Include a clear description of the issue, reproduction steps, affected versions, and potential impact.

You can expect an initial acknowledgment within 72 hours and a coordinated fix before public disclosure.

## Security Model and Invariants

`quai` executes repository-configured verification steps and interacts with `git` and `gh`. Its design enforces six invariants:

1. **No shell interpolation from forge metadata.** Pull request titles, bodies, branch names, and check names are never passed to `sh -c`. All `git` and `gh` calls use explicit argument slices (`exec.CommandContext`).
2. **Validated step placeholders.** Template placeholders in `steps[].run` (`{{base}}`, `{{head}}`, `{{tree}}`) are substituted only with validated 40-character hexadecimal git SHAs.
3. **No forged or dirty receipts.** `quai` provides no flag to mark a step passed without running it, refuses to write receipts on dirty working trees, and aborts receipt writes if a step mutates tracked files, adds untracked non-ignored files, or moves `HEAD`.
4. **No privilege escalation on merge.** `quai land` never passes `--admin` and never passes `--delete-branch` to `gh pr merge`.
5. **No untrusted forks.** `quai land` refuses cross-repository pull requests before acquiring locks or touching the local checkout.
6. **Lease-protected rebase pushes.** Rebase pushes always use `--force-with-lease=<branch>:<expected_sha>` pinned to the commit SHA read before rebasing.

### Trust boundary for `.quai.toml`

Commands declared in `steps[].run` execute via `sh -c` in the repository root, matching `Makefile` or `package.json` scripts. Only run `quai verify` or `quai hooks pre-push` in repositories whose `.quai.toml` you trust.
