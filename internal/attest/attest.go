package attest

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/receipt"
	"github.com/coding-pr0jects/quai/internal/repo"
)

const (
	Namespace     = "quai"
	PayloadType   = "application/vnd.in-toto+json"
	StatementType = "https://in-toto.io/Statement/v1"
	PredicateType = "https://coding-pr0jects.github.io/quai/attestation/v1"
	RefPrefix     = "refs/quai/records/"
	BlobFileName  = "attestation.json"
)

type Envelope struct {
	PayloadType string      `json:"payloadType"`
	Payload     string      `json:"payload"`
	Signatures  []Signature `json:"signatures"`
}

type Signature struct {
	KeyID string `json:"keyid,omitempty"`
	Sig   string `json:"sig"`
}

type Statement struct {
	Type          string    `json:"_type"`
	Subject       []Subject `json:"subject"`
	PredicateType string    `json:"predicateType"`
	Predicate     Predicate `json:"predicate"`
}

type Subject struct {
	Name   string            `json:"name"`
	Digest map[string]string `json:"digest"`
}

type Predicate struct {
	Version     int       `json:"version"`
	Step        string    `json:"step"`
	Fingerprint string    `json:"fingerprint"`
	TreeSHA     string    `json:"tree_sha"`
	HeadSHA     string    `json:"head_sha"`
	BaseSHA     string    `json:"base_sha"`
	Passed      bool      `json:"passed"`
	DurationMS  int64     `json:"duration_ms"`
	CreatedAt   time.Time `json:"created_at"`
}

type VerifiedInfo struct {
	Signer    string
	Statement Statement
}

// Diagnostic reason values reported by quai attest check for every step.
const (
	ReasonVerified         = "verified"
	ReasonNoRecord         = "no_record"
	ReasonBaseMismatch     = "base_mismatch"
	ReasonTreeMismatch     = "tree_mismatch"
	ReasonConfigMismatch   = "config_mismatch"
	ReasonSignatureInvalid = "signature_invalid"
	ReasonExpired          = "expired"
	ReasonSampled          = "sampled"
)

// VerificationError carries the normalized reason a stored record failed verification.
type VerificationError struct {
	Reason string
	Err    error
}

func (e *VerificationError) Error() string { return e.Err.Error() }

func (e *VerificationError) Unwrap() error { return e.Err }

type CheckOptions struct {
	StepNames      []string
	Since          string
	Onto           string
	Head           string
	ConfigRef      string
	AllowedSigners string
	Sample         int
	Fetch          bool
	Remote         string
	Now            func() time.Time
	Roll           func() int // returns [0, 100); defaults to crypto/rand
}

type StepCheck struct {
	Name        string     `json:"name"`
	Status      string     `json:"status"` // verified | missing | invalid | sampled
	Reason      string     `json:"reason"`
	Fingerprint string     `json:"fingerprint,omitempty"`
	TreeSHA     string     `json:"tree_sha,omitempty"`
	BaseSHA     string     `json:"base_sha,omitempty"`
	Ref         string     `json:"ref,omitempty"`
	Signer      string     `json:"signer,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	MatchMode   string     `json:"match_mode,omitempty"`
	Error       string     `json:"error,omitempty"`
}

type CheckResult struct {
	OK              bool        `json:"ok"`
	Sampled         bool        `json:"sampled,omitempty"`
	MatchMode       string      `json:"match_mode,omitempty"`
	ConfigRef       string      `json:"config_ref,omitempty"`
	TreeSHA         string      `json:"tree_sha"`
	BaseSHA         string      `json:"base_sha"`
	Onto            string      `json:"onto,omitempty"`
	HeadSHA         string      `json:"head_sha,omitempty"`
	OldBaseSHA      string      `json:"old_base_sha,omitempty"`
	DisjointRefusal string      `json:"disjoint_refusal,omitempty"`
	Steps           []StepCheck `json:"steps"`
}

type PushOptions struct {
	StepNames []string
	Since     string
	Remote    string
}

type PushResult struct {
	Remote string   `json:"remote"`
	Pushed []string `json:"pushed"`
}

type PruneOptions struct {
	MaxAge    time.Duration
	Remote    string
	LocalOnly bool
	DryRun    bool
	Now       func() time.Time
}

type PruneResult struct {
	OK           bool     `json:"ok"`
	DryRun       bool     `json:"dry_run,omitempty"`
	MaxAge       string   `json:"max_age"`
	Remote       string   `json:"remote,omitempty"`
	Kept         int      `json:"kept"`
	PrunedLocal  []string `json:"pruned_local,omitempty"`
	PrunedRemote []string `json:"pruned_remote,omitempty"`
}

// PAE returns the DSSE v1 Pre-Authentication Encoding over payloadType and payload.
func PAE(payloadType string, payload []byte) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "DSSEv1 %d %s %d ", len(payloadType), payloadType, len(payload))
	buf.Write(payload)
	return buf.Bytes()
}

// RefName returns the git ref path storing the signed attestation for (step, fingerprint).
func RefName(step, fingerprint string) string {
	return RefPrefix + step + "-" + fingerprint
}

// NewStatement builds an in-toto Statement v1 from a passing receipt.
func NewStatement(r receipt.Receipt) Statement {
	created := r.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	version := r.Version
	if version == 0 {
		version = receipt.CurrentVersion
	}
	return Statement{
		Type: StatementType,
		Subject: []Subject{
			{
				Name:   "quai:step:" + r.Step,
				Digest: map[string]string{"sha256": r.Fingerprint},
			},
		},
		PredicateType: PredicateType,
		Predicate: Predicate{
			Version:     version,
			Step:        r.Step,
			Fingerprint: r.Fingerprint,
			TreeSHA:     r.TreeSHA,
			HeadSHA:     r.HeadSHA,
			BaseSHA:     r.BaseSHA,
			Passed:      r.Passed,
			DurationMS:  r.DurationMS,
			CreatedAt:   created,
		},
	}
}

// ShouldSample returns true when probabilistic audit sampling triggers a forced rerun.
func ShouldSample(samplePct int, roll func() int) bool {
	if samplePct <= 0 {
		return false
	}
	if samplePct >= 100 {
		return true
	}
	return roll() < samplePct
}

func verifyStatement(stmt Statement, expectedStep, expectedFingerprint string, maxAge time.Duration, now time.Time) (string, error) {
	if stmt.Type != StatementType {
		return ReasonSignatureInvalid, fmt.Errorf("unexpected statement _type %q", stmt.Type)
	}
	if stmt.PredicateType != PredicateType {
		return ReasonSignatureInvalid, fmt.Errorf("unexpected predicateType %q", stmt.PredicateType)
	}
	if !stmt.Predicate.Passed {
		return ReasonSignatureInvalid, errors.New("attestation predicate is not passing")
	}
	if stmt.Predicate.Step != expectedStep {
		return ReasonConfigMismatch, fmt.Errorf("attestation step %q does not match expected %q", stmt.Predicate.Step, expectedStep)
	}
	if stmt.Predicate.Fingerprint != expectedFingerprint {
		return ReasonConfigMismatch, fmt.Errorf("attestation fingerprint %q does not match expected %q", stmt.Predicate.Fingerprint, expectedFingerprint)
	}
	if len(stmt.Subject) == 0 || stmt.Subject[0].Digest["sha256"] != expectedFingerprint {
		return ReasonConfigMismatch, errors.New("attestation subject sha256 does not match expected fingerprint")
	}
	if stmt.Predicate.CreatedAt.IsZero() {
		return ReasonSignatureInvalid, errors.New("attestation missing created_at timestamp")
	}
	if maxAge > 0 && now.Sub(stmt.Predicate.CreatedAt) > maxAge {
		return ReasonExpired, fmt.Errorf("attestation expired (age %s > max_age %s)", now.Sub(stmt.Predicate.CreatedAt).Round(time.Second), maxAge)
	}
	return ReasonVerified, nil
}

// SignPayload wraps raw JSON payload bytes in a DSSE v1 envelope signed with an OpenSSH
// private key under Namespace ("quai").
func SignPayload(ctx context.Context, payload []byte, keyPath, principal string) (Envelope, error) {
	if strings.TrimSpace(keyPath) == "" {
		return Envelope{}, errors.New("no SSH signing key configured (pass --key, set QUAI_SIGNING_KEY, [attest].signing_key, or git user.signingkey)")
	}
	resolvedKey := expandHome(keyPath)
	if _, err := os.Stat(resolvedKey); err != nil {
		return Envelope{}, fmt.Errorf("signing key %s: %w", resolvedKey, err)
	}
	pae := PAE(PayloadType, payload)
	sigArmor, err := sshSign(ctx, resolvedKey, Namespace, pae)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		PayloadType: PayloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
		Signatures: []Signature{
			{
				KeyID: principal,
				Sig:   base64.StdEncoding.EncodeToString(sigArmor),
			},
		},
	}, nil
}

// VerifyPayload cryptographically verifies a DSSE v1 envelope against allowedSignersPath
// under Namespace ("quai") and returns the verified signer principal and decoded payload bytes.
func VerifyPayload(ctx context.Context, env Envelope, allowedSignersPath string, allowedPrincipals []string) (string, []byte, error) {
	if env.PayloadType != PayloadType {
		return "", nil, fmt.Errorf("unexpected DSSE payloadType %q", env.PayloadType)
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return "", nil, fmt.Errorf("decode DSSE payload: %w", err)
	}
	if len(env.Signatures) == 0 {
		return "", nil, errors.New("DSSE envelope has no signatures")
	}
	if strings.TrimSpace(allowedSignersPath) == "" {
		return "", nil, errors.New("[attest].allowed_signers is not configured")
	}
	if _, err := os.Stat(allowedSignersPath); err != nil {
		return "", nil, fmt.Errorf("allowed_signers file %s: %w", allowedSignersPath, err)
	}
	pae := PAE(env.PayloadType, payload)
	var lastErr error
	for _, s := range env.Signatures {
		sigBytes, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil {
			lastErr = fmt.Errorf("decode signature: %w", err)
			continue
		}
		principal, err := sshVerify(ctx, allowedSignersPath, Namespace, sigBytes, pae)
		if err != nil {
			lastErr = err
			continue
		}
		if len(allowedPrincipals) > 0 && !containsString(allowedPrincipals, principal) {
			lastErr = fmt.Errorf("signer %q is not in allowed principals %v", principal, allowedPrincipals)
			continue
		}
		return principal, payload, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no valid signature found")
	}
	return "", nil, lastErr
}

// SignReceipt signs a passing receipt with an OpenSSH private key into a DSSE envelope.
func SignReceipt(ctx context.Context, r receipt.Receipt, keyPath, principal string) (Envelope, error) {
	if !r.Passed {
		return Envelope{}, errors.New("refusing to sign a failing receipt")
	}
	if r.Step == "" || r.Fingerprint == "" {
		return Envelope{}, errors.New("receipt requires step and fingerprint")
	}
	stmt := NewStatement(r)
	payload, err := json.Marshal(stmt)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal statement: %w", err)
	}
	return SignPayload(ctx, payload, keyPath, principal)
}

// VerifyEnvelope cryptographically verifies a DSSE envelope against allowedSignersPath
// using OpenSSH SSHSIG under Namespace ("quai") and enforces principal allowlist and maxAge.
func VerifyEnvelope(
	ctx context.Context,
	env Envelope,
	allowedSignersPath string,
	allowedPrincipals []string,
	expectedStep, expectedFingerprint string,
	maxAge time.Duration,
	now time.Time,
) (VerifiedInfo, error) {
	if env.PayloadType != PayloadType {
		return VerifiedInfo{}, fmt.Errorf("unexpected DSSE payloadType %q", env.PayloadType)
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return VerifiedInfo{}, fmt.Errorf("decode DSSE payload: %w", err)
	}
	var stmt Statement
	if err := json.Unmarshal(payload, &stmt); err != nil {
		return VerifiedInfo{}, fmt.Errorf("decode in-toto statement: %w", err)
	}
	if reason, err := verifyStatement(stmt, expectedStep, expectedFingerprint, maxAge, now); err != nil {
		return VerifiedInfo{}, &VerificationError{Reason: reason, Err: err}
	}
	if len(env.Signatures) == 0 {
		return VerifiedInfo{}, errors.New("DSSE envelope has no signatures")
	}
	if strings.TrimSpace(allowedSignersPath) == "" {
		return VerifiedInfo{}, errors.New("[attest].allowed_signers is not configured")
	}
	if _, err := os.Stat(allowedSignersPath); err != nil {
		return VerifiedInfo{}, fmt.Errorf("allowed_signers file %s: %w", allowedSignersPath, err)
	}

	pae := PAE(env.PayloadType, payload)
	var lastErr error
	for _, s := range env.Signatures {
		sigBytes, err := base64.StdEncoding.DecodeString(s.Sig)
		if err != nil {
			lastErr = fmt.Errorf("decode signature: %w", err)
			continue
		}
		principal, err := sshVerify(ctx, allowedSignersPath, Namespace, sigBytes, pae)
		if err != nil {
			lastErr = err
			continue
		}
		if len(allowedPrincipals) > 0 && !containsString(allowedPrincipals, principal) {
			lastErr = fmt.Errorf("signer %q is not in allowed principals %v", principal, allowedPrincipals)
			continue
		}
		return VerifiedInfo{
			Signer:    principal,
			Statement: stmt,
		}, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no valid signature found")
	}
	return VerifiedInfo{}, &VerificationError{Reason: ReasonSignatureInvalid, Err: lastErr}
}

// WriteRecord signs receipt r and stores the DSSE envelope under refs/quai/records/<step>-<fingerprint>.
func WriteRecord(ctx context.Context, g repo.Git, r receipt.Receipt, keyPath, principal string) (string, Envelope, error) {
	env, err := SignReceipt(ctx, r, keyPath, principal)
	if err != nil {
		return "", Envelope{}, err
	}
	raw, err := json.MarshalIndent(env, "", "\t")
	if err != nil {
		return "", Envelope{}, err
	}
	ref := RefName(r.Step, r.Fingerprint)
	if err := g.WriteRefBlob(ctx, ref, BlobFileName, append(raw, '\n')); err != nil {
		return "", Envelope{}, err
	}
	return ref, env, nil
}

// ReadRecord reads a stored DSSE envelope from refs/quai/records/<step>-<fingerprint>.
func ReadRecord(ctx context.Context, g repo.Git, step, fingerprint string) (Envelope, bool, error) {
	ref := RefName(step, fingerprint)
	raw, ok, err := g.ReadRefBlob(ctx, ref, BlobFileName)
	if err != nil || !ok {
		return Envelope{}, ok, err
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Envelope{}, true, fmt.Errorf("parse attestation at %s: %w", ref, err)
	}
	return env, true, nil
}

// ResolveSigningKey returns the SSH private key path by precedence:
// explicit flag > QUAI_SIGNING_KEY > [attest].signing_key > git config user.signingkey.
func ResolveSigningKey(ctx context.Context, g repo.Git, cfg *config.Config, flagKey string) (string, error) {
	for _, candidate := range []string{
		strings.TrimSpace(flagKey),
		strings.TrimSpace(os.Getenv("QUAI_SIGNING_KEY")),
		strings.TrimSpace(cfg.Attest.SigningKey),
	} {
		if candidate != "" {
			return expandHome(candidate), nil
		}
	}
	if gitKey, err := g.ConfigGet(ctx, "user.signingkey"); err == nil && strings.TrimSpace(gitKey) != "" {
		return expandHome(strings.TrimSpace(gitKey)), nil
	}
	return "", errors.New("no SSH signing key configured (pass --key, set QUAI_SIGNING_KEY, [attest].signing_key, or git user.signingkey)")
}

// ResolveAllowedSigners resolves the OpenSSH allowed_signers path by precedence:
// explicit flag > QUAI_ALLOWED_SIGNERS > [attest].allowed_signers (resolved relative to topLevel).
func ResolveAllowedSigners(topLevel string, cfg *config.Config, flagPath string) (string, error) {
	raw := ""
	for _, candidate := range []string{
		strings.TrimSpace(flagPath),
		strings.TrimSpace(os.Getenv("QUAI_ALLOWED_SIGNERS")),
		strings.TrimSpace(cfg.Attest.AllowedSigners),
	} {
		if candidate != "" {
			raw = candidate
			break
		}
	}
	if raw == "" {
		return "", errors.New("no allowed_signers configured (pass --allowed-signers, set QUAI_ALLOWED_SIGNERS, or set [attest].allowed_signers in .quai.toml)")
	}
	p := expandHome(raw)
	if !filepath.IsAbs(p) {
		p = filepath.Join(topLevel, p)
	}
	return p, nil
}

// Fetch downloads refs/quai/records/* from remote.
func Fetch(ctx context.Context, g repo.Git, remote string) error {
	if remote == "" {
		remote = "origin"
	}
	return g.FetchRefs(ctx, remote, RefPrefix+"*:"+RefPrefix+"*")
}

// FetchTargeted checks which of the requested refs exist on remote via one ls-remote call
// and fetches only those exact refs in one git fetch call.
func FetchTargeted(ctx context.Context, g repo.Git, remote string, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	if remote == "" {
		remote = "origin"
	}
	existing, err := g.ListRemoteRefs(ctx, remote, refs)
	if err != nil {
		return err
	}
	if len(existing) == 0 {
		return nil
	}
	refspecs := make([]string, 0, len(existing))
	for _, r := range existing {
		refspecs = append(refspecs, r+":"+r)
	}
	return g.FetchRefspecs(ctx, remote, refspecs)
}

// Prune deletes expired or unreadable attestation refs under refs/quai/records/* locally
// and, unless LocalOnly is set, on remote as well. A dry run never fetches, so it reports
// the expired refs already known locally without mutating any ref.
func Prune(ctx context.Context, g repo.Git, cfg *config.Config, opts PruneOptions) (PruneResult, error) {
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = cfg.Attest.MaxAgeDur
	}
	if maxAge <= 0 {
		maxAge = 14 * 24 * time.Hour
	}
	if maxAge < config.MinAttestMaxAge {
		return PruneResult{}, fmt.Errorf("attest max_age %s is below the minimum %s", maxAge, config.MinAttestMaxAge)
	}
	remote := opts.Remote
	if remote == "" {
		remote = cfg.Attest.Remote
	}
	if remote == "" {
		remote = "origin"
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()

	res := PruneResult{
		OK:     true,
		DryRun: opts.DryRun,
		MaxAge: maxAge.String(),
	}
	if !opts.LocalOnly {
		res.Remote = remote
	}

	remoteSet := make(map[string]struct{})
	var remoteErr error
	if !opts.LocalOnly {
		if !opts.DryRun {
			remoteErr = Fetch(ctx, g, remote)
		}
		if remoteErr == nil {
			remoteRefs, err := g.ListRemoteRefs(ctx, remote, []string{RefPrefix + "*"})
			if err != nil {
				remoteErr = err
			} else {
				for _, r := range remoteRefs {
					remoteSet[r] = struct{}{}
				}
			}
		}
	}

	localRefs, err := g.ListRefs(ctx, RefPrefix)
	if err != nil {
		return res, err
	}

	var expiredLocal []string
	var expiredRemote []string
	for _, ref := range localRefs {
		if !isRefExpired(ctx, g, ref, maxAge, now) {
			res.Kept++
			continue
		}
		expiredLocal = append(expiredLocal, ref)
		if _, onRemote := remoteSet[ref]; onRemote {
			expiredRemote = append(expiredRemote, ref)
		}
	}

	if opts.DryRun {
		res.PrunedLocal = expiredLocal
		res.PrunedRemote = expiredRemote
		return res, remoteErr
	}

	for _, ref := range expiredLocal {
		if err := g.DeleteLocalRef(ctx, ref); err != nil {
			return res, err
		}
		res.PrunedLocal = append(res.PrunedLocal, ref)
	}
	if !opts.LocalOnly && len(expiredRemote) > 0 && remoteErr == nil {
		if err := g.DeleteRemoteRefs(ctx, remote, expiredRemote); err != nil {
			return res, err
		}
		res.PrunedRemote = expiredRemote
	}
	return res, remoteErr
}

func isRefExpired(ctx context.Context, g repo.Git, ref string, maxAge time.Duration, now time.Time) bool {
	raw, ok, err := g.ReadRefBlob(ctx, ref, BlobFileName)
	if err != nil || !ok {
		return true
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return true
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return true
	}
	var stmt Statement
	if err := json.Unmarshal(payload, &stmt); err != nil {
		return true
	}
	if stmt.Predicate.CreatedAt.IsZero() {
		return true
	}
	return now.Sub(stmt.Predicate.CreatedAt) > maxAge
}

// Push pushes local signed attestation refs for the current tree's selected steps to remote.
func Push(ctx context.Context, g repo.Git, cfg *config.Config, opts PushOptions) (PushResult, error) {
	remote := opts.Remote
	if remote == "" {
		remote = cfg.Attest.Remote
	}
	treeSHA, baseSHA, err := resolveCurrentTreeAndBase(ctx, g, cfg, opts.Since)
	if err != nil {
		return PushResult{}, err
	}
	steps, err := selectSteps(cfg, opts.StepNames)
	if err != nil {
		return PushResult{}, err
	}
	var refs []string
	for _, step := range steps {
		fp, err := receipt.Fingerprint(ctx, g, step, treeSHA, baseSHA)
		if err != nil {
			return PushResult{}, err
		}
		ref := RefName(step.Name, fp)
		if _, ok, err := g.ReadRefBlob(ctx, ref, BlobFileName); err != nil {
			return PushResult{}, err
		} else if ok {
			refs = append(refs, ref)
		}
	}
	if err := g.PushRefs(ctx, remote, refs); err != nil {
		return PushResult{}, err
	}
	return PushResult{Remote: remote, Pushed: refs}, nil
}

// RecordCandidate identifies a stored attestation for a step under another fingerprint.
type RecordCandidate struct {
	Fingerprint string
	TreeSHA     string
	BaseSHA     string
}

// ClassifyMiss names the most specific reason no stored record matched treeSHA and baseSHA.
func ClassifyMiss(treeSHA, baseSHA string, candidates []RecordCandidate) (reason, detail string) {
	for _, c := range candidates {
		if c.TreeSHA == treeSHA && c.BaseSHA == baseSHA {
			return ReasonConfigMismatch, fmt.Sprintf(
				"stored attestation %s matches tree and base but not the step fingerprint: step run, inputs, or lockfiles changed",
				shortSHA(c.Fingerprint),
			)
		}
	}
	for _, c := range candidates {
		if c.TreeSHA == treeSHA {
			return ReasonBaseMismatch, fmt.Sprintf(
				"stored attestation %s matches the current tree but was signed against base %s, current base is %s: the base moved or origin/<base> is stale",
				shortSHA(c.Fingerprint), shortSHA(c.BaseSHA), shortSHA(baseSHA),
			)
		}
	}
	if len(candidates) > 0 {
		c := candidates[0]
		return ReasonTreeMismatch, fmt.Sprintf(
			"stored attestation %s was signed for tree %s, current tree is %s: the tree changed since signing",
			shortSHA(c.Fingerprint), shortSHA(c.TreeSHA), shortSHA(treeSHA),
		)
	}
	return ReasonNoRecord, "no signed attestation found for fingerprint"
}

func diagnoseMiss(ctx context.Context, g repo.Git, stepName, treeSHA, baseSHA string) (string, string) {
	refs, err := g.ListRefs(ctx, RefPrefix+stepName+"-*")
	if err != nil {
		return ReasonNoRecord, "no signed attestation found for fingerprint"
	}
	candidates := make([]RecordCandidate, 0, len(refs))
	for _, ref := range refs {
		raw, ok, err := g.ReadRefBlob(ctx, ref, BlobFileName)
		if err != nil || !ok {
			continue
		}
		var env Envelope
		if json.Unmarshal(raw, &env) != nil {
			continue
		}
		payload, err := base64.StdEncoding.DecodeString(env.Payload)
		if err != nil {
			continue
		}
		var stmt Statement
		if json.Unmarshal(payload, &stmt) != nil || stmt.Predicate.Step != stepName {
			continue
		}
		candidates = append(candidates, RecordCandidate{
			Fingerprint: stmt.Predicate.Fingerprint,
			TreeSHA:     stmt.Predicate.TreeSHA,
			BaseSHA:     stmt.Predicate.BaseSHA,
		})
	}
	return ClassifyMiss(treeSHA, baseSHA, candidates)
}

func shortSHA(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

// Check verifies signed attestations for the selected steps on HEAD (or virtual merge-tree --onto <ref>).
// When opts.ConfigRef is set, .quai.toml is loaded from that git ref instead of the working tree.
func Check(ctx context.Context, g repo.Git, cfg *config.Config, opts CheckOptions) (CheckResult, error) {
	if opts.Sample < 0 || opts.Sample > 100 {
		return CheckResult{}, fmt.Errorf("--sample must be between 0 and 100, got %d", opts.Sample)
	}
	if opts.Head != "" && opts.ConfigRef == "" {
		return CheckResult{}, errors.New("--head requires --config-ref, so global_paths come from a trusted ref (e.g. --config-ref origin/main)")
	}
	if opts.Head != "" && opts.Onto != "" {
		return CheckResult{}, errors.New("--head and --onto cannot be combined: --head checks a checked-out merge commit, --onto builds one")
	}
	topLevel, err := g.TopLevel(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	if opts.ConfigRef != "" {
		rawCfg, err := g.ShowFile(ctx, opts.ConfigRef, config.FileName)
		if err != nil {
			return CheckResult{}, fmt.Errorf("load %s from --config-ref %q: %w", config.FileName, opts.ConfigRef, err)
		}
		cfg, err = config.Parse(rawCfg)
		if err != nil {
			return CheckResult{}, fmt.Errorf("parse %s from --config-ref %q: %w", config.FileName, opts.ConfigRef, err)
		}
	}
	if cfg == nil {
		return CheckResult{}, errors.New("config is required")
	}
	allowedSigners, err := ResolveAllowedSigners(topLevel, cfg, opts.AllowedSigners)
	if err != nil {
		return CheckResult{}, err
	}
	remote := opts.Remote
	if remote == "" {
		remote = cfg.Attest.Remote
	}

	steps, err := selectSteps(cfg, opts.StepNames)
	if err != nil {
		return CheckResult{}, err
	}

	var treeSHA, baseSHA string
	if opts.Onto != "" {
		ontoSHA, err := g.RevParse(ctx, opts.Onto)
		if err != nil {
			return CheckResult{}, fmt.Errorf("resolve --onto %q: %w", opts.Onto, err)
		}
		mt, err := g.MergeTree(ctx, ontoSHA, "HEAD")
		if err != nil {
			return CheckResult{}, err
		}
		if !mt.Clean {
			return CheckResult{}, &repo.RebaseConflictError{ConflictFiles: mt.ConflictFiles}
		}
		treeSHA = mt.TreeSHA
		baseSHA = ontoSHA
	} else {
		treeSHA, baseSHA, err = resolveCurrentTreeAndBase(ctx, g, cfg, opts.Since)
		if err != nil {
			return CheckResult{}, err
		}
	}
	var headSHA string
	if opts.Head != "" {
		if headSHA, err = g.RevParse(ctx, opts.Head); err != nil {
			return CheckResult{}, fmt.Errorf("resolve --head %q: %w", opts.Head, err)
		}
	}

	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()

	rollFn := opts.Roll
	if rollFn == nil {
		rollFn = cryptoRoll100
	}

	res := CheckResult{
		OK:        true,
		ConfigRef: opts.ConfigRef,
		TreeSHA:   treeSHA,
		BaseSHA:   baseSHA,
		Onto:      opts.Onto,
		HeadSHA:   headSHA,
	}
	addStep := func(sc StepCheck) {
		sc.TreeSHA = treeSHA
		sc.BaseSHA = baseSHA
		res.Steps = append(res.Steps, sc)
	}

	stepFPs := make([]string, len(steps))
	targetRefs := make([]string, len(steps))
	for i, step := range steps {
		fp, err := receipt.Fingerprint(ctx, g, step, treeSHA, baseSHA)
		if err != nil {
			return res, err
		}
		stepFPs[i] = fp
		targetRefs[i] = RefName(step.Name, fp)
	}

	if ShouldSample(opts.Sample, rollFn) {
		res.OK = false
		res.Sampled = true
		for i, step := range steps {
			addStep(StepCheck{
				Name:        step.Name,
				Status:      "sampled",
				Reason:      ReasonSampled,
				Fingerprint: stepFPs[i],
				Ref:         targetRefs[i],
				Error:       fmt.Sprintf("selected for probabilistic audit rerun (--sample %d%%)", opts.Sample),
			})
		}
		return res, nil
	}

	if opts.Fetch {
		if err := FetchTargeted(ctx, g, remote, targetRefs); err != nil {
			return CheckResult{}, err
		}
	}

	c := stepChecker{g: g, cfg: cfg, allowedSigners: allowedSigners, now: now}
	for i, step := range steps {
		res.Steps = append(res.Steps, c.check(ctx, step, stepFPs[i], MatchExact, treeSHA, baseSHA))
	}
	if headSHA != "" && anyStatus(res.Steps, "missing") {
		if err := c.retryDisjointBase(ctx, steps, headSHA, opts.Fetch, remote, &res); err != nil {
			return res, err
		}
	}
	res.OK = !anyStatus(res.Steps, "missing", "invalid")
	if res.OK {
		res.MatchMode = MatchExact
		if anyMatchMode(res.Steps, MatchDisjointBase) {
			res.MatchMode = MatchDisjointBase
		}
	}
	return res, nil
}

type stepChecker struct {
	g              repo.Git
	cfg            *config.Config
	allowedSigners string
	now            time.Time
}

func (c stepChecker) retryDisjointBase(ctx context.Context, steps []config.Step, headSHA string, fetch bool, remote string, res *CheckResult) error {
	in, err := gatherDisjointBase(ctx, c.g, c.cfg, headSHA, res.BaseSHA)
	res.OldBaseSHA = in.OldBaseSHA
	if err != nil {
		res.DisjointRefusal = err.Error()
		return nil
	}
	if refusal := RefuseDisjointBase(in); refusal != "" {
		res.DisjointRefusal = refusal
		return nil
	}
	headTree, err := c.g.TreeSHA(ctx, headSHA)
	if err != nil {
		return err
	}
	fps := make([]string, len(steps))
	var refs []string
	for i, step := range steps {
		if res.Steps[i].Status != "missing" {
			continue
		}
		if fps[i], err = receipt.Fingerprint(ctx, c.g, step, headTree, in.OldBaseSHA); err != nil {
			return err
		}
		refs = append(refs, RefName(step.Name, fps[i]))
	}
	if fetch {
		if err := FetchTargeted(ctx, c.g, remote, refs); err != nil {
			return err
		}
	}
	for i, fp := range fps {
		if fp == "" {
			continue
		}
		if sc := c.check(ctx, steps[i], fp, MatchDisjointBase, headTree, in.OldBaseSHA); sc.Status != "missing" {
			res.Steps[i] = sc
		}
	}
	return nil
}

func (c stepChecker) check(ctx context.Context, step config.Step, fp, mode, treeSHA, baseSHA string) StepCheck {
	sc := StepCheck{
		Name:        step.Name,
		Fingerprint: fp,
		Ref:         RefName(step.Name, fp),
		TreeSHA:     treeSHA,
		BaseSHA:     baseSHA,
	}
	env, ok, err := ReadRecord(ctx, c.g, step.Name, fp)
	if err != nil {
		sc.Status, sc.Reason, sc.Error = "invalid", ReasonSignatureInvalid, err.Error()
		return sc
	}
	if !ok {
		sc.Status = "missing"
		sc.Reason, sc.Error = diagnoseMiss(ctx, c.g, step.Name, treeSHA, baseSHA)
		return sc
	}
	info, err := VerifyEnvelope(ctx, env, c.allowedSigners, c.cfg.AllowedPrincipalsFor(step), step.Name, fp, c.cfg.Attest.MaxAgeDur, c.now)
	if err != nil {
		sc.Status, sc.Reason, sc.Error = "invalid", ReasonSignatureInvalid, err.Error()
		var verr *VerificationError
		if errors.As(err, &verr) && verr.Reason != "" {
			sc.Reason = verr.Reason
		}
		return sc
	}
	ts := info.Statement.Predicate.CreatedAt
	sc.Status, sc.Signer, sc.CreatedAt, sc.MatchMode, sc.Reason = "verified", info.Signer, &ts, mode, ReasonVerified
	return sc
}

func anyStatus(steps []StepCheck, statuses ...string) bool {
	for _, s := range steps {
		if containsString(statuses, s.Status) {
			return true
		}
	}
	return false
}

func anyMatchMode(steps []StepCheck, mode string) bool {
	for _, s := range steps {
		if s.MatchMode == mode {
			return true
		}
	}
	return false
}

func sshSign(ctx context.Context, keyPath, namespace string, data []byte) ([]byte, error) {
	tmpDir, err := os.MkdirTemp("", "quai-sign-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	payloadPath := filepath.Join(tmpDir, "payload")
	if err := os.WriteFile(payloadPath, data, 0o600); err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, "ssh-keygen", "-Y", "sign", "-f", keyPath, "-n", namespace, payloadPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ssh-keygen -Y sign: %w (%s)", err, strings.TrimSpace(stderr.String()))
	}
	sig, err := os.ReadFile(payloadPath + ".sig")
	if err != nil {
		return nil, fmt.Errorf("read generated signature: %w", err)
	}
	return sig, nil
}

func sshVerify(ctx context.Context, allowedSignersPath, namespace string, sigArmor, data []byte) (string, error) {
	tmpDir, err := os.MkdirTemp("", "quai-verify-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)

	sigPath := filepath.Join(tmpDir, "payload.sig")
	if err := os.WriteFile(sigPath, sigArmor, 0o600); err != nil {
		return "", err
	}

	findCmd := exec.CommandContext(ctx, "ssh-keygen", "-Y", "find-principals", "-f", allowedSignersPath, "-s", sigPath)
	var findOut, findErr bytes.Buffer
	findCmd.Stdout = &findOut
	findCmd.Stderr = &findErr
	if err := findCmd.Run(); err != nil {
		return "", fmt.Errorf("signer not trusted in %s: %s", allowedSignersPath, strings.TrimSpace(findErr.String()))
	}
	principals := nonEmptyLines(findOut.String())
	if len(principals) == 0 {
		return "", errors.New("no matching principal in allowed_signers")
	}

	var lastErr error
	for _, principal := range principals {
		verifyCmd := exec.CommandContext(
			ctx,
			"ssh-keygen", "-Y", "verify",
			"-f", allowedSignersPath,
			"-I", principal,
			"-n", namespace,
			"-s", sigPath,
		)
		verifyCmd.Stdin = bytes.NewReader(data)
		var vErr bytes.Buffer
		verifyCmd.Stderr = &vErr
		if err := verifyCmd.Run(); err != nil {
			lastErr = fmt.Errorf("ssh-keygen -Y verify (%s): %s", principal, strings.TrimSpace(vErr.String()))
			continue
		}
		return principal, nil
	}
	if lastErr == nil {
		lastErr = errors.New("signature verification failed")
	}
	return "", lastErr
}

func resolveCurrentTreeAndBase(ctx context.Context, g repo.Git, cfg *config.Config, since string) (string, string, error) {
	treeSHA, err := g.TreeSHA(ctx, "HEAD")
	if err != nil {
		return "", "", err
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
		return "", "", fmt.Errorf("resolve merge-base with %s: %w", baseRef, err)
	}
	return treeSHA, baseSHA, nil
}

func selectSteps(cfg *config.Config, names []string) ([]config.Step, error) {
	if len(names) == 0 {
		return cfg.Steps, nil
	}
	out := make([]config.Step, 0, len(names))
	for _, n := range names {
		s, ok := cfg.StepByName(n)
		if !ok {
			return nil, fmt.Errorf("unknown step %q", n)
		}
		out = append(out, s)
	}
	return out, nil
}

func cryptoRoll100() int {
	n, err := rand.Int(rand.Reader, big.NewInt(100))
	if err != nil {
		return 0
	}
	return int(n.Int64())
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}
