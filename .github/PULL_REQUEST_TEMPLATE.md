## Summary

<!-- Describe what this pull request changes and why in 1 to 3 sentences. -->

## Related Issue

<!-- Link an issue if applicable, e.g., Closes #12 -->

## Changes

- 

## Verification

<!-- Paste the exact commands you ran and their outcomes. -->

- [ ] `gofmt -l .` returns no unformatted files
- [ ] `go vet ./...` passes
- [ ] `go test -race ./...` passes
- [ ] `quai verify` passes
- [ ] End-to-end CLI verification harness run (when touching CLI, hooks, locks, worktrees, or exit codes)

## Checklist

- [ ] Changes follow the one-feature-per-folder layout under `internal/<feature>/`
- [ ] Decision logic stays pure and unit-tested without network or `gh` calls
- [ ] No untrusted forge metadata is passed to `sh -c`
- [ ] Documentation (`README.md`, `docs/`, `SPEC.md`, `CHANGELOG.md`) updated where behavior changed
