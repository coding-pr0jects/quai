# Market, competitive, and open-source research for `quai`

Date: 2026-11-21
Status: Primary-source investigation supporting [SPEC.md](SPEC.md)

## 1. Problem validation and market sizing

### Verified industry baselines

| Metric | Figure | Source |
|---|---|---|
| Professional developers worldwide, strict employed definition | 20.8M in 2025, 22.0M in 2026 | JetBrains Developer Ecosystem (2025, 2026) |
| Professional developers worldwide, broad commercial definition | 36.5M professional, 47.2M+ total coders | SlashData Developer Nation (2025–2026) |
| Total registered GitHub developer accounts | 180M+ in 2025, 225M+ in 2026 | GitHub Octoverse (2025, 2026) |
| Developers using AI tools regularly | ~80% | Stack Overflow Developer Survey (2025–2026) |
| GitHub Copilot reach | 50M total users, 4.7M+ paid subscribers, ~140k organizations | Microsoft disclosures and Octoverse (2026) |
| Cursor scale | Over 1M daily active users, over $4B ARR mid-2026 | Industry reporting (2026) |
| Claude Code scale | $2.5B ARR early 2026, ~39% workplace usage among surveyed professionals | Industry reporting and JetBrains survey (2026) |
| Pull requests initiated by an AI agent's first draft | Over 40% of all GitHub PRs by Sep 2026 (~110M PRs/month), up from 17% in Mar 2026 | GitHub Octoverse (2026) |
| Commit velocity multiplier on repos with agent config files | ~8x more commits than new repos without agent configs | GitHub Octoverse (2026) |

### Addressable target funnel for `quai`

Every figure below is a transparent estimate derived from the verified baselines above, not a surveyed count.

1. **Broad audience using coding agents with git and CI:** roughly 8M to 15M developers, based on 22M to 36.5M professional developers and 40% to 60% active agentic-tool penetration across Copilot, Cursor, Claude Code, Codex, and OpenCode.
2. **Developers who actually hit `quai`'s bottleneck:** roughly 500k to 2M individual power users and tech leads, plus 50k to 150k small and mid-sized engineering teams. These are teams running 2 or more concurrent agent worktrees, or hitting multi-minute CI and rebase loops on repositories where GitHub Merge Queue is unavailable on Free or Team private plans, or too slow and costly.
3. **Immediate CLI-native early adopters today:** roughly 50k to 250k developers running terminal-first multi-worktree setups with Claude Code (`--worktree`), Codex, OpenCode, or custom harnesses. Existing traction confirms the category is real and active: `jj-vcs/jj` has 32,585 GitHub stars, `arxanas/git-branchless` has 4,144, `Ataraxy-Labs/weave` has 1,364, `Ataraxy-Labs/opensessions` has 1,240, `funador/claude-code-merge-queue` has 132, and `o8-run/o8` has 64.

### Why the bottleneck is structural

Code generation is no longer the limiting factor. When a single developer orchestrates 3 to 8 parallel agents in git worktrees, traditional CI and merge queues fail in four ways:

1. **Duplicate verification.** Author runs, `pre-push` hooks, remote PR CI, and independent reviewer agents repeat identical checks on the same code.
2. **Local CPU starvation.** Parallel worktrees isolate files, not CPU or memory. Concurrent test suites cause timeouts and flaky failures.
3. **O(N^2) rebase loops.** Strict "up-to-date with `main`" merge gates invalidate every sibling PR each time one PR lands.
4. **Undifferentiated review overhead.** Mechanical refactors and clean rebases trigger the same review cost as high-risk UI or protocol changes.

## 2. Deep dive: `useland.dev` (`land`, by Ataraxy Labs)

Primary sources: `https://useland.dev/`, `https://useland.dev/how-it-works`, `https://useland.dev/compare`, `https://docs.useland.dev/`, `https://docs.useland.dev/pricing`, `https://docs.useland.dev/limits`, and `https://github.com/Ataraxy-Labs`.

### Problem it solves

`land` targets the hosted CI and merge-queue bottleneck for parallel coding agents. It replaces per-PR remote CI queues with batched branch merging, kernel-traced test skipping, and entity-level conflict resolution.

### How it works

1. **Kernel-level file-read tracing per test command.** On green runs of `main`, `land` records in the Linux kernel every file each test command opens. On a new branch or batch, any command whose read files did not change is skipped and carried from `main`. No manual input globs or LLM heuristics are required. Ataraxy Labs also publishes this runner standalone as `Ataraxy-Labs/land-action` for GitHub Actions and GitLab CI.
2. **Batched multi-branch landing.** One command, `land merge agent/api agent/ledger agent/email --onto main --json`, combines all finished branches onto `main` at once, tests them together in one pass on ephemeral Linux VMs, lands the passing subset, and returns structured JSON naming the failing test, assertion, clashing branch (`hitBy`), and exact reproduction command for any rejected branch.
3. **Entity-level AST merging via `Ataraxy-Labs/weave`.** Uses an open-source Rust merge driver (1,364 stars) that merges functions, classes, and methods rather than lines, claiming ~95% fewer false conflicts when two agents edit the same file.
4. **Test-tampering refusal.** Refuses a branch before running tests if it deletes, skips, or weakens existing tests (`tests_changed`).
5. **Signed landing without GitHub write credentials.** Produces signed attestations (`refs/land/attestations`) and instructs the local agent to run `git push --atomic origin <sha>:refs/heads/main refs/land/attestations`.
6. **Pricing model.** Hosted SaaS billed by the second: $0.0008/min (2 vCPU Glider), $0.0016/min (4 vCPU Airliner), $0.0024/min (6 vCPU Heavy Lander), with 500 free 2-vCPU minutes per month.

### Differences from `quai`

| Dimension | `useland.dev` | `quai` |
|---|---|---|
| Deployment | Hosted cloud SaaS + CLI | 100% local static Go binary, $0 |
| Target workflow | Replaces PR CI and merge queue, pushes directly to `main` | Coexists with GitHub PRs and existing CI or local-ci mode |
| Execution environment | Managed Linux VMs, no Docker inside tests | Any local runner, Docker sandbox, devcontainer, macOS, Linux, Electron |
| Test skipping | Linux kernel file-read tracing per command | Per-step input fingerprints or repo's own selective runner |
| Conflict handling | Built-in AST entity merge (`weave`) | Standard `git rebase`, composable with `weave` as git merge driver |
| Agent review support | `tests_changed` guard only | Risk tiers (`gate-only`, `diff-audit`, `full-live`) and `patch-id` survival |

### Pros and cons

- **Pros:** Tests N parallel branches in one run instead of N sequential queue jobs; kernel-level dependency tracking eliminates stale manual input lists; entity-level merge eliminates most same-file agent conflicts; catches test-weakening cheats; never asks for GitHub write tokens; per-second pricing is cheaper than GitHub Actions.
- **Cons:** **No Docker, docker compose, or testcontainers inside tests**, which blocks repositories like `agent-studio` that run sandbox and SSH-container tests; Linux-only execution; closed-source hosted backend requiring code upload to Ataraxy Labs; skips only at whole-command granularity; no flaky-test quarantine; no PR risk tiering or post-rebase review verdict survival.

## 3. Deep dive: `github.com/jayehmke/prove`

Primary sources: `https://github.com/jayehmke/prove` (Go, Apache-2.0, created 2026-10-24, 1 star as of 2026-11-21).

### Problem it solves

`prove` solves redundant CI execution by producing cryptographically signed evidence that specific checks passed on exact inputs, so CI verifies the signature in seconds instead of rerunning the checks.

### Measured production result

According to `prove`'s README, Fluent (`becomefluent.io`) moved its CI to `prove` on 2026-10-28. Comparing the first two days against the prior seven days on green runs:

- Pull request CI median dropped from **7.9 min** (470 runs) to **0.7 min** (172 runs).
- Main branch CI median dropped from **10.7 min** (112 runs) to **6.5 min** (51 runs).
- **459 of 565 PR checks (81%)** settled from signed records without rerunning.
- 30 random `--sample` audit reruns on `main` all agreed with their records.

### How it works

1. **Per-check input fingerprints (`.prove.yaml`).** Each check declares `run`, `inputs` globs (defaulting to `**`), and `lockfiles`. `prove` hashes the command, sorted input paths and contents, and lockfiles. If a later commit leaves a check's inputs unchanged, the earlier signed record stays valid.
2. **Standard cryptographic attestations.** Writes an in-toto Statement v1 inside a DSSE envelope signed with the user's SSH key (`SSHSIG`, namespace `prove` so signatures cannot replay as git commit signatures).
3. **Git-ref and external stores.** Stores records as commits under `refs/prove/records/<id>` (never touching branches, HEAD, or working tree), in a local folder, or in S3.
4. **Virtual side-merge check (`--onto <ref>`).** Uses git 2.38+ merge-tree capabilities to evaluate the commit merged onto `main` off to the side without moving HEAD or checking out files. Reuses a branch record where `main` left the check's inputs unchanged and reruns where `main` altered them.
5. **Working-tree mutation guard.** Refuses to record a check if running it modified tracked files, created untracked files, or moved `HEAD` (catching `--fix` or codegen side effects).
6. **Defense in depth for CI.** Supports per-check `signers:` allowlists, `--sample N` probabilistic reruns on CI, and split `prove run --unsigned` plus `prove sign` jobs so untrusted repository code never sees the signing key.

### Differences from `quai`

`prove` is strictly a cryptographic attestation and verification-caching layer. It has no PR landing command, no landing lock, no automated rebase, no local CPU slot semaphore across parallel agents, no diff risk tiering, and no `patch-id` verdict survival check.

### Pros and cons

- **Pros:** Strongest attestation model in this space (in-toto + DSSE + SSHSIG); per-check input fingerprints survive unrelated commits and clean rebases; `--onto` virtual side-merge avoids touching the working tree; mutation guard prevents recording dirty runs; clean git-ref transport; proven 11x PR CI speedup; single static Apache-2.0 Go binary.
- **Cons:** Very new solo repository (1 star, no tagged release yet); zero merge, rebase, or landing orchestration; zero CPU concurrency control for parallel local agents; requires SSH key and signer policy setup for team CI; manual `inputs:` globs can drift if misconfigured.

## 4. Open-source GitHub landscape

All repository facts verified via GitHub API and READMEs on 2026-11-21.

| Project | Stars / Lang / License | Core problem solved | Key mechanism | Main pros | Main cons vs `quai` |
|---|---|---|---|---|---|
| `funador/claude-code-merge-queue` | 132 / TypeScript / MIT | Local zero-cost FIFO merge queue and build serialization for parallel Claude Code agents | Numbered worktree lanes, `build-lock`, FIFO `land`, `regenerableFiles`, port offsets | Solves local CPU build contention and push races for $0; zero runtime deps | Node/TS only; bypasses GitHub PR flow (direct push); no receipts, no tiers, no `patch-id` |
| `jayleaton/go-fast-stack` (`scripts/cilocal`, `land.sh`, `ci-lock.sh`) | Template repo / Go + Bash / MIT | Local CI attestation plus atomic locked PR landing | `make ci-local` status + PR body check; `mkdir` lock with `.takeover`; stack-aware `rebase --onto` via GraphQL + `patch-id` | Battle-tested atomic lock and stack-aware rebase; eliminates GitHub PR test minutes | Tightly coupled to one Go template repo; declarative self-reported `RAN="..."` attestation; no optimistic merge; no `verify` slot pool |
| `Ataraxy-Labs/weave` | 1,364 / Rust / Open source | False line-level merge conflicts between parallel agents | AST entity-level git merge driver (functions, classes, methods) | Drop-in git merge driver; ~95% fewer false conflicts; works seamlessly under `quai land`'s rebase | Merge driver only; no verification, locks, receipts, or PR landing |
| `Ataraxy-Labs/land-action` | 0 / Python / Open source | Skipping unaffected test commands on GitHub Actions and GitLab CI | Linux kernel file-read tracing cached from green `main` runs | Zero manual input globs; works on forks without secrets | Linux CI only; command-granular only; no local workflow or landing |
| `o8-run/o8` | 64 / TypeScript + Rust / MIT | Independent multi-model agent code review and governed merge | Separate reviewer agent tied to exact commit SHA; caps fix loops at 2; CLI + MCP | Clean separation between author and reviewer; multi-agent harness support | Heavy desktop app (macOS-first, Linux preview); governed merge still WIP; does not solve CI/test duplication or rebase locks |
| `arxanas/git-branchless` (`git test`) | 4,144 / Rust / Apache-2.0 | Monorepo-scale git workflow with per-tree cached test results | Caches test pass/fail per git tree SHA in git refs; runs tests in ephemeral worktrees | Mature Rust binary; true per-tree test cache across commits | Opinionated VCS model (anonymous heads, custom revsets); no GitHub PR `land`, optimistic merge, or risk tiers |
| `jellologic/claude-fleet` | 1 / Bash + Python / MIT | Full multi-agent worktree fleet coordination and delegation | Git branch-ref CAS locks, sequential `fleet integrate --cohort`, `fleet gate-check`, disjoint fanout check | Strong anti-bypass hooks (`gate-check`), cohort integration for co-dependent branches | Heavy fleet orchestrator coupled to Claude Code; Bash + Python runtime; no tree-keyed receipts |
| `jdx/hk` | 1,352 / Rust / MIT | Fast git hooks and project lint orchestration | Rust hook runner with parallel step execution | Very fast, mature git-hook manager | Hooks/linting only; no receipts across worktrees, no PR landing, no risk tiers |
| `abhinav/git-spice` | 783 / Go / GPL-3.0 | Stacked Git branches and PR submission | Tracks branch stacks and submits/restacks PRs on GitHub/GitLab | Clean Go CLI for stacked branches | Stack management only; no verification receipts, CPU slots, or landing lock |
| `jj-vcs/jj` | 32,585 / Rust / Apache-2.0 | Modern Git-compatible VCS with first-class rebasing | Stable `change-id` survives rebases; conflict objects stored in commits | Solves rebase pain and change identity at the VCS layer | Replaces daily Git UX; no CI receipts, CPU slots, or PR landing gate |

## 5. Strategic recommendations for `quai`

Comparing `quai`'s v0.1 spec against `prove`, `useland.dev`, `weave`, and `claude-code-merge-queue` surfaces a clear positioning gap: **no existing open-source tool combines local per-input verification receipts, local CPU slot throttling, `git merge-tree` optimistic disjoint landing with a rebase lock, risk tiering, and `patch-id` survival in one zero-dependency Go binary.**

### Features to add or borrow into `quai`

1. **Per-step `inputs` globs and content fingerprinting (from `jayehmke/prove`).** Keying receipts only by whole `HEAD^{tree}` invalidates every receipt on a `README.md` edit or a disjoint rebase. Adding optional `inputs = [...]` (default `["**"]`) and `lockfiles = [...]` lets unchanged steps survive unrelated edits and clean rebases automatically while staying 100% safe by default.
2. **Working-tree mutation guard in `quai verify` (from `jayehmke/prove`).** Record `HEAD` and `git status --porcelain` before and after each step. If a check modifies tracked files, creates untracked non-ignored files, or moves `HEAD`, refuse to write a receipt because the command did not verify the committed tree.
3. **Virtual side-merge check with `git merge-tree --write-tree` (from `jayehmke/prove`, git 2.38+).** Inside `quai land`, run `git merge-tree --write-tree origin/<base> HEAD` before deciding whether optimistic merge is safe. In ~20 ms without touching the working tree, this proves zero textual conflict and lets `quai` check whether `origin/<base>` altered any step's `inputs`.
4. **Test-tampering detection in `quai tier` (from `useland.dev`).** Flag in `quai tier --json` whenever a diff deletes test files or introduces skip markers (such as `.skip`, `it.skip`, `t.Skip`, `@pytest.mark.skip`) and automatically escalate the tier to `full-live` so an agent cannot slip weakened tests through `gate-only` or `diff-audit`.
5. **General-purpose slot lock command `quai exec --pool <name> -- <cmd>` (from `funador/claude-code-merge-queue`'s `build-lock`).** Exposing the slot semaphore directly lets any agent or script serialize heavy builds or e2e runs outside `quai verify` with almost zero extra code.
6. **Document native composition with `Ataraxy-Labs/weave`.** Keep `quai` using standard `git rebase`, and document adding `weave` as the repo's git merge driver so users get entity-level conflict reduction during `quai land` for free.

### Features to drop or keep out of `quai`

1. **Drop custom unsigned `quai/local-ci` GitHub attestation from v0.1.** `jayehmke/prove` already proves that serious remote CI attestation requires cryptographic signing (in-toto + DSSE + SSHSIG), signer allowlists, and sampling. Keep `quai` v0.1 focused on local receipts plus existing GitHub CI checks, and either adopt SSH/DSSE signing in v0.2 or integrate cleanly with `prove` for teams that want remote CI skipping.
2. **Never build a general git-hook/linter framework.** `jdx/hk`, `lefthook`, and `pre-commit` already own that layer. Keep `quai hooks` strictly to the thin `pre-push` receipt check.
3. **Never build an AST merge driver or hosted VM runner.** `Ataraxy-Labs/weave`, `useland.dev`, `davit`, and repo-native sandboxes already own those layers.
4. **Never embed LLM reviewer orchestration.** `o8`, `claude-fleet`, and `/poteto-mode` own agent orchestration. `quai` stays a fast, deterministic, zero-LLM mechanical gate.
