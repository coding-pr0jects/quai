# Detached runs

`quai verify --detach` and `quai exec --detach` start the command in the background as a detached run under `<git-common-dir>/quai/runs/<run-id>/`. `quai verify --wait` streams its new output and maps its state to an exit code, and `quai verify --stop` ends it with its steps and locks.

## Sub-features

- `detach-pass`: `--detach` exits `0` at once; `--wait` exits `4` while the run runs, then `0` once it passed, and the run's receipts are cached for a foreground `quai verify`.
- `detach-cursor`: a second `--wait` on the same run prints no repeated output.
- `detach-fail`: a failing step makes `--wait` exit `1` at once with a summary naming the step, long before `--max`.
- `detach-kill`: after `kill -9` of the run's PID, `quai status` lists it `dead`, and `--wait` exits `1`, kills the step's process group, and releases its slot and in-flight locks.
- `detach-stop`: `--stop` exits `0`, the step process is gone, and no lock remains.
- `detach-worktrees`: runs started at the same time in the repo and the linked worktree keep separate logs; `--wait` without a run ID follows each worktree's own run.
- `detach-exec`: `quai exec --detach` records the wrapped command's exit code.

## How to get to it (user POV)

- `quai verify [flags...] --detach [--json]`, then `quai verify --wait [<run-id>] [--max <duration>] [--json]` until it exits other than `4`, or `quai verify --stop [<run-id>]`.

## Driving it with quai-verify

Preconditions: `quai-verify launch` and `quai-verify doctor` succeeded. Give the scratch repo a slow step first, committed so the tree stays clean: set the `verify` step's `run` to `sleep 300 & echo $! > /tmp/quai-verify-$QUAI_VERIFY_RUN_ID/step.pid; wait` (for `detach-kill` and `detach-stop`) or `sleep 3; echo done-$(basename $PWD)` (for `detach-pass` and `detach-worktrees`).

- **`detach-pass`.** `run detach-pass-start -- quai verify --detach --json`, `run detach-pass-wait0 -- quai verify --wait --max 0` (exit `4`), `run detach-pass-wait -- quai verify --wait --max 60s` (exit `0`), then `run detach-pass-cached -- quai verify --json` shows both steps `cached`.
- **`detach-cursor`.** `run detach-cursor -- quai verify --wait --json` prints `"output": ""`.
- **`detach-kill`.** Start a run, wait for `step.pid`, `kill -9` the run's `pid`, then `run detach-kill-status -- quai status --json` (`"state": "dead"`) and `run detach-kill-wait -- quai verify --wait --json` (exit `1`, `killed_groups` and `released_locks` set). Check with `ps -p $(cat step.pid)` and `ls .git/quai/locks` that nothing is left.
- **`detach-stop`.** Start a run, wait for `step.pid`, `run detach-stop -- quai verify --stop --json` (exit `0`, `"stopped": true`), then the same `ps` and `ls` checks.
- **`detach-worktrees`.** Start a run in the repo and one with `run-wt` within the same second, then `--wait --max 60s` in each; each output holds only its own `done-<dir>` line.
- **`detach-exec`.** `run detach-exec -- quai exec --detach --pool heavy -- sh -c 'exit 7'`, then `run detach-exec-wait -- quai verify --wait --json` (exit `1`, `"exit_code": 7`).

## Gotchas

- The run's process outlives the harness's `run` call by design; record its `pid` from the `--json` output and make sure it is gone before `cleanup`.
- Two worktrees with the same tree share one execution through the in-flight lock, so give the worktree its own commit before `detach-worktrees`.
