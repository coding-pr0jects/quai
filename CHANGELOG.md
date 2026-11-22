# Changelog

All notable changes to `quai` are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
