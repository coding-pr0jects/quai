# Contributing to `quai`

Thank you for your interest in contributing to `quai`. This guide covers local setup, architectural conventions, testing, and the pull request workflow.

By participating in this project, you agree to abide by the [Code of Conduct](CODE_OF_CONDUCT.md).

## Prerequisites

- `git` 2.38 or newer (`git merge-tree --write-tree`)
- Go 1.23.4 (pinned in [mise.toml](mise.toml) and [go.mod](go.mod))
- `gh` (GitHub CLI) if testing `quai land` against GitHub
- `mise` (recommended, optional)

## Local setup

Clone the repository and provision the pinned toolchain:

```bash
git clone https://github.com/coding-pr0jects/quai.git
cd quai
mise trust
mise install
```

Build and install locally, or run the test suite:

```bash
mise run build    # compiles ./bin/quai
mise run install  # compiles ~/.local/bin/quai
mise run test     # runs go vet ./... and go test -race ./...
```

Without `mise`, run `go vet ./...` and `go test -race ./...` directly.

## Architecture rules

[AGENTS.md](AGENTS.md) holds the architecture rules, the taste, and the security invariants for every contributor, human or agent. Read it, then [docs/architecture.md](docs/architecture.md), before making structural changes. [GLOSSARY.md](GLOSSARY.md) defines the domain words.

## Testing and verification

Every code change should be backed by tests at the lowest layer that catches the failure:

- **Unit and integration tests:** run `go vet ./...` and `go test -race ./...`. Format all Go files with `gofmt -w .`.
- **Dogfooding:** run `quai verify` against this repository to verify `.quai.toml` and receipt caching.
- **End-to-end CLI verification harness:** for changes touching CLI behavior, hooks, locks, worktrees, or exit codes, run the isolated scratch-repo harness documented in [.agents/skills/verify-quai/SKILL.md](.agents/skills/verify-quai/SKILL.md):

```bash
export QUAI_VERIFY_RUN_ID="dev-$(date +%s)-$$"
.agents/skills/verify-quai/quai-verify launch
.agents/skills/verify-quai/quai-verify doctor
# run scenarios against the isolated scratch repo and linked worktree
.agents/skills/verify-quai/quai-verify cleanup
```

Never test `quai land` without `--dry-run` against a live repository unless you intend to merge a real PR.

## Commit and pull request workflow

1. **Open an issue first for large changes.** Small bug fixes, test improvements, and documentation updates can go straight to a pull request. For new commands, config fields, or scope additions, open a feature request first so we align on design.
2. **Branch from `main`.** Keep pull requests focused on one logical change.
3. **Commit messages.** Use Conventional Commits (`feat(verify): ...`, `fix(lock): ...`, `docs: ...`, `test(land): ...`, `chore: ...`). Keep headers at or under 72 characters.
4. **Update documentation.** If you add or change a flag, config field, exit code, or behavior, update the relevant guide in `docs/`, `README.md`, `GLOSSARY.md`, and `CHANGELOG.md` (under `[Unreleased]`).
5. **Fill out the pull request template.** Include the exact commands you ran and their outcomes.

## Reporting issues

Use the structured forms under `.github/ISSUE_TEMPLATE/` for bug reports and feature requests. For security vulnerabilities, follow [SECURITY.md](SECURITY.md) instead of opening a public issue.
