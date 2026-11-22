# `quai` verification map

This directory is the maintained source for verifying the user-facing behavior of `quai`. Read this index before driving the binary, then follow the matching feature file.

## Baseline preconditions

- Set `QUAI_VERIFY_RUN_ID` to a unique string per run.
- Run `.agents/skills/verify-quai/quai-verify launch` to build `./cmd/quai` and seed `/tmp/quai-verify-$QUAI_VERIFY_RUN_ID/{repo,worktree,origin.git}`.
- Run `.agents/skills/verify-quai/quai-verify doctor` and confirm `OK` on binary, repo, worktree, git version, and evidence dir.
- Never drive the developer's live checkout or merge real GitHub pull requests during verification.

## Driving conventions

- Invoke commands through `quai-verify run <label> -- ...` for the main scratch repo and `quai-verify run-wt <label> -- ...` for the linked worktree.
- Use `--json` when asserting structured fields and plain mode when checking human readability.
- Check side effects after mutations with a second read-only command (`ls`, `git status --porcelain`, `git rev-parse HEAD`, or `quai tier --json`).
- Run `quai-verify cleanup` when finished and confirm evidence files remain in `/tmp/quai-verify-evidence-$QUAI_VERIFY_RUN_ID/`.

## Proof and skip reporting

- Every CLI proof captures the command, working directory, stdout, stderr, and exit code in `<evidence>/<label>.txt`.
- Mutation proofs pair the command transcript with a read-only check of the stored receipt, lock, hook, or git ref.
- Never report a feature verified if a required precondition was missing; record the unmet precondition and stop.

## Feature entry contract

Each feature file starts with an H1 title and one summary paragraph, followed by four H2 sections in order:

1. `Sub-features`
2. `How to get to it (user POV)`
3. `Driving it with quai-verify`
4. `Gotchas`

## Features

- [Verify and receipts](./verify-and-receipts.md) covers `quai verify` fresh runs, input-fingerprinted cache hits across worktrees and unrelated commits, dirty-tree behavior, and the working-tree mutation guard.
- [Slots and exec](./slots-and-exec.md) covers `quai exec` pool concurrency throttling, exit-code propagation, and dead-PID lock takeover.
- [Tier and test tampering](./tier-and-tamper.md) covers `quai tier` risk classification, `R100` pure rename handling, `tests_tampered` escalation, and receipt visibility.
- [Patch-id and hooks](./patch-id-and-hooks.md) covers `quai patch-id` verdict survival after rebase, drift detection, and `quai hooks` installation, non-destructive refusal, and fast `pre-push` receipt hits.
- [Land pull requests](./land-pr.md) covers `quai land` dry-run planning, direct merge, `git merge-tree` optimistic merge, rebase-and-merge with conflict abort, and draft or fork refusal.
