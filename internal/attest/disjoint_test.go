package attest

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/receipt"
	"github.com/coding-pr0jects/quai/internal/repo"
	"github.com/coding-pr0jects/quai/internal/tier"
)

func TestRefuseDisjointBase(t *testing.T) {
	safe := func() DisjointBaseInput {
		return DisjointBaseInput{
			OldBaseSHA:           "old",
			BaseSHA:              "new",
			BaseDelta:            []string{"docs/a.md"},
			HeadFiles:            []string{"src/app.go"},
			HeadToCheckout:       []string{"docs/a.md"},
			BaseToCheckout:       []string{"src/app.go"},
			MergeTree:            repo.MergeTreeResult{Clean: true},
			GlobalPaths:          []string{"go.mod", ".github/**"},
			EscalateOnTestTamper: true,
		}
	}
	tests := []struct {
		name   string
		mutate func(*DisjointBaseInput)
		want   string
	}{
		{"disjoint non-global base move", func(*DisjointBaseInput) {}, ""},
		{"base did not move", func(in *DisjointBaseInput) { in.BaseSHA = "old" }, "base has not moved"},
		{"merge-tree conflict", func(in *DisjointBaseInput) { in.MergeTree.Clean = false }, "has conflicts"},
		{"base touched global path", func(in *DisjointBaseInput) {
			in.BaseDelta = append(in.BaseDelta, ".github/workflows/ci.yml")
		}, "global path .github/workflows/ci.yml"},
		{"base and PR overlap", func(in *DisjointBaseInput) {
			in.BaseDelta = append(in.BaseDelta, "src/app.go")
		}, "both modified src/app.go"},
		{"checkout differs from head outside base delta", func(in *DisjointBaseInput) {
			in.HeadToCheckout = append(in.HeadToCheckout, "src/extra.go")
		}, "src/extra.go differs from --head"},
		{"checkout differs from base outside PR files", func(in *DisjointBaseInput) {
			in.BaseToCheckout = append(in.BaseToCheckout, "docs/a.md")
		}, "docs/a.md differs from --since"},
		{"test tamper escalates", func(in *DisjointBaseInput) {
			in.TestsTampered = []tier.TamperFinding{{Path: "src/app_test.go", Kind: "deleted"}}
		}, "test tamper (deleted src/app_test.go)"},
		{"test tamper without escalation", func(in *DisjointBaseInput) {
			in.EscalateOnTestTamper = false
			in.TestsTampered = []tier.TamperFinding{{Path: "src/app_test.go", Kind: "deleted"}}
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := safe()
			tt.mutate(&in)
			got := RefuseDisjointBase(in)
			if tt.want == "" && got != "" {
				t.Fatalf("RefuseDisjointBase = %q, want accept", got)
			}
			if !strings.Contains(got, tt.want) {
				t.Fatalf("RefuseDisjointBase = %q, want it to contain %q", got, tt.want)
			}
		})
	}
}

const disjointCfgText = `
version = 1
base = "main"

[[steps]]
name = "test"
run = "go test ./..."

[[steps]]
name = "lint"
run = "lint --since {{base}}"
inputs = ["src/**"]

[attest]
signers = ["dev@example.com"]

[land]
global_paths = ["go.mod"]
`

type disjointFixture struct {
	dir     string
	g       *repo.ExecGit
	allowed string
	oldBase string
	head    string
	newMain string
}

func appSource(first, last string) string {
	lines := []string{"package app", "func First() int { return " + first + " }"}
	for i := 0; i < 20; i++ {
		lines = append(lines, "// filler")
	}
	lines = append(lines, "func Last() int { return "+last+" }")
	return strings.Join(lines, "\n") + "\n"
}

// newDisjointFixture signs both steps for a PR head on main's old tip, lets moveMain commit on
// main, then checks out the merge of the PR head into the new main the way GitHub builds
// refs/pull/<n>/merge (first parent is the base).
func newDisjointFixture(t *testing.T, moveMain func(dir string)) disjointFixture {
	t.Helper()
	ctx := context.Background()
	tmp := t.TempDir()
	keyPath, pubKey := genSSHKey(t, tmp, "signer")
	f := disjointFixture{dir: filepath.Join(tmp, "repo"), allowed: filepath.Join(tmp, "allowed_signers")}
	writeFile(t, f.allowed, "dev@example.com "+pubKey+"\n")

	gitRun(t, tmp, "init", "-q", "-b", "main", f.dir)
	gitRun(t, f.dir, "config", "user.name", "dev")
	gitRun(t, f.dir, "config", "user.email", "dev@example.com")
	writeFile(t, filepath.Join(f.dir, ".quai.toml"), disjointCfgText)
	writeFile(t, filepath.Join(f.dir, "go.mod"), "module demo\n")
	writeFile(t, filepath.Join(f.dir, "docs/readme.md"), "# demo\n")
	writeFile(t, filepath.Join(f.dir, "src/app.go"), appSource("1", "1"))
	writeFile(t, filepath.Join(f.dir, "src/app_test.go"), "package app\nfunc TestFirst(t *testing.T) {}\n")
	gitRun(t, f.dir, "add", "-A")
	gitRun(t, f.dir, "commit", "-q", "-m", "init")
	f.oldBase = gitRun(t, f.dir, "rev-parse", "HEAD")

	gitRun(t, f.dir, "checkout", "-q", "-b", "feat")
	writeFile(t, filepath.Join(f.dir, "src/app.go"), appSource("2", "1"))
	gitRun(t, f.dir, "commit", "-q", "-am", "feature")
	f.head = gitRun(t, f.dir, "rev-parse", "HEAD")

	f.g = repo.NewExecGit(f.dir)
	cfg, err := config.Parse([]byte(disjointCfgText))
	if err != nil {
		t.Fatal(err)
	}
	headTree, _ := f.g.TreeSHA(ctx, "HEAD")
	for _, step := range cfg.Steps {
		fp, err := receipt.Fingerprint(ctx, f.g, step, headTree, f.oldBase)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := WriteRecord(ctx, f.g, receipt.Receipt{
			Version: receipt.CurrentVersion, Fingerprint: fp, TreeSHA: headTree, HeadSHA: f.head,
			BaseSHA: f.oldBase, Step: step.Name, Passed: true, CreatedAt: time.Now().UTC(),
		}, keyPath, "dev@example.com"); err != nil {
			t.Fatal(err)
		}
	}

	gitRun(t, f.dir, "checkout", "-q", "main")
	moveMain(f.dir)
	gitRun(t, f.dir, "add", "-A")
	gitRun(t, f.dir, "commit", "-q", "-m", "main moved")
	f.newMain = gitRun(t, f.dir, "rev-parse", "HEAD")
	gitRun(t, f.dir, "checkout", "-q", "--detach", f.newMain)
	gitRun(t, f.dir, "merge", "-q", "--no-ff", "-m", "merge feat", f.head)
	return f
}

func (f disjointFixture) check(t *testing.T, mutate func(*CheckOptions)) CheckResult {
	t.Helper()
	opts := CheckOptions{Head: f.head, Since: f.newMain, ConfigRef: f.newMain, AllowedSigners: f.allowed}
	if mutate != nil {
		mutate(&opts)
	}
	res, err := Check(context.Background(), f.g, nil, opts)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	return res
}

func stepByName(t *testing.T, res CheckResult, name string) StepCheck {
	t.Helper()
	for _, s := range res.Steps {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("step %q missing from %+v", name, res.Steps)
	return StepCheck{}
}

func TestCheckAcceptsDisjointBaseMove(t *testing.T) {
	f := newDisjointFixture(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "docs/readme.md"), "# demo, updated on main\n")
	})

	exact := f.check(t, func(o *CheckOptions) { o.Head = "" })
	if exact.OK || stepByName(t, exact, "test").Status != "missing" || exact.MatchMode != "" {
		t.Fatalf("without --head the moved base must miss the whole-tree step, got %+v", exact)
	}

	res := f.check(t, nil)
	if !res.OK || res.MatchMode != MatchDisjointBase || res.OldBaseSHA != f.oldBase || res.HeadSHA != f.head || res.DisjointRefusal != "" {
		t.Fatalf("expected disjoint-base acceptance, got %+v", res)
	}
	if s := stepByName(t, res, "test"); s.Status != "verified" || s.MatchMode != MatchDisjointBase || s.Signer != "dev@example.com" {
		t.Fatalf("test step = %+v, want verified by disjoint-base", s)
	}
	if s := stepByName(t, res, "lint"); s.Status != "verified" || s.MatchMode != MatchExact {
		t.Fatalf("lint step = %+v, want verified by exact match (main left src/** alone)", s)
	}

	sampled := f.check(t, func(o *CheckOptions) { o.Sample = 100; o.Roll = func() int { return 0 } })
	if sampled.OK || !sampled.Sampled || sampled.MatchMode != "" {
		t.Fatalf("--sample 100 must force a rerun, got %+v", sampled)
	}

	origin := filepath.Join(t.TempDir(), "origin.git")
	ciDir := filepath.Join(t.TempDir(), "ci")
	gitRun(t, f.dir, "init", "--bare", "-q", "-b", "main", origin)
	gitRun(t, f.dir, "push", "-q", origin, "main", "feat", RefPrefix+"*:"+RefPrefix+"*")
	gitRun(t, f.dir, "clone", "-q", origin, ciDir)
	gitRun(t, ciDir, "config", "user.name", "ci")
	gitRun(t, ciDir, "config", "user.email", "ci@example.com")
	gitRun(t, ciDir, "checkout", "-q", "--detach", f.newMain)
	gitRun(t, ciDir, "merge", "-q", "--no-ff", "-m", "merge feat", f.head)
	ci := f
	ci.g = repo.NewExecGit(ciDir)
	fetched := ci.check(t, func(o *CheckOptions) { o.Fetch = true; o.Remote = "origin" })
	if !fetched.OK || fetched.MatchMode != MatchDisjointBase {
		t.Fatalf("fresh CI clone with --fetch must accept the disjoint-base attestation, got %+v", fetched)
	}
}

func TestCheckRefusesUnsafeDisjointBase(t *testing.T) {
	tests := []struct {
		name     string
		moveMain func(t *testing.T, dir string)
		after    func(t *testing.T, dir string)
		want     string
	}{
		{"base touched a PR file", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "src/app.go"), appSource("1", "9"))
		}, nil, "both modified src/app.go"},
		{"base touched a global path", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "go.mod"), "module demo\n\ngo 1.25\n")
		}, nil, "global path go.mod"},
		{"base deleted a test", func(t *testing.T, dir string) {
			gitRun(t, dir, "rm", "-q", "src/app_test.go")
		}, nil, "test tamper (deleted src/app_test.go)"},
		{"checkout is not the merge", func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "docs/readme.md"), "# demo, updated on main\n")
		}, func(t *testing.T, dir string) {
			writeFile(t, filepath.Join(dir, "src/extra.go"), "package app\n")
			gitRun(t, dir, "add", "-A")
			gitRun(t, dir, "commit", "-q", "-m", "extra on top of the merge")
		}, "src/extra.go differs from --head"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newDisjointFixture(t, func(dir string) { tt.moveMain(t, dir) })
			if tt.after != nil {
				tt.after(t, f.dir)
			}
			res := f.check(t, nil)
			if res.OK || res.MatchMode != "" || res.OldBaseSHA != f.oldBase || !strings.Contains(res.DisjointRefusal, tt.want) {
				t.Fatalf("expected refusal containing %q, got %+v", tt.want, res)
			}
			if s := stepByName(t, res, "test"); s.Status != "missing" || s.MatchMode != "" {
				t.Fatalf("refused step must stay missing, got %+v", s)
			}
		})
	}
}

func TestCheckHeadRequiresTrustedConfigAndNoOnto(t *testing.T) {
	f := newDisjointFixture(t, func(dir string) {
		writeFile(t, filepath.Join(dir, "docs/readme.md"), "# demo, updated on main\n")
	})
	cfg, err := config.Parse([]byte(disjointCfgText))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := Check(ctx, f.g, cfg, CheckOptions{Head: f.head, Since: f.newMain, AllowedSigners: f.allowed}); err == nil || !strings.Contains(err.Error(), "--head requires --config-ref") {
		t.Fatalf("--head without --config-ref: err = %v", err)
	}
	if _, err := Check(ctx, f.g, nil, CheckOptions{Head: f.head, Onto: f.newMain, ConfigRef: f.newMain, AllowedSigners: f.allowed}); err == nil || !strings.Contains(err.Error(), "cannot be combined") {
		t.Fatalf("--head with --onto: err = %v", err)
	}
	if _, err := Check(ctx, f.g, nil, CheckOptions{Head: "no-such-ref", Since: f.newMain, ConfigRef: f.newMain, AllowedSigners: f.allowed}); err == nil || !strings.Contains(err.Error(), "resolve --head") {
		t.Fatalf("unknown --head: err = %v", err)
	}
}
