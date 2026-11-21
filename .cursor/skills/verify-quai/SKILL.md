---
name: verify-quai
description: Build and drive the real quai CLI binary in an isolated scratch git repository and linked worktree to prove verify, exec, tier, patch-id, hooks, and land behavior with persisted evidence. Use when proving any change to quai or reproducing a CLI bug.
---

# Verify `quai`

This skill drives the compiled `quai` CLI against disposable git repositories so every verification run proves real git, lock, receipt, hook, and exit-code behavior without touching the developer's checkout or real GitHub repositories.

Read [features/README.md](./features/README.md) before driving a feature. Each feature file lists its entry points, commands, expected end states, and gotchas.

## Launch

Pick a unique `QUAI_VERIFY_RUN_ID` so concurrent runs never collide, then launch:

```bash
export QUAI_VERIFY_RUN_ID="run-$(date +%s)-$$"
.cursor/skills/verify-quai/quai-verify launch
```

`launch` compiles `./cmd/quai` into `/tmp/quai-verify-$QUAI_VERIFY_RUN_ID/bin/quai`, initializes a bare remote `origin.git`, creates a clean git repository with `.quai.toml` and baseline files, pushes `main`, and attaches a linked worktree on `feat/wt`.

Ready means `launch` exits `0` and prints `LAUNCHED`, `SCRATCH`, `EVIDENCE`, and `BINARY`.

## Doctor

Run this read-only check before driving or whenever a run behaves unexpectedly:

```bash
.cursor/skills/verify-quai/quai-verify doctor
```

It verifies that the freshly built `quai` binary executes `quai version`, that both the scratch repo and its linked worktree exist, prints the system `git` version (must be 2.38+ for `git merge-tree`), and confirms the evidence directory exists.

## Drive

Run commands in the main scratch checkout with `run` or in the linked worktree with `run-wt`:

```bash
.cursor/skills/verify-quai/quai-verify run <label> -- quai verify --json
.cursor/skills/verify-quai/quai-verify run-wt <label> -- quai tier --json
```

Each invocation puts `/tmp/quai-verify-$QUAI_VERIFY_RUN_ID/bin` first on `PATH`, runs the exact command, prints the full transcript, and saves `<label>.txt` under `/tmp/quai-verify-evidence-$QUAI_VERIFY_RUN_ID/` with command, working directory, exit code, stdout, and stderr.

For `quai land` verification without hitting real GitHub, place an executable fake `gh` shim inside `/tmp/quai-verify-$QUAI_VERIFY_RUN_ID/bin/gh` that logs every invocation to the evidence directory and returns fixture PR and check JSON. Never run `quai land` without `--dry-run` against a live GitHub repository unless you intend to merge a real PR.

## Evidence

Proof artifacts are written to `/tmp/quai-verify-evidence-$QUAI_VERIFY_RUN_ID/` (override with `QUAI_VERIFY_EVIDENCE_DIR`).

Standards:

- Drive the compiled `quai` binary directly, never internal Go packages.
- Capture both the action and its observable side effect: stdout, stderr, exit code, and a second read-only check of disk state (`receipts/`, `locks/`, `git rev-parse`, `git status --porcelain`).
- For `--dry-run`, verify by reading `git rev-parse HEAD` and `git log` that no rebase, push, or merge occurred.
- Record the feature ID from [features/README.md](./features/README.md) in the `<label>` name.

List all captured artifacts with:

```bash
.cursor/skills/verify-quai/quai-verify evidence
```

## Cleanup

Always run cleanup after a run, including after failed attempts:

```bash
.cursor/skills/verify-quai/quai-verify cleanup
```

`cleanup` deletes `/tmp/quai-verify-$QUAI_VERIFY_RUN_ID` and never deletes `/tmp/quai-verify-evidence-$QUAI_VERIFY_RUN_ID`. Confirm after cleanup that the evidence files remain readable. Never kill processes by name; kill only PIDs spawned by the verification script.

## Helpers

- `.cursor/skills/verify-quai/quai-verify`: executable bash harness providing `launch`, `doctor`, `run`, `run-wt`, `evidence`, and `cleanup`.
