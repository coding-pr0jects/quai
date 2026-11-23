package land

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/exitkind"
	"github.com/coding-pr0jects/quai/internal/forge"
	"github.com/coding-pr0jects/quai/internal/lock"
	"github.com/coding-pr0jects/quai/internal/repo"
)

func TestBatchExitKind(t *testing.T) {
	cases := []struct {
		name    string
		entries []BatchEntry
		err     error
		want    exitkind.Kind
	}{
		{"no entries is pass", nil, nil, exitkind.Pass},
		{"all landed is pass", []BatchEntry{{Result: Result{ExitKind: exitkind.Pass}, Status: BatchLanded}}, nil, exitkind.Pass},
		{"landed status fills pass", []BatchEntry{{Status: BatchLanded}}, nil, exitkind.Pass},
		{"skipped is refused", []BatchEntry{{Status: BatchSkipped}}, nil, exitkind.Refused},
		{"refused beats pass", []BatchEntry{
			{Result: Result{ExitKind: exitkind.Pass}, Status: BatchLanded},
			{Result: Result{ExitKind: exitkind.Refused}, Status: BatchRefused},
		}, nil, exitkind.Refused},
		{"step failed beats refused", []BatchEntry{
			{Result: Result{ExitKind: exitkind.Refused}, Status: BatchRefused},
			{Result: Result{ExitKind: exitkind.StepFailed}, Status: BatchFailed},
		}, nil, exitkind.StepFailed},
		{"lock timeout beats step failed", []BatchEntry{
			{Result: Result{ExitKind: exitkind.StepFailed}, Status: BatchFailed},
			{Result: Result{ExitKind: exitkind.LockTimeout}, Status: BatchFailed},
		}, nil, exitkind.LockTimeout},
		{"run error is infra", []BatchEntry{
			{Result: Result{ExitKind: exitkind.LockTimeout}, Status: BatchFailed},
		}, errors.New("interrupted"), exitkind.InfraError},
		{"lock timeout error is lock_timeout", nil, &lock.TimeoutError{Wait: time.Second}, exitkind.LockTimeout},
		{"lock timeout beats member kinds", []BatchEntry{
			{Result: Result{ExitKind: exitkind.StepFailed}, Status: BatchFailed},
		}, &lock.TimeoutError{Wait: time.Second}, exitkind.LockTimeout},
		{"conflict status fills step_failed", []BatchEntry{{Status: BatchConflict}}, nil, exitkind.StepFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := batchExitKind(tc.entries, tc.err); got != tc.want {
				t.Fatalf("batchExitKind(%+v, %v) = %q, want %q", tc.entries, tc.err, got, tc.want)
			}
		})
	}
}

func TestRunBatchLandLockTimeoutReportsLockTimeoutAndEmptyPRs(t *testing.T) {
	ctx := context.Background()
	fx := newBatchFixture(t, []batchPR{
		{number: 301, branch: "feat/lock-wait", file: "lock-wait.txt", body: "x\n"},
	}, nil)
	writeFile(t, fx.mainDir, config.FileName, "version = 1\n[land]\nlock_wait = \"100ms\"\n")
	gitRun(t, fx.mainDir, "commit", "-qam", "short lock wait")
	gitRun(t, fx.mainDir, "push", "-q", "origin", "main")

	g := repo.NewExecGit(fx.mainDir)
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := lock.AcquireDir(ctx, filepath.Join(commonDir, "quai", "locks", "land.lock"),
		lock.Owner{Command: "held by test"}, lock.Options{Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()

	local, err := config.Parse([]byte("version = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	res, err := RunBatch(ctx, g, fx.forge, local, BatchOptions{
		PRNumbers:       []int{301},
		ContinueOnError: true,
		Options:         Options{Stdout: &buf, Stderr: &buf, ChecksPoll: 10 * time.Millisecond},
	})
	var te *lock.TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected a land.lock timeout, got res=%+v err=%v", res, err)
	}
	if res.ExitKind != exitkind.LockTimeout {
		t.Fatalf("exit_kind = %q, want lock_timeout", res.ExitKind)
	}
	if res.PRs == nil || len(res.PRs) != 0 {
		t.Fatalf("aborted batch should report an empty, non-nil prs list, got %#v", res.PRs)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"prs":[]`) {
		t.Fatalf("aborted batch JSON should carry prs: [], got %s", raw)
	}
}

func TestPlanBatch(t *testing.T) {
	cands := func(cs ...BatchCandidate) []BatchCandidate { return cs }
	c := func(pr int, base, head string, a Action) BatchCandidate {
		return BatchCandidate{PR: pr, BaseRefName: base, HeadRefName: head, Action: a}
	}
	cases := []struct {
		name string
		in   []BatchCandidate
		want []int
	}{
		{
			name: "disjoint PRs before rebases, input order kept",
			in: cands(
				c(101, "main", "a", ActionOptimisticMerge),
				c(102, "main", "b", ActionRebaseAndMerge),
				c(103, "main", "c", ActionDirectMerge),
			),
			want: []int{101, 103, 102},
		},
		{
			name: "refused PRs are left out",
			in: cands(
				c(1, "main", "a", ActionRefuse),
				c(2, "main", "b", ActionDirectMerge),
			),
			want: []int{2},
		},
		{
			name: "stacked child waits for its parent even when listed first",
			in: cands(
				c(31, "feat/proto", "feat/api", ActionDirectMerge),
				c(30, "main", "feat/proto", ActionRebaseAndMerge),
			),
			want: []int{30, 31},
		},
		{
			name: "three-level stack in reverse input order",
			in: cands(
				c(3, "b", "c", ActionRebaseAndMerge),
				c(2, "a", "b", ActionDirectMerge),
				c(1, "main", "a", ActionDirectMerge),
			),
			want: []int{1, 2, 3},
		},
		{
			name: "child of a refused parent stays planned and is skipped at run time",
			in: cands(
				c(1, "main", "a", ActionRefuse),
				c(2, "a", "b", ActionDirectMerge),
			),
			want: []int{2},
		},
		{
			name: "a cycle still yields every PR once",
			in: cands(
				c(1, "b", "a", ActionRebaseAndMerge),
				c(2, "a", "b", ActionRebaseAndMerge),
			),
			want: []int{1, 2},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PlanBatch(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("PlanBatch = %v, want %v", got, tc.want)
			}
		})
	}
}

type batchForge struct {
	t       *testing.T
	origin  string
	mainDir string
	prs     map[int]*forge.PullRequest
	red     map[int]bool
	merged  []int
	onMerge func(number int)
}

func (f *batchForge) ViewPR(ctx context.Context, number int) (forge.PullRequest, error) {
	pr, ok := f.prs[number]
	if !ok {
		return forge.PullRequest{}, errors.New("not found")
	}
	if out, err := exec.Command("git", "-C", f.origin, "rev-parse", "refs/heads/"+pr.HeadRefName).Output(); err == nil {
		pr.HeadRefOid = strings.TrimSpace(string(out))
	}
	return *pr, nil
}

func (f *batchForge) Checks(ctx context.Context, number int) ([]forge.Check, error) {
	if f.red[number] {
		return []forge.Check{{Name: "check", State: "FAILURE", Bucket: "fail"}}, nil
	}
	return []forge.Check{{Name: "check", State: "SUCCESS", Bucket: "pass"}}, nil
}

func (f *batchForge) MergePR(ctx context.Context, number int, method config.MergeMethod, expectedHeadSHA string) error {
	f.t.Helper()
	gitRun(f.t, f.mainDir, "merge", "--no-ff", "-q", "-m", "land", expectedHeadSHA)
	gitRun(f.t, f.mainDir, "push", "-q", "origin", "main")
	f.prs[number].State = "MERGED"
	f.merged = append(f.merged, number)
	if f.onMerge != nil {
		f.onMerge(number)
	}
	return nil
}

func (f *batchForge) ListPRsByBase(ctx context.Context, baseBranch string) ([]forge.PullRequest, error) {
	var out []forge.PullRequest
	for _, pr := range f.prs {
		if pr.BaseRefName == baseBranch {
			out = append(out, *pr)
		}
	}
	return out, nil
}

func (f *batchForge) EditPRBase(ctx context.Context, number int, newBase string) error {
	f.prs[number].BaseRefName = newBase
	return nil
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, rel, body string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

type batchPR struct {
	number int
	branch string
	base   string
	file   string
	body   string
}

type batchFixture struct {
	tmp     string
	mainDir string
	forge   *batchForge
	wt      map[int]string
}

const sharedBase = "1\n2\n3\n4\n5\n"

func newBatchFixture(t *testing.T, prs []batchPR, mainEdits map[string]string) *batchFixture {
	t.Helper()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	mainDir := filepath.Join(tmp, "main")
	gitRun(t, tmp, "init", "--bare", "-q", "-b", "main", origin)
	gitRun(t, tmp, "init", "-q", "-b", "main", mainDir)
	gitRun(t, mainDir, "config", "user.name", "t")
	gitRun(t, mainDir, "config", "user.email", "t@example.com")
	gitRun(t, mainDir, "remote", "add", "origin", origin)
	writeFile(t, mainDir, config.FileName, "version = 1\n[land]\nrequired_checks = [\"check\"]\n")
	writeFile(t, mainDir, "shared.txt", sharedBase)
	gitRun(t, mainDir, "add", "-A")
	gitRun(t, mainDir, "commit", "-qm", "base")
	gitRun(t, mainDir, "push", "-q", "-u", "origin", "main")

	fx := &batchFixture{tmp: tmp, mainDir: mainDir, wt: map[int]string{}}
	fx.forge = &batchForge{t: t, origin: origin, mainDir: mainDir, prs: map[int]*forge.PullRequest{}, red: map[int]bool{}}
	for _, p := range prs {
		base := p.base
		if base == "" {
			base = "main"
		}
		wt := filepath.Join(tmp, "wt-"+strings.ReplaceAll(p.branch, "/", "-"))
		gitRun(t, mainDir, "worktree", "add", "-q", "-b", p.branch, wt, base)
		writeFile(t, wt, p.file, p.body)
		gitRun(t, wt, "add", "-A")
		gitRun(t, wt, "commit", "-qm", p.branch)
		gitRun(t, wt, "push", "-q", "-u", "origin", p.branch)
		fx.wt[p.number] = wt
		fx.forge.prs[p.number] = &forge.PullRequest{
			Number: p.number, State: "OPEN", BaseRefName: base, HeadRefName: p.branch,
			MergeStateStatus: "CLEAN", URL: "https://github.com/o/r/pull/" + p.branch,
		}
	}
	if len(mainEdits) > 0 {
		for rel, body := range mainEdits {
			writeFile(t, mainDir, rel, body)
		}
		gitRun(t, mainDir, "commit", "-qam", "main moves")
		gitRun(t, mainDir, "push", "-q", "origin", "main")
	}
	return fx
}

func (fx *batchFixture) run(t *testing.T, ctx context.Context, opts BatchOptions) (BatchResult, error) {
	t.Helper()
	var buf bytes.Buffer
	opts.Stdout, opts.Stderr = &buf, &buf
	opts.ChecksPoll = 10 * time.Millisecond
	local, err := config.Parse([]byte("version = 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := RunBatch(ctx, repo.NewExecGit(fx.mainDir), fx.forge, local, opts)
	if _, statErr := os.Stat(filepath.Join(fx.mainDir, ".git", "quai", "locks", "land.lock")); !os.IsNotExist(statErr) {
		t.Fatalf("land.lock still present after RunBatch: %v", statErr)
	}
	if wts := gitRun(t, fx.mainDir, "worktree", "list", "--porcelain"); strings.Contains(wts, TempWorktreePrefix) {
		t.Fatalf("temporary land worktree left after RunBatch:\n%s", wts)
	}
	return res, err
}

type entrySummary struct {
	PR     int
	Status BatchStatus
	Action Action
}

func summarize(res BatchResult) []entrySummary {
	out := make([]entrySummary, len(res.PRs))
	for i, e := range res.PRs {
		out[i] = entrySummary{e.PR, e.Status, e.Action}
	}
	return out
}

func TestRunBatchLandsDisjointPRsBeforeRebasingOverlap(t *testing.T) {
	for _, overlapHasWorktree := range []bool{true, false} {
		t.Run(fmt.Sprintf("overlap worktree=%v", overlapHasWorktree), func(t *testing.T) {
			ctx := context.Background()
			fx := newBatchFixture(t, []batchPR{
				{number: 101, branch: "feat/a", file: "a.txt", body: "a\n"},
				{number: 102, branch: "feat/shared", file: "shared.txt", body: "1\n2\n3\n4\nfive\n"},
				{number: 103, branch: "feat/c", file: "c.txt", body: "c\n"},
			}, map[string]string{"shared.txt": "one\n2\n3\n4\n5\n"})
			if !overlapHasWorktree {
				gitRun(t, fx.mainDir, "worktree", "remove", fx.wt[102])
			}

			dry, err := fx.run(t, ctx, BatchOptions{PRNumbers: []int{101, 102, 103}, Options: Options{DryRun: true}})
			if err != nil || !dry.OK || !dry.DryRun {
				t.Fatalf("dry run: res=%+v err=%v", dry, err)
			}
			wantPlan := []entrySummary{
				{101, BatchPlanned, ActionOptimisticMerge},
				{103, BatchPlanned, ActionOptimisticMerge},
				{102, BatchPlanned, ActionRebaseAndMerge},
			}
			if got := summarize(dry); !reflect.DeepEqual(got, wantPlan) {
				t.Fatalf("dry-run plan = %+v, want %+v", got, wantPlan)
			}
			if len(fx.forge.merged) != 0 {
				t.Fatalf("dry run merged %v", fx.forge.merged)
			}

			res, err := fx.run(t, ctx, BatchOptions{PRNumbers: []int{101, 102, 103}, ContinueOnError: true})
			if err != nil || !res.OK {
				t.Fatalf("batch: res=%+v err=%v", res, err)
			}
			want := []entrySummary{
				{101, BatchLanded, ActionOptimisticMerge},
				{103, BatchLanded, ActionOptimisticMerge},
				{102, BatchLanded, ActionRebaseAndMerge},
			}
			if got := summarize(res); !reflect.DeepEqual(got, want) {
				t.Fatalf("batch = %+v, want %+v", got, want)
			}
			if !reflect.DeepEqual(fx.forge.merged, []int{101, 103, 102}) {
				t.Fatalf("merge order = %v, want [101 103 102]", fx.forge.merged)
			}
			if got := gitRun(t, fx.mainDir, "show", "origin/main:shared.txt"); got != "one\n2\n3\n4\nfive" {
				t.Fatalf("shared.txt on main = %q", got)
			}
		})
	}
}

func TestRunBatchReportsFailuresAndLandsTheRest(t *testing.T) {
	ctx := context.Background()
	prs := []batchPR{
		{number: 201, branch: "feat/red", file: "red.txt", body: "red\n"},
		{number: 202, branch: "feat/conflict", file: "shared.txt", body: "ONE\n2\n3\n4\n5\n"},
		{number: 203, branch: "feat/green", file: "green.txt", body: "green\n"},
		{number: 204, branch: "feat/gone", file: "gone.txt", body: "gone\n"},
	}
	mainEdits := map[string]string{"shared.txt": "one\n2\n3\n4\n5\n"}

	t.Run("continue on error", func(t *testing.T) {
		fx := newBatchFixture(t, prs, mainEdits)
		fx.forge.red[201] = true
		fx.forge.prs[205] = &forge.PullRequest{Number: 205, State: "OPEN", IsDraft: true, BaseRefName: "main", HeadRefName: "feat/green"}
		gitRun(t, fx.mainDir, "worktree", "remove", fx.wt[204])

		res, err := fx.run(t, ctx, BatchOptions{PRNumbers: []int{201, 202, 203, 204, 205}, ContinueOnError: true})
		if err != nil || res.OK {
			t.Fatalf("expected a failed batch without error, got res=%+v err=%v", res, err)
		}
		want := []entrySummary{
			{201, BatchFailed, ActionOptimisticMerge},
			{203, BatchLanded, ActionOptimisticMerge},
			{204, BatchLanded, ActionOptimisticMerge},
			{202, BatchConflict, ActionRebaseAndMerge},
			{205, BatchRefused, ActionRefuse},
		}
		if got := summarize(res); !reflect.DeepEqual(got, want) {
			t.Fatalf("batch = %+v, want %+v", got, want)
		}
		if !reflect.DeepEqual(fx.forge.merged, []int{203, 204}) {
			t.Fatalf("merged = %v, want [203 204]", fx.forge.merged)
		}
		if !strings.Contains(res.PRs[0].Reason, "checks failing") || len(res.PRs[3].ConflictFiles) == 0 ||
			!strings.Contains(res.PRs[4].Reason, "draft") {
			t.Fatalf("per-PR reasons missing: %+v", res.PRs)
		}
		if clean := gitRun(t, fx.wt[202], "status", "--porcelain"); clean != "" {
			t.Fatalf("conflicting worktree left dirty: %q", clean)
		}
	})

	t.Run("stop on error", func(t *testing.T) {
		fx := newBatchFixture(t, prs[:3], mainEdits)
		fx.forge.red[201] = true

		res, err := fx.run(t, ctx, BatchOptions{PRNumbers: []int{201, 202, 203}, ContinueOnError: false})
		if err != nil || res.OK {
			t.Fatalf("expected a failed batch without error, got res=%+v err=%v", res, err)
		}
		want := []entrySummary{
			{201, BatchFailed, ActionOptimisticMerge},
			{203, BatchSkipped, ActionOptimisticMerge},
			{202, BatchSkipped, ActionRebaseAndMerge},
		}
		if got := summarize(res); !reflect.DeepEqual(got, want) {
			t.Fatalf("batch = %+v, want %+v", got, want)
		}
		if len(fx.forge.merged) != 0 {
			t.Fatalf("merged = %v, want none", fx.forge.merged)
		}
	})

	t.Run("refusal upfront stops before any merge", func(t *testing.T) {
		fx := newBatchFixture(t, []batchPR{prs[2], prs[3]}, nil)
		fx.forge.prs[204].IsDraft = true

		res, err := fx.run(t, ctx, BatchOptions{PRNumbers: []int{203, 204}, ContinueOnError: false})
		if err != nil || res.OK {
			t.Fatalf("expected a failed batch without error, got res=%+v err=%v", res, err)
		}
		want := []entrySummary{{203, BatchSkipped, ActionDirectMerge}, {204, BatchRefused, ActionRefuse}}
		if got := summarize(res); !reflect.DeepEqual(got, want) {
			t.Fatalf("batch = %+v, want %+v", got, want)
		}
	})

	t.Run("child of a refused parent is skipped", func(t *testing.T) {
		fx := newBatchFixture(t, []batchPR{prs[3], {number: 205, branch: "feat/child", base: "feat/gone", file: "child.txt", body: "child\n"}}, nil)
		fx.forge.prs[204].IsDraft = true

		res, err := fx.run(t, ctx, BatchOptions{PRNumbers: []int{205, 204}, ContinueOnError: true})
		if err != nil || res.OK {
			t.Fatalf("expected a failed batch without error, got res=%+v err=%v", res, err)
		}
		want := []entrySummary{{205, BatchSkipped, ActionDirectMerge}, {204, BatchRefused, ActionRefuse}}
		if got := summarize(res); !reflect.DeepEqual(got, want) {
			t.Fatalf("batch = %+v, want %+v", got, want)
		}
		if len(fx.forge.merged) != 0 {
			t.Fatalf("merged = %v, want none", fx.forge.merged)
		}
	})

	t.Run("interrupt skips the rest and releases land.lock", func(t *testing.T) {
		fx := newBatchFixture(t, []batchPR{prs[0], prs[2]}, nil)
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		fx.forge.onMerge = func(int) { cancel() }

		res, err := fx.run(t, cctx, BatchOptions{PRNumbers: []int{201, 203}, ContinueOnError: true})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context.Canceled, got %v", err)
		}
		want := []entrySummary{{201, BatchLanded, ActionDirectMerge}, {203, BatchSkipped, ActionDirectMerge}}
		if got := summarize(res); !reflect.DeepEqual(got, want) {
			t.Fatalf("batch = %+v, want %+v", got, want)
		}
	})
}

func TestRunBatchLandsStackParentFirstAndRestacksChild(t *testing.T) {
	ctx := context.Background()
	fx := newBatchFixture(t, []batchPR{
		{number: 30, branch: "feat/proto", file: "proto.txt", body: "proto\n"},
		{number: 31, branch: "feat/api", base: "feat/proto", file: "api.txt", body: "api\n"},
	}, nil)

	res, err := fx.run(t, ctx, BatchOptions{PRNumbers: []int{31, 30}, ContinueOnError: true})
	if err != nil || !res.OK {
		t.Fatalf("batch: res=%+v err=%v", res, err)
	}
	if !reflect.DeepEqual(fx.forge.merged, []int{30, 31}) {
		t.Fatalf("merge order = %v, want [30 31]", fx.forge.merged)
	}
	parent := res.PRs[0]
	if len(parent.StackedChildren) != 1 || parent.StackedChildren[0].Status != "restacked" {
		t.Fatalf("expected child restacked between merges, got %+v", parent.StackedChildren)
	}
	if fx.forge.prs[31].BaseRefName != "main" {
		t.Fatalf("child base = %q, want main", fx.forge.prs[31].BaseRefName)
	}
	if got := gitRun(t, fx.mainDir, "show", "origin/main:api.txt"); got != "api" {
		t.Fatalf("api.txt on main = %q", got)
	}
}

func TestRunBatchReadsPolicyFromBase(t *testing.T) {
	ctx := context.Background()
	fx := newBatchFixture(t, []batchPR{{number: 1, branch: "feat/x", file: "x.txt", body: "x\n"}}, nil)
	gitRun(t, fx.mainDir, "rm", "-q", config.FileName)
	gitRun(t, fx.mainDir, "commit", "-qm", "drop policy")
	gitRun(t, fx.mainDir, "push", "-q", "origin", "main")

	_, err := fx.run(t, ctx, BatchOptions{PRNumbers: []int{1}, ContinueOnError: true})
	if !errors.Is(err, ErrBasePolicy) {
		t.Fatalf("expected ErrBasePolicy, got %v", err)
	}
}

func TestRunBatchHoldsLocksAsLand(t *testing.T) {
	ctx := context.Background()
	fx := newBatchFixture(t, []batchPR{
		{number: 1, branch: "feat/x", file: "x.txt", body: "x\n"},
		{number: 2, branch: "feat/y", file: "y.txt", body: "y\n"},
	}, nil)
	gitRun(t, fx.mainDir, "worktree", "remove", fx.wt[2])
	locks := filepath.Join(fx.mainDir, ".git", "quai", "locks")
	out := filepath.Join(fx.tmp, "owners.jsonl")
	writeFile(t, fx.mainDir, config.FileName, `version = 1
[slots]
heavy = 1
[[steps]]
name = "record-owners"
run = "cat '`+locks+`/land.lock/owner' >> '`+out+`' && echo >> '`+out+`' && cat '`+locks+`/slot-heavy-0.lock/owner' >> '`+out+`' && echo >> '`+out+`'"
pool = "heavy"
slots = 1
[land]
optimistic_merge = false
required_checks = ["check"]
`)
	gitRun(t, fx.mainDir, "commit", "-qam", "policy with a slot step")
	gitRun(t, fx.mainDir, "push", "-q", "origin", "main")

	res, err := fx.run(t, ctx, BatchOptions{PRNumbers: []int{1, 2}, ContinueOnError: true})
	if err != nil || !res.OK {
		t.Fatalf("batch: res=%+v err=%v", res, err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("post-rebase verify did not record lock owners: %v", err)
	}
	var lines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) != 4 {
		t.Fatalf("expected land.lock and slot owners for both PRs, got %d lines:\n%s", len(lines), raw)
	}
	for _, line := range lines {
		var o lock.Owner
		if err := json.Unmarshal([]byte(line), &o); err != nil || o.Role != lock.RoleLand || o.Priority != SlotPriority {
			t.Fatalf("owner = %s (err=%v), want role %q priority %d", line, err, lock.RoleLand, SlotPriority)
		}
	}
}
