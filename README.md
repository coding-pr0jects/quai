# quai

`quai` is a single static Go CLI binary that coordinates local verification, input-fingerprinted receipts, CPU slot throttling, diff risk tiering, rebase patch-id survival checks, and serialized PR landing for any git repository.

Full design and contracts: [SPEC.md](SPEC.md).
Market and open-source landscape research: [RESEARCH.md](RESEARCH.md).

## Install

Requires Go 1.23+, git 2.38+ (`git merge-tree`), and `gh` for `quai land`.

```bash
go build -o ~/.local/bin/quai ./cmd/quai
```

## Quickstart

Commit a `.quai.toml` at your repository root (see [SPEC.md](SPEC.md#3-configuration-contract-quaitoml) for the full schema and the `agent-studio` example):

```bash
# Run verification steps with CPU slot throttling and write receipts
quai verify

# Reuse receipts automatically on pre-push (<50 ms on cache hit)
quai hooks install

# Classify the branch diff risk tier and inspect local receipt status
quai tier --json

# Check whether a prior review verdict survived a rebase
quai patch-id <verdict-sha> HEAD --json

# Serialize any heavy command behind a configured slot pool
quai exec --pool heavy -- make test

# Land a PR with optimistic disjoint merge or atomic rebase under lock
quai land 42 --dry-run
quai land 42
```

## Entity-level conflict reduction

`quai land` uses standard `git rebase`. To reduce false line-level conflicts when parallel agents edit different functions in the same file, configure [Ataraxy-Labs/weave](https://github.com/Ataraxy-Labs/weave) as your repository's git merge driver. `quai land` inherits it automatically during rebase.

## Development and verification

```bash
go vet ./...
go test -race ./...
```

Project-local end-to-end verification skill: `.cursor/skills/verify-quai/SKILL.md`.
