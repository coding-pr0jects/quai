package land

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
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

const (
	ActionRefuse          Action = "refuse"
	ActionDirectMerge     Action = "direct-merge"
	ActionOptimisticMerge Action = "optimistic-merge"
	ActionRebaseAndMerge  Action = "rebase-and-merge"
)

type DecisionInput struct {
	Clean         bool
	CurrentBranch string
	LocalHeadSHA  string
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
}

// Decide is the pure state machine that selects direct-merge, optimistic-merge,
// rebase-and-merge, or refuse from repository and PR facts.
func Decide(in DecisionInput) Decision {
	if !in.Clean {
		return Decision{Action: ActionRefuse, Reason: "working tree is dirty; commit or stash changes first"}
	}
	if in.PR.State != "OPEN" {
		return Decision{Action: ActionRefuse, Reason: fmt.Sprintf("PR #%d is %s, not OPEN", in.PR.Number, in.PR.State)}
	}
	if in.PR.IsDraft {
		return Decision{Action: ActionRefuse, Reason: fmt.Sprintf("PR #%d is a draft; mark it ready first", in.PR.Number)}
	}
	if in.PR.IsCrossRepository {
		return Decision{Action: ActionRefuse, Reason: fmt.Sprintf("PR #%d is from a fork; refusing to land untrusted fork", in.PR.Number)}
	}
	if in.CurrentBranch != in.PR.HeadRefName {
		return Decision{Action: ActionRefuse, Reason: fmt.Sprintf("checked-out branch %q does not match PR head %q", in.CurrentBranch, in.PR.HeadRefName)}
	}
	if in.LocalHeadSHA != in.PR.HeadRefOid {
		return Decision{Action: ActionRefuse, Reason: fmt.Sprintf("local HEAD %s differs from remote PR head %s; push or pull first", short(in.LocalHeadSHA), short(in.PR.HeadRefOid))}
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

// Run executes quai land for one pull request, holding land.lock across rebase, verify, push, and merge.
func Run(ctx context.Context, g repo.Git, f forge.Forge, cfg *config.Config, opts Options) (Result, error) {
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

	// Pre-lock fast validation so obviously invalid calls never wait on land.lock.
	clean, _, err := g.IsClean(ctx)
	if err != nil {
		return Result{}, err
	}
	branch, err := g.CurrentBranch(ctx)
	if err != nil {
		return Result{}, err
	}
	headSHA, err := g.HeadSHA(ctx)
	if err != nil {
		return Result{}, err
	}
	pr, err := f.ViewPR(ctx, opts.PRNumber)
	if err != nil {
		return Result{}, err
	}
	preDec := Decide(DecisionInput{
		Clean:         clean,
		CurrentBranch: branch,
		LocalHeadSHA:  headSHA,
		PR:            pr,
		Policy:        cfg.Land,
	})
	if preDec.Action == ActionRefuse {
		return Result{
			OK:      false,
			DryRun:  opts.DryRun,
			PR:      opts.PRNumber,
			Action:  ActionRefuse,
			Reason:  preDec.Reason,
			HeadSHA: headSHA,
			URL:     pr.URL,
		}, nil
	}

	if opts.DryRun {
		return runDry(ctx, g, f, cfg, opts, pr, headSHA)
	}

	lockDir := filepath.Join(commonDir, "quai", "locks", "land.lock")
	lease, err := lock.AcquireDir(ctx, lockDir, lock.Owner{
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
		headSHA, err = g.HeadSHA(ctx)
		if err != nil {
			return Result{}, err
		}
		clean, _, err = g.IsClean(ctx)
		if err != nil {
			return Result{}, err
		}
		branch, err = g.CurrentBranch(ctx)
		if err != nil {
			return Result{}, err
		}

		dec, err := computeDecision(ctx, g, cfg, clean, branch, headSHA, pr, baseTip)
		if err != nil {
			return Result{}, err
		}
		if dec.Action == ActionRefuse {
			return Result{
				OK:      false,
				PR:      opts.PRNumber,
				Action:  ActionRefuse,
				Reason:  dec.Reason,
				HeadSHA: headSHA,
				URL:     pr.URL,
			}, nil
		}

		if !opts.JSON && opts.Stdout != nil {
			fmt.Fprintf(opts.Stdout, "land #%d: %s (%s)\n", pr.Number, dec.Action, dec.Reason)
		}

		var patchRes *patchid.Result
		if dec.Action == ActionRebaseAndMerge {
			preRebaseSHA := headSHA
			if err := g.Rebase(ctx, "origin/"+pr.BaseRefName); err != nil {
				var rce *repo.RebaseConflictError
				if errors.As(err, &rce) {
					return Result{
						OK:            false,
						PR:            opts.PRNumber,
						Action:        ActionRebaseAndMerge,
						Reason:        rce.Error(),
						HeadSHA:       preRebaseSHA,
						URL:           pr.URL,
						ConflictFiles: rce.ConflictFiles,
					}, rce
				}
				return Result{}, err
			}
			newHead, err := g.HeadSHA(ctx)
			if err != nil {
				return Result{}, err
			}
			pid, err := patchid.Compare(ctx, g, "origin/"+pr.BaseRefName, preRebaseSHA, newHead)
			if err == nil {
				patchRes = &pid
			}

			if len(cfg.Steps) > 0 {
				doSign, doPush, bestEffort := verify.ResolveAutoAttest(cfg, false, opts.NoSign, false, opts.NoPush)
				vRes, vErr := verify.Run(ctx, g, cfg, verify.Options{
					Since:          "origin/" + pr.BaseRefName,
					Sign:           doSign,
					BestEffortSign: bestEffort,
					Push:           doPush,
					JSON:           opts.JSON,
					Stdout:         opts.Stdout,
					Stderr:         opts.Stderr,
				})
				if vErr != nil {
					return Result{
						OK:      false,
						PR:      opts.PRNumber,
						Action:  ActionRebaseAndMerge,
						Reason:  fmt.Sprintf("verification failed after rebase: %v", vErr),
						HeadSHA: newHead,
						URL:     pr.URL,
						PatchID: patchRes,
					}, vErr
				}
				if !vRes.OK {
					return Result{
						OK:      false,
						PR:      opts.PRNumber,
						Action:  ActionRebaseAndMerge,
						Reason:  "verification failed after rebase",
						HeadSHA: newHead,
						URL:     pr.URL,
						PatchID: patchRes,
					}, nil
				}
			}

			if err := g.PushForceWithLease(ctx, "origin", pr.HeadRefName, preRebaseSHA); err != nil {
				return Result{}, err
			}
			headSHA = newHead
		}

		verdictRes, vRefuse, err := enforceVerdict(ctx, g, cfg, "origin/"+pr.BaseRefName, opts)
		if err != nil {
			return Result{}, err
		}
		if vRefuse != "" {
			return Result{
				OK:      false,
				PR:      opts.PRNumber,
				Action:  ActionRefuse,
				Reason:  vRefuse,
				HeadSHA: headSHA,
				URL:     pr.URL,
				PatchID: patchRes,
				Verdict: verdictRes,
			}, nil
		}

		verdictChecks, err := waitChecks(ctx, f, pr.Number, cfg.Land.RequiredChecks, cfg.Land.ChecksWaitDur, opts)
		if err != nil {
			return Result{}, err
		}
		if verdictChecks.Kind != "green" {
			return Result{
				OK:      false,
				PR:      opts.PRNumber,
				Action:  dec.Action,
				Reason:  formatCheckFailure(verdictChecks),
				HeadSHA: headSHA,
				URL:     pr.URL,
				PatchID: patchRes,
				Checks:  &verdictChecks,
				Verdict: verdictRes,
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

func runDry(ctx context.Context, g repo.Git, f forge.Forge, cfg *config.Config, opts Options, pr forge.PullRequest, headSHA string) (Result, error) {
	_ = g.Fetch(ctx, "origin", pr.BaseRefName)
	baseTip, err := g.RevParse(ctx, "origin/"+pr.BaseRefName)
	if err != nil {
		baseTip, err = g.RevParse(ctx, pr.BaseRefName)
		if err != nil {
			return Result{}, err
		}
	}
	dec, err := computeDecision(ctx, g, cfg, true, pr.HeadRefName, headSHA, pr, baseTip)
	if err != nil {
		return Result{}, err
	}
	checks, err := f.Checks(ctx, pr.Number)
	if err != nil {
		return Result{}, err
	}
	verdictChecks := forge.EvaluateChecks(checks, cfg.Land.RequiredChecks)
	verdictRes, vRefuse, err := enforceVerdict(ctx, g, cfg, baseTip, opts)
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
	return Result{
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
	}, nil
}

func enforceVerdict(ctx context.Context, g repo.Git, cfg *config.Config, since string, opts Options) (*verdict.CheckResult, string, error) {
	if !opts.RequireVerdict && len(cfg.Land.RequireVerdictTiers) == 0 {
		return nil, "", nil
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
	vRes, err := verdict.Check(ctx, g, cfg, verdict.CheckOptions{
		Since:          since,
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
		LocalHeadSHA:  headSHA,
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
