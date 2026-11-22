package conflicts

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/repo"
)

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
	}
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEvaluateCrossWorktreeCollisionsAndBaseConflicts(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	mainDir := filepath.Join(tmp, "main")
	wtA := filepath.Join(tmp, "wt-a")
	wtB := filepath.Join(tmp, "wt-b")
	wtC := filepath.Join(tmp, "wt-c")

	gitCmd(t, tmp, "init", "-q", "-b", "main", mainDir)
	gitCmd(t, mainDir, "config", "user.name", "t")
	gitCmd(t, mainDir, "config", "user.email", "t@example.com")
	writeFile(t, filepath.Join(mainDir, "shared.go"), "package p\nfunc A() int { return 1 }\n\n\n\n\nfunc B() int { return 2 }\n")
	writeFile(t, filepath.Join(mainDir, "other.go"), "package p\nfunc C() int { return 3 }\n")
	gitCmd(t, mainDir, "add", "-A")
	gitCmd(t, mainDir, "commit", "-qm", "init")

	// Create three linked worktrees.
	gitCmd(t, mainDir, "worktree", "add", "-q", "-b", "feat/a", wtA, "main")
	gitCmd(t, mainDir, "worktree", "add", "-q", "-b", "feat/b", wtB, "main")
	gitCmd(t, mainDir, "worktree", "add", "-q", "-b", "feat/c", wtC, "main")

	// wt-a and wt-b edit conflicting lines in shared.go; wt-c edits disjoint other.go.
	writeFile(t, filepath.Join(wtA, "shared.go"), "package p\nfunc A() int { return 10 }\n\n\n\n\nfunc B() int { return 2 }\n")
	gitCmd(t, wtA, "commit", "-qam", "edit A")

	writeFile(t, filepath.Join(wtB, "shared.go"), "package p\nfunc A() int { return 20 }\n\n\n\n\nfunc B() int { return 2 }\n")
	gitCmd(t, wtB, "commit", "-qam", "edit A conflicting")

	writeFile(t, filepath.Join(wtC, "other.go"), "package p\nfunc C() int { return 30 }\n")
	gitCmd(t, wtC, "commit", "-qam", "edit C disjoint")

	g := repo.NewExecGit(mainDir)
	cfg := &config.Config{Base: "main"}

	res, err := Evaluate(ctx, g, cfg, Options{})
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if res.OK {
		t.Fatalf("expected collision between feat/a and feat/b, got OK=true: %+v", res)
	}
	if len(res.Collisions) != 1 {
		t.Fatalf("expected exactly 1 pair collision, got %+v", res.Collisions)
	}
	col := res.Collisions[0]
	if !col.Conflict || len(col.ConflictFiles) != 1 || col.ConflictFiles[0] != "shared.go" || len(col.OverlapFiles) != 1 {
		t.Fatalf("unexpected collision details: %+v", col)
	}

	// Now advance main with a conflicting edit to other.go -> wt-c must report a base conflict.
	writeFile(t, filepath.Join(mainDir, "other.go"), "package p\nfunc C() int { return 999 }\n")
	gitCmd(t, mainDir, "commit", "-qam", "main edits other.go")

	res2, err := Evaluate(ctx, g, cfg, Options{})
	if err != nil {
		t.Fatalf("Evaluate after main move failed: %v", err)
	}
	if len(res2.BaseConflicts) != 1 || res2.BaseConflicts[0].Branch != "feat/c" || res2.BaseConflicts[0].ConflictFiles[0] != "other.go" {
		t.Fatalf("expected base conflict for feat/c on other.go, got %+v", res2.BaseConflicts)
	}

	// Ensure no worktree was modified.
	for _, dir := range []string{mainDir, wtA, wtB, wtC} {
		clean, _, err := repo.NewExecGit(dir).IsClean(ctx)
		if err != nil || !clean {
			t.Fatalf("worktree %s was dirtied by conflicts check", dir)
		}
	}
}

func TestEvaluateCleanDisjointWorktreesReturnOK(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	mainDir := filepath.Join(tmp, "main")
	wt1 := filepath.Join(tmp, "wt-1")
	wt2 := filepath.Join(tmp, "wt-2")

	gitCmd(t, tmp, "init", "-q", "-b", "main", mainDir)
	gitCmd(t, mainDir, "config", "user.name", "t")
	gitCmd(t, mainDir, "config", "user.email", "t@example.com")
	writeFile(t, filepath.Join(mainDir, "a.txt"), "1\n")
	writeFile(t, filepath.Join(mainDir, "b.txt"), "1\n")
	gitCmd(t, mainDir, "add", "-A")
	gitCmd(t, mainDir, "commit", "-qm", "init")

	gitCmd(t, mainDir, "worktree", "add", "-q", "-b", "feat/1", wt1, "main")
	gitCmd(t, mainDir, "worktree", "add", "-q", "-b", "feat/2", wt2, "main")
	writeFile(t, filepath.Join(wt1, "a.txt"), "2\n")
	gitCmd(t, wt1, "commit", "-qam", "1")
	writeFile(t, filepath.Join(wt2, "b.txt"), "2\n")
	gitCmd(t, wt2, "commit", "-qam", "2")

	res, err := Evaluate(ctx, repo.NewExecGit(mainDir), &config.Config{Base: "main"}, Options{})
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if !res.OK || len(res.Collisions) != 0 || len(res.BaseConflicts) != 0 {
		t.Fatalf("expected clean disjoint OK, got %+v", res)
	}
}
