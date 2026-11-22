# Configuration (`.quai.toml`)

Every repository adopts `quai` by committing a `.quai.toml` file at the repository root. Unknown keys are rejected strictly with exit code `2` so typos fail immediately.

## Full schema

```toml
version = 1                     # Required. Must be 1.
base = "main"                   # Default base branch. Defaults to "main".

[slots]
# Named concurrency pools shared across all linked worktrees of the repo.
# Pool names must match ^[a-z0-9-]+$ and values must be positive integers.
heavy = 2

[[steps]]
name = "fast"                   # Required, unique, matches ^[a-z0-9-]+$.
run = "go vet ./..."            # Required. Executed via sh -c in repo root.
inputs = ["**"]                 # Optional globs hashed into the receipt. Default: ["**"].
lockfiles = []                  # Optional extra files always hashed into the receipt.
hook = "pre-push"               # Optional: "pre-push" or empty.
pool = ""                       # Optional: pool name from [slots].
slots = 0                       # Optional: number of slots to hold (>= 0). 0 means unlocked.
timeout = "30m"                 # Optional Go duration string. Default: "30m".

[tiers]
default = "diff-audit"          # "gate-only" | "diff-audit" | "full-live". Default: "diff-audit".
full_live = []                  # Globs that escalate the branch to full-live.
diff_audit = []                 # Globs that escalate the branch to diff-audit.
gate_only = []                  # Globs allowed to stay gate-only.
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

[attest]
allowed_signers = ".quai/allowed_signers" # OpenSSH allowed_signers file for verifying CI attestations.
signing_key = ""                          # Optional default SSH private key path (or QUAI_SIGNING_KEY / git user.signingkey).
signers = []                              # Optional principal allowlist (empty = any key in allowed_signers).
max_age = "14d"                           # Max attestation age (supports d/h/m/s). Default: "14d".
remote = "origin"                         # Default remote for refs/quai/records/* push and fetch.
auto_sign = false                         # Best-effort sign passing receipts in verify, pre-push, and land.
auto_push = false                         # Implies auto_sign; push signed attestation refs automatically.
```

Steps also accept an optional `signers = ["alice@example.com"]` array to restrict which principals may attest a specific step.

## How input fingerprinting works

Each step computes a SHA-256 fingerprint before running on a clean tree:

- When `inputs = ["**"]` and `lockfiles` is empty, `quai` hashes `step.run`, `HEAD^{tree}`, and `merge_base` (when `{{base}}` appears in `run`).
- When narrower `inputs` or `lockfiles` are declared, `quai` lists all tracked files in the git tree matching those globs (`git ls-tree -r`), sorts them by path, and hashes `step.run` plus every `<path>:<blob_sha>`.

Narrowing `inputs` lets unrelated edits (such as editing `README.md` or another package) reuse existing receipts automatically across commits, rebases, and linked worktrees. Always list build manifests and lockfiles in `lockfiles` so dependency or toolchain updates invalidate cached receipts.

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
