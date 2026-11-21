package patchid

import (
	"context"
	"fmt"
	"sort"

	"github.com/smbss1/quai/internal/repo"
)

const (
	StatusSurvives = "SURVIVES"
	StatusChanged  = "CHANGED"
)

type Result struct {
	Status         string   `json:"status"`
	VerdictSHA     string   `json:"verdict_sha"`
	TargetSHA      string   `json:"target_sha"`
	VerdictPatchID string   `json:"verdict_patch_id"`
	TargetPatchID  string   `json:"target_patch_id"`
	ChangedFiles   []string `json:"changed_files"`
}

// Compare computes normalized git patch-ids of verdictRev and targetRev against baseRef
// and reports whether the logical patch survived a rebase unchanged.
func Compare(ctx context.Context, g repo.Git, baseRef, verdictRev, targetRev string) (Result, error) {
	if targetRev == "" {
		targetRev = "HEAD"
	}
	verdictSHA, err := g.RevParse(ctx, verdictRev)
	if err != nil {
		return Result{}, fmt.Errorf("resolve verdict ref %q: %w", verdictRev, err)
	}
	targetSHA, err := g.RevParse(ctx, targetRev)
	if err != nil {
		return Result{}, fmt.Errorf("resolve target ref %q: %w", targetRev, err)
	}
	verdictBase, err := g.MergeBase(ctx, baseRef, verdictSHA)
	if err != nil {
		return Result{}, fmt.Errorf("merge-base(%s, %s): %w", baseRef, verdictSHA, err)
	}
	targetBase, err := g.MergeBase(ctx, baseRef, targetSHA)
	if err != nil {
		return Result{}, fmt.Errorf("merge-base(%s, %s): %w", baseRef, targetSHA, err)
	}

	vDiff, err := g.DiffBinary(ctx, verdictBase, verdictSHA)
	if err != nil {
		return Result{}, err
	}
	tDiff, err := g.DiffBinary(ctx, targetBase, targetSHA)
	if err != nil {
		return Result{}, err
	}
	vID, err := g.PatchID(ctx, vDiff)
	if err != nil {
		return Result{}, err
	}
	tID, err := g.PatchID(ctx, tDiff)
	if err != nil {
		return Result{}, err
	}

	if vID == tID {
		return Result{
			Status:         StatusSurvives,
			VerdictSHA:     verdictSHA,
			TargetSHA:      targetSHA,
			VerdictPatchID: vID,
			TargetPatchID:  tID,
			ChangedFiles:   []string{},
		}, nil
	}

	vFiles, err := g.DiffNameOnly(ctx, verdictBase, verdictSHA)
	if err != nil {
		return Result{}, err
	}
	tFiles, err := g.DiffNameOnly(ctx, targetBase, targetSHA)
	if err != nil {
		return Result{}, err
	}
	union := make(map[string]struct{}, len(vFiles)+len(tFiles))
	for _, f := range vFiles {
		union[f] = struct{}{}
	}
	for _, f := range tFiles {
		union[f] = struct{}{}
	}
	var drifted []string
	for f := range union {
		vp, err := g.DiffFilePatch(ctx, verdictBase, verdictSHA, f)
		if err != nil {
			return Result{}, err
		}
		tp, err := g.DiffFilePatch(ctx, targetBase, targetSHA, f)
		if err != nil {
			return Result{}, err
		}
		vfID, err := g.PatchID(ctx, vp)
		if err != nil {
			return Result{}, err
		}
		tfID, err := g.PatchID(ctx, tp)
		if err != nil {
			return Result{}, err
		}
		if vfID != tfID {
			drifted = append(drifted, f)
		}
	}
	sort.Strings(drifted)
	if drifted == nil {
		drifted = []string{}
	}
	return Result{
		Status:         StatusChanged,
		VerdictSHA:     verdictSHA,
		TargetSHA:      targetSHA,
		VerdictPatchID: vID,
		TargetPatchID:  tID,
		ChangedFiles:   drifted,
	}, nil
}
