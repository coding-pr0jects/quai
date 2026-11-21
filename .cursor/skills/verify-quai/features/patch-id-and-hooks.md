# Patch-id and hooks

`quai patch-id` proves whether a verified branch patch survived a rebase unchanged, and `quai hooks` installs and executes a receipt-aware `pre-push` hook without overwriting existing custom hooks.

## Sub-features

- `patchid-survives`: after a clean rebase onto an advanced `main`, `quai patch-id <old> HEAD --json` returns `SURVIVES` and exits `0`.
- `patchid-changed`: modifying a file after rebase returns `CHANGED`, lists the drifted file in `changed_files`, and exits `1`.
- `hooks-install-uninstall`: installs a `# managed by quai` `pre-push` hook and cleanly removes it on `uninstall`.
- `hooks-non-destructive`: refuses to overwrite an existing unmanaged hook or custom `core.hooksPath` and prints a chain snippet.
- `hooks-pre-push-cache`: `quai hooks pre-push` reuses a valid receipt from `quai verify` and exits `0` with `"cached"`.

## How to get to it (user POV)

- Run `quai patch-id <verdict-sha> [target-sha] [--base <ref>] [--json]`.
- Run `quai hooks install`, `quai hooks uninstall`, or `quai hooks pre-push [--json]`.

## Driving it with quai-verify

Preconditions: `quai-verify launch` and `quai-verify doctor` succeeded.

- **Patch survival (`patchid-survives`).** Commit a change to `app/main.txt` on `feat/wt` and record `OLD_SHA`. Commit an unrelated change to `docs/guide.md` on `main`, push `main` to `origin`, rebase `feat/wt` onto `origin/main`, and run `quai-verify run-wt patchid-ok -- quai patch-id "$OLD_SHA" HEAD --json`. Observe exit code `0` and `"status": "SURVIVES"`.
- **Patch drift (`patchid-changed`).** Add one more line to `app/main.txt` on `feat/wt`, commit, and run `quai-verify run-wt patchid-drift -- quai patch-id "$OLD_SHA" HEAD --json`. Observe exit code `1`, `"status": "CHANGED"`, and `"changed_files": ["app/main.txt"]`.
- **Hooks lifecycle (`hooks-install-uninstall` and `hooks-pre-push-cache`).** Run `quai-verify run hooks-install -- quai hooks install --json`, then `quai-verify run hooks-verify -- quai verify --step fast --json`, then `quai-verify run hooks-prepush -- quai hooks pre-push --json`, then `quai-verify run hooks-uninstall -- quai hooks uninstall --json`. Observe `"installed": true`, `"cached"`, and removal confirmation.

## Gotchas

- `quai patch-id` defaults to `origin/<base>` when that remote-tracking ref exists. If you advance local `main` and rebase onto local `main` without pushing to `origin`, pass `--base main` or push `main` first so `origin/main` reflects the new base.
