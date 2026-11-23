package verdict

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/repo"
)

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestVerdictSurvivesCleanRebaseAndInvalidatesOnChange(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "repo")
	keyPath := filepath.Join(tmp, "reviewer")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, string(out))
	}
	pubRaw, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	allowedPath := filepath.Join(tmp, "allowed_signers")
	writeFile(t, allowedPath, "reviewer@example.com "+strings.TrimSpace(string(pubRaw))+"\n")

	runGit(t, tmp, "init", "-q", "-b", "main", dir)
	runGit(t, dir, "config", "user.name", "rev")
	runGit(t, dir, "config", "user.email", "reviewer@example.com")
	policy := `
version = 1
base = "main"
[tiers]
default = "diff-audit"
full_live = ["ui/**"]
gate_only = ["docs/**"]
`
	writeFile(t, filepath.Join(dir, "ui/app.ts"), "export const x = 1\n")
	writeFile(t, filepath.Join(dir, "docs/a.md"), "v1\n")
	writeFile(t, filepath.Join(dir, ".quai.toml"), policy)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")

	cfg, err := config.Parse([]byte(policy))
	if err != nil {
		t.Fatal(err)
	}
	g := repo.NewExecGit(dir)

	// Feature branch touches ui/app.ts (full-live).
	runGit(t, dir, "switch", "-q", "-c", "feat/ui")
	writeFile(t, filepath.Join(dir, "ui/app.ts"), "export const x = 2\n")
	runGit(t, dir, "commit", "-qam", "ui change")

	// Before recording, Check must fail with "missing".
	miss, err := Check(ctx, g, cfg, CheckOptions{})
	if err != nil || miss.OK || miss.Status != "missing" || miss.BranchTier != config.TierFullLive {
		t.Fatalf("expected missing full-live verdict, got res=%+v err=%v", miss, err)
	}

	// Record a signed approved verdict.
	recRes, err := RecordVerdict(ctx, g, cfg, RecordOptions{
		Status:     StatusApproved,
		Sign:       true,
		SigningKey: keyPath,
		Note:       "verified live",
	})
	if err != nil || !recRes.OK || !recRes.Signed || recRes.Record.Tier != config.TierFullLive {
		t.Fatalf("RecordVerdict failed: res=%+v err=%v", recRes, err)
	}

	// Unrelated commit lands on main, then feat/ui rebases cleanly onto main.
	runGit(t, dir, "switch", "-q", "main")
	writeFile(t, filepath.Join(dir, "docs/a.md"), "v2\n")
	runGit(t, dir, "commit", "-qam", "docs on main")
	runGit(t, dir, "switch", "-q", "feat/ui")
	runGit(t, dir, "rebase", "-q", "main")

	// Verdict MUST survive the clean rebase (both unsigned and require-signed)!
	okSigned, err := Check(ctx, g, cfg, CheckOptions{
		RequireSigned:  true,
		AllowedSigners: allowedPath,
	})
	if err != nil || !okSigned.OK || !okSigned.Signed || okSigned.Signer != "reviewer@example.com" {
		t.Fatalf("expected signed verdict to survive clean rebase, got res=%+v err=%v", okSigned, err)
	}

	writeFile(t, filepath.Join(dir, "docs/evidence/shot.png"), "png\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "evidence")
	afterEvidence, err := Check(ctx, g, cfg, CheckOptions{ConfigRef: "main", RequireSigned: true, AllowedSigners: allowedPath})
	if err != nil || !afterEvidence.OK || afterEvidence.Record.PatchID == afterEvidence.PatchID {
		t.Fatalf("a gate-only commit must keep the verdict through its code patch-id, got res=%+v err=%v", afterEvidence, err)
	}

	// Now edit ui/app.ts -> patch-id changes -> verdict must be invalidated!
	writeFile(t, filepath.Join(dir, "ui/app.ts"), "export const x = 3\n")
	runGit(t, dir, "commit", "-qam", "extra edit")

	afterEdit, err := Check(ctx, g, cfg, CheckOptions{})
	if err != nil || afterEdit.OK || afterEdit.Status != "missing" {
		t.Fatalf("expected verdict invalidation after code edit, got res=%+v err=%v", afterEdit, err)
	}
}

func TestVerdictSurvivesGateOnlyFollowUps(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "repo")
	keyPath := filepath.Join(tmp, "reviewer")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, string(out))
	}
	pubRaw, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	allowedPath := filepath.Join(tmp, "allowed_signers")
	writeFile(t, allowedPath, "reviewer@example.com "+strings.TrimSpace(string(pubRaw))+"\n")

	const basePolicy = `
version = 1
base = "main"
[tiers]
default = "diff-audit"
full_live = ["ui/**", "docs/api/**"]
gate_only = ["docs/**", ".quai/**"]
[attest]
allowed_signers = ".quai/allowed_signers"
`
	const widenedPolicy = `
version = 1
base = "main"
[tiers]
default = "diff-audit"
full_live = ["ui/**", "docs/api/**"]
gate_only = ["docs/**", ".quai/**", "lib/**", ".quai.toml"]
[attest]
allowed_signers = ".quai/allowed_signers"
`
	runGit(t, tmp, "init", "-q", "-b", "main", dir)
	runGit(t, dir, "config", "user.name", "rev")
	runGit(t, dir, "config", "user.email", "reviewer@example.com")
	writeFile(t, filepath.Join(dir, "ui/app.ts"), "export const x = 1\n")
	writeFile(t, filepath.Join(dir, "docs/a.md"), "v1\n")
	writeFile(t, filepath.Join(dir, "docs/api/schema.md"), "v1\n")
	writeFile(t, filepath.Join(dir, ".quai/allowed_signers"), "v1\n")
	writeFile(t, filepath.Join(dir, ".quai.toml"), basePolicy)
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-qm", "init")
	g := repo.NewExecGit(dir)

	runGit(t, dir, "switch", "-q", "-c", "feat/ui")
	writeFile(t, filepath.Join(dir, "ui/app.ts"), "export const x = 2\n")
	writeFile(t, filepath.Join(dir, "docs/a.md"), "v2\n")
	runGit(t, dir, "commit", "-qam", "ui change with docs")
	cfg, err := config.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := RecordVerdict(ctx, g, cfg, RecordOptions{Status: StatusApproved, Sign: true, SigningKey: keyPath})
	if err != nil || rec.Record.CodePatchID == "" || rec.Record.CodePatchID == rec.Record.PatchID || rec.CodeRef != RefName(rec.Record.CodePatchID) {
		t.Fatalf("a reviewed diff with gate-only paths must record its code patch-id under its own ref: res=%+v err=%v", rec, err)
	}
	reviewed := runGit(t, dir, "rev-parse", "HEAD")

	cases := []struct {
		name  string
		edit  map[string]string
		valid bool
	}{
		{"evidence added", map[string]string{"docs/evidence/shot.png": "png\n"}, true},
		{"gate-only doc edited", map[string]string{"docs/a.md": "v3\n"}, true},
		{"gate-only doc back to base", map[string]string{"docs/a.md": "v1\n"}, true},
		{"gate-only file beside allowed_signers", map[string]string{".quai/readme.txt": "hi\n"}, true},
		{"code edited", map[string]string{"ui/app.ts": "export const x = 3\n"}, false},
		{"full_live beats gate_only", map[string]string{"docs/api/schema.md": "v2\n"}, false},
		{"test file under gate_only", map[string]string{"docs/example_test.go": "package docs\n"}, false},
		{"path outside every glob", map[string]string{"tool.go": "package tool\n"}, false},
		{"code and evidence", map[string]string{"ui/app.ts": "export const x = 3\n", "docs/evidence/shot.png": "png\n"}, false},
		{"allowed_signers under gate_only", map[string]string{".quai/allowed_signers": "v2\n"}, false},
		{"branch widens gate_only to hide code", map[string]string{".quai.toml": widenedPolicy, "lib/evil.go": "package lib\n"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runGit(t, dir, "reset", "-q", "--hard", reviewed)
			for rel, body := range tc.edit {
				writeFile(t, filepath.Join(dir, rel), body)
			}
			runGit(t, dir, "add", "-A")
			runGit(t, dir, "commit", "-qm", tc.name)
			branchCfg, err := config.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, opts := range []CheckOptions{
				{ConfigRef: "main"},
				{ConfigRef: "main", RequireSigned: true, AllowedSigners: allowedPath},
				{},
				{RequireSigned: true, AllowedSigners: allowedPath},
			} {
				res, err := Check(ctx, g, branchCfg, opts)
				if err != nil {
					t.Fatal(err)
				}
				wantValid := tc.valid && opts.ConfigRef != ""
				if res.OK != wantValid || (!wantValid && res.Status != "missing") {
					t.Fatalf("opts=%+v: want valid=%v, got res=%+v", opts, wantValid, res)
				}
				if wantValid && (res.Record.PatchID != rec.Record.PatchID || res.Signed != opts.RequireSigned) {
					t.Fatalf("opts=%+v: matched the wrong verdict: %+v", opts, res)
				}
			}
		})
	}
}
