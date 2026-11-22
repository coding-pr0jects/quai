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
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-qm", message}, {"rev-parse", "HEAD"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = r.dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		if args[0] == "rev-parse" {
			return strings.TrimSpace(string(out))
		}
	}
	return ""
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
	repo := initPushRepo(t, requireCachedTOML+"on_missing = \"warn\"\n")
	cfg, err := config.Load(repo.dir)
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, repo.git, cfg, Options{Refs: repo.pushRef(t, repo.head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("warn must allow the push: res=%+v err=%v", res, err)
	}
	if len(res.RequireCached) != 1 || res.RequireCached[0].Status != "missing" {
		t.Fatalf("require_cached = %+v, want one missing entry", res.RequireCached)
	}
	if len(res.Steps) != 1 || res.Steps[0].Name != "fast" {
		t.Fatalf("pre-push steps must still run, got %+v", res.Steps)
	}
	for _, want := range []string{"no cached receipt", `"verify"`, repo.head[:12], "run quai verify first"} {
		if !strings.Contains(res.Warning, want) || !strings.Contains(errOut.String(), want) {
			t.Fatalf("warning %q / stderr %q missing %q", res.Warning, errOut.String(), want)
		}
	}
}

func TestPrePushRequireCachedRefusesUntilVerified(t *testing.T) {
	ctx := context.Background()
	repo := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	refs := repo.pushRef(t, repo.head)

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, repo.git, cfg, Options{Refs: refs, RefsProvided: true, Stdout: &out, Stderr: &errOut})
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
	if vres, verr := verify.Run(ctx, repo.git, cfg, verify.Options{StepNames: []string{"verify"}, Stdout: &seed, Stderr: &seed}); verr != nil || !vres.OK {
		t.Fatalf("seed verify failed: %+v err=%v", vres, verr)
	}
	out.Reset()
	errOut.Reset()
	res, err = RunPrePush(ctx, repo.git, cfg, Options{Refs: refs, RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("verified head must pass: res=%+v err=%v", res, err)
	}
	if len(res.RequireCached) != 1 || res.RequireCached[0].Status != "cached" {
		t.Fatalf("require_cached = %+v, want cached", res.RequireCached)
	}
}

func TestPrePushRequireCachedSkipsStepsWithNoChangedInputs(t *testing.T) {
	ctx := context.Background()
	repo := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	head := repo.commit(t, "README.md", "v2\n", "docs only")

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, repo.git, cfg, Options{Refs: repo.pushRef(t, head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("docs-only commit must satisfy require_cached: res=%+v err=%v", res, err)
	}
	if len(res.RequireCached) != 1 || res.RequireCached[0].Status != "skipped" {
		t.Fatalf("require_cached = %+v, want skipped", res.RequireCached)
	}
}

func TestPrePushEscapeHatchStillRunsSteps(t *testing.T) {
	ctx := context.Background()
	repo := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvAllowUnverifiedPush, "1")

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, repo.git, cfg, Options{Refs: repo.pushRef(t, repo.head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
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
	repo := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(repo.dir)
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	refs, err := ParsePushRefs(strings.NewReader(
		"refs/tags/v1 " + repo.head + " refs/tags/v1 " + zeroSHA + "\n" +
			"(delete) " + zeroSHA + " refs/heads/gone " + repo.head + "\n" +
			"refs/quai/records/fast-x " + repo.head + " refs/quai/records/fast-x " + zeroSHA + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := RunPrePush(ctx, repo.git, cfg, Options{Refs: refs, RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("tag/deletion/attestation refs must not be gated: res=%+v err=%v", res, err)
	}
	if len(res.RequireCached) != 0 {
		t.Fatalf("require_cached checked non-branch refs: %+v", res.RequireCached)
	}

	out.Reset()
	errOut.Reset()
	res, err = RunPrePush(ctx, repo.git, cfg, Options{Refs: refs[2:], RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("attestation-only push must pass: res=%+v err=%v", res, err)
	}
	if res.Skipped != "attestation push" || len(res.Steps) != 0 {
		t.Fatalf("attestation-only push must skip steps, got %+v", res)
	}
}

func TestPrePushReportsEveryMissingStepAndRef(t *testing.T) {
	ctx := context.Background()
	repo := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	other := repo.commit(t, "app/main.txt", "v2\n", "second branch commit")
	refs, err := ParsePushRefs(strings.NewReader(
		"refs/heads/feat " + repo.head + " refs/heads/feat " + zeroSHA + "\n" +
			"refs/heads/other " + other + " refs/heads/other " + repo.head + "\n"))
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, repo.git, cfg, Options{Refs: refs, RefsProvided: true, Stdout: &out, Stderr: &errOut})
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
	for _, want := range []string{"refs/heads/feat", "refs/heads/other", repo.head[:12], other[:12]} {
		if !strings.Contains(res.Warning, want) {
			t.Fatalf("warning %q missing %q", res.Warning, want)
		}
	}
}

func TestPrePushManualInvocationChecksCurrentBranch(t *testing.T) {
	ctx := context.Background()
	repo := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(repo.dir)
	if err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, repo.git, cfg, Options{Stdout: &out, Stderr: &errOut})
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

	repo := initPushRepo(t, `
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
	gitCmd.Dir = repo.dir
	if out, err := gitCmd.CombinedOutput(); err != nil {
		t.Fatalf("add remote: %v\n%s", err, string(out))
	}
	cfg, err := config.Load(repo.dir)
	if err != nil {
		t.Fatal(err)
	}

	var seed bytes.Buffer
	vres, err := verify.Run(ctx, repo.git, cfg, verify.Options{StepNames: []string{"verify"}, Sign: true, SigningKey: keyPath, Stdout: &seed, Stderr: &seed})
	if err != nil || !vres.OK || len(vres.Attestations) != 1 {
		t.Fatalf("seed signed verify failed: %+v err=%v", vres, err)
	}
	attRef := vres.Attestations[0]
	if remoteRefs, err := repo.git.ListRemoteRefs(ctx, "origin", []string{attestationRefPrefix + "*"}); err != nil || len(remoteRefs) != 0 {
		t.Fatalf("expected no remote attestation yet, got %v (err=%v)", remoteRefs, err)
	}

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, repo.git, cfg, Options{Refs: repo.pushRef(t, repo.head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK {
		t.Fatalf("pre-push must pass with a cached receipt: res=%+v err=%v", res, err)
	}
	if len(res.AttestationsPushed) != 1 || res.AttestationsPushed[0] != attRef {
		t.Fatalf("attestations_pushed = %v, want [%s]", res.AttestationsPushed, attRef)
	}
	if len(res.RequireCached) != 1 || !res.RequireCached[0].AttestationPushed {
		t.Fatalf("require_cached = %+v, want attestation_pushed", res.RequireCached)
	}
	remoteRefs, err := repo.git.ListRemoteRefs(ctx, "origin", []string{attestationRefPrefix + "*"})
	if err != nil || len(remoteRefs) != 1 || remoteRefs[0] != attRef {
		t.Fatalf("remote refs = %v, want [%s] (err=%v)", remoteRefs, attRef, err)
	}

	out.Reset()
	errOut.Reset()
	res, err = RunPrePush(ctx, repo.git, cfg, Options{Refs: repo.pushRef(t, repo.head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
	if err != nil || !res.OK || len(res.AttestationsPushed) != 0 {
		t.Fatalf("second push must not re-push the attestation: res=%+v err=%v", res, err)
	}
}

func TestRequireCachedCheckStaysUnder100ms(t *testing.T) {
	ctx := context.Background()
	repo := initPushRepo(t, requireCachedTOML+"on_missing = \"refuse\"\n")
	cfg, err := config.Load(repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	var seed bytes.Buffer
	if vres, verr := verify.Run(ctx, repo.git, cfg, verify.Options{StepNames: []string{"verify"}, Stdout: &seed, Stderr: &seed}); verr != nil || !vres.OK {
		t.Fatalf("seed verify failed: %+v err=%v", vres, verr)
	}
	targets := BranchTargets(repo.pushRef(t, repo.head))

	durations := make([]time.Duration, 5)
	for i := range durations {
		started := time.Now()
		report, err := CheckCached(ctx, repo.git, cfg, targets, "")
		durations[i] = time.Since(started)
		if err != nil || len(report.Missing) != 0 || len(report.Steps) != 1 || report.Steps[0].Status != "cached" {
			t.Fatalf("check %d: report=%+v err=%v", i, report, err)
		}
	}
	median := durations[len(durations)/2]
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
	repo := initPushRepo(t, `
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
	cfg, err := config.Load(repo.dir)
	if err != nil {
		t.Fatal(err)
	}
	var seed bytes.Buffer
	if vres, verr := verify.Run(ctx, repo.git, cfg, verify.Options{StepNames: []string{"verify"}, Sign: true, SigningKey: keyPath, Stdout: &seed, Stderr: &seed}); verr != nil || !vres.OK {
		t.Fatalf("seed signed verify failed: %+v err=%v", vres, verr)
	}

	var out, errOut bytes.Buffer
	res, err := RunPrePush(ctx, repo.git, cfg, Options{Refs: repo.pushRef(t, repo.head), RefsProvided: true, Stdout: &out, Stderr: &errOut})
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
