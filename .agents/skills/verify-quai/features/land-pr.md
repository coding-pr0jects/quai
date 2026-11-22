# Land pull requests

`quai land` serializes pull request merges under `<git-common-dir>/quai/locks/land.lock`, skips unnecessary rebases via `git merge-tree` when `main` moved on disjoint files, aborts cleanly on rebase conflicts, and refuses drafts, forks, or failing checks.

## Sub-features

- `land-dry-run`: prints the landing plan (`direct-merge`, `optimistic-merge`, `rebase-and-merge`, or `refuse`) without mutating git or GitHub.
- `land-optimistic`: merges at the original head SHA without rebasing when `main` moved on disjoint non-global files and `git merge-tree` is clean.
- `land-rebase-conflict`: exits `3`, runs `git rebase --abort`, leaves the working tree clean, and releases `land.lock`.
- `land-refusals`: refuses drafts, forks, dirty trees, wrong checked-out branch, and failing or skipped required checks with exit code `1`.

## How to get to it (user POV)

- Run `quai land <pr> [--dry-run] [--method squash|merge|rebase] [--json]` from the checked-out PR branch.

## Driving it with quai-verify

Preconditions: `quai-verify launch` and `quai-verify doctor` succeeded. Place an executable fake `gh` shim in `/tmp/quai-verify-$QUAI_VERIFY_RUN_ID/bin/gh` that handles `pr view`, `pr checks`, and `pr merge` against fixture JSON and logs every call.

- **Dry-run optimistic plan (`land-dry-run`).** Commit a disjoint change on `feat/wt`, push to `origin`, commit a disjoint change to `docs/guide.md` on `main`, push to `origin`, configure the fake `gh` shim with PR `#1` open and check `check` passing, and run `quai-verify run-wt land-dry -- quai land 1 --dry-run --json`. Observe exit code `0`, `"action": "optimistic-merge"`, and verify `git rev-parse HEAD` is unchanged and the fake `gh` shim received no `pr merge` call.
- **Optimistic merge execution (`land-optimistic`).** Run `quai-verify run-wt land-opt -- quai land 1 --json`. Observe exit code `0`, `"merged": true`, `"action": "optimistic-merge"`, and verify the fake `gh` log recorded `pr merge 1 --squash --match-head-commit <sha>` with neither `--admin` nor `--delete-branch`.
- **Draft refusal (`land-refusals`).** Set `"isDraft": true` in the fake `gh` fixture and run `quai-verify run-wt land-draft -- quai land 1 --json`. Observe exit code `1` and `"action": "refuse"`.

## Gotchas

- Never run `quai land` without a fake `gh` shim in the scratch directory unless you explicitly intend to inspect or merge a real GitHub PR.
- After any `quai land` invocation, verify `<git-common-dir>/quai/locks/land.lock` does not remain on disk.
