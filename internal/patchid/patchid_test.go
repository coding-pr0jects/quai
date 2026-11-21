package patchid

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/smbss1/quai/internal/repo"
)

func TestCompareSurvivesCleanRebaseAndDetectsDrift(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runGit("init", "-q", "-b", "main")
	runGit("config", "user.name", "t")
	runGit("config", "user.email", "t@example.com")
	write("a.txt", "a1\n")
	write("b.txt", "b1\n")
	runGit("add", "-A")
	runGit("commit", "-qm", "base")

	// Branch feat edits b.txt.
	runGit("switch", "-q", "-c", "feat")
	write("b.txt", "b1\nb2\n")
	runGit("commit", "-qam", "feat edit")
	g := repo.NewExecGit(dir)
	verdictSHA, _ := g.HeadSHA(ctx)

	// Main advances on unrelated a.txt.
	runGit("switch", "-q", "main")
	write("a.txt", "a1\na2\n")
	runGit("commit", "-qam", "main edit")

	// Rebase feat cleanly onto main -> new SHA, identical patch-id -> SURVIVES.
	runGit("switch", "-q", "feat")
	runGit("rebase", "main")
	rebasedSHA, _ := g.HeadSHA(ctx)
	if rebasedSHA == verdictSHA {
		t.Fatal("expected rebase to produce a new SHA")
	}

	res, err := Compare(ctx, g, "main", verdictSHA, rebasedSHA)
	if err != nil {
		t.Fatalf("Compare after clean rebase: %v", err)
	}
	if res.Status != StatusSurvives || len(res.ChangedFiles) != 0 {
		t.Fatalf("expected SURVIVES, got %+v", res)
	}

	// Edit b.txt after rebase -> CHANGED with ChangedFiles == ["b.txt"].
	write("b.txt", "b1\nb2\nb3\n")
	runGit("commit", "-qam", "extra edit")
	res2, err := Compare(ctx, g, "main", verdictSHA, "HEAD")
	if err != nil {
		t.Fatalf("Compare after drift: %v", err)
	}
	if res2.Status != StatusChanged || len(res2.ChangedFiles) != 1 || res2.ChangedFiles[0] != "b.txt" {
		t.Fatalf("expected CHANGED on b.txt, got %+v", res2)
	}
}
