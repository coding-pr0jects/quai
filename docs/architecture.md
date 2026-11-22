# Architecture

`quai` is a single static Go binary with one external dependency (`github.com/BurntSushi/toml`). It shells out to `git` (2.38+) and `gh` through explicit argument slices (`exec.CommandContext`) with `LC_ALL=C` forced for deterministic parsing across locales.

## One feature per folder

Every feature owns a dedicated package under `internal/<feature>/`. `cmd/quai/` handles CLI flag parsing, wiring, formatting, and exit-code mapping.

```text
quai/
├── cmd/quai/          # CLI entrypoint, subcommand routing, flag parsing, exit codes
├── internal/
│   ├── attest/        # in-toto v1 / DSSE v1 envelopes, ssh-keygen -Y (namespace quai), refs/quai/records/*, --onto check
│   ├── config/        # .quai.toml decoding, defaults, validation, SHA placeholder expansion
│   ├── glob/          # double-star (**) path glob matcher shared by config, receipt, tier, land
│   ├── repo/          # Git interface and ExecGit adapter (diff, ls-tree, merge-tree, patch-id, rebase)
│   ├── lock/          # atomic POSIX mkdir locks, dead-PID .takeover, named slot semaphores
│   ├── execrun/       # process-group command runner (Setpgid) with signal cleanup and slot binding
│   ├── receipt/       # input fingerprinting, atomic JSON writes, lookup, retention pruning
│   ├── verify/        # step orchestration, cache lookup, mutation guard
│   ├── tier/          # pure diff risk classifier, R100 rename handling, test-tamper scanner
│   ├── patchid/       # pure patch-id survival comparator and per-file drift detector
│   ├── hooks/         # non-destructive pre-push installer and receipt fast path
│   ├── forge/         # Forge interface and gh CLI implementation
│   └── land/          # pure DecideLand state machine and locked landing executor
```

## Pure engines vs side-effect adapters

`quai` separates deterministic decision logic from filesystem, process, git, and network side effects:

- **Pure decision engines:** `internal/tier` (classification + tamper detection), `internal/patchid` (diff survival), and `internal/land` (`DecideLand`) take plain structs and return plain decisions. Unit tests cover every branch and edge case in milliseconds without spawning `gh` or touching a network.
- **Side-effect adapters:** `internal/repo` (`repo.Git` interface + `ExecGit`) and `internal/forge` (`forge.Forge` interface + `GHForge`) isolate external binaries behind narrow interfaces so integration tests can use either real temporary git repositories or in-memory fakes.

## Shared state on disk

All mutable state lives inside `<git-common-dir>/quai/` (resolved via `git rev-parse --git-common-dir`) so main checkouts and linked worktrees share locks and receipts:

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

### Atomic lock and dead-PID recovery (`internal/lock`)

1. Acquisition creates `<lock-dir>` with `os.Mkdir`, which is atomic on POSIX filesystems.
2. The holder writes `<lock-dir>/owner` atomically via temp file + rename containing `{pid, command, worktree, acquired_at}`.
3. If `os.Mkdir` returns `EEXIST`, a waiter reads `owner` and probes `kill(pid, 0)`.
4. If the holder PID is dead (`ESRCH`), the waiter acquires `<lock-dir>.takeover`, re-reads `owner` to confirm the same dead PID, atomically replaces `owner` with its own metadata, and removes `.takeover`.
5. If `owner` is absent for over 10 seconds, the waiter emits a warning naming the lock directory without breaking it.

### Process-group isolation (`internal/execrun`)

Every child command runs in its own process group (`SysProcAttr{Setpgid: true}`). On `SIGINT`, `SIGTERM`, `SIGHUP`, or step timeout, `quai` sends `SIGTERM` to `-pgid`, waits up to 5 seconds, then sends `SIGKILL` to `-pgid` before releasing any held locks.

### Receipts and mutation guard (`internal/receipt`, `internal/verify`)

1. Fingerprints hash `step.run` plus sorted `<path>:<blob_sha>` entries from `git ls-tree -r` matching `inputs` and `lockfiles` (plus `base_sha` when `{{base}}` is used and affected).
2. Before running a step, `verify` snapshots `HEAD` and `git status --porcelain`.
3. After a passing step on a clean tree, `verify` re-checks `HEAD` and `git status --porcelain`. Any modification, untracked non-ignored file, or `HEAD` move aborts the receipt write and exits `1`.
4. Valid receipts are written atomically via temp file + rename. After each write, `receipt` prunes entries older than 14 days and retains at most 200 files.

## Testing layers

- **Package unit tests (`go test -race ./...`):** Pure table-driven tests for config, glob, tier, patchid, and `DecideLand`; concurrency and `SIGKILL` takeover tests for `lock`; real temporary git repo integration tests for `repo`, `receipt`, `verify`, `hooks`, and `land`.
- **Isolated CLI harness (`.agents/skills/verify-quai/`):** Builds the real `quai` binary into `/tmp/quai-verify-<run-id>/bin/quai`, initializes a bare remote plus repo and linked worktree, drives every CLI subcommand end-to-end, and persists full transcripts as evidence.
