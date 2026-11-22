# Garbage collection and automatic pruning

Covers `quai gc [--dry-run] [--json]` and the best-effort automatic pass triggered by `[attest].auto_prune` after `quai verify` or `quai land`.

## Sub-features

- `quai gc --dry-run --json` lists expired receipts, expired local and remote `refs/quai/records/*`, expired verdicts, and stale lock directories without mutating refs, files, or locks.
- `quai gc --json` removes those items, writes the `last-prune` stamp, and serializes through `prune.lock`.
- `[attest].auto_prune = true` runs the same pass after a non-dry-run `quai land` or `quai verify`, at most once per `prune_interval`, only when `base` is a plain branch name equal to the remote default branch (`refs/remotes/origin/HEAD`).
- A missing or unreachable remote, an untrusted base, or a malformed `max_age` below `1m` produces a warning or a clear config error and never changes the primary command's exit code.

## How to get to it (user POV)

```bash
quai gc --dry-run --json
quai gc --json
```

Automatic pruning is enabled in `.quai.toml`:

```toml
[attest]
max_age = "14d"
auto_prune = true
prune_interval = "24h"
```

## Driving it with quai-verify

1. Seed an expired receipt under `<common-dir>/quai/receipts/` (JSON with `created_at` older than `max_age`) and a stale `land.lock` directory whose `owner` has `pid: 0`.
2. Run `quai gc --dry-run --json`, then read the receipt and lock back with `ls` to prove nothing was removed and no `last-prune` file appeared.
3. Run `quai gc --json`, then confirm the receipt and lock are gone and `<common-dir>/quai/last-prune` exists.
4. Add `auto_prune = true` to `.quai.toml`, commit it on `main`, push `origin main`, and run `git remote set-head origin main` (automatic pruning reads the policy only from the remote default branch). If `refs/remotes/origin/HEAD` is unset, the skip warning and `quai doctor` must tell you to run `git remote set-head origin --auto`. Seed another expired receipt or stale lock and run `quai verify --json`; confirm exit `0`, the item is gone, and a second item stays when a second verify runs inside `prune_interval`.
5. Delete `refs/remotes/origin/HEAD` (`git symbolic-ref --delete refs/remotes/origin/HEAD`), re-age the stamp, and seed a stale lock. Run `quai verify --json`: exit `0`, a warning containing `git remote set-head origin --auto`, and the lock stays. `quai doctor --json` reports `auto-prune` with status `warn` and that fix. Run `git remote set-head origin --auto`, re-age the stamp, and repeat verify: the lock is removed.
6. Point `origin` at a missing path, re-age the `last-prune` stamp, seed an expired receipt, and run `quai verify --json`; confirm exit `0` and an `auto-prune` warning on stderr.
7. Set `base = "@"`, commit it, re-age the stamp, seed a stale lock, and run `quai verify --json`; confirm exit `0`, a `not a plain branch name` warning, and that the lock stays.
8. Create `feat/evil` with `base = "feat/evil"`, commit and push it, re-age the stamp, seed a stale lock, and run `quai verify --json`; confirm exit `0`, a `not the remote default branch` warning, and that the lock stays.

## Gotchas

- A dry run never fetches, so it reports record refs already present locally plus their remote counterparts; it cannot list remote-only refs without mutating the repository.
- Automatic pruning reads `[attest]` only from the remote default branch; a branch-only `auto_prune = true`, `base = "@"`, or `base` set to the PR's own branch must not prune and should warn.
- `refs/remotes/origin/HEAD` is often unset outside a clone. Automatic pruning then skips; the warning names `git remote set-head origin --auto`, and `quai doctor` warns with the same fix.
- The automatic pass runs after the primary command and only writes warnings to stderr; never use stderr emptiness as the success signal.
- A live lock (`prune.lock`, `land.lock`, slot, or in-flight) must survive every pass; assert it with a read-only `quai status --json`.
- `quai land --dry-run` must never prune; check the `last-prune` stamp is unchanged after it.
