# Slots and exec

`quai exec` exposes the repository's shared CPU slot semaphore so any heavy command outside `quai verify` runs with bounded concurrency across worktrees, propagates its exit code, and cleans up child process groups on signal.

## Sub-features

- `exec-basic`: acquires a slot in `--pool`, runs the command directly, prints output, and releases the slot.
- `exec-exit-code`: returns the exact exit code of the wrapped command.
- `exec-timeout`: returns exit code `4` when all slots in the pool are held past `--wait`.
- `exec-dead-pid`: takes over a stale slot whose owner PID no longer exists.
- `exec-priority`: a waiter with a higher `--priority` gets the slots before an older waiter of lower priority; `quai status` lists both with their role and priority in service order.

## How to get to it (user POV)

- Run `quai exec --pool <name> [--slots <n>] [--wait <duration>] [--priority <int>] -- <command...>` from inside a repository with `[slots]` configured in `.quai.toml`.

## Driving it with quai-verify

Preconditions: `quai-verify launch` and `quai-verify doctor` succeeded.

- **Basic execution (`exec-basic`).** Run `quai-verify run exec-basic -- quai exec --pool heavy -- sh -c 'echo hello'`. Observe exit code `0`, stdout `hello`, and verify `<git-common-dir>/quai/locks/` has no remaining `slot-heavy-*.lock` directory afterward.
- **Exit code propagation (`exec-exit-code`).** Run `quai-verify run exec-exit -- quai exec --pool heavy -- sh -c 'exit 7'`. Observe exit code `7`.
- **Slot timeout (`exec-timeout`).** Acquire both slots of `heavy` in background subshells (`sleep 3`), then run `quai-verify run exec-timeout -- quai exec --pool heavy --wait 100ms -- true`. Observe exit code `4`. Wait for the background subshells to finish and confirm no lock directory remains.
- **Priority order (`exec-priority`).** In the background, hold both slots with `quai exec --pool heavy --slots 2 -- sleep 3`, then queue waiter A (`--slots 2`, default priority) and, once its ticket is in `slot-heavy.queue/`, waiter B (`--slots 2 --priority 10`), each appending its name to a file outside the repository. Run `quai-verify run exec-priority-status -- quai status --json` while both wait: the waiters list B (`priority: 10`) before A (`priority: 0`). After all three finish, the file reads B then A and `locks/` is empty.

## Gotchas

- Always pass `--` before the wrapped command when the command itself takes flags, so `quai exec` does not parse them.
- Never leave background `sleep` processes running after `exec-timeout`; record their PIDs and `wait` on them before cleanup.
