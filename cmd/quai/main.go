package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coding-pr0jects/quai/internal/attest"
	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/conflicts"
	"github.com/coding-pr0jects/quai/internal/detach"
	"github.com/coding-pr0jects/quai/internal/execrun"
	"github.com/coding-pr0jects/quai/internal/forge"
	"github.com/coding-pr0jects/quai/internal/gc"
	"github.com/coding-pr0jects/quai/internal/hooks"
	"github.com/coding-pr0jects/quai/internal/land"
	"github.com/coding-pr0jects/quai/internal/lock"
	"github.com/coding-pr0jects/quai/internal/patchid"
	"github.com/coding-pr0jects/quai/internal/repo"
	"github.com/coding-pr0jects/quai/internal/status"
	"github.com/coding-pr0jects/quai/internal/tier"
	"github.com/coding-pr0jects/quai/internal/verdict"
	"github.com/coding-pr0jects/quai/internal/verify"
)

const version = "0.3.0"

const rootUsage = `quai: local verification, signed attestations, receipts, risk tiering, and serialized PR landing

Usage:
  quai verify [--step <name>...] [--since <ref>] [--no-cache] [--sign|--no-sign] [--key <path>] [--push|--no-push] [--remote <name>] [--detach] [--json]
  quai verify --wait [<run-id>] [--max <duration>] [--json]
  quai verify --stop [<run-id>] [--json]
  quai attest check|push|fetch|prune [flags...]
  quai exec --pool <name> [--slots <n>] [--wait <duration>] [--detach] -- <command...>
  quai tier [--since <ref>] [--json]
  quai patch-id <verdict-sha> [target-sha] [--base <ref>] [--json]
  quai verdict record|check [flags...]
  quai land <pr>... [--dry-run] [--continue-on-error=false] [--method squash|merge|rebase] [--restack] [--require-verdict] [--no-sign] [--no-push] [--json]
  quai restack <old-parent-tip> [--base <ref>] [--push] [--remote <name>] [--json]
  quai hooks install|uninstall|pre-push [--json]
  quai gc [--dry-run] [--json]
  quai status [--json]
  quai doctor [--json]
  quai conflicts [--base <ref>] [--conflicts-only] [--json]
  quai version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	var code int
	if id, ok := os.LookupEnv(detach.EnvRunID); ok {
		_ = os.Unsetenv(detach.EnvRunID)
		code = runRecorded(ctx, id, os.Args[1:], os.Stdout, os.Stderr)
	} else {
		code = run(ctx, os.Args[1:], ".", os.Stdin, os.Stdout, os.Stderr)
	}
	stop()
	os.Exit(code)
}

func runRecorded(ctx context.Context, id string, args []string, stdout, stderr io.Writer) int {
	commonDir, err := repo.NewExecGit(".").CommonDir(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "quai: detached run %s: not inside a git repository: %v\n", id, err)
		return 2
	}
	rec, err := detach.OpenRecorder(commonDir, id)
	if err != nil {
		fmt.Fprintf(stderr, "quai: %v\n", err)
		return 2
	}
	code := 2
	switch {
	case len(args) > 0 && args[0] == "verify":
		code = runVerify(ctx, ".", args[1:], stdout, stderr, rec)
	case len(args) > 0 && args[0] == "exec":
		code = runExec(ctx, ".", args[1:], nil, stdout, stderr, rec)
	default:
		fmt.Fprintf(stderr, "quai: detached run %s: unsupported command %v\n", id, args)
	}
	if err := rec.Finish(code); err != nil {
		fmt.Fprintf(stderr, "quai: record exit of detached run %s: %v\n", id, err)
	}
	return code
}

func run(ctx context.Context, args []string, cwd string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, rootUsage)
		return 2
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, rootUsage)
		return 0
	case "version", "--version":
		fmt.Fprintf(stdout, "quai %s\n", version)
		return 0
	case "verify":
		return runVerify(ctx, cwd, rest, stdout, stderr, nil)
	case "attest":
		return runAttest(ctx, cwd, rest, stdout, stderr)
	case "exec":
		return runExec(ctx, cwd, rest, stdin, stdout, stderr, nil)
	case "tier":
		return runTier(ctx, cwd, rest, stdout, stderr)
	case "patch-id":
		return runPatchID(ctx, cwd, rest, stdout, stderr)
	case "verdict":
		return runVerdict(ctx, cwd, rest, stdout, stderr)
	case "land":
		return runLand(ctx, cwd, rest, stdout, stderr)
	case "restack":
		return runRestack(ctx, cwd, rest, stdout, stderr)
	case "hooks":
		return runHooks(ctx, cwd, rest, stdout, stderr)
	case "gc":
		return runGC(ctx, cwd, rest, stdout, stderr)
	case "status":
		return runStatus(ctx, cwd, rest, stdout, stderr)
	case "doctor":
		return runDoctor(ctx, cwd, rest, stdout, stderr)
	case "conflicts":
		return runConflicts(ctx, cwd, rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "quai: unknown command %q\n\n%s", sub, rootUsage)
		return 2
	}
}

type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func loadRepoAndConfig(ctx context.Context, cwd string) (*repo.ExecGit, *config.Config, string, error) {
	g := repo.NewExecGit(cwd)
	top, err := g.TopLevel(ctx)
	if err != nil {
		return nil, nil, "", fmt.Errorf("not inside a git repository: %w", err)
	}
	g.Dir = top
	cfg, err := config.Load(top)
	if err != nil {
		return nil, nil, "", err
	}
	return g, cfg, top, nil
}

func runVerify(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer, rec *detach.Recorder) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var steps multiFlag
	fs.Var(&steps, "step", "step name to run (repeatable)")
	since := fs.String("since", "", "base ref")
	noCache := fs.Bool("no-cache", false, "ignore existing receipts")
	fetchBase := fs.Bool("fetch-base", false, "fetch origin/<base> before resolving the base")
	sign := fs.Bool("sign", false, "sign passing receipts with SSH key into refs/quai/records/*")
	noSign := fs.Bool("no-sign", false, "disable automatic attestation signing even when configured")
	keyPath := fs.String("key", "", "SSH private key path for --sign")
	push := fs.Bool("push", false, "push signed attestation refs to remote (implies --sign)")
	noPush := fs.Bool("no-push", false, "disable automatic attestation push even when configured")
	remote := fs.String("remote", "", "git remote for --push (defaults to [attest].remote)")
	jsonOut := fs.Bool("json", false, "output JSON")
	detachRun := fs.Bool("detach", false, "start the run in the background, print its run id, and exit 0")
	wait := fs.Bool("wait", false, "wait for a detached run: [<run-id>], default the latest from this worktree")
	stop := fs.Bool("stop", false, "stop a detached run: [<run-id>], default the latest from this worktree")
	maxWait := fs.Duration("max", 8*time.Minute, "with --wait: longest wait before exiting 4 (0 checks without blocking)")
	priority := fs.Int("priority", 0, priorityUsage)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var runID string
	if (*wait || *stop) && fs.NArg() > 0 {
		runID = fs.Arg(0)
		if err := fs.Parse(fs.Args()[1:]); err != nil {
			return 2
		}
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "quai verify: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	if *wait || *stop {
		if msg := checkRunControlFlags(fs, *detachRun, *wait, *stop); msg != "" {
			fmt.Fprintf(stderr, "quai verify: %s\n", msg)
			return 2
		}
		return runRunControl(ctx, cwd, runID, *wait, *maxWait, *jsonOut, stdout, stderr)
	}
	if isFlagSet(fs, "max") {
		fmt.Fprintln(stderr, "quai verify: --max applies only to --wait")
		return 2
	}
	if err := checkPriority(*priority); err != nil {
		fmt.Fprintf(stderr, "quai verify: %v\n", err)
		return 2
	}
	g, cfg, top, err := loadRepoAndConfig(ctx, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "quai verify: %v\n", err)
		return 2
	}
	if *detachRun {
		return startDetached(ctx, g, top, cwd, append([]string{"verify"}, forwardFlags(fs, "detach", "json")...), *jsonOut, stdout, stderr)
	}
	defer gc.MaybeAuto(ctx, g, cfg, stderr)
	doSign, doPush, bestEffort := verify.ResolveAutoAttest(cfg, *sign, *noSign, *push, *noPush)
	var onStepStart func(execrun.Process)
	if rec != nil {
		onStepStart = rec.ProcessStarted
	}
	res, err := verify.Run(ctx, g, cfg, verify.Options{
		StepNames:      steps,
		Since:          *since,
		NoCache:        *noCache,
		FetchBase:      *fetchBase,
		Sign:           doSign,
		BestEffortSign: bestEffort,
		SigningKey:     *keyPath,
		Push:           doPush,
		Remote:         *remote,
		JSON:           *jsonOut,
		Stdout:         stdout,
		Stderr:         stderr,
		OnStepStart:    onStepStart,
		Priority:       *priority,
	})
	if res.BaseWarning != "" {
		fmt.Fprintf(stderr, "quai verify: warning: %s\n", res.BaseWarning)
	}
	if rec != nil {
		rec.SetResult(res)
	}
	if *jsonOut {
		writeJSON(stdout, res)
	} else if err != nil && !res.OK {
		fmt.Fprintf(stderr, "quai verify: %v\n", err)
	} else if !res.OK {
		for _, s := range res.Steps {
			if s.Status == "mutated" {
				fmt.Fprintf(stderr, "quai verify: step %q mutated working tree: %s\n", s.Name, strings.Join(s.MutatedPaths, ", "))
			}
		}
	}
	if !*jsonOut {
		fmt.Fprintf(stdout, "exit_kind: %s\n", res.ExitKind)
	}
	if err != nil {
		var te *lock.TimeoutError
		if errors.As(err, &te) {
			return 4
		}
		if res.OK {
			fmt.Fprintf(stderr, "quai verify: %v\n", err)
			return 2
		}
		return 1
	}
	if !res.OK {
		return 1
	}
	return 0
}

func runAttest(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: quai attest check|push|fetch|prune [flags...]")
		return 2
	}
	action := args[0]
	rest := args[1:]

	switch action {
	case "check":
		fs := flag.NewFlagSet("attest check", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var steps multiFlag
		fs.Var(&steps, "step", "step name to verify (repeatable)")
		since := fs.String("since", "", "base ref")
		onto := fs.String("onto", "", "virtual merge-tree target ref (e.g. origin/main)")
		head := fs.String("head", "", "PR head commit of the checked-out merge; accepts its attestation on the old merge base when the base moved disjointly (requires --config-ref)")
		configRef := fs.String("config-ref", "", "load .quai.toml from a trusted git ref instead of the working tree (e.g. origin/main)")
		allowedSigners := fs.String("allowed-signers", "", "path to OpenSSH allowed_signers file (overrides QUAI_ALLOWED_SIGNERS and [attest].allowed_signers)")
		sample := fs.Int("sample", 0, "probabilistic audit rerun percentage (0-100)")
		fetch := fs.Bool("fetch", false, "fetch matching refs/quai/records/* from remote first")
		remote := fs.String("remote", "", "git remote for --fetch")
		jsonOut := fs.Bool("json", false, "output JSON")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() > 0 {
			fmt.Fprintf(stderr, "quai attest check: unexpected arguments: %v\n", fs.Args())
			return 2
		}
		var g *repo.ExecGit
		var cfg *config.Config
		if *configRef != "" {
			g = repo.NewExecGit(cwd)
			top, err := g.TopLevel(ctx)
			if err != nil {
				fmt.Fprintf(stderr, "quai attest check: not inside a git repository: %v\n", err)
				return 2
			}
			g.Dir = top
		} else {
			var err error
			g, cfg, _, err = loadRepoAndConfig(ctx, cwd)
			if err != nil {
				fmt.Fprintf(stderr, "quai attest check: %v\n", err)
				return 2
			}
		}
		res, err := attest.Check(ctx, g, cfg, attest.CheckOptions{
			StepNames:      steps,
			Since:          *since,
			Onto:           *onto,
			Head:           *head,
			ConfigRef:      *configRef,
			AllowedSigners: *allowedSigners,
			Sample:         *sample,
			Fetch:          *fetch,
			Remote:         *remote,
		})
		if err != nil {
			var rce *repo.RebaseConflictError
			if errors.As(err, &rce) {
				if *jsonOut {
					writeJSON(stdout, map[string]any{"ok": false, "conflict": true, "conflict_files": rce.ConflictFiles, "error": rce.Error()})
				} else {
					fmt.Fprintf(stderr, "quai attest check: %v\n", err)
				}
				return 3
			}
			fmt.Fprintf(stderr, "quai attest check: %v\n", err)
			return 2
		}
		if *jsonOut {
			writeJSON(stdout, res)
		} else {
			for _, s := range res.Steps {
				switch s.Status {
				case "verified":
					fmt.Fprintf(stdout, "verified %s (%s by %s, %s)\n", s.Name, shortID(s.Fingerprint), s.Signer, s.MatchMode)
				default:
					fmt.Fprintf(stdout, "%-8s %s [%s] (%s)\n", s.Status, s.Name, s.Reason, s.Error)
				}
			}
			if res.DisjointRefusal != "" {
				fmt.Fprintf(stdout, "disjoint-base refused: %s\n", res.DisjointRefusal)
			} else if res.MatchMode == attest.MatchDisjointBase {
				fmt.Fprintf(stdout, "disjoint-base: PR head %s on old base %s\n", res.HeadSHA, res.OldBaseSHA)
			}
		}
		if !res.OK {
			return 1
		}
		return 0

	case "push":
		fs := flag.NewFlagSet("attest push", flag.ContinueOnError)
		fs.SetOutput(stderr)
		var steps multiFlag
		fs.Var(&steps, "step", "step name to push (repeatable)")
		since := fs.String("since", "", "base ref")
		remote := fs.String("remote", "", "git remote")
		jsonOut := fs.Bool("json", false, "output JSON")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() > 0 {
			fmt.Fprintf(stderr, "quai attest push: unexpected arguments: %v\n", fs.Args())
			return 2
		}
		g, cfg, _, err := loadRepoAndConfig(ctx, cwd)
		if err != nil {
			fmt.Fprintf(stderr, "quai attest push: %v\n", err)
			return 2
		}
		res, err := attest.Push(ctx, g, cfg, attest.PushOptions{
			StepNames: steps,
			Since:     *since,
			Remote:    *remote,
		})
		if err != nil {
			fmt.Fprintf(stderr, "quai attest push: %v\n", err)
			return 1
		}
		if *jsonOut {
			writeJSON(stdout, res)
		} else {
			fmt.Fprintf(stdout, "pushed %d attestation(s) to %s\n", len(res.Pushed), res.Remote)
		}
		return 0

	case "fetch":
		fs := flag.NewFlagSet("attest fetch", flag.ContinueOnError)
		fs.SetOutput(stderr)
		remote := fs.String("remote", "", "git remote")
		jsonOut := fs.Bool("json", false, "output JSON")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() > 0 {
			fmt.Fprintf(stderr, "quai attest fetch: unexpected arguments: %v\n", fs.Args())
			return 2
		}
		g := repo.NewExecGit(cwd)
		top, err := g.TopLevel(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "quai attest fetch: not inside a git repository: %v\n", err)
			return 2
		}
		g.Dir = top
		rem := *remote
		if rem == "" {
			if cfg, err := config.Load(top); err == nil {
				rem = cfg.Attest.Remote
			} else {
				rem = "origin"
			}
		}
		if err := attest.Fetch(ctx, g, rem); err != nil {
			fmt.Fprintf(stderr, "quai attest fetch: %v\n", err)
			return 1
		}
		if *jsonOut {
			writeJSON(stdout, map[string]any{"ok": true, "remote": rem})
		} else {
			fmt.Fprintf(stdout, "fetched attestations from %s\n", rem)
		}
		return 0

	case "prune":
		fs := flag.NewFlagSet("attest prune", flag.ContinueOnError)
		fs.SetOutput(stderr)
		maxAgeStr := fs.String("max-age", "", "maximum age before pruning (defaults to [attest].max_age)")
		remote := fs.String("remote", "", "git remote")
		localOnly := fs.Bool("local-only", false, "only prune local refs/quai/records/*")
		dryRun := fs.Bool("dry-run", false, "preview refs that would be pruned without deleting")
		jsonOut := fs.Bool("json", false, "output JSON")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() > 0 {
			fmt.Fprintf(stderr, "quai attest prune: unexpected arguments: %v\n", fs.Args())
			return 2
		}
		g, cfg, _, err := loadRepoAndConfig(ctx, cwd)
		if err != nil {
			fmt.Fprintf(stderr, "quai attest prune: %v\n", err)
			return 2
		}
		var maxAge time.Duration
		if *maxAgeStr != "" {
			maxAge, err = config.ParseDuration(*maxAgeStr)
			if err != nil || maxAge <= 0 {
				fmt.Fprintf(stderr, "quai attest prune: invalid --max-age %q\n", *maxAgeStr)
				return 2
			}
			if maxAge < config.MinAttestMaxAge {
				fmt.Fprintf(stderr, "quai attest prune: --max-age must be at least %s\n", config.MinAttestMaxAge)
				return 2
			}
		}
		res, err := attest.Prune(ctx, g, cfg, attest.PruneOptions{
			MaxAge:    maxAge,
			Remote:    *remote,
			LocalOnly: *localOnly,
			DryRun:    *dryRun,
		})
		if *jsonOut {
			writeJSON(stdout, res)
		} else {
			prefix := "pruned"
			if res.DryRun {
				prefix = "would prune"
			}
			fmt.Fprintf(stdout, "%s %d local and %d remote attestation(s) (kept %d)\n", prefix, len(res.PrunedLocal), len(res.PrunedRemote), res.Kept)
		}
		if err != nil {
			fmt.Fprintf(stderr, "quai attest prune: %v\n", err)
			return 1
		}
		return 0

	default:
		fmt.Fprintf(stderr, "quai attest: unknown subcommand %q (expected check, push, fetch, or prune)\n", action)
		return 2
	}
}

func runExec(ctx context.Context, cwd string, args []string, stdin io.Reader, stdout, stderr io.Writer, rec *detach.Recorder) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pool := fs.String("pool", "", "slot pool name from [slots]")
	slots := fs.Int("slots", 1, "number of slots to acquire")
	waitStr := fs.String("wait", "30m", "max wait duration")
	detachRun := fs.Bool("detach", false, "start the command in the background, print its run id, and exit 0")
	priority := fs.Int("priority", 0, priorityUsage)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cmdArgs := fs.Args()
	if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}
	if *pool == "" || len(cmdArgs) == 0 || *slots <= 0 {
		fmt.Fprintln(stderr, "Usage: quai exec --pool <name> [--slots <n>] [--wait <duration>] [--priority <int>] [--detach] -- <command...>")
		return 2
	}
	if err := checkPriority(*priority); err != nil {
		fmt.Fprintf(stderr, "quai exec: %v\n", err)
		return 2
	}
	waitDur, err := time.ParseDuration(*waitStr)
	if err != nil || waitDur <= 0 {
		fmt.Fprintf(stderr, "quai exec: invalid --wait %q\n", *waitStr)
		return 2
	}
	g, cfg, top, err := loadRepoAndConfig(ctx, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "quai exec: %v\n", err)
		return 2
	}
	sp, ok := cfg.Slots[*pool]
	if !ok {
		fmt.Fprintf(stderr, "quai exec: unknown slot pool %q in %s\n", *pool, config.FileName)
		return 2
	}
	if *slots > sp.Capacity {
		fmt.Fprintf(stderr, "quai exec: requested %d slots in pool %q (capacity %d)\n", *slots, *pool, sp.Capacity)
		return 2
	}
	if *detachRun {
		args := append(append([]string{"exec"}, forwardFlags(fs, "detach")...), "--")
		return startDetached(ctx, g, top, cwd, append(args, cmdArgs...), false, stdout, stderr)
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "quai exec: %v\n", err)
		return 2
	}
	var onStart func(execrun.Process)
	if rec != nil {
		onStart = rec.ProcessStarted
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		absCwd = top
	}
	res, err := execrun.Run(ctx, execrun.Request{
		Dir:              absCwd,
		Argv:             cmdArgs,
		Stdin:            stdin,
		Stdout:           stdout,
		Stderr:           stderr,
		LocksRoot:        filepath.Join(commonDir, "quai", "locks"),
		Pool:             *pool,
		Slots:            *slots,
		Capacity:         sp.Capacity,
		MinFreeMemBytes:  sp.MinFreeMemBytes,
		MinFreeDiskBytes: sp.MinFreeDiskBytes,
		LockWait:         waitDur,
		Owner: lock.Owner{
			Role:     lock.RoleExec,
			Priority: *priority,
			Command:  "quai exec --pool " + *pool + " -- " + strings.Join(cmdArgs, " "),
			Worktree: top,
		},
		OnStart: onStart,
	})
	if err != nil {
		var te *lock.TimeoutError
		if errors.As(err, &te) {
			fmt.Fprintf(stderr, "quai exec: %v\n", err)
			return 4
		}
	}
	return res.ExitCode
}

const (
	priorityUsage = "slot queue priority, higher runs first (quai land uses 10)"
	maxPriority   = 1000
)

func checkPriority(p int) error {
	if p < -maxPriority || p > maxPriority {
		return fmt.Errorf("--priority %d out of range [-%d, %d]", p, maxPriority, maxPriority)
	}
	return nil
}

func runTier(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tier", flag.ContinueOnError)
	fs.SetOutput(stderr)
	since := fs.String("since", "", "base ref")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "quai tier: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	g, cfg, _, err := loadRepoAndConfig(ctx, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "quai tier: %v\n", err)
		return 2
	}
	res, err := tier.Evaluate(ctx, g, cfg, *since)
	if err != nil {
		fmt.Fprintf(stderr, "quai tier: %v\n", err)
		return 2
	}
	if *jsonOut {
		writeJSON(stdout, res)
		return 0
	}
	if res.BaseWarning != "" {
		fmt.Fprintf(stdout, "warning: %s\n", res.BaseWarning)
	}
	fmt.Fprintf(stdout, "tier: %s\n", res.Tier)
	if len(res.Triggers) > 0 {
		fmt.Fprintf(stdout, "triggers: %s\n", strings.Join(res.Triggers, ", "))
	}
	if res.PureRenames > 0 {
		fmt.Fprintf(stdout, "pure_renames: %d\n", res.PureRenames)
	}
	for _, f := range res.TestsTampered {
		fmt.Fprintf(stdout, "test_tamper: %s (%s %s)\n", f.Path, f.Kind, f.Marker)
	}
	for name, r := range res.Receipts {
		status := "missing"
		if r.Valid {
			status = "valid"
		}
		fmt.Fprintf(stdout, "receipt %s: %s\n", name, status)
	}
	return 0
}

func runPatchID(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer) int {
	// Support flags before or after positional SHAs.
	reordered := reorderFlags(args, map[string]bool{"--base": true})
	fs := flag.NewFlagSet("patch-id", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baseFlag := fs.String("base", "", "base ref")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(reordered); err != nil {
		return 2
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fmt.Fprintln(stderr, "Usage: quai patch-id <verdict-sha> [target-sha] [--base <ref>] [--json]")
		return 2
	}
	g := repo.NewExecGit(cwd)
	top, err := g.TopLevel(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "quai patch-id: not inside a git repository: %v\n", err)
		return 2
	}
	g.Dir = top

	baseRef := *baseFlag
	if baseRef == "" {
		baseName := "main"
		if cfg, err := config.Load(top); err == nil {
			baseName = cfg.Base
		}
		baseRef, err = g.ResolveBaseRef(ctx, baseName)
		if err != nil {
			fmt.Fprintf(stderr, "quai patch-id: %v\n", err)
			return 2
		}
	}
	target := "HEAD"
	if fs.NArg() == 2 {
		target = fs.Arg(1)
	}
	res, err := patchid.Compare(ctx, g, baseRef, fs.Arg(0), target)
	if err != nil {
		fmt.Fprintf(stderr, "quai patch-id: %v\n", err)
		return 2
	}
	if *jsonOut {
		writeJSON(stdout, res)
	} else {
		fmt.Fprintf(stdout, "%s\n", res.Status)
		if len(res.ChangedFiles) > 0 {
			fmt.Fprintf(stdout, "changed: %s\n", strings.Join(res.ChangedFiles, ", "))
		}
	}
	if res.Status == patchid.StatusSurvives {
		return 0
	}
	return 1
}

func runLand(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer) int {
	reordered := reorderFlags(args, map[string]bool{"--method": true})
	fs := flag.NewFlagSet("land", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "preview landing decision without mutating")
	methodStr := fs.String("method", "", "merge method override (squash|merge|rebase)")
	restack := fs.Bool("restack", false, "automatically rebase local stacked child worktrees onto the new base after merge")
	requireVerdict := fs.Bool("require-verdict", false, "require an approved review verdict for this PR regardless of tier")
	noSign := fs.Bool("no-sign", false, "disable automatic attestation signing during rebase verify")
	noPush := fs.Bool("no-push", false, "disable automatic attestation push during rebase verify")
	continueOnError := fs.Bool("continue-on-error", true, "in a batch, keep landing the other PRs after one fails")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(reordered); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "Usage: quai land <pr>... [--dry-run] [--continue-on-error=false] [--method squash|merge|rebase] [--restack] [--require-verdict] [--no-sign] [--no-push] [--json]")
		return 2
	}
	prNums := make([]int, 0, fs.NArg())
	seen := make(map[int]bool, fs.NArg())
	for _, arg := range fs.Args() {
		n, err := strconv.Atoi(strings.TrimPrefix(arg, "#"))
		if err != nil || n <= 0 {
			fmt.Fprintf(stderr, "quai land: invalid PR number %q\n", arg)
			return 2
		}
		if seen[n] {
			fmt.Fprintf(stderr, "quai land: PR #%d is listed twice\n", n)
			return 2
		}
		seen[n] = true
		prNums = append(prNums, n)
	}
	prNum := prNums[0]
	var method config.MergeMethod
	if *methodStr != "" {
		method = config.MergeMethod(*methodStr)
		if !config.ValidMergeMethod(method) {
			fmt.Fprintf(stderr, "quai land: invalid --method %q\n", *methodStr)
			return 2
		}
	}
	g := repo.NewExecGit(cwd)
	top, err := g.TopLevel(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "quai land: not inside a git repository: %v\n", err)
		return 2
	}
	g.Dir = top
	f := forge.NewRetry(forge.NewGHForge(top), stderr)
	cfg, err := config.Load(top)
	if errors.Is(err, os.ErrNotExist) {
		cfg, err = land.BaseConfig(ctx, g, f, prNum)
	}
	if err != nil {
		fmt.Fprintf(stderr, "quai land: %v\n", err)
		return 2
	}
	if !*dryRun {
		defer gc.MaybeAuto(ctx, g, cfg, stderr)
	}
	opts := land.Options{
		PRNumber:       prNum,
		DryRun:         *dryRun,
		Method:         method,
		Restack:        *restack,
		RequireVerdict: *requireVerdict,
		NoSign:         *noSign,
		NoPush:         *noPush,
		JSON:           *jsonOut,
		Stdout:         stdout,
		Stderr:         stderr,
	}
	if len(prNums) > 1 {
		return runLandBatch(ctx, g, f, cfg, land.BatchOptions{
			Options:         opts,
			PRNumbers:       prNums,
			ContinueOnError: *continueOnError,
		}, stdout, stderr)
	}
	res, err := land.Run(ctx, g, f, cfg, opts)
	if *jsonOut {
		writeJSON(stdout, res)
	} else {
		if res.Action != "" {
			fmt.Fprintf(stdout, "#%d: %s (%s)\n", res.PR, res.Action, res.Reason)
		}
		fmt.Fprintf(stdout, "exit_kind: %s\n", res.ExitKind)
	}
	if err != nil {
		var rce *repo.RebaseConflictError
		if errors.As(err, &rce) {
			if !*jsonOut {
				fmt.Fprintf(stderr, "quai land: %v\n", err)
			}
			return 3
		}
		var te *lock.TimeoutError
		if errors.As(err, &te) {
			fmt.Fprintf(stderr, "quai land: %v\n", err)
			return 4
		}
		fmt.Fprintf(stderr, "quai land: %v\n", err)
		return 1
	}
	if !res.OK {
		return 1
	}
	return 0
}

func runLandBatch(ctx context.Context, g repo.Git, f forge.Forge, cfg *config.Config, opts land.BatchOptions, stdout, stderr io.Writer) int {
	res, err := land.RunBatch(ctx, g, f, cfg, opts)
	if opts.JSON {
		writeJSON(stdout, res)
	} else {
		printBatchHuman(stdout, res)
	}
	if err != nil {
		fmt.Fprintf(stderr, "quai land: %v\n", err)
		var te *lock.TimeoutError
		switch {
		case errors.As(err, &te):
			return 4
		case errors.Is(err, land.ErrBasePolicy):
			return 2
		}
		return 1
	}
	if !res.OK {
		return 1
	}
	return 0
}

func printBatchHuman(w io.Writer, res land.BatchResult) {
	for _, e := range res.PRs {
		fmt.Fprintf(w, "#%d: %s %s (%s)\n", e.PR, e.Status, e.Action, e.Reason)
		if e.Error != "" {
			fmt.Fprintf(w, "#%d: error: %s\n", e.PR, e.Error)
		}
		fmt.Fprintf(w, "exit_kind: %s\n", e.ExitKind)
	}
}

func runHooks(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: quai hooks install|uninstall|pre-push [--json]")
		return 2
	}
	action := args[0]
	fs := flag.NewFlagSet("hooks", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	g := repo.NewExecGit(cwd)
	top, err := g.TopLevel(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "quai hooks: not inside a git repository: %v\n", err)
		return 2
	}
	g.Dir = top

	switch action {
	case "install":
		res, err := hooks.Install(ctx, g)
		if err != nil {
			fmt.Fprintf(stderr, "quai hooks install: %v\n", err)
			return 1
		}
		if *jsonOut {
			writeJSON(stdout, res)
		} else {
			fmt.Fprintln(stdout, res.Message)
			if res.Snippet != "" {
				fmt.Fprintf(stdout, "Add to your hook:\n  %s\n", res.Snippet)
			}
		}
		return 0
	case "uninstall":
		msg, err := hooks.Uninstall(ctx, g)
		if err != nil {
			fmt.Fprintf(stderr, "quai hooks uninstall: %v\n", err)
			return 1
		}
		if *jsonOut {
			writeJSON(stdout, map[string]any{"ok": true, "message": msg})
		} else {
			fmt.Fprintln(stdout, msg)
		}
		return 0
	case "pre-push":
		cfg, err := config.Load(top)
		if err != nil {
			fmt.Fprintf(stderr, "quai hooks pre-push: %v\n", err)
			return 2
		}
		res, err := hooks.RunPrePush(ctx, g, cfg, *jsonOut, stdout, stderr)
		if *jsonOut {
			writeJSON(stdout, res)
		}
		if err != nil {
			var te *lock.TimeoutError
			if errors.As(err, &te) {
				return 4
			}
			return 1
		}
		if !res.OK {
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "quai hooks: unknown subcommand %q (expected install, uninstall, or pre-push)\n", action)
		return 2
	}
}

func runGC(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dryRun := fs.Bool("dry-run", false, "list what would be removed without mutating refs, files, or locks")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "quai gc: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	g, cfg, _, err := loadRepoAndConfig(ctx, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "quai gc: %v\n", err)
		return 2
	}
	if !*dryRun {
		lease, err := gc.Acquire(ctx, g, "quai gc", 30*time.Second)
		if err != nil {
			var te *lock.TimeoutError
			if errors.As(err, &te) {
				fmt.Fprintf(stderr, "quai gc: %v\n", err)
				return 4
			}
			fmt.Fprintf(stderr, "quai gc: acquire prune lock: %v\n", err)
			return 1
		}
		defer lease.Release()
	}
	res, err := gc.Run(ctx, g, cfg, gc.Options{DryRun: *dryRun})
	if err != nil {
		fmt.Fprintf(stderr, "quai gc: %v\n", err)
		return 1
	}
	if !*dryRun {
		if commonDir, err := g.CommonDir(ctx); err == nil && ctx.Err() == nil {
			if err := gc.WriteStamp(commonDir, time.Now().UTC()); err != nil {
				res.Warnings = append(res.Warnings, fmt.Sprintf("last-prune stamp: %v", err))
			}
		}
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(stderr, "quai gc: warning: %s\n", w)
	}
	if *jsonOut {
		writeJSON(stdout, res)
		return 0
	}
	verb := "removed"
	if res.DryRun {
		verb = "would remove"
	}
	fmt.Fprintf(stdout, "%s %d item(s), kept %d\n", verb, len(res.Removed), res.Kept)
	for _, item := range res.Removed {
		location := ""
		if item.Location != "" {
			location = " [" + item.Location + "]"
		}
		fmt.Fprintf(stdout, "  %s%s %s (%s)\n", item.Kind, location, item.Name, item.Reason)
	}
	return 0
}

func runStatus(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "quai status: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	g := repo.NewExecGit(cwd)
	top, err := g.TopLevel(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "quai status: not inside a git repository: %v\n", err)
		return 2
	}
	g.Dir = top
	cfg, _ := config.Load(top)
	rep, err := status.Inspect(ctx, g, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "quai status: %v\n", err)
		return 1
	}
	if *jsonOut {
		writeJSON(stdout, rep)
		return 0
	}
	if rep.LandLock.Held {
		state := "live"
		if rep.LandLock.Stale {
			state = "stale"
		}
		if rep.LandLock.Owner != nil {
			fmt.Fprintf(stdout, "land.lock: held (%s, pid %d, %s, waiters=%d)\n", state, rep.LandLock.Owner.PID, rep.LandLock.Owner.Command, len(rep.LandLock.Waiters))
		} else {
			fmt.Fprintf(stdout, "land.lock: held (%s, waiters=%d)\n", state, len(rep.LandLock.Waiters))
		}
	} else {
		fmt.Fprintln(stdout, "land.lock: free")
	}
	printWaiters(stdout, rep.LandLock.Waiters)
	for _, p := range rep.Pools {
		fmt.Fprintf(stdout, "pool %-10s %d/%d held (stale=%d, waiters=%d)\n", p.Name+":", p.Held, p.Capacity, p.Stale, len(p.Waiters))
		for _, sl := range p.Slots {
			if sl.Owner != nil {
				printQueueEntry(stdout, "holder", *sl.Owner)
			}
		}
		printWaiters(stdout, p.Waiters)
	}
	if len(rep.InFlight) > 0 {
		fmt.Fprintf(stdout, "in-flight: %d step(s)\n", len(rep.InFlight))
	}
	for _, wt := range rep.LandWorktrees {
		fmt.Fprintf(stdout, "land worktree: %s\n", wt)
	}
	if rep.AvailableMemBytes > 0 {
		fmt.Fprintf(stdout, "memory: %s available\n", lock.FormatBytes(rep.AvailableMemBytes))
	}
	if rep.AvailableDiskBytes > 0 {
		fmt.Fprintf(stdout, "disk: %s free\n", lock.FormatBytes(rep.AvailableDiskBytes))
	}
	fmt.Fprintf(stdout, "receipts: %d cached, attestations: %d local\n", rep.ReceiptCount, rep.AttestCount)
	for _, r := range rep.Runs {
		fmt.Fprintf(stdout, "run %s: %s (pid %d, quai %s, %s)\n", r.ID, r.State, r.PID, strings.Join(r.Args, " "), r.Worktree)
	}
	if rep.AutoPrune {
		last := "never"
		if rep.LastPrune != nil {
			last = rep.LastPrune.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(stdout, "prune: automatic every %s (last %s)\n", rep.PruneInterval, last)
	} else {
		fmt.Fprintln(stdout, "prune: manual (quai gc)")
	}
	return 0
}

func printWaiters(w io.Writer, waiters []lock.Owner) {
	for _, o := range waiters {
		printQueueEntry(w, "waiter", o)
	}
}

func printQueueEntry(w io.Writer, kind string, o lock.Owner) {
	role := string(o.Role)
	if role == "" {
		role = "-"
	}
	fmt.Fprintf(w, "  %-6s %-6s priority=%-4d pid %d: %s\n", kind, role, o.Priority, o.PID, o.Command)
}

func runDoctor(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "quai doctor: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	g := repo.NewExecGit(cwd)
	if top, err := g.TopLevel(ctx); err == nil {
		g.Dir = top
	}
	rep := status.Doctor(ctx, g)
	if *jsonOut {
		writeJSON(stdout, rep)
	} else {
		for _, c := range rep.Checks {
			fmt.Fprintf(stdout, "%-4s %-22s %s\n", c.Status, c.Name, c.Message)
			if c.Fix != "" {
				fmt.Fprintf(stdout, "     fix: %s\n", c.Fix)
			}
		}
	}
	if !rep.OK {
		return 1
	}
	return 0
}

func runConflicts(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("conflicts", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baseRef := fs.String("base", "", "base ref (defaults to origin/<base> or <base>)")
	conflictsOnly := fs.Bool("conflicts-only", false, "report only merge-tree conflicts and ignore clean file overlaps")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "quai conflicts: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	g := repo.NewExecGit(cwd)
	top, err := g.TopLevel(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "quai conflicts: not inside a git repository: %v\n", err)
		return 2
	}
	g.Dir = top
	cfg, _ := config.Load(top)
	res, err := conflicts.Evaluate(ctx, g, cfg, conflicts.Options{
		BaseRef:       *baseRef,
		ConflictsOnly: *conflictsOnly,
	})
	if err != nil {
		fmt.Fprintf(stderr, "quai conflicts: %v\n", err)
		return 2
	}
	if *jsonOut {
		writeJSON(stdout, res)
	} else if res.OK {
		fmt.Fprintf(stdout, "no conflicts across %d worktree(s) (base %s)\n", len(res.Worktrees), res.BaseRef)
	} else {
		for _, bc := range res.BaseConflicts {
			fmt.Fprintf(stdout, "base conflict  %s (%s) vs %s: %s\n", bc.Branch, bc.Worktree, bc.BaseRef, strings.Join(bc.ConflictFiles, ", "))
		}
		for _, col := range res.Collisions {
			if col.Conflict {
				fmt.Fprintf(stdout, "conflict       %s <-> %s: %s\n", labelWT(col.BranchA, col.WorktreeA), labelWT(col.BranchB, col.WorktreeB), strings.Join(col.ConflictFiles, ", "))
			} else {
				fmt.Fprintf(stdout, "overlap        %s <-> %s: %s\n", labelWT(col.BranchA, col.WorktreeA), labelWT(col.BranchB, col.WorktreeB), strings.Join(col.OverlapFiles, ", "))
			}
		}
	}
	if !res.OK {
		return 1
	}
	return 0
}

func labelWT(branch, path string) string {
	if branch != "" {
		return branch
	}
	return path
}

func runVerdict(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "Usage: quai verdict record|check [flags...]")
		return 2
	}
	action := args[0]
	rest := args[1:]

	switch action {
	case "record":
		fs := flag.NewFlagSet("verdict record", flag.ContinueOnError)
		fs.SetOutput(stderr)
		statusStr := fs.String("status", "approved", "verdict status (approved|rejected)")
		tierStr := fs.String("tier", "", "risk tier override (defaults to current branch tier)")
		reviewer := fs.String("reviewer", "", "reviewer identity (defaults to git user.email)")
		note := fs.String("note", "", "optional short review note")
		since := fs.String("since", "", "base ref")
		sign := fs.Bool("sign", false, "sign verdict with SSH key into refs/quai/verdicts/<patch-id>")
		keyPath := fs.String("key", "", "SSH signing key path for --sign")
		push := fs.Bool("push", false, "push signed verdict ref to remote (implies --sign)")
		remote := fs.String("remote", "", "git remote for --push")
		jsonOut := fs.Bool("json", false, "output JSON")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() > 0 {
			fmt.Fprintf(stderr, "quai verdict record: unexpected arguments: %v\n", fs.Args())
			return 2
		}
		g, cfg, _, err := loadRepoAndConfig(ctx, cwd)
		if err != nil {
			fmt.Fprintf(stderr, "quai verdict record: %v\n", err)
			return 2
		}
		res, err := verdict.RecordVerdict(ctx, g, cfg, verdict.RecordOptions{
			Status:     verdict.Status(*statusStr),
			Tier:       config.Tier(*tierStr),
			Reviewer:   *reviewer,
			Note:       *note,
			Since:      *since,
			Sign:       *sign || *push,
			SigningKey: *keyPath,
			Push:       *push,
			Remote:     *remote,
		})
		if err != nil {
			fmt.Fprintf(stderr, "quai verdict record: %v\n", err)
			return 1
		}
		if *jsonOut {
			writeJSON(stdout, res)
		} else {
			fmt.Fprintf(stdout, "recorded %s verdict (%s) for patch-id %s", res.Record.Status, res.Record.Tier, shortID(res.Record.PatchID))
			if code := res.Record.CodePatchID; code != "" && code != res.Record.PatchID {
				fmt.Fprintf(stdout, " and code patch-id %s", shortID(code))
			}
			fmt.Fprintln(stdout)
		}
		return 0

	case "check":
		fs := flag.NewFlagSet("verdict check", flag.ContinueOnError)
		fs.SetOutput(stderr)
		since := fs.String("since", "", "base ref")
		requireSigned := fs.Bool("require-signed", false, "require a cryptographically signed verdict ref")
		allowedSigners := fs.String("allowed-signers", "", "path to OpenSSH allowed_signers file")
		configRef := fs.String("config-ref", "", "load .quai.toml from a trusted git ref (e.g. origin/main); needed to keep a verdict across gate-only commits")
		fetch := fs.Bool("fetch", false, "fetch refs/quai/verdicts/<patch-id> from remote first")
		remote := fs.String("remote", "", "git remote for --fetch")
		jsonOut := fs.Bool("json", false, "output JSON")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() > 0 {
			fmt.Fprintf(stderr, "quai verdict check: unexpected arguments: %v\n", fs.Args())
			return 2
		}
		g, cfg, _, err := loadRepoAndConfig(ctx, cwd)
		if err != nil {
			fmt.Fprintf(stderr, "quai verdict check: %v\n", err)
			return 2
		}
		res, err := verdict.Check(ctx, g, cfg, verdict.CheckOptions{
			Since:          *since,
			ConfigRef:      *configRef,
			RequireSigned:  *requireSigned || cfg.Land.RequireSignedVerdict,
			AllowedSigners: *allowedSigners,
			Fetch:          *fetch,
			Remote:         *remote,
		})
		if err != nil {
			fmt.Fprintf(stderr, "quai verdict check: %v\n", err)
			return 2
		}
		if *jsonOut {
			writeJSON(stdout, res)
		} else if res.OK && res.Record != nil && res.Record.PatchID != res.PatchID {
			fmt.Fprintf(stdout, "valid (%s, patch-id %s): reviewed at patch-id %s, only gate-only paths changed since\n",
				res.BranchTier, shortID(res.PatchID), shortID(res.Record.PatchID))
		} else if res.OK {
			fmt.Fprintf(stdout, "valid (%s, patch-id %s)\n", res.BranchTier, shortID(res.PatchID))
		} else {
			fmt.Fprintf(stdout, "%s: %s\n", res.Status, res.Error)
		}
		if !res.OK {
			return 1
		}
		return 0

	default:
		fmt.Fprintf(stderr, "quai verdict: unknown subcommand %q (expected record or check)\n", action)
		return 2
	}
}

func runRestack(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer) int {
	reordered := reorderFlags(args, map[string]bool{"--base": true, "--remote": true})
	fs := flag.NewFlagSet("restack", flag.ContinueOnError)
	fs.SetOutput(stderr)
	baseRef := fs.String("base", "", "target base ref (defaults to origin/<base> or <base>)")
	push := fs.Bool("push", false, "push restacked branch with --force-with-lease")
	remote := fs.String("remote", "origin", "git remote for --push")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(reordered); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: quai restack <old-parent-tip> [--base <ref>] [--push] [--remote <name>] [--json]")
		return 2
	}
	g := repo.NewExecGit(cwd)
	top, err := g.TopLevel(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "quai restack: not inside a git repository: %v\n", err)
		return 2
	}
	g.Dir = top
	cfg, _ := config.Load(top)
	res, err := land.Restack(ctx, g, cfg, land.RestackOptions{
		OldUpstream: fs.Arg(0),
		BaseRef:     *baseRef,
		Push:        *push,
		Remote:      *remote,
	})
	if *jsonOut {
		writeJSON(stdout, res)
	} else if err == nil {
		pidStatus := ""
		if res.PatchID != nil {
			pidStatus = fmt.Sprintf(", patch-id=%s", res.PatchID.Status)
		}
		fmt.Fprintf(stdout, "restacked %s onto %s (%s -> %s%s)\n", res.Branch, res.BaseRef, res.OldHeadSHA[:min(9, len(res.OldHeadSHA))], res.NewHeadSHA[:min(9, len(res.NewHeadSHA))], pidStatus)
	}
	if err != nil {
		var rce *repo.RebaseConflictError
		if errors.As(err, &rce) {
			if !*jsonOut {
				fmt.Fprintf(stderr, "quai restack: %v\n", err)
			}
			return 3
		}
		fmt.Fprintf(stderr, "quai restack: %v\n", err)
		return 1
	}
	return 0
}

func reorderFlags(args []string, valueFlags map[string]bool) []string {
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
			if valueFlags[a] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			pos = append(pos, a)
		}
	}
	return append(flags, pos...)
}

func writeJSON(w io.Writer, v any) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "\t")
	_ = enc.Encode(v)
}

func shortID(s string) string {
	return s[:min(12, len(s))]
}
