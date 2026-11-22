package conflicts

import (
	"context"
	"fmt"
	"sort"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/repo"
)

type Options struct {
	BaseRef       string
	ConflictsOnly bool
}

type WorktreeInfo struct {
	Path         string   `json:"path"`
	Branch       string   `json:"branch,omitempty"`
	Head         string   `json:"head"`
	Clean        bool     `json:"clean"`
	ChangedFiles []string `json:"changed_files,omitempty"`
}

type BaseConflict struct {
	Worktree      string   `json:"worktree"`
	Branch        string   `json:"branch,omitempty"`
	BaseRef       string   `json:"base_ref"`
	ConflictFiles []string `json:"conflict_files"`
}

type PairCollision struct {
	WorktreeA     string   `json:"worktree_a"`
	BranchA       string   `json:"branch_a,omitempty"`
	WorktreeB     string   `json:"worktree_b"`
	BranchB       string   `json:"branch_b,omitempty"`
	Conflict      bool     `json:"conflict"`
	ConflictFiles []string `json:"conflict_files,omitempty"`
	OverlapFiles  []string `json:"overlap_files,omitempty"`
}

type Result struct {
	OK            bool            `json:"ok"`
	BaseRef       string          `json:"base_ref"`
	BaseSHA       string          `json:"base_sha"`
	Worktrees     []WorktreeInfo  `json:"worktrees"`
	BaseConflicts []BaseConflict  `json:"base_conflicts,omitempty"`
	Collisions    []PairCollision `json:"collisions,omitempty"`
}

// Evaluate inspects all active linked worktrees in the repository, running read-only
// virtual `git merge-tree` checks against base and pairwise across worktrees plus file-overlap checks.
func Evaluate(ctx context.Context, g repo.Git, cfg *config.Config, opts Options) (Result, error) {
	baseRef := opts.BaseRef
	if baseRef == "" {
		baseName := "main"
		if cfg != nil && cfg.Base != "" {
			baseName = cfg.Base
		}
		var err error
		baseRef, err = g.ResolveBaseRef(ctx, baseName)
		if err != nil {
			return Result{}, err
		}
	}
	baseSHA, err := g.RevParse(ctx, baseRef)
	if err != nil {
		return Result{}, fmt.Errorf("resolve base %q: %w", baseRef, err)
	}

	rawWts, err := g.Worktrees(ctx)
	if err != nil {
		return Result{}, err
	}

	type activeWT struct {
		info    WorktreeInfo
		fileSet map[string]struct{}
	}

	res := Result{
		OK:      true,
		BaseRef: baseRef,
		BaseSHA: baseSHA,
	}

	var active []activeWT
	for _, wt := range rawWts {
		if wt.Bare || wt.Head == "" {
			continue
		}
		wtGit := repo.NewExecGit(wt.Path)
		clean, _, err := wtGit.IsClean(ctx)
		if err != nil {
			clean = true
		}

		fileSet := make(map[string]struct{})
		var mergeBase string
		if mb, err := g.MergeBase(ctx, baseSHA, wt.Head); err == nil {
			mergeBase = mb
			if committed, err := g.DiffNameOnly(ctx, mb, wt.Head); err == nil {
				for _, f := range committed {
					fileSet[f] = struct{}{}
				}
			}
		}
		if !clean {
			if dirty, err := wtGit.DiffNameOnly(ctx, "HEAD", ""); err == nil {
				for _, f := range dirty {
					fileSet[f] = struct{}{}
				}
			}
			if untracked, err := wtGit.UntrackedFiles(ctx); err == nil {
				for _, f := range untracked {
					fileSet[f] = struct{}{}
				}
			}
		}

		changed := sortedKeys(fileSet)
		info := WorktreeInfo{
			Path:         wt.Path,
			Branch:       wt.Branch,
			Head:         wt.Head,
			Clean:        clean,
			ChangedFiles: changed,
		}
		res.Worktrees = append(res.Worktrees, info)

		// Skip clean worktrees sitting directly on baseSHA.
		if wt.Head == baseSHA && clean {
			continue
		}

		// Check virtual merge-tree against base when base has advanced past mergeBase.
		if wt.Head != baseSHA && mergeBase != "" && mergeBase != baseSHA {
			mt, err := g.MergeTree(ctx, baseSHA, wt.Head)
			if err != nil {
				return Result{}, fmt.Errorf("merge-tree(%s, %s): %w", baseRef, wt.Path, err)
			}
			if !mt.Clean {
				res.OK = false
				res.BaseConflicts = append(res.BaseConflicts, BaseConflict{
					Worktree:      wt.Path,
					Branch:        wt.Branch,
					BaseRef:       baseRef,
					ConflictFiles: mt.ConflictFiles,
				})
			}
		}

		active = append(active, activeWT{info: info, fileSet: fileSet})
	}

	// Pairwise overlap and virtual merge-tree checks across active feature worktrees.
	for i := 0; i < len(active); i++ {
		for j := i + 1; j < len(active); j++ {
			a, b := active[i], active[j]
			overlap := intersectKeys(a.fileSet, b.fileSet)

			var conflict bool
			var conflictFiles []string
			if a.info.Head != b.info.Head {
				mt, err := g.MergeTree(ctx, a.info.Head, b.info.Head)
				if err != nil {
					return Result{}, fmt.Errorf("merge-tree(%s, %s): %w", a.info.Path, b.info.Path, err)
				}
				if !mt.Clean {
					conflict = true
					conflictFiles = mt.ConflictFiles
				}
			}

			if conflict || (!opts.ConflictsOnly && len(overlap) > 0) {
				res.OK = false
				res.Collisions = append(res.Collisions, PairCollision{
					WorktreeA:     a.info.Path,
					BranchA:       a.info.Branch,
					WorktreeB:     b.info.Path,
					BranchB:       b.info.Branch,
					Conflict:      conflict,
					ConflictFiles: conflictFiles,
					OverlapFiles:  overlap,
				})
			}
		}
	}

	return res, nil
}

func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func intersectKeys(a, b map[string]struct{}) []string {
	var out []string
	for k := range a {
		if _, ok := b[k]; ok {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
