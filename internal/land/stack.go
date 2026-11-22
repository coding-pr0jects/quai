package land

import (
	"context"
	"errors"
	"fmt"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/forge"
	"github.com/coding-pr0jects/quai/internal/patchid"
	"github.com/coding-pr0jects/quai/internal/repo"
)

type StackedChild struct {
	PR            int             `json:"pr"`
	Branch        string          `json:"branch"`
	Worktree      string          `json:"worktree,omitempty"`
	RetargetedTo  string          `json:"retargeted_to,omitempty"`
	Status        string          `json:"status"` // detected | retargeted | restacked | conflict | dirty-skipped | no-worktree | error
	OldHeadSHA    string          `json:"old_head_sha,omitempty"`
	NewHeadSHA    string          `json:"new_head_sha,omitempty"`
	PatchID       *patchid.Result `json:"patch_id,omitempty"`
	ConflictFiles []string        `json:"conflict_files,omitempty"`
	Hint          string          `json:"hint,omitempty"`
	Error         string          `json:"error,omitempty"`
}

type RestackOptions struct {
	OldUpstream string
	BaseRef     string
	Push        bool
	Remote      string
}

type RestackResult struct {
	OK            bool            `json:"ok"`
	Branch        string          `json:"branch"`
	BaseRef       string          `json:"base_ref"`
	OldUpstream   string          `json:"old_upstream"`
	OldHeadSHA    string          `json:"old_head_sha"`
	NewHeadSHA    string          `json:"new_head_sha,omitempty"`
	Pushed        bool            `json:"pushed,omitempty"`
	PatchID       *patchid.Result `json:"patch_id,omitempty"`
	ConflictFiles []string        `json:"conflict_files,omitempty"`
	Error         string          `json:"error,omitempty"`
}

// Restack replays the current branch's commits after OldUpstream onto BaseRef
// via `git rebase --onto`, reports patch-id survival, and optionally pushes with --force-with-lease.
func Restack(ctx context.Context, g repo.Git, cfg *config.Config, opts RestackOptions) (RestackResult, error) {
	clean, _, err := g.IsClean(ctx)
	if err != nil {
		return RestackResult{}, err
	}
	if !clean {
		return RestackResult{}, errors.New("working tree is dirty; commit or stash changes before restacking")
	}
	branch, err := g.CurrentBranch(ctx)
	if err != nil || branch == "" {
		return RestackResult{}, errors.New("cannot restack a detached HEAD; check out a branch first")
	}
	oldUpstreamSHA, err := g.RevParse(ctx, opts.OldUpstream)
	if err != nil {
		return RestackResult{}, fmt.Errorf("resolve old parent commit %q: %w", opts.OldUpstream, err)
	}
	baseRef := opts.BaseRef
	if baseRef == "" {
		baseName := "main"
		if cfg != nil && cfg.Base != "" {
			baseName = cfg.Base
		}
		baseRef, err = g.ResolveBaseRef(ctx, baseName)
		if err != nil {
			return RestackResult{}, err
		}
	}
	oldHead, err := g.HeadSHA(ctx)
	if err != nil {
		return RestackResult{}, err
	}

	res := RestackResult{
		Branch:      branch,
		BaseRef:     baseRef,
		OldUpstream: oldUpstreamSHA,
		OldHeadSHA:  oldHead,
	}

	if err := g.RebaseOnto(ctx, baseRef, oldUpstreamSHA); err != nil {
		var rce *repo.RebaseConflictError
		if errors.As(err, &rce) {
			res.ConflictFiles = rce.ConflictFiles
			res.Error = rce.Error()
			return res, rce
		}
		return res, err
	}

	newHead, err := g.HeadSHA(ctx)
	if err != nil {
		return res, err
	}
	res.NewHeadSHA = newHead

	// Compare patch-id before (relative to oldUpstreamSHA) vs after (relative to baseRef).
	if oldDiff, err := g.DiffBinary(ctx, oldUpstreamSHA, oldHead); err == nil {
		if newBase, err := g.MergeBase(ctx, baseRef, newHead); err == nil {
			if newDiff, err := g.DiffBinary(ctx, newBase, newHead); err == nil {
				oldPID, err1 := g.PatchID(ctx, oldDiff)
				newPID, err2 := g.PatchID(ctx, newDiff)
				if err1 == nil && err2 == nil {
					st := patchid.StatusChanged
					if oldPID == newPID {
						st = patchid.StatusSurvives
					}
					res.PatchID = &patchid.Result{
						Status:         st,
						VerdictSHA:     oldHead,
						TargetSHA:      newHead,
						VerdictPatchID: oldPID,
						TargetPatchID:  newPID,
					}
				}
			}
		}
	}

	if opts.Push {
		remote := opts.Remote
		if remote == "" {
			remote = "origin"
		}
		if err := g.PushForceWithLease(ctx, remote, branch, oldHead); err != nil {
			res.Error = err.Error()
			return res, err
		}
		res.Pushed = true
	}

	res.OK = true
	return res, nil
}

func discoverChildPRs(ctx context.Context, f forge.Forge, parentBranch string) []forge.PullRequest {
	prs, err := f.ListPRsByBase(ctx, parentBranch)
	if err != nil || len(prs) == 0 {
		return nil
	}
	var out []forge.PullRequest
	for _, c := range prs {
		if c.State == "OPEN" && !c.IsCrossRepository {
			out = append(out, c)
		}
	}
	return out
}

func retargetChildrenBeforeMerge(ctx context.Context, f forge.Forge, children []forge.PullRequest, newBase string) ([]StackedChild, error) {
	if len(children) == 0 {
		return nil, nil
	}
	var out []StackedChild
	for _, c := range children {
		if err := f.EditPRBase(ctx, c.Number, newBase); err != nil {
			return out, fmt.Errorf("retarget stacked child PR #%d (%s) to %s: %w", c.Number, c.HeadRefName, newBase, err)
		}
		out = append(out, StackedChild{
			PR:           c.Number,
			Branch:       c.HeadRefName,
			RetargetedTo: newBase,
			OldHeadSHA:   c.HeadRefOid,
			Status:       "retargeted",
		})
	}
	return out, nil
}

func restackChildrenAfterMerge(
	ctx context.Context,
	g repo.Git,
	cfg *config.Config,
	children []StackedChild,
	newBaseBranch string,
	oldParentTip string,
	doRestack bool,
) []StackedChild {
	if len(children) == 0 {
		return nil
	}
	targetBase := "origin/" + newBaseBranch
	_ = g.Fetch(ctx, "origin", newBaseBranch)

	wtByBranch := make(map[string]string)
	if wts, err := g.Worktrees(ctx); err == nil {
		for _, wt := range wts {
			if !wt.Bare && wt.Branch != "" {
				wtByBranch[wt.Branch] = wt.Path
			}
		}
	}

	for i := range children {
		c := &children[i]
		c.Hint = fmt.Sprintf("quai restack %s --base %s --push", oldParentTip, targetBase)
		wtPath, hasWT := wtByBranch[c.Branch]
		if hasWT {
			c.Worktree = wtPath
		}
		if !doRestack {
			continue
		}
		if !hasWT {
			c.Status = "no-worktree"
			continue
		}
		childGit := repo.NewExecGit(wtPath)
		clean, _, err := childGit.IsClean(ctx)
		if err != nil || !clean {
			c.Status = "dirty-skipped"
			continue
		}
		localHead, err := childGit.HeadSHA(ctx)
		if err != nil {
			c.Status = "error"
			c.Error = err.Error()
			continue
		}
		if c.OldHeadSHA != "" && localHead != c.OldHeadSHA {
			c.Status = "error"
			c.Error = fmt.Sprintf("local HEAD %s differs from remote child PR head %s", short(localHead), short(c.OldHeadSHA))
			continue
		}
		rRes, rErr := Restack(ctx, childGit, cfg, RestackOptions{
			OldUpstream: oldParentTip,
			BaseRef:     targetBase,
			Push:        true,
			Remote:      "origin",
		})
		if rErr != nil {
			var rce *repo.RebaseConflictError
			if errors.As(rErr, &rce) {
				c.Status = "conflict"
				c.ConflictFiles = rce.ConflictFiles
				c.Error = rce.Error()
			} else {
				c.Status = "error"
				c.Error = rErr.Error()
			}
			continue
		}
		c.Status = "restacked"
		c.NewHeadSHA = rRes.NewHeadSHA
		c.PatchID = rRes.PatchID
	}
	return children
}
