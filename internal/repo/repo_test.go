package repo

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initTestRepo(t *testing.T) (string, *ExecGit) {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.name", "test")
	runGit("config", "user.email", "test@example.com")
	writeFile(t, dir, "a.txt", "line1\n")
	writeFile(t, dir, "b.txt", "hello\n")
	runGit("add", "-A")
	runGit("commit", "-q", "-m", "initial")
	return dir, NewExecGit(dir)
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExecGitQueriesAndMergeTree(t *testing.T) {
	ctx := context.Background()
	dir, g := initTestRepo(t)

	branch, err := g.CurrentBranch(ctx)
	if err != nil || branch != "main" {
		t.Fatalf("CurrentBranch = %q, %v", branch, err)
	}
	clean, _, err := g.IsClean(ctx)
	if err != nil || !clean {
		t.Fatalf("IsClean = %v, %v", clean, err)
	}

	// Create branch feat touching b.txt, while main touches a.txt -> clean merge-tree.
	execGit(t, dir, "switch", "-q", "-c", "feat")
	writeFile(t, dir, "b.txt", "hello world\n")
	execGit(t, dir, "commit", "-qam", "feat change")

	execGit(t, dir, "switch", "-q", "main")
	writeFile(t, dir, "a.txt", "line1-main\n")
	execGit(t, dir, "commit", "-qam", "main change")

	res, err := g.MergeTree(ctx, "main", "feat")
	if err != nil {
		t.Fatalf("MergeTree clean failed: %v", err)
	}
	if !res.Clean || len(res.TreeSHA) != 40 {
		t.Fatalf("expected clean MergeTree with 40-char tree SHA, got %+v", res)
	}

	// Create conflicting branch touching a.txt differently -> conflict merge-tree and aborted rebase.
	execGit(t, dir, "switch", "-q", "-c", "conflict", "HEAD~1")
	writeFile(t, dir, "a.txt", "line1-conflict\n")
	execGit(t, dir, "commit", "-qam", "conflict change")

	cres, err := g.MergeTree(ctx, "main", "conflict")
	if err != nil {
		t.Fatalf("MergeTree conflict returned error: %v", err)
	}
	if cres.Clean || len(cres.ConflictFiles) != 1 || cres.ConflictFiles[0] != "a.txt" {
		t.Fatalf("unexpected conflict MergeTree result: %+v", cres)
	}

	err = g.Rebase(ctx, "main")
	var rce *RebaseConflictError
	if !errors.As(err, &rce) {
		t.Fatalf("expected RebaseConflictError, got %v", err)
	}
	cleanAfter, _, err := g.IsClean(ctx)
	if err != nil || !cleanAfter {
		t.Fatalf("expected clean tree after aborted rebase, clean=%v err=%v", cleanAfter, err)
	}
}

func TestListTreeAndRenameDetection(t *testing.T) {
	ctx := context.Background()
	dir, g := initTestRepo(t)

	entries, err := g.ListTree(ctx, "HEAD")
	if err != nil || len(entries) != 2 {
		t.Fatalf("ListTree = %+v, %v", entries, err)
	}

	execGit(t, dir, "switch", "-q", "-c", "rename")
	execGit(t, dir, "mv", "b.txt", "b-moved.txt")
	execGit(t, dir, "commit", "-qm", "pure rename")

	changes, err := g.DiffNameStatus(ctx, "main", "HEAD", true)
	if err != nil {
		t.Fatalf("DiffNameStatus: %v", err)
	}
	if len(changes) != 1 || changes[0].Status != "R100" || changes[0].OldPath != "b.txt" || changes[0].Path != "b-moved.txt" {
		t.Fatalf("unexpected rename diff: %+v", changes)
	}
}

func execGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, string(out))
	}
}

func TestRemoteAndRefArgumentsCannotInjectOptions(t *testing.T) {
	ctx := context.Background()
	dir, g := initTestRepo(t)
	origin := filepath.Join(t.TempDir(), "origin.git")
	execGit(t, dir, "init", "--bare", "-q", origin)
	execGit(t, dir, "remote", "add", "origin", origin)
	execGit(t, dir, "push", "-q", "origin", "HEAD:refs/heads/main")

	refs, err := g.ListRemoteRefs(ctx, "origin", []string{"HEAD", "refs/heads/*"})
	if err != nil {
		t.Fatalf("ListRemoteRefs: %v", err)
	}
	if len(refs) == 0 {
		t.Fatal("expected the bare remote to expose refs/heads/main")
	}

	marker := filepath.Join(t.TempDir(), "pwned")
	payload := "--upload-pack=touch " + marker + ";true"
	if _, err := g.ListRemoteRefs(ctx, payload, []string{"HEAD"}); err == nil {
		t.Fatal("expected ls-remote to reject an option-shaped remote name")
	}
	if err := g.Fetch(ctx, payload, "main"); err == nil {
		t.Fatal("expected fetch to reject an option-shaped remote name")
	}
	if err := g.Fetch(ctx, "origin", payload); err == nil {
		t.Fatal("expected fetch to reject an option-shaped ref")
	}
	if err := g.PushRefs(ctx, payload, []string{"refs/heads/main"}); err == nil {
		t.Fatal("expected push to reject an option-shaped remote name")
	}
	written := filepath.Join(t.TempDir(), "diff-output")
	if _, err := g.DiffBinaryPaths(ctx, "--output="+written, "HEAD", []string{"README.md"}); err == nil {
		t.Fatal("expected diff to reject an option-shaped revision")
	}
	if _, err := os.Stat(written); !os.IsNotExist(err) {
		t.Fatalf("option injection executed: diff wrote %s", written)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("option injection executed: marker %s exists", marker)
	}
}

func TestResetHardRequiresCommitSHA(t *testing.T) {
	ctx := context.Background()
	_, g := initTestRepo(t)
	if err := g.ResetHard(ctx, "--upload-pack=touch /tmp/quai-pwn;true"); err == nil {
		t.Fatal("expected reset --hard to reject a non-SHA revision")
	}
	head, err := g.HeadSHA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.ResetHard(ctx, head); err != nil {
		t.Fatalf("reset --hard to HEAD SHA: %v", err)
	}
}

func TestBranchAndSymrefArgumentsCannotInjectOptions(t *testing.T) {
	ctx := context.Background()
	_, g := initTestRepo(t)
	marker := filepath.Join(t.TempDir(), "pwned")
	payload := "--upload-pack=touch " + marker + ";true"

	if err := g.CheckRefFormatBranch(ctx, payload); err == nil {
		t.Fatal("expected check-ref-format to reject an option-shaped branch")
	}
	if _, err := g.SymbolicRef(ctx, payload); err == nil {
		t.Fatal("expected symbolic-ref to reject an option-shaped ref")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("option injection executed: marker %s exists", marker)
	}
	if err := g.CheckRefFormatBranch(ctx, "main"); err != nil {
		t.Fatalf("check-ref-format main: %v", err)
	}
	if _, err := g.SymbolicRef(ctx, "HEAD"); err != nil {
		t.Fatalf("symbolic-ref HEAD: %v", err)
	}
}
