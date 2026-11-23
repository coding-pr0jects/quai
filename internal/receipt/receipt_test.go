package receipt

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/repo"
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

func TestLookupFreshIgnoresExpiredReceipt(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	store := &Store{
		Dir:      t.TempDir(),
		MaxFiles: DefaultMaxFiles,
		MaxAge:   24 * time.Hour,
		Now:      func() time.Time { return now },
	}
	if err := store.Write(Receipt{Step: "test", Fingerprint: "fresh", Passed: true, CreatedAt: now.Add(-time.Hour)}); err != nil {
		t.Fatalf("write fresh: %v", err)
	}
	stale := filepath.Join(store.Dir, "test-stale.json")
	if err := os.WriteFile(stale, []byte(`{"version":1,"fingerprint":"stale","step":"test","passed":true,"created_at":"2026-11-20T11:00:00Z"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := store.LookupFresh("test", "fresh"); err != nil || !ok {
		t.Fatalf("expected fresh hit, ok=%v err=%v", ok, err)
	}
	if _, ok, _ := store.Lookup("test", "stale"); !ok {
		t.Fatal("expected Lookup to read the stale receipt")
	}
	if _, ok, err := store.LookupFresh("test", "stale"); err != nil || ok {
		t.Fatalf("expected LookupFresh to reject the expired receipt, ok=%v err=%v", ok, err)
	}
}

func TestFingerprintStableAcrossUnrelatedChanges(t *testing.T) {
	cases := []struct {
		name      string
		inputs    []string
		lockfiles []string
	}{
		{name: "narrow inputs", inputs: []string{"**/*.go"}},
		{name: "negated inputs", inputs: []string{"**", "!README.md", "!docs/evidence/**"}},
		{name: "lockfile beats an exclusion", inputs: []string{"**", "!README.md", "!docs/evidence/**", "!go.sum"}, lockfiles: []string{"go.sum"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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
			g := repo.NewExecGit(dir)
			step := config.Step{Name: "go", Run: "go test ./...", Inputs: tc.inputs, Lockfiles: tc.lockfiles}
			fingerprint := func() string {
				t.Helper()
				tree, err := g.TreeSHA(ctx, "HEAD")
				if err != nil {
					t.Fatal(err)
				}
				fp, err := Fingerprint(ctx, g, step, tree, "")
				if err != nil {
					t.Fatalf("fingerprint: %v", err)
				}
				return fp
			}

			runGit("init", "-q", "-b", "main")
			runGit("config", "user.name", "t")
			runGit("config", "user.email", "t@example.com")
			write("pkg/a.go", "package pkg\n")
			write("README.md", "v1\n")
			runGit("add", "-A")
			runGit("commit", "-qm", "init")
			fp1 := fingerprint()

			write("README.md", "v2\n")
			write("docs/evidence/shot.png", "png\n")
			runGit("add", "-A")
			runGit("commit", "-qm", "docs")
			if fp2 := fingerprint(); fp1 != fp2 {
				t.Fatalf("fingerprint changed on a commit outside inputs: %s vs %s", fp1, fp2)
			}

			write("pkg/a.go", "package pkg\nvar X = 1\n")
			runGit("commit", "-qam", "code")
			fp3 := fingerprint()
			if fp1 == fp3 {
				t.Fatal("fingerprint did not change when an included input changed")
			}

			if len(tc.lockfiles) > 0 {
				write("go.sum", "sum\n")
				runGit("add", "-A")
				runGit("commit", "-qm", "lockfile")
				if fingerprint() == fp3 {
					t.Fatal("fingerprint did not change when a lockfile changed")
				}
			}
		})
	}
}
