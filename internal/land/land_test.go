package land

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/forge"
	"github.com/coding-pr0jects/quai/internal/lock"
	"github.com/coding-pr0jects/quai/internal/repo"
	"github.com/coding-pr0jects/quai/internal/verdict"
)

func TestDecideStateMachine(t *testing.T) {
	yes := true
	no := false
	basePR := forge.PullRequest{
		Number:           42,
		State:            "OPEN",
		BaseRefName:      "main",
		HeadRefName:      "feat/x",
		HeadRefOid:       strings.Repeat("a", 40),
		MergeStateStatus: "CLEAN",
	}
	policy := config.LandPolicy{
		OptimisticMerge: &yes,
		GlobalPaths:     []string{"pnpm-lock.yaml", "packages/protocol/**"},
	}

	cases := []struct {
		name string
		in   DecisionInput
		want Action
	}{
		{
			name: "dirty tree refused",
			in:   DecisionInput{Clean: false, CurrentBranch: "feat/x", HeadSHA: basePR.HeadRefOid, PR: basePR, Policy: policy},
			want: ActionRefuse,
		},
		{
			name: "draft refused",
			in: func() DecisionInput {
				pr := basePR
				pr.IsDraft = true
				return DecisionInput{Clean: true, CurrentBranch: "feat/x", HeadSHA: pr.HeadRefOid, PR: pr, Policy: policy}
			}(),
			want: ActionRefuse,
		},
		{
			name: "fork refused",
			in: func() DecisionInput {
				pr := basePR
				pr.IsCrossRepository = true
				return DecisionInput{Clean: true, CurrentBranch: "feat/x", HeadSHA: pr.HeadRefOid, PR: pr, Policy: policy}
			}(),
			want: ActionRefuse,
		},
		{
			name: "PR branch checked out with unpushed commit refused",
			in:   DecisionInput{Clean: true, CurrentBranch: "feat/x", HeadSHA: strings.Repeat("b", 40), PR: basePR, Policy: policy},
			want: ActionRefuse,
		},
		{
			name: "coordinator on another branch ignores its own dirty tree",
			in:   DecisionInput{Clean: false, CurrentBranch: "coord", HeadSHA: basePR.HeadRefOid, PR: basePR, Policy: policy},
			want: ActionDirectMerge,
		},
		{
			name: "detached coordinator lands optimistically",
			in: DecisionInput{
				CurrentBranch: "", HeadSHA: basePR.HeadRefOid, PR: basePR,
				BaseDelta: []string{"docs/a.md"}, PRFiles: []string{"pkg/b.go"},
				MergeTree: repo.MergeTreeResult{Clean: true}, Policy: policy,
			},
			want: ActionOptimisticMerge,
		},
		{
			name: "coordinator refuses origin head GitHub does not report",
			in:   DecisionInput{Clean: true, CurrentBranch: "coord", HeadSHA: strings.Repeat("c", 40), PR: basePR, Policy: policy},
			want: ActionRefuse,
		},
		{
			name: "fork refused before any head check",
			in: func() DecisionInput {
				pr := basePR
				pr.IsCrossRepository = true
				return DecisionInput{Clean: true, CurrentBranch: "coord", HeadSHA: pr.HeadRefOid, PR: pr, Policy: policy}
			}(),
			want: ActionRefuse,
		},
		{
			name: "up to date -> direct merge",
			in:   DecisionInput{Clean: true, CurrentBranch: "feat/x", HeadSHA: basePR.HeadRefOid, PR: basePR, Policy: policy},
			want: ActionDirectMerge,
		},
		{
			name: "disjoint base move + clean merge-tree -> optimistic merge",
			in: DecisionInput{
				Clean: true, CurrentBranch: "feat/x", HeadSHA: basePR.HeadRefOid, PR: basePR,
				BaseDelta: []string{"docs/a.md"}, PRFiles: []string{"pkg/b.go"},
				MergeTree: repo.MergeTreeResult{Clean: true}, Policy: policy,
			},
			want: ActionOptimisticMerge,
		},
		{
			name: "global path on base forces rebase",
			in: DecisionInput{
				Clean: true, CurrentBranch: "feat/x", HeadSHA: basePR.HeadRefOid, PR: basePR,
				BaseDelta: []string{"packages/protocol/src/a.ts"}, PRFiles: []string{"pkg/b.go"},
				MergeTree: repo.MergeTreeResult{Clean: true}, Policy: policy,
			},
			want: ActionRebaseAndMerge,
		},
		{
			name: "overlapping file forces rebase",
			in: DecisionInput{
				Clean: true, CurrentBranch: "feat/x", HeadSHA: basePR.HeadRefOid, PR: basePR,
				BaseDelta: []string{"pkg/b.go"}, PRFiles: []string{"pkg/b.go"},
				MergeTree: repo.MergeTreeResult{Clean: true}, Policy: policy,
			},
			want: ActionRebaseAndMerge,
		},
		{
			name: "merge-tree conflict forces rebase",
			in: DecisionInput{
				Clean: true, CurrentBranch: "feat/x", HeadSHA: basePR.HeadRefOid, PR: basePR,
				BaseDelta: []string{"docs/a.md"}, PRFiles: []string{"pkg/b.go"},
				MergeTree: repo.MergeTreeResult{Clean: false, ConflictFiles: []string{"x"}}, Policy: policy,
			},
			want: ActionRebaseAndMerge,
		},
		{
			name: "GitHub BEHIND status forces rebase",
			in: func() DecisionInput {
				pr := basePR
				pr.MergeStateStatus = "BEHIND"
				return DecisionInput{
					Clean: true, CurrentBranch: "feat/x", HeadSHA: pr.HeadRefOid, PR: pr,
					BaseDelta: []string{"docs/a.md"}, PRFiles: []string{"pkg/b.go"},
					MergeTree: repo.MergeTreeResult{Clean: true}, Policy: policy,
				}
			}(),
			want: ActionRebaseAndMerge,
		},
		{
			name: "optimistic disabled forces rebase",
			in: DecisionInput{
				Clean: true, CurrentBranch: "feat/x", HeadSHA: basePR.HeadRefOid, PR: basePR,
				BaseDelta: []string{"docs/a.md"}, PRFiles: []string{"pkg/b.go"},
				MergeTree: repo.MergeTreeResult{Clean: true}, Policy: config.LandPolicy{OptimisticMerge: &no},
			},
			want: ActionRebaseAndMerge,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(tc.in)
			if got.Action != tc.want {
				t.Fatalf("Decide = %+v, want %q", got, tc.want)
			}
		})
	}
}

type fakeForge struct {
	pr        forge.PullRequest
	checks    []forge.Check
	mergedSHA string
	merged    bool
}

func (f *fakeForge) ViewPR(ctx context.Context, number int) (forge.PullRequest, error) {
	pr := f.pr
	if f.merged {
		pr.State = "MERGED"
	}
	return pr, nil
}

func (f *fakeForge) Checks(ctx context.Context, number int) ([]forge.Check, error) {
	return f.checks, nil
}

func (f *fakeForge) MergePR(ctx context.Context, number int, method config.MergeMethod, expectedHeadSHA string) error {
	f.merged = true
	f.mergedSHA = expectedHeadSHA
	return nil
}

func (f *fakeForge) ListPRsByBase(ctx context.Context, baseBranch string) ([]forge.PullRequest, error) {
	return nil, nil
}

func (f *fakeForge) EditPRBase(ctx context.Context, number int, newBase string) error {
	return nil
}

func TestRunOptimisticMergeAndConflictAbort(t *testing.T) {
	ctx := context.Background()
	origin := t.TempDir()
	work := t.TempDir()

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
		}
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run(origin, "init", "--bare", "-q", "-b", "main")
	run(work, "init", "-q", "-b", "main")
	run(work, "config", "user.name", "t")
	run(work, "config", "user.email", "t@example.com")
	run(work, "remote", "add", "origin", origin)
	write("a.txt", "a1\n")
	write("b.txt", "b1\n")
	run(work, "add", "-A")
	run(work, "commit", "-qm", "base")
	run(work, "push", "-q", "-u", "origin", "main")

	// Feature branch edits b.txt and pushes.
	run(work, "switch", "-q", "-c", "feat/disjoint")
	write("b.txt", "b2\n")
	run(work, "commit", "-qam", "feat")
	run(work, "push", "-q", "-u", "origin", "feat/disjoint")

	g := repo.NewExecGit(work)
	featSHA, _ := g.HeadSHA(ctx)

	// Main advances on disjoint a.txt and pushes.
	run(work, "switch", "-q", "main")
	write("a.txt", "a2\n")
	run(work, "commit", "-qam", "main disjoint")
	run(work, "push", "-q", "origin", "main")

	// Switch back to feat/disjoint and run land -> optimistic merge without rebasing!
	run(work, "switch", "-q", "feat/disjoint")
	cfg, _ := config.Parse([]byte(`
version = 1
[land]
optimistic_merge = true
required_checks = ["check"]
`))
	ff := &fakeForge{
		pr: forge.PullRequest{
			Number:           7,
			State:            "OPEN",
			BaseRefName:      "main",
			HeadRefName:      "feat/disjoint",
			HeadRefOid:       featSHA,
			MergeStateStatus: "CLEAN",
			URL:              "https://github.com/o/r/pull/7",
		},
		checks: []forge.Check{{Name: "check", State: "SUCCESS", Bucket: "pass"}},
	}

	var buf bytes.Buffer
	res, err := Run(ctx, g, ff, cfg, Options{
		PRNumber:   7,
		Stdout:     &buf,
		Stderr:     &buf,
		ChecksPoll: 10 * time.Millisecond,
	})
	if err != nil || !res.OK || !res.Merged || res.Action != ActionOptimisticMerge {
		t.Fatalf("expected optimistic merge, got res=%+v err=%v", res, err)
	}
	if ff.mergedSHA != featSHA {
		t.Fatalf("expected merge at original un-rebased SHA %s, got %s", featSHA, ff.mergedSHA)
	}

	// Now create a conflicting branch on a.txt -> rebase aborts cleanly and returns RebaseConflictError.
	run(work, "switch", "-q", "-c", "feat/conflict", "main~1")
	write("a.txt", "a-conflict\n")
	run(work, "commit", "-qam", "conflict")
	run(work, "push", "-q", "-u", "origin", "feat/conflict")
	cSHA, _ := g.HeadSHA(ctx)

	ff2 := &fakeForge{
		pr: forge.PullRequest{
			Number:           8,
			State:            "OPEN",
			BaseRefName:      "main",
			HeadRefName:      "feat/conflict",
			HeadRefOid:       cSHA,
			MergeStateStatus: "CONFLICTING",
		},
		checks: []forge.Check{{Name: "check", State: "SUCCESS", Bucket: "pass"}},
	}
	res2, err2 := Run(ctx, g, ff2, cfg, Options{
		PRNumber:   8,
		Stdout:     &buf,
		Stderr:     &buf,
		ChecksPoll: 10 * time.Millisecond,
	})
	var rce *repo.RebaseConflictError
	if !errors.As(err2, &rce) || res2.Merged {
		t.Fatalf("expected RebaseConflictError and no merge, got res=%+v err=%v", res2, err2)
	}
	clean, _, _ := g.IsClean(ctx)
	if !clean {
		t.Fatal("expected clean working tree after conflict abort")
	}
}

type originSyncForge struct {
	originDir string
	pr        forge.PullRequest
	checks    []forge.Check
	mergedSHA string
	merged    bool
}

func (f *originSyncForge) ViewPR(ctx context.Context, number int) (forge.PullRequest, error) {
	pr := f.pr
	if out, err := exec.Command("git", "-C", f.originDir, "rev-parse", "refs/heads/"+pr.HeadRefName).Output(); err == nil {
		pr.HeadRefOid = strings.TrimSpace(string(out))
	}
	if f.merged {
		pr.State = "MERGED"
	}
	return pr, nil
}

func (f *originSyncForge) Checks(ctx context.Context, number int) ([]forge.Check, error) {
	return f.checks, nil
}

func (f *originSyncForge) MergePR(ctx context.Context, number int, method config.MergeMethod, expectedHeadSHA string) error {
	f.merged = true
	f.mergedSHA = expectedHeadSHA
	return nil
}

func (f *originSyncForge) ListPRsByBase(ctx context.Context, baseBranch string) ([]forge.PullRequest, error) {
	return nil, nil
}

func (f *originSyncForge) EditPRBase(ctx context.Context, number int, newBase string) error {
	return nil
}

func TestRunRebaseAutoSignsAndPushesAttestation(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")
	keyPath := filepath.Join(tmp, "signer")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, string(out))
	}

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	write := func(rel, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(work, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run(tmp, "init", "--bare", "-q", "-b", "main", origin)
	run(tmp, "init", "-q", "-b", "main", work)
	run(work, "config", "user.name", "t")
	run(work, "config", "user.email", "t@example.com")
	run(work, "remote", "add", "origin", origin)
	write("a.txt", "a1\n")
	write("b.txt", "b1\n")
	run(work, "add", "-A")
	run(work, "commit", "-qm", "base")
	run(work, "push", "-q", "-u", "origin", "main")

	run(work, "switch", "-q", "-c", "feat/rebase-sign")
	write("b.txt", "b2\n")
	run(work, "commit", "-qam", "feat")
	run(work, "push", "-q", "-u", "origin", "feat/rebase-sign")

	// Main advances on a.txt; optimistic_merge = false forces rebase-and-merge.
	run(work, "switch", "-q", "main")
	write("a.txt", "a2\n")
	run(work, "commit", "-qam", "main advance")
	run(work, "push", "-q", "origin", "main")
	run(work, "switch", "-q", "feat/rebase-sign")

	slotOwner := filepath.Join(tmp, "slot-owner.json")
	cfg, err := config.Parse([]byte(`
version = 1
[slots]
heavy = 1

[[steps]]
name = "fast"
run = "cat '` + filepath.Join(work, ".git", "quai", "locks", "slot-heavy-0.lock", "owner") + `' > '` + slotOwner + `'"
pool = "heavy"
slots = 1

[land]
optimistic_merge = false
required_checks = ["check"]

[attest]
signing_key = "` + keyPath + `"
auto_push = true
`))
	if err != nil {
		t.Fatal(err)
	}

	g := repo.NewExecGit(work)
	ff := &originSyncForge{
		originDir: origin,
		pr: forge.PullRequest{
			Number:           11,
			State:            "OPEN",
			BaseRefName:      "main",
			HeadRefName:      "feat/rebase-sign",
			MergeStateStatus: "CLEAN",
			URL:              "https://github.com/o/r/pull/11",
		},
		checks: []forge.Check{{Name: "check", State: "SUCCESS", Bucket: "pass"}},
	}

	var buf bytes.Buffer
	res, err := Run(ctx, g, ff, cfg, Options{
		PRNumber:   11,
		Stdout:     &buf,
		Stderr:     &buf,
		ChecksPoll: 10 * time.Millisecond,
	})
	if err != nil || !res.OK || !res.Merged || res.Action != ActionRebaseAndMerge {
		t.Fatalf("expected rebase-and-merge to succeed, got res=%+v err=%v", res, err)
	}
	remoteRefs, err := g.ListRemoteRefs(ctx, "origin", []string{"refs/quai/records/*"})
	if err != nil || len(remoteRefs) != 1 {
		t.Fatalf("expected post-rebase attestation pushed to origin, got refs=%v err=%v", remoteRefs, err)
	}
	raw, err := os.ReadFile(slotOwner)
	if err != nil {
		t.Fatalf("step did not record its slot owner: %v", err)
	}
	var owner lock.Owner
	if err := json.Unmarshal(raw, &owner); err != nil || owner.Role != lock.RoleLand || owner.Priority != SlotPriority {
		t.Fatalf("expected post-rebase verify to hold its slot as land/%d, got %s (err=%v)", SlotPriority, raw, err)
	}
}

func TestLandEnforcesVerdictOnConfiguredTier(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")

	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(work, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	const basePolicy = `
version = 1
[tiers]
full_live = ["ui/**"]
gate_only = ["docs/**"]
[land]
required_checks = ["check"]
require_verdict_tiers = ["full-live"]
`
	const widenedPolicy = `
version = 1
[tiers]
full_live = ["ui/**"]
gate_only = ["docs/**", "lib/**", ".quai.toml"]
[land]
required_checks = ["check"]
require_verdict_tiers = ["full-live"]
`

	run(tmp, "init", "--bare", "-q", "-b", "main", origin)
	run(tmp, "init", "-q", "-b", "main", work)
	run(work, "config", "user.name", "t")
	run(work, "config", "user.email", "rev@example.com")
	run(work, "remote", "add", "origin", origin)
	write("ui/app.ts", "v1\n")
	write(".quai.toml", basePolicy)
	run(work, "add", "-A")
	run(work, "commit", "-qm", "init")
	run(work, "push", "-q", "-u", "origin", "main")

	run(work, "switch", "-q", "-c", "feat/ui-gate")
	write("ui/app.ts", "v2\n")
	run(work, "commit", "-qam", "ui change")
	run(work, "push", "-q", "-u", "origin", "feat/ui-gate")
	reviewed := run(work, "rev-parse", "HEAD")

	g := repo.NewExecGit(work)
	cfg, err := config.Parse([]byte(basePolicy))
	if err != nil {
		t.Fatal(err)
	}
	ff := &fakeForge{
		pr: forge.PullRequest{
			Number:           21,
			State:            "OPEN",
			BaseRefName:      "main",
			HeadRefName:      "feat/ui-gate",
			HeadRefOid:       reviewed,
			MergeStateStatus: "CLEAN",
		},
		checks: []forge.Check{{Name: "check", State: "SUCCESS", Bucket: "pass"}},
	}
	land := func(cfg *config.Config) Result {
		t.Helper()
		var buf bytes.Buffer
		res, err := Run(ctx, g, ff, cfg, Options{PRNumber: 21, RequireVerdict: true, Stdout: &buf, Stderr: &buf, ChecksPoll: 10 * time.Millisecond})
		if err != nil {
			t.Fatalf("land: %v\n%s", err, buf.String())
		}
		return res
	}
	pushFollowUp := func(edits map[string]string) {
		t.Helper()
		run(work, "reset", "-q", "--hard", reviewed)
		for rel, body := range edits {
			write(rel, body)
		}
		run(work, "add", "-A")
		run(work, "commit", "-qm", "follow-up")
		run(work, "push", "-q", "-f", "origin", "feat/ui-gate")
		ff.pr.HeadRefOid = run(work, "rev-parse", "HEAD")
	}

	if res := land(cfg); res.OK || res.Action != ActionRefuse || res.Merged {
		t.Fatalf("expected land refusal without verdict, got res=%+v", res)
	}
	if _, err := verdict.RecordVerdict(ctx, g, cfg, verdict.RecordOptions{Status: verdict.StatusApproved}); err != nil {
		t.Fatalf("RecordVerdict failed: %v", err)
	}

	pushFollowUp(map[string]string{".quai.toml": widenedPolicy, "lib/evil.go": "package lib\n"})
	widened, err := config.Parse([]byte(widenedPolicy))
	if err != nil {
		t.Fatal(err)
	}
	if res := land(widened); res.OK || res.Merged || res.Verdict == nil || res.Verdict.Status != "missing" {
		t.Fatalf("a branch widening its own gate_only must not keep the verdict, got res=%+v verdict=%+v", res, res.Verdict)
	}

	pushFollowUp(map[string]string{"docs/evidence/shot.png": "png\n"})
	res := land(cfg)
	if !res.OK || !res.Merged || res.Verdict == nil || !res.Verdict.OK || res.Verdict.Record.PatchID == res.Verdict.PatchID {
		t.Fatalf("a gate-only follow-up must keep the verdict under the base policy, got res=%+v verdict=%+v", res, res.Verdict)
	}
}

type stackForge struct {
	pr              forge.PullRequest
	children        []forge.PullRequest
	checks          []forge.Check
	merged          bool
	events          []string
	retargetedBases map[int]string
	onMerge         func() error
}

func (f *stackForge) ViewPR(ctx context.Context, number int) (forge.PullRequest, error) {
	pr := f.pr
	if f.merged {
		pr.State = "MERGED"
	}
	return pr, nil
}

func (f *stackForge) Checks(ctx context.Context, number int) ([]forge.Check, error) {
	return f.checks, nil
}

func (f *stackForge) MergePR(ctx context.Context, number int, method config.MergeMethod, expectedHeadSHA string) error {
	f.events = append(f.events, "merge")
	f.merged = true
	if f.onMerge != nil {
		return f.onMerge()
	}
	return nil
}

func (f *stackForge) ListPRsByBase(ctx context.Context, baseBranch string) ([]forge.PullRequest, error) {
	if baseBranch == f.pr.HeadRefName {
		return f.children, nil
	}
	return nil, nil
}

func (f *stackForge) EditPRBase(ctx context.Context, number int, newBase string) error {
	f.events = append(f.events, "retarget")
	if f.retargetedBases == nil {
		f.retargetedBases = make(map[int]string)
	}
	f.retargetedBases[number] = newBase
	return nil
}

func TestStackedPRRetargetAndRestack(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	mainDir := filepath.Join(tmp, "main")
	wtParent := filepath.Join(tmp, "wt-parent")
	wtChild := filepath.Join(tmp, "wt-child")

	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, string(out))
		}
		return strings.TrimSpace(string(out))
	}
	write := func(dir, rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run(tmp, "init", "--bare", "-q", "-b", "main", origin)
	run(tmp, "init", "-q", "-b", "main", mainDir)
	run(mainDir, "config", "user.name", "t")
	run(mainDir, "config", "user.email", "t@example.com")
	run(mainDir, "remote", "add", "origin", origin)
	write(mainDir, "base.txt", "base\n")
	run(mainDir, "add", "-A")
	run(mainDir, "commit", "-qm", "init")
	run(mainDir, "push", "-q", "-u", "origin", "main")

	// Parent branch feat/proto in wt-parent with 2 commits (simulating squash-merge difference).
	run(mainDir, "worktree", "add", "-q", "-b", "feat/proto", wtParent, "main")
	write(wtParent, "proto.txt", "v1\n")
	run(wtParent, "add", "-A")
	run(wtParent, "commit", "-qm", "proto 1")
	write(wtParent, "proto.txt", "v2\n")
	run(wtParent, "commit", "-qam", "proto 2")
	run(wtParent, "push", "-q", "-u", "origin", "feat/proto")
	parentHead := run(wtParent, "rev-parse", "HEAD")

	// Child branch feat/api stacked on top of feat/proto in wt-child.
	run(mainDir, "worktree", "add", "-q", "-b", "feat/api", wtChild, "feat/proto")
	write(wtChild, "api.txt", "api-1\n")
	run(wtChild, "add", "-A")
	run(wtChild, "commit", "-qm", "api work")
	run(wtChild, "push", "-q", "-u", "origin", "feat/api")
	childOldHead := run(wtChild, "rev-parse", "HEAD")

	sf := &stackForge{
		pr: forge.PullRequest{
			Number:           30,
			State:            "OPEN",
			BaseRefName:      "main",
			HeadRefName:      "feat/proto",
			HeadRefOid:       parentHead,
			MergeStateStatus: "CLEAN",
		},
		children: []forge.PullRequest{
			{
				Number:      31,
				State:       "OPEN",
				BaseRefName: "feat/proto",
				HeadRefName: "feat/api",
				HeadRefOid:  childOldHead,
			},
		},
		checks: []forge.Check{{Name: "check", State: "SUCCESS", Bucket: "pass"}},
		onMerge: func() error {
			// Simulate GitHub squash-merging feat/proto into origin/main as one new commit!
			write(mainDir, "proto.txt", "v2\n")
			run(mainDir, "add", "-A")
			run(mainDir, "commit", "-qm", "feat: proto (#30)")
			run(mainDir, "push", "-q", "origin", "main")
			return nil
		},
	}

	cfg, err := config.Parse([]byte(`
version = 1
[land]
required_checks = ["check"]
`))
	if err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	res, err := Run(ctx, repo.NewExecGit(wtParent), sf, cfg, Options{
		PRNumber:   30,
		Restack:    true,
		Stdout:     &buf,
		Stderr:     &buf,
		ChecksPoll: 10 * time.Millisecond,
	})
	if err != nil || !res.OK || !res.Merged {
		t.Fatalf("expected stacked land to succeed, got res=%+v err=%v", res, err)
	}
	// Verify child PR was retargeted BEFORE parent merge!
	if len(sf.events) < 2 || sf.events[0] != "retarget" || sf.events[1] != "merge" {
		t.Fatalf("expected retarget before merge, got events=%v", sf.events)
	}
	if sf.retargetedBases[31] != "main" {
		t.Fatalf("expected PR #31 retargeted to main, got %+v", sf.retargetedBases)
	}
	if len(res.StackedChildren) != 1 || res.StackedChildren[0].Status != "restacked" {
		t.Fatalf("expected 1 restacked child, got %+v", res.StackedChildren)
	}
	child := res.StackedChildren[0]
	if child.PatchID == nil || child.PatchID.Status != "SURVIVES" {
		t.Fatalf("expected child patch-id SURVIVES after squash-merge restack, got %+v", child.PatchID)
	}
	// Child branch must now have exactly 1 commit ahead of origin/main (no duplicate proto commits!).
	countAhead := run(wtChild, "rev-list", "--count", "origin/main..HEAD")
	if countAhead != "1" {
		t.Fatalf("expected child branch to have 1 commit ahead of origin/main, got %s", countAhead)
	}
	// Remote feat/api must match the new restacked head.
	remoteChildHead := run(origin, "rev-parse", "refs/heads/feat/api")
	if remoteChildHead != child.NewHeadSHA {
		t.Fatalf("remote feat/api = %s, want %s", remoteChildHead, child.NewHeadSHA)
	}
}
