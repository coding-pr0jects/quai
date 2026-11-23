package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/coding-pr0jects/quai/internal/attest"
	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/execrun"
	"github.com/coding-pr0jects/quai/internal/exitkind"
	"github.com/coding-pr0jects/quai/internal/lock"
	"github.com/coding-pr0jects/quai/internal/receipt"
	"github.com/coding-pr0jects/quai/internal/repo"
	"github.com/coding-pr0jects/quai/internal/tier"
)

// PassedStepsEnv names the variable that gives each step the comma-separated steps that
// already passed or were cached earlier in the same quai verify run. quai only writes it.
const PassedStepsEnv = "QUAI_PASSED_STEPS"

type Options struct {
	StepNames      []string
	Since          string
	NoCache        bool
	FetchBase      bool
	Sign           bool
	BestEffortSign bool
	SigningKey     string
	Push           bool
	Remote         string
	UnsetEnv       []string
	JSON           bool
	Stdout         io.Writer
	Stderr         io.Writer
	OnStepStart    func(execrun.Process)
	Role           lock.Role
	Priority       int
}

// ResolveAutoAttest resolves whether verification should sign and push attestations,
// combining explicit CLI flags, QUAI_SIGN/QUAI_PUSH env vars, and [attest] config defaults.
// bestEffort is true only when signing came implicitly from .quai.toml rather than an explicit flag or env var.
func ResolveAutoAttest(cfg *config.Config, flagSign, flagNoSign, flagPush, flagNoPush bool) (sign, push, bestEffort bool) {
	if flagNoSign {
		return false, false, false
	}
	envSign := strings.TrimSpace(os.Getenv("QUAI_SIGN"))
	envPush := strings.TrimSpace(os.Getenv("QUAI_PUSH"))
	wantAutoPush := !flagNoPush && envPush != "0" && (envPush == "1" || cfg.Attest.AutoPush)

	if flagPush {
		return true, true, false
	}
	if flagSign {
		return true, wantAutoPush, false
	}
	if envPush == "1" {
		return true, !flagNoPush, false
	}
	if envSign == "1" {
		return true, wantAutoPush, false
	}
	if envSign == "0" {
		return false, false, false
	}
	if cfg.Attest.AutoPush && !flagNoPush && envPush != "0" {
		return true, true, true
	}
	if cfg.Attest.AutoSign {
		return true, false, true
	}
	return false, false, false
}

type StepResult struct {
	Name           string        `json:"name"`
	Status         string        `json:"status"` // cached | passed | failed | mutated
	Fingerprint    string        `json:"fingerprint,omitempty"`
	DurationMS     int64         `json:"duration_ms"`
	AttestationRef string        `json:"attestation_ref,omitempty"`
	MutatedPaths   []string      `json:"mutated_paths,omitempty"`
	Pool           string        `json:"pool,omitempty"`
	PoolAcquired   *bool         `json:"pool_acquired,omitempty"`
	ExitKind       exitkind.Kind `json:"exit_kind,omitempty"`
	Error          string        `json:"error,omitempty"`
}

type Result struct {
	OK             bool          `json:"ok"`
	ExitKind       exitkind.Kind `json:"exit_kind"`
	Clean          bool          `json:"clean"`
	HeadSHA        string        `json:"head_sha"`
	TreeSHA        string        `json:"tree_sha"`
	BaseSHA        string        `json:"base_sha"`
	BaseWarning    string        `json:"base_warning,omitempty"`
	ReceiptWritten bool          `json:"receipt_written"`
	Attestations   []string      `json:"attestations,omitempty"`
	Pushed         bool          `json:"pushed,omitempty"`
	Steps          []StepResult  `json:"steps"`
}

// Run executes the selected verification steps, enforces slot limits and the working-tree
// mutation guard, deduplicates concurrent runs on identical fingerprints, and writes
// input-fingerprinted receipts on clean passing runs.
func Run(ctx context.Context, g repo.Git, cfg *config.Config, opts Options) (res Result, runErr error) {
	defer func() { res.ExitKind = classifyExit(res, runErr) }()

	topLevel, err := g.TopLevel(ctx)
	if err != nil {
		return Result{}, err
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		return Result{}, err
	}
	headSHA, err := g.HeadSHA(ctx)
	if err != nil {
		return Result{}, err
	}
	treeSHA, err := g.TreeSHA(ctx, "HEAD")
	if err != nil {
		return Result{}, err
	}

	if opts.FetchBase {
		if err := g.Fetch(ctx, "origin", cfg.Base); err != nil {
			return Result{}, fmt.Errorf("fetch origin %s: %w", cfg.Base, err)
		}
	}
	baseRef := opts.Since
	if baseRef == "" {
		baseRef, err = g.ResolveBaseRef(ctx, cfg.Base)
		if err != nil {
			baseRef = "HEAD"
		}
	}
	baseSHA, err := g.MergeBase(ctx, baseRef, "HEAD")
	if err != nil {
		return Result{}, fmt.Errorf("resolve merge-base with %s: %w", baseRef, err)
	}

	selected, err := cfg.StepPlan(opts.StepNames)
	if err != nil {
		return Result{}, err
	}

	clean, _, err := g.IsClean(ctx)
	if err != nil {
		return Result{}, err
	}

	routing, err := collectRouting(ctx, g, cfg, selected, clean, baseSHA, opts.Since)
	if err != nil {
		return Result{}, err
	}

	var signingKey, principal string
	if opts.Sign && clean {
		signingKey, err = attest.ResolveSigningKey(ctx, g, cfg, opts.SigningKey)
		if err != nil {
			if opts.BestEffortSign {
				if !opts.JSON && opts.Stderr != nil {
					fmt.Fprintf(opts.Stderr, "quai: auto_sign skipped (%v)\n", err)
				}
				opts.Sign = false
				opts.Push = false
			} else {
				return Result{}, err
			}
		} else {
			principal, _ = g.ConfigGet(ctx, "user.email")
		}
	}

	store := receipt.NewStore(commonDir)
	locksRoot := filepath.Join(commonDir, "quai", "locks")

	res = Result{
		OK:      true,
		Clean:   clean,
		HeadSHA: headSHA,
		TreeSHA: treeSHA,
		BaseSHA: baseSHA,
	}
	if age, fetched, err := repo.FetchHeadAge(ctx, g, time.Now()); err == nil {
		res.BaseWarning = repo.StaleBaseWarning(baseRef, age, fetched)
	}

	var passed []string
	for _, step := range selected {
		route := routeForStep(step, routing)
		stepRes, receiptWritten, attRef, stepErr := runOneStep(
			ctx, g, cfg, store, locksRoot, topLevel,
			step, route, clean, headSHA, treeSHA, baseSHA,
			signingKey, principal, passed, opts,
		)
		if receiptWritten {
			res.ReceiptWritten = true
		}
		if attRef != "" {
			res.Attestations = append(res.Attestations, attRef)
		}
		if stepRes.Name != "" {
			res.Steps = append(res.Steps, stepRes)
		}
		if stepErr != nil {
			if stepRes.Status == "failed" {
				res.OK = false
			}
			return res, stepErr
		}
		if stepRes.Status == "failed" || stepRes.Status == "mutated" {
			res.OK = false
			return res, nil
		}
		passed = append(passed, step.Name)
	}

	if opts.Sign && opts.Push && len(res.Attestations) > 0 {
		remote := opts.Remote
		if remote == "" {
			remote = cfg.Attest.Remote
		}
		if err := g.PushRefs(ctx, remote, res.Attestations); err != nil {
			return res, err
		}
		res.Pushed = true
	}
	return res, nil
}

type routingInput struct {
	changed []string
	tier    config.Tier
}

func collectRouting(ctx context.Context, g repo.Git, cfg *config.Config, selected []config.Step, clean bool, baseSHA, since string) (routingInput, error) {
	var rc routingInput
	needPaths, needTier := false, false
	for _, s := range selected {
		if s.HasConditionalPool() {
			needPaths = true
		}
		if len(s.PoolTiers) > 0 {
			needTier = true
		}
	}
	if !needPaths {
		return rc, nil
	}

	to := "HEAD"
	var untracked []string
	if !clean {
		to = ""
		var err error
		untracked, err = g.UntrackedFiles(ctx)
		if err != nil {
			return routingInput{}, err
		}
	}
	changes, err := g.DiffNameStatus(ctx, baseSHA, to, true)
	if err != nil {
		return routingInput{}, err
	}
	rc.changed = changedPaths(changes, untracked)

	if needTier {
		bc, err := tier.ClassifyBranch(ctx, g, cfg, since)
		if err != nil {
			return routingInput{}, err
		}
		rc.tier = bc.Decision.Tier
	}
	return rc, nil
}

func changedPaths(changes []repo.FileChange, untracked []string) []string {
	seen := make(map[string]struct{}, len(changes)+len(untracked))
	out := make([]string, 0, len(changes)+len(untracked))
	add := func(p string) {
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		out = append(out, p)
	}
	for _, ch := range changes {
		add(ch.Path)
		add(ch.OldPath)
	}
	for _, p := range untracked {
		add(p)
	}
	sort.Strings(out)
	return out
}

type poolRoute struct {
	conditional  bool
	acquire      bool
	pool         string
	slots        int
	matchedPaths int
	tier         config.Tier
}

func routeForStep(step config.Step, rc routingInput) poolRoute {
	route := poolRoute{
		conditional: step.HasConditionalPool(),
		acquire:     true,
		pool:        step.Pool,
		slots:       step.Slots,
		tier:        rc.tier,
	}
	if !route.conditional {
		return route
	}
	decision := step.RoutePool(rc.changed, rc.tier)
	route.acquire = decision.Acquire
	route.matchedPaths = decision.MatchedPaths
	if !decision.Acquire {
		route.slots = 0
	}
	return route
}

func (r poolRoute) annotate(sr *StepResult, held bool) {
	if !r.conditional {
		return
	}
	sr.Pool = r.pool
	heldCopy := held
	sr.PoolAcquired = &heldCopy
}

func (r poolRoute) describeRouting() string {
	if r.acquire {
		detail := "branch tier " + string(r.tier)
		if r.matchedPaths > 0 {
			detail = fmt.Sprintf("%d changed path(s) matched", r.matchedPaths)
		}
		return fmt.Sprintf("routed to %s (%s)", r.pool, detail)
	}
	return fmt.Sprintf("bypassed %s (diff did not match pool_when or pool_tiers)", r.pool)
}

func runOneStep(
	ctx context.Context,
	g repo.Git,
	cfg *config.Config,
	store *receipt.Store,
	locksRoot, topLevel string,
	step config.Step,
	route poolRoute,
	clean bool,
	headSHA, treeSHA, baseSHA string,
	signingKey, principal string,
	passed []string,
	opts Options,
) (StepResult, bool, string, error) {
	started := time.Now()
	var fp string
	var inflightLease *lock.Lease
	owner := lock.Owner{
		Role:     opts.Role,
		Priority: opts.Priority,
		Command:  "quai verify --step " + step.Name,
		Worktree: topLevel,
	}
	if owner.Role == "" {
		owner.Role = lock.RoleVerify
	}

	if clean {
		var err error
		fp, err = receipt.Fingerprint(ctx, g, step, treeSHA, baseSHA)
		if err != nil {
			return StepResult{}, false, "", err
		}
		if !opts.NoCache {
			if sr, attRef, hit, err := checkCache(ctx, g, store, step.Name, fp, started, signingKey, principal, opts); err != nil || hit {
				return sr, false, attRef, err
			}
		}

		// Deduplicate concurrent executions of the exact same (step, fingerprint) across worktrees.
		inflightDir := filepath.Join(locksRoot, fmt.Sprintf("inflight-%s-%s.lock", step.Name, fp))
		inflightLease, err = lock.AcquireDir(ctx, inflightDir, owner, lock.Options{
			Wait:         cfg.Land.LockWaitDur,
			PollInterval: 200 * time.Millisecond,
			StatusWriter: opts.Stderr,
		})
		if err != nil {
			return StepResult{}, false, "", err
		}
		defer inflightLease.Release()

		if !opts.NoCache {
			if sr, attRef, hit, err := checkCache(ctx, g, store, step.Name, fp, started, signingKey, principal, opts); err != nil || hit {
				return sr, false, attRef, err
			}
		}
	}

	expanded, err := config.ExpandCommand(step.Run, baseSHA, headSHA, treeSHA)
	if err != nil {
		return StepResult{}, false, "", err
	}

	var beforeStatus string
	if clean {
		beforeStatus, err = g.StatusPorcelain(ctx)
		if err != nil {
			return StepResult{}, false, "", err
		}
	}

	var capBuf bytes.Buffer
	stdout, stderr := opts.Stdout, opts.Stderr
	if opts.JSON {
		stdout, stderr = &capBuf, &capBuf
	} else if opts.Stdout != nil {
		fmt.Fprintf(opts.Stdout, "run     %s\n", step.Name)
		if route.conditional {
			fmt.Fprintf(opts.Stdout, "pool    %s: %s\n", step.Name, route.describeRouting())
		}
	}

	held := false
	var onSlotAcquire func()
	if route.conditional {
		onSlotAcquire = func() {
			held = true
			if !opts.JSON && opts.Stdout != nil {
				fmt.Fprintf(opts.Stdout, "pool    %s: acquired %s\n", step.Name, route.pool)
			}
		}
	}

	capacity := 0
	var minFreeMem, minFreeDisk uint64
	if route.pool != "" {
		sp := cfg.Slots[route.pool]
		capacity = sp.Capacity
		minFreeMem = sp.MinFreeMemBytes
		minFreeDisk = sp.MinFreeDiskBytes
	}
	runRes, runErr := execrun.Run(ctx, execrun.Request{
		Dir:              topLevel,
		ShellCmd:         expanded,
		Env:              []string{PassedStepsEnv + "=" + strings.Join(passed, ",")},
		Timeout:          step.Duration,
		Stdout:           stdout,
		Stderr:           stderr,
		LocksRoot:        locksRoot,
		Pool:             route.pool,
		Slots:            route.slots,
		Capacity:         capacity,
		MinFreeMemBytes:  minFreeMem,
		MinFreeDiskBytes: minFreeDisk,
		UnsetEnv:         opts.UnsetEnv,
		LockWait:         cfg.Land.LockWaitDur,
		Owner:            owner,
		OnStart:          opts.OnStepStart,
		OnSlotAcquire:    onSlotAcquire,
	})
	dur := time.Since(started).Milliseconds()
	if runErr != nil || runRes.ExitCode != 0 {
		errMsg := ""
		if runErr != nil {
			errMsg = runErr.Error()
		} else {
			errMsg = fmt.Sprintf("exit code %d", runRes.ExitCode)
		}
		sr := StepResult{
			Name:        step.Name,
			Status:      "failed",
			Fingerprint: fp,
			DurationMS:  dur,
			ExitKind:    stepExitKind(runRes, runErr),
			Error:       errMsg,
		}
		route.annotate(&sr, held)
		return sr, false, "", runErr
	}

	var attRef string
	receiptWritten := false
	if clean {
		afterHead, err := g.HeadSHA(ctx)
		if err != nil {
			return StepResult{}, false, "", err
		}
		afterStatus, err := g.StatusPorcelain(ctx)
		if err != nil {
			return StepResult{}, false, "", err
		}
		if afterHead != headSHA || afterStatus != beforeStatus {
			mutated := parseStatusPaths(afterStatus)
			if afterHead != headSHA {
				mutated = append(mutated, fmt.Sprintf("HEAD moved (%s -> %s)", shortSHA(headSHA), shortSHA(afterHead)))
			}
			sr := StepResult{
				Name:         step.Name,
				Status:       "mutated",
				Fingerprint:  fp,
				DurationMS:   dur,
				MutatedPaths: mutated,
				ExitKind:     exitkind.DirtyTree,
				Error:        "step mutated the working tree or HEAD; refusing to record receipt",
			}
			route.annotate(&sr, held)
			return sr, false, "", nil
		}

		r := receipt.Receipt{
			Version:     receipt.CurrentVersion,
			Fingerprint: fp,
			TreeSHA:     treeSHA,
			HeadSHA:     headSHA,
			BaseSHA:     baseSHA,
			Step:        step.Name,
			Passed:      true,
			DurationMS:  dur,
			CreatedAt:   time.Now().UTC(),
		}
		if err := store.Write(r); err != nil {
			return StepResult{}, false, "", err
		}
		receiptWritten = true

		if opts.Sign {
			ref, _, err := attest.WriteRecord(ctx, g, r, signingKey, principal)
			if err != nil {
				return StepResult{}, receiptWritten, "", err
			}
			attRef = ref
		}
	}

	sr := StepResult{
		Name:           step.Name,
		Status:         "passed",
		Fingerprint:    fp,
		DurationMS:     dur,
		AttestationRef: attRef,
	}
	route.annotate(&sr, held)
	if !opts.JSON && opts.Stdout != nil {
		fmt.Fprintf(opts.Stdout, "pass    %s (%dms)\n", step.Name, dur)
	}
	return sr, receiptWritten, attRef, nil
}

func checkCache(
	ctx context.Context,
	g repo.Git,
	store *receipt.Store,
	stepName, fp string,
	started time.Time,
	signingKey, principal string,
	opts Options,
) (StepResult, string, bool, error) {
	cachedRec, hit, err := store.LookupFresh(stepName, fp)
	if err != nil || !hit {
		return StepResult{}, "", false, nil
	}
	var attRef string
	if opts.Sign {
		ref := attest.RefName(stepName, fp)
		if _, exists, err := attest.ReadRecord(ctx, g, stepName, fp); err != nil {
			return StepResult{}, "", false, err
		} else if !exists {
			if _, _, err := attest.WriteRecord(ctx, g, *cachedRec, signingKey, principal); err != nil {
				return StepResult{}, "", false, err
			}
		}
		attRef = ref
	}
	sr := StepResult{
		Name:           stepName,
		Status:         "cached",
		Fingerprint:    fp,
		DurationMS:     time.Since(started).Milliseconds(),
		AttestationRef: attRef,
	}
	if !opts.JSON && opts.Stdout != nil {
		fmt.Fprintf(opts.Stdout, "cached  %s (%s)\n", stepName, shortSHA(fp))
	}
	return sr, attRef, true, nil
}

func stepExitKind(res execrun.Result, err error) exitkind.Kind {
	var te *lock.TimeoutError
	switch {
	case errors.As(err, &te):
		return exitkind.Classify(false, exitkind.Facts{LockTimedOut: true})
	case errors.Is(err, context.DeadlineExceeded):
		return exitkind.Classify(false, exitkind.Facts{TimedOut: true})
	case errors.Is(err, context.Canceled):
		return exitkind.Classify(false, exitkind.Facts{})
	case res.KilledBySIGKILL:
		return exitkind.Classify(false, exitkind.Facts{KilledBySIGKILL: true})
	case res.ExitCode != 0 || err != nil:
		return exitkind.Classify(false, exitkind.Facts{StepFailed: true})
	default:
		return exitkind.Pass
	}
}

func classifyExit(res Result, err error) exitkind.Kind {
	var te *lock.TimeoutError
	if errors.As(err, &te) {
		return exitkind.Classify(false, exitkind.Facts{LockTimedOut: true})
	}
	if res.OK {
		if err != nil {
			return exitkind.Classify(false, exitkind.Facts{})
		}
		return exitkind.Pass
	}
	for i := len(res.Steps) - 1; i >= 0; i-- {
		if res.Steps[i].ExitKind != "" {
			return res.Steps[i].ExitKind
		}
	}
	return exitkind.Classify(false, exitkind.Facts{})
}

func parseStatusPaths(porcelain string) []string {
	var paths []string
	for _, line := range strings.Split(strings.TrimRight(porcelain, "\n"), "\n") {
		if len(line) >= 4 {
			paths = append(paths, strings.TrimSpace(line[3:]))
		}
	}
	return paths
}

func shortSHA(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
