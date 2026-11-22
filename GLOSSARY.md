# Glossary

The domain words of `quai`. Code, flags, JSON fields, docs, commits, and issues use these words with these meanings. A term lists the synonyms to avoid, because one word per concept keeps search and review exact.

## Verification

**Step**
A named command declared under `[[steps]]` in `.quai.toml`, with its `inputs`, `lockfiles`, `pool`, and `slots`. The unit that `quai verify` runs and caches.
_Avoid_: check, job, task.

**Inputs**
The globs whose tracked blobs a step reads. A change outside a step's inputs leaves its fingerprint, and so its receipt, valid.

**Lockfiles**
Files hashed into every fingerprint of a step whatever its inputs, such as `go.sum` or `mise.toml`, because a toolchain or dependency change can change any result.

**Fingerprint**
The sha256 of a step's `run` string plus the sorted `<path>:<blob_sha>` entries of its inputs and lockfiles (plus `base_sha` when `{{base}}` is used). Two trees with the same fingerprint verify the same way for that step.
_Avoid_: hash, cache key.

**Receipt**
The local JSON record, at `receipts/<step>-<fingerprint>.json`, that a step passed on a clean tree with that fingerprint. A receipt is never written for a failed, dirty, or mutating run.
_Avoid_: cache entry, result, proof.

**Cached**
The `status` of a step whose receipt already exists, so it does not run.

**Mutation guard**
The check that a passing step left `HEAD` and `git status --porcelain` unchanged. A step that edits files or moves `HEAD` reports `mutated` and gets no receipt.

**In-flight lock**
The `inflight-<step>-<fingerprint>.lock` that makes concurrent `quai verify` calls on the same fingerprint share one run.

**Attestation**
A receipt signed with SSH (`ssh-keygen -Y`, namespace `quai`) inside an in-toto v1 statement and a DSSE v1 envelope, stored under `refs/quai/records/<step>-<fingerprint>`. A receipt is local trust; an attestation is trust CI can check.
_Avoid_: signed receipt, record (except in the ref name).

**Allowed signers**
The OpenSSH `allowed_signers` file that decides which keys `quai attest check` and `quai verdict check` accept. CI passes it from a trusted source so a PR cannot widen its own trust root.

**Detached run**
A `quai verify` or `quai exec` started with `--detach`, running in the background under `runs/<run-id>/` with its own log and read cursor. `quai verify --wait` streams it, `quai verify --stop` ends it. A detached run gone without an exit record is `dead`.
_Avoid_: job, background task, daemon.

**Disjoint-base match**
The `match_mode` of a step that `quai attest check --head` accepted through the PR head's attestation on its old merge base, because the base moved only by changes an optimistic merge would accept. Its opposite is an `exact` match on the checked-out tree's own fingerprint.

**Sampled**
The `status` that `quai attest check --sample` gives every step on a sampled run, telling CI to rerun the steps itself as an audit.

**Garbage collection (`gc`)**
The maintenance pass that removes expired receipts, expired attestation and verdict refs, and stale lock directories. `quai gc` runs it on demand; `[attest].auto_prune` runs it after `quai verify` or `quai land` at most once per `prune_interval`, using the policy on the remote default branch.
_Avoid_: cleanup, vacuum.

**Last prune stamp**
The `<git-common-dir>/quai/last-prune` timestamp written atomically after a maintenance pass. A fresh stamp skips automatic pruning until `prune_interval` elapses.

**Miss reason**
The normalized `reason` that `quai attest check` gives every step: `verified`, `no_record`, `base_mismatch`, `tree_mismatch`, `config_mismatch`, `signature_invalid`, `expired`, or `sampled`. It says why a fallback to a full run is needed without reading raw git refs.
_Avoid_: error, failure code.

## Concurrency

**Pool**
A named capacity under `[slots]`, shared by every worktree of the repository. A pool may set `min_free_mem`.
_Avoid_: queue, semaphore (in user-facing text).

**Conditional pool**
A step whose `pool_when` globs or `pool_tiers` threshold decide from the branch diff whether it acquires its configured pool, or bypasses it and runs with `slots = 0`.
_Avoid_: smart pool, adaptive pool.

**Slot**
One unit of a pool, held as a `slot-<pool>-<index>.lock` directory. A step with `slots = 0` takes none.

**Lease**
The `owner` file of a held lock: PID, host, PID namespace, token, and a heartbeat. A lease whose heartbeat goes stale, or whose PID is dead on the same host and namespace, can be taken over.
_Avoid_: lock owner, pidfile.

**Takeover**
Replacing the lease of a dead holder, guarded by `<lock-dir>.takeover` so two waiters never both win.

**Queue**
The ticket directory beside a lock (`land.lock.queue/`, `slot-<pool>.queue/`) that serves waiters by effective priority, then in arrival order.

**Priority**
The integer on a queue ticket and a lease, default `0`, higher served first. `quai land` uses `10`; `--priority` sets it for `quai verify` and `quai exec`.

**Aging boost**
The points a waiter gains while queued, one per 30 seconds, capped at 10. Priority plus aging boost is the waiter's effective priority, so a low-priority waiter never starves.

**Role**
The command a lease or ticket belongs to: `land`, `verify`, or `exec`. Shown by `quai status`.

**Land lock**
`land.lock`, the repository-wide lock that serializes rebase-and-merge landings.

## Review

**Tier**
The review depth a diff needs: `gate-only` < `diff-audit` < `full-live`. A branch's tier is the highest tier among its changed files.
_Avoid_: risk level, severity.

**Test tamper**
A diff that deletes a test file or adds a skip marker to one. With `escalate_on_test_tamper`, it raises the tier to `full-live`.

**Patch-id**
The stable `git patch-id` of a diff, independent of its base and line numbers.

**Survives / changed**
The two outcomes of `quai patch-id`: the rebased diff is identical (`SURVIVES`), or files drifted (`CHANGED`, with `changed_files`). A review of a surviving patch still applies.
_Avoid_: same, different, stale.

**Verdict**
A reviewer's `approved` or `rejected` decision keyed by patch-id and tier, stored under `refs/quai/verdicts/<patch-id>`, optionally signed. It survives clean rebases and lapses when the patch changes.
_Avoid_: approval, review result.

## Landing

**Land**
Merge a PR through `quai land`, which picks one action: `refuse`, `direct-merge`, `optimistic-merge`, or `rebase-and-merge`. "Merge" alone means the forge's merge call that landing ends with.
_Avoid_: ship, merge (for the whole flow).

**Optimistic merge**
Landing without a rebase when `origin/<base>` moved but `git merge-tree --write-tree` is clean and the base's changes touch neither the PR's files nor the global paths.

**Batch**
Several PRs given to one `quai land` call and landed one after another under one `land.lock` session, disjoint PRs first. Each PR still gets its own merge; a batch never merges PRs together and never bisects.
_Avoid_: train, queue (for the batch itself).

**Global paths**
The globs under `[land].global_paths` (`go.mod`, `.quai.toml`, `.github/**`…) whose change on the base disqualifies an optimistic merge, because they can affect every step.

**Stacked PR**
A PR whose base branch is another PR's branch. Landing the parent retargets its children to the parent's base.

**Restack**
Replaying a child branch's own commits onto the updated base with `git rebase --onto`, then checking patch-id survival. `quai restack`, or `quai land --restack`.

**Land worktree**
The temporary detached worktree, `land-worktree-<pr>-<pid>` under the common dir, in which `quai land` rebases and verifies a PR whose branch the caller does not have checked out. Removed when the run ends.
_Avoid_: scratch worktree, temp checkout.

**Transient error**
A `gh` failure from the network rather than from GitHub's judgment: TLS handshake timeout, EOF, connection reset, i/o timeout, or HTTP 5xx. `quai land` retries it; a refusal (HTTP 4xx, conflict, failing check) it never retries.

**Conflicts**
What `quai conflicts` reports: virtual merge conflicts and file overlaps between active worktrees, or between a worktree and the base, found before anyone lands.

## Places

**Common dir**
`<git-common-dir>/quai/`, the state shared by the main checkout and every linked worktree: locks, queues, receipts, detached runs, and land worktrees.

**Forge**
The hosting service behind PRs and checks. Today only GitHub, reached through `gh`.
