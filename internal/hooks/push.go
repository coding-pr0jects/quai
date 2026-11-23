package hooks

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/coding-pr0jects/quai/internal/attest"
	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/glob"
	"github.com/coding-pr0jects/quai/internal/receipt"
	"github.com/coding-pr0jects/quai/internal/repo"
)

const (
	// EnvAllowUnverifiedPush downgrades [hooks.pre_push].on_missing = "refuse" to a warning.
	EnvAllowUnverifiedPush = "QUAI_ALLOW_UNVERIFIED_PUSH"

	branchRefPrefix      = "refs/heads/"
	attestationRefPrefix = "refs/quai/records/"
	zeroSHA              = "0000000000000000000000000000000000000000"
)

var sha40Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

// PushRef is one ref update git hands the pre-push hook on stdin.
type PushRef struct {
	LocalRef  string
	LocalSHA  string
	RemoteRef string
	RemoteSHA string
}

// BranchTarget is a branch commit a push creates or updates.
type BranchTarget struct {
	Ref    string `json:"ref"`
	Commit string `json:"commit"`
}

// ParsePushRefs parses git's pre-push stdin protocol: four space-separated fields per line.
func ParsePushRefs(r io.Reader) ([]PushRef, error) {
	scanner := bufio.NewScanner(r)
	var refs []PushRef
	for line := 1; scanner.Scan(); line++ {
		fields := strings.Fields(scanner.Text())
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 4 {
			return nil, fmt.Errorf("malformed pre-push input on line %d: expected 4 fields, got %d", line, len(fields))
		}
		if !sha40Re.MatchString(fields[1]) || !sha40Re.MatchString(fields[3]) {
			return nil, fmt.Errorf("malformed pre-push input on line %d: invalid SHA", line)
		}
		refs = append(refs, PushRef{
			LocalRef:  fields[0],
			LocalSHA:  fields[1],
			RemoteRef: fields[2],
			RemoteSHA: fields[3],
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read pre-push input: %w", err)
	}
	return refs, nil
}

// ReadPushRefs reads git's pre-push stdin protocol. Only a real terminal means a manual
// invocation, so /dev/null (an old managed hook) and empty pipes are read as no refs.
func ReadPushRefs(stdin io.Reader) (refs []PushRef, provided bool, err error) {
	if f, ok := stdin.(*os.File); ok && isTerminal(f.Fd()) {
		return nil, false, nil
	}
	refs, err = ParsePushRefs(stdin)
	return refs, true, err
}

// BranchTargets returns the branch creations and updates among refs. Tag pushes, branch
// deletions, and attestation pushes map to nothing.
func BranchTargets(refs []PushRef) []BranchTarget {
	var out []BranchTarget
	for _, r := range refs {
		if !strings.HasPrefix(r.RemoteRef, branchRefPrefix) || r.LocalSHA == zeroSHA {
			continue
		}
		out = append(out, BranchTarget{Ref: r.RemoteRef, Commit: r.LocalSHA})
	}
	return out
}

// IsAttestationPush reports whether every ref updates the attestation namespace, which never needs the hook.
func IsAttestationPush(refs []PushRef) bool {
	if len(refs) == 0 {
		return false
	}
	for _, r := range refs {
		if !strings.HasPrefix(r.RemoteRef, attestationRefPrefix) {
			return false
		}
	}
	return true
}

// CachedStep records the pre-push receipt check for one required step at one pushed commit.
type CachedStep struct {
	Step              string `json:"step"`
	Commit            string `json:"commit"`
	Ref               string `json:"ref"`
	Status            string `json:"status"`
	Fingerprint       string `json:"fingerprint,omitempty"`
	AttestationRef    string `json:"attestation_ref,omitempty"`
	AttestationPushed bool   `json:"attestation_pushed,omitempty"`
}

const (
	statusCached  = "cached"
	statusSkipped = "skipped"
	statusMissing = "missing"
)

// CachedReport is the outcome of checking [hooks.pre_push].require_cached for every pushed branch commit.
type CachedReport struct {
	Steps     []CachedStep `json:"steps"`
	Missing   []CachedStep `json:"missing,omitempty"`
	Pushed    []string     `json:"attestations_pushed,omitempty"`
	PushError string       `json:"attestation_push_error,omitempty"`
}

type cacheStatus int

const (
	cacheMiss cacheStatus = iota
	cacheHit
	cacheSkippedInputs
)

type requirementDecision struct {
	blocked  bool
	bypassed bool
	message  string
}

// CheckCached verifies that every require_cached step has a fresh local receipt for each pushed
// branch commit and pushes attestation refs whose local record the remote lacks. It reads
// receipts and trees only: no step runs and no slot is acquired.
func CheckCached(ctx context.Context, g repo.Git, cfg *config.Config, targets []BranchTarget, remote string) (CachedReport, error) {
	report := CachedReport{Steps: []CachedStep{}}
	steps := cfg.RequireCachedSteps()
	if len(steps) == 0 || len(targets) == 0 {
		return report, nil
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		return report, err
	}
	store := receipt.NewStore(commonDir)
	baseRef, err := g.ResolveBaseRef(ctx, cfg.Base)
	if err != nil {
		return report, fmt.Errorf("resolve base ref %q: %w", cfg.Base, err)
	}
	if remote == "" {
		remote = cfg.Attest.Remote
	}

	baseByCommit := make(map[string]string, len(targets))
	for _, target := range targets {
		baseSHA, ok := baseByCommit[target.Commit]
		if !ok {
			baseSHA, err = g.MergeBase(ctx, baseRef, target.Commit)
			if err != nil {
				return report, fmt.Errorf("resolve merge-base of %s with %s: %w", short(target.Commit), baseRef, err)
			}
			baseByCommit[target.Commit] = baseSHA
		}
		for _, step := range steps {
			entry := CachedStep{Step: step.Name, Commit: target.Commit, Ref: target.Ref}
			status, fp, err := cachedStatus(ctx, g, store, step, target.Commit, baseSHA)
			if err != nil {
				return report, err
			}
			entry.Fingerprint = fp
			switch status {
			case cacheHit:
				entry.Status = statusCached
				entry.AttestationRef = attest.RefName(step.Name, fp)
			case cacheSkippedInputs:
				entry.Status = statusSkipped
			default:
				entry.Status = statusMissing
				report.Missing = append(report.Missing, entry)
			}
			report.Steps = append(report.Steps, entry)
		}
	}

	report.Pushed, report.PushError = pushMissingAttestations(ctx, g, remote, report.Steps)
	if len(report.Pushed) > 0 {
		pushed := make(map[string]struct{}, len(report.Pushed))
		for _, ref := range report.Pushed {
			pushed[ref] = struct{}{}
		}
		for i := range report.Steps {
			if _, ok := pushed[report.Steps[i].AttestationRef]; ok {
				report.Steps[i].AttestationPushed = true
			}
		}
	}
	return report, nil
}

func cachedStatus(ctx context.Context, g repo.Git, store *receipt.Store, step config.Step, commit, baseSHA string) (cacheStatus, string, error) {
	changed, err := g.DiffNameOnly(ctx, baseSHA, commit)
	if err != nil {
		return cacheMiss, "", fmt.Errorf("diff %s..%s: %w", short(baseSHA), short(commit), err)
	}
	if len(changed) == 0 {
		// An empty diff changes no input, so the step cannot have been affected.
		return cacheSkippedInputs, "", nil
	}
	relevant := false
	for _, path := range changed {
		if glob.MatchAny(step.Inputs, path) || glob.MatchAny(step.Lockfiles, path) {
			relevant = true
			break
		}
	}
	if !relevant {
		return cacheSkippedInputs, "", nil
	}
	treeSHA, err := g.TreeSHA(ctx, commit)
	if err != nil {
		return cacheMiss, "", fmt.Errorf("resolve tree of %s: %w", short(commit), err)
	}
	fp, err := receipt.Fingerprint(ctx, g, step, treeSHA, baseSHA)
	if err != nil {
		return cacheMiss, "", err
	}
	if _, hit, err := store.LookupFresh(step.Name, fp); err != nil {
		return cacheMiss, "", err
	} else if hit {
		return cacheHit, fp, nil
	}
	return cacheMiss, fp, nil
}

// pushMissingAttestations pushes the attestation ref of every cached step whose local record the
// remote lacks, so CI sees it before the branch objects arrive. A failed sync never blocks the push.
func pushMissingAttestations(ctx context.Context, g repo.Git, remote string, steps []CachedStep) ([]string, string) {
	var local []string
	seen := make(map[string]struct{}, len(steps))
	for _, entry := range steps {
		if entry.Status != statusCached || entry.Fingerprint == "" {
			continue
		}
		if _, ok := seen[entry.AttestationRef]; ok {
			continue
		}
		_, exists, err := attest.ReadRecord(ctx, g, entry.Step, entry.Fingerprint)
		if err != nil {
			return nil, fmt.Sprintf("read attestation %s: %v", entry.AttestationRef, err)
		}
		if !exists {
			continue
		}
		seen[entry.AttestationRef] = struct{}{}
		local = append(local, entry.AttestationRef)
	}
	if len(local) == 0 {
		return nil, ""
	}
	onRemote, err := g.ListRemoteRefs(ctx, remote, local)
	if err != nil {
		return nil, fmt.Sprintf("list attestation refs on %s: %v", remote, err)
	}
	existing := make(map[string]struct{}, len(onRemote))
	for _, ref := range onRemote {
		existing[ref] = struct{}{}
	}
	var missing []string
	for _, ref := range local {
		if _, ok := existing[ref]; !ok {
			missing = append(missing, ref)
		}
	}
	if len(missing) == 0 {
		return nil, ""
	}
	if err := g.PushRefs(ctx, remote, missing); err != nil {
		return nil, err.Error()
	}
	return missing, ""
}

func decideRequirement(missing []CachedStep, policy config.PrePushPolicy, allowUnverified, unknownRef bool) requirementDecision {
	details := make([]string, 0, len(missing))
	for _, m := range missing {
		details = append(details, fmt.Sprintf("no cached receipt for step %q at %s on %s", m.Step, short(m.Commit), m.Ref))
	}
	if unknownRef {
		details = append([]string{"the pushed ref is unknown because stdin carried no refs"}, details...)
	}
	if len(details) == 0 {
		return requirementDecision{}
	}
	detail := strings.Join(details, "; ")
	if allowUnverified && policy.OnMissing == config.OnMissingRefuse {
		return requirementDecision{
			bypassed: true,
			message:  fmt.Sprintf("%s=1 bypasses [hooks.pre_push].on_missing = %q: %s; run quai verify before pushing when the branch is ready", EnvAllowUnverifiedPush, config.OnMissingRefuse, detail),
		}
	}
	if policy.OnMissing == config.OnMissingRefuse {
		if unknownRef {
			return requirementDecision{
				blocked: true,
				message: fmt.Sprintf("%s; run quai hooks install so the hook receives the pushed refs, then push again", detail),
			}
		}
		return requirementDecision{
			blocked: true,
			message: fmt.Sprintf("%s; run quai verify first, then push again (or set [hooks.pre_push].on_missing = \"warn\")", detail),
		}
	}
	return requirementDecision{message: detail + "; run quai verify first"}
}

func pushTargets(ctx context.Context, g repo.Git, refs []PushRef, provided bool) []BranchTarget {
	if provided && len(refs) > 0 {
		return BranchTargets(refs)
	}
	// A manual invocation, or a hook that drops stdin, has not told us the ref,
	// so the current branch stands for it.
	head, err := g.HeadSHA(ctx)
	if err != nil {
		return nil
	}
	ref := "HEAD"
	if branch, _ := g.CurrentBranch(ctx); branch != "" {
		ref = branchRefPrefix + branch
	}
	return []BranchTarget{{Ref: ref, Commit: head}}
}

func short(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}
