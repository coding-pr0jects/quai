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

type pushRepo struct {
	dir    string
	git    *repo.ExecGit
	head   string
	origin string
}

// initPushRepo commits toml as .quai.toml on a fresh main branch and returns the repository.
func initPushRepo(t *testing.T, toml string) pushRepo {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "user.name", "t")
	run("config", "user.email", "t@example.com")
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".quai.toml", toml)
	write("app/main.txt", "v1\n")
	write("README.md", "v1\n")
	run("add", "-A")
	run("commit", "-qm", "init")
	head := run("rev-parse", "HEAD")
	// The base branch must stay behind the pushed branch, as it does for a real feature branch.
	run("checkout", "-q", "-b", "feat")
	return pushRepo{dir: dir, git: repo.NewExecGit(dir), head: head}
}

func (r pushRepo) commit(t *testing.T, rel, body, message string) string {
	t.Helper()
	p := filepath.Join(r.dir, rel)
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = r.dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	run("add", "-A")
	run("commit", "-qm", message)
	return run("rev-parse", "HEAD")
}

func (r pushRepo) pushRef(t *testing.T, commit string) []PushRef {
	t.Helper()
	refs, err := ParsePushRefs(strings.NewReader("refs/heads/feat " + commit + " refs/heads/feat " + zeroSHA + "\n"))
	if err != nil {
		t.Fatalf("ParsePushRefs: %v", err)
	}
	return refs
}

const requireCachedTOML = `
version = 1
base = "main"

[[steps]]
name = "fast"
run = "true"
hook = "pre-push"

[[steps]]
name = "verify"
run = "true"
inputs = ["app/**"]

[hooks.pre_push]
require_cached = ["verify"]
`

func TestPrePushRequireCachedWarnsOnMissingReceipt(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML+"on_missing = \"warn\"\n")
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	head := pr.commit(t, "app/main.txt", "v2\n", "feat: app change")

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: pr.pushRef(t, head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("warn must allow the push: res=%+v err=%v", res, err)
	}
	if len(res.RequireCached) != 1 || res.RequireCached[0].Status != "missing" {
		t.Fatalf("require_cached = %+v, want one missing entry", res.RequireCached)
	}
	if len(res.Steps) != 1 || res.Steps[0].Name != "fast" {
		t.Fatalf("pre-push steps must still run, got %+v", res.Steps)
	}
	for _, want := range []string{"no cached receipt", `"verify"`, head[:12], "run quai verify first"} {
		if !strings.Contains(res.Warning, want) || !strings.Contains(errOut.String(), want) {
			t.Fatalf("warning %q / stderr %q missing %q", res.Warning, errOut.String(), want)
		}
	}
}

func TestPrePushRequireCachedRefusesUntilVerified(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	head := pr.commit(t, "app/main.txt", "v2\n", "feat: app change")
	refs := pr.pushRef(t, head)

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: refs, RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatalf("RunPrePush: %v", err)
	}
	if res.OK {
		t.Fatalf("refuse must block the push, got %+v", res)
	}
	if !strings.Contains(errOut.String(), "run quai verify first") {
		t.Fatalf("stderr must name the remediation, got %q", errOut.String())
	}

	var seed bytes.Buffer
	if vres, verr := verify.Run(ctx, pr.git, cfg, verify.Options{StepNames: []string{"verify"}, Stdout: &seed, Stderr: &seed}); verr != nil || !vres.OK {
		t.Fatalf("seed verify failed: %+v err=%v", vres, verr)
	}
	out.Reset()
	errOut.Reset()
	res, err = RunPrePush(ctx, pr.git, cfg, Options{Refs: refs, RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("verified head must pass: res=%+v err=%v", res, err)
	}
	if len(res.RequireCached) != 1 || res.RequireCached[0].Status != "cached" {
		t.Fatalf("require_cached = %+v, want cached", res.RequireCached)
	}
}

func TestPrePushRequireCachedSkipsStepsWithNoChangedInputs(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	head := pr.commit(t, "README.md", "v2\n", "docs only")

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: pr.pushRef(t, head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("docs-only commit must satisfy require_cached: res=%+v err=%v", res, err)
	}
	if len(res.RequireCached) != 1 || res.RequireCached[0].Status != "skipped" {
		t.Fatalf("require_cached = %+v, want skipped", res.RequireCached)
	}
}

func TestPrePushEscapeHatchStillRunsSteps(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	head := pr.commit(t, "app/main.txt", "v2\n", "feat: app change")
	t.Setenv(EnvAllowUnverifiedPush, "1")

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: pr.pushRef(t, head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("escape hatch must allow the push: res=%+v err=%v", res, err)
	}
	if !res.Bypassed {
		t.Fatalf("expected bypassed result, got %+v", res)
	}
	if len(res.Steps) != 1 || res.Steps[0].Status != "passed" {
		t.Fatalf("pre-push steps must run before the bypass, got %+v", res.Steps)
	}
	if !strings.Contains(errOut.String(), EnvAllowUnverifiedPush+"=1 bypasses") {
		t.Fatalf("bypass notice missing from stderr: %q", errOut.String())
	}
}

func TestPrePushIgnoresTagsDeletionsAndAttestationPushes(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	refs, err := ParsePushRefs(strings.NewReader(
		"refs/tags/v1 " + pr.head + " refs/tags/v1 " + zeroSHA + "\n" +
			"(delete) " + zeroSHA + " refs/heads/gone " + pr.head + "\n" +
			"refs/quai/records/fast-x " + pr.head + " refs/quai/records/fast-x " + zeroSHA + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: refs, RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("tag/deletion/attestation refs must not be gated: res=%+v err=%v", res, err)
	}
	if len(res.RequireCached) != 0 {
		t.Fatalf("require_cached checked non-branch refs: %+v", res.RequireCached)
	}

	out.Reset()
	errOut.Reset()
	res, err = RunPrePush(ctx, pr.git, cfg, Options{Refs: refs[2:], RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("attestation-only push must pass: res=%+v err=%v", res, err)
	}
	if res.Skipped != "attestation push" || len(res.Steps) != 0 {
		t.Fatalf("attestation-only push must skip steps, got %+v", res)
	}
}

func TestPrePushReportsEveryMissingStepAndRef(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	first := pr.commit(t, "app/main.txt", "v2\n", "first branch commit")
	other := pr.commit(t, "app/other.txt", "v1\n", "second branch commit")
	refs, err := ParsePushRefs(strings.NewReader(
		"refs/heads/feat " + first + " refs/heads/feat " + zeroSHA + "\n" +
			"refs/heads/other " + other + " refs/heads/other " + zeroSHA + "\n"))
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: refs, RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatalf("RunPrePush: %v", err)
	}
	if res.OK || len(res.RequireCached) != 2 {
		t.Fatalf("require_cached = %+v, want both refs checked and refused", res.RequireCached)
	}
	missing := 0
	for _, entry := range res.RequireCached {
		if entry.Status == "missing" {
			missing++
		}
	}
	if missing != 2 {
		t.Fatalf("require_cached = %+v, want both entries missing", res.RequireCached)
	}
	for _, want := range []string{"refs/heads/feat", "refs/heads/other", first[:12], other[:12]} {
		if !strings.Contains(res.Warning, want) {
			t.Fatalf("warning %q missing %q", res.Warning, want)
		}
	}
}

func TestPrePushManualInvocationChecksCurrentBranch(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	pr.commit(t, "app/main.txt", "v2\n", "feat: app change")

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatalf("RunPrePush: %v", err)
	}
	if res.OK || len(res.RequireCached) != 1 {
		t.Fatalf("manual invocation must check HEAD: %+v", res)
	}
	if res.RequireCached[0].Ref != "refs/heads/feat" {
		t.Fatalf("target ref = %q, want refs/heads/feat", res.RequireCached[0].Ref)
	}
}

func TestPrePushAutoPushesMissingAttestationRef(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	originCmd := exec.Command("git", "init", "--bare", "-q", "-b", "main", origin)
	if out, err := originCmd.CombinedOutput(); err != nil {
		t.Fatalf("init origin: %v\n%s", err, string(out))
	}
	keyPath := filepath.Join(tmp, "signer")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, string(out))
	}

	pr := initPushRepo(t, `
version = 1
base = "main"

[[steps]]
name = "fast"
run = "true"
hook = "pre-push"

[[steps]]
name = "verify"
run = "true"
inputs = ["app/**"]

[hooks.pre_push]
require_cached = ["verify"]
on_missing = "refuse"

[attest]
signing_key = "`+keyPath+`"
remote = "origin"
`)
	gitCmd := exec.Command("git", "remote", "add", "origin", origin)
	gitCmd.Dir = pr.dir
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("add remote: %v\n%s", err, string(out))
	}
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	head := pr.commit(t, "app/main.txt", "v2\n", "feat: app change")

	var seed bytes.Buffer
	vres, err := verify.Run(ctx, pr.git, cfg, verify.Options{StepNames: []string{"verify"}, Sign: true, SigningKey: keyPath, Stdout: &seed, Stderr: &seed})
	if err != nil || !vres.OK || len(vres.Attestations) != 1 {
		t.Fatalf("seed signed verify failed: %+v err=%v", vres, err)
	}
	attRef := vres.Attestations[0]
	if remoteRefs, err := pr.git.ListRemoteRefs(ctx, "origin", []string{attestationRefPrefix + "*"}); err != nil || len(remoteRefs) != 0 {
		t.Fatalf("expected no remote attestation yet, got %v (err=%v)", remoteRefs, err)
	}

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: pr.pushRef(t, head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("pre-push must pass with a cached receipt: res=%+v err=%v", res, err)
	}
	if len(res.AttestationsPushed) != 1 || res.AttestationsPushed[0] != attRef {
		t.Fatalf("attestations_pushed = %v, want [%s]", res.AttestationsPushed, attRef)
	}
	if len(res.RequireCached) != 1 || !res.RequireCached[0].AttestationPushed {
		t.Fatalf("require_cached = %+v, want attestation_pushed", res.RequireCached)
	}
	remoteRefs, err := pr.git.ListRemoteRefs(ctx, "origin", []string{attestationRefPrefix + "*"})
	if err != nil || len(remoteRefs) != 1 || remoteRefs[0] != attRef {
		t.Fatalf("remote refs = %v, want [%s] (err=%v)", remoteRefs, attRef, err)
	}

	out.Reset()
	errOut.Reset()
	res, err = RunPrePush(ctx, pr.git, cfg, Options{Refs: pr.pushRef(t, head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK || len(res.AttestationsPushed) != 0 {
		t.Fatalf("second push must not re-push the attestation: res=%+v err=%v", res, err)
	}
}

func TestRequireCachedCheckStaysUnder100ms(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	head := pr.commit(t, "app/main.txt", "v2\n", "feat: app change")
	var seed bytes.Buffer
	if vres, verr := verify.Run(ctx, pr.git, cfg, verify.Options{StepNames: []string{"verify"}, Stdout: &seed, Stderr: &seed}); verr != nil || !vres.OK {
		t.Fatalf("seed verify failed: %+v err=%v", vres, verr)
	}
	targets := BranchTargets(pr.pushRef(t, head))

	durations := make([]time.Duration, 5)
	for i := range durations {
		started := time.Now()
		report, err := CheckCached(ctx, pr.git, cfg, targets, "")
		durations[i] = time.Since(started)
		if err != nil || len(report.Missing) != 0 || len(report.Steps) != 1 || report.Steps[0].Status != "cached" {
			t.Fatalf("check %d: report=%+v err=%v", i, report, err)
		}
	}
	median := durations[len(durations)/2]
	t.Logf("require_cached check median %s over %v", median, durations)
	if median > 100*time.Millisecond {
		t.Fatalf("require_cached check took %s, want <100ms (runs: %v)", median, durations)
	}
}

func TestPrePushAttestationPushFailureDoesNotBlock(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	keyPath := filepath.Join(tmp, "signer")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, string(out))
	}
	pr := initPushRepo(t, `
version = 1
base = "main"

[[steps]]
name = "fast"
run = "true"
hook = "pre-push"

[[steps]]
name = "verify"
run = "true"
inputs = ["app/**"]

[hooks.pre_push]
require_cached = ["verify"]
on_missing = "refuse"

[attest]
signing_key = "`+keyPath+`"
remote = "origin"
`)
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	head := pr.commit(t, "app/main.txt", "v2\n", "feat: app change")
	var seed bytes.Buffer
	if vres, verr := verify.Run(ctx, pr.git, cfg, verify.Options{StepNames: []string{"verify"}, Sign: true, SigningKey: keyPath, Stdout: &seed, Stderr: &seed}); verr != nil || !vres.OK {
		t.Fatalf("seed signed verify failed: %+v err=%v", vres, verr)
	}

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: pr.pushRef(t, head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("an unreachable remote must not block the push: res=%+v err=%v", res, err)
	}
	if len(res.AttestationsPushed) != 0 || res.AttestationPushError == "" {
		t.Fatalf("expected a reported sync failure, got pushed=%v error=%q", res.AttestationsPushed, res.AttestationPushError)
	}
	if !strings.Contains(errOut.String(), "attestation push failed") {
		t.Fatalf("expected a stderr warning, got %q", errOut.String())
	}
}

func (r pushRepo) commitEmpty(t *testing.T, message string) string {
	t.Helper()
	cmd := exec.Command("git", "commit", "--allow-empty", "-qm", message)
	cmd.Dir = r.dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit --allow-empty: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = r.dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestPrePushEmptyDiffSatisfiesRequiredCache(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}

	// The branch is new but still at main, so it changes no file at all.
	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: pr.pushRef(t, pr.head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("a branch with no changes must satisfy require_cached: res=%+v err=%v", res, err)
	}
	if len(res.RequireCached) != 1 || res.RequireCached[0].Status != "skipped" {
		t.Fatalf("require_cached = %+v, want skipped", res.RequireCached)
	}

	// --allow-empty adds a commit with no file changes.
	empty := pr.commitEmpty(t, "empty commit")
	out.Reset()
	errOut.Reset()
	res, err = RunPrePush(ctx, pr.git, cfg, Options{Refs: pr.pushRef(t, empty), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("an --allow-empty commit must satisfy require_cached: res=%+v err=%v", res, err)
	}
	if len(res.RequireCached) != 1 || res.RequireCached[0].Status != "skipped" {
		t.Fatalf("require_cached = %+v, want skipped", res.RequireCached)
	}
}

func TestAttestationPushSkipsPrePushHook(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML)
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	head := pr.commit(t, "app/main.txt", "v2\n", "feat: app change")
	if vres, verr := verify.Run(ctx, pr.git, cfg, verify.Options{StepNames: []string{"verify"}, Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}); verr != nil || !vres.OK {
		t.Fatalf("seed verify failed: %+v err=%v", vres, verr)
	}
	// A stand-in for a repository hook: quai's own ref push must not run it.
	logPath := filepath.Join(t.TempDir(), "hook-runs.log")
	hookPath := filepath.Join(pr.dir, ".git", "hooks", "pre-push")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\necho run >> "+logPath+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No origin remote is configured, so the sync fails after the push attempt; the
	// assertion is that git never invoked the hook for it.
	var out, errOut bytes.Buffer
	if _, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: pr.pushRef(t, head), RefsProvided: true, Stdout: &out, Stderr: &errOut}); err != nil {
		t.Fatalf("RunPrePush: %v", err)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatalf("quai's attestation push ran the pre-push hook")
	}
}

func TestReadPushRefsFromDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	refs, provided, err := ReadPushRefs(f)
	if err != nil || !provided || len(refs) != 0 {
		t.Fatalf("devnull: refs=%v provided=%v err=%v, want provided with no refs", refs, provided, err)
	}
}

func TestPrePushEmptyStdinFallsBackToHeadWithWarning(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	pr.commit(t, "app/main.txt", "v2\n", "feat: app change")

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: nil, RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatalf("RunPrePush: %v", err)
	}
	if res.OK {
		t.Fatalf("empty stdin must not fail open under refuse: %+v", res)
	}
	for _, want := range []string{"stdin carried no refs", "refs/heads/feat", "quai hooks install", "pushed ref is unknown"} {
		if !strings.Contains(res.Warning, want) || !strings.Contains(errOut.String(), want) {
			t.Fatalf("warning %q / stderr %q missing %q", res.Warning, errOut.String(), want)
		}
	}

	var seed bytes.Buffer
	if vres, verr := verify.Run(ctx, pr.git, cfg, verify.Options{StepNames: []string{"verify"}, Stdout: &seed, Stderr: &seed}); verr != nil || !vres.OK {
		t.Fatalf("seed verify failed: %+v err=%v", vres, verr)
	}
	out.Reset()
	errOut.Reset()
	res, err = RunPrePush(ctx, pr.git, cfg, Options{Refs: nil, RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatalf("RunPrePush: %v", err)
	}
	if res.OK || !strings.Contains(res.Warning, "pushed ref is unknown") {
		t.Fatalf("refuse must block when the pushed ref is unknown even if HEAD is cached: %+v", res)
	}
	if !strings.Contains(res.Warning, "quai hooks install") {
		t.Fatalf("the refusal must name the upgrade, got %q", res.Warning)
	}
}

func TestAttestationPushSkipsHookAndPushes(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	if out, err := exec.Command("git", "init", "--bare", "-q", "-b", "main", origin).CombinedOutput(); err != nil {
		t.Fatalf("init origin: %v\n%s", err, out)
	}
	keyPath := filepath.Join(tmp, "signer")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pr := initPushRepo(t, `
version = 1
base = "main"

[[steps]]
name = "fast"
run = "true"
hook = "pre-push"

[[steps]]
name = "verify"
run = "true"
inputs = ["app/**"]

[hooks.pre_push]
require_cached = ["verify"]
on_missing = "refuse"

[attest]
signing_key = "`+keyPath+`"
remote = "origin"
`)
	gitCmd := exec.Command("git", "remote", "add", "origin", origin)
	gitCmd.Dir = pr.dir
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("add remote: %v\n%s", err, out)
	}
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	head := pr.commit(t, "app/main.txt", "v2\n", "feat: app change")
	var seed bytes.Buffer
	if vres, verr := verify.Run(ctx, pr.git, cfg, verify.Options{StepNames: []string{"verify"}, Sign: true, SigningKey: keyPath, Stdout: &seed, Stderr: &seed}); verr != nil || !vres.OK {
		t.Fatalf("seed signed verify failed: %+v err=%v", vres, verr)
	}

	logPath := filepath.Join(tmp, "hook-runs.log")
	hookPath := filepath.Join(pr.dir, ".git", "hooks", "pre-push")
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hookPath, []byte("#!/bin/sh\necho run >> "+logPath+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: pr.pushRef(t, head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK || len(res.AttestationsPushed) != 1 {
		t.Fatalf("expected one pushed attestation: res=%+v err=%v", res, err)
	}
	if _, err := os.Stat(logPath); err == nil {
		t.Fatalf("the attestation push ran the pre-push hook")
	}
	if refs, err := pr.git.ListRemoteRefs(ctx, "origin", []string{attestationRefPrefix + "*"}); err != nil || len(refs) != 1 {
		t.Fatalf("remote refs = %v, want one attestation (err=%v)", refs, err)
	}
}

func TestPrePushEmptyStdinWarnsUnderWarnPolicy(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML+"on_missing = \"warn\"\n")
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	pr.commit(t, "app/main.txt", "v2\n", "feat: app change")

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: nil, RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("warn must allow the push: res=%+v err=%v", res, err)
	}
	for _, want := range []string{"stdin carried no refs", "quai hooks install", "run quai verify first"} {
		if !strings.Contains(res.Warning, want) {
			t.Fatalf("warning %q missing %q", res.Warning, want)
		}
	}
}

func TestPrePushIgnoresStaleGuardEnv(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	head := pr.commit(t, "app/main.txt", "v2\n", "feat: app change")
	t.Setenv("QUAI_PREPUSH_ACTIVE", "1")

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: pr.pushRef(t, head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatalf("RunPrePush: %v", err)
	}
	if res.OK || len(res.RequireCached) != 1 || res.RequireCached[0].Status != "missing" {
		t.Fatalf("a stale guard variable must not skip the check: %+v", res)
	}
}

func TestPrePushUnknownRefRefusesWithCachedHead(t *testing.T) {
	ctx := context.Background()
	pr := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(pr.dir)
	if err != nil {
		t.Fatal(err)
	}
	pr.commit(t, "app/main.txt", "v2\n", "feat: app change")
	var seed bytes.Buffer
	if vres, verr := verify.Run(ctx, pr.git, cfg, verify.Options{StepNames: []string{"verify"}, Stdout: &seed, Stderr: &seed}); verr != nil || !vres.OK {
		t.Fatalf("seed verify failed: %+v err=%v", vres, verr)
	}

	// HEAD is cached, but the pushed ref is unknown, so refuse cannot know it is safe.
	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, pr.git, cfg, Options{Refs: []PushRef{}, RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil {
		t.Fatalf("RunPrePush: %v", err)
	}
	if res.OK || !strings.Contains(res.Warning, "pushed ref is unknown") {
		t.Fatalf("a cached HEAD must not cover an unknown pushed ref: %+v", res)
	}
	if res.RequireCached[0].Status != "cached" {
		t.Fatalf("expected the HEAD check to be reported, got %+v", res.RequireCached)
	}
}
