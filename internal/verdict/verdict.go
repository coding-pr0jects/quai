package verdict

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/coding-pr0jects/quai/internal/attest"
	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/repo"
	"github.com/coding-pr0jects/quai/internal/tier"
)

const (
	CurrentVersion = 1
	PredicateType  = "https://coding-pr0jects.github.io/quai/verdict/v1"
	RefPrefix      = "refs/quai/verdicts/"
	BlobFileName   = "verdict.json"
	DefaultMaxAge  = 14 * 24 * time.Hour
	DefaultMaxFile = 200
)

type Status string

const (
	StatusApproved Status = "approved"
	StatusRejected Status = "rejected"
)

type Record struct {
	Version     int         `json:"version"`
	PatchID     string      `json:"patch_id"`
	CodePatchID string      `json:"code_patch_id,omitempty"`
	Tier        config.Tier `json:"tier"`
	Status      Status      `json:"status"`
	Reviewer    string      `json:"reviewer,omitempty"`
	Note        string      `json:"note,omitempty"`
	HeadSHA     string      `json:"head_sha"`
	BaseSHA     string      `json:"base_sha"`
	CreatedAt   time.Time   `json:"created_at"`
}

type Statement struct {
	Type          string           `json:"_type"`
	Subject       []attest.Subject `json:"subject"`
	PredicateType string           `json:"predicateType"`
	Predicate     Record           `json:"predicate"`
}

type RecordOptions struct {
	Status     Status
	Tier       config.Tier
	Reviewer   string
	Note       string
	Since      string
	Sign       bool
	SigningKey string
	Push       bool
	Remote     string
	Now        func() time.Time
}

type RecordResult struct {
	OK      bool   `json:"ok"`
	Record  Record `json:"record"`
	Path    string `json:"path"`
	Ref     string `json:"ref,omitempty"`
	CodeRef string `json:"code_ref,omitempty"`
	Signed  bool   `json:"signed,omitempty"`
	Pushed  bool   `json:"pushed,omitempty"`
}

type CheckOptions struct {
	Since          string
	ConfigRef      string
	RequireSigned  bool
	AllowedSigners string
	Fetch          bool
	Remote         string
	Now            func() time.Time
}

type CheckResult struct {
	OK          bool        `json:"ok"`
	Status      string      `json:"status"` // valid | missing | rejected | insufficient-tier | invalid | empty-diff
	PatchID     string      `json:"patch_id,omitempty"`
	CodePatchID string      `json:"code_patch_id,omitempty"`
	BranchTier  config.Tier `json:"branch_tier"`
	Signed      bool        `json:"signed,omitempty"`
	Signer      string      `json:"signer,omitempty"`
	Record      *Record     `json:"record,omitempty"`
	Error       string      `json:"error,omitempty"`
}

// RefName returns the git ref path storing a signed verdict for patchID.
func RefName(patchID string) string {
	return RefPrefix + patchID
}

// TierCovers returns true when recorded tier meets or exceeds required tier.
func TierCovers(recorded, required config.Tier) bool {
	return tierRank(recorded) >= tierRank(required)
}

func tierRank(t config.Tier) int {
	switch t {
	case config.TierFullLive:
		return 3
	case config.TierDiffAudit:
		return 2
	case config.TierGateOnly:
		return 1
	default:
		return 0
	}
}

// ComputePatchID computes the stable git patch-id for HEAD relative to merge-base(since, HEAD).
func ComputePatchID(ctx context.Context, g repo.Git, cfg *config.Config, since string) (patchID, baseSHA, headSHA string, err error) {
	headSHA, err = g.HeadSHA(ctx)
	if err != nil {
		return "", "", "", err
	}
	baseRef := since
	if baseRef == "" {
		baseName := "main"
		if cfg != nil && cfg.Base != "" {
			baseName = cfg.Base
		}
		baseRef, err = g.ResolveBaseRef(ctx, baseName)
		if err != nil {
			return "", "", "", err
		}
	}
	baseSHA, err = g.MergeBase(ctx, baseRef, headSHA)
	if err != nil {
		return "", "", "", fmt.Errorf("merge-base(%s, HEAD): %w", baseRef, err)
	}
	diff, err := g.DiffBinary(ctx, baseSHA, headSHA)
	if err != nil {
		return "", "", "", err
	}
	pid, err := g.PatchID(ctx, diff)
	if err != nil {
		return "", "", "", err
	}
	return pid, baseSHA, headSHA, nil
}

// CodePatchID computes the patch-id of the base..head diff limited to the paths that
// tier.GateOnlyPath does not call gate-only. It is "" when every changed path is gate-only,
// and when the diff changes a policy file (.quai.toml or the configured allowed_signers),
// because cfg then may not be the policy the diff is checked against.
func CodePatchID(ctx context.Context, g repo.Git, cfg *config.Config, baseSHA, headSHA string) (string, error) {
	files, err := g.DiffNameOnly(ctx, baseSHA, headSHA)
	if err != nil {
		return "", err
	}
	var code []string
	for _, f := range files {
		if isPolicyFile(cfg, f) {
			return "", nil
		}
		if !tier.GateOnlyPath(cfg.Tiers, f) {
			code = append(code, f)
		}
	}
	diff, err := g.DiffBinaryPaths(ctx, baseSHA, headSHA, code)
	if err != nil {
		return "", err
	}
	return g.PatchID(ctx, diff)
}

func isPolicyFile(cfg *config.Config, path string) bool {
	if path == config.FileName {
		return true
	}
	signers := cfg.Attest.AllowedSigners
	return signers != "" && !filepath.IsAbs(signers) && filepath.ToSlash(filepath.Clean(signers)) == path
}

func lookupKeys(patchID, codePatchID string) []string {
	if codePatchID == "" || codePatchID == patchID {
		return []string{patchID}
	}
	return []string{patchID, codePatchID}
}

// RecordVerdict records an approved or rejected review verdict keyed by the branch's stable patch-id.
func RecordVerdict(ctx context.Context, g repo.Git, cfg *config.Config, opts RecordOptions) (RecordResult, error) {
	if opts.Status == "" {
		opts.Status = StatusApproved
	}
	if opts.Status != StatusApproved && opts.Status != StatusRejected {
		return RecordResult{}, fmt.Errorf("invalid verdict status %q (expected approved or rejected)", opts.Status)
	}
	clean, _, err := g.IsClean(ctx)
	if err != nil {
		return RecordResult{}, err
	}
	if !clean {
		return RecordResult{}, errors.New("working tree is dirty; commit changes before recording a review verdict")
	}

	pid, baseSHA, headSHA, err := ComputePatchID(ctx, g, cfg, opts.Since)
	if err != nil {
		return RecordResult{}, err
	}
	if pid == "" {
		return RecordResult{}, errors.New("branch diff against base is empty; nothing to record a verdict for")
	}

	codePID, err := CodePatchID(ctx, g, cfg, baseSHA, headSHA)
	if err != nil {
		return RecordResult{}, err
	}

	branchTier := opts.Tier
	if branchTier == "" {
		tRes, err := tier.Evaluate(ctx, g, cfg, opts.Since)
		if err != nil {
			return RecordResult{}, err
		}
		branchTier = tRes.Tier
	}
	if !config.ValidTier(branchTier) {
		return RecordResult{}, fmt.Errorf("invalid tier %q", branchTier)
	}

	reviewer := strings.TrimSpace(opts.Reviewer)
	if reviewer == "" {
		reviewer, _ = g.ConfigGet(ctx, "user.email")
	}

	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	rec := Record{
		Version:     CurrentVersion,
		PatchID:     pid,
		CodePatchID: codePID,
		Tier:        branchTier,
		Status:      opts.Status,
		Reviewer:    reviewer,
		Note:        strings.TrimSpace(opts.Note),
		HeadSHA:     headSHA,
		BaseSHA:     baseSHA,
		CreatedAt:   nowFn().UTC(),
	}
	keys := lookupKeys(pid, codePID)

	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		return RecordResult{}, err
	}
	var verdictPath string
	for _, key := range keys {
		p, err := writeLocal(commonDir, key, rec, nowFn().UTC())
		if err != nil {
			return RecordResult{}, err
		}
		if verdictPath == "" {
			verdictPath = p
		}
	}

	res := RecordResult{
		OK:     true,
		Record: rec,
		Path:   verdictPath,
	}

	if opts.Sign || opts.Push {
		keyPath, err := attest.ResolveSigningKey(ctx, g, cfg, opts.SigningKey)
		if err != nil {
			return res, err
		}
		stmt := Statement{
			Type: attest.StatementType,
			Subject: []attest.Subject{
				{
					Name:   "quai:verdict:" + rec.PatchID,
					Digest: map[string]string{"git-patch-id": rec.PatchID},
				},
			},
			PredicateType: PredicateType,
			Predicate:     rec,
		}
		payload, err := json.Marshal(stmt)
		if err != nil {
			return res, err
		}
		env, err := attest.SignPayload(ctx, payload, keyPath, reviewer)
		if err != nil {
			return res, err
		}
		raw, err := json.MarshalIndent(env, "", "\t")
		if err != nil {
			return res, err
		}
		var refs []string
		for _, key := range keys {
			ref := RefName(key)
			if err := g.WriteRefBlob(ctx, ref, BlobFileName, append(raw, '\n')); err != nil {
				return res, err
			}
			refs = append(refs, ref)
		}
		res.Ref = refs[0]
		if len(refs) > 1 {
			res.CodeRef = refs[1]
		}
		res.Signed = true

		if opts.Push {
			remote := opts.Remote
			if remote == "" && cfg != nil {
				remote = cfg.Attest.Remote
			}
			if remote == "" {
				remote = "origin"
			}
			if err := g.PushRefs(ctx, remote, refs); err != nil {
				return res, err
			}
			res.Pushed = true
		}
	}

	return res, nil
}

// Check verifies whether an approved verdict covering the current branch tier exists
// for the current branch's stable patch-id. With opts.ConfigRef, it loads the policy from
// that trusted ref and also accepts a verdict for the branch's code patch-id, when the
// branch changed only gate-only paths since the review. Without it, the branch's own
// .quai.toml could widen gate_only, so only the exact patch-id matches.
func Check(ctx context.Context, g repo.Git, cfg *config.Config, opts CheckOptions) (CheckResult, error) {
	if opts.ConfigRef != "" {
		var err error
		cfg, err = attest.LoadConfigRef(ctx, g, opts.ConfigRef)
		if err != nil {
			return CheckResult{}, err
		}
		opts.RequireSigned = opts.RequireSigned || cfg.Land.RequireSignedVerdict
	}
	tRes, err := tier.Evaluate(ctx, g, cfg, opts.Since)
	if err != nil {
		return CheckResult{}, err
	}
	pid, baseSHA, headSHA, err := ComputePatchID(ctx, g, cfg, opts.Since)
	if err != nil {
		return CheckResult{}, err
	}
	if pid == "" {
		return CheckResult{
			OK:         true,
			Status:     "empty-diff",
			BranchTier: tRes.Tier,
		}, nil
	}
	var codePID string
	if opts.ConfigRef != "" {
		codePID, err = CodePatchID(ctx, g, cfg, baseSHA, headSHA)
		if err != nil {
			return CheckResult{}, err
		}
	}
	keys := lookupKeys(pid, codePID)
	failed := func(status, msg string) CheckResult {
		return CheckResult{OK: false, Status: status, PatchID: pid, CodePatchID: codePID, BranchTier: tRes.Tier, Error: msg}
	}

	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()

	maxAge := DefaultMaxAge
	if cfg != nil && cfg.Attest.MaxAgeDur > 0 {
		maxAge = cfg.Attest.MaxAgeDur
	}
	evaluate := func(rec Record) CheckResult {
		return evaluateRecord(rec, pid, codePID, tRes.Tier, maxAge, now)
	}

	if opts.Fetch {
		remote := opts.Remote
		if remote == "" && cfg != nil {
			remote = cfg.Attest.Remote
		}
		refs := make([]string, len(keys))
		for i, key := range keys {
			refs[i] = RefName(key)
		}
		if err := attest.FetchTargeted(ctx, g, remote, refs); err != nil {
			return CheckResult{}, err
		}
	}

	for _, key := range keys {
		rec, signer, problem, found := readSigned(ctx, g, cfg, opts.AllowedSigners, RefName(key))
		if problem != "" && opts.RequireSigned {
			return failed("invalid", problem), nil
		}
		if found && coversPatch(rec, pid, codePID) {
			res := evaluate(rec)
			res.Signed = true
			res.Signer = signer
			return res, nil
		}
	}

	if opts.RequireSigned {
		return failed("missing", fmt.Sprintf("no valid signed verdict found for patch-id %s", shortID(pid))), nil
	}

	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	for _, key := range keys {
		rec, ok, err := readLocal(commonDir, key)
		if err != nil {
			return failed("invalid", err.Error()), nil
		}
		if ok && coversPatch(rec, pid, codePID) {
			return evaluate(rec), nil
		}
	}
	return failed("missing", fmt.Sprintf("no review verdict found for patch-id %s", shortID(pid))), nil
}

func readSigned(ctx context.Context, g repo.Git, cfg *config.Config, allowedSigners, ref string) (rec Record, signer, problem string, found bool) {
	rawEnv, ok, err := g.ReadRefBlob(ctx, ref, BlobFileName)
	if err != nil || !ok {
		return Record{}, "", "", false
	}
	topLevel, _ := g.TopLevel(ctx)
	allowedPath, err := attest.ResolveAllowedSigners(topLevel, cfg, allowedSigners)
	if err != nil {
		return Record{}, "", err.Error(), false
	}
	var env attest.Envelope
	if err := json.Unmarshal(rawEnv, &env); err != nil {
		return Record{}, "", fmt.Sprintf("parse signed verdict: %v", err), false
	}
	var principals []string
	if cfg != nil {
		principals = cfg.Attest.Signers
	}
	signer, payload, err := attest.VerifyPayload(ctx, env, allowedPath, principals)
	if err != nil {
		return Record{}, "", err.Error(), false
	}
	var stmt Statement
	if err := json.Unmarshal(payload, &stmt); err != nil || stmt.PredicateType != PredicateType {
		return Record{}, "", "invalid verdict statement payload", false
	}
	return stmt.Predicate, signer, "", true
}

func coversPatch(rec Record, patchID, codePatchID string) bool {
	if rec.PatchID == patchID {
		return true
	}
	return codePatchID != "" && (rec.CodePatchID == codePatchID || rec.PatchID == codePatchID)
}

func evaluateRecord(rec Record, patchID, codePatchID string, branchTier config.Tier, maxAge time.Duration, now time.Time) CheckResult {
	rCopy := rec
	result := func(ok bool, status, msg string) CheckResult {
		return CheckResult{OK: ok, Status: status, PatchID: patchID, CodePatchID: codePatchID, BranchTier: branchTier, Record: &rCopy, Error: msg}
	}
	if rec.Version != CurrentVersion {
		return result(false, "invalid", fmt.Sprintf("unsupported verdict version %d", rec.Version))
	}
	if maxAge > 0 && !rec.CreatedAt.IsZero() && now.Sub(rec.CreatedAt) > maxAge {
		return result(false, "invalid", "verdict expired")
	}
	if rec.Status == StatusRejected {
		return result(false, "rejected", "patch was rejected by reviewer")
	}
	if rec.Status != StatusApproved {
		return result(false, "invalid", fmt.Sprintf("unknown verdict status %q", rec.Status))
	}
	if !TierCovers(rec.Tier, branchTier) {
		return result(false, "insufficient-tier", fmt.Sprintf("verdict covers %q, but branch requires %q", rec.Tier, branchTier))
	}
	return result(true, "valid", "")
}

func verdictsDir(commonDir string) string {
	return filepath.Join(commonDir, "quai", "verdicts")
}

func writeLocal(commonDir, key string, rec Record, now time.Time) (string, error) {
	dir := verdictsDir(commonDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create verdicts dir: %w", err)
	}
	data, err := json.MarshalIndent(rec, "", "\t")
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".verdict-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	target := filepath.Join(dir, key+".json")
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		return "", err
	}
	pruneLocal(dir, now)
	return target, nil
}

func readLocal(commonDir, patchID string) (Record, bool, error) {
	p := filepath.Join(verdictsDir(commonDir), patchID+".json")
	raw, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Record{}, false, nil
		}
		return Record{}, false, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, true, err
	}
	return rec, true, nil
}

func pruneLocal(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	cutoff := now.Add(-DefaultMaxAge)
	type item struct {
		path string
		ts   time.Time
	}
	var kept []item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		full := filepath.Join(dir, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		ts := info.ModTime()
		if raw, err := os.ReadFile(full); err == nil {
			var r Record
			if json.Unmarshal(raw, &r) == nil && !r.CreatedAt.IsZero() {
				ts = r.CreatedAt
			}
		}
		if ts.Before(cutoff) {
			_ = os.Remove(full)
			continue
		}
		kept = append(kept, item{path: full, ts: ts})
	}
	if len(kept) <= DefaultMaxFile {
		return
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].ts.After(kept[j].ts) })
	for _, old := range kept[DefaultMaxFile:] {
		_ = os.Remove(old.path)
	}
}

func shortID(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
