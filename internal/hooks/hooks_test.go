package hooks

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/repo"
	"github.com/coding-pr0jects/quai/internal/verify"
)

func initHookRepo(t *testing.T) (string, *repo.ExecGit) {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "t")
	run("config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "init")
	return dir, repo.NewExecGit(dir)
}

func TestInstallUninstallAndNonDestructiveRefusal(t *testing.T) {
	ctx := context.Background()
	dir, g := initHookRepo(t)

	res, err := Install(ctx, g)
	if err != nil || !res.Installed {
		t.Fatalf("Install failed: %+v err=%v", res, err)
	}
	if _, err := os.Stat(res.Path); err != nil {
		t.Fatalf("hook file missing: %v", err)
	}

	msg, err := Uninstall(ctx, g)
	if err != nil || !strings.Contains(msg, "removed") {
		t.Fatalf("Uninstall failed: %q err=%v", msg, err)
	}

	// Pre-existing unmanaged hook must not be overwritten.
	if err := os.WriteFile(res.Path, []byte("#!/bin/sh\necho custom\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	res2, err := Install(ctx, g)
	if err != nil || res2.Installed || res2.Snippet == "" {
		t.Fatalf("expected non-destructive refusal with snippet, got %+v err=%v", res2, err)
	}
	if _, err := Uninstall(ctx, g); err == nil {
		t.Fatal("expected Uninstall to refuse removing unmanaged hook")
	}

	// Custom core.hooksPath must not be overwritten.
	cmd := exec.Command("git", "config", "core.hooksPath", ".githooks")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config core.hooksPath: %v\n%s", err, string(out))
	}
	res3, err := Install(ctx, g)
	if err != nil || res3.Installed || !strings.Contains(res3.Message, ".githooks") {
		t.Fatalf("expected core.hooksPath refusal, got %+v err=%v", res3, err)
	}
}

func TestPrePushHitsReceiptFast(t *testing.T) {
	ctx := context.Background()
	_, g := initHookRepo(t)
	cfg, err := config.Parse([]byte(`
version = 1
[[steps]]
name = "fast"
run = "sleep 0.2"
hook = "pre-push"
`))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if r, err := verify.Run(ctx, g, cfg, verify.Options{Stdout: &buf, Stderr: &buf}); err != nil || !r.OK {
		t.Fatalf("seed verify failed: %+v err=%v", r, err)
	}

	started := time.Now()
	r, err := RunPrePush(ctx, g, cfg, false, &buf, &buf)
	elapsed := time.Since(started)
	if err != nil || !r.OK || len(r.Steps) != 1 || r.Steps[0].Status != "cached" {
		t.Fatalf("expected cached pre-push hit, got %+v err=%v", r, err)
	}
	if elapsed > 150*time.Millisecond {
		t.Fatalf("cached pre-push took %s, expected fast hit", elapsed)
	}
}
