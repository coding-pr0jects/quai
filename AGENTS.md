# quai

`quai` is a single static Go CLI, deterministic and zero-LLM, with no backend. It coordinates verification, CPU slots, risk tiering, and PR landing between parallel worktrees of one git repository. Agents come first: the `--json` output and the exit codes are the product, and human output comes second. It grew out of agent-studio, which runs many coding agents in parallel worktrees, and stays repository-agnostic.

## Values, in rank order

These break a tie when two goals conflict.

1. **Never lie.** A receipt, verdict, or attestation says "passed" only for a command that really ran, on exactly those inputs, and left the tree untouched. Truth beats speed.
2. **Deterministic and zero-LLM.** Same input, same output. The `--json` schemas and the exit codes `0`..`4` are a contract: add fields, never rename or repurpose one.
3. **Nothing left behind.** Every lock is released, every process group is killed, and no daemon keeps running.
4. **Light and fast.** The `pre-push` receipt fast path stays under 50 ms. One binary, one dependency. A performance claim comes with its measurement.
5. **Above git and gh, never instead of them.** `quai` composes the tools a repository already has.

## What quai is not

Check a feature request against this list first.

- An agent orchestrator or LLM reviewer. Orchestration belongs to the harness, and `quai` stays a mechanical gate.
- A hook or linter framework. `hk`, `lefthook`, and `pre-commit` own that layer, so `quai hooks` stays the thin `pre-push` receipt check.
- A merge driver or a sandbox. Entity-level merging belongs to `weave`, and VM or container runners belong to the repository.
- A service. State lives in `<git-common-dir>/quai/` and git refs. Receipts stay on the machine, and attestations are the only results that cross machines.
- Multi-forge. GitHub through `gh` is the only forge.
- A merge train. No batching, no automatic bisection.

## Three ways to hurt yourself

1. **Running `quai land` without `--dry-run` on a real repository.** It merges a real PR. Try landing in the scratch repository of the `verify-quai` harness.
2. **Touching the `<git-common-dir>/quai/` of the repository you work in.** Its locks, queues, and receipts are shared with every other worktree and agent on the machine. Tests build their own repositories in `t.TempDir()`.
3. **Bypassing a guard.** Fix the code when a check fails: no `--no-verify`, no weakened test, and no path that writes a receipt for a step that did not run. Each guard exists for a mistake that already happened.

## Hit every surface

The common defect is a change that works on the path you tried and is missing everywhere else. Walk this list before you call a change done, and name in your report the entries that applied:

- **Human output and `--json`.** Both say the same thing.
- **Exit codes.** The `0`..`4` table of `docs/commands.md`.
- **`.quai.toml`.** A new field gets its default and its validation in `internal/config`.
- **`quai status` and `quai doctor`.** New state is visible there, and a new prerequisite is checked there.
- **Callers.** `quai hooks pre-push` and `quai land` reuse `verify`, receipts, and attestations.
- **Concurrency.** Two worktrees running the same command at the same time.
- **Interruption.** `SIGINT`, `SIGTERM`, and timeout release every lock and kill the process group.
- **Docs.** `docs/commands.md`, `docs/configuration.md`, `README.md`, and `CHANGELOG.md` under `[Unreleased]`.
- **`GLOSSARY.md`.** For a new domain word.

## Verifying

- `mise install` provisions the pinned Go and `gh` (`mise trust` first if mise refuses the config).
- While iterating: `go test -race ./internal/<feature>/...`.
- Before pushing: `gofmt -l .` prints nothing, `go vet ./...` and `go test -race ./...` pass, and `quai verify` passes on the clean commit. `quai verify` runs the installed binary, so run `mise run install` first when your change touches `verify`, `receipt`, or `lock`.
- A change to CLI behavior, hooks, locks, worktrees, or exit codes also runs the `verify-quai` harness (`.agents/skills/verify-quai/SKILL.md`) and keeps its evidence.
- A task is done when all of the above pass, each claim is proven by a test or harness evidence, and your report names the surfaces that applied.

## Where code lives

- `cmd/quai/` parses flags, wires packages, formats output, and maps errors to exit codes. Each feature owns one package under `internal/<feature>/`. Every feature lives in a named package, never in `utils` or `common`.
- Decision logic stays pure and takes plain structs: `tier`, `patchid`, and `land.DecideLand`. Side effects go through narrow interfaces, `repo.Git` and `forge.Forge`, so a test can swap a real temp repository or a fake.
- The only dependency is `github.com/BurntSushi/toml`. Adding a second one needs an issue and the owner's yes first.
- `docs/architecture.md` holds the package map, the data shapes, and the lock protocol.

## Taste

- **Fewer, deeper modules, with smaller interfaces.** A package that hides a lot behind a small interface is cheaper to read, test, and change than many thin ones.
- **Call `git` and `gh` with argv slices through `exec.CommandContext`, with `LC_ALL=C`.** Output then parses the same in every locale, and no PR title or branch name reaches a shell.
- **Start every child process through `internal/execrun`.** It owns the process group (`Setpgid`) and the `SIGTERM`-then-`SIGKILL` cleanup.
- **Give everything you start its stop.** A lock, a heartbeat, a child process, or a temp file has a release on every path, including errors and signals.
- **Write files atomically,** through a temp file and a rename, because another worktree may read at any moment.
- **Name a thing by its role or its action,** so a reader understands it without reading its body.
- **Keep one place per piece of knowledge.** Factor logic out on its third repetition, because two similar shapes don't yet show what they share.
- **Wrap errors with `%w` and context.** An error the user or an agent sees (a refused guard, a bad config, a missing prerequisite) says what to do next: `inputs changed since signing: rerun quai verify, then quai attest push`.
- **Write code, identifiers, comments, commits, and docs in English, with the words of `GLOSSARY.md`.**
- **Comments.** A comment exists only for what the code cannot say and we cannot change. That covers three cases: an outside constraint (a git or gh behavior, a platform quirk, a spec such as DSSE) with its link; the doc comment of an exported identifier; and the reason a lint suppression must give. Everything else goes. When our own code surprises, change the code (a name, a type, an extraction). A « do not remove » constraint is encoded in a test, and then the comment goes.

## Security

`SECURITY.md` lists the invariants: no shell interpolation of forge metadata, validated SHA placeholders, no forged or dirty receipts, no `--admin` or `--delete-branch`, no fork PRs, and lease-pinned force pushes. Read it before you touch `land`, `forge`, `verify`, `receipt`, `attest`, or `verdict`. A PR's branch is untrusted input, its `.quai.toml` and `allowed_signers` included. CI reads policy from the base (`--config-ref`, `--allowed-signers`).

## Git and merging

- Conventional Commits with the package as scope: `feat(land): …`, `fix(lock): …`, `docs: …`, `test(verify): …`, `chore: …`. The header is at most 72 characters, a body line 100.
- PRs are squash-merged (`[land].method`), so the PR title becomes the commit on `main`: a Conventional Commit of at most 64 characters.
- The PR body follows `.github/PULL_REQUEST_TEMPLATE.md`, with the exact commands you ran.
- A change in behavior adds a line to `CHANGELOG.md` under `[Unreleased]`.
- Merge only with `quai land <number>`, after `quai land <number> --dry-run`. Never with `gh pr merge` or the GitHub button.

## Before you…, read…

| Before you… | Read |
|---|---|
| add a package or change how packages talk | `docs/architecture.md` |
| add or change a command, flag, JSON field, or exit code | `docs/commands.md` |
| add or change a `.quai.toml` field | `docs/configuration.md` |
| touch CLI behavior, hooks, locks, worktrees, or exit codes | `.agents/skills/verify-quai/SKILL.md` |
| touch `land`, `forge`, `verify`, `receipt`, `attest`, or `verdict` | `SECURITY.md` |
| name a new concept | `GLOSSARY.md` |
| change how agents are told to call `quai` | `docs/agent-integration.md` |

## Tests

Test what can break, once, at the lowest layer that fails: table tests for the pure engines, real temporary git repositories for anything that shells out to git. Extend an existing test before adding one. Every test passes under `-race`.

## Conflicts

If a rule here fights the task in front of you, keep the rule. Say so loudly, name the rule, and get the owner's sign-off. When nobody can answer (a delegate, an unattended run), finish what you can and put the conflict first in your report.
