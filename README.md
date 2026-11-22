# quai

[![CI](https://github.com/coding-pr0jects/quai/actions/workflows/ci.yml/badge.svg)](https://github.com/coding-pr0jects/quai/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.23+-00ADD8.svg)](go.mod)

`quai` is a single static Go CLI binary that coordinates local verification, input-fingerprinted receipts, CPU slot throttling, diff risk tiering, rebase `patch-id` survival checks, and serialized PR landing for any git repository.

It sits above `git`, `gh`, and your existing test runner or sandbox. It does not replace them, requires no cloud backend, and outputs structured `--json` for coding agents, humans, and git hooks.

## Why `quai`?

Running multiple coding agents or developers in parallel git worktrees on one machine hits four recurring bottlenecks:

1. **Duplicate verification.** Author checks, `pre-push` hooks, and reviewer passes run the exact same commands repeatedly on unchanged inputs.
2. **Local CPU starvation.** Worktrees isolate files, not CPU or RAM. Concurrent heavy test suites contend for cores and cause flaky timeouts.
3. **Undifferentiated review overhead.** Pure file renames, doc edits, and clean rebases trigger the same review depth as high-risk code changes, while test-skip tampering goes unnoticed.
4. **Rebase churn when landing PRs.** Parallel PRs racing to merge into `main` trigger repeated rebases and redundant CI cycles even when their changes are disjoint.

`quai` stores deterministic shared state inside `<git-common-dir>/quai/` so every linked worktree of a repository shares the same locks, slot pools, and receipts:

```text
worktree A ─┐                                     ┌─ quai verify    (fingerprinted receipts + mutation guard)
worktree B ─┼─► <git-common-dir>/quai/            ├─ quai exec      (machine-wide CPU slot semaphore)
worktree C ─┘     ├── locks/land.lock/            ├─ quai hooks     (<50 ms pre-push receipt fast path)
                  ├── locks/slot-<pool>-<n>.lock/ ├─ quai tier      (risk classification + test-tamper guard)
                  └── receipts/<step>-<hash>.json ├─ quai patch-id  (post-rebase review verdict survival)
                                                  └─ quai land      (merge-tree optimistic merge + locked rebase)
```

## Installation

### Prerequisites

- `git` 2.38 or newer (`git merge-tree --write-tree`)
- `gh` (GitHub CLI, authenticated) for `quai land`
- Go 1.23+ if installing from source

### Option 1: `go install`

```bash
go install github.com/coding-pr0jects/quai/cmd/quai@latest
```

### Option 2: `mise` (from a local clone)

Go and `gh` versions are pinned in [mise.toml](mise.toml):

```bash
git clone https://github.com/coding-pr0jects/quai.git
cd quai
mise trust
mise install
mise run install
```

This compiles and installs `quai` to `~/.local/bin/quai`.

### Option 3: `go build` (from a local clone)

```bash
git clone https://github.com/coding-pr0jects/quai.git
cd quai
go build -o ~/.local/bin/quai ./cmd/quai
```

Verify the install:

```bash
quai version
```

## Quickstart

### 1. Commit `.quai.toml` at your repository root

```toml
version = 1
base = "main"

[slots]
heavy = 2

[[steps]]
name = "fast"
run = "go vet ./..."
inputs = ["**/*.go", "go.mod", "go.sum"]
lockfiles = ["go.sum"]
hook = "pre-push"
slots = 0

[[steps]]
name = "test"
run = "go test -race ./..."
inputs = ["**/*.go", "go.mod", "go.sum"]
lockfiles = ["go.sum"]
pool = "heavy"
slots = 1
timeout = "10m"

[tiers]
default = "diff-audit"
full_live = ["internal/lock/**", "internal/land/**", "cmd/**"]
gate_only = ["*.md", "docs/**"]
test_globs = ["**/*_test.go"]
escalate_on_test_tamper = true

[land]
method = "squash"
optimistic_merge = true
required_checks = ["ci"]
global_paths = ["go.mod", "go.sum", ".quai.toml", ".github/**"]
```

See [docs/configuration.md](docs/configuration.md) for Node/TypeScript, Rust, and Python presets and the full schema.

### 2. Run verification and enable the `pre-push` fast path

```bash
# Run steps with CPU slot throttling and write input-fingerprinted receipts
quai verify

# Install the managed pre-push hook (reuses receipts in <50 ms on cache hit)
quai hooks install

# Run a long verification in the background, then poll it within an agent's tool timeout
quai verify --detach
quai verify --wait --max 8m   # exits 4 while it still runs; rerun to stream the next lines
```

Re-running `quai verify` on a clean tree with unchanged inputs (or after editing a file outside `inputs`, such as `README.md`) reuses the stored receipt immediately across all linked worktrees. If a step modifies tracked files, creates untracked non-ignored files, or moves `HEAD`, `quai verify` refuses to write a receipt and exits `1`.

### 3. Classify risk, check rebase survival, and land PRs

```bash
# Classify branch risk tier, detect test tampering, and inspect local receipts
quai tier --json

# Check whether a reviewed patch survived a rebase unchanged
quai patch-id <verdict-sha> HEAD --json

# Run any heavy command behind a shared slot pool
quai exec --pool heavy -- make e2e

# Preview or execute serialized PR landing
quai land 42 --dry-run
quai land 42
```

## Commands and exit codes

| Command | Purpose |
|---|---|
| `quai verify [--step <name>...] [--since <ref>] [--no-cache] [--sign\|--no-sign] [--key <path>] [--push\|--no-push] [--detach] [--json]` | Run steps with slot locks, in-flight deduplication, mutation guard, receipts, and optional SSH-signed attestations, in the foreground or as a detached run |
| `quai verify --wait [<run-id>] [--max <duration>] [--json]`, `quai verify --stop [<run-id>] [--json]` | Stream a detached run's new output until it ends or `--max` passes, or stop it and its steps |
| `quai attest check\|push\|fetch\|prune [flags...]` | Verify SSH-signed in-toto/DSSE attestations in CI (`--onto`, `--config-ref`, `--sample`), sync, and prune expired `refs/quai/records/*` |
| `quai exec --pool <name> [--slots <n>] [--wait <duration>] [--detach] -- <cmd...>` | Serialize any command behind a FIFO slot pool in its own process group |
| `quai tier [--since <ref>] [--json]` | Classify diff risk (`gate-only`, `diff-audit`, `full-live`), detect test tampering, list receipt status |
| `quai patch-id <verdict-sha> [target-sha] [--base <ref>] [--json]` | Verify whether a reviewed diff survived rebase unchanged (`SURVIVES` vs `CHANGED`) |
| `quai verdict record\|check [flags...]` | Record and verify local or SSH-signed review verdicts keyed by `patch-id` and risk tier |
| `quai land <pr> [--dry-run] [--method squash\|merge\|rebase] [--restack] [--require-verdict] [--no-sign] [--no-push] [--json]` | Land a GitHub PR with `git merge-tree` optimistic merge, stacked child retargeting/restacking, or locked rebase |
| `quai restack <old-parent-tip> [--base <ref>] [--push] [--json]` | Replay a stacked child branch onto the updated base via `git rebase --onto` with `patch-id` survival check |
| `quai hooks install\|uninstall\|pre-push [--json]` | Manage the non-destructive `pre-push` receipt hook (with optional auto-sign/push) |
| `quai status [--json]` | Inspect active `land.lock`, slot pools, free RAM, FIFO waiters, in-flight steps, detached runs, and cached counts |
| `quai doctor [--json]` | Validate git >= 2.38, `.quai.toml`, hooks, `gh`, `ssh-keygen`, signing keys, and stale locks |
| `quai conflicts [--base <ref>] [--conflicts-only] [--json]` | Run read-only virtual `git merge-tree` and overlap checks across active linked worktrees |
| `quai version` | Print the installed version |

Exit codes are uniform across commands:

| Exit code | Meaning |
|---|---|
| `0` | Success, passing checks, `SURVIVES`, or PR merged |
| `1` | Check failed, working tree mutated during `verify`, `CHANGED`, or PR refused |
| `2` | Invalid arguments, missing or invalid `.quai.toml`, or not a git repository |
| `3` | Git rebase conflict encountered and cleanly aborted |
| `4` | Lock or slot acquisition timed out, or `quai verify --wait` reached `--max` while the run still runs |

## Composing with `weave`

`quai land` delegates rebases to standard `git rebase`. To cut false line-level conflicts when parallel agents edit different functions in the same file, configure [Ataraxy-Labs/weave](https://github.com/Ataraxy-Labs/weave) as your repository's git merge driver. `quai land` picks it up automatically.

## Documentation

- [Configuration guide](docs/configuration.md) — full `.quai.toml` schema, input fingerprinting, and language presets
- [Command reference](docs/commands.md) — flags, behaviors, and `--json` output schemas
- [Coding agent integration](docs/agent-integration.md) — wiring `quai` into Claude Code, Codex, OpenCode, Cursor, and custom harnesses
- [Architecture](docs/architecture.md) — one-feature-per-folder layout, pure engines vs adapters, lock protocol, and testing
- [Contributing](CONTRIBUTING.md) — setup and pull request workflow
- [Agent guide](AGENTS.md) — values, non-goals, architecture rules, and taste for anyone changing `quai`
- [Glossary](GLOSSARY.md) — the domain words `quai` uses
- [Security policy](SECURITY.md) — security invariants and vulnerability reporting
- [Changelog](CHANGELOG.md) — release history

## Development

```bash
mise run test
```

Or without `mise`:

```bash
go vet ./...
go test -race ./...
```

## License

MIT. See [LICENSE](LICENSE).
