package verify

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/repo"
)

func setupRepo(t *testing.T) (string, *repo.ExecGit) {
	t.Helper()
	dir := t.TempDir()
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.name", "t")
	runGit("config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-qm", "init")
	return dir, repo.NewExecGit(dir)
}

func TestVerifyCachesReceiptsAndSurvivesUnrelatedCommits(t *testing.T) {
	ctx := context.Background()
	dir, g := setupRepo(t)
	counter := filepath.Join(t.TempDir(), "runs.log")

	cfg, err := config.Parse([]byte(`
version = 1
[[steps]]
name = "code"
run = "echo x >> ` + counter + `"
inputs = ["**/*.go"]
`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	var buf bytes.Buffer
	r1, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	if err != nil || !r1.OK || !r1.ReceiptWritten || r1.Steps[0].Status != "passed" {
		t.Fatalf("first run unexpected: res=%+v err=%v", r1, err)
	}

	// Second run on identical tree must hit receipt cache.
	r2, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	if err != nil || !r2.OK || r2.Steps[0].Status != "cached" {
		t.Fatalf("second run should cache: res=%+v err=%v", r2, err)
	}

	// Commit README.md change (outside **/*.go) -> still cached.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "commit", "-qam", "docs")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit docs: %v\n%s", err, string(out))
	}

	r3, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	if err != nil || !r3.OK || r3.Steps[0].Status != "cached" {
		t.Fatalf("unrelated commit should reuse fingerprint receipt: res=%+v err=%v", r3, err)
	}

	raw, _ := os.ReadFile(counter)
	if len(bytes.Split(bytes.TrimSpace(raw), []byte("\n"))) != 1 {
		t.Fatalf("expected command to run exactly once, log=%q", string(raw))
	}
}

func TestVerifyInFlightDeduplicatesConcurrentRuns(t *testing.T) {
	ctx := context.Background()
	_, g := setupRepo(t)
	counter := filepath.Join(t.TempDir(), "concurrent.log")

	cfg, err := config.Parse([]byte(`
version = 1
[[steps]]
name = "slow"
run = "sleep 0.25 && echo x >> ` + counter + `"
inputs = ["**/*.go"]
`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	results := make([]Result, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			var buf bytes.Buffer
			results[idx], errs[idx] = Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
		}(i)
	}
	wg.Wait()

	for i := 0; i < 2; i++ {
		if errs[i] != nil || !results[i].OK || len(results[i].Steps) != 1 {
			t.Fatalf("caller %d failed: res=%+v err=%v", i, results[i], errs[i])
		}
	}

	passed, cached := 0, 0
	for _, r := range results {
		switch r.Steps[0].Status {
		case "passed":
			passed++
		case "cached":
			cached++
		}
	}
	if passed != 1 || cached != 1 {
		t.Fatalf("expected 1 passed and 1 cached, got statuses=%q,%q", results[0].Steps[0].Status, results[1].Steps[0].Status)
	}

	raw, _ := os.ReadFile(counter)
	if len(bytes.Split(bytes.TrimSpace(raw), []byte("\n"))) != 1 {
		t.Fatalf("expected command to run exactly once concurrently, log=%q", string(raw))
	}
}

func TestVerifyRefusesReceiptOnDirtyTreeAndOnMutation(t *testing.T) {
	ctx := context.Background()
	_, g := setupRepo(t)

	// Step that mutates a tracked file must fail with status "mutated" and write no receipt.
	cfgMut, err := config.Parse([]byte(`
version = 1
[[steps]]
name = "mutator"
run = "echo dirty >> main.go"
`))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	rm, err := Run(ctx, g, cfgMut, Options{Stdout: &buf, Stderr: &buf})
	if err != nil || rm.OK || rm.ReceiptWritten || rm.Steps[0].Status != "mutated" {
		t.Fatalf("expected mutated refusal, got res=%+v err=%v", rm, err)
	}

	// Dirty tree before run: runs check, reports OK, but writes no receipt.
	cfgOk, _ := config.Parse([]byte(`
version = 1
[[steps]]
name = "ok"
run = "true"
`))
	rd, err := Run(ctx, g, cfgOk, Options{Stdout: &buf, Stderr: &buf})
	if err != nil || !rd.OK || rd.Clean || rd.ReceiptWritten {
		t.Fatalf("dirty tree run should succeed without writing receipt: res=%+v err=%v", rd, err)
	}
}
