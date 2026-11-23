package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/repo"
	"github.com/coding-pr0jects/quai/internal/verify"
)

const ManagedMarker = "# managed by quai"

const hookScript = `#!/bin/sh
` + ManagedMarker + `
set -eu
if ! command -v quai >/dev/null 2>&1; then
  echo "quai hooks pre-push: quai binary not found on PATH" >&2
  exit 1
fi
exec quai hooks pre-push
`

const ChainSnippet = `if command -v quai >/dev/null 2>&1 && [ -f .quai.toml ]; then quai hooks pre-push || exit 1; fi`

type InstallResult struct {
	Installed bool   `json:"installed"`
	Path      string `json:"path"`
	Message   string `json:"message"`
	Snippet   string `json:"snippet,omitempty"`
}

// Install writes a managed pre-push hook unless core.hooksPath is customized or an unmanaged hook exists.
func Install(ctx context.Context, g repo.Git) (InstallResult, error) {
	if customPath, _ := g.ConfigGet(ctx, "core.hooksPath"); customPath != "" {
		return InstallResult{
			Installed: false,
			Path:      filepath.Join(customPath, "pre-push"),
			Message:   fmt.Sprintf("core.hooksPath is set to %q; chain quai manually in %s/pre-push", customPath, customPath),
			Snippet:   ChainSnippet,
		}, nil
	}

	hookPath, err := g.GitPath(ctx, "hooks/pre-push")
	if err != nil {
		return InstallResult{}, err
	}
	if data, err := os.ReadFile(hookPath); err == nil {
		if !strings.Contains(string(data), ManagedMarker) {
			return InstallResult{
				Installed: false,
				Path:      hookPath,
				Message:   fmt.Sprintf("%s already exists and is not managed by quai; chain it manually", hookPath),
				Snippet:   ChainSnippet,
			}, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return InstallResult{}, fmt.Errorf("read %s: %w", hookPath, err)
	}

	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("create hooks dir: %w", err)
	}
	if err := os.WriteFile(hookPath, []byte(hookScript), 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("write %s: %w", hookPath, err)
	}
	return InstallResult{
		Installed: true,
		Path:      hookPath,
		Message:   fmt.Sprintf("installed managed pre-push hook at %s", hookPath),
	}, nil
}

// Uninstall removes the pre-push hook only if it carries the quai managed marker.
func Uninstall(ctx context.Context, g repo.Git) (string, error) {
	hookPath, err := g.GitPath(ctx, "hooks/pre-push")
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(hookPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "no pre-push hook installed", nil
		}
		return "", err
	}
	if !strings.Contains(string(data), ManagedMarker) {
		return "", fmt.Errorf("%s is not managed by quai; refusing to remove", hookPath)
	}
	if err := os.Remove(hookPath); err != nil {
		return "", err
	}
	return fmt.Sprintf("removed managed pre-push hook at %s", hookPath), nil
}

type Options struct {
	Refs         []PushRef
	RefsProvided bool
	JSON         bool
	Stdout       io.Writer
	Stderr       io.Writer
}

type Result struct {
	verify.Result
	RequireCached        []CachedStep `json:"require_cached,omitempty"`
	AttestationsPushed   []string     `json:"attestations_pushed,omitempty"`
	AttestationPushError string       `json:"attestation_push_error,omitempty"`
	Bypassed             bool         `json:"require_cached_bypassed,omitempty"`
	Skipped              string       `json:"skipped,omitempty"`
	Warning              string       `json:"warning,omitempty"`
}

// RunPrePush runs the steps configured with hook = "pre-push", then checks
// [hooks.pre_push].require_cached for every pushed branch commit from the local receipt cache.
// A missing receipt warns or refuses according to on_missing, unless QUAI_ALLOW_UNVERIFIED_PUSH=1.
func RunPrePush(ctx context.Context, g repo.Git, cfg *config.Config, opts Options) (Result, error) {
	res := Result{Result: verify.Result{OK: true, Clean: true}}

	if opts.RefsProvided && IsQuaiRefPush(opts.Refs) {
		res.Skipped = "quai ref push"
		if !opts.JSON && opts.Stdout != nil {
			fmt.Fprintf(opts.Stdout, "skipped pre-push checks: quai ref push (%s*)\n", quaiRefPrefix)
		}
		return res, nil
	}

	var warnings []string
	unknownRef := opts.RefsProvided && len(opts.Refs) == 0
	targets := pushTargets(ctx, g, opts.Refs, opts.RefsProvided)

	steps := cfg.PrePushSteps()
	if len(steps) > 0 {
		names := make([]string, len(steps))
		for i, s := range steps {
			names[i] = s.Name
		}
		doSign, doPush, bestEffort := verify.ResolveAutoAttest(cfg, false, false, false, false)
		verifyRes, err := verify.Run(ctx, g, cfg, verify.Options{
			StepNames:      names,
			Sign:           doSign,
			BestEffortSign: bestEffort,
			Push:           doPush,
			UnsetEnv:       []string{EnvAllowUnverifiedPush, EnvPrePushActive},
			JSON:           opts.JSON,
			Stdout:         opts.Stdout,
			Stderr:         opts.Stderr,
		})
		res.Result = verifyRes
		if err != nil || !verifyRes.OK {
			return res, err
		}
	}

	if len(cfg.Hooks.PrePush.RequireCached) == 0 {
		return res, nil
	}
	if unknownRef {
		checked := "HEAD"
		if len(targets) > 0 {
			checked = targets[0].Ref
		}
		warnings = append(warnings, fmt.Sprintf("stdin carried no refs, so the pushed ref is unknown and %s was checked instead; run quai hooks install to pass the pushed refs", checked))
	}
	report, err := CheckCached(ctx, g, cfg, targets, "")
	if err != nil {
		return res, err
	}
	res.RequireCached = report.Steps
	res.AttestationsPushed = report.Pushed
	res.AttestationPushError = report.PushError
	if report.PushError != "" {
		warnings = append(warnings, "attestation push failed: "+report.PushError)
	}

	decision := decideRequirement(report.Missing, cfg.Hooks.PrePush, os.Getenv(EnvAllowUnverifiedPush) == "1", unknownRef)
	res.Bypassed = decision.bypassed
	if decision.message != "" {
		warnings = append(warnings, decision.message)
	}
	res.OK = !decision.blocked
	res.Warning = strings.Join(warnings, "; ")
	for _, warning := range warnings {
		writeWarning(opts, warning)
	}
	return res, nil
}

func writeWarning(opts Options, message string) {
	if opts.Stderr != nil {
		fmt.Fprintf(opts.Stderr, "quai hooks pre-push: %s\n", message)
	}
}
