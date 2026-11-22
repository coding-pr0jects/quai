# Verify and receipts

`quai verify` runs the verification steps configured in `.quai.toml`, caches passing runs by input fingerprint inside `<git-common-dir>/quai/receipts/` so linked worktrees and unrelated commits skip redundant work, and refuses to record receipts when a step mutates the working tree.

## Sub-features

- `verify-fresh`: clean tree runs steps, writes receipts, exits `0`.
- `verify-cache-hit`: second run in the same repo or a linked worktree returns `cached` without re-executing.
- `verify-unrelated-commit`: editing a file outside `inputs` keeps the receipt valid.
- `verify-mutation-guard`: a step that modifies tracked files or creates untracked files fails with `status: "mutated"`, exits `1`, and writes no receipt.
- `verify-dirty-tree`: a dirty checkout runs steps and exits `0` on pass with `receipt_written: false`.
- `verify-pool-bypass`: a step with `pool_when` skips an occupied pool for a diff that matches no glob, still writes a receipt, and reports `pool_acquired: false`; a matching diff waits for the pool and reports `pool_acquired: true`.

## How to get to it (user POV)

- Run `quai verify [--step <name>...] [--since <ref>] [--no-cache] [--json]` from any directory inside a git worktree that has `.quai.toml` at its root.

## Driving it with quai-verify

Preconditions: `quai-verify launch` and `quai-verify doctor` succeeded.

- **Fresh run (`verify-fresh`).** Execute `quai-verify run verify-fresh -- quai verify --json`. Observe exit code `0`, `"ok": true`, `"receipt_written": true`, and both `fast` and `verify` with `"status": "passed"`.
- **Cross-worktree cache hit (`verify-cache-hit`).** Execute `quai-verify run-wt verify-wt-hit -- quai verify --json`. Observe exit code `0` and both steps returning `"status": "cached"` from the shared `<git-common-dir>/quai/receipts/` store.
- **Unrelated commit reuse (`verify-unrelated-commit`).** Commit a change to `docs/guide.md` (outside `app/**`) in the worktree, then run `quai-verify run-wt verify-docs-hit -- quai verify --json`. Observe `"status": "cached"` on both steps.
- **Mutation guard (`verify-mutation-guard`).** Add a step whose command writes to `app/main.txt`, commit `.quai.toml`, and run `quai-verify run verify-mutated -- quai verify --step mutator --json`. Observe exit code `1`, `"status": "mutated"`, `"receipt_written": false`, and verify no receipt file was created for `mutator`.
- **Dirty tree (`verify-dirty-tree`).** Append a line to `docs/guide.md` without committing and run `quai-verify run verify-dirty -- quai verify --json`. Observe exit code `0`, `"clean": false`, and `"receipt_written": false`.
- **Conditional pool (`verify-pool-bypass`).** Add `pool_when = ["app/**"]` to the `verify` step and `[land] lock_wait = "500ms"`, then commit. Hold both `heavy` slots with `quai exec --pool heavy --slots 2 -- sleep 5` in the background, commit a `docs/guide.md` change, and run `quai-verify run verify-pool-bypass -- quai verify --json`. Observe exit code `0`, `"pool_acquired": false` on `verify`, and `"receipt_written": true`. Then commit an `app/main.txt` change and rerun: observe exit code `4` (slot timeout) and `"pool_acquired": true`. Plain output prints `pool    verify: bypassed heavy (...)` or `acquired heavy (...)`.

## Gotchas

- `launch` seeds `inputs = ["app/**"]`; editing `app/main.txt` invalidates the fingerprint as expected, whereas editing `docs/guide.md` preserves it.
- Always restore `app/main.txt` or reset after testing `verify-mutation-guard` before driving another feature in the same scratch repo.
