package receipt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/smbss1/quai/internal/config"
	"github.com/smbss1/quai/internal/repo"
)

func TestStoreWriteLookupAndPrune(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	store := &Store{
		Dir:      t.TempDir(),
		MaxFiles: 2,
		MaxAge:   24 * time.Hour,
		Now:      func() time.Time { return now },
	}

	if err := store.Write(Receipt{Step: "fast", Fingerprint: "expired", Passed: true, CreatedAt: now.Add(-48 * time.Hour)}); err != nil {
		t.Fatalf("write expired: %v", err)
	}
	if _, ok, _ := store.Lookup("fast", "expired"); ok {
		t.Fatal("expired receipt should have been pruned")
	}

	for i, fp := range []string{"fp1", "fp2", "fp3"} {
		r := Receipt{Step: "fast", Fingerprint: fp, Passed: true, CreatedAt: now.Add(time.Duration(i) * time.Minute)}
		if err := store.Write(r); err != nil {
			t.Fatalf("write %s: %v", fp, err)
		}
	}
	if _, ok, _ := store.Lookup("fast", "fp1"); ok {
		t.Fatal("oldest receipt fp1 should be pruned when MaxFiles=2")
	}
	if _, ok, _ := store.Lookup("fast", "fp2"); !ok {
		t.Fatal("expected fp2 to remain")
	}
	if _, ok, _ := store.Lookup("fast", "fp3"); !ok {
		t.Fatal("expected fp3 to remain")
	}
}

func TestFingerprintStableAcrossUnrelatedChanges(t *testing.T) {
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
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runGit("init", "-q", "-b", "main")
	runGit("config", "user.name", "t")
	runGit("config", "user.email", "t@example.com")
	write("pkg/a.go", "package pkg\n")
	write("README.md", "v1\n")
	runGit("add", "-A")
	runGit("commit", "-qm", "init")

	g := repo.NewExecGit(dir)
	step := config.Step{Name: "go", Run: "go test ./...", Inputs: []string{"**/*.go"}}

	t1, _ := g.TreeSHA(ctx, "HEAD")
	fp1, err := Fingerprint(ctx, g, step, t1, "")
	if err != nil {
		t.Fatalf("fp1: %v", err)
	}

	// Edit README.md (outside inputs) -> fingerprint must stay identical.
	write("README.md", "v2\n")
	runGit("commit", "-qam", "docs")
	t2, _ := g.TreeSHA(ctx, "HEAD")
	fp2, err := Fingerprint(ctx, g, step, t2, "")
	if err != nil {
		t.Fatalf("fp2: %v", err)
	}
	if fp1 != fp2 {
		t.Fatalf("fingerprint changed on unrelated README edit: %s vs %s", fp1, fp2)
	}

	// Edit pkg/a.go (inside inputs) -> fingerprint must change.
	write("pkg/a.go", "package pkg\nvar X = 1\n")
	runGit("commit", "-qam", "code")
	t3, _ := g.TreeSHA(ctx, "HEAD")
	fp3, err := Fingerprint(ctx, g, step, t3, "")
	if err != nil {
		t.Fatalf("fp3: %v", err)
	}
	if fp2 == fp3 {
		t.Fatal("fingerprint did not change when matching input changed")
	}
}
