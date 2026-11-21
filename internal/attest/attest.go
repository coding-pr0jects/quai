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

type CheckOptions struct {
	StepNames []string
	Since     string
	Onto      string
	Sample    int
	Fetch     bool
	Remote    string
	Now       func() time.Time
	Roll      func() int // returns [0, 100); defaults to crypto/rand
}

type StepCheck struct {
	Name        string     `json:"name"`
	Status      string     `json:"status"` // verified | missing | invalid | sampled
	Fingerprint string     `json:"fingerprint,omitempty"`
	Ref         string     `json:"ref,omitempty"`
	Signer      string     `json:"signer,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	Error       string     `json:"error,omitempty"`
}

type CheckResult struct {
	OK      bool        `json:"ok"`
	Sampled bool        `json:"sampled,omitempty"`
	TreeSHA string      `json:"tree_sha"`
	BaseSHA string      `json:"base_sha"`
	Onto    string      `json:"onto,omitempty"`
	Steps   []StepCheck `json:"steps"`
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

// VerifyStatement checks all non-cryptographic invariants of a decoded in-toto Statement.
func VerifyStatement(stmt Statement, expectedStep, expectedFingerprint string, maxAge time.Duration, now time.Time) error {
	if stmt.Type != StatementType {
		return fmt.Errorf("unexpected statement _type %q", stmt.Type)
	}
	if stmt.PredicateType != PredicateType {
		return fmt.Errorf("unexpected predicateType %q", stmt.PredicateType)
	}
	if !stmt.Predicate.Passed {
		return errors.New("attestation predicate is not passing")
	}
	if stmt.Predicate.Step != expectedStep {
		return fmt.Errorf("attestation step %q does not match expected %q", stmt.Predicate.Step, expectedStep)
	}
	if stmt.Predicate.Fingerprint != expectedFingerprint {
		return fmt.Errorf("attestation fingerprint %q does not match expected %q", stmt.Predicate.Fingerprint, expectedFingerprint)
	}
	if len(stmt.Subject) == 0 || stmt.Subject[0].Digest["sha256"] != expectedFingerprint {
		return errors.New("attestation subject sha256 does not match expected fingerprint")
	}
	if stmt.Predicate.CreatedAt.IsZero() {
		return errors.New("attestation missing created_at timestamp")
	}
	if maxAge > 0 && now.Sub(stmt.Predicate.CreatedAt) > maxAge {
		return fmt.Errorf("attestation expired (age %s > max_age %s)", now.Sub(stmt.Predicate.CreatedAt).Round(time.Second), maxAge)
	}
	return nil
}

// SignReceipt signs a passing receipt with an OpenSSH private key into a DSSE envelope.
func SignReceipt(ctx context.Context, r receipt.Receipt, keyPath, principal string) (Envelope, error) {
	if !r.Passed {
		return Envelope{}, errors.New("refusing to sign a failing receipt")
	}
	if r.Step == "" || r.Fingerprint == "" {
		return Envelope{}, errors.New("receipt requires step and fingerprint")
	}
	if strings.TrimSpace(keyPath) == "" {
		return Envelope{}, errors.New("no SSH signing key configured (pass --key, set QUAI_SIGNING_KEY, [attest].signing_key, or git user.signingkey)")
	}
	resolvedKey := expandHome(keyPath)
	if _, err := os.Stat(resolvedKey); err != nil {
		return Envelope{}, fmt.Errorf("signing key %s: %w", resolvedKey, err)
	}

	stmt := NewStatement(r)
	payload, err := json.Marshal(stmt)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal statement: %w", err)
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
	if err := VerifyStatement(stmt, expectedStep, expectedFingerprint, maxAge, now); err != nil {
		return VerifiedInfo{}, err
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
	return VerifiedInfo{}, lastErr
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

// ResolveAllowedSigners resolves [attest].allowed_signers relative to topLevel.
func ResolveAllowedSigners(topLevel string, cfg *config.Config) (string, error) {
	raw := strings.TrimSpace(cfg.Attest.AllowedSigners)
	if raw == "" {
		return "", errors.New("[attest].allowed_signers must be set in .quai.toml")
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

// Check verifies signed attestations for the selected steps on HEAD (or virtual merge-tree --onto <ref>).
func Check(ctx context.Context, g repo.Git, cfg *config.Config, opts CheckOptions) (CheckResult, error) {
	if opts.Sample < 0 || opts.Sample > 100 {
		return CheckResult{}, fmt.Errorf("--sample must be between 0 and 100, got %d", opts.Sample)
	}
	topLevel, err := g.TopLevel(ctx)
	if err != nil {
		return CheckResult{}, err
	}
	allowedSigners, err := ResolveAllowedSigners(topLevel, cfg)
	if err != nil {
		return CheckResult{}, err
	}
	remote := opts.Remote
	if remote == "" {
		remote = cfg.Attest.Remote
	}
	if opts.Fetch {
		if err := Fetch(ctx, g, remote); err != nil {
			return CheckResult{}, err
		}
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
		OK:      true,
		TreeSHA: treeSHA,
		BaseSHA: baseSHA,
		Onto:    opts.Onto,
	}

	if ShouldSample(opts.Sample, rollFn) {
		res.OK = false
		res.Sampled = true
		for _, step := range steps {
			fp, err := receipt.Fingerprint(ctx, g, step, treeSHA, baseSHA)
			if err != nil {
				return res, err
			}
			res.Steps = append(res.Steps, StepCheck{
				Name:        step.Name,
				Status:      "sampled",
				Fingerprint: fp,
				Ref:         RefName(step.Name, fp),
				Error:       fmt.Sprintf("selected for probabilistic audit rerun (--sample %d%%)", opts.Sample),
			})
		}
		return res, nil
	}

	for _, step := range steps {
		fp, err := receipt.Fingerprint(ctx, g, step, treeSHA, baseSHA)
		if err != nil {
			return res, err
		}
		ref := RefName(step.Name, fp)
		env, ok, err := ReadRecord(ctx, g, step.Name, fp)
		if err != nil {
			res.OK = false
			res.Steps = append(res.Steps, StepCheck{
				Name:        step.Name,
				Status:      "invalid",
				Fingerprint: fp,
				Ref:         ref,
				Error:       err.Error(),
			})
			continue
		}
		if !ok {
			res.OK = false
			res.Steps = append(res.Steps, StepCheck{
				Name:        step.Name,
				Status:      "missing",
				Fingerprint: fp,
				Ref:         ref,
				Error:       "no signed attestation found for fingerprint",
			})
			continue
		}
		info, err := VerifyEnvelope(
			ctx,
			env,
			allowedSigners,
			cfg.AllowedPrincipalsFor(step),
			step.Name,
			fp,
			cfg.Attest.MaxAgeDur,
			now,
		)
		if err != nil {
			res.OK = false
			res.Steps = append(res.Steps, StepCheck{
				Name:        step.Name,
				Status:      "invalid",
				Fingerprint: fp,
				Ref:         ref,
				Error:       err.Error(),
			})
			continue
		}
		ts := info.Statement.Predicate.CreatedAt
		res.Steps = append(res.Steps, StepCheck{
			Name:        step.Name,
			Status:      "verified",
			Fingerprint: fp,
			Ref:         ref,
			Signer:      info.Signer,
			CreatedAt:   &ts,
		})
	}
	return res, nil
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
