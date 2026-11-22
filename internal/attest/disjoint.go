package attest

import (
	"context"
	"fmt"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/glob"
	"github.com/coding-pr0jects/quai/internal/repo"
	"github.com/coding-pr0jects/quai/internal/tier"
)

// Match modes of a verified attestation in CheckResult and StepCheck: the checked-out tree's
// own fingerprint, or the PR head's fingerprint on its old merge base.
const (
	MatchExact        = "exact"
	MatchDisjointBase = "disjoint-base"
)

// DisjointBaseInput holds the facts that decide whether an attestation signed for a PR head
// on its old merge base still covers the checked-out merge of that head with a moved base.
// BaseDelta and HeadFiles are the files changed from OldBaseSHA to the base and to the head;
// HeadToCheckout and BaseToCheckout are the files where the checked-out HEAD differs from them.
type DisjointBaseInput struct {
	OldBaseSHA           string
	BaseSHA              string
	BaseDelta            []string
	HeadFiles            []string
	HeadToCheckout       []string
	BaseToCheckout       []string
	MergeTree            repo.MergeTreeResult
	GlobalPaths          []string
	EscalateOnTestTamper bool
	TestsTampered        []tier.TamperFinding
}

// RefuseDisjointBase returns why a disjoint-base match is unsafe, or "" when every
// optimistic-merge safety condition holds.
func RefuseDisjointBase(in DisjointBaseInput) string {
	if in.OldBaseSHA == in.BaseSHA {
		return "base has not moved since the merge base of --head"
	}
	if !in.MergeTree.Clean {
		return "git merge-tree of --since and --head has conflicts"
	}
	if hit := firstGlobalMatch(in.BaseDelta, in.GlobalPaths); hit != "" {
		return fmt.Sprintf("base modified global path %s", hit)
	}
	if overlap := firstOverlap(in.BaseDelta, in.HeadFiles); overlap != "" {
		return fmt.Sprintf("base and PR both modified %s", overlap)
	}
	if extra := firstOutside(in.HeadToCheckout, in.BaseDelta); extra != "" {
		return fmt.Sprintf("checked-out HEAD is not the merge of --since and --head: %s differs from --head outside the base's changes", extra)
	}
	if extra := firstOutside(in.BaseToCheckout, in.HeadFiles); extra != "" {
		return fmt.Sprintf("checked-out HEAD is not the merge of --since and --head: %s differs from --since outside the PR's changes", extra)
	}
	if in.EscalateOnTestTamper && len(in.TestsTampered) > 0 {
		f := in.TestsTampered[0]
		return fmt.Sprintf("test tamper (%s %s) escalates to a full run", f.Kind, f.Path)
	}
	return ""
}

func gatherDisjointBase(ctx context.Context, g repo.Git, cfg *config.Config, headSHA, baseSHA string) (DisjointBaseInput, error) {
	oldBase, err := g.MergeBase(ctx, headSHA, baseSHA)
	if err != nil {
		return DisjointBaseInput{}, fmt.Errorf("merge-base of --head and --since: %w (fetch the full history, e.g. actions/checkout fetch-depth: 0)", err)
	}
	in := DisjointBaseInput{
		OldBaseSHA:           oldBase,
		BaseSHA:              baseSHA,
		GlobalPaths:          cfg.Land.GlobalPaths,
		EscalateOnTestTamper: cfg.Tiers.EscalateOnTestTamper == nil || *cfg.Tiers.EscalateOnTestTamper,
	}
	for _, d := range []struct {
		from, to string
		into     *[]string
	}{
		{oldBase, baseSHA, &in.BaseDelta},
		{oldBase, headSHA, &in.HeadFiles},
		{headSHA, "HEAD", &in.HeadToCheckout},
		{baseSHA, "HEAD", &in.BaseToCheckout},
	} {
		if *d.into, err = g.DiffNameOnly(ctx, d.from, d.to); err != nil {
			return in, err
		}
	}
	if in.MergeTree, err = g.MergeTree(ctx, baseSHA, headSHA); err != nil {
		return in, err
	}
	if in.EscalateOnTestTamper {
		for _, to := range []string{baseSHA, headSHA} {
			_, tampered, err := tier.ScanDiff(ctx, g, cfg.Tiers, oldBase, to)
			if err != nil {
				return in, err
			}
			in.TestsTampered = append(in.TestsTampered, tampered...)
		}
	}
	return in, nil
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

func firstOutside(files, allowed []string) string {
	set := make(map[string]struct{}, len(allowed))
	for _, x := range allowed {
		set[x] = struct{}{}
	}
	for _, f := range files {
		if _, ok := set[f]; !ok {
			return f
		}
	}
	return ""
}
