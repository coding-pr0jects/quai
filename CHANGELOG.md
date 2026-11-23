# Changelog

All notable changes to `quai` are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `quai attest check --json` reports a normalized per-step `reason` (`verified`, `no_record`, `base_mismatch`, `tree_mismatch`, `config_mismatch`, `signature_invalid`, `expired`, `sampled`) plus `base_sha`, `tree_sha`, and `fingerprint`, so CI logs explain a fallback to a full run (#15).
- `quai verify --fetch-base` refreshes `origin/<base>` before resolving `{{base}}`, and both `quai verify` and `quai tier --json` report `base_warning` when `FETCH_HEAD` is missing or older than 10 minutes (#15).
- `quai doctor` checks end-to-end attestation readiness: `[attest]` config, signing key resolution, `ssh-agent` identity, `allowed_signers` file, and `git ls-remote` remote reachability, all read-only (#15).
- `AGENTS.md` (imported by `CLAUDE.md`) and `GLOSSARY.md` for contributors and coding agents.
- Detached runs (`internal/detach`): `quai verify --detach` and `quai exec --detach` start the command in the background under `<git-common-dir>/quai/runs/<run-id>/`; `quai verify --wait [<run-id>] [--max <duration>]` streams the new output and exits `0` passed, `1` failed or dead, `4` still running; `quai verify --stop [<run-id>]` stops the run and its steps. Dead runs have their step groups killed and their locks released, and `quai status` and `quai doctor` list running and dead runs (#11).
- `quai land` works from any worktree: off the PR branch it evaluates `origin/<pr-head>`, and rebases and verifies in a temporary detached worktree under `<git-common-dir>/quai/`, removed on every exit and signal, even when the PR branch is checked out elsewhere. It reads `.quai.toml` from `origin/<base>` when the caller's worktree has none (#12).
- `quai land` retries `gh` calls failing on a transient network error (TLS handshake timeout, EOF, connection reset, i/o timeout, HTTP 5xx) after 2 s, 5 s, and 10 s; semantic refusals are never retried (#12).
- `quai land` resets the rebased branch to its original head when verification or the push fails after a rebase, and reports `"failure": "post-rebase-verify-failed"` for a failed verification (#12).
- `quai status` lists temporary land worktrees (`land_worktrees`), and `quai doctor` warns about ones left while `land.lock` is free (#12).
- `quai attest check --head <pr-head-sha>`: on a checked-out merge commit whose base moved since local `quai verify`, accepts the PR head's attestation on its old merge base when the base's changes are disjoint from the PR, touch no `[land].global_paths` (read from `--config-ref`), merge cleanly, and tamper with no test. JSON adds `match_mode` (`exact` | `disjoint-base`), `head_sha`, `old_base_sha`, and `disjoint_refusal` (#13).
- Priority slot queues (`internal/lock`): waiters are served by `priority` plus a capped aging boost, then FIFO. `quai land` queues at priority `10` so it passes waiting `quai verify` runs; `quai verify` and `quai exec` take `--priority <int>`. Leases and tickets record their `role` (`land`, `verify`, `exec`) and `priority`, shown by `quai status` and `quai status --json` (#14).
- Conditional pool routing in `quai verify`: `[[steps]]` accepts `pool_when` globs and `pool_tiers` thresholds, acquiring the configured `pool`/`slots` only for matching diffs and otherwise running unlocked while still writing receipts and signing attestations. Output adds `pool` and `pool_acquired`, which is true only once the slot lock is held (#17).
- Unified maintenance command `quai gc [--dry-run] [--json]` (`internal/gc`): prunes expired receipts, local and remote `refs/quai/records/*`, expired verdicts, and stale lock directories without touching a live lock, and reports the plan without mutating anything under `--dry-run`.
- Best-effort automatic pruning via `[attest].auto_prune` and `[attest].prune_interval` (default `24h`), rate-limited by an atomically written `<git-common-dir>/quai/last-prune` stamp and serialized by `prune.lock`. The automatic policy is read from the remote default branch; when `refs/remotes/origin/HEAD` is unset the skip warning and `quai doctor` both name `git remote set-head origin --auto`, remote failures warn on stderr, and neither changes the outcome of `quai verify` or `quai land` (#20).
- Batch landing: `quai land <pr1> <pr2>...` lands several PRs under one `land.lock` session, `direct-merge` and `optimistic-merge` PRs first, then rebases with stacked parents before children, with `--continue-on-error` (default `true`) and a per-PR `--json` report. Policy comes from `origin/<base>`, and a PR whose branch no worktree has checked out lands from the calling checkout like `quai land <pr>` does (#18).
- Step chaining: `depends_on = ["fast"]` on `[[steps]]` runs each prerequisite once and first, for `quai verify`, `quai verify --step`, `quai hooks pre-push`, and `quai attest check|push --step`; each step reads `QUAI_PASSED_STEPS`, the steps that already passed or were cached in the same run (#16).
- Negated `inputs` globs: `inputs = ["**", "!docs/evidence/**"]` leaves the step's fingerprint, receipt, and attestation unchanged when only excluded paths change (#16).
- Review verdicts record a `code_patch_id`, the patch-id without gate-only paths, under a second ref when it differs; `quai verdict check --config-ref <trusted-ref>` and `quai land` (which reads `origin/<base>`'s `.quai.toml`) keep an approved verdict when later commits change only `[tiers].gate_only` paths. A diff that changes `.quai.toml` or the `allowed_signers` file has no code patch-id (#16).

### Security
- Every git call that takes a remote, ref, base, or refspec now passes `--end-of-options` (and `git reset --hard`, which has none, accepts only a validated 40-character SHA), and `.quai.toml` rejects a `base` or `[attest].remote` that starts with `-`, closing a git option-injection path through `quai doctor`, `quai attest fetch/push`, `quai verify --fetch-base`, and land's temporary worktrees (#15).
- `quai doctor` shell-quotes signing-key paths in its `fix` hints instead of pasting them into a runnable command unquoted (#15).

### Changed
- `[attest].max_age` and `quai attest prune --max-age` now reject values below `1m`, so a mistyped policy cannot expire the whole attestation history in one pass.
- `docs/agents.md` is now `docs/agent-integration.md`; the `verify-quai` harness moved to `.agents/skills/verify-quai/`, linked from `.claude/skills/` and `.cursor/skills/`.
- `internal/attest` and `internal/verdict` are classified `full-live` in this repository's `.quai.toml`.

### Removed
- `SPEC.md` (v0.1 contract, superseded by `docs/`) and `RESEARCH.md`; their non-goals now live in `AGENTS.md`.

## [0.3.0] - 2026-11-22

### Added
- Stacked PR landing (`internal/land/stack.go`): `quai land` discovers open child PRs targeting the landing branch, retargets them to the parent's base before merge, and optionally restacks clean local child worktrees with `--restack` / `[land].auto_restack` (`git rebase --onto` + `patch-id` survival + `--force-with-lease`). Added standalone `quai restack <old-parent-tip>` (#7).
- Early cross-worktree conflict detection (`internal/conflicts`): `quai conflicts [--base <ref>] [--conflicts-only] [--json]` enumerates active linked worktrees and runs read-only virtual `git merge-tree` and file-overlap checks against base and pairwise across worktrees (#8).
- Review verdicts tied to `patch-id` (`internal/verdict`): `quai verdict record|check` storing local and optionally SSH-signed DSSE verdicts under `refs/quai/verdicts/<patch-id>` that survive clean rebases, enforced optionally by `quai land` via `[land].require_verdict_tiers`, `require_signed_verdict`, and `--require-verdict` (#9).
- Memory-aware slot admission guard (`internal/lock/mem.go`): `[slots.<pool>]` supports `capacity` and `min_free_mem` (e.g. `"4G"`), reading Linux `/proc/meminfo` `MemAvailable` before granting slots and exposing `available_mem_bytes` in `quai status` (#10).

## [0.2.0] - 2026-11-22

### Added
- PID-namespace-safe leases (`host`, Linux `pid_ns`, periodic lease heartbeats) and fair FIFO ticket queues in `internal/lock` so locks are never stolen across container namespaces and waiting agents never starve (#1).
- Automatic attestation signing and pushing via `[attest].auto_sign`, `[attest].auto_push`, `QUAI_SIGN`/`QUAI_PUSH`, and `--no-sign`/`--no-push` overrides across `quai verify`, `quai hooks pre-push`, and `quai land` rebase-and-merge (#2).
- Targeted O(1) ref fetching via `git ls-remote` in `quai attest check --fetch` and `quai attest prune` to clean expired local and remote `refs/quai/records/*` (#3).
- `--config-ref <ref>` on `quai attest check` to load `.quai.toml` from a trusted base ref instead of the PR working tree (#4).
- Per-`(step, fingerprint)` in-flight deduplication in `quai verify` so concurrent agents share a single run and receive an immediate cache hit (#5).
- `quai status` and `quai doctor` commands (`internal/status`) for multi-agent lock/slot observability and environment diagnostics (#6).

## [0.2.0-alpha] - 2026-11-21

### Added
- Cryptographic SSH-signed attestations (`internal/attest`): in-toto Statement v1 inside DSSE v1 envelopes signed via `ssh-keygen -Y` under dedicated namespace `quai`, stored as orphan commits under `refs/quai/records/<step>-<fingerprint>`.
- `quai verify --sign [--key <path>] [--push]` (and `QUAI_SIGN=1`) to sign clean passing receipts and push attestation refs.
- `quai attest check|push|fetch` supporting `--onto <ref>` virtual `git merge-tree` verification, per-step `signers` allowlists, `max_age` expiry, and probabilistic `--sample <0-100>` CI audit reruns.
- `[attest]` configuration section (`allowed_signers`, `signing_key`, `signers`, `max_age`, `remote`) and per-step `signers` in `.quai.toml`.
- MIT license, open-source documentation suite (`docs/configuration.md`, `docs/commands.md`, `docs/agents.md`, `docs/architecture.md`), `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, `SECURITY.md`, `CHANGELOG.md`.
- GitHub Actions CI workflow (`.github/workflows/ci.yml`), pull request template, and structured issue forms.
- `mise.toml` pinning `go` `1.23.4` and `gh` `2.105.0` with `install`, `build`, and `test` tasks.

### Changed
- Updated Go module path to `github.com/coding-pr0jects/quai`.

## [0.1.0-alpha] - 2026-11-21

### Added
- `.quai.toml` configuration schema, validation, and glob matcher (`internal/config`, `internal/glob`).
- Git repository adapter supporting `git merge-tree --write-tree` and `LC_ALL=C` locale isolation (`internal/repo`).
- Atomic POSIX directory locks, dead-PID `.takeover` recovery, named CPU slot pools, and process-group execution (`internal/lock`, `internal/execrun`).
- `quai verify` with input-fingerprinted receipts shared across linked worktrees, working-tree mutation guard, and automatic receipt pruning (`internal/receipt`, `internal/verify`).
- `quai exec` for running arbitrary commands behind configured slot pools.
- `quai tier` diff risk classifier (`gate-only`, `diff-audit`, `full-live`), pure `R100` rename handling, and test-tampering escalation (`internal/tier`).
- `quai patch-id` rebase survival check with per-file drift reporting (`internal/patchid`).
- `quai hooks` non-destructive `pre-push` hook installer and `<50 ms` receipt fast path (`internal/hooks`).
- `quai land` pure landing decision engine, `git merge-tree` optimistic disjoint merge, locked rebase-and-merge flow, and GitHub CLI adapter (`internal/forge`, `internal/land`).
- Isolated end-to-end CLI verification harness under `.cursor/skills/verify-quai/`.
