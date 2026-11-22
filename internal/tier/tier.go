package tier

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/glob"
	"github.com/coding-pr0jects/quai/internal/receipt"
	"github.com/coding-pr0jects/quai/internal/repo"
)

var defaultTestGlobs = []string{
	"**/*_test.*",
	"**/*.test.*",
	"**/*.spec.*",
	"**/*.e2e.*",
	"**/test_*.py",
}

var skipMarkers = []string{
	".skip(",
	"it.skip",
	"describe.skip",
	"test.skip",
	"t.Skip(",
	"t.SkipNow(",
	"@pytest.mark.skip",
	"#[ignore]",
}

type TamperFinding struct {
	Path   string `json:"path"`
	Kind   string `json:"kind"` // deleted | skip_added
	Marker string `json:"marker,omitempty"`
}

type ReceiptStatus struct {
	Valid       bool       `json:"valid"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
}

type Decision struct {
	Tier          config.Tier
	Triggers      []string
	PureRenames   int
	TestsTampered []TamperFinding
}

type Result struct {
	Tier          config.Tier              `json:"tier"`
	BaseSHA       string                   `json:"base_sha"`
	TreeSHA       string                   `json:"tree_sha"`
	Clean         bool                     `json:"clean"`
	Triggers      []string                 `json:"triggers"`
	PureRenames   int                      `json:"pure_renames"`
	TestsTampered []TamperFinding          `json:"tests_tampered"`
	Receipts      map[string]ReceiptStatus `json:"receipts"`
}

func tierRank(t config.Tier) int {
	switch t {
	case config.TierFullLive:
		return 3
	case config.TierDiffAudit:
		return 2
	default:
		return 1
	}
}

func effectiveTestGlobs(policy config.TierPolicy) []string {
	if len(policy.TestGlobs) > 0 {
		return policy.TestGlobs
	}
	return defaultTestGlobs
}

// DetectTestTamper inspects deleted test files and added skip markers in test diffs.
func DetectTestTamper(changes []repo.FileChange, patches map[string][]byte, policy config.TierPolicy) []TamperFinding {
	testGlobs := effectiveTestGlobs(policy)
	var findings []TamperFinding
	for _, ch := range changes {
		if !glob.MatchAny(testGlobs, ch.Path) {
			continue
		}
		if ch.Status == "D" {
			findings = append(findings, TamperFinding{Path: ch.Path, Kind: "deleted"})
			continue
		}
		patch := patches[ch.Path]
		if len(patch) == 0 {
			continue
		}
		if marker := findAddedSkipMarker(string(patch)); marker != "" {
			findings = append(findings, TamperFinding{Path: ch.Path, Kind: "skip_added", Marker: marker})
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Kind < findings[j].Kind
	})
	return findings
}

func findAddedSkipMarker(diffText string) string {
	for _, line := range strings.Split(diffText, "\n") {
		if !strings.HasPrefix(line, "+") || strings.HasPrefix(line, "+++") {
			continue
		}
		added := line[1:]
		for _, m := range skipMarkers {
			if strings.Contains(added, m) {
				return m
			}
		}
	}
	return ""
}

// Classify is the pure risk-tier decision engine.
func Classify(changes []repo.FileChange, untracked []string, tampered []TamperFinding, policy config.TierPolicy) Decision {
	pureRenames := 0
	pureGate := policy.PureRenamesGateOnly == nil || *policy.PureRenamesGateOnly
	escalateTamper := policy.EscalateOnTestTamper == nil || *policy.EscalateOnTestTamper

	best := config.TierGateOnly
	byTier := map[config.Tier][]string{}

	classifyPath := func(path string, isPureRename bool) {
		var assigned config.Tier
		switch {
		case pureGate && isPureRename:
			assigned = config.TierGateOnly
		case glob.MatchAny(policy.FullLive, path):
			assigned = config.TierFullLive
		case glob.MatchAny(policy.DiffAudit, path):
			assigned = config.TierDiffAudit
		case glob.MatchAny(policy.GateOnly, path):
			assigned = config.TierGateOnly
		default:
			assigned = policy.Default
			if assigned == "" {
				assigned = config.TierDiffAudit
			}
		}
		if tierRank(assigned) > tierRank(best) {
			best = assigned
		}
		byTier[assigned] = append(byTier[assigned], path)
	}

	for _, ch := range changes {
		isPure := ch.Status == "R100"
		if isPure {
			pureRenames++
		}
		classifyPath(ch.Path, isPure)
	}
	for _, u := range untracked {
		classifyPath(u, false)
	}

	if escalateTamper && len(tampered) > 0 {
		best = config.TierFullLive
		for _, f := range tampered {
			byTier[config.TierFullLive] = append(byTier[config.TierFullLive], f.Path)
		}
	}

	triggers := dedupSorted(byTier[best])
	if tampered == nil {
		tampered = []TamperFinding{}
	}
	return Decision{
		Tier:          best,
		Triggers:      triggers,
		PureRenames:   pureRenames,
		TestsTampered: tampered,
	}
}

// ScanDiff lists the changes between from and to (the working tree when to is empty)
// and the test-tamper findings among them.
func ScanDiff(ctx context.Context, g repo.Git, policy config.TierPolicy, from, to string) ([]repo.FileChange, []TamperFinding, error) {
	changes, err := g.DiffNameStatus(ctx, from, to, true)
	if err != nil {
		return nil, nil, err
	}
	testGlobs := effectiveTestGlobs(policy)
	patches := make(map[string][]byte)
	for _, ch := range changes {
		if ch.Status == "D" || ch.Status == "R100" {
			continue
		}
		if glob.MatchAny(testGlobs, ch.Path) {
			p, err := g.DiffFilePatch(ctx, from, to, ch.Path)
			if err != nil {
				return nil, nil, err
			}
			patches[ch.Path] = p
		}
	}
	return changes, DetectTestTamper(changes, patches, policy), nil
}

// Evaluate computes the branch tier, test-tamper findings, and receipt statuses from git.
func Evaluate(ctx context.Context, g repo.Git, cfg *config.Config, since string) (Result, error) {
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		return Result{}, err
	}
	treeSHA, err := g.TreeSHA(ctx, "HEAD")
	if err != nil {
		return Result{}, err
	}
	baseRef := since
	if baseRef == "" {
		baseRef, err = g.ResolveBaseRef(ctx, cfg.Base)
		if err != nil {
			baseRef = "HEAD"
		}
	}
	baseSHA, err := g.MergeBase(ctx, baseRef, "HEAD")
	if err != nil {
		return Result{}, fmt.Errorf("merge-base with %s: %w", baseRef, err)
	}
	clean, _, err := g.IsClean(ctx)
	if err != nil {
		return Result{}, err
	}

	to := "HEAD"
	var untracked []string
	if !clean {
		to = ""
		untracked, err = g.UntrackedFiles(ctx)
		if err != nil {
			return Result{}, err
		}
	}
	changes, tampered, err := ScanDiff(ctx, g, cfg.Tiers, baseSHA, to)
	if err != nil {
		return Result{}, err
	}
	dec := Classify(changes, untracked, tampered, cfg.Tiers)

	store := receipt.NewStore(commonDir)
	receipts := make(map[string]ReceiptStatus, len(cfg.Steps))
	for _, step := range cfg.Steps {
		if !clean {
			receipts[step.Name] = ReceiptStatus{Valid: false}
			continue
		}
		fp, err := receipt.Fingerprint(ctx, g, step, treeSHA, baseSHA)
		if err != nil {
			return Result{}, err
		}
		r, ok, err := store.Lookup(step.Name, fp)
		if err != nil {
			return Result{}, err
		}
		if ok {
			ts := r.CreatedAt
			receipts[step.Name] = ReceiptStatus{Valid: true, Fingerprint: fp, CreatedAt: &ts}
		} else {
			receipts[step.Name] = ReceiptStatus{Valid: false, Fingerprint: fp}
		}
	}

	return Result{
		Tier:          dec.Tier,
		BaseSHA:       baseSHA,
		TreeSHA:       treeSHA,
		Clean:         clean,
		Triggers:      dec.Triggers,
		PureRenames:   dec.PureRenames,
		TestsTampered: dec.TestsTampered,
		Receipts:      receipts,
	}, nil
}

func dedupSorted(in []string) []string {
	if len(in) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
