# Command Reference

All `quai` commands work from the repository root or any subdirectory inside a main checkout or linked git worktree.

## Exit codes

| Exit code | Meaning |
|---|---|
| `0` | Success, all checks passed, `SURVIVES`, or PR merged |
| `1` | Verification failed, step mutated the working tree, `CHANGED`, or PR refused |
| `2` | Invalid arguments, missing or invalid `.quai.toml`, or not inside a git repository |
| `3` | Git rebase conflict encountered and cleanly aborted |
| `4` | Lock or slot acquisition timed out, or `quai verify --wait` reached `--max` while the detached run still runs: retry later |

---

## `quai verify`

Run configured verification steps with slot throttling, mutation guard, input-fingerprinted receipts, and optional SSH-signed attestations.

```bash
quai verify [--step <name>...] [--since <ref>] [--no-cache] [--fetch-base] [--sign|--no-sign] [--key <path>] [--push|--no-push] [--remote <name>] [--priority <int>] [--detach] [--json]
quai verify --wait [<run-id>] [--max <duration>] [--json]
quai verify --stop [<run-id>] [--json]
```

### Flags

- `--step <name>`: Run only the named step and its `depends_on` prerequisites, prerequisites first. Repeat `--step` to run multiple steps. Omit to run all configured steps in declaration order, each prerequisite moved before the steps that depend on it.
- `--since <ref>`: Override the base reference used for `merge-base` and `{{base}}`. Defaults to `origin/<base>` or `<base>`.
- `--no-cache`: Ignore existing receipts and force execution.
- `--fetch-base`: Fetch `origin/<base>` before resolving the base, so `{{base}}` and the merge-base use the newest remote tip.
- `--sign` / `--no-sign`: Enable or suppress SSH signing of clean passing receipts into `refs/quai/records/<step>-<fingerprint>`. Also controlled by `QUAI_SIGN=1|0` and `[attest].auto_sign`.
- `--key <path>`: Explicit SSH private key path for `--sign` (overrides `QUAI_SIGNING_KEY`, `[attest].signing_key`, and `git config user.signingkey`).
- `--push` / `--no-push`: Push written attestation refs to `--remote` (implies `--sign`), or suppress `[attest].auto_push` / `QUAI_PUSH=1`.
- `--remote <name>`: Remote name for `--push` (defaults to `[attest].remote`, `"origin"`).
- `--priority <int>`: Queue priority for the slot and in-flight locks of every step, from `-1000` to `1000` (default `0`). See [Slot queue order](#slot-queue-order). Out of range exits `2`.
- `--json`: Print a structured JSON summary to stdout.
- `--detach`: Start the run as a detached run (see below), print its run ID, and exit `0` at once.
- `--wait [<run-id>]`: Wait for a detached run. Defaults to the latest run started from the current worktree.
- `--max <duration>`: With `--wait`, the longest wait before exiting `4` (default `8m`, under the 10-minute limit of agent tool calls). `--max 0` checks without blocking.
- `--stop [<run-id>]`: Stop a detached run. Defaults to the latest run started from the current worktree.

### Behavior

- On a clean working tree, passing steps reuse an existing receipt if the fingerprint matches (`status: "cached"`).
- Concurrent calls on the same `(step, fingerprint)` share one execution via an in-flight lock; waiting callers immediately receive `status: "cached"` once the first run succeeds.
- On a dirty working tree, steps still execute, but no receipts are read or written (`clean: false`, `receipt_written: false`).
- **Mutation guard:** if a passing step edits tracked files, creates untracked non-ignored files, or moves `HEAD`, `quai` refuses to write a receipt, reports `status: "mutated"` with `mutated_paths`, and exits `1`.
- **Conditional pools:** a step with `pool_when` or `pool_tiers` acquires its `pool` only for matching diffs and otherwise runs with `slots = 0`, still writing receipts and signing attestations. Human output prints `pool <step>: routed to|bypassed <pool> ...`, then `pool <step>: acquired <pool>` only once the slot lock is held; JSON adds `pool` and `pool_acquired`, with `pool_acquired: false` on a slot timeout (see [configuration](configuration.md#conditional-pool-routing)).
- **Stale base warning:** when the base ref is `origin/<base>` and this worktree's `FETCH_HEAD` is missing or older than 10 minutes, `quai` reports `base_warning` so an agent knows the merge-base may not reflect the remote tip. Pass `--fetch-base` to refresh it first.
- Stops at the first failing or mutating step, so a step whose prerequisite failed neither runs nor signs.
- Runs each step once per invocation and sets `QUAI_PASSED_STEPS` in its environment to the steps that passed or were cached before it (see [Step dependencies](configuration.md#step-dependencies-depends_on)).
- When `[attest].auto_prune = true` on the remote default branch, runs one best-effort maintenance pass after the steps complete, at most once per `[attest].prune_interval`. The policy is read only from `refs/remotes/origin/<base>` when `<base>` is a plain branch name equal to `origin/HEAD`; any other value skips automatic pruning with a warning. If `refs/remotes/origin/HEAD` is unset, run `git remote set-head origin --auto`. Prune failures are warnings on stderr and never change the verify outcome.

### JSON output

```json
{
	"ok": true,
	"clean": true,
	"head_sha": "a1b2c3d4...",
	"tree_sha": "e5f6a7b8...",
	"base_sha": "99887766...",
	"base_warning": "origin/main may be stale: last fetch was 1h0m0s ago (threshold 10m0s); run git fetch origin",
	"receipt_written": true,
	"steps": [
		{
			"name": "fast",
			"status": "cached",
			"fingerprint": "7a8b...",
			"duration_ms": 4
		},
		{
			"name": "test",
			"status": "passed",
			"fingerprint": "9c0d...",
			"duration_ms": 1694,
			"pool": "heavy",
			"pool_acquired": false
		}
	]
}
```

### Detached runs

A detached run executes `quai verify` (or `quai exec`) in the background, so a verification longer than an agent's tool-call timeout still takes its slots, writes its receipts, and signs and pushes its attestations exactly as in the foreground.

- `--detach` checks the arguments and `.quai.toml`, then starts the same command in a new session (its own process group), with stdin from `/dev/null` and stdout and stderr appended to the run's `output.log`. It prints `Started quai run <run-id>: quai verify --wait <run-id>`, or with `--json` the run's status (`run_id`, `pid`, `worktree`, `log_path`, `state`).
- `--wait` prints the lines of `output.log` written since the previous `--wait` on that run, as they arrive, until the run finishes or `--max` passes. A run ends as soon as a step fails, so a failing step returns at once, followed by a one-line summary naming the failed step. It exits `0` when the run passed, `1` when it failed or died, and `4` when it still runs at `--max`.
- A run whose process is gone without an exit record (for example after `SIGKILL`) is `dead`. The next `--wait` or `--stop` kills the process groups of its steps and releases the locks and queue tickets its lease still holds.
- `--stop` sends `SIGTERM` to the run's process group. The run then stops its step the usual way (`SIGTERM`, `SIGKILL` 5 seconds later) and releases its locks. If the run is still alive 10 seconds after `SIGTERM`, `--stop` sends `SIGKILL` to its group, kills its step groups, and releases its locks itself. It exits `0` once the run no longer runs, `2` for an unknown run ID.
- `--wait` and `--stop` accept the run IDs of `quai exec --detach` too, since `quai exec --wait` already names the slot wait.
- Runs live in `<git-common-dir>/quai/runs/<run-id>/`, one directory per run, so two worktrees never share a log or a read position. Starting a run deletes the finished runs beyond the 20 most recent.
- A run started on another host or PID namespace is never declared dead from here, and `--stop` refuses it.

`quai verify --wait --json` prints one object when the wait ends:

```json
{
	"run_id": "20261122T230242Z-9b845f27",
	"args": ["verify"],
	"worktree": "/work/wt-agent-1",
	"pid": 41234,
	"start_time": 13158963,
	"started_at": "2026-11-22T23:02:42Z",
	"state": "failed",
	"log_path": "/work/repo/.git/quai/runs/20261122T230242Z-9b845f27/output.log",
	"exit": {
		"exit_code": 1,
		"finished_at": "2026-11-22T23:02:45Z",
		"result": { "ok": false, "steps": [{ "name": "fast", "status": "failed", "error": "exit status 1" }] }
	},
	"output": "run     fast\n...\n"
}
```

`state` is `running`, `passed`, `failed`, or `dead`. `exit` is absent while the run runs or when it died; `exit.result` holds the verify JSON of a `quai verify` run, and `exit.stopped` is `true` after `--stop`. A dead run also lists `killed_groups` and `released_locks`.

---

## `quai attest`

Verify SSH-signed in-toto v1 / DSSE v1 attestations in CI and synchronize `refs/quai/records/*` with a git remote.

```bash
quai attest check [--step <name>...] [--onto <ref> | --head <sha>] [--since <ref>] [--config-ref <ref>] [--allowed-signers <path>] [--sample <0-100>] [--fetch] [--remote <name>] [--json]
quai attest push [--step <name>...] [--since <ref>] [--remote <name>] [--json]
quai attest fetch [--remote <name>] [--json]
quai attest prune [--max-age <duration>] [--remote <name>] [--local-only] [--dry-run] [--json]
```

`quai attest check --step` and `quai attest push --step` add each named step's `depends_on` prerequisites, as `quai verify --step` does.

### `quai attest check`

- Computes step fingerprints first and fetches only the matching `refs/quai/records/<step>-<fingerprint>` refs via `git ls-remote` when `--fetch` is set.
- When `--config-ref <ref>` is set (for example `--config-ref origin/main`), loads `.quai.toml` from that git ref via `git show` so a PR cannot weaken its own step policy.
- Resolves the OpenSSH `allowed_signers` file by precedence: `--allowed-signers` > `QUAI_ALLOWED_SIGNERS` > `[attest].allowed_signers`. Passing `--allowed-signers` from a CI variable or `origin/main` prevents an untrusted PR branch from modifying its own trust root.
- When `--onto <ref>` is set (for example `--onto origin/main` on a PR), runs virtual `git merge-tree --write-tree <onto> HEAD` without touching the working tree and computes each step's fingerprint on the merged tree. Exits `3` on conflict.
- When `--head <pr-head-sha>` is set on a checked-out merge commit (GitHub's `refs/pull/<n>/merge`), a step whose exact fingerprint is `"missing"` gets a second lookup. Let `base = merge-base(--since, HEAD)` (the `base_sha` of the exact lookup; `--since` defaults to `origin/<base>`) and `old_base = merge-base(--head, base)`. The second lookup is the attestation signed for the PR head's tree on `old_base`, which is what local `quai verify` signed before `main` moved. The fallback is accepted only if every optimistic-merge condition holds, with `[land].global_paths` and `[tiers]` read from `--config-ref`:
  1. `base` moved past `old_base`, and `git merge-tree --write-tree <base> <head>` is clean.
  2. No file in `old_base..base` matches `[land].global_paths`. With the default empty list, any disjoint move qualifies.
  3. The changed files of `old_base..base` and `old_base..head` are disjoint.
  4. The checked-out `HEAD` is that merge: it differs from `--head` only in `old_base..base` files and from `base` only in `old_base..head` files.
  5. Neither `old_base..base` nor `old_base..head` deletes a test or adds a skip marker while `escalate_on_test_tamper` is on.

  A refused fallback leaves the steps `"missing"`, sets `disjoint_refusal`, and exits `1`, so CI runs the steps itself. `--head` requires `--config-ref` and cannot be combined with `--onto` (exit `2`). It needs the history of both parents (`actions/checkout` with `fetch-depth: 0`).
- When `--sample <pct>` triggers (0 to 100), marks all steps `"sampled"` and exits `1` so CI runs an independent audit, before any exact or disjoint-base lookup.
- Verifies DSSE v1 PAE, in-toto Statement v1 fields, OpenSSH `SSHSIG` signature under namespace `quai` against the resolved `allowed_signers` file, per-step or `[attest].signers` allowlist, and `[attest].max_age`.
- Reports a normalized `reason` for every step, so CI logs explain a fallback to a full run without reading raw git refs:
  `verified`, `no_record`, `base_mismatch`, `tree_mismatch`, `config_mismatch`, `signature_invalid`, `expired`, `sampled`.
- Each step also carries `base_sha`, `tree_sha`, and `fingerprint`; `base_mismatch` names the base the stored record was signed against and suggests fetching `origin/<base>` when it is stale.
- `reason` is advisory: a miss is diagnosed by reading other stored records for the step without verifying their signatures, so treat the wording as a hint while `status` and `ok` stay the trust decision.
- Exits `0` only if every selected step is cryptographically `"verified"`, else `1`.

### `quai attest check` JSON output

```json
{
	"ok": true,
	"match_mode": "disjoint-base",
	"config_ref": "origin/main",
	"tree_sha": "e5f6a7b8...",
	"base_sha": "99887766...",
	"head_sha": "11223344...",
	"old_base_sha": "55667788...",
	"steps": [
		{"name": "fast", "status": "verified", "reason": "verified", "fingerprint": "7a8b...", "ref": "refs/quai/records/fast-7a8b...", "signer": "dev@example.com", "created_at": "2026-11-21T18:10:00Z", "match_mode": "disjoint-base"}
	]
}
```

A miss keeps its diagnostic fields:

```json
{
	"ok": false,
	"tree_sha": "e5f6a7b8...",
	"base_sha": "99887766...",
	"steps": [
		{
			"name": "fast",
			"status": "missing",
			"reason": "tree_mismatch",
			"fingerprint": "7a8b...",
			"tree_sha": "e5f6a7b8...",
			"base_sha": "99887766...",
			"ref": "refs/quai/records/fast-7a8b...",
			"error": "stored attestation 1234abcd... was signed for tree 00112233..., current tree is e5f6a7b8...: the tree changed since signing"
		}
	]
}
```

- `match_mode` is `"exact"` when every step matched the checked-out tree's own fingerprint, and `"disjoint-base"` when at least one step was accepted through `--head`. The top-level `match_mode` is omitted when `ok` is `false`. Each step carries its own `match_mode` (`"exact"` or `"disjoint-base"`) exactly when its `status` is `"verified"`, even if another step leaves `ok` `false`; `missing`, `invalid`, and `sampled` steps have none.
- `head_sha` is the resolved `--head`. `old_base_sha` is `merge-base(--head, base_sha)`, set once the fallback was evaluated. `disjoint_refusal` says why the fallback was refused.

### `quai attest prune`

- Deletes attestation refs under `refs/quai/records/*` whose signed `created_at` timestamp exceeds `--max-age` (default `[attest].max_age`, minimum `1m`) or whose payload is unreadable.
- Prunes both local and remote refs unless `--local-only` is passed; pass `--dry-run` to preview without deleting.
- A dry run never fetches, so it previews record refs already known locally plus their remote counterparts. A remote error still reports local progress and exits `1`.

### GitHub Actions usage

```yaml
- name: Check signed quai attestations
  id: attest
  env:
    ALLOWED_SIGNERS: ${{ vars.QUAI_ALLOWED_SIGNERS }}
  run: |
    printf '%s\n' "$ALLOWED_SIGNERS" > /tmp/quai_allowed_signers
    if quai attest check --fetch --step fast --onto origin/main --config-ref origin/main --allowed-signers /tmp/quai_allowed_signers --sample 10 --json; then
      echo "verified=true" >> "$GITHUB_OUTPUT"
    fi
```

On `pull_request` events, `actions/checkout` checks out the merge commit instead. Check it with `--head`, so a PR verified before an unrelated commit landed on `main` still skips the rerun:

```yaml
- uses: actions/checkout@v4
  with:
    fetch-depth: 0
- name: Check signed quai attestations
  id: attest
  env:
    ALLOWED_SIGNERS: ${{ vars.QUAI_ALLOWED_SIGNERS }}
    PR_HEAD: ${{ github.event.pull_request.head.sha }}
  run: |
    printf '%s\n' "$ALLOWED_SIGNERS" > /tmp/quai_allowed_signers
    if quai attest check --fetch --step fast --head "$PR_HEAD" --since HEAD^1 --config-ref origin/main --allowed-signers /tmp/quai_allowed_signers --sample 10 --json; then
      echo "verified=true" >> "$GITHUB_OUTPUT"
    fi

- name: Run fast checks
  if: steps.attest.outputs.verified != 'true'
  run: ...
```

---

## `quai exec`

Run any arbitrary command behind a named slot pool from `[slots]`.

```bash
quai exec --pool <name> [--slots <n>] [--wait <duration>] [--priority <int>] [--detach] -- <command...>
```

### Flags

- `--pool <name>`: Required pool name declared under `[slots]` in `.quai.toml`.
- `--slots <n>`: Number of slots to acquire (default `1`).
- `--wait <duration>`: Maximum wait duration before exiting `4` (default `30m`).
- `--priority <int>`: Queue priority in the pool, from `-1000` to `1000` (default `0`). See [Slot queue order](#slot-queue-order). Out of range exits `2`.
- `--detach`: Run the command as a detached run with stdin from `/dev/null`, print its run ID, and exit `0`. Follow it with `quai verify --wait <run-id>` or `quai verify --stop <run-id>`; its `exit.exit_code` is the command's own.

### Behavior

- Executes `<command...>` directly without a shell wrapper in its own process group (`Setpgid: true`).
- Streams stdin, stdout, and stderr directly.
- Propagates the child process exit code and always releases acquired slots on exit or signal (`SIGINT`, `SIGTERM`, `SIGHUP`).

---

## `quai tier`

Classify the current branch diff into a verification risk tier, check for test tampering, and report local receipt status.

```bash
quai tier [--since <ref>] [--json]
```

### Flags

- `--since <ref>`: Override base ref for diffing.
- `--json`: Print structured JSON.

### Classification precedence

1. Empty diff: `gate-only`.
2. Pure 100% similarity rename (`R100`) when `pure_renames_gate_only = true`: `gate-only`.
3. Path matches `tiers.full_live`: `full-live`.
4. Path matches `tiers.diff_audit`: `diff-audit`.
5. Path matches `tiers.gate_only`: `gate-only`.
6. Otherwise: `tiers.default`.
7. If `escalate_on_test_tamper = true` and a test file is deleted or gains a skip marker, the overall tier escalates to `full-live`.

Overall branch tier is the maximum across all changed files: `full-live > diff-audit > gate-only`.

### JSON output

```json
{
	"tier": "diff-audit",
	"base_sha": "99887766...",
	"tree_sha": "e5f6a7b8...",
	"clean": true,
	"base_warning": "origin/main may be stale: last fetch was 1h0m0s ago (threshold 10m0s); run git fetch origin",
	"triggers": ["internal/verify/verify.go"],
	"pure_renames": 0,
	"tests_tampered": [],
	"receipts": {
		"fast": {
			"valid": true,
			"fingerprint": "7a8b...",
			"created_at": "2026-11-21T18:10:00Z"
		}
	}
}
```

---

## `quai patch-id`

Check whether a reviewed patch survived a rebase unchanged.

```bash
quai patch-id <verdict-sha> [target-sha] [--base <ref>] [--json]
```

### Arguments and flags

- `<verdict-sha>`: Commit SHA that was previously reviewed or verified.
- `[target-sha]`: Target commit SHA to compare (defaults to `HEAD`).
- `--base <ref>`: Base ref used to compute each commit's `merge-base`. Defaults to `origin/<base>` or `<base>`.
- `--json`: Print structured JSON.

### Behavior

Computes `git diff --binary <merge_base>..<sha> | git patch-id --stable` for both commits.

- Identical patch-ids: prints `SURVIVES` and exits `0`.
- Different patch-ids: computes per-file stable patch-ids across both diffs, lists every changed, added, or removed file in `changed_files`, prints `CHANGED`, and exits `1`.

### JSON output

```json
{
	"status": "SURVIVES",
	"verdict_sha": "11111111...",
	"target_sha": "22222222...",
	"verdict_patch_id": "abcd...",
	"target_patch_id": "abcd...",
	"changed_files": []
}
```

---

## `quai land`

Land a GitHub pull request with virtual `git merge-tree` optimistic merge or serialized locked rebase.

```bash
quai land <pr>... [--dry-run] [--continue-on-error=false] [--method squash|merge|rebase] [--restack] [--require-verdict] [--no-sign] [--no-push] [--json]
```

### Arguments and flags

- `<pr>...`: Pull request number (accepts `42` or `#42`). Two or more numbers land a batch (see below); a number may appear once.
- `--continue-on-error`: In a batch, keep landing the other PRs after one fails (default `true`). With `--continue-on-error=false`, an upfront refusal skips the whole batch and the first failure skips the rest.
- `--dry-run`: Evaluate and print the landing plan (including any detected stacked child PRs) without acquiring `land.lock`, rebasing, pushing, or merging. Exits `0` if mergeable, `1` if refused.
- `--method`: Override `land.method` (`squash`, `merge`, or `rebase`).
- `--restack`: After merging, automatically rebase clean local stacked child worktrees onto the updated base via `git rebase --onto` and push with `--force-with-lease`. Open child PRs targeting the landed branch are always retargeted before merge.
- `--require-verdict`: Require an approved review verdict for this PR's `patch-id` regardless of tier. The verdict is checked against the policy in `origin/<base>`'s `.quai.toml` (see [`quai verdict`](#quai-verdict)).
- `--no-sign` / `--no-push`: Disable automatic attestation signing or pushing during `rebase-and-merge` verification.
- `--json`: Print structured JSON.

### Decision outcomes

| Action | When taken |
|---|---|
| `refuse` | PR closed/draft/fork, required checks failed/missing, or the PR head out of sync: the caller's `HEAD` (on the PR branch) or `origin/<pr-head>` (elsewhere) differs from the head GitHub reports. On the PR branch only, a dirty working tree |
| `direct-merge` | `origin/<base>` equals `merge_base`; verifies required checks and merges with `--match-head-commit` |
| `optimistic-merge` | `origin/<base>` moved, `optimistic_merge = true`, `git merge-tree --write-tree` is clean, base delta is disjoint from PR files and `global_paths`, and GitHub does not block `BEHIND`; merges without rebasing |
| `rebase-and-merge` | `origin/<base>` moved and optimistic merge is disqualified or disabled; acquires `land.lock`, rebases onto `origin/<base>`, runs `patch-id` and `verify` (reusing unchanged step receipts, and queueing for slots at priority `10`), pushes with `--force-with-lease`, waits for required checks, and merges with `--match-head-commit` |

If `git rebase` conflicts, `quai` runs `git rebase --abort`, releases `land.lock`, reports the conflicting files, and exits `3`.

### Where `quai land` runs

`quai land` works from any worktree of the repository, such as a coordinator's worktree on its own branch while the PR branch is checked out in another worktree.

- **On the PR branch:** the caller's worktree must be clean and its `HEAD` must equal the PR head. A `rebase-and-merge` rebases and verifies in place.
- **Anywhere else** (another branch or a detached `HEAD`): `quai` fetches `origin/<pr-head>` and evaluates `direct-merge` and `optimistic-merge` from git objects and the GitHub API, ignoring the caller's own working tree. A `rebase-and-merge`, and a verdict check, run in a temporary detached worktree at `<git-common-dir>/quai/land-worktree-<pr>-<pid>`, which works even when `<pr-head>` is checked out in another worktree. The temporary worktree is removed on every exit, including errors, `SIGINT`, `SIGTERM`, and `SIGHUP`; `quai status` lists any that remain and `quai doctor` warns about them.
- **Without `.quai.toml`:** when the caller's worktree predates `.quai.toml`, `quai land` reads it from `origin/<base>`.

The rebase push always uses `--force-with-lease=refs/heads/<pr-head>:<sha>`, pinned to the PR head read before rebasing.

### Failed verification after rebase

When verification fails after the rebase (or the lease-pinned push is refused), `quai` resets the rebased checkout back to the PR head it started from, pushes nothing, reports `"failure": "post-rebase-verify-failed"` for a verification failure, and exits `1`. The caller's `HEAD` therefore never stays diverged from the remote PR head.

### Transient GitHub errors

Every `gh` call retries up to 3 times, waiting 2 s, 5 s, then 10 s, when it fails on a transient network error: `TLS handshake timeout`, `EOF`, `connection reset`, `i/o timeout`, or HTTP 5xx. Each retry is reported on stderr. Semantic refusals (HTTP 4xx such as 403 and 422, `CONFLICTING`, failing checks) are never retried. When a `gh pr merge` response is lost, `quai` checks that the PR is merged at the expected head before retrying, so a merge that already happened is reported as merged.

When `[attest].auto_prune = true` on the remote default branch, a non-dry-run landing runs the same best-effort maintenance pass as `quai verify`, at most once per `[attest].prune_interval`. If `refs/remotes/origin/HEAD` is unset, run `git remote set-head origin --auto`. `quai land --dry-run` never prunes.

### JSON output

```json
{
	"ok": false,
	"dry_run": false,
	"merged": false,
	"pr": 42,
	"action": "rebase-and-merge",
	"reason": "verification failed after rebase; PR head reset to 1a2b3c4d5, nothing pushed",
	"head_sha": "99887766...",
	"failure": "post-rebase-verify-failed"
}
```

`failure` is absent unless a landing failed for a reason with a stable code; today the only code is `post-rebase-verify-failed`.

### Batch landing

`quai land 101 102 103` lands several PRs in one `land.lock` session:

1. It reads the policy once from `.quai.toml` on `origin/<base>` (`base` from the local `.quai.toml`; a checkout without one takes the first PR's base), never from a PR branch. A missing or invalid policy there exits `2`.
2. It inspects every PR as `--dry-run` would, from the worktree that has the PR's head branch checked out. When no worktree has it, the PR lands from the calling checkout as described in [Where `quai land` runs](#where-quai-land-runs).
3. It lands `direct-merge` and `optimistic-merge` PRs first, in argument order, then the rest with each stacked parent before its children. Every PR re-decides its action under the lock against the moved base, with every single-PR guard (no forks, `--force-with-lease` pinned pushes, required checks, verdicts, `--match-head-commit`).
4. A PR with a child in the batch restacks its children after it merges, as if `--restack` were given. A child whose parent did not land is skipped.

Each PR runs once: no PRs are merged together, and a failure is never bisected. The batch exits `0` when every PR landed, `1` when any PR did not land (refused, failed, conflict, or skipped), `4` when `land.lock` timed out, and `1` on `SIGINT`/`SIGTERM` after releasing the lock. `--dry-run` prints the plan in landing order without taking the lock.

Batch `--json` is a new shape. Each entry of `prs` is the single-PR result plus `status` (`landed`, `planned`, `refused`, `conflict`, `failed`, `skipped`) and an optional `error`, in landing order with refused PRs last (abbreviated):

```json
{
	"ok": false,
	"dry_run": false,
	"prs": [
		{"ok": true, "merged": true, "pr": 101, "action": "optimistic-merge", "status": "landed"},
		{"ok": false, "pr": 102, "action": "rebase-and-merge", "conflict_files": ["shared.txt"], "status": "conflict"},
		{"ok": false, "pr": 104, "action": "optimistic-merge", "reason": "checks failing: [check (FAILURE)]", "status": "failed"}
	]
}
```

---

## `quai gc`

Run the unified maintenance pass on demand: expired receipts, expired `refs/quai/records/*` (local and remote), expired verdicts, and stale lock directories. Detached run directories under `runs/` self-prune to the 20 newest and are not part of this pass.

```bash
quai gc [--dry-run] [--json]
```

### Flags

- `--dry-run`: List what would be removed without mutating any git ref, file, or lock directory. A dry run never fetches, so it reports record refs already known locally plus their remote counterparts.
- `--json`: Print the structured result.

### Behavior

- Uses `[attest].max_age` for receipts, attestation refs, and verdicts, and `[attest].remote` for remote record refs. The manual command uses the working tree's `.quai.toml`; the automatic pass reads `[attest]` only from the remote default branch (`refs/remotes/origin/HEAD`), so a PR branch cannot widen it.
- Automatic pruning rejects a `base` that is not a plain branch name, or whose `refs/remotes/origin/<base>` is not the remote default branch (`@`, `HEAD`, `main@{upstream}`, a PR's own branch), and skips with a warning.
- Automatic pruning also skips when `refs/remotes/origin/HEAD` is unset, which happens in repositories made with `git init`, `git remote add`, and `git fetch`. The warning names the fix; run `git remote set-head origin --auto`, and `quai doctor` reports the same warning.
- Removes a lock directory only when its lease is dead or its owner file has been absent longer than 10 seconds, guarded by the lock's takeover directory. A live holder is never deleted.
- Every category is best effort: a transient remote or filesystem error becomes a warning and the rest of the pass still completes.
- A non-dry run serializes through `prune.lock` and writes the `<git-common-dir>/quai/last-prune` stamp atomically, which also rate-limits automatic pruning.

### JSON output

```json
{
	"ok": true,
	"dry_run": true,
	"max_age": "336h0m0s",
	"remote": "origin",
	"removed": [
		{
			"kind": "receipt",
			"name": ".git/quai/receipts/fast-7a8b.json",
			"location": "local",
			"reason": "expired"
		},
		{
			"kind": "attestation_ref",
			"name": "refs/quai/records/fast-7a8b",
			"location": "remote",
			"reason": "expired"
		}
	],
	"kept": 12
}
```

When `dry_run` is `true`, `removed` lists what would be removed. Warnings appear both in `warnings` and on stderr.

---

## `quai hooks`

Manage the non-destructive `pre-push` receipt hook.

```bash
quai hooks install [--json]
quai hooks uninstall [--json]
quai hooks pre-push [--json]
```

- `install`: Writes a managed hook at `git rev-parse --git-path hooks/pre-push` with `# managed by quai`. If a non-quai hook already exists or `core.hooksPath` points to a committed directory, `install` refuses to overwrite it and prints the one-line snippet to chain `quai hooks pre-push`.
- `uninstall`: Removes the hook only if it contains `# managed by quai`.
- `pre-push`: Checks all steps with `hook = "pre-push"`. If the tree is clean and every receipt hits, exits `0` in under 50 ms. Otherwise executes the missing steps via `verify` and writes receipts on pass.

---

## `quai status`

Inspect active `land.lock`, configured slot pools, their holders and queued waiters, in-flight verifications, detached runs that are `running` or `dead` (`runs` in JSON), temporary land worktrees (`land_worktrees`), local receipt count, and local attestation count without mutating any state.

```bash
quai status [--json]
```

Every holder (`pools[].slots[].owner`, `land_lock.owner`) and waiter (`pools[].waiters[]`, `land_lock.waiters[]`) carries its `role` (`land`, `verify`, or `exec`) and its `priority`. Waiters are listed in service order. The human output prints one line per holder and waiter under its lock:

```text
pool heavy:     1/1 held (stale=0, waiters=2)
  holder verify priority=0    pid 4120: quai verify --step test
  waiter land   priority=10   pid 4188: quai land 42
  waiter verify priority=0    pid 4153: quai verify --step test
```

### Slot queue order

A waiter that finds its lock held writes a ticket into the lock's queue, and only the first ticket in service order may try to acquire. Service order is effective priority, highest first, then enqueue time, oldest first, so waiters of equal priority stay FIFO. The effective priority is the ticket's `priority` plus one point of aging boost per 30 seconds waited, capped at `10`. `quai land` queues at priority `10`, so it passes waiting `quai verify` and `quai exec` runs, which would verify a `main` it is about to move. A priority `0` waiter catches up with a landing after 5 minutes and then wins by enqueue time, so it never starves. A ticket written by a `quai` without priorities reads as priority `0`.

---

## `quai doctor`

Verify repository and host readiness: `git` >= 2.38, `.quai.toml`, `pre-push` hook wiring, `gh`, `ssh-keygen`, attestation readiness, stale lock detection, detached runs that died without an exit record, and temporary land worktrees left while no `quai land` holds `land.lock`. Exits `0` when healthy, `1` if any blocking check fails.

Attestation readiness is checked end to end without mutating anything:

- `attest-config`: the resolved `[attest]` remote, `max_age`, and signer count.
- `attest-signing-key`: `QUAI_SIGNING_KEY`, `[attest].signing_key`, or `git config user.signingkey`, and that the path is a readable private key. The key is checked when `.quai.toml` or the environment enables attestation signing, so a `user.signingkey` set only for commit signing is not flagged.
- `attest-agent-key`: whether `ssh-agent` is running and whether the signing key's `SHA256:` fingerprint is loaded (`ssh-add -l`).
- `attest-allowed-signers`: the resolved `allowed_signers` file exists.
- `attest-remote`: `git ls-remote` proves the attestation remote is reachable for fetch and push; a failure is blocking when `[attest].auto_push` is enabled.

```bash
quai doctor [--json]
```

---

## `quai verdict`

Record and check review verdicts keyed by the branch's stable `git patch-id` and risk tier. Valid verdicts survive clean rebases automatically and invalidate whenever the diff changes outside gate-only paths.

```bash
quai verdict record [--status approved|rejected] [--tier <tier>] [--reviewer <id>] [--note <text>] [--since <ref>] [--sign] [--key <path>] [--push] [--remote <name>] [--json]
quai verdict check [--since <ref>] [--config-ref <ref>] [--require-signed] [--allowed-signers <path>] [--fetch] [--remote <name>] [--json]
```

- `record` also stores the **code patch-id**: the patch-id of the branch diff limited to the paths that are not gate-only. A path is gate-only here when it matches `[tiers].gate_only` and neither `full_live`, `diff_audit`, nor a test glob; `[tiers].default` never makes a path gate-only. `.quai.toml` and the `[attest].allowed_signers` file are never gate-only: a diff that changes either has no code patch-id, so only its exact patch-id can match. When the code patch-id differs from the patch-id, the verdict is also written under `refs/quai/verdicts/<code-patch-id>` (`code_ref` in `--json`), and `--push` pushes both refs. A later verdict on the same code replaces the earlier one under that ref.
- `check` always accepts a verdict whose patch-id matches the branch.
- `--config-ref <ref>` (for example `origin/main`) loads `.quai.toml` from that trusted ref via `git show`, as `quai attest check --config-ref` does, and adds its `[land].require_signed_verdict`. Only with it does `check` also accept a verdict whose code patch-id (or patch-id) matches the branch's code patch-id: the branch then changed only gate-only paths since the review, such as `docs/evidence/**` screenshots. Without `--config-ref`, the gate-only globs would come from the branch's own `.quai.toml`, which the branch can widen, so only the exact patch-id matches.
- `--fetch` fetches both refs. The tier check still applies to the current branch tier. `--json` reports `code_patch_id` when it was computed, and `record.patch_id` names the reviewed diff.
- `quai land` checks verdicts (`--require-verdict`, `[land].require_verdict_tiers`) against the `.quai.toml` of `origin/<base>`, so the code patch-id rule applies there without a flag. When the base has no `.quai.toml`, the verdict check fails.

```yaml
- name: Check the review verdict
  env:
    ALLOWED_SIGNERS: ${{ vars.QUAI_ALLOWED_SIGNERS }}
  run: |
    printf '%s\n' "$ALLOWED_SIGNERS" > /tmp/quai_allowed_signers
    quai verdict check --fetch --since origin/main --config-ref origin/main --require-signed --allowed-signers /tmp/quai_allowed_signers --json
```

---

## `quai restack`

Replay commits after `<old-parent-tip>` onto the updated base branch via `git rebase --onto`, report `patch-id` survival, and optionally push with `--force-with-lease`. Exits `3` on rebase conflict after cleanly aborting.

```bash
quai restack <old-parent-tip> [--base <ref>] [--push] [--remote <name>] [--json]
```

---

## `quai conflicts`

Enumerate active linked worktrees and run read-only virtual `git merge-tree` and file-overlap checks against the base branch and pairwise across worktrees without mutating any checkout. Exits `0` when clean, `1` when conflicts or overlaps are found.

```bash
quai conflicts [--base <ref>] [--conflicts-only] [--json]
```

---

## `quai version`

Print the binary version (`quai 0.3.0`).
