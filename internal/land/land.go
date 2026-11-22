package land

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/exitkind"
	"github.com/coding-pr0jects/quai/internal/forge"
	"github.com/coding-pr0jects/quai/internal/glob"
	"github.com/coding-pr0jects/quai/internal/lock"
	"github.com/coding-pr0jects/quai/internal/patchid"
	"github.com/coding-pr0jects/quai/internal/repo"
	"github.com/coding-pr0jects/quai/internal/tier"
	"github.com/coding-pr0jects/quai/internal/verdict"
	"github.com/coding-pr0jects/quai/internal/verify"
)

type Action string

// SlotPriority is the queue priority of quai land, so a landing passes queued
// quai verify and quai exec waiters, which default to priority 0.
const SlotPriority = 10

const (
	ActionRefuse          Action = "refuse"
	ActionDirectMerge     Action = "direct-merge"
	ActionOptimisticMerge Action = "optimistic-merge"
	ActionRebaseAndMerge  Action = "rebase-and-merge"
)

// FailurePostRebaseVerify is the Result.Failure of a landing whose verification failed after the
// rebase; the rebased branch was reset to its original commit and nothing was pushed.
const FailurePostRebaseVerify = "post-rebase-verify-failed"

// DecisionInput holds the facts Decide reads. HeadSHA is the PR head as quai sees it: the caller's
// HEAD when CurrentBranch is the PR head branch, else the fetched origin/<pr-head>. Clean is
// consulted only in the first case, because landing never touches another branch's worktree.
type DecisionInput struct {
	Clean         bool
	CurrentBranch string
	HeadSHA       string
	PR            forge.PullRequest
	BaseDelta     []string
	PRFiles       []string
	MergeTree     repo.MergeTreeResult
	Policy        config.LandPolicy
}

type Decision struct {
	Action Action `json:"action"`
	Reason string `json:"reason"`
}

type Options struct {
	PRNumber             int
	DryRun               bool
	Method               config.MergeMethod // optional override
	NoSign               bool
	NoPush               bool
	Restack              bool
	RequireVerdict       bool
	RequireSignedVerdict bool
	AllowedSigners       string
	JSON                 bool
	Stdout               io.Writer
	Stderr               io.Writer
	ChecksPoll           time.Duration
	CreationGrace        time.Duration
}

type Result struct {
	OK              bool                 `json:"ok"`
	ExitKind        exitkind.Kind        `json:"exit_kind"`
	DryRun          bool                 `json:"dry_run"`
	Merged          bool                 `json:"merged"`
	PR              int                  `json:"pr"`
	Action          Action               `json:"action"`
	Reason          string               `json:"reason"`
	HeadSHA         string               `json:"head_sha,omitempty"`
	MergeCommit     string               `json:"merge_commit,omitempty"`
	URL             string               `json:"url,omitempty"`
	PatchID         *patchid.Result      `json:"patch_id,omitempty"`
	Checks          *forge.ChecksVerdict `json:"checks,omitempty"`
	Verdict         *verdict.CheckResult `json:"verdict,omitempty"`
	StackedChildren []StackedChild       `json:"stacked_children,omitempty"`
	ConflictFiles   []string             `json:"conflict_files,omitempty"`
	Failure         string               `json:"failure,omitempty"`
}

// Decide is the pure state machine that selects direct-merge, optimistic-merge,
// rebase-and-merge, or refuse from repository and PR facts.
func Decide(in DecisionInput) Decision {
	if d, ok := admit(in.PR); !ok {
		return d
	}
	if in.CurrentBranch == in.PR.HeadRefName {
		if !in.Clean {
			return Decision{Action: ActionRefuse, Reason: "working tree is dirty; commit or stash changes first"}
		}
		if in.HeadSHA != in.PR.HeadRefOid {
			return Decision{Action: ActionRefuse, Reason: fmt.Sprintf("local HEAD %s differs from remote PR head %s; push or pull first", short(in.HeadSHA), short(in.PR.HeadRefOid))}
		}
	} else if in.HeadSHA != in.PR.HeadRefOid {
		return Decision{Action: ActionRefuse, Reason: fmt.Sprintf("origin/%s is at %s but GitHub reports PR head %s; rerun quai land %d", in.PR.HeadRefName, short(in.HeadSHA), short(in.PR.HeadRefOid), in.PR.Number)}
	}
	if len(in.BaseDelta) == 0 {
		return Decision{Action: ActionDirectMerge, Reason: "branch is already up to date with base"}
	}

	optimistic := in.Policy.OptimisticMerge == nil || *in.Policy.OptimisticMerge
	if !optimistic {
		return Decision{Action: ActionRebaseAndMerge, Reason: "base moved and optimistic_merge is disabled"}
	}
	if in.PR.MergeStateStatus == "BEHIND" {
		return Decision{Action: ActionRebaseAndMerge, Reason: "GitHub branch protection requires branch to be up to date with base"}
	}
	if !in.MergeTree.Clean {
		return Decision{Action: ActionRebaseAndMerge, Reason: "virtual merge-tree detected conflicts with base"}
	}
	if hit := firstGlobalMatch(in.BaseDelta, in.Policy.GlobalPaths); hit != "" {
		return Decision{Action: ActionRebaseAndMerge, Reason: fmt.Sprintf("base modified global path %s", hit)}
	}
	if overlap := firstOverlap(in.BaseDelta, in.PRFiles); overlap != "" {
		return Decision{Action: ActionRebaseAndMerge, Reason: fmt.Sprintf("base and PR both modified %s", overlap)}
	}
	return Decision{Action: ActionOptimisticMerge, Reason: fmt.Sprintf("base moved by %d disjoint file(s) and merge-tree is clean", len(in.BaseDelta))}
}

// BaseConfig loads .quai.toml from origin/<base> of PR prNumber, for a caller whose worktree predates it.
func BaseConfig(ctx context.Context, g repo.Git, f forge.Forge, prNumber int) (*config.Config, error) {
	pr, err := f.ViewPR(ctx, prNumber)
	if err != nil {
		return nil, err
	}
	baseRef := "refs/remotes/origin/" + pr.BaseRefName
	if err := g.Fetch(ctx, "origin", "+refs/heads/"+pr.BaseRefName+":"+baseRef); err != nil {
		return nil, err
	}
	raw, err := g.ShowFile(ctx, baseRef, config.FileName)
	if err != nil {
		return nil, fmt.Errorf("no %s in this worktree nor on origin/%s: %w", config.FileName, pr.BaseRefName, err)
	}
	return config.Parse(raw)
}

func admit(pr forge.PullRequest) (Decision, bool) {
	switch {
	case pr.State != "OPEN":
		return Decision{Action: ActionRefuse, Reason: fmt.Sprintf("PR #%d is %s, not OPEN", pr.Number, pr.State)}, false
	case pr.IsDraft:
		return Decision{Action: ActionRefuse, Reason: fmt.Sprintf("PR #%d is a draft; mark it ready first", pr.Number)}, false
	case pr.IsCrossRepository:
		return Decision{Action: ActionRefuse, Reason: fmt.Sprintf("PR #%d is from a fork; refusing to land untrusted fork", pr.Number)}, false
	}
	return Decision{}, true
}

// Run executes quai land for one pull request, holding land.lock across rebase, verify, push, and merge.
func Run(ctx context.Context, g repo.Git, f forge.Forge, cfg *config.Config, opts Options) (res Result, runErr error) {
	defer func() { res.ExitKind = landExitKind(res, runErr) }()

	topLevel, err := g.TopLevel(ctx)
	if err != nil {
		return Result{}, err
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		return Result{}, err
	}
	method := cfg.Land.Method
	if opts.Method != "" {
		method = opts.Method
	}

	pr, err := f.ViewPR(ctx, opts.PRNumber)
	if err != nil {
		return Result{}, err
	}
	if d, ok := admit(pr); !ok {
		return Result{
			ExitKind: exitkind.Refused,
			DryRun:   opts.DryRun,
			PR:       opts.PRNumber,
			Action:   ActionRefuse,
			Reason:   d.Reason,
			URL:      pr.URL,
		}, nil
	}
	branch, err := g.CurrentBranch(ctx)
	if err != nil {
		return Result{}, err
	}
	ws := newWorkspace(g, commonDir, branch, pr.Number, pr.HeadRefName)
	defer ws.close(ctx, opts.Stderr)

	clean, headSHA, err := ws.prHead(ctx, pr)
	if err != nil {
		return Result{}, err
	}
	preDec := Decide(DecisionInput{
		Clean:         clean,
		CurrentBranch: branch,
		HeadSHA:       headSHA,
		PR:            pr,
		Policy:        cfg.Land,
	})
	if preDec.Action == ActionRefuse {
		return Result{
			OK:       false,
			ExitKind: refusalExitKind(clean),
			DryRun:   opts.DryRun,
			PR:       opts.PRNumber,
			Action:   ActionRefuse,
			Reason:   preDec.Reason,
			HeadSHA:  headSHA,
			URL:      pr.URL,
		}, nil
	}

	if opts.DryRun {
		return runDry(ctx, g, ws, f, cfg, opts, pr, clean, branch, headSHA)
	}

	lockDir := filepath.Join(commonDir, "quai", "locks", "land.lock")
	lease, err := lock.AcquireDir(ctx, lockDir, lock.Owner{
		Role:     lock.RoleLand,
		Priority: SlotPriority,
		Command:  fmt.Sprintf("quai land %d", opts.PRNumber),
		Worktree: topLevel,
	}, lock.Options{
		Wait:         cfg.Land.LockWaitDur,
		PollInterval: 2 * time.Second,
		ReportEvery:  30 * time.Second,
		StatusWriter: opts.Stderr,
	})
	if err != nil {
		return Result{}, err
	}
	defer lease.Release()
	return landLocked(ctx, g, ws, branch, f, cfg, opts, method)
}

func landLocked(ctx context.Context, g repo.Git, ws *workspace, branch string, f forge.Forge, cfg *config.Config, opts Options, method config.MergeMethod) (res Result, runErr error) {
	defer func() { res.ExitKind = landExitKind(res, runErr) }()

	var (
		pr      forge.PullRequest
		headSHA string
		clean   bool
		err     error
	)
	const maxRounds = 3
	for round := 1; round <= maxRounds; round++ {
		pr, err = f.ViewPR(ctx, opts.PRNumber)
		if err != nil {
			return Result{}, err
		}
		if err := g.Fetch(ctx, "origin", pr.BaseRefName); err != nil {
			return Result{}, err
		}
		baseTip, err := g.RevParse(ctx, "origin/"+pr.BaseRefName)
		if err != nil {
			return Result{}, err
		}
		clean, headSHA, err = ws.prHead(ctx, pr)
		if err != nil {
			return Result{}, err
		}

		dec, err := computeDecision(ctx, g, cfg, clean, branch, headSHA, pr, baseTip)
		if err != nil {
			return Result{}, err
		}
		if dec.Action == ActionRefuse {
			return Result{
				OK:       false,
				ExitKind: refusalExitKind(clean),
				PR:       opts.PRNumber,
				Action:   ActionRefuse,
				Reason:   dec.Reason,
				HeadSHA:  headSHA,
				URL:      pr.URL,
			}, nil
		}

		if !opts.JSON && opts.Stdout != nil {
			fmt.Fprintf(opts.Stdout, "land #%d: %s (%s)\n", pr.Number, dec.Action, dec.Reason)
		}

		var patchRes *patchid.Result
		if dec.Action == ActionRebaseAndMerge {
			wg, err := ws.open(ctx, headSHA)
			if err != nil {
				return Result{}, err
			}
			rb, err := rebaseVerifyPush(ctx, wg, cfg, opts, pr, headSHA)
			var rce *repo.RebaseConflictError
			switch {
			case errors.As(err, &rce):
				return Result{
					OK:            false,
					ExitKind:      exitkind.StepFailed,
					PR:            opts.PRNumber,
					Action:        ActionRebaseAndMerge,
					Reason:        rce.Error(),
					HeadSHA:       headSHA,
					URL:           pr.URL,
					ConflictFiles: rce.ConflictFiles,
				}, rce
			case rb.verifyFailure != "":
				return Result{
					OK:       false,
					ExitKind: rb.exitKind,
					PR:       opts.PRNumber,
					Action:   ActionRebaseAndMerge,
					Reason:   rb.verifyFailure,
					HeadSHA:  rb.headSHA,
					URL:      pr.URL,
					PatchID:  rb.patchID,
					Failure:  FailurePostRebaseVerify,
				}, err
			case err != nil:
				return Result{}, err
			}
			headSHA = rb.headSHA
			patchRes = rb.patchID
		}

		verdictRes, vRefuse, err := enforceVerdict(ctx, ws, headSHA, cfg, "origin/"+pr.BaseRefName, opts)
		if err != nil {
			return Result{}, err
		}
		if vRefuse != "" {
			return Result{
				OK:       false,
				ExitKind: exitkind.Refused,
				PR:       opts.PRNumber,
				Action:   ActionRefuse,
				Reason:   vRefuse,
				HeadSHA:  headSHA,
				URL:      pr.URL,
				PatchID:  patchRes,
				Verdict:  verdictRes,
			}, nil
		}

		verdictChecks, err := waitChecks(ctx, f, pr.Number, cfg.Land.RequiredChecks, cfg.Land.ChecksWaitDur, opts)
		if err != nil {
			return Result{}, err
		}
		if verdictChecks.Kind != "green" {
			return Result{
				OK:       false,
				ExitKind: exitkind.StepFailed,
				PR:       opts.PRNumber,
				Action:   dec.Action,
				Reason:   formatCheckFailure(verdictChecks),
				HeadSHA:  headSHA,
				URL:      pr.URL,
				PatchID:  patchRes,
				Checks:   &verdictChecks,
				Verdict:  verdictRes,
			}, nil
		}

		// Re-check head and base race before invoking merge.
		afterPR, err := f.ViewPR(ctx, opts.PRNumber)
		if err != nil {
			return Result{}, err
		}
		if afterPR.HeadRefOid != headSHA {
			continue
		}
		if err := g.Fetch(ctx, "origin", pr.BaseRefName); err != nil {
			return Result{}, err
		}
		nowBase, err := g.RevParse(ctx, "origin/"+pr.BaseRefName)
		if err != nil {
			return Result{}, err
		}
		if nowBase != baseTip {
			continue
		}

		// Retarget stacked child PRs to pr.BaseRefName before merging so GitHub
		// branch auto-deletion cannot auto-close child PRs.
		childPRs := discoverChildPRs(ctx, f, pr.HeadRefName)
		stackedChildren, err := retargetChildrenBeforeMerge(ctx, f, childPRs, pr.BaseRefName)
		if err != nil {
			return Result{}, err
		}

		if err := f.MergePR(ctx, pr.Number, method, headSHA); err != nil {
			return Result{}, err
		}
		mergedPR, err := f.ViewPR(ctx, pr.Number)
		if err != nil {
			return Result{}, err
		}
		mergeCommit := ""
		if mergedPR.MergeCommit != nil {
			mergeCommit = mergedPR.MergeCommit.OID
		}
		if mergedPR.State == "MERGED" && len(stackedChildren) > 0 {
			stackedChildren = restackChildrenAfterMerge(
				ctx, g, cfg, stackedChildren, pr.BaseRefName, headSHA,
				opts.Restack || cfg.Land.AutoRestack,
			)
		}
		return Result{
			OK:              mergedPR.State == "MERGED",
			Merged:          mergedPR.State == "MERGED",
			PR:              pr.Number,
			Action:          dec.Action,
			Reason:          dec.Reason,
			HeadSHA:         headSHA,
			MergeCommit:     mergeCommit,
			URL:             pr.URL,
			PatchID:         patchRes,
			Checks:          &verdictChecks,
			Verdict:         verdictRes,
			StackedChildren: stackedChildren,
		}, nil
	}

	return Result{
		OK:      false,
		PR:      opts.PRNumber,
		Action:  ActionRefuse,
		Reason:  fmt.Sprintf("head or base kept moving for %d rounds; rerun quai land %d", maxRounds, opts.PRNumber),
		HeadSHA: headSHA,
		URL:     pr.URL,
	}, nil
}

type rebased struct {
	headSHA       string
	patchID       *patchid.Result
	verifyFailure string
	exitKind      exitkind.Kind
}

func rebaseVerifyPush(ctx context.Context, wg repo.Git, cfg *config.Config, opts Options, pr forge.PullRequest, preRebaseSHA string) (rb rebased, err error) {
	pushed := false
	defer func() {
		if pushed {
			return
		}
		if resetErr := wg.ResetHard(context.WithoutCancel(ctx), preRebaseSHA); resetErr != nil {
			err = errors.Join(err, fmt.Errorf("roll back PR head to %s: %w", short(preRebaseSHA), resetErr))
		}
	}()
	baseRef := "origin/" + pr.BaseRefName
	if err := wg.Rebase(ctx, baseRef); err != nil {
		return rb, err
	}
	rb.headSHA, err = wg.HeadSHA(ctx)
	if err != nil {
		return rb, err
	}
	if pid, err := patchid.Compare(ctx, wg, baseRef, preRebaseSHA, rb.headSHA); err == nil {
		rb.patchID = &pid
	}
	if len(cfg.Steps) > 0 {
		doSign, doPush, bestEffort := verify.ResolveAutoAttest(cfg, false, opts.NoSign, false, opts.NoPush)
		vRes, vErr := verify.Run(ctx, wg, cfg, verify.Options{
			Since:          baseRef,
			Sign:           doSign,
			BestEffortSign: bestEffort,
			Push:           doPush,
			JSON:           opts.JSON,
			Stdout:         opts.Stdout,
			Stderr:         opts.Stderr,
			Role:           lock.RoleLand,
			Priority:       SlotPriority,
		})
		rolledBack := fmt.Sprintf("PR head reset to %s, nothing pushed", short(preRebaseSHA))
		if vErr != nil {
			rb.verifyFailure = fmt.Sprintf("verification failed after rebase: %v; %s", vErr, rolledBack)
			rb.exitKind = vRes.ExitKind
			return rb, vErr
		}
		if !vRes.OK {
			rb.verifyFailure = "verification failed after rebase; " + rolledBack
			rb.exitKind = vRes.ExitKind
			return rb, nil
		}
	}
	if err := wg.PushForceWithLease(ctx, "origin", pr.HeadRefName, preRebaseSHA); err != nil {
		return rb, err
	}
	pushed = true
	return rb, nil
}

func runDry(ctx context.Context, g repo.Git, ws *workspace, f forge.Forge, cfg *config.Config, opts Options, pr forge.PullRequest, clean bool, branch, headSHA string) (Result, error) {
	_ = g.Fetch(ctx, "origin", pr.BaseRefName)
	baseTip, err := g.RevParse(ctx, "origin/"+pr.BaseRefName)
	if err != nil {
		baseTip, err = g.RevParse(ctx, pr.BaseRefName)
		if err != nil {
			return Result{}, err
		}
	}
	dec, err := computeDecision(ctx, g, cfg, clean, branch, headSHA, pr, baseTip)
	if err != nil {
		return Result{}, err
	}
	checks, err := f.Checks(ctx, pr.Number)
	if err != nil {
		return Result{}, err
	}
	verdictChecks := forge.EvaluateChecks(checks, cfg.Land.RequiredChecks)
	verdictRes, vRefuse, err := enforceVerdict(ctx, ws, headSHA, cfg, baseTip, opts)
	if err != nil {
		return Result{}, err
	}
	if vRefuse != "" {
		dec = Decision{Action: ActionRefuse, Reason: vRefuse}
	}
	var detectedChildren []StackedChild
	for _, c := range discoverChildPRs(ctx, f, pr.HeadRefName) {
		detectedChildren = append(detectedChildren, StackedChild{
			PR:         c.Number,
			Branch:     c.HeadRefName,
			OldHeadSHA: c.HeadRefOid,
			Status:     "detected",
		})
	}
	ok := dec.Action != ActionRefuse && (verdictChecks.Kind == "green" || dec.Action == ActionRebaseAndMerge)
	res := Result{
		OK:              ok,
		DryRun:          true,
		PR:              pr.Number,
		Action:          dec.Action,
		Reason:          dec.Reason,
		HeadSHA:         headSHA,
		URL:             pr.URL,
		Checks:          &verdictChecks,
		Verdict:         verdictRes,
		StackedChildren: detectedChildren,
	}
	if dec.Action == ActionRefuse {
		res.ExitKind = refusalExitKind(true)
	} else if verdictChecks.Kind != "green" {
		res.ExitKind = exitkind.StepFailed
	}
	return res, nil
}

func enforceVerdict(ctx context.Context, ws *workspace, headSHA string, cfg *config.Config, since string, opts Options) (*verdict.CheckResult, string, error) {
	if !opts.RequireVerdict && len(cfg.Land.RequireVerdictTiers) == 0 {
		return nil, "", nil
	}
	g, err := ws.open(ctx, headSHA)
	if err != nil {
		return nil, "", err
	}
	tRes, err := tier.Evaluate(ctx, g, cfg, since)
	if err != nil {
		return nil, "", err
	}
	required := opts.RequireVerdict
	if !required {
		for _, rt := range cfg.Land.RequireVerdictTiers {
			if rt == tRes.Tier {
				required = true
				break
			}
		}
	}
	if !required {
		return nil, "", nil
	}
	configRef := since
	if _, err := g.ShowFile(ctx, since, config.FileName); err != nil {
		configRef = ""
	}
	vRes, err := verdict.Check(ctx, g, cfg, verdict.CheckOptions{
		Since:          since,
		ConfigRef:      configRef,
		RequireSigned:  opts.RequireSignedVerdict || cfg.Land.RequireSignedVerdict,
		AllowedSigners: opts.AllowedSigners,
	})
	if err != nil {
		return nil, "", err
	}
	if !vRes.OK {
		return &vRes, fmt.Sprintf("tier %q requires an approved review verdict (%s)", tRes.Tier, vRes.Error), nil
	}
	return &vRes, "", nil
}

func computeDecision(ctx context.Context, g repo.Git, cfg *config.Config, clean bool, branch, headSHA string, pr forge.PullRequest, baseTip string) (Decision, error) {
	mergeBase, err := g.MergeBase(ctx, baseTip, headSHA)
	if err != nil {
		return Decision{}, fmt.Errorf("merge-base(%s, %s): %w", baseTip, headSHA, err)
	}
	var baseDelta, prFiles []string
	var mt repo.MergeTreeResult
	if mergeBase != baseTip {
		baseDelta, err = g.DiffNameOnly(ctx, mergeBase, baseTip)
		if err != nil {
			return Decision{}, err
		}
		prFiles, err = g.DiffNameOnly(ctx, mergeBase, headSHA)
		if err != nil {
			return Decision{}, err
		}
		if cfg.Land.OptimisticMerge == nil || *cfg.Land.OptimisticMerge {
			mt, err = g.MergeTree(ctx, baseTip, headSHA)
			if err != nil {
				return Decision{}, err
			}
		}
	}
	return Decide(DecisionInput{
		Clean:         clean,
		CurrentBranch: branch,
		HeadSHA:       headSHA,
		PR:            pr,
		BaseDelta:     baseDelta,
		PRFiles:       prFiles,
		MergeTree:     mt,
		Policy:        cfg.Land,
	}), nil
}

func waitChecks(ctx context.Context, f forge.Forge, prNumber int, required []string, maxWait time.Duration, opts Options) (forge.ChecksVerdict, error) {
	poll := opts.ChecksPoll
	if poll <= 0 {
		poll = 5 * time.Second
	}
	grace := opts.CreationGrace
	if grace <= 0 {
		grace = 60 * time.Second
	}
	start := time.Now()
	deadline := start.Add(maxWait)

	for {
		checks, err := f.Checks(ctx, prNumber)
		if err != nil {
			return forge.ChecksVerdict{}, err
		}
		verdict := forge.EvaluateChecks(checks, required)
		waitingForCreation := verdict.Kind == "red" && len(verdict.Failing) == 0 && len(verdict.Missing) > 0 && time.Since(start) < grace
		if verdict.Kind == "green" || (verdict.Kind == "red" && !waitingForCreation) {
			return verdict, nil
		}
		if time.Now().After(deadline) {
			return verdict, nil
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return forge.ChecksVerdict{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func refusalExitKind(clean bool) exitkind.Kind {
	if !clean {
		return exitkind.Classify(false, exitkind.Facts{DirtyTree: true})
	}
	return exitkind.Classify(false, exitkind.Facts{Refused: true})
}

func landExitKind(res Result, err error) exitkind.Kind {
	var te *lock.TimeoutError
	switch {
	case errors.As(err, &te):
		return exitkind.Classify(false, exitkind.Facts{LockTimedOut: true})
	case res.ExitKind != "":
		return res.ExitKind
	case res.OK && err == nil:
		return exitkind.Pass
	default:
		return exitkind.Classify(false, exitkind.Facts{})
	}
}

func firstGlobalMatch(files, patterns []string) string {
	for _, f := range files {
		if glob.MatchAny(patterns, f) {
			return f
		}
	}
	return ""
}

func firstOverlap(a, b []string) string {
	set := make(map[string]struct{}, len(b))
	for _, x := range b {
		set[x] = struct{}{}
	}
	for _, x := range a {
		if _, ok := set[x]; ok {
			return x
		}
	}
	return ""
}

func formatCheckFailure(v forge.ChecksVerdict) string {
	if v.Kind == "pending" {
		return fmt.Sprintf("checks still pending: %v", v.Pending)
	}
	if len(v.Failing) > 0 && len(v.Missing) > 0 {
		return fmt.Sprintf("checks failing: %v; missing: %v", v.Failing, v.Missing)
	}
	if len(v.Failing) > 0 {
		return fmt.Sprintf("checks failing: %v", v.Failing)
	}
	return fmt.Sprintf("required checks missing: %v", v.Missing)
}

func short(sha string) string {
	if len(sha) <= 9 {
		return sha
	}
	return sha[:9]
}
