package verify

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coding-pr0jects/quai/internal/attest"
	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/execrun"
	"github.com/coding-pr0jects/quai/internal/lock"
	"github.com/coding-pr0jects/quai/internal/receipt"
	"github.com/coding-pr0jects/quai/internal/repo"
)

type Options struct {
	StepNames      []string
	Since          string
	NoCache        bool
	Sign           bool
	BestEffortSign bool
	SigningKey     string
	Push           bool
	Remote         string
	JSON           bool
	Stdout         io.Writer
	Stderr         io.Writer
	OnStepStart    func(execrun.Process)
}

// ResolveAutoAttest resolves whether verification should sign and push attestations,
// combining explicit CLI flags, QUAI_SIGN/QUAI_PUSH env vars, and [attest] config defaults.
// bestEffort is true only when signing came implicitly from .quai.toml rather than an explicit flag or env var.
func ResolveAutoAttest(cfg *config.Config, flagSign, flagNoSign, flagPush, flagNoPush bool) (sign, push, bestEffort bool) {
	if flagNoSign {
		return false, false, false
	}
	envSign := strings.TrimSpace(os.Getenv("QUAI_SIGN"))
	envPush := strings.TrimSpace(os.Getenv("QUAI_PUSH"))
	wantAutoPush := !flagNoPush && envPush != "0" && (envPush == "1" || cfg.Attest.AutoPush)

	if flagPush {
		return true, true, false
	}
	if flagSign {
		return true, wantAutoPush, false
	}
	if envPush == "1" {
		return true, !flagNoPush, false
	}
	if envSign == "1" {
		return true, wantAutoPush, false
	}
	if envSign == "0" {
		return false, false, false
	}
	if cfg.Attest.AutoPush && !flagNoPush && envPush != "0" {
		return true, true, true
	}
	if cfg.Attest.AutoSign {
		return true, false, true
	}
	return false, false, false
}

type StepResult struct {
	Name           string   `json:"name"`
	Status         string   `json:"status"` // cached | passed | failed | mutated
	Fingerprint    string   `json:"fingerprint,omitempty"`
	DurationMS     int64    `json:"duration_ms"`
	AttestationRef string   `json:"attestation_ref,omitempty"`
	MutatedPaths   []string `json:"mutated_paths,omitempty"`
	Error          string   `json:"error,omitempty"`
}

type Result struct {
	OK             bool         `json:"ok"`
	Clean          bool         `json:"clean"`
	HeadSHA        string       `json:"head_sha"`
	TreeSHA        string       `json:"tree_sha"`
	BaseSHA        string       `json:"base_sha"`
	ReceiptWritten bool         `json:"receipt_written"`
	Attestations   []string     `json:"attestations,omitempty"`
	Pushed         bool         `json:"pushed,omitempty"`
	Steps          []StepResult `json:"steps"`
}

// Run executes the selected verification steps, enforces slot limits and the working-tree
// mutation guard, deduplicates concurrent runs on identical fingerprints, and writes
// input-fingerprinted receipts on clean passing runs.
func Run(ctx context.Context, g repo.Git, cfg *config.Config, opts Options) (Result, error) {
	topLevel, err := g.TopLevel(ctx)
	if err != nil {
		return Result{}, err
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		return Result{}, err
	}
	headSHA, err := g.HeadSHA(ctx)
	if err != nil {
		return Result{}, err
	}
	treeSHA, err := g.TreeSHA(ctx, "HEAD")
	if err != nil {
		return Result{}, err
	}

	baseRef := opts.Since
	if baseRef == "" {
		baseRef, err = g.ResolveBaseRef(ctx, cfg.Base)
		if err != nil {
			baseRef = "HEAD"
		}
	}
	baseSHA, err := g.MergeBase(ctx, baseRef, "HEAD")
	if err != nil {
		return Result{}, fmt.Errorf("resolve merge-base with %s: %w", baseRef, err)
	}

	selected, err := selectSteps(cfg, opts.StepNames)
	if err != nil {
		return Result{}, err
	}

	clean, _, err := g.IsClean(ctx)
	if err != nil {
		return Result{}, err
	}

	var signingKey, principal string
	if opts.Sign && clean {
		signingKey, err = attest.ResolveSigningKey(ctx, g, cfg, opts.SigningKey)
		if err != nil {
			if opts.BestEffortSign {
				if !opts.JSON && opts.Stderr != nil {
					fmt.Fprintf(opts.Stderr, "quai: auto_sign skipped (%v)\n", err)
				}
				opts.Sign = false
				opts.Push = false
			} else {
				return Result{}, err
			}
		} else {
			principal, _ = g.ConfigGet(ctx, "user.email")
		}
	}

	store := receipt.NewStore(commonDir)
	locksRoot := filepath.Join(commonDir, "quai", "locks")

	res := Result{
		OK:      true,
		Clean:   clean,
		HeadSHA: headSHA,
		TreeSHA: treeSHA,
		BaseSHA: baseSHA,
	}

	for _, step := range selected {
		stepRes, receiptWritten, attRef, stepErr := runOneStep(
			ctx, g, cfg, store, locksRoot, topLevel,
			step, clean, headSHA, treeSHA, baseSHA,
			signingKey, principal, opts,
		)
		if receiptWritten {
			res.ReceiptWritten = true
		}
		if attRef != "" {
			res.Attestations = append(res.Attestations, attRef)
		}
		if stepRes.Name != "" {
			res.Steps = append(res.Steps, stepRes)
		}
		if stepErr != nil {
			if stepRes.Status == "failed" {
				res.OK = false
			}
			return res, stepErr
		}
		if stepRes.Status == "failed" || stepRes.Status == "mutated" {
			res.OK = false
			return res, nil
		}
	}

	if opts.Sign && opts.Push && len(res.Attestations) > 0 {
		remote := opts.Remote
		if remote == "" {
			remote = cfg.Attest.Remote
		}
		if err := g.PushRefs(ctx, remote, res.Attestations); err != nil {
			return res, err
		}
		res.Pushed = true
	}
	return res, nil
}

func runOneStep(
	ctx context.Context,
	g repo.Git,
	cfg *config.Config,
	store *receipt.Store,
	locksRoot, topLevel string,
	step config.Step,
	clean bool,
	headSHA, treeSHA, baseSHA string,
	signingKey, principal string,
	opts Options,
) (StepResult, bool, string, error) {
	started := time.Now()
	var fp string
	var inflightLease *lock.Lease

	if clean {
		var err error
		fp, err = receipt.Fingerprint(ctx, g, step, treeSHA, baseSHA)
		if err != nil {
			return StepResult{}, false, "", err
		}
		if !opts.NoCache {
			if sr, attRef, hit, err := checkCache(ctx, g, store, step.Name, fp, started, signingKey, principal, opts); err != nil || hit {
				return sr, false, attRef, err
			}
		}

		// Deduplicate concurrent executions of the exact same (step, fingerprint) across worktrees.
		inflightDir := filepath.Join(locksRoot, fmt.Sprintf("inflight-%s-%s.lock", step.Name, fp))
		inflightLease, err = lock.AcquireDir(ctx, inflightDir, lock.Owner{
			Command:  "quai verify --step " + step.Name,
			Worktree: topLevel,
		}, lock.Options{
			Wait:         cfg.Land.LockWaitDur,
			PollInterval: 200 * time.Millisecond,
			StatusWriter: opts.Stderr,
		})
		if err != nil {
			return StepResult{}, false, "", err
		}
		defer inflightLease.Release()

		if !opts.NoCache {
			if sr, attRef, hit, err := checkCache(ctx, g, store, step.Name, fp, started, signingKey, principal, opts); err != nil || hit {
				return sr, false, attRef, err
			}
		}
	}

	expanded, err := config.ExpandCommand(step.Run, baseSHA, headSHA, treeSHA)
	if err != nil {
		return StepResult{}, false, "", err
	}

	var beforeStatus string
	if clean {
		beforeStatus, err = g.StatusPorcelain(ctx)
		if err != nil {
			return StepResult{}, false, "", err
		}
	}

	var capBuf bytes.Buffer
	stdout, stderr := opts.Stdout, opts.Stderr
	if opts.JSON {
		stdout, stderr = &capBuf, &capBuf
	} else if opts.Stdout != nil {
		fmt.Fprintf(opts.Stdout, "run     %s\n", step.Name)
	}

	capacity := 0
	var minFreeMem uint64
	if step.Pool != "" {
		sp := cfg.Slots[step.Pool]
		capacity = sp.Capacity
		minFreeMem = sp.MinFreeMemBytes
	}
	code, runErr := execrun.Run(ctx, execrun.Request{
		Dir:             topLevel,
		ShellCmd:        expanded,
		Timeout:         step.Duration,
		Stdout:          stdout,
		Stderr:          stderr,
		LocksRoot:       locksRoot,
		Pool:            step.Pool,
		Slots:           step.Slots,
		Capacity:        capacity,
		MinFreeMemBytes: minFreeMem,
		LockWait:        cfg.Land.LockWaitDur,
		Owner: lock.Owner{
			Command:  "quai verify --step " + step.Name,
			Worktree: topLevel,
		},
		OnStart: opts.OnStepStart,
	})
	dur := time.Since(started).Milliseconds()
	if runErr != nil || code != 0 {
		errMsg := ""
		if runErr != nil {
			errMsg = runErr.Error()
		} else {
			errMsg = fmt.Sprintf("exit code %d", code)
		}
		return StepResult{
			Name:        step.Name,
			Status:      "failed",
			Fingerprint: fp,
			DurationMS:  dur,
			Error:       errMsg,
		}, false, "", runErr
	}

	var attRef string
	receiptWritten := false
	if clean {
		afterHead, err := g.HeadSHA(ctx)
		if err != nil {
			return StepResult{}, false, "", err
		}
		afterStatus, err := g.StatusPorcelain(ctx)
		if err != nil {
			return StepResult{}, false, "", err
		}
		if afterHead != headSHA || afterStatus != beforeStatus {
			mutated := parseStatusPaths(afterStatus)
			if afterHead != headSHA {
				mutated = append(mutated, fmt.Sprintf("HEAD moved (%s -> %s)", shortSHA(headSHA), shortSHA(afterHead)))
			}
			return StepResult{
				Name:         step.Name,
				Status:       "mutated",
				Fingerprint:  fp,
				DurationMS:   dur,
				MutatedPaths: mutated,
				Error:        "step mutated the working tree or HEAD; refusing to record receipt",
			}, false, "", nil
		}

		r := receipt.Receipt{
			Version:     receipt.CurrentVersion,
			Fingerprint: fp,
			TreeSHA:     treeSHA,
			HeadSHA:     headSHA,
			BaseSHA:     baseSHA,
			Step:        step.Name,
			Passed:      true,
			DurationMS:  dur,
			CreatedAt:   time.Now().UTC(),
		}
		if err := store.Write(r); err != nil {
			return StepResult{}, false, "", err
		}
		receiptWritten = true

		if opts.Sign {
			ref, _, err := attest.WriteRecord(ctx, g, r, signingKey, principal)
			if err != nil {
				return StepResult{}, receiptWritten, "", err
			}
			attRef = ref
		}
	}

	sr := StepResult{
		Name:           step.Name,
		Status:         "passed",
		Fingerprint:    fp,
		DurationMS:     dur,
		AttestationRef: attRef,
	}
	if !opts.JSON && opts.Stdout != nil {
		fmt.Fprintf(opts.Stdout, "pass    %s (%dms)\n", step.Name, dur)
	}
	return sr, receiptWritten, attRef, nil
}

func checkCache(
	ctx context.Context,
	g repo.Git,
	store *receipt.Store,
	stepName, fp string,
	started time.Time,
	signingKey, principal string,
	opts Options,
) (StepResult, string, bool, error) {
	cachedRec, hit, err := store.Lookup(stepName, fp)
	if err != nil || !hit {
		return StepResult{}, "", false, nil
	}
	var attRef string
	if opts.Sign {
		ref := attest.RefName(stepName, fp)
		if _, exists, err := attest.ReadRecord(ctx, g, stepName, fp); err != nil {
			return StepResult{}, "", false, err
		} else if !exists {
			if _, _, err := attest.WriteRecord(ctx, g, *cachedRec, signingKey, principal); err != nil {
				return StepResult{}, "", false, err
			}
		}
		attRef = ref
	}
	sr := StepResult{
		Name:           stepName,
		Status:         "cached",
		Fingerprint:    fp,
		DurationMS:     time.Since(started).Milliseconds(),
		AttestationRef: attRef,
	}
	if !opts.JSON && opts.Stdout != nil {
		fmt.Fprintf(opts.Stdout, "cached  %s (%s)\n", stepName, shortSHA(fp))
	}
	return sr, attRef, true, nil
}

func selectSteps(cfg *config.Config, names []string) ([]config.Step, error) {
	if len(names) == 0 {
		return cfg.Steps, nil
	}
	out := make([]config.Step, 0, len(names))
	for _, n := range names {
		step, ok := cfg.StepByName(n)
		if !ok {
			return nil, fmt.Errorf("unknown step %q", n)
		}
		out = append(out, step)
	}
	return out, nil
}

func parseStatusPaths(porcelain string) []string {
	var paths []string
	for _, line := range strings.Split(strings.TrimRight(porcelain, "\n"), "\n") {
		if len(line) >= 4 {
			paths = append(paths, strings.TrimSpace(line[3:]))
		}
	}
	return paths
}

func shortSHA(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}
