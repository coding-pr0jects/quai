# `quai` v0.1 specification

`quai` is a single static Go CLI binary that coordinates local verification, input-fingerprinted receipts, CPU slot throttling, diff risk tiering, rebase patch-id survival checks, and serialized PR landing for any git repository.

It sits above `git`, `gh`, and each repository's own test runner or sandbox. It does not replace them.

## 1. Goal and users

Coding agents running in parallel worktrees on one machine lose time to four recurring bottlenecks:

1. Identical checks run three or four times on the same code across the author, `pre-push`, remote CI, and independent verifiers.
2. Parallel test suites starve local CPUs and cause 30-second timeouts and false failures.
3. Mechanical refactors and clean rebases trigger redundant agent verification passes.
4. Parallel PRs targeting `main` force repeated rebases and multiple CI runs per PR.

`quai` solves these bottlenecks through deterministic local state stored in the repository's shared git directory. Every command works for coding agents, humans, and git hooks, and supports `--json` for machine parsing.

The first reference consumer is `agent-studio`. Every feature in v0.1 must remain repository-agnostic and work in any git project without requiring Node, Python, or `mise`.

## 2. Architecture and domain model

`quai` is implemented in Go 1.23 or newer, compiled to one static binary installed at `~/.local/bin/quai`. The only external dependency is `github.com/BurntSushi/toml`. All git and GitHub operations shell out to `git` and `gh` through explicit argv arrays with no shell interpolation.

Packages are organized by domain ownership rather than execution phase:

```text
quai/
├── cmd/quai/               # CLI entrypoint, flag parsing, exit-code mapping
├── internal/
│   ├── config/             # .quai.toml schema, defaults, validation
│   ├── repo/               # git adapter interface and exec implementation (git 2.38+)
│   ├── lock/               # atomic directory locks, slot semaphores, PID takeover
│   ├── receipt/            # input-fingerprinted receipt store, validation, pruning
│   ├── verify/             # step runner, mutation guard, process-group isolation
│   ├── tier/               # pure diff risk classifier, rename and test-tamper checks
│   ├── patchid/            # pure git patch-id survival comparator
│   ├── hooks/              # pre-push hook installation and receipt fast path
│   ├── forge/              # GitHub adapter interface and gh implementation
│   └── land/               # pure landing state machine, merge-tree check, locked executor
```

### Core data shapes

```go
type Tier string

const (
	TierGateOnly  Tier = "gate-only"
	TierDiffAudit Tier = "diff-audit"
	TierFullLive  Tier = "full-live"
)

type Receipt struct {
	Version     int       `json:"version"`
	Fingerprint string    `json:"fingerprint"` // sha256(step.run, sorted input paths + blob hashes, lockfiles)
	TreeSHA     string    `json:"tree_sha"`
	HeadSHA     string    `json:"head_sha"`
	BaseSHA     string    `json:"base_sha"`
	Step        string    `json:"step"`
	Passed      bool      `json:"passed"`
	DurationMS  int64     `json:"duration_ms"`
	CreatedAt   time.Time `json:"created_at"`
}

type LandAction string

const (
	LandRefuse          LandAction = "refuse"
	LandDirectMerge     LandAction = "direct-merge"
	LandOptimisticMerge LandAction = "optimistic-merge"
	LandRebaseAndMerge  LandAction = "rebase-and-merge"
)
```

Every decision engine in `internal/tier`, `internal/patchid`, and `internal/land` is pure. Side effects live only in `internal/repo`, `internal/forge`, `internal/lock`, `internal/receipt`, and `internal/verify`, so unit tests exercise every state transition without spawning `gh` or touching a network.

## 3. Configuration contract (`.quai.toml`)

A repository adopts `quai` by committing `.quai.toml` at its root. `quai` reads `.quai.toml` from the repository top level and fails with exit code 2 if the file is missing or invalid.

### Schema

```toml
version = 1               # required, must be 1
base = "main"             # default base branch, defaults to "main"

[slots]
# Named concurrency pools shared across all worktrees of the repository.
# A pool value must be a positive integer.
heavy = 2

[[steps]]
name = "fast"             # unique step identifier, [a-z0-9-]+
run = "..."               # shell command executed via sh -c in repo root
inputs = ["**"]           # optional globs hashed into the receipt fingerprint; default ["**"]
lockfiles = []            # optional extra files always hashed into the fingerprint
hook = "pre-push"         # optional: "pre-push" marks steps checked by quai hooks pre-push
pool = ""                 # optional: pool name from [slots]; empty means unlimited
slots = 0                 # optional: slots acquired in pool; 0 means no lock
timeout = "15m"           # optional Go duration, defaults to "30m"

[tiers]
default = "diff-audit"        # "gate-only" | "diff-audit" | "full-live"
full_live = []                # glob patterns; any match escalates PR to full-live
diff_audit = []               # glob patterns; any match escalates PR to diff-audit
gate_only = []                # glob patterns; files matching only these stay gate-only
pure_renames_gate_only = true # 100% similarity git renames count as gate-only
test_globs = []               # optional test file globs for test-tampering detection
escalate_on_test_tamper = true # deleted test files or newly added skip markers escalate to full-live

[land]
method = "squash"         # "squash" | "merge" | "rebase", defaults to "squash"
optimistic_merge = true   # skip rebase when merge-tree is clean and main delta is disjoint
required_checks = []      # GitHub check or status names that must pass before merge
lock_wait = "30m"         # max time waiting for land.lock
checks_wait = "15m"       # max time waiting for required GitHub checks
global_paths = []         # globs that disable optimistic merge when modified on base
```

### Template variables in `steps[].run`

`quai` expands only three fixed placeholders before executing a step command:

- `{{base}}` resolves to the exact merge-base commit SHA against the target base ref.
- `{{head}}` resolves to `HEAD` commit SHA.
- `{{tree}}` resolves to `HEAD^{tree}` SHA.

All three values are validated 40-character hexadecimal git SHAs before substitution, preventing shell injection.

### Reference `.quai.toml` for `agent-studio`

```toml
version = 1
base = "main"

[slots]
heavy = 2

[[steps]]
name = "fast"
run = "node scripts/check.ts --since {{base}} && pnpm typecheck"
inputs = ["**"]
lockfiles = ["pnpm-lock.yaml"]
hook = "pre-push"
slots = 0

[[steps]]
name = "verify"
run = "node scripts/sandbox.ts exec pnpm verify --changed --since {{base}}"
inputs = ["**"]
lockfiles = ["pnpm-lock.yaml"]
pool = "heavy"
slots = 1
timeout = "25m"

[tiers]
default = "diff-audit"
full_live = [
	"apps/desktop/src/renderer/**",
	"apps/desktop/src/main/**",
	"packages/protocol/src/**",
	"formal/**"
]
gate_only = [
	"docs/**",
	"*.md",
	".oxfmtrc.jsonc",
	"eslint.config.js",
	"oxlint.config.ts"
]
test_globs = [
	"**/*.test.ts",
	"**/*.e2e.ts",
	"**/test/**"
]
escalate_on_test_tamper = true

[land]
method = "squash"
optimistic_merge = true
required_checks = ["check", "test", "conventions"]
global_paths = [
	"package.json",
	"pnpm-lock.yaml",
	"pnpm-workspace.yaml",
	"tsconfig.base.json",
	"mise.toml",
	"packages/protocol/**",
	".github/**",
	"sandbox/**"
]
```

## 4. Command reference

### Exit codes

All commands use the same exit codes:

| Code | Meaning |
|---|---|
| `0` | Success, passing checks, `SURVIVES`, or PR merged |
| `1` | Check failed, tree mutated during verify, `CHANGED`, or PR refused to land |
| `2` | Invalid arguments, missing or invalid `.quai.toml`, or not a git repository |
| `3` | Git rebase conflict encountered and cleanly aborted |
| `4` | Lock or slot acquisition timed out |

---

### `quai verify`

```text
quai verify [--step <name>...] [--since <ref>] [--no-cache] [--json]
```

Runs configured verification steps in declaration order, manages CPU slots, checks that no step mutates the working tree, and records input-fingerprinted receipts.

**Input fingerprinting.**

For a step on a given git tree (`HEAD^{tree}` or a virtual tree from `git merge-tree`):

- If `inputs` is `["**"]` and `lockfiles` is empty, the fingerprint is `sha256(step.run, tree_sha, base_sha)`.
- Otherwise, `quai` lists every tracked file in that tree matching `inputs` or `lockfiles` via `git ls-tree -r`, sorts by path, and computes `sha256(step.run, sorted "<path>:<blob_sha>" lines)`.
- If a step's `run` contains `{{base}}`, `base_sha` is also included in the fingerprint when the step's inputs intersect the base delta. When none of the step's inputs changed, the fingerprint stays identical across unrelated commits and clean rebases.

**Behavior.**

1. Resolve `base` from `--since`, or `origin/<config.base>`, or `<config.base>`, and compute `merge_base`.
2. Select steps named by `--step`, or all configured steps if `--step` is omitted.
3. Check whether the working tree is clean (`git status --porcelain` is empty).
4. For each selected step:
   1. If the tree is clean and `--no-cache` is not set, look up `<git-common-dir>/quai/receipts/<step>-<fingerprint>.json`. If a passing receipt exists, report `cached: true` and skip execution.
   2. If `step.slots > 0`, acquire a slot in `step.pool` before spawning the command and release it immediately when the step finishes.
   3. Capture `before_head` and `before_status` before spawning.
   4. Run `step.run` in its own process group (`Setpgid: true`) bounded by `step.timeout`. Stream stdout and stderr directly in human mode, or capture them in `--json` mode.
   5. Stop at the first failing step and exit 1. Write no receipt for a failing step.
   6. **Working-tree mutation guard.** After a passing step on a clean tree, compare `HEAD` and `git status --porcelain` against `before_head` and `before_status`. If the step edited tracked files, created untracked non-ignored files, or moved `HEAD`, write no receipt, report `mutated: true` with the changed paths, and exit 1.
   7. If the step passed and left the tree untouched, atomically write the receipt.

**Constraints.**

- `quai` provides no flag to write a receipt without running the step.
- When the working tree is dirty before `quai verify` starts, `quai verify` still executes all requested steps and reports their pass or fail status, but writes no receipt and sets `receipt_written: false`.

**JSON output shape.**

```json
{
	"ok": true,
	"clean": true,
	"head_sha": "a1b2c3d4...",
	"tree_sha": "e5f6a7b8...",
	"base_sha": "99887766...",
	"receipt_written": true,
	"steps": [
		{ "name": "fast", "status": "cached", "fingerprint": "7a8b...", "duration_ms": 4 },
		{ "name": "verify", "status": "passed", "fingerprint": "9c0d...", "duration_ms": 41200 }
	]
}
```

---

### `quai exec`

```text
quai exec --pool <name> [--slots <n>] [--wait <duration>] -- <command...>
```

Acquires slots in a named pool from `[slots]` in `.quai.toml`, runs `<command...>` directly with no shell wrapper in its own process group, streams stdin/stdout/stderr, releases the slots on exit or signal, and exits with the child command's exit code.

This exposes the machine-wide concurrency semaphore for any heavy build, benchmark, or end-to-end command invoked outside `quai verify`.

---

### `quai tier`

```text
quai tier [--since <ref>] [--json]
```

Classifies the current branch diff against the base into a verification risk tier, flags test tampering, and reports which local step receipts already exist for `HEAD^{tree}`.

**Classification rules.**

1. Collect changed files from `git diff -M100% --name-status <merge_base>` plus untracked non-ignored files when the tree is dirty.
2. For each file:
   - If `pure_renames_gate_only` is true and git reports status `R100`, assign `gate-only`.
   - Else if the path matches any glob in `tiers.full_live`, assign `full-live`.
   - Else if the path matches any glob in `tiers.diff_audit`, assign `diff-audit`.
   - Else if the path matches any glob in `tiers.gate_only`, assign `gate-only`.
   - Else assign `tiers.default`.
3. **Test-tampering detection.** Scan files matching `tiers.test_globs` (or default test patterns `*_test.*`, `*.test.*`, `*.spec.*`, `*.e2e.*`, `test_*.py` when `test_globs` is empty). Flag `tests_changed` if any test file is deleted, or if added diff lines introduce skip markers (`.skip(`, `it.skip`, `describe.skip`, `test.skip`, `t.Skip(`, `@pytest.mark.skip`, `#[ignore]`). When `escalate_on_test_tamper` is true and any tamper finding is present, escalate the branch tier to `full-live`.
4. The branch tier is the highest tier among all changed files and escalations, ordered `full-live > diff-audit > gate-only`. An empty diff returns `gate-only`.
5. Compute each configured step's fingerprint on `HEAD^{tree}` and report whether a valid local receipt exists.

**JSON output shape.**

```json
{
	"tier": "diff-audit",
	"base_sha": "99887766...",
	"tree_sha": "e5f6a7b8...",
	"clean": true,
	"triggers": ["apps/agent-host/src/sessions/sessions.ts"],
	"pure_renames": 0,
	"tests_tampered": [],
	"receipts": {
		"fast": { "valid": true, "fingerprint": "7a8b...", "created_at": "2026-11-21T18:10:00Z" },
		"verify": { "valid": true, "fingerprint": "9c0d...", "created_at": "2026-11-21T18:11:12Z" }
	}
}
```

---

### `quai patch-id`

```text
quai patch-id <verdict-sha> [target-sha] [--base <ref>] [--json]
```

Determines whether a verified patch survived a rebase unchanged. `target-sha` defaults to `HEAD`.

**Algorithm.**

1. Resolve `base_ref` from `--base`, or `origin/<config.base>`, or `<config.base>`.
2. Compute `verdict_base = git merge-base <base_ref> <verdict-sha>` and `target_base = git merge-base <base_ref> <target-sha>`.
3. Compute `verdict_id` by piping `git diff --binary <verdict_base>..<verdict-sha>` into `git patch-id --stable`.
4. Compute `target_id` by piping `git diff --binary <target_base>..<target-sha>` into `git patch-id --stable`.
5. If `verdict_id == target_id`, return `SURVIVES` and exit 0.
6. If they differ, compute per-file `git patch-id --stable` across the union of files touched in both diffs, list every file whose file-level patch-id differs or that was added or removed between the two states, return `CHANGED`, and exit 1.

**JSON output shape.**

```json
{
	"status": "SURVIVES",
	"verdict_sha": "11111111...",
	"target_sha": "22222222...",
	"verdict_patch_id": "abcd...",
	"target_patch_id": "abcd...",
	"changed_files": []
}
```

---

### `quai land`

```text
quai land <pr> [--dry-run] [--method squash|merge|rebase] [--json]
```

Lands one GitHub pull request targeting `main` or an explicit non-stacked base branch, serializing landing mutations under a repository-wide lock.

**Virtual merge check (`git merge-tree`, git 2.38+).**

When `origin/<base>` has moved past `merge_base` and `optimistic_merge` is true, `quai land` runs `git merge-tree --write-tree origin/<base> HEAD` without touching the working tree or moving `HEAD`:

- If `merge-tree` reports a conflict, optimistic merge is disqualified immediately and `quai` takes the rebase path (which will surface the exact conflict cleanly).
- If `merge-tree` succeeds cleanly with `merged_tree_sha`, `quai` checks both that `base_delta` is disjoint from `pr_files` and `global_paths` AND whether every configured step's input fingerprint on `merged_tree_sha` still matches a valid receipt (when narrow `inputs` are configured). If disjoint and clean, `quai` proceeds with `optimistic-merge` without rebasing or restarting remote CI.

**State and action table.**

| Condition | Action | Exit |
|---|---|---|
| Working tree dirty, or checked-out branch differs from PR head branch | Refuse before acquiring lock | `1` |
| PR closed, merged, draft, or from a fork | Refuse | `1` |
| Local `HEAD` differs from remote `pr.headRefOid` | Refuse, tell caller to sync | `1` |
| `origin/<base>` equals `merge_base` | `direct-merge`: check required checks, merge with `--match-head-commit` | `0` or `1` |
| `origin/<base>` moved, `optimistic_merge == true`, `git merge-tree` is clean, base delta is disjoint from PR files and `global_paths`, and GitHub does not block `BEHIND` | `optimistic-merge`: check required checks on current head, merge with `--match-head-commit` | `0` or `1` |
| `origin/<base>` moved and fails optimistic check, `optimistic_merge == false`, or GitHub blocks `BEHIND` | `rebase-and-merge`: rebase onto `origin/<base>`, run only steps whose fingerprint has no valid receipt, push `--force-with-lease`, wait checks, merge with `--match-head-commit` | `0`, `1`, or `3` |
| Rebase produces a git conflict | `git rebase --abort`, release lock, return conflict files | `3` |
| Any required check missing, failing, cancelled, or `skipped` | Refuse without merging | `1` |
| Base or head moved in the race window before merge | Retry decision loop up to 2 times inside the held lock, then refuse | `1` |

**Locking and execution steps.**

1. Run pre-lock checks: verify clean tree, query `gh pr view <pr>`, verify PR is open, not draft, not cross-repository fork, and current branch matches `headRefName`.
2. If `--dry-run` is set, compute and print the `LandAction` plan without acquiring the lock, rebasing, pushing, or merging. Exit 0 if the plan is mergeable, 1 if refused.
3. Acquire `<git-common-dir>/quai/locks/land.lock`, waiting up to `land.lock_wait`. Always release the lock on exit, error, or signal.
4. Fetch `origin/<base>` and `refs/pull/<pr>/head`. Re-read PR metadata from GitHub.
5. Evaluate the pure `DecideLand(...)` state machine using the `git merge-tree` check.
6. If `rebase-and-merge`:
   1. Record `pre_rebase_sha = HEAD`.
   2. Run `git rebase origin/<base>`. Repositories that install `Ataraxy-Labs/weave` as their git merge driver get entity-level conflict resolution here automatically. On failure, run `git rebase --abort` and exit 3.
   3. Compute `quai patch-id <pre_rebase_sha> HEAD` and include the result in the output so callers know whether prior review verdicts survived.
   4. Run `quai verify` for configured steps. Steps whose `inputs` fingerprint did not change reuse their existing receipts automatically.
   5. Push with `git push --force-with-lease=<branch>:<pre_rebase_sha> origin HEAD:<branch>`.
7. Wait for every check listed in `land.required_checks` up to `land.checks_wait`. Refuse if any required check is missing after creation grace window, failing, cancelled, or skipped, or if any other check that ran failed.
8. Merge using `gh pr merge <pr> --<method> --match-head-commit <head>`. Never pass `--admin` and never pass `--delete-branch`.
9. Verify merged state via `gh pr view <pr> --json state,mergeCommit` and release `land.lock`.

---

### `quai hooks`

```text
quai hooks install
quai hooks uninstall
quai hooks pre-push
```

- `install` writes a managed `pre-push` hook at `git rev-parse --git-path hooks/pre-push` with the marker `# managed by quai`. If a non-quai hook already exists at that path or `core.hooksPath` points to a committed folder like `.githooks`, `install` does not overwrite it and prints the exact shell snippet to chain `quai hooks pre-push`.
- `uninstall` removes the hook only if it contains `# managed by quai`.
- `pre-push` reads all steps where `hook == "pre-push"` from `.quai.toml`. If the working tree is clean and every such step already has a valid receipt for its current fingerprint, it prints one summary line and exits 0 in under 50 ms. Otherwise it runs those steps via `internal/verify` and writes receipts on pass.

## 5. Concurrency, locks, and lifecycle

All local state lives inside `<git-common-dir>/quai/` so every linked worktree of a repository shares the same locks, slot pools, and receipts:

```text
<git-common-dir>/quai/
├── locks/
│   ├── land.lock/
│   │   └── owner
│   └── slot-<pool>-<index>.lock/
│       └── owner
└── receipts/
    └── <step>-<fingerprint>.json
```

### Atomic lock and dead-PID takeover

1. Acquisition creates `<lock-dir>` using `os.Mkdir`, which is atomic on POSIX filesystems.
2. On success, the holder writes `owner` atomically containing JSON: `pid`, `command`, `worktree`, `acquired_at`.
3. On `EEXIST`, the waiter reads `owner`. If `pid` is dead (`kill(pid, 0)` returns `ESRCH`), the waiter acquires `<lock-dir>.takeover`, re-reads `owner` to confirm the same dead PID, rewrites `owner` with its own PID, and removes `.takeover`.
4. If `owner` is absent for more than 10 seconds, the waiter emits a warning naming the lock path without breaking it.
5. Polling interval is 500 ms for `slot` locks and 2 s for `land.lock`, with a progress line printed every 30 s while waiting.

### Slot semaphore for parallel verification

For a step declaring `pool = "heavy"` and `slots = 1` with `[slots] heavy = 2`, `quai verify` and `quai exec` attempt to acquire any free directory in `slot-heavy-0.lock` or `slot-heavy-1.lock`. When all slots are held, `quai` waits until one releases or a dead holder is taken over. Steps with `slots = 0` acquire no lock and run immediately in parallel.

### Process cleanup and receipt retention

- Every child command runs with `Setpgid: true`. On `SIGINT`, `SIGTERM`, `SIGHUP`, or step timeout, `quai` sends `SIGTERM` to `-pgid`, waits 5 seconds, then sends `SIGKILL` to `-pgid` before releasing its locks.
- Receipts are written to a temporary file in `<git-common-dir>/quai/receipts/` and atomically renamed into place.
- After every receipt write, `quai` prunes receipts older than 14 days and keeps at most the 200 newest receipt files.

## 6. Security invariants

1. **No command injection from forge metadata.** PR titles, bodies, comments, and branch names are never passed to `sh -c`. All `git` and `gh` invocations use direct argument arrays.
2. **No forged or dirty receipts.** `quai` exposes no command to mark a step passed without running it, and refuses to store a receipt if the step modified the working tree or moved `HEAD`.
3. **No privilege escalation on merge.** `quai land` never passes `--admin` and never passes `--delete-branch`.
4. **No untrusted forks.** `quai land` refuses any cross-repository pull request before acquiring the lock or touching the local checkout.
5. **No silent overwrite of remote commits.** Rebase pushes always use `--force-with-lease=<branch>:<expected_sha>` pinned to the head SHA read before rebasing.

## 7. Verification plan and delivery units

Build v0.1 in six small, verifiable commits in `/home/samuel/Documents/Projects/quai`, proving each unit with `go test -race ./...` before starting the next:

| Unit | Scope | Verification proof |
|---|---|---|
| 1 | Module init, `internal/config`, `internal/repo`, CLI skeleton | Unit tests for `.quai.toml` parsing, validation errors, and git queries including `merge-tree` on a temp repo |
| 2 | `internal/lock` atomic lock, slot pool, dead-PID takeover, `quai exec` | Concurrency test spawning real parallel child processes contending for 2 slots and killing one holder with `SIGKILL` |
| 3 | `internal/receipt` and `internal/verify` runner | Integration test on a temp repo proving fingerprint reuse across unrelated commits, mutation guard refusal on `--fix` or dirty write, pruning, and orphan cleanup on `SIGTERM` |
| 4 | `internal/tier` and `internal/patchid` | Temp git repo tests covering `R100` renames, `tests_tampered` escalation, tier precedence, clean rebase returning `SURVIVES`, and modified rebase returning `CHANGED` with exact drifted files |
| 5 | `internal/hooks` | Temp git repo tests for `install`, non-destructive refusal on existing hooks, `uninstall`, and `<50 ms` `pre-push` receipt hit |
| 6 | `internal/forge` and `internal/land` | Table tests for `DecideLand` across every state, `merge-tree` optimistic merge and conflict abort tests on real temp repos, and live dogfooding of `quai verify`, `quai tier`, `quai patch-id`, and `quai land --dry-run` on `agent-studio` |

## 8. Out of scope for v0.1

- Stacked PRs, child branch retargeting, and `git rebase --onto` past a squash-merged parent. Planned for v0.2.
- Forges other than GitHub via `gh`, such as GitLab or Forgejo.
- Cross-machine receipt sync via `git notes`, `refs/quai/*`, or S3.
- Batch merge trains with automatic bisection.

## 9. Future ideas (post-v0.1 backlog)

The ideas below are deliberately left out of v0.1 to keep the alpha small and fast, and are kept here as documented options if needed later:

1. **Cryptographic remote CI attestation (`local-ci` mode).** Sign receipts using SSH keys in DSSE / in-toto envelopes (following `jayehmke/prove`), add a `signers` policy, publish records to `refs/quai/records/*` or integrate directly with `prove`, and provide a thin GitHub Action with `--sample` audit reruns so remote PR CI can verify local records in seconds instead of rerunning tests.
2. **Extended git-hook and linter orchestration.** Expand `quai hooks` beyond the thin `pre-push` receipt check to cover staged `pre-commit` formatting, linting, and commit-message rules for repositories that do not want a separate hook manager such as `hk` or `lefthook`.
3. **Deeper merge, isolation, and review layers.** Optional integrations or built-in modes for entity-level AST merging (beyond documenting `Ataraxy-Labs/weave` as a git merge driver), managed container or Firecracker microVM step execution (similar to `davit`), and a signed independent-reviewer verdict registry that `quai land` can require on `full-live` PRs.
