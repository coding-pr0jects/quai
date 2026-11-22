package status

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/land"
	"github.com/coding-pr0jects/quai/internal/lock"
	"github.com/coding-pr0jects/quai/internal/repo"
)

func TestParseGitVersion(t *testing.T) {
	cases := []struct {
		in       string
		maj, min int
		ok       bool
	}{
		{"git version 2.43.0\n", 2, 43, true},
		{"git version 2.38.1.windows.1", 2, 38, true},
		{"git version 3.0.0", 3, 0, true},
		{"nope", 0, 0, false},
	}
	for _, tc := range cases {
		maj, min, ok := ParseGitVersion(tc.in)
		if ok != tc.ok || maj != tc.maj || min != tc.min {
			t.Fatalf("ParseGitVersion(%q) = (%d,%d,%v), want (%d,%d,%v)", tc.in, maj, min, ok, tc.maj, tc.min, tc.ok)
		}
	}
}

func TestInspectAndDoctor(t *testing.T) {
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
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.name", "t")
	runGit("config", "user.email", "t@example.com")
	if err := os.WriteFile(filepath.Join(dir, ".quai.toml"), []byte(`
version = 1
base = "main"

[slots]
heavy = 2

[[steps]]
name = "fast"
run = "true"
hook = "pre-push"
pool = "heavy"
slots = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-qm", "init")

	g := repo.NewExecGit(dir)
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	locksRoot := filepath.Join(commonDir, "quai", "locks")

	// Hold one slot and land.lock, then inspect.
	landLease, err := lock.AcquireDir(ctx, filepath.Join(locksRoot, "land.lock"), lock.Owner{Command: "quai land 99", Worktree: dir}, lock.Options{Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer landLease.Release()

	slotLease, err := lock.AcquireSlots(ctx, locksRoot, "heavy", 2, 2, lock.Owner{Role: lock.RoleVerify, Command: "quai verify", Worktree: dir}, lock.Options{Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer slotLease.Release()

	waitCtx, stopWaiters := context.WithCancel(ctx)
	var waiters sync.WaitGroup
	defer waiters.Wait()
	defer stopWaiters()
	queueDir := filepath.Join(locksRoot, "slot-heavy.queue")
	for _, w := range []lock.Owner{
		{Role: lock.RoleVerify, Command: "quai verify --step fast"},
		{Role: lock.RoleLand, Priority: 10, Command: "quai land 99"},
	} {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			_, _ = lock.AcquireSlots(waitCtx, locksRoot, "heavy", 1, 2, w, lock.Options{PollInterval: 10 * time.Millisecond})
		}()
		want := len(lock.InspectWaiters(queueDir, 0)) + 1
		for deadline := time.Now().Add(5 * time.Second); len(lock.InspectWaiters(queueDir, 0)) < want; {
			if time.Now().After(deadline) {
				t.Fatalf("waiter %q never queued", w.Command)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	rep, err := Inspect(ctx, g, cfg)
	if err != nil {
		t.Fatalf("Inspect failed: %v", err)
	}
	if !rep.OK || !rep.LandLock.Held || !rep.LandLock.Alive {
		t.Fatalf("expected live land.lock, got %+v", rep.LandLock)
	}
	if len(rep.Pools) != 1 || rep.Pools[0].Name != "heavy" || rep.Pools[0].Held != 2 || rep.Pools[0].Capacity != 2 {
		t.Fatalf("unexpected pool status: %+v", rep.Pools)
	}
	for _, sl := range rep.Pools[0].Slots {
		if sl.Owner == nil || sl.Owner.Role != lock.RoleVerify || sl.Owner.Priority != 0 {
			t.Fatalf("expected verify holders at priority 0, got %+v", sl)
		}
	}
	if w := rep.Pools[0].Waiters; len(w) != 2 || w[0].Role != lock.RoleLand || w[0].Priority != 10 || w[1].Role != lock.RoleVerify || w[1].Priority != 0 {
		t.Fatalf("expected waiters [land/10 verify/0] in service order, got %+v", w)
	}

	leftover := filepath.Join(commonDir, "quai", land.TempWorktreePrefix+"7-4242")
	if err := os.MkdirAll(leftover, 0o755); err != nil {
		t.Fatal(err)
	}
	rep, err = Inspect(ctx, g, cfg)
	if err != nil || len(rep.LandWorktrees) != 1 || rep.LandWorktrees[0] != leftover {
		t.Fatalf("expected land worktree %s in status, got %v (err %v)", leftover, rep.LandWorktrees, err)
	}

	doc := Doctor(ctx, g)
	if !doc.OK || hasCheck(doc, "land-worktrees") {
		t.Fatalf("expected healthy doctor report while quai land holds land.lock, got %+v", doc)
	}
	landLease.Release()
	doc = Doctor(ctx, g)
	if !doc.OK || !hasCheck(doc, "land-worktrees") {
		t.Fatalf("expected a land-worktrees warning once land.lock is free, got %+v", doc)
	}
}

func hasCheck(rep DoctorReport, name string) bool {
	for _, c := range rep.Checks {
		if c.Name == name {
			return true
		}
	}
	return false
}

func TestDoctorWarnsWhenOriginHeadUnsetWithBaseAutoPrune(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	dir := filepath.Join(tmp, "repo")
	run := func(workdir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = workdir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	run(tmp, "init", "--bare", "-q", "-b", "main", origin)
	run(tmp, "init", "-q", "-b", "main", dir)
	run(dir, "config", "user.name", "t")
	run(dir, "config", "user.email", "t@example.com")
	run(dir, "remote", "add", "origin", origin)
	cfgText := `
version = 1
base = "main"

[[steps]]
name = "fast"
run = "true"

[attest]
auto_prune = true
`
	if err := os.WriteFile(filepath.Join(dir, ".quai.toml"), []byte(cfgText), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(dir, "add", "-A")
	run(dir, "commit", "-qm", "init")
	run(dir, "push", "-q", "-u", "origin", "main")

	g := repo.NewExecGit(dir)
	check := func(rep DoctorReport) CheckItem {
		t.Helper()
		for _, c := range rep.Checks {
			if c.Name == "auto-prune" {
				return c
			}
		}
		t.Fatalf("doctor has no auto-prune check: %+v", rep.Checks)
		return CheckItem{}
	}

	got := check(Doctor(ctx, g))
	if got.Status != "warn" || !strings.Contains(got.Message, "refs/remotes/origin/HEAD is unset") || !strings.Contains(got.Fix, "git remote set-head origin --auto") {
		t.Fatalf("unset origin/HEAD check = %+v, want warn with fix", got)
	}

	run(dir, "remote", "set-head", "origin", "main")
	if got := check(Doctor(ctx, g)); got.Status != "ok" {
		t.Fatalf("set origin/HEAD check = %+v, want ok", got)
	}
}

func findCheck(rep DoctorReport, name string) *CheckItem {
	for i := range rep.Checks {
		if rep.Checks[i].Name == name {
			return &rep.Checks[i]
		}
	}
	return nil
}

func isolateGitConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	for _, key := range []string{"QUAI_SIGNING_KEY", "QUAI_ALLOWED_SIGNERS", "QUAI_SIGN", "QUAI_PUSH"} {
		t.Setenv(key, "")
	}
}

func TestDoctorFlagsMissingAttestSigningKey(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	isolateGitConfig(t)
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
	if err := os.WriteFile(filepath.Join(dir, ".quai.toml"), []byte(`
version = 1
base = "main"

[[steps]]
name = "fast"
run = "true"

[attest]
auto_sign = true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit("add", "-A")
	runGit("commit", "-qm", "init")

	rep := Doctor(ctx, repo.NewExecGit(dir))
	item := findCheck(rep, "attest-signing-key")
	if item == nil {
		t.Fatalf("doctor did not check attest-signing-key: %+v", rep.Checks)
	}
	if item.Status != "fail" {
		t.Fatalf("attest-signing-key status = %q, want fail (%s)", item.Status, item.Message)
	}
	if rep.OK {
		t.Fatal("doctor reported healthy despite missing attest signing key")
	}
}

func TestDoctorReportsAttestReadiness(t *testing.T) {
	ctx := context.Background()
	isolateGitConfig(t)
	tmp := t.TempDir()
	keyPath := filepath.Join(tmp, "signer")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, string(out))
	}
	pubRaw, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}

	origin := filepath.Join(tmp, "origin.git")
	dir := filepath.Join(tmp, "repo")
	runGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	runGit("init", "--bare", "-q", "-b", "main", origin)
	runGit("init", "-q", "-b", "main", dir)
	repoGit := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	repoGit("config", "user.name", "t")
	repoGit("config", "user.email", "t@example.com")
	repoGit("remote", "add", "origin", origin)
	if err := os.MkdirAll(filepath.Join(dir, ".quai"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".quai", "allowed_signers"), []byte("t@example.com "+strings.TrimSpace(string(pubRaw))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".quai.toml"), []byte(`
version = 1
base = "main"

[[steps]]
name = "fast"
run = "true"

[attest]
allowed_signers = ".quai/allowed_signers"
signing_key = "`+keyPath+`"
auto_push = true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	repoGit("add", "-A")
	repoGit("commit", "-qm", "init")

	rep := Doctor(ctx, repo.NewExecGit(dir))
	for _, name := range []string{"attest-config", "attest-signing-key", "attest-agent-key", "attest-allowed-signers", "attest-remote"} {
		item := findCheck(rep, name)
		if item == nil {
			t.Fatalf("doctor did not check %s: %+v", name, rep.Checks)
		}
		if item.Status == "fail" {
			t.Fatalf("%s failed: %s", name, item.Message)
		}
	}
	remote := findCheck(rep, "attest-remote")
	if remote.Status != "ok" {
		t.Fatalf("attest-remote status = %q, want ok (%s)", remote.Status, remote.Message)
	}
	if !rep.OK {
		t.Fatalf("expected healthy doctor report, got %+v", rep.Checks)
	}
}

func TestCheckAgentKeyWithoutAgent(t *testing.T) {
	if _, err := exec.LookPath("ssh-add"); err != nil {
		t.Skip("ssh-add not installed")
	}
	t.Setenv("SSH_AUTH_SOCK", filepath.Join(t.TempDir(), "missing-agent.sock"))
	item := checkAgentKey(context.Background(), "/nonexistent/signing-key")
	if item.Name != "attest-agent-key" {
		t.Fatalf("check name = %q", item.Name)
	}
	if item.Status != "warn" {
		t.Fatalf("status = %q, want warn (%s)", item.Status, item.Message)
	}
	if !strings.Contains(item.Message, "ssh-agent") {
		t.Fatalf("message %q does not mention ssh-agent", item.Message)
	}
	if item.Fix == "" {
		t.Fatal("expected a fix hint")
	}
}
