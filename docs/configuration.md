# Configuration (`.quai.toml`)

Every repository adopts `quai` by committing a `.quai.toml` file at the repository root. Unknown keys are rejected strictly with exit code `2` so typos fail immediately.

`quai land` reads `.quai.toml` from the caller's worktree, or from `origin/<base>` of the PR when that worktree predates it.

## Full schema

```toml
version = 1                     # Required. Must be 1.
base = "main"                   # Default base branch. Defaults to "main". Must not start with "-".

[slots]
# Named concurrency pools shared across all linked worktrees of the repo.
# Pool names must match ^[a-z0-9-]+$ and values must be positive integers
# or tables with capacity and optional min_free_mem.
light = 4

[slots.heavy]
capacity = 2
min_free_mem = "4G"             # Optional RAM admission guard (Linux /proc/meminfo MemAvailable).
min_free_disk = "2G"            # Optional free-disk guard on the repo and temp filesystems.

[[steps]]
name = "fast"                   # Required, unique, matches ^[a-z0-9-]+$.
run = "go vet ./..."            # Required. Executed via sh -c in repo root.
inputs = ["**"]                 # Optional globs hashed into the receipt; "!glob" excludes. Default: ["**"].
lockfiles = []                  # Optional extra files always hashed into the receipt.
depends_on = []                 # Optional steps that must pass (or be cached) before this one.
hook = "pre-push"               # Optional: "pre-push" or empty.
pool = ""                       # Optional: pool name from [slots].
slots = 0                       # Optional: number of slots to hold (>= 0). 0 means unlocked.
pool_when = []                  # Optional globs on changed paths that require pool + slots.
pool_tiers = []                 # Optional tier threshold that also requires pool + slots.
timeout = "30m"                 # Optional Go duration string. Default: "30m".

[tiers]
default = "diff-audit"          # "gate-only" | "diff-audit" | "full-live". Default: "diff-audit".
full_live = []                  # Globs that escalate the branch to full-live.
diff_audit = []                 # Globs that escalate the branch to diff-audit.
gate_only = []                  # Globs allowed to stay gate-only; changes there keep an approved verdict (base policy only).
pure_renames_gate_only = true   # 100% similarity renames (R100) count as gate-only. Default: true.
test_globs = []                 # Optional globs for test files. Empty uses built-in defaults.
escalate_on_test_tamper = true  # Escalate deleted tests or added skip markers to full-live. Default: true.

[land]
method = "squash"               # "squash" | "merge" | "rebase". Default: "squash".
optimistic_merge = true         # Skip rebase when merge-tree is clean and base delta is disjoint. Default: true.
required_checks = []            # GitHub check or commit status names that must succeed before merge.
lock_wait = "30m"               # Max wait for land.lock. Default: "30m".
checks_wait = "15m"             # Max wait for required GitHub checks. Default: "15m".
global_paths = []               # Globs that disable optimistic merge when changed on base.
require_verdict_tiers = []      # Optional risk tiers (e.g. ["full-live"]) requiring an approved verdict before merge.
require_signed_verdict = false  # When true, require SSH-signed verdicts under refs/quai/verdicts/<patch-id>.
auto_restack = false            # When true, automatically restack clean local child worktrees after landing a parent PR.

[attest]
allowed_signers = ".quai/allowed_signers" # OpenSSH allowed_signers file for verifying CI attestations.
signing_key = ""                          # Optional default SSH private key path (or QUAI_SIGNING_KEY / git user.signingkey).
signers = []                              # Optional principal allowlist (empty = any key in allowed_signers).
max_age = "14d"                           # Max attestation age (supports d/h/m/s; minimum 1m). Default: "14d".
remote = "origin"                         # Default remote for refs/quai/records/* push and fetch. Must not start with "-".
auto_sign = false                         # Best-effort sign passing receipts in verify, pre-push, and land.
auto_push = false                         # Implies auto_sign; push signed attestation refs automatically.
auto_prune = false                        # Best-effort prune of expired state after verify or land. Default: false.
prune_interval = "24h"                    # Minimum time between automatic prunes. Supports d/h/m/s. Default: "24h".

[hooks.pre_push]
require_cached = ["verify"]   # Steps whose receipt must already exist before a branch push. Default: [].
on_missing = "warn"           # "warn" (default) | "refuse". "refuse" blocks git push without a receipt.
```

Automatic pruning reads these fields from the remote default branch (`refs/remotes/origin/HEAD`, resolved from `base` only when `base` is a plain branch name) rather than the working tree, so a pull request branch cannot widen a destructive maintenance pass. The manual `quai gc` command uses the working tree's configuration because the operator invoked it explicitly. `max_age` must be at least `1m`.

If `refs/remotes/origin/HEAD` is unset, which `git init` + `git remote add` + `git fetch` leaves unset, automatic pruning is skipped with a warning and `quai doctor` warns. Run `git remote set-head origin --auto` to resolve it.

Steps also accept an optional `signers = ["alice@example.com"]` array to restrict which principals may attest a specific step.

### Admission guards

`min_free_mem` and `min_free_disk` gate a pool before any slot is granted. `quai` reads Linux `MemAvailable` and the free space of the filesystems that hold the repository and the system temp directory, and a step waits in the pool queue while any value is below its threshold. When `lock_wait` (or `quai exec --wait`) expires first, the command exits `4` with a message naming the threshold that was not met. Hosts or filesystems that cannot report a value are treated as satisfying that guard.

## How input fingerprinting works

Each step computes a SHA-256 fingerprint before running on a clean tree:

- When `inputs = ["**"]` and `lockfiles` is empty, `quai` hashes `step.run`, `HEAD^{tree}`, and `merge_base` (when `{{base}}` appears in `run`).
- When narrower `inputs` or `lockfiles` are declared, `quai` lists all tracked files in the git tree matching those globs (`git ls-tree -r`), sorts them by path, and hashes `step.run` plus every `<path>:<blob_sha>`.

- An input that starts with `!` excludes the paths its glob matches. A path counts as an input when it matches at least one plain glob and no `!` glob, whatever their order, so `inputs = ["**", "!docs/evidence/**"]` keeps the receipt and the attestation of the step when only files under `docs/evidence/` change. An exclusion turns off the `HEAD^{tree}` shortcut above. `quai` rejects `inputs` made only of exclusions, a bare `!`, and `!` in `lockfiles`: a lockfile is hashed even when an input excludes it.

Narrowing `inputs` lets unrelated edits (such as editing `README.md` or another package) reuse existing receipts automatically across commits, rebases, and linked worktrees. Always list build manifests and lockfiles in `lockfiles` so dependency or toolchain updates invalidate cached receipts.

## Conditional pool routing

A step can reserve its heavy pool for the diffs that need it and skip the queue for the rest:

```toml
[[steps]]
name = "e2e"
run = "pnpm test:e2e"
pool = "heavy"
slots = 1
pool_when = ["apps/**", "packages/**"]
pool_tiers = ["full-live"]
```

- `pool_when` acquires the configured `pool` and `slots` when any changed path since the merge-base with the base ref matches one of its globs. Changed paths come from `git diff --name-status` between the merge-base and `HEAD` (both sides of a rename count), plus untracked files on a dirty tree.
- `pool_tiers` acquires them when the branch tier reported by `quai tier` is at least the lowest listed tier.
- Setting both is an OR: a matching path or a high enough tier acquires the pool.
- Otherwise `quai verify` runs the step immediately with `slots = 0`. It takes no slot and skips `min_free_mem`, but still writes a receipt on a clean passing tree and still signs attestations.
- `quai verify` prints `pool    <step>: routed to <pool> (...)` when the diff routes to the pool, then `pool    <step>: acquired <pool>` only once the slot lock is actually held; a bypass prints `pool    <step>: bypassed <pool> (...)`. `--json` adds `pool` and `pool_acquired` to that step, and `pool_acquired` is `true` only when the slot lock was held (a slot timeout reports `false`). Unconditional and cached steps report neither field.
- `pool_when` and `pool_tiers` require `pool` and `slots > 0`. Configs without them behave exactly as before.

## Step dependencies (`depends_on`)

`depends_on = ["fast"]` makes `fast` a prerequisite of the step. Every command that selects steps (`quai verify`, `quai verify --step`, `quai hooks pre-push`, `quai attest check --step`) adds the transitive prerequisites and puts each one before the steps that need it. A step runs once per invocation even when several steps depend on it. When a prerequisite fails or mutates the tree, `quai verify` stops before the dependent step runs, so the dependent step gets no receipt and no attestation. `quai` rejects an unknown step name, a name listed twice, and a cycle.

Each step runs with `QUAI_PASSED_STEPS` set to the comma-separated names of the steps that passed or were cached earlier in the same `quai verify` invocation, in run order (empty for the first step). A step script can skip its own call to a prerequisite when the name is listed:

```sh
case ",$QUAI_PASSED_STEPS," in
  *,fast,*) ;;
  *) quai verify --step fast ;;
esac
```

`quai` sets this variable and never reads it, so an inherited value cannot mark a step passed. On a clean tree, each listed step passed on the current tree under the mutation guard. On a dirty tree, no mutation guard runs, so the list only says the step passed earlier in this invocation.

## Required receipts at push time

`hooks.pre_push.require_cached` names steps whose receipt must already exist before a branch push, without running them in the hook:

```toml
[[steps]]
name = "fast"
run = "golangci-lint run"
hook = "pre-push"

[[steps]]
name = "verify"
run = "go test -race ./..."
inputs = ["**/*.go", "go.mod", "go.sum"]

[hooks.pre_push]
require_cached = ["verify"]
on_missing = "refuse"
```

For every `refs/heads/*` ref git hands the hook, `quai hooks pre-push` first runs the `hook = "pre-push"` steps as before, then checks each `require_cached` step against the pushed commit:

- Changed files between the step base (`merge-base` with `base`) and the pushed commit are matched against the step's `inputs` and `lockfiles`. When none match (a docs-only or `gate-only` change, or a branch with no changes at all), the step counts as satisfied without a receipt.
- Otherwise a passing receipt for that pushed tree and base must exist in the local receipt cache and be inside the 14-day retention window. The check reads receipts only: it never executes the step, takes no slot, and its local part stays under 100 ms.
- When a signed attestation for that receipt exists locally but not on `[attest].remote`, the hook pushes the `refs/quai/records/*` ref before returning so CI sees it when the workflow starts. That sync adds one `git ls-remote` round trip and a push to the remote, so a network remote can push the hook past 100 ms. A failed sync is reported and does not block the push. The sync pushes only `refs/quai/*`, so the pre-push hook it triggers returns at once (every ref is quai metadata) and cannot nest on any hook version. When stdin carries no refs, the hook pushes no attestations at all.
- `on_missing = "warn"` (default) prints the step, commit, ref, and remediation and allows the push. `on_missing = "refuse"` prints the same diagnostic and exits `1` to block the push.
- When stdin carries no refs (an outdated managed hook drops them), the hook reports that the pushed ref is unknown, checks HEAD for information only, and pushes no attestations. `warn` allows, `refuse` blocks even when HEAD is cached, and both name `quai hooks install` as the fix. The managed hook passes `--refs-from-stdin`, so a `git push` with nothing to send exits `0` without checking anything.
- Tag pushes, branch deletions, and `refs/quai/*` pushes are never gated; a `refs/quai/*` push (attestation records or verdicts) skips the hook entirely. The hook also strips `QUAI_ALLOW_UNVERIFIED_PUSH` and the legacy `QUAI_PREPUSH_ACTIVE` from its step children.
- `QUAI_ALLOW_UNVERIFIED_PUSH=1` downgrades `refuse` to a warning, still runs `[hooks.pre_push].steps`, and prints an auditable notice. `quai restack --push` sets it for its own push so a restack never deadlocks on an unverified rebased branch.

Disjoint-base matching across a moved base (issue #13) is not applied to the local pre-push check yet: a rebase onto a newer base needs a fresh `quai verify`.

## Template placeholders in `steps[].run`

`quai` substitutes three fixed placeholders before executing `step.run`:

| Placeholder | Value |
|---|---|
| `{{base}}` | Exact 40-char hex `merge-base` SHA against the target base ref |
| `{{head}}` | Exact 40-char hex `HEAD` commit SHA |
| `{{tree}}` | Exact 40-char hex `HEAD^{tree}` SHA |

Every placeholder value is validated against `^[0-9a-f]{40}$` before substitution.

## Default test globs for tamper detection

If `tiers.test_globs` is left empty, `quai tier` checks files matching:

- `*_test.*`
- `*.test.*`
- `*.spec.*`
- `*.e2e.*`
- `test_*.py`

When `escalate_on_test_tamper = true`, deleting any matching test file or introducing skip markers (`.skip(`, `it.skip`, `describe.skip`, `test.skip`, `t.Skip(`, `@pytest.mark.skip`, `#[ignore]`) escalates the branch to `full-live`.

## Starter presets

### Go repository

```toml
version = 1
base = "main"

[slots]
heavy = 2

[[steps]]
name = "vet"
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
full_live = ["cmd/**", "internal/auth/**", "internal/db/**"]
gate_only = ["*.md", "docs/**"]
test_globs = ["**/*_test.go"]
escalate_on_test_tamper = true

[land]
method = "squash"
optimistic_merge = true
required_checks = ["Vet, test, and build"]
global_paths = ["go.mod", "go.sum", ".quai.toml", ".github/**"]
```

### TypeScript / Node monorepo

```toml
version = 1
base = "main"

[slots]
heavy = 2

[[steps]]
name = "fast"
run = "pnpm lint && pnpm typecheck"
inputs = ["apps/**", "packages/**", "tsconfig*.json"]
lockfiles = ["pnpm-lock.yaml", "package.json"]
hook = "pre-push"
slots = 0

[[steps]]
name = "test"
run = "pnpm test --changed --since {{base}}"
inputs = ["apps/**", "packages/**"]
lockfiles = ["pnpm-lock.yaml", "package.json"]
pool = "heavy"
slots = 1
timeout = "20m"

[tiers]
default = "diff-audit"
full_live = ["apps/web/src/**", "packages/protocol/**"]
gate_only = ["*.md", "docs/**"]
test_globs = ["**/*.test.ts", "**/*.spec.ts", "**/*.e2e.ts"]
escalate_on_test_tamper = true

[land]
method = "squash"
optimistic_merge = true
required_checks = ["check", "test"]
global_paths = ["package.json", "pnpm-lock.yaml", "pnpm-workspace.yaml", "tsconfig.base.json", ".github/**"]
```

### Rust repository

```toml
version = 1
base = "main"

[slots]
heavy = 1

[[steps]]
name = "clippy"
run = "cargo clippy --all-targets -- -D warnings"
inputs = ["**/*.rs", "Cargo.toml", "Cargo.lock"]
lockfiles = ["Cargo.lock"]
hook = "pre-push"
slots = 0

[[steps]]
name = "test"
run = "cargo test"
inputs = ["**/*.rs", "Cargo.toml", "Cargo.lock"]
lockfiles = ["Cargo.lock"]
pool = "heavy"
slots = 1
timeout = "15m"

[tiers]
default = "diff-audit"
full_live = ["src/bin/**", "src/net/**"]
gate_only = ["*.md", "docs/**"]
escalate_on_test_tamper = true

[land]
method = "squash"
optimistic_merge = true
required_checks = ["ci"]
global_paths = ["Cargo.toml", "Cargo.lock", ".github/**"]
```

### Python repository

```toml
version = 1
base = "main"

[slots]
heavy = 2

[[steps]]
name = "lint"
run = "ruff check . && mypy src"
inputs = ["src/**/*.py", "tests/**/*.py", "pyproject.toml"]
lockfiles = ["uv.lock", "pyproject.toml"]
hook = "pre-push"
slots = 0

[[steps]]
name = "test"
run = "pytest"
inputs = ["src/**/*.py", "tests/**/*.py", "pyproject.toml"]
lockfiles = ["uv.lock", "pyproject.toml"]
pool = "heavy"
slots = 1
timeout = "10m"

[tiers]
default = "diff-audit"
full_live = ["src/api/**", "src/migrations/**"]
gate_only = ["*.md", "docs/**"]
test_globs = ["tests/**/test_*.py", "**/*_test.py"]
escalate_on_test_tamper = true

[land]
method = "squash"
optimistic_merge = true
required_checks = ["test"]
global_paths = ["pyproject.toml", "uv.lock", ".github/**"]
```
