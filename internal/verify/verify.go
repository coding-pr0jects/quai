package verify

import (
	"bytes"
	"context"
	"fmt"
	"io"
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
	StepNames  []string
	Since      string
	NoCache    bool
	Sign       bool
	SigningKey string
	Push       bool
	Remote     string
	JSON       bool
	Stdout     io.Writer
	Stderr     io.Writer
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
// mutation guard, and writes input-fingerprinted receipts on clean passing runs.
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
			return Result{}, err
		}
		principal, _ = g.ConfigGet(ctx, "user.email")
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
		started := time.Now()
		var fp string
		if clean {
			fp, err = receipt.Fingerprint(ctx, g, step, treeSHA, baseSHA)
			if err != nil {
				return res, err
			}
			if !opts.NoCache {
				if cachedRec, hit, err := store.Lookup(step.Name, fp); err == nil && hit {
					var attRef string
					if opts.Sign {
						ref := attest.RefName(step.Name, fp)
						if _, exists, err := attest.ReadRecord(ctx, g, step.Name, fp); err != nil {
							return res, err
						} else if !exists {
							if _, _, err := attest.WriteRecord(ctx, g, *cachedRec, signingKey, principal); err != nil {
								return res, err
							}
						}
						attRef = ref
						res.Attestations = append(res.Attestations, ref)
					}
					res.Steps = append(res.Steps, StepResult{
						Name:           step.Name,
						Status:         "cached",
						Fingerprint:    fp,
						DurationMS:     time.Since(started).Milliseconds(),
						AttestationRef: attRef,
					})
					if !opts.JSON && opts.Stdout != nil {
						fmt.Fprintf(opts.Stdout, "cached  %s (%s)\n", step.Name, shortSHA(fp))
					}
					continue
				}
			}
		}

		expanded, err := config.ExpandCommand(step.Run, baseSHA, headSHA, treeSHA)
		if err != nil {
			return res, err
		}

		var beforeStatus string
		if clean {
			beforeStatus, err = g.StatusPorcelain(ctx)
			if err != nil {
				return res, err
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
		if step.Pool != "" {
			capacity = cfg.Slots[step.Pool]
		}
		code, runErr := execrun.Run(ctx, execrun.Request{
			Dir:       topLevel,
			ShellCmd:  expanded,
			Timeout:   step.Duration,
			Stdout:    stdout,
			Stderr:    stderr,
			LocksRoot: locksRoot,
			Pool:      step.Pool,
			Slots:     step.Slots,
			Capacity:  capacity,
			LockWait:  cfg.Land.LockWaitDur,
			Owner: lock.Owner{
				Command:  "quai verify --step " + step.Name,
				Worktree: topLevel,
			},
		})
		dur := time.Since(started).Milliseconds()
		if runErr != nil || code != 0 {
			res.OK = false
			errMsg := ""
			if runErr != nil {
				errMsg = runErr.Error()
			} else {
				errMsg = fmt.Sprintf("exit code %d", code)
			}
			res.Steps = append(res.Steps, StepResult{
				Name:        step.Name,
				Status:      "failed",
				Fingerprint: fp,
				DurationMS:  dur,
				Error:       errMsg,
			})
			return res, runErr
		}

		var attRef string
		if clean {
			afterHead, err := g.HeadSHA(ctx)
			if err != nil {
				return res, err
			}
			afterStatus, err := g.StatusPorcelain(ctx)
			if err != nil {
				return res, err
			}
			if afterHead != headSHA || afterStatus != beforeStatus {
				res.OK = false
				mutated := parseStatusPaths(afterStatus)
				if afterHead != headSHA {
					mutated = append(mutated, fmt.Sprintf("HEAD moved (%s -> %s)", shortSHA(headSHA), shortSHA(afterHead)))
				}
				res.Steps = append(res.Steps, StepResult{
					Name:         step.Name,
					Status:       "mutated",
					Fingerprint:  fp,
					DurationMS:   dur,
					MutatedPaths: mutated,
					Error:        "step mutated the working tree or HEAD; refusing to record receipt",
				})
				return res, nil
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
				return res, err
			}
			res.ReceiptWritten = true

			if opts.Sign {
				ref, _, err := attest.WriteRecord(ctx, g, r, signingKey, principal)
				if err != nil {
					return res, err
				}
				attRef = ref
				res.Attestations = append(res.Attestations, ref)
			}
		}

		res.Steps = append(res.Steps, StepResult{
			Name:           step.Name,
			Status:         "passed",
			Fingerprint:    fp,
			DurationMS:     dur,
			AttestationRef: attRef,
		})
		if !opts.JSON && opts.Stdout != nil {
			fmt.Fprintf(opts.Stdout, "pass    %s (%dms)\n", step.Name, dur)
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
