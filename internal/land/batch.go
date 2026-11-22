package land

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/forge"
	"github.com/coding-pr0jects/quai/internal/lock"
	"github.com/coding-pr0jects/quai/internal/repo"
)

// BatchStatus is the outcome of one PR in a batch landing.
type BatchStatus string

const (
	BatchLanded   BatchStatus = "landed"
	BatchPlanned  BatchStatus = "planned"
	BatchRefused  BatchStatus = "refused"
	BatchConflict BatchStatus = "conflict"
	BatchFailed   BatchStatus = "failed"
	BatchSkipped  BatchStatus = "skipped"
)

// ErrBasePolicy marks a missing or invalid .quai.toml on origin/<base> during batch landing.
var ErrBasePolicy = errors.New("no valid base policy")

// BatchOptions configures RunBatch. Options carries the per-PR settings; its PRNumber is ignored.
type BatchOptions struct {
	Options
	PRNumbers       []int
	ContinueOnError bool
}

// BatchEntry is the single-PR Result of one batch member plus its batch status.
type BatchEntry struct {
	Result
	Status BatchStatus `json:"status"`
	Error  string      `json:"error,omitempty"`
}

// BatchResult lists every batch member in processing order.
type BatchResult struct {
	OK     bool         `json:"ok"`
	DryRun bool         `json:"dry_run"`
	PRs    []BatchEntry `json:"prs"`
}

// BatchCandidate is what PlanBatch needs to know about one inspected PR.
type BatchCandidate struct {
	PR          int
	BaseRefName string
	HeadRefName string
	Action      Action
}

// PlanBatch orders the landable candidates: direct-merge and optimistic-merge PRs first,
// then the rest with every stacked parent before its children, input order breaking ties.
// Refused candidates are left out.
func PlanBatch(cands []BatchCandidate) []int {
	parentOf := batchParents(cands)
	var first, rest []BatchCandidate
	for _, c := range cands {
		switch {
		case c.Action == ActionRefuse:
		case (c.Action == ActionDirectMerge || c.Action == ActionOptimisticMerge) && parentOf[c.PR] == 0:
			first = append(first, c)
		default:
			rest = append(rest, c)
		}
	}
	order := make([]int, 0, len(first)+len(rest))
	for _, c := range first {
		order = append(order, c.PR)
	}
	pending := make(map[int]bool, len(rest))
	for _, c := range rest {
		pending[c.PR] = true
	}
	for len(rest) > 0 {
		next := 0
		for i, c := range rest {
			if !pending[parentOf[c.PR]] {
				next = i
				break
			}
		}
		c := rest[next]
		order = append(order, c.PR)
		delete(pending, c.PR)
		rest = append(rest[:next], rest[next+1:]...)
	}
	return order
}

func batchParents(cands []BatchCandidate) map[int]int {
	byHead := make(map[string]int, len(cands))
	for _, c := range cands {
		if c.HeadRefName != "" {
			byHead[c.HeadRefName] = c.PR
		}
	}
	parentOf := make(map[int]int, len(cands))
	for _, c := range cands {
		if p, ok := byHead[c.BaseRefName]; ok && p != c.PR {
			parentOf[c.PR] = p
		}
	}
	return parentOf
}

// RunBatch lands several PRs under one land.lock session. The policy comes from
// .quai.toml on origin/<base>, never from a PR branch. Each PR lands from the worktree
// that has its head branch checked out, else from g as Run does off the PR branch, through
// the same guards as Run.
func RunBatch(ctx context.Context, g repo.Git, f forge.Forge, localCfg *config.Config, opts BatchOptions) (BatchResult, error) {
	cfg, err := basePolicy(ctx, g, localCfg.Base)
	if err != nil {
		return BatchResult{}, err
	}
	wtByBranch := make(map[string]string)
	wts, err := g.Worktrees(ctx)
	if err != nil {
		return BatchResult{}, err
	}
	for _, wt := range wts {
		if !wt.Bare && wt.Branch != "" {
			wtByBranch[wt.Branch] = wt.Path
		}
	}

	inspected := make(map[int]BatchEntry, len(opts.PRNumbers))
	gitOf := make(map[int]repo.Git, len(opts.PRNumbers))
	cands := make([]BatchCandidate, 0, len(opts.PRNumbers))
	for _, n := range opts.PRNumbers {
		e, cand, wtGit := inspectBatchPR(ctx, g, f, cfg, opts, n, wtByBranch)
		inspected[n] = e
		gitOf[n] = wtGit
		cands = append(cands, cand)
	}

	order := PlanBatch(cands)
	out := BatchResult{DryRun: opts.DryRun}
	appendUnplanned := func() {
		for _, c := range cands {
			if c.Action == ActionRefuse {
				out.PRs = append(out.PRs, inspected[c.PR])
			}
		}
	}

	if opts.DryRun {
		out.OK = len(order) == len(cands)
		for _, n := range order {
			e := inspected[n]
			out.OK = out.OK && e.OK
			out.PRs = append(out.PRs, e)
		}
		appendUnplanned()
		return out, nil
	}

	if len(order) < len(cands) && !opts.ContinueOnError {
		for _, n := range order {
			out.PRs = append(out.PRs, skipped(inspected[n], "another PR in the batch was refused and --continue-on-error=false"))
		}
		appendUnplanned()
		return out, nil
	}

	lease, err := acquireBatchLock(ctx, g, cfg, opts)
	if err != nil {
		return BatchResult{}, err
	}
	defer lease.Release()

	headOf := make(map[int]string, len(cands))
	for _, c := range cands {
		headOf[c.PR] = c.HeadRefName
	}
	parentOf := batchParents(cands)
	hasChild := make(map[int]bool, len(cands))
	for _, p := range parentOf {
		hasChild[p] = true
	}
	method := cfg.Land.Method
	if opts.Method != "" {
		method = opts.Method
	}
	landed := make(map[int]bool, len(order))
	stopReason := ""
	for _, n := range order {
		if stopReason != "" {
			out.PRs = append(out.PRs, skipped(inspected[n], stopReason))
			continue
		}
		if p := parentOf[n]; p != 0 && !landed[p] {
			out.PRs = append(out.PRs, skipped(inspected[n], fmt.Sprintf("stacked on PR #%d, which did not land", p)))
			continue
		}
		one := opts.Options
		one.PRNumber = n
		one.Restack = opts.Restack || hasChild[n]
		res, err := landBatchPR(ctx, gitOf[n], headOf[n], f, cfg, one, method)
		e := batchEntry(inspected[n], res, err)
		landed[n] = e.Status == BatchLanded
		out.PRs = append(out.PRs, e)
		switch {
		case ctx.Err() != nil:
			stopReason = fmt.Sprintf("interrupted: %v", ctx.Err())
		case !landed[n] && !opts.ContinueOnError:
			stopReason = fmt.Sprintf("PR #%d did not land and --continue-on-error=false", n)
		}
	}
	appendUnplanned()

	out.OK = true
	for _, e := range out.PRs {
		out.OK = out.OK && e.Status == BatchLanded
	}
	return out, ctx.Err()
}

func basePolicy(ctx context.Context, g repo.Git, base string) (*config.Config, error) {
	if err := g.Fetch(ctx, "origin", base); err != nil {
		return nil, err
	}
	ref := "origin/" + base
	raw, err := g.ShowFile(ctx, ref, config.FileName)
	if err != nil {
		return nil, fmt.Errorf("%w: read %s from %s: %v; batch landing reads policy from the base, so commit %s there first", ErrBasePolicy, config.FileName, ref, err, config.FileName)
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrBasePolicy, ref, err)
	}
	return cfg, nil
}

func inspectBatchPR(ctx context.Context, g repo.Git, f forge.Forge, cfg *config.Config, opts BatchOptions, n int, wtByBranch map[string]string) (BatchEntry, BatchCandidate, repo.Git) {
	var pr forge.PullRequest
	refused := func(res Result, errMsg string) (BatchEntry, BatchCandidate, repo.Git) {
		res.DryRun = opts.DryRun
		res.PR = n
		res.Action = ActionRefuse
		cand := BatchCandidate{PR: n, BaseRefName: pr.BaseRefName, HeadRefName: pr.HeadRefName, Action: ActionRefuse}
		return BatchEntry{Result: res, Status: BatchRefused, Error: errMsg}, cand, nil
	}
	pr, err := f.ViewPR(ctx, n)
	if err != nil {
		return refused(Result{Reason: "cannot read PR"}, err.Error())
	}
	wtGit := g
	if wtPath, ok := wtByBranch[pr.HeadRefName]; ok {
		wtGit = repo.NewExecGit(wtPath)
	}
	dry := opts.Options
	dry.PRNumber = n
	dry.DryRun = true
	res, err := Run(ctx, wtGit, f, cfg, dry)
	if err != nil {
		return refused(Result{URL: pr.URL, Reason: "cannot inspect PR"}, err.Error())
	}
	res.DryRun = opts.DryRun
	cand := BatchCandidate{PR: n, BaseRefName: pr.BaseRefName, HeadRefName: pr.HeadRefName, Action: res.Action}
	status := BatchPlanned
	if res.Action == ActionRefuse {
		status = BatchRefused
	}
	return BatchEntry{Result: res, Status: status}, cand, wtGit
}

func landBatchPR(ctx context.Context, g repo.Git, prHead string, f forge.Forge, cfg *config.Config, opts Options, method config.MergeMethod) (Result, error) {
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		return Result{}, err
	}
	branch, err := g.CurrentBranch(ctx)
	if err != nil {
		return Result{}, err
	}
	ws := newWorkspace(g, commonDir, branch, opts.PRNumber, prHead)
	defer ws.close(ctx, opts.Stderr)
	return landLocked(ctx, g, ws, branch, f, cfg, opts, method)
}

func batchEntry(planned BatchEntry, res Result, err error) BatchEntry {
	if res.PR == 0 {
		res.PR = planned.PR
		res.HeadSHA = planned.HeadSHA
		res.URL = planned.URL
		res.Action = planned.Action
	}
	e := BatchEntry{Result: res}
	var rce *repo.RebaseConflictError
	switch {
	case errors.As(err, &rce):
		e.Status = BatchConflict
	case err != nil:
		e.Status = BatchFailed
		e.Error = err.Error()
	case res.Merged:
		e.Status = BatchLanded
	case res.Action == ActionRefuse:
		e.Status = BatchRefused
	default:
		e.Status = BatchFailed
	}
	return e
}

func skipped(planned BatchEntry, reason string) BatchEntry {
	return BatchEntry{
		Result: Result{PR: planned.PR, Action: planned.Action, HeadSHA: planned.HeadSHA, URL: planned.URL, Reason: reason},
		Status: BatchSkipped,
	}
}

func acquireBatchLock(ctx context.Context, g repo.Git, cfg *config.Config, opts BatchOptions) (*lock.Lease, error) {
	topLevel, err := g.TopLevel(ctx)
	if err != nil {
		return nil, err
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		return nil, err
	}
	nums := make([]string, len(opts.PRNumbers))
	for i, n := range opts.PRNumbers {
		nums[i] = strconv.Itoa(n)
	}
	return lock.AcquireDir(ctx, filepath.Join(commonDir, "quai", "locks", "land.lock"), lock.Owner{
		Command:  "quai land " + strings.Join(nums, " "),
		Worktree: topLevel,
	}, lock.Options{
		Wait:         cfg.Land.LockWaitDur,
		PollInterval: 2 * time.Second,
		ReportEvery:  30 * time.Second,
		StatusWriter: opts.Stderr,
	})
}
