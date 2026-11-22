package verify

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/attest"
	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/lock"
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
	if r1.Steps[0].Pool != "" || r1.Steps[0].PoolAcquired != nil {
		t.Fatalf("unconditional step should not report pool routing: %+v", r1.Steps[0])
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

func commitFile(t *testing.T, dir, relPath, content, message string) {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", message}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
}

func checkoutBranch(t *testing.T, dir, name string) {
	t.Helper()
	cmd := exec.Command("git", "checkout", "-q", "-b", name)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout -b %s: %v\n%s", name, err, string(out))
	}
}

func holdSlot(t *testing.T, ctx context.Context, g *repo.ExecGit, pool string, capacity int) {
	t.Helper()
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := lock.AcquireSlots(ctx, filepath.Join(commonDir, "quai", "locks"), pool, 1, capacity, lock.Owner{
		Command:  "test holds a slot",
		Worktree: g.Dir,
	}, lock.Options{Wait: 5 * time.Second})
	if err != nil {
		t.Fatalf("hold slot in %q: %v", pool, err)
	}
	t.Cleanup(func() { _ = lease.Release() })
}

func TestVerifyPoolWhenBypassesOccupiedPoolAndStillWritesReceipt(t *testing.T) {
	ctx := context.Background()
	dir, g := setupRepo(t)
	checkoutBranch(t, dir, "feat")
	commitFile(t, dir, "README.md", "v2\n", "docs only")

	holdSlot(t, ctx, g, "heavy", 1)

	cfg, err := config.Parse([]byte(`
version = 1

[slots.heavy]
capacity = 1
min_free_mem = "999999G"

[[steps]]
name = "e2e"
run = "test -f main.go"
pool = "heavy"
slots = 1
pool_when = ["internal/**"]

[land]
lock_wait = "200ms"
`))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	res, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	if err != nil || !res.OK || !res.ReceiptWritten {
		t.Fatalf("bypassed run should pass and write a receipt: res=%+v err=%v", res, err)
	}
	step := res.Steps[0]
	if step.Pool != "heavy" || step.PoolAcquired == nil || *step.PoolAcquired {
		t.Fatalf("expected an annotated bypassed pool, got %+v", step)
	}
	if !strings.Contains(buf.String(), "bypassed heavy") {
		t.Fatalf("human output should name the bypass, got %q", buf.String())
	}

	again, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	if err != nil || again.Steps[0].Status != "cached" {
		t.Fatalf("second run should reuse the bypassed receipt: res=%+v err=%v", again, err)
	}

	commitFile(t, dir, "internal/heavy.go", "package internal\n", "heavy change")
	buf.Reset()
	res, err = Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	var timeout *lock.TimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("expected slot timeout for a matching path, got res=%+v err=%v", res, err)
	}
	if len(res.Steps) != 1 || res.Steps[0].PoolAcquired == nil || *res.Steps[0].PoolAcquired {
		t.Fatalf("a timed-out wait must not claim the pool was acquired: %+v", res.Steps)
	}
	if !strings.Contains(buf.String(), "routed to heavy") {
		t.Fatalf("human output should name the routing decision, got %q", buf.String())
	}
	if strings.Contains(buf.String(), "acquired heavy") {
		t.Fatalf("human output must not claim acquisition before the slot is held, got %q", buf.String())
	}
}

func TestVerifyPoolWhenReportsAcquiredOnlyAfterSlotIsHeld(t *testing.T) {
	ctx := context.Background()
	dir, g := setupRepo(t)
	checkoutBranch(t, dir, "feat")
	commitFile(t, dir, "internal/heavy.go", "package internal\n", "heavy change")

	cfg, err := config.Parse([]byte(`
version = 1

[slots.heavy]
capacity = 1

[[steps]]
name = "e2e"
run = "test -f main.go"
pool = "heavy"
slots = 1
pool_when = ["internal/**"]
`))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	res, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	if err != nil || !res.OK || !res.ReceiptWritten {
		t.Fatalf("acquired run should pass and write a receipt: res=%+v err=%v", res, err)
	}
	if res.Steps[0].PoolAcquired == nil || !*res.Steps[0].PoolAcquired {
		t.Fatalf("expected pool_acquired true once the slot is held: %+v", res.Steps[0])
	}
	if !strings.Contains(buf.String(), "routed to heavy (1 changed path(s) matched)") {
		t.Fatalf("human output should report the routing decision, got %q", buf.String())
	}
	if !strings.Contains(buf.String(), "acquired heavy") {
		t.Fatalf("human output should report the held slot, got %q", buf.String())
	}
}

func TestVerifyUnconditionalPoolKeepsOutputUnchanged(t *testing.T) {
	ctx := context.Background()
	_, g := setupRepo(t)

	cfg, err := config.Parse([]byte(`
version = 1

[slots]
heavy = 2

[[steps]]
name = "e2e"
run = "true"
pool = "heavy"
slots = 1
`))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	res, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	if err != nil || !res.OK {
		t.Fatalf("unconditional pooled step should pass: res=%+v err=%v", res, err)
	}
	if res.Steps[0].Pool != "" || res.Steps[0].PoolAcquired != nil {
		t.Fatalf("unconditional step should not report pool routing: %+v", res.Steps[0])
	}
	if strings.Contains(buf.String(), "pool    ") {
		t.Fatalf("unconditional step should not print pool routing, got %q", buf.String())
	}
}

func TestVerifyPoolTiersRoutesByBranchTier(t *testing.T) {
	ctx := context.Background()
	dir, g := setupRepo(t)
	checkoutBranch(t, dir, "feat")
	commitFile(t, dir, "docs/guide.md", "guide\n", "docs only")
	holdSlot(t, ctx, g, "heavy", 1)

	cfg, err := config.Parse([]byte(`
version = 1

[slots.heavy]
capacity = 1

[[steps]]
name = "e2e"
run = "test -f main.go"
pool = "heavy"
slots = 1
pool_tiers = ["full-live"]

[tiers]
default = "diff-audit"
full_live = ["ui/**"]
gate_only = ["docs/**"]

[land]
lock_wait = "200ms"
`))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	res, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	if err != nil || !res.OK || res.Steps[0].PoolAcquired == nil || *res.Steps[0].PoolAcquired {
		t.Fatalf("gate-only diff should bypass the pool: res=%+v err=%v", res, err)
	}

	commitFile(t, dir, "ui/view.txt", "screen\n", "full-live change")
	res, err = Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	var timeout *lock.TimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("full-live diff should wait for the occupied pool, got res=%+v err=%v", res, err)
	}
	if len(res.Steps) != 1 || res.Steps[0].PoolAcquired == nil || *res.Steps[0].PoolAcquired {
		t.Fatalf("a timed-out wait must not claim the pool was acquired: %+v", res.Steps)
	}
}

func TestVerifyBypassedPoolSignsAttestation(t *testing.T) {
	ctx := context.Background()
	dir, g := setupRepo(t)
	checkoutBranch(t, dir, "feat")
	commitFile(t, dir, "README.md", "v2\n", "docs only")
	holdSlot(t, ctx, g, "heavy", 1)

	keyPath := filepath.Join(t.TempDir(), "signer")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, string(out))
	}

	cfg, err := config.Parse([]byte(`
version = 1

[slots.heavy]
capacity = 1

[[steps]]
name = "e2e"
run = "true"
pool = "heavy"
slots = 1
pool_when = ["internal/**"]

[attest]
signing_key = "` + keyPath + `"

[land]
lock_wait = "200ms"
`))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	res, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf, Sign: true})
	if err != nil || !res.OK || len(res.Attestations) != 1 {
		t.Fatalf("bypassed run should sign an attestation: res=%+v err=%v", res, err)
	}
	if _, ok, err := attest.ReadRecord(ctx, g, "e2e", res.Steps[0].Fingerprint); err != nil || !ok {
		t.Fatalf("attestation should exist for the bypassed run (ok=%v err=%v)", ok, err)
	}
}
