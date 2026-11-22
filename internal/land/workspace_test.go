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
)

type coordinatorFixture struct {
	t         *testing.T
	tmp       string
	origin    string
	coord     string
	deleg     string
	commonDir string
	featSHA   string
	coordGit  *repo.ExecGit
	delegGit  *repo.ExecGit
	forge     *originSyncForge
}

func (fx *coordinatorFixture) git(dir string, args ...string) string {
	fx.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		fx.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (fx *coordinatorFixture) write(dir, rel, body string) {
	fx.t.Helper()
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
		fx.t.Fatal(err)
	}
}

func newCoordinatorFixture(t *testing.T, mainFile string) *coordinatorFixture {
	t.Helper()
	tmp := t.TempDir()
	fx := &coordinatorFixture{
		t:      t,
		tmp:    tmp,
		origin: filepath.Join(tmp, "origin.git"),
		coord:  filepath.Join(tmp, "coord"),
		deleg:  filepath.Join(tmp, "deleg"),
	}
	fx.git(tmp, "init", "--bare", "-q", "-b", "main", fx.origin)
	fx.git(tmp, "init", "-q", "-b", "main", fx.coord)
	fx.git(fx.coord, "config", "user.name", "t")
	fx.git(fx.coord, "config", "user.email", "t@example.com")
	fx.git(fx.coord, "remote", "add", "origin", fx.origin)
	fx.write(fx.coord, "a.txt", "a1\n")
	fx.write(fx.coord, "b.txt", "b1\n")
	fx.git(fx.coord, "add", "-A")
	fx.git(fx.coord, "commit", "-qm", "base")
	fx.git(fx.coord, "push", "-q", "-u", "origin", "main")

	fx.git(fx.coord, "branch", "feat")
	fx.git(fx.coord, "worktree", "add", "-q", fx.deleg, "feat")
	fx.write(fx.deleg, "b.txt", "b2\n")
	fx.git(fx.deleg, "commit", "-qam", "feat")
	fx.git(fx.deleg, "push", "-q", "-u", "origin", "feat")
	fx.featSHA = fx.git(fx.deleg, "rev-parse", "HEAD")

	if mainFile != "" {
		fx.write(fx.coord, mainFile, "main moved\n")
		fx.git(fx.coord, "commit", "-qam", "main advance")
		fx.git(fx.coord, "push", "-q", "origin", "main")
	}
	fx.git(fx.coord, "switch", "-q", "-c", "coord")
	fx.write(fx.coord, "scratch.txt", "coordinator work in progress\n")

	fx.coordGit = repo.NewExecGit(fx.coord)
	fx.delegGit = repo.NewExecGit(fx.deleg)
	fx.commonDir = fx.git(fx.coord, "rev-parse", "--path-format=absolute", "--git-common-dir")
	fx.forge = &originSyncForge{
		originDir: fx.origin,
		pr: forge.PullRequest{
			Number:           31,
			State:            "OPEN",
			BaseRefName:      "main",
			HeadRefName:      "feat",
			MergeStateStatus: "CLEAN",
		},
		checks: []forge.Check{{Name: "check", State: "SUCCESS", Bucket: "pass"}},
	}
	return fx
}

func (fx *coordinatorFixture) originHead(branch string) string {
	return fx.git(fx.origin, "rev-parse", "refs/heads/"+branch)
}

func (fx *coordinatorFixture) run(ctx context.Context, g repo.Git, cfgText string, opts Options) (Result, error) {
	fx.t.Helper()
	cfg, err := config.Parse([]byte(cfgText))
	if err != nil {
		fx.t.Fatal(err)
	}
	var buf bytes.Buffer
	opts.PRNumber = fx.forge.pr.Number
	opts.Stdout = &buf
	opts.Stderr = &buf
	opts.ChecksPoll = 10 * time.Millisecond
	res, err := Run(ctx, g, fx.forge, cfg, opts)
	fx.t.Logf("land output:\n%s", buf.String())
	return res, err
}

func (fx *coordinatorFixture) assertNothingLeftBehind() {
	fx.t.Helper()
	matches, _ := filepath.Glob(filepath.Join(fx.commonDir, "quai", TempWorktreePrefix+"*"))
	if len(matches) != 0 {
		fx.t.Fatalf("temporary land worktree left behind: %v", matches)
	}
	if list := fx.git(fx.coord, "worktree", "list", "--porcelain"); strings.Contains(list, TempWorktreePrefix) {
		fx.t.Fatalf("git still lists a temporary land worktree:\n%s", list)
	}
	if got := fx.git(fx.deleg, "rev-parse", "HEAD"); got != fx.featSHA {
		fx.t.Fatalf("delegation worktree moved from %s to %s", fx.featSHA, got)
	}
	if got := fx.git(fx.coord, "symbolic-ref", "--short", "HEAD"); got != "coord" {
		fx.t.Fatalf("coordinator worktree left branch coord for %q", got)
	}
}

func TestLandFromCoordinatorWorktree(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name     string
		mainFile string
		cfg      string
		want     Action
	}{
		{name: "direct merge", cfg: "version = 1\n[land]\nrequired_checks = [\"check\"]\n", want: ActionDirectMerge},
		{name: "optimistic merge", mainFile: "a.txt", cfg: "version = 1\n[land]\nrequired_checks = [\"check\"]\n", want: ActionOptimisticMerge},
		{
			name:     "rebase and merge in a temporary worktree",
			mainFile: "a.txt",
			cfg: `version = 1
[[steps]]
name = "pr-content"
run = "grep -q b2 b.txt && grep -q 'main moved' a.txt"
[land]
optimistic_merge = false
required_checks = ["check"]
`,
			want: ActionRebaseAndMerge,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCoordinatorFixture(t, tc.mainFile)
			mainTip := fx.originHead("main")

			dry, err := fx.run(ctx, fx.coordGit, tc.cfg, Options{DryRun: true})
			if err != nil || !dry.OK || dry.Action != tc.want {
				t.Fatalf("dry run = %+v, %v; want ok %s", dry, err, tc.want)
			}
			fx.assertNothingLeftBehind()

			res, err := fx.run(ctx, fx.coordGit, tc.cfg, Options{})
			if err != nil || !res.OK || !res.Merged || res.Action != tc.want {
				t.Fatalf("land = %+v, %v; want merged by %s", res, err, tc.want)
			}
			landed := fx.originHead("feat")
			if fx.forge.mergedSHA != landed || res.HeadSHA != landed {
				t.Fatalf("merged %s, reported %s, origin/feat is %s", fx.forge.mergedSHA, res.HeadSHA, landed)
			}
			if tc.want == ActionRebaseAndMerge {
				if parent := fx.git(fx.origin, "rev-parse", landed+"^"); parent != mainTip {
					t.Fatalf("rebased head %s has parent %s, want origin/main %s", landed, parent, mainTip)
				}
			} else if landed != fx.featSHA {
				t.Fatalf("%s rewrote origin/feat from %s to %s", tc.want, fx.featSHA, landed)
			}
			fx.assertNothingLeftBehind()
		})
	}
}

func TestLandRefusesStaleRemoteHeadFromCoordinator(t *testing.T) {
	fx := newCoordinatorFixture(t, "")
	fx.forge.originDir = t.TempDir()
	fx.forge.pr.HeadRefOid = strings.Repeat("e", 40)
	res, err := fx.run(context.Background(), fx.coordGit, "version = 1\n", Options{})
	if err != nil || res.OK || res.Action != ActionRefuse || !strings.Contains(res.Reason, "GitHub reports PR head") {
		t.Fatalf("land = %+v, %v; want refusal on a remote head GitHub does not report", res, err)
	}
	fx.assertNothingLeftBehind()
}

func TestLandRollsBackFailedPostRebaseVerify(t *testing.T) {
	const failingCfg = `version = 1
[[steps]]
name = "broken"
run = "false"
[land]
optimistic_merge = false
required_checks = ["check"]
`
	cases := []struct {
		name   string
		caller func(fx *coordinatorFixture) repo.Git
	}{
		{name: "temporary worktree", caller: func(fx *coordinatorFixture) repo.Git { return fx.coordGit }},
		{name: "caller on the PR branch", caller: func(fx *coordinatorFixture) repo.Git { return fx.delegGit }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newCoordinatorFixture(t, "a.txt")
			res, err := fx.run(context.Background(), tc.caller(fx), failingCfg, Options{})
			if res.OK || res.Merged || res.Failure != FailurePostRebaseVerify {
				t.Fatalf("land = %+v, %v; want failure %q", res, err, FailurePostRebaseVerify)
			}
			if !strings.Contains(res.Reason, "nothing pushed") {
				t.Fatalf("reason %q does not say the rebase was rolled back", res.Reason)
			}
			if got := fx.originHead("feat"); got != fx.featSHA {
				t.Fatalf("origin/feat moved to %s after a failed verify", got)
			}
			fx.assertNothingLeftBehind()
			if clean, dirty, _ := fx.delegGit.IsClean(context.Background()); !clean {
				t.Fatalf("delegation worktree left dirty: %v", dirty)
			}
		})
	}
}

func TestLandRemovesTemporaryWorktreeOnCancel(t *testing.T) {
	fx := newCoordinatorFixture(t, "a.txt")
	started := filepath.Join(fx.tmp, "verify-started")
	cfg := `version = 1
[[steps]]
name = "slow"
run = "touch '` + started + `' && sleep 30"
[land]
optimistic_merge = false
required_checks = ["check"]
`
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			if _, err := os.Stat(started); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	begin := time.Now()
	res, err := fx.run(ctx, fx.coordGit, cfg, Options{})
	if !errors.Is(err, context.Canceled) || res.Merged || res.Failure != FailurePostRebaseVerify {
		t.Fatalf("land = %+v, %v; want a cancelled verify", res, err)
	}
	if elapsed := time.Since(begin); elapsed > 20*time.Second {
		t.Fatalf("cancelled land took %s; the step was not killed", elapsed)
	}
	if got := fx.originHead("feat"); got != fx.featSHA {
		t.Fatalf("origin/feat moved to %s after a cancelled land", got)
	}
	fx.assertNothingLeftBehind()
	if _, err := os.Stat(filepath.Join(fx.commonDir, "quai", "locks", "land.lock")); !os.IsNotExist(err) {
		t.Fatalf("land.lock still held after cancel: %v", err)
	}
}

func TestBaseConfigReadsOriginBase(t *testing.T) {
	ctx := context.Background()
	fx := newCoordinatorFixture(t, "")
	fx.git(fx.coord, "switch", "-q", "main")
	fx.write(fx.coord, config.FileName, "version = 1\n[land]\nrequired_checks = [\"from-base\"]\n")
	fx.git(fx.coord, "add", config.FileName)
	fx.git(fx.coord, "commit", "-qm", "adopt quai")
	fx.git(fx.coord, "push", "-q", "origin", "main")
	fx.git(fx.coord, "switch", "-q", "coord")
	if _, err := os.Stat(filepath.Join(fx.coord, config.FileName)); !os.IsNotExist(err) {
		t.Fatalf("coordinator worktree should predate %s: %v", config.FileName, err)
	}
	if _, err := config.Load(fx.coord); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config.Load error %v does not wrap os.ErrNotExist", err)
	}

	cfg, err := BaseConfig(ctx, fx.coordGit, fx.forge, fx.forge.pr.Number)
	if err != nil || len(cfg.Land.RequiredChecks) != 1 || cfg.Land.RequiredChecks[0] != "from-base" {
		t.Fatalf("BaseConfig = %+v, %v; want the policy of origin/main", cfg, err)
	}
}

func TestLandChecksVerdictOfPRHeadFromCoordinator(t *testing.T) {
	ctx := context.Background()
	fx := newCoordinatorFixture(t, "")
	const cfg = `version = 1
[tiers]
full_live = ["b.txt"]
[land]
required_checks = ["check"]
require_verdict_tiers = ["full-live"]
`
	for _, dryRun := range []bool{true, false} {
		res, err := fx.run(ctx, fx.coordGit, cfg, Options{DryRun: dryRun})
		if err != nil || res.OK || res.Action != ActionRefuse || !strings.Contains(res.Reason, `tier "full-live"`) {
			t.Fatalf("dry_run=%v: land = %+v, %v; want refusal for the PR head's full-live tier", dryRun, res, err)
		}
		fx.assertNothingLeftBehind()
	}
	if fx.forge.merged {
		t.Fatal("a PR without an approved verdict was merged")
	}
}
