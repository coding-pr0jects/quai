# Command Reference

All `quai` commands work from the repository root or any subdirectory inside a main checkout or linked git worktree.

## Exit codes

| Exit code | Meaning |
|---|---|
| `0` | Success, all checks passed, `SURVIVES`, or PR merged |
| `1` | Verification failed, step mutated the working tree, `CHANGED`, or PR refused |
| `2` | Invalid arguments, missing or invalid `.quai.toml`, or not inside a git repository |
| `3` | Git rebase conflict encountered and cleanly aborted |
| `4` | Lock or slot acquisition timed out |

---

## `quai verify`

Run configured verification steps with slot throttling, mutation guard, input-fingerprinted receipts, and optional SSH-signed attestations.

```bash
quai verify [--step <name>...] [--since <ref>] [--no-cache] [--sign|--no-sign] [--key <path>] [--push|--no-push] [--remote <name>] [--json]
```

### Flags

- `--step <name>`: Run only the named step. Repeat `--step` to run multiple steps. Omit to run all configured steps in declaration order.
- `--since <ref>`: Override the base reference used for `merge-base` and `{{base}}`. Defaults to `origin/<base>` or `<base>`.
- `--no-cache`: Ignore existing receipts and force execution.
- `--sign` / `--no-sign`: Enable or suppress SSH signing of clean passing receipts into `refs/quai/records/<step>-<fingerprint>`. Also controlled by `QUAI_SIGN=1|0` and `[attest].auto_sign`.
- `--key <path>`: Explicit SSH private key path for `--sign` (overrides `QUAI_SIGNING_KEY`, `[attest].signing_key`, and `git config user.signingkey`).
- `--push` / `--no-push`: Push written attestation refs to `--remote` (implies `--sign`), or suppress `[attest].auto_push` / `QUAI_PUSH=1`.
- `--remote <name>`: Remote name for `--push` (defaults to `[attest].remote`, `"origin"`).
- `--json`: Print a structured JSON summary to stdout.

### Behavior

- On a clean working tree, passing steps reuse an existing receipt if the fingerprint matches (`status: "cached"`).
- Concurrent calls on the same `(step, fingerprint)` share one execution via an in-flight lock; waiting callers immediately receive `status: "cached"` once the first run succeeds.
- On a dirty working tree, steps still execute, but no receipts are read or written (`clean: false`, `receipt_written: false`).
- **Mutation guard:** if a passing step edits tracked files, creates untracked non-ignored files, or moves `HEAD`, `quai` refuses to write a receipt, reports `status: "mutated"` with `mutated_paths`, and exits `1`.
- Stops at the first failing or mutating step.

### JSON output

```json
{
	"ok": true,
	"clean": true,
	"head_sha": "a1b2c3d4...",
	"tree_sha": "e5f6a7b8...",
	"base_sha": "99887766...",
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
			"duration_ms": 1694
		}
	]
}
```

---

## `quai attest`

Verify SSH-signed in-toto v1 / DSSE v1 attestations in CI and synchronize `refs/quai/records/*` with a git remote.

```bash
quai attest check [--step <name>...] [--onto <ref>] [--since <ref>] [--config-ref <ref>] [--allowed-signers <path>] [--sample <0-100>] [--fetch] [--remote <name>] [--json]
quai attest push [--step <name>...] [--since <ref>] [--remote <name>] [--json]
quai attest fetch [--remote <name>] [--json]
quai attest prune [--max-age <duration>] [--remote <name>] [--local-only] [--dry-run] [--json]
```

### `quai attest check`

- Computes step fingerprints first and fetches only the matching `refs/quai/records/<step>-<fingerprint>` refs via `git ls-remote` when `--fetch` is set.
- When `--config-ref <ref>` is set (for example `--config-ref origin/main`), loads `.quai.toml` from that git ref via `git show` so a PR cannot weaken its own step policy.
- Resolves the OpenSSH `allowed_signers` file by precedence: `--allowed-signers` > `QUAI_ALLOWED_SIGNERS` > `[attest].allowed_signers`. Passing `--allowed-signers` from a CI variable or `origin/main` prevents an untrusted PR branch from modifying its own trust root.
- When `--onto <ref>` is set (for example `--onto origin/main` on a PR), runs virtual `git merge-tree --write-tree <onto> HEAD` without touching the working tree and computes each step's fingerprint on the merged tree. Exits `3` on conflict.
- When `--sample <pct>` triggers (0 to 100), marks all steps `"sampled"` and exits `1` so CI runs an independent audit.
- Verifies DSSE v1 PAE, in-toto Statement v1 fields, OpenSSH `SSHSIG` signature under namespace `quai` against the resolved `allowed_signers` file, per-step or `[attest].signers` allowlist, and `[attest].max_age`.
- Exits `0` only if every selected step is cryptographically `"verified"`, else `1`.

### `quai attest prune`

- Deletes attestation refs under `refs/quai/records/*` whose signed `created_at` timestamp exceeds `--max-age` (default `[attest].max_age`) or whose payload is unreadable.
- Prunes both local and remote refs unless `--local-only` is passed; pass `--dry-run` to preview without deleting.

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

- name: Run fast checks
  if: steps.attest.outputs.verified != 'true'
  run: ...
```

---

## `quai exec`

Run any arbitrary command behind a named slot pool from `[slots]`.

```bash
quai exec --pool <name> [--slots <n>] [--wait <duration>] -- <command...>
```

### Flags

- `--pool <name>`: Required pool name declared under `[slots]` in `.quai.toml`.
- `--slots <n>`: Number of slots to acquire (default `1`).
- `--wait <duration>`: Maximum wait duration before exiting `4` (default `30m`).

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
quai land <pr> [--dry-run] [--method squash|merge|rebase] [--no-sign] [--no-push] [--json]
```

### Arguments and flags

- `<pr>`: Pull request number (accepts `42` or `#42`).
- `--dry-run`: Evaluate and print the landing plan without acquiring `land.lock`, rebasing, pushing, or merging. Exits `0` if mergeable, `1` if refused.
- `--method`: Override `land.method` (`squash`, `merge`, or `rebase`).
- `--no-sign` / `--no-push`: Disable automatic attestation signing or pushing during `rebase-and-merge` verification.
- `--json`: Print structured JSON.

### Decision outcomes

| Action | When taken |
|---|---|
| `refuse` | Dirty working tree, wrong checked-out branch, PR closed/draft/fork, local `HEAD` out of sync with remote PR head, or required checks failed/missing |
| `direct-merge` | `origin/<base>` equals `merge_base`; verifies required checks and merges with `--match-head-commit` |
| `optimistic-merge` | `origin/<base>` moved, `optimistic_merge = true`, `git merge-tree --write-tree` is clean, base delta is disjoint from PR files and `global_paths`, and GitHub does not block `BEHIND`; merges without rebasing |
| `rebase-and-merge` | `origin/<base>` moved and optimistic merge is disqualified or disabled; acquires `land.lock`, rebases onto `origin/<base>`, runs `patch-id` and `verify` (reusing unchanged step receipts), pushes with `--force-with-lease`, waits for required checks, and merges with `--match-head-commit` |

If `git rebase` conflicts, `quai` runs `git rebase --abort`, releases `land.lock`, reports the conflicting files, and exits `3`.

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

Inspect active `land.lock`, configured slot pools, FIFO waiters, in-flight verifications, local receipt count, and local attestation count without mutating any state.

```bash
quai status [--json]
```

---

## `quai doctor`

Verify repository and host readiness: `git` >= 2.38, `.quai.toml`, `pre-push` hook wiring, `gh`, `ssh-keygen`, attestation key and `allowed_signers` resolution, and stale lock detection. Exits `0` when healthy, `1` if any blocking check fails.

```bash
quai doctor [--json]
```

---

## `quai version`

Print the binary version (`quai 0.2.0`).
