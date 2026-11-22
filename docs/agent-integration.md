# Integrating `quai` with Coding Agents

`quai` is designed to sit underneath coding agents (Claude Code, Codex, OpenCode, Cursor, or custom agent harnesses) running in parallel git worktrees. It stays deterministic and zero-LLM: agents call `quai` with `--json` and branch on exit codes and structured fields.

## Recommended multi-worktree layout

Create one linked git worktree per parallel agent:

```bash
git worktree add ../wt-agent-1 -b feat/auth main
git worktree add ../wt-agent-2 -b feat/billing main
git worktree add ../wt-agent-3 -b fix/retry main
```

Because every linked worktree shares the same `<git-common-dir>`, all three agents automatically share:

- CPU slot pools under `<git-common-dir>/quai/locks/slot-<pool>-<n>.lock/`
- The repository landing lock under `<git-common-dir>/quai/locks/land.lock/`
- Passing verification receipts under `<git-common-dir>/quai/receipts/`

No extra daemon, port, or environment variable is required.

## Agent workflow pattern

### 1. Authoring and verifying without CPU starvation

Instruct author agents to run `quai verify --json` instead of raw test commands, and wrap ad-hoc heavy commands in `quai exec`:

```bash
quai verify --json
quai exec --pool heavy -- npm run test:e2e
```

Benefits:

- At most `slots.heavy` suites execute concurrently across all worktrees.
- When `quai hooks install` is enabled, the subsequent `git push` hits the receipt cache in under 50 ms instead of rerunning checks.
- If a check secretly runs a formatter or code generator that dirties the tree, the mutation guard catches it and exits `1` instead of recording a false pass.

### 2. Right-sizing independent review with `quai tier --json`

Before spinning up an expensive reviewer agent or browser/E2E verification session, query `quai tier --json`:

```bash
quai tier --json
```

Route verification depth based on `tier` and `tests_tampered`:

| `tier` | Suggested agent action |
|---|---|
| `gate-only` | Rely on passing `receipts` and CI gates; skip heavy live-app review |
| `diff-audit` | Run a focused code-diff review on `triggers`; reuse valid local `receipts` |
| `full-live` | Require full live/end-to-end verification; inspect any entries in `tests_tampered` |

### 3. Avoiding redundant re-reviews after a rebase

When an agent rebases a previously reviewed branch onto an updated `main`, check whether the patch actually changed:

```bash
quai patch-id <reviewed-sha> HEAD --json
```

- Exit `0` (`"status": "SURVIVES"`): the diff is identical; prior review verdicts still apply.
- Exit `1` (`"status": "CHANGED"`): re-review only the files listed in `changed_files`.

### 4. Serialized PR landing

Instead of letting parallel agents race to rebase and merge PRs simultaneously:

```bash
# Preview the landing strategy safely
quai land 42 --dry-run --json

# Execute optimistic merge or serialized rebase-and-merge
quai land 42 --json
```

- Disjoint PRs whose virtual `git merge-tree` is clean merge immediately without rebasing or triggering a second CI cycle.
- Overlapping PRs wait politely on `land.lock`, rebase one at a time, reuse receipts for unaffected steps, push with `--force-with-lease`, wait for `required_checks`, and merge with `--match-head-commit`.
- If a rebase conflicts, `quai` aborts cleanly, releases `land.lock`, and exits `3` with the conflicting paths so the agent can resolve them explicitly.

## Pairing `quai` with `Ataraxy-Labs/weave`

Parallel agents frequently edit different functions in the same file, which causes false line-level rebase conflicts. Configure [Ataraxy-Labs/weave](https://github.com/Ataraxy-Labs/weave) as a git merge driver in `.gitattributes` and `.git/config`. Because `quai land` delegates rebases to standard `git rebase`, entity-level merging activates automatically with zero extra configuration in `.quai.toml`.

## Example snippet for `AGENTS.md` or `CLAUDE.md`

```markdown
## Verification and landing

- Run `quai verify` before pushing. For heavy ad-hoc commands, wrap them in `quai exec --pool heavy -- <cmd>`.
- Run `quai tier --json` before review to determine whether `gate-only`, `diff-audit`, or `full-live` verification is required.
- After rebasing a reviewed commit, run `quai patch-id <reviewed-sha> HEAD --json` to confirm whether the patch survived unchanged.
- Land PRs via `quai land <pr> --dry-run` followed by `quai land <pr>`.
```
