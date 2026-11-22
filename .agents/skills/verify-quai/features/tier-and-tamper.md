# Tier and test tampering

`quai tier` classifies a branch diff into `gate-only`, `diff-audit`, or `full-live`, keeps 100%-similarity git renames at `gate-only`, escalates deleted or skipped tests to `full-live`, and reports current local receipt validity in one JSON response.

## Sub-features

- `tier-gate-only`: docs-only changes or empty diffs return `gate-only`.
- `tier-pure-rename`: moving a file with `git mv` and zero content edits reports `pure_renames: 1` and stays `gate-only`.
- `tier-full-live`: modifying a path matching `tiers.full_live` returns `full-live` with exact `triggers`.
- `tier-test-tamper`: adding `.skip(` or `t.Skip(` to a test file or deleting a test file populates `tests_tampered` and escalates to `full-live`.
- `tier-receipts`: reports `receipts.<step>.valid` for each configured step on `HEAD^{tree}`.

## How to get to it (user POV)

- Run `quai tier [--since <ref>] [--json]` from any worktree of a repository with `.quai.toml`.

## Driving it with quai-verify

Preconditions: `quai-verify launch` and `quai-verify doctor` succeeded.

- **Docs edit (`tier-gate-only`).** In the worktree, append to `docs/guide.md`, commit, and run `quai-verify run-wt tier-docs -- quai tier --json`. Observe exit code `0` and `"tier": "gate-only"`.
- **Pure rename (`tier-pure-rename`).** In the worktree, run `git mv ui/view.txt ui/view-moved.txt`, commit, and run `quai-verify run-wt tier-rename -- quai tier --json`. Observe `"tier": "gate-only"` and `"pure_renames": 1`.
- **Test tamper escalation (`tier-test-tamper`).** In the worktree, append `it.skip("x")` to `app/main_test.txt`, commit, and run `quai-verify run-wt tier-tamper -- quai tier --json`. Observe `"tier": "full-live"` and `"tests_tampered"` naming `app/main_test.txt` with `"kind": "skip_added"`.

## Gotchas

- `git diff -M100%` marks a rename as `R100` only when file content is byte-for-byte identical. Editing a single line in the moved file makes it a normal modification subject to `full_live` or `diff_audit`.
