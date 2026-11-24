package verify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/attest"
	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/exitkind"
	"github.com/coding-pr0jects/quai/internal/lock"
	"github.com/coding-pr0jects/quai/internal/receipt"
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
	if err != nil || !r1.OK || !r1.ReceiptWritten || r1.Steps[0].Status != "passed" || r1.ExitKind != exitkind.Pass {
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
	if err != nil || rm.OK || rm.ReceiptWritten || rm.Steps[0].Status != "mutated" || rm.ExitKind != exitkind.DirtyTree {
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

func TestVerifyFetchBaseRefreshesOriginAndWarnsWhenStale(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	devDir := filepath.Join(tmp, "dev")
	cloneDir := filepath.Join(tmp, "clone")

	runIn := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
		}
	}
	runIn(tmp, "init", "--bare", "-q", "-b", "main", origin)
	runIn(tmp, "init", "-q", "-b", "main", devDir)
	runIn(devDir, "config", "user.name", "t")
	runIn(devDir, "config", "user.email", "t@example.com")
	runIn(devDir, "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(devDir, "app.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runIn(devDir, "add", "-A")
	runIn(devDir, "commit", "-qm", "init")
	runIn(devDir, "push", "-q", "-u", "origin", "main")
	runIn(tmp, "clone", "-q", origin, cloneDir)

	g := repo.NewExecGit(cloneDir)
	oldMain, err := g.RevParse(ctx, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(cloneDir, ".git", "FETCH_HEAD")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(devDir, "next.go"), []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runIn(devDir, "add", "-A")
	runIn(devDir, "commit", "-qm", "advance main")
	runIn(devDir, "push", "-q", "origin", "main")

	cfg, err := config.Parse([]byte(`
version = 1
[[steps]]
name = "fast"
run = "true"
`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}

	var buf bytes.Buffer
	stale, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	if err != nil {
		t.Fatalf("Run without fetch: %v", err)
	}
	if stale.BaseWarning == "" {
		t.Fatalf("expected a stale base warning without FETCH_HEAD, got none")
	}
	if got, _ := g.RevParse(ctx, "origin/main"); got != oldMain {
		t.Fatalf("origin/main moved without --fetch-base: %s -> %s", oldMain, got)
	}

	fresh, err := Run(ctx, g, cfg, Options{FetchBase: true, Stdout: &buf, Stderr: &buf})
	if err != nil {
		t.Fatalf("Run with --fetch-base: %v", err)
	}
	if fresh.BaseWarning != "" {
		t.Fatalf("unexpected warning after fetch: %s", fresh.BaseWarning)
	}
	newMain, err := g.RevParse(ctx, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if newMain == oldMain {
		t.Fatalf("--fetch-base did not refresh origin/main (%s)", oldMain)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, string(out))
	}
}

func signingSetup(t *testing.T) (keyPath, allowedSigners string) {
	t.Helper()
	tmp := t.TempDir()
	keyPath = filepath.Join(tmp, "signer")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, string(out))
	}
	pub, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	allowedSigners = filepath.Join(tmp, "allowed_signers")
	if err := os.WriteFile(allowedSigners, []byte("t@example.com "+string(pub)), 0o644); err != nil {
		t.Fatal(err)
	}
	return keyPath, allowedSigners
}

func stepNames(res Result) string {
	var names []string
	for _, s := range res.Steps {
		names = append(names, s.Name+"="+s.Status)
	}
	return strings.Join(names, ",")
}

func TestVerifyRunsDependencyOnceAndExportsPassedSteps(t *testing.T) {
	t.Setenv(PassedStepsEnv, "fast")
	cases := []struct {
		name  string
		dirty bool
		steps []string
	}{
		{"clean, every step", false, nil},
		{"clean, --step verify", false, []string{"verify"}},
		{"dirty, every step", true, nil},
		{"dirty, --step verify", true, []string{"verify"}},
		{"dirty, --step fast --step verify", true, []string{"fast", "verify"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dir, g := setupRepo(t)
			log := filepath.Join(t.TempDir(), "runs.log")
			cfg, err := config.Parse([]byte(`
version = 1
[[steps]]
name = "verify"
run = 'test "$QUAI_PASSED_STEPS" = fast && echo verify >> ` + log + `'
depends_on = ["fast"]
[[steps]]
name = "fast"
run = 'test -z "$QUAI_PASSED_STEPS" && echo fast >> ` + log + `'
`))
			if err != nil {
				t.Fatal(err)
			}
			if tc.dirty {
				if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nvar dirty = 1\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var buf bytes.Buffer
			res, err := Run(ctx, g, cfg, Options{StepNames: tc.steps, Stdout: &buf, Stderr: &buf})
			if err != nil || !res.OK || stepNames(res) != "fast=passed,verify=passed" {
				t.Fatalf("res=%+v err=%v\n%s", res, err, buf.String())
			}
			if raw, _ := os.ReadFile(log); string(raw) != "fast\nverify\n" {
				t.Fatalf("each step must run once, fast first; log=%q", raw)
			}
			if tc.dirty {
				return
			}
			again, err := Run(ctx, g, cfg, Options{StepNames: tc.steps, Stdout: &buf, Stderr: &buf})
			if err != nil || stepNames(again) != "fast=cached,verify=cached" {
				t.Fatalf("rerun on the same clean tree should be cached: res=%+v err=%v", again, err)
			}
		})
	}
}

func TestVerifyStepRefusesToSignWhenDependencyFails(t *testing.T) {
	ctx := context.Background()
	_, g := setupRepo(t)
	keyPath, _ := signingSetup(t)
	log := filepath.Join(t.TempDir(), "runs.log")
	cfg, err := config.Parse([]byte(`
version = 1
[[steps]]
name = "fast"
run = "false"
[[steps]]
name = "verify"
run = "echo verify >> ` + log + `"
depends_on = ["fast"]
`))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	res, _ := Run(ctx, g, cfg, Options{StepNames: []string{"verify"}, Sign: true, SigningKey: keyPath, Stdout: &buf, Stderr: &buf})
	if res.OK || res.ReceiptWritten || len(res.Attestations) != 0 || stepNames(res) != "fast=failed" {
		t.Fatalf("a failing dependency must stop verify before it runs or signs: res=%+v", res)
	}
	if _, statErr := os.Stat(log); !os.IsNotExist(statErr) {
		t.Fatalf("verify ran although fast failed (stat err=%v)", statErr)
	}
}

func TestVerifyExcludedInputsKeepReceiptAndAttestation(t *testing.T) {
	ctx := context.Background()
	dir, g := setupRepo(t)
	keyPath, allowedSigners := signingSetup(t)
	cfg, err := config.Parse([]byte(`
version = 1
[[steps]]
name = "verify"
run = "true"
inputs = ["**", "!docs/evidence/**"]
`))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	signed := Options{Sign: true, SigningKey: keyPath, Stdout: &buf, Stderr: &buf}
	first, err := Run(ctx, g, cfg, signed)
	if err != nil || stepNames(first) != "verify=passed" || len(first.Attestations) != 1 {
		t.Fatalf("first run: res=%+v err=%v\n%s", first, err, buf.String())
	}

	if err := os.MkdirAll(filepath.Join(dir, "docs", "evidence"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "docs", "evidence", "shot.png"), []byte("png\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-qm", "evidence")

	second, err := Run(ctx, g, cfg, signed)
	if err != nil || stepNames(second) != "verify=cached" || second.Steps[0].Fingerprint != first.Steps[0].Fingerprint {
		t.Fatalf("an excluded path must keep the receipt: res=%+v err=%v", second, err)
	}
	check, err := attest.Check(ctx, g, cfg, attest.CheckOptions{AllowedSigners: allowedSigners})
	if err != nil || !check.OK || check.Steps[0].Status != "verified" {
		t.Fatalf("an excluded path must keep the attestation: res=%+v err=%v", check, err)
	}

	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nvar x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, dir, "commit", "-qam", "code")
	third, err := Run(ctx, g, cfg, signed)
	if err != nil || stepNames(third) != "verify=passed" || third.Steps[0].Fingerprint == first.Steps[0].Fingerprint {
		t.Fatalf("an included input must change the fingerprint: res=%+v err=%v", third, err)
	}
}

func TestVerifyDependencyRoutesItsOwnPool(t *testing.T) {
	ctx := context.Background()
	dir, g := setupRepo(t)
	checkoutBranch(t, dir, "feat")
	commitFile(t, dir, "README.md", "v2\n", "docs only")
	holdSlot(t, ctx, g, "heavy", 1)

	cfg, err := config.Parse([]byte(`
version = 1

[slots.heavy]
capacity = 1

[[steps]]
name = "verify"
run = 'test "$QUAI_PASSED_STEPS" = e2e'
depends_on = ["e2e"]

[[steps]]
name = "e2e"
run = 'test -z "$QUAI_PASSED_STEPS"'
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
	res, err := Run(ctx, g, cfg, Options{StepNames: []string{"verify"}, Stdout: &buf, Stderr: &buf})
	if err != nil || !res.OK || stepNames(res) != "e2e=passed,verify=passed" {
		t.Fatalf("a prerequisite pulled in by depends_on must bypass its pool on a docs-only diff: res=%+v err=%v\n%s", res, err, buf.String())
	}
	if e2e := res.Steps[0]; e2e.Pool != "heavy" || e2e.PoolAcquired == nil || *e2e.PoolAcquired {
		t.Fatalf("expected e2e annotated as bypassing heavy, got %+v", e2e)
	}
}

func TestVerifyInflightLockTimeoutKeepsOKTrue(t *testing.T) {
	ctx := context.Background()
	_, g := setupRepo(t)
	cfg, err := config.Parse([]byte(`
version = 1
[[steps]]
name = "fast"
run = "true"
[land]
lock_wait = "100ms"
`))
	if err != nil {
		t.Fatal(err)
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := g.TreeSHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	base, err := g.MergeBase(ctx, "main", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	fp, err := receipt.Fingerprint(ctx, g, cfg.Steps[0], tree, base)
	if err != nil {
		t.Fatal(err)
	}
	inflight := filepath.Join(commonDir, "quai", "locks", "inflight-fast-"+fp+".lock")
	lease, err := lock.AcquireDir(ctx, inflight, lock.Owner{Command: "blocker"}, lock.Options{Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	var buf bytes.Buffer
	res, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	var te *lock.TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected in-flight lock timeout, got res=%+v err=%v", res, err)
	}
	if !res.OK || len(res.Steps) != 0 || res.ExitKind != exitkind.LockTimeout {
		t.Fatalf("expected ok:true with lock_timeout and no step entry, got %+v", res)
	}
}

func TestVerifyClassifiesSlotLockTimeoutPerStep(t *testing.T) {
	ctx := context.Background()
	_, g := setupRepo(t)
	cfg, err := config.Parse([]byte(`
version = 1
[slots]
heavy = 1
[[steps]]
name = "queued"
run = "true"
pool = "heavy"
slots = 1
[land]
lock_wait = "100ms"
`))
	if err != nil {
		t.Fatal(err)
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := lock.AcquireSlots(ctx, filepath.Join(commonDir, "quai", "locks"), "heavy", 1, 1,
		lock.Owner{Command: "blocker"}, lock.Options{Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	var buf bytes.Buffer
	res, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	var te *lock.TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected slot lock timeout, got res=%+v err=%v", res, err)
	}
	if res.ExitKind != exitkind.LockTimeout || len(res.Steps) != 1 || res.Steps[0].ExitKind != exitkind.LockTimeout {
		t.Fatalf("expected lock_timeout top-level and per step, got res=%+v", res)
	}
}

func TestVerifyClassifiesStepExitKind(t *testing.T) {
	ctx := context.Background()
	_, g := setupRepo(t)

	cases := []struct {
		name string
		run  string
		want exitkind.Kind
	}{
		{"failed", "exit 3", exitkind.StepFailed},
		{"timeout", "sleep 5", exitkind.StepTimeout},
		{"sigkill", "kill -9 $$", exitkind.OOMKilled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := "version = 1\n[[steps]]\nname = \"s\"\nrun = \"" + tc.run + "\"\n"
			if tc.name == "timeout" {
				raw += "timeout = \"100ms\"\n"
			}
			cfg, err := config.Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			res, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
			if res.OK || len(res.Steps) != 1 {
				t.Fatalf("expected one failed step, got res=%+v err=%v", res, err)
			}
			if res.ExitKind != tc.want || res.Steps[0].ExitKind != tc.want {
				t.Fatalf("exit kinds = (%q, %q), want %q (err=%v)", res.ExitKind, res.Steps[0].ExitKind, tc.want, err)
			}
		})
	}
}
func TestVerifyRerunsExpiredReceipt(t *testing.T) {
	ctx := context.Background()
	counter := filepath.Join(t.TempDir(), "runs.log")
	_, g := setupRepo(t)
	cfg, err := config.Parse([]byte(`
version = 1
[[steps]]
name = "code"
run = "echo x >> ` + counter + `"
inputs = ["**/*.go"]
`))
	if err != nil {
		t.Fatal(err)
	}

	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	treeSHA, _ := g.TreeSHA(ctx, "HEAD")
	headSHA, _ := g.HeadSHA(ctx)
	baseSHA, _ := g.MergeBase(ctx, "main", "HEAD")
	fp, err := receipt.Fingerprint(ctx, g, cfg.Steps[0], treeSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	store := receipt.NewStore(commonDir)
	stale := receipt.Receipt{
		Version:     receipt.CurrentVersion,
		Fingerprint: fp,
		TreeSHA:     treeSHA,
		HeadSHA:     headSHA,
		BaseSHA:     baseSHA,
		Step:        "code",
		Passed:      true,
		CreatedAt:   time.Now().UTC().Add(-30 * 24 * time.Hour),
	}
	if err := os.MkdirAll(store.Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Dir, "code-"+fp+".json"), mustJSON(t, stale), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	res, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	if err != nil || !res.OK || res.Steps[0].Status != "passed" {
		t.Fatalf("expired receipt must rerun the step: res=%+v err=%v", res, err)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func queueBehindStepA(t *testing.T, ctx context.Context, g *repo.ExecGit, startedA, order string) <-chan error {
	t.Helper()
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for {
			if _, err := os.Stat(startedA); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		lease, err := lock.AcquireSlots(ctx, filepath.Join(commonDir, "quai", "locks"), "heavy", 1, 1,
			lock.Owner{Command: "competitor"}, lock.Options{Wait: 10 * time.Second, PollInterval: 20 * time.Millisecond})
		if err != nil {
			done <- err
			return
		}
		f, err := os.OpenFile(order, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err == nil {
			_, err = f.WriteString("competitor\n")
			_ = f.Close()
		}
		_ = lease.Release()
		done <- err
	}()
	return done
}

func TestVerifyKeepsTheSlotForTheNextStepOfTheSamePool(t *testing.T) {
	ctx := context.Background()
	_, g := setupRepo(t)
	scratch := t.TempDir()
	startedA := filepath.Join(scratch, "a-started")
	order := filepath.Join(scratch, "order.log")
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
version = 1
[slots]
heavy = 1
[[steps]]
name = "unit"
run = "touch %[1]s && sleep 0.5"
pool = "heavy"
slots = 1
[[steps]]
name = "e2e"
run = "echo e2e >> %[2]s"
depends_on = ["unit"]
pool = "heavy"
slots = 1
`, startedA, order)))
	if err != nil {
		t.Fatal(err)
	}
	competitor := queueBehindStepA(t, ctx, g, startedA, order)

	var buf bytes.Buffer
	res, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	if err != nil || !res.OK {
		t.Fatalf("both steps should pass: res=%+v err=%v\n%s", res, err, buf.String())
	}
	if err := <-competitor; err != nil {
		t.Fatalf("the competitor should get the slot once the run ends: %v", err)
	}
	got, err := os.ReadFile(order)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "e2e\ncompetitor\n" {
		t.Fatalf("e2e should run on the slot unit held, before the waiting competitor; order was %q", got)
	}
	if !strings.Contains(buf.String(), "pool    e2e: kept heavy from the previous step") {
		t.Fatalf("human output should say the slot was kept, got %q", buf.String())
	}
}

func TestVerifyReleasesTheSlotBeforeAStepWithoutThePool(t *testing.T) {
	ctx := context.Background()
	_, g := setupRepo(t)
	scratch := t.TempDir()
	startedA := filepath.Join(scratch, "a-started")
	order := filepath.Join(scratch, "order.log")
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
version = 1
[slots]
heavy = 1
[[steps]]
name = "unit"
run = "touch %[1]s && sleep 0.3"
pool = "heavy"
slots = 1
[[steps]]
name = "report"
run = "for i in $(seq 100); do grep -q competitor %[2]s 2>/dev/null && exit 0; sleep 0.05; done; exit 1"
depends_on = ["unit"]
`, startedA, order)))
	if err != nil {
		t.Fatal(err)
	}
	competitor := queueBehindStepA(t, ctx, g, startedA, order)

	var buf bytes.Buffer
	res, err := Run(ctx, g, cfg, Options{Stdout: &buf, Stderr: &buf})
	if err != nil || !res.OK {
		t.Fatalf("the competitor should take the slot while report runs: res=%+v err=%v\n%s", res, err, buf.String())
	}
	if err := <-competitor; err != nil {
		t.Fatal(err)
	}
}
