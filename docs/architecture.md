# Architecture

`quai` is a single static Go binary with one external dependency (`github.com/BurntSushi/toml`). It shells out to `git` (2.38+) and `gh` through explicit argument slices (`exec.CommandContext`) with `LC_ALL=C` forced for deterministic parsing across locales.

## One feature per folder

Every feature owns a dedicated package under `internal/<feature>/`. `cmd/quai/` handles CLI flag parsing, wiring, formatting, and exit-code mapping.

```text
quai/
├── cmd/quai/          # CLI entrypoint, subcommand routing, flag parsing, exit codes
├── internal/
│   ├── attest/        # in-toto v1 / DSSE v1 envelopes, ssh-keygen -Y (namespace quai), refs/quai/records/*, --onto and --head (disjoint-base) checks
│   ├── config/        # .quai.toml decoding, defaults, validation, SHA placeholder expansion
│   ├── conflicts/     # read-only merge-tree and file-overlap checks across linked worktrees
│   ├── glob/          # double-star (**) path glob matcher shared by config, receipt, tier, land
│   ├── gc/            # unified maintenance: expired receipts/attestation/verdict refs, stale locks, last-prune stamp
│   ├── repo/          # Git interface and ExecGit adapter (diff, ls-tree, merge-tree, patch-id, rebase)
│   ├── lock/          # mkdir locks, leases with heartbeats, priority queues, slot pools, min_free_mem
│   ├── execrun/       # process-group command runner (Setpgid) with signal cleanup and slot binding
│   ├── receipt/       # input fingerprinting, atomic JSON writes, lookup, retention pruning
│   ├── verify/        # step orchestration, conditional pool routing, cache lookup, mutation guard
│   ├── detach/        # detached runs: start, wait with a read cursor, stop, dead-run cleanup, pruning
│   ├── tier/          # pure diff risk classifier, R100 rename handling, test-tamper scanner
│   ├── patchid/       # pure patch-id survival comparator and per-file drift detector
│   ├── hooks/         # non-destructive pre-push installer and receipt fast path
│   ├── status/        # read-only quai status inspection and quai doctor readiness checks
│   ├── verdict/       # review verdicts keyed by patch-id under refs/quai/verdicts/<patch-id>
│   ├── forge/         # Forge interface, gh CLI implementation, transient-error retry
│   └── land/          # pure DecideLand state machine and PlanBatch planner, locked landing executor, batch landing, stacked PR restack
```

## Pure engines vs side-effect adapters

`quai` separates deterministic decision logic from filesystem, process, git, and network side effects:

- **Pure decision engines:** `internal/tier` (classification + tamper detection), `internal/patchid` (diff survival), and `internal/land` (`DecideLand`, `PlanBatch`) take plain structs and return plain decisions. Unit tests cover every branch and edge case in milliseconds without spawning `gh` or touching a network.
- **Side-effect adapters:** `internal/repo` (`repo.Git` interface + `ExecGit`) and `internal/forge` (`forge.Forge` interface + `GHForge`) isolate external binaries behind narrow interfaces so integration tests can use either real temporary git repositories or in-memory fakes.

## Core data shapes

The JSON field names below are part of the `--json` and on-disk contract. Renaming one is a breaking change.

```go
// internal/config
type Tier string // "gate-only" | "diff-audit" | "full-live"

// internal/receipt: <git-common-dir>/quai/receipts/<step>-<fingerprint>.json
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

// internal/land
type Action string // "refuse" | "direct-merge" | "optimistic-merge" | "rebase-and-merge"

// internal/verdict: refs/quai/verdicts/<patch-id>
type Status string // "approved" | "rejected"
```

Attestations wrap a `Receipt` in an in-toto v1 statement inside a DSSE v1 envelope, signed with `ssh-keygen -Y sign` (namespace `quai`) and stored under `refs/quai/records/<step>-<fingerprint>`.

## Shared state on disk

All mutable state lives inside `<git-common-dir>/quai/` (resolved via `git rev-parse --git-common-dir`) so main checkouts and linked worktrees share locks and receipts:

```text
<git-common-dir>/quai/
├── locks/
│   ├── land.lock/
│   │   └── owner
│   ├── prune.lock/               # serializes manual and automatic maintenance passes
│   ├── land.lock.queue/          # waiter tickets: <enqueue-unix-nanos>-<token>.ticket
│   ├── slot-<pool>-<index>.lock/
│   │   └── owner
│   ├── slot-<pool>.queue/
│   └── inflight-<step>-<fingerprint>.lock/   # dedupes concurrent identical verify runs
├── receipts/
│   └── <step>-<fingerprint>.json
├── verdicts/
│   └── <patch-id>.json
├── runs/
│   └── <run-id>/                 # one detached run; the 20 latest finished runs are kept
│       ├── meta.json             # args, worktree, pid + kernel start time, host, pid_ns
│       ├── output.log            # stdout and stderr of the run
│       ├── cursor                # byte offset read by the previous quai verify --wait
│       ├── groups                # pid + start time of each step's process group
│       └── exit.json             # exit code, stopped flag, verify result; absent while running or dead
├── land-worktree-<pr>-<pid>/     # temporary detached worktree of a quai land run off the PR branch
└── last-prune                    # RFC3339 stamp that rate-limits automatic pruning
```

### Maintenance pass (`internal/gc`)

`quai gc` and the automatic pass after `quai verify` / `quai land` share one engine:

1. Expired receipts, attestation refs, verdicts, and verdict refs are selected from `[attest].max_age`. A dry run lists them and never fetches or deletes.
2. Record refs are pruned locally and on `[attest].remote`; a fetch or push failure is a warning, not an error.
3. A lock directory is removed only when its lease is dead or its owner file has been absent past the grace period, under the lock's `.takeover` guard, so a live holder is never deleted.
4. Automatic pruning is best effort and rate-limited: after a pass, `last-prune` is written atomically, and a fresh stamp skips the next pass until `prune_interval` elapses. `prune.lock` serializes concurrent passes across worktrees, and the automatic policy is read only from `refs/remotes/origin/HEAD`; when that ref is unset the pass is skipped and the warning names `git remote set-head origin --auto`.

### Atomic lock and dead-PID recovery (`internal/lock`)

1. When the lock is held or others already wait, a waiter writes a ticket into the lock's queue directory (`land.lock.queue/`, `slot-<pool>.queue/`), and only the first live ticket in service order may try to acquire. Service order is `(priority + aging boost) DESC, enqueue time ASC`, where the aging boost is one point per 30 seconds waited, capped at 10 (`lock.EffectivePriority`). `quai land` queues at priority 10, everything else at 0 unless `--priority` says otherwise. Equal priorities stay FIFO, and the boost lets a priority 0 waiter catch up with a landing, so waiters never starve.
2. Acquisition creates `<lock-dir>` with `os.Mkdir`, which is atomic on POSIX filesystems.
3. The holder writes `<lock-dir>/owner` atomically via temp file + rename. The owner is a lease: `{pid, host, pid_ns, token, role, priority, command, worktree, acquired_at, heartbeat_at}`, where `role` is `land`, `verify`, or `exec`, with `heartbeat_at` refreshed every 5 seconds.
4. A holder is dead when its heartbeat is older than 30 seconds, or, on the same host and Linux PID namespace, when `kill(pid, 0)` returns `ESRCH`. A PID from another host or namespace is never probed, so a container never steals a live lock.
5. To take over a dead holder, the waiter acquires `<lock-dir>.takeover`, re-reads `owner` to confirm the same dead lease, atomically replaces `owner` with its own, and removes `.takeover`.
6. If `owner` is absent for over 10 seconds, the waiter emits a warning naming the lock directory without breaking it.
7. A slot pool with `min_free_mem` grants a slot only while Linux `MemAvailable` stays above that threshold.

### Process-group isolation (`internal/execrun`)

Every child command runs in its own process group (`SysProcAttr{Setpgid: true}`). On `SIGINT`, `SIGTERM`, `SIGHUP`, or step timeout, `quai` sends `SIGTERM` to `-pgid`, waits up to 5 seconds, then sends `SIGKILL` to `-pgid` before releasing any held locks.

### Detached runs (`internal/detach`)

1. `--detach` re-executes the same `quai` binary with the same flags in a new session (`Setsid`), with `QUAI_RUN_ID=<run-id>` in its environment. That process unsets the variable, runs the command in-process, appends each step's process group (PID and kernel start time from `/proc/<pid>/stat`) to `groups`, and writes `exit.json` atomically when the command returns, including after `SIGTERM`.
2. A process is identified by PID plus start time, so a reused PID never passes for the run, and a step group whose PID now names another process is never signalled.
3. A run is `passed` or `failed` once `exit.json` exists, `running` while its process lives (or when it belongs to another host or PID namespace), and `dead` otherwise. Cleaning a dead run kills its live step groups, then removes the locks and queue tickets whose lease names its PID on this host and namespace, under the same `.takeover` guard as a takeover, and only while no process has that PID.
4. `--stop` sends `SIGTERM` to the run's group and gives it 10 seconds, longer than the 5 seconds a step gets, so the run can kill its own steps and release its own locks first.

### Receipts and mutation guard (`internal/receipt`, `internal/verify`)

1. Fingerprints hash `step.run` plus sorted `<path>:<blob_sha>` entries from `git ls-tree -r` matching `inputs` and `lockfiles` (plus `base_sha` when `{{base}}` is used and affected).
2. Before running a step, `verify` snapshots `HEAD` and `git status --porcelain`.
3. After a passing step on a clean tree, `verify` re-checks `HEAD` and `git status --porcelain`. Any modification, untracked non-ignored file, or `HEAD` move aborts the receipt write and exits `1`.
4. Valid receipts are written atomically via temp file + rename. After each write, `receipt` prunes entries older than 14 days and retains at most 200 files.

## Testing layers

- **Package unit tests (`go test -race ./...`):** Pure table-driven tests for config, glob, tier, patchid, and `DecideLand`; concurrency and `SIGKILL` takeover tests for `lock`; real temporary git repo integration tests for `repo`, `receipt`, `verify`, `hooks`, and `land`.
- **Isolated CLI harness (`.agents/skills/verify-quai/`):** Builds the real `quai` binary into `/tmp/quai-verify-<run-id>/bin/quai`, initializes a bare remote plus repo and linked worktree, drives every CLI subcommand end-to-end, and persists full transcripts as evidence.
