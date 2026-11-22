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
	"github.com/coding-pr0jects/quai/internal/execrun"
	"github.com/coding-pr0jects/quai/internal/forge"
	"github.com/coding-pr0jects/quai/internal/hooks"
	"github.com/coding-pr0jects/quai/internal/land"
	"github.com/coding-pr0jects/quai/internal/lock"
	"github.com/coding-pr0jects/quai/internal/patchid"
	"github.com/coding-pr0jects/quai/internal/repo"
	"github.com/coding-pr0jects/quai/internal/tier"
	"github.com/coding-pr0jects/quai/internal/verify"
)

const version = "0.2.0-alpha"

const rootUsage = `quai: local verification, signed attestations, receipts, risk tiering, and serialized PR landing

Usage:
  quai verify [--step <name>...] [--since <ref>] [--no-cache] [--sign] [--key <path>] [--push] [--remote <name>] [--json]
  quai attest check|push|fetch|prune [flags...]
  quai exec --pool <name> [--slots <n>] [--wait <duration>] -- <command...>
  quai tier [--since <ref>] [--json]
  quai patch-id <verdict-sha> [target-sha] [--base <ref>] [--json]
  quai land <pr> [--dry-run] [--method squash|merge|rebase] [--json]
  quai hooks install|uninstall|pre-push [--json]
  quai version
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	code := run(ctx, os.Args[1:], ".", os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
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
		return runVerify(ctx, cwd, rest, stdout, stderr)
	case "attest":
		return runAttest(ctx, cwd, rest, stdout, stderr)
	case "exec":
		return runExec(ctx, cwd, rest, stdin, stdout, stderr)
	case "tier":
		return runTier(ctx, cwd, rest, stdout, stderr)
	case "patch-id":
		return runPatchID(ctx, cwd, rest, stdout, stderr)
	case "land":
		return runLand(ctx, cwd, rest, stdout, stderr)
	case "hooks":
		return runHooks(ctx, cwd, rest, stdout, stderr)
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

func runVerify(ctx context.Context, cwd string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var steps multiFlag
	fs.Var(&steps, "step", "step name to run (repeatable)")
	since := fs.String("since", "", "base ref")
	noCache := fs.Bool("no-cache", false, "ignore existing receipts")
	sign := fs.Bool("sign", os.Getenv("QUAI_SIGN") == "1", "sign passing receipts with SSH key into refs/quai/records/*")
	keyPath := fs.String("key", "", "SSH private key path for --sign")
	push := fs.Bool("push", false, "push signed attestation refs to remote (implies --sign)")
	remote := fs.String("remote", "", "git remote for --push (defaults to [attest].remote)")
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "quai verify: unexpected arguments: %v\n", fs.Args())
		return 2
	}
	g, cfg, _, err := loadRepoAndConfig(ctx, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "quai verify: %v\n", err)
		return 2
	}
	doSign := *sign || *push
	res, err := verify.Run(ctx, g, cfg, verify.Options{
		StepNames:  steps,
		Since:      *since,
		NoCache:    *noCache,
		Sign:       doSign,
		SigningKey: *keyPath,
		Push:       *push,
		Remote:     *remote,
		JSON:       *jsonOut,
		Stdout:     stdout,
		Stderr:     stderr,
	})
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
		allowedSigners := fs.String("allowed-signers", "", "path to OpenSSH allowed_signers file (overrides QUAI_ALLOWED_SIGNERS and [attest].allowed_signers)")
		sample := fs.Int("sample", 0, "probabilistic audit rerun percentage (0-100)")
		fetch := fs.Bool("fetch", false, "fetch refs/quai/records/* from remote first")
		remote := fs.String("remote", "", "git remote for --fetch")
		jsonOut := fs.Bool("json", false, "output JSON")
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() > 0 {
			fmt.Fprintf(stderr, "quai attest check: unexpected arguments: %v\n", fs.Args())
			return 2
		}
		g, cfg, _, err := loadRepoAndConfig(ctx, cwd)
		if err != nil {
			fmt.Fprintf(stderr, "quai attest check: %v\n", err)
			return 2
		}
		res, err := attest.Check(ctx, g, cfg, attest.CheckOptions{
			StepNames:      steps,
			Since:          *since,
			Onto:           *onto,
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
					fmt.Fprintf(stdout, "verified %s (%s by %s)\n", s.Name, s.Fingerprint[:min(12, len(s.Fingerprint))], s.Signer)
				default:
					fmt.Fprintf(stdout, "%-8s %s (%s)\n", s.Status, s.Name, s.Error)
				}
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
		}
		res, err := attest.Prune(ctx, g, cfg, attest.PruneOptions{
			MaxAge:    maxAge,
			Remote:    *remote,
			LocalOnly: *localOnly,
			DryRun:    *dryRun,
		})
		if err != nil {
			fmt.Fprintf(stderr, "quai attest prune: %v\n", err)
			return 1
		}
		if *jsonOut {
			writeJSON(stdout, res)
		} else {
			prefix := "pruned"
			if res.DryRun {
				prefix = "would prune"
			}
			fmt.Fprintf(stdout, "%s %d local and %d remote attestation(s) (kept %d)\n", prefix, len(res.PrunedLocal), len(res.PrunedRemote), res.Kept)
		}
		return 0

	default:
		fmt.Fprintf(stderr, "quai attest: unknown subcommand %q (expected check, push, fetch, or prune)\n", action)
		return 2
	}
}

func runExec(ctx context.Context, cwd string, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("exec", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pool := fs.String("pool", "", "slot pool name from [slots]")
	slots := fs.Int("slots", 1, "number of slots to acquire")
	waitStr := fs.String("wait", "30m", "max wait duration")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cmdArgs := fs.Args()
	if len(cmdArgs) > 0 && cmdArgs[0] == "--" {
		cmdArgs = cmdArgs[1:]
	}
	if *pool == "" || len(cmdArgs) == 0 || *slots <= 0 {
		fmt.Fprintln(stderr, "Usage: quai exec --pool <name> [--slots <n>] [--wait <duration>] -- <command...>")
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
	capacity, ok := cfg.Slots[*pool]
	if !ok {
		fmt.Fprintf(stderr, "quai exec: unknown slot pool %q in %s\n", *pool, config.FileName)
		return 2
	}
	if *slots > capacity {
		fmt.Fprintf(stderr, "quai exec: requested %d slots in pool %q (capacity %d)\n", *slots, *pool, capacity)
		return 2
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "quai exec: %v\n", err)
		return 2
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		absCwd = top
	}
	code, err := execrun.Run(ctx, execrun.Request{
		Dir:       absCwd,
		Argv:      cmdArgs,
		Stdin:     stdin,
		Stdout:    stdout,
		Stderr:    stderr,
		LocksRoot: filepath.Join(commonDir, "quai", "locks"),
		Pool:      *pool,
		Slots:     *slots,
		Capacity:  capacity,
		LockWait:  waitDur,
		Owner: lock.Owner{
			Command:  "quai exec --pool " + *pool + " -- " + strings.Join(cmdArgs, " "),
			Worktree: top,
		},
	})
	if err != nil {
		var te *lock.TimeoutError
		if errors.As(err, &te) {
			fmt.Fprintf(stderr, "quai exec: %v\n", err)
			return 4
		}
	}
	return code
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
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(reordered); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "Usage: quai land <pr> [--dry-run] [--method squash|merge|rebase] [--json]")
		return 2
	}
	prNum, err := strconv.Atoi(strings.TrimPrefix(fs.Arg(0), "#"))
	if err != nil || prNum <= 0 {
		fmt.Fprintf(stderr, "quai land: invalid PR number %q\n", fs.Arg(0))
		return 2
	}
	var method config.MergeMethod
	if *methodStr != "" {
		method = config.MergeMethod(*methodStr)
		if !config.ValidMergeMethod(method) {
			fmt.Fprintf(stderr, "quai land: invalid --method %q\n", *methodStr)
			return 2
		}
	}
	g, cfg, top, err := loadRepoAndConfig(ctx, cwd)
	if err != nil {
		fmt.Fprintf(stderr, "quai land: %v\n", err)
		return 2
	}
	res, err := land.Run(ctx, g, forge.NewGHForge(top), cfg, land.Options{
		PRNumber: prNum,
		DryRun:   *dryRun,
		Method:   method,
		JSON:     *jsonOut,
		Stdout:   stdout,
		Stderr:   stderr,
	})
	if *jsonOut {
		writeJSON(stdout, res)
	} else if res.Action != "" {
		fmt.Fprintf(stdout, "#%d: %s (%s)\n", res.PR, res.Action, res.Reason)
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
