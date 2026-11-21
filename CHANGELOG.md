# Changelog

All notable changes to `quai` are documented in this file.

The format follows [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
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
