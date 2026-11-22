package land

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/forge"
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
			in:   DecisionInput{Clean: false, CurrentBranch: "feat/x", LocalHeadSHA: basePR.HeadRefOid, PR: basePR, Policy: policy},
			want: ActionRefuse,
		},
		{
			name: "draft refused",
			in: func() DecisionInput {
				pr := basePR
				pr.IsDraft = true
				return DecisionInput{Clean: true, CurrentBranch: "feat/x", LocalHeadSHA: pr.HeadRefOid, PR: pr, Policy: policy}
			}(),
			want: ActionRefuse,
		},
		{
			name: "fork refused",
			in: func() DecisionInput {
				pr := basePR
				pr.IsCrossRepository = true
				return DecisionInput{Clean: true, CurrentBranch: "feat/x", LocalHeadSHA: pr.HeadRefOid, PR: pr, Policy: policy}
			}(),
			want: ActionRefuse,
		},
		{
			name: "up to date -> direct merge",
			in:   DecisionInput{Clean: true, CurrentBranch: "feat/x", LocalHeadSHA: basePR.HeadRefOid, PR: basePR, Policy: policy},
			want: ActionDirectMerge,
		},
		{
			name: "disjoint base move + clean merge-tree -> optimistic merge",
			in: DecisionInput{
				Clean: true, CurrentBranch: "feat/x", LocalHeadSHA: basePR.HeadRefOid, PR: basePR,
				BaseDelta: []string{"docs/a.md"}, PRFiles: []string{"pkg/b.go"},
				MergeTree: repo.MergeTreeResult{Clean: true}, Policy: policy,
			},
			want: ActionOptimisticMerge,
		},
		{
			name: "global path on base forces rebase",
			in: DecisionInput{
				Clean: true, CurrentBranch: "feat/x", LocalHeadSHA: basePR.HeadRefOid, PR: basePR,
				BaseDelta: []string{"packages/protocol/src/a.ts"}, PRFiles: []string{"pkg/b.go"},
				MergeTree: repo.MergeTreeResult{Clean: true}, Policy: policy,
			},
			want: ActionRebaseAndMerge,
		},
		{
			name: "overlapping file forces rebase",
			in: DecisionInput{
				Clean: true, CurrentBranch: "feat/x", LocalHeadSHA: basePR.HeadRefOid, PR: basePR,
				BaseDelta: []string{"pkg/b.go"}, PRFiles: []string{"pkg/b.go"},
				MergeTree: repo.MergeTreeResult{Clean: true}, Policy: policy,
			},
			want: ActionRebaseAndMerge,
		},
		{
			name: "merge-tree conflict forces rebase",
			in: DecisionInput{
				Clean: true, CurrentBranch: "feat/x", LocalHeadSHA: basePR.HeadRefOid, PR: basePR,
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
					Clean: true, CurrentBranch: "feat/x", LocalHeadSHA: pr.HeadRefOid, PR: pr,
					BaseDelta: []string{"docs/a.md"}, PRFiles: []string{"pkg/b.go"},
					MergeTree: repo.MergeTreeResult{Clean: true}, Policy: policy,
				}
			}(),
			want: ActionRebaseAndMerge,
		},
		{
			name: "optimistic disabled forces rebase",
			in: DecisionInput{
				Clean: true, CurrentBranch: "feat/x", LocalHeadSHA: basePR.HeadRefOid, PR: basePR,
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

	cfg, err := config.Parse([]byte(`
version = 1
[[steps]]
name = "fast"
run = "true"

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
}

func TestLandEnforcesVerdictOnConfiguredTier(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	work := filepath.Join(tmp, "work")

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
		p := filepath.Join(work, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run(tmp, "init", "--bare", "-q", "-b", "main", origin)
	run(tmp, "init", "-q", "-b", "main", work)
	run(work, "config", "user.name", "t")
	run(work, "config", "user.email", "rev@example.com")
	run(work, "remote", "add", "origin", origin)
	write("ui/app.ts", "v1\n")
	run(work, "add", "-A")
	run(work, "commit", "-qm", "init")
	run(work, "push", "-q", "-u", "origin", "main")

	run(work, "switch", "-q", "-c", "feat/ui-gate")
	write("ui/app.ts", "v2\n")
	run(work, "commit", "-qam", "ui change")
	run(work, "push", "-q", "-u", "origin", "feat/ui-gate")

	g := repo.NewExecGit(work)
	headSHA, _ := g.HeadSHA(ctx)

	cfg, err := config.Parse([]byte(`
version = 1
[tiers]
full_live = ["ui/**"]
[land]
required_checks = ["check"]
require_verdict_tiers = ["full-live"]
`))
	if err != nil {
		t.Fatal(err)
	}

	ff := &fakeForge{
		pr: forge.PullRequest{
			Number:           21,
			State:            "OPEN",
			BaseRefName:      "main",
			HeadRefName:      "feat/ui-gate",
			HeadRefOid:       headSHA,
			MergeStateStatus: "CLEAN",
		},
		checks: []forge.Check{{Name: "check", State: "SUCCESS", Bucket: "pass"}},
	}

	var buf bytes.Buffer
	// Without verdict -> refused!
	resRefused, err := Run(ctx, g, ff, cfg, Options{PRNumber: 21, Stdout: &buf, Stderr: &buf, ChecksPoll: 10 * time.Millisecond})
	if err != nil || resRefused.OK || resRefused.Action != ActionRefuse || resRefused.Merged {
		t.Fatalf("expected land refusal without verdict, got res=%+v err=%v", resRefused, err)
	}

	// Record approved verdict -> land succeeds!
	if _, err := verdict.RecordVerdict(ctx, g, cfg, verdict.RecordOptions{Status: verdict.StatusApproved}); err != nil {
		t.Fatalf("RecordVerdict failed: %v", err)
	}
	resOK, err := Run(ctx, g, ff, cfg, Options{PRNumber: 21, Stdout: &buf, Stderr: &buf, ChecksPoll: 10 * time.Millisecond})
	if err != nil || !resOK.OK || !resOK.Merged || resOK.Verdict == nil || !resOK.Verdict.OK {
		t.Fatalf("expected land to succeed with approved verdict, got res=%+v err=%v", resOK, err)
	}
}
