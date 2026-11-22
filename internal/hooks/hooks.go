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
exec quai hooks pre-push </dev/null
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

// RunPrePush runs all steps configured with hook = "pre-push", reusing cached receipts when valid
// and honoring [attest].auto_sign / auto_push (or QUAI_SIGN / QUAI_PUSH).
func RunPrePush(ctx context.Context, g repo.Git, cfg *config.Config, jsonOut bool, stdout, stderr io.Writer) (verify.Result, error) {
	steps := cfg.PrePushSteps()
	if len(steps) == 0 {
		return verify.Result{OK: true, Clean: true}, nil
	}
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.Name
	}
	doSign, doPush, bestEffort := verify.ResolveAutoAttest(cfg, false, false, false, false)
	return verify.Run(ctx, g, cfg, verify.Options{
		StepNames:      names,
		Sign:           doSign,
		BestEffortSign: bestEffort,
		Push:           doPush,
		JSON:           jsonOut,
		Stdout:         stdout,
		Stderr:         stderr,
	})
}
