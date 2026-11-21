package tier

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/smbss1/quai/internal/config"
	"github.com/smbss1/quai/internal/repo"
)

func TestClassifyPrecedenceAndPureRenames(t *testing.T) {
	yes := true
	policy := config.TierPolicy{
		Default:              config.TierDiffAudit,
		FullLive:             []string{"apps/desktop/src/renderer/**", "packages/protocol/src/**"},
		GateOnly:             []string{"docs/**", "*.md"},
		PureRenamesGateOnly:  &yes,
		EscalateOnTestTamper: &yes,
	}

	// Empty diff -> gate-only.
	if d := Classify(nil, nil, nil, policy); d.Tier != config.TierGateOnly {
		t.Fatalf("empty diff = %q, want gate-only", d.Tier)
	}

	// Pure rename in renderer + docs edit -> gate-only.
	changes := []repo.FileChange{
		{Status: "R100", OldPath: "apps/desktop/src/renderer/a.ts", Path: "apps/desktop/src/renderer/b.ts"},
		{Status: "M", Path: "docs/guide.md"},
	}
	d := Classify(changes, nil, nil, policy)
	if d.Tier != config.TierGateOnly || d.PureRenames != 1 {
		t.Fatalf("pure rename should stay gate-only, got %+v", d)
	}

	// Modified backend file -> diff-audit.
	d2 := Classify([]repo.FileChange{{Status: "M", Path: "apps/agent-host/src/sessions.ts"}}, nil, nil, policy)
	if d2.Tier != config.TierDiffAudit || len(d2.Triggers) != 1 {
		t.Fatalf("backend change = %+v, want diff-audit", d2)
	}

	// Modified protocol file escalates to full-live.
	d3 := Classify([]repo.FileChange{
		{Status: "M", Path: "docs/a.md"},
		{Status: "M", Path: "packages/protocol/src/rpc.ts"},
	}, nil, nil, policy)
	if d3.Tier != config.TierFullLive || len(d3.Triggers) != 1 || d3.Triggers[0] != "packages/protocol/src/rpc.ts" {
		t.Fatalf("protocol change = %+v, want full-live triggered by rpc.ts", d3)
	}
}

func TestEvaluateDetectsTestTamperingAndEscalates(t *testing.T) {
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
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runGit("init", "-q", "-b", "main")
	runGit("config", "user.name", "t")
	runGit("config", "user.email", "t@example.com")
	write("pkg/foo_test.go", "package pkg\nfunc TestA(t *testing.T) {}\n")
	runGit("add", "-A")
	runGit("commit", "-qm", "init")

	runGit("switch", "-q", "-c", "feat/skip")
	write("pkg/foo_test.go", "package pkg\nfunc TestA(t *testing.T) { t.Skip(\"flaky\") }\n")
	runGit("commit", "-qam", "skip test")

	cfg, err := config.Parse([]byte(`
version = 1
[tiers]
default = "diff-audit"
gate_only = ["pkg/**"]
`))
	if err != nil {
		t.Fatal(err)
	}

	res, err := Evaluate(ctx, repo.NewExecGit(dir), cfg, "main")
	if err != nil {
		t.Fatalf("Evaluate failed: %v", err)
	}
	if res.Tier != config.TierFullLive {
		t.Fatalf("expected escalation to full-live on test skip, got %+v", res)
	}
	if len(res.TestsTampered) != 1 || res.TestsTampered[0].Kind != "skip_added" || res.TestsTampered[0].Marker != "t.Skip(" {
		t.Fatalf("unexpected tamper findings: %+v", res.TestsTampered)
	}
}
