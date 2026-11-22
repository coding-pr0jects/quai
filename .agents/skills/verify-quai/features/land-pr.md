# Land pull requests

`quai land` serializes pull request merges under `<git-common-dir>/quai/locks/land.lock`, skips unnecessary rebases via `git merge-tree` when `main` moved on disjoint files, aborts cleanly on rebase conflicts, and refuses drafts, forks, or failing checks.

## Sub-features

- `land-dry-run`: prints the landing plan (`direct-merge`, `optimistic-merge`, `rebase-and-merge`, or `refuse`) without mutating git or GitHub.
- `land-optimistic`: merges at the original head SHA without rebasing when `main` moved on disjoint non-global files and `git merge-tree` is clean.
- `land-rebase-conflict`: exits `3`, runs `git rebase --abort`, leaves the working tree clean, and releases `land.lock`.
- `land-refusals`: refuses drafts, forks, a dirty tree on the PR branch, a PR head out of sync with GitHub, and failing or skipped required checks with exit code `1`.
- `land-coordinator`: lands from a worktree on another branch while the PR branch is checked out in a second worktree; `rebase-and-merge` runs in `<git-common-dir>/quai/land-worktree-<pr>-<pid>`, which is gone afterwards.
- `land-rollback`: a failing step after the rebase reports `"failure": "post-rebase-verify-failed"`, exits `1`, pushes nothing, and leaves no land worktree.

## How to get to it (user POV)

- Run `quai land <pr> [--dry-run] [--method squash|merge|rebase] [--json]` from the checked-out PR branch or from any other worktree of the repository.

## Driving it with quai-verify

Preconditions: `quai-verify launch` and `quai-verify doctor` succeeded. Place an executable fake `gh` shim in `/tmp/quai-verify-$QUAI_VERIFY_RUN_ID/bin/gh` that handles `pr view`, `pr checks`, and `pr merge` against fixture JSON and logs every call.

- **Dry-run optimistic plan (`land-dry-run`).** Commit a disjoint change on `feat/wt`, push to `origin`, commit a disjoint change to `docs/guide.md` on `main`, push to `origin`, configure the fake `gh` shim with PR `#1` open and check `check` passing, and run `quai-verify run-wt land-dry -- quai land 1 --dry-run --json`. Observe exit code `0`, `"action": "optimistic-merge"`, and verify `git rev-parse HEAD` is unchanged and the fake `gh` shim received no `pr merge` call.
- **Optimistic merge execution (`land-optimistic`).** Run `quai-verify run-wt land-opt -- quai land 1 --json`. Observe exit code `0`, `"merged": true`, `"action": "optimistic-merge"`, and verify the fake `gh` log recorded `pr merge 1 --squash --match-head-commit <sha>` with neither `--admin` nor `--delete-branch`.
- **Draft refusal (`land-refusals`).** Set `"isDraft": true` in the fake `gh` fixture and run `quai-verify run-wt land-draft -- quai land 1 --json`. Observe exit code `1` and `"action": "refuse"`.
- **Batch landing (`land-batch`).** Create one worktree per PR (`git worktree add -b <branch> <path> main`), push each branch, and teach the fake `gh` shim `pr list` and `pr edit` too, with a `pr merge` that merges `--match-head-commit <sha>` into `origin/main`. Make two PRs disjoint from a `main` move, one overlapping it without conflict, one with a failing `check`, and one disjoint PR whose worktree you remove after pushing. Run `quai-verify run land-batch -- quai land <a> <overlap> <b> <red> <no-worktree> --json` from the main checkout. Observe exit code `1`, the disjoint PRs (the one without a worktree included) `landed` by `optimistic-merge` first, the red PR `failed`, the overlapping PR `landed` by `rebase-and-merge` last, `pr merge` calls in that order without `--admin` or `--delete-branch`, and no `land.lock` or `land-worktree-*` left on disk. With `[attest].auto_prune = true` on `main` and `git remote set-head origin --auto`, `<git-common-dir>/quai/last-prune` appears after the batch run but not after a batch `--dry-run`. `quai land <n> '#<n>'` exits `2`.

## Gotchas

- Never run `quai land` without a fake `gh` shim in the scratch directory unless you explicitly intend to inspect or merge a real GitHub PR.
- After any `quai land` invocation, verify `<git-common-dir>/quai/locks/land.lock` and `<git-common-dir>/quai/land-worktree-*` do not remain on disk.
