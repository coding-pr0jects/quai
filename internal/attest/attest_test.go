package attest

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/receipt"
	"github.com/coding-pr0jects/quai/internal/repo"
)

func genSSHKey(t *testing.T, dir, name string) (privPath, pubKey string) {
	t.Helper()
	privPath = filepath.Join(dir, name)
	cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", privPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen failed: %v\n%s", err, string(out))
	}
	rawPub, err := os.ReadFile(privPath + ".pub")
	if err != nil {
		t.Fatalf("read pubkey: %v", err)
	}
	return privPath, strings.TrimSpace(string(rawPub))
}

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, string(out))
	}
	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPAEAndSignVerifyRoundTrip(t *testing.T) {
	pae := string(PAE("text/plain", []byte("hello")))
	if pae != "DSSEv1 10 text/plain 5 hello" {
		t.Fatalf("PAE = %q", pae)
	}

	dir := t.TempDir()
	aliceKey, alicePub := genSSHKey(t, dir, "alice")
	bobKey, bobPub := genSSHKey(t, dir, "bob")

	allowed := filepath.Join(dir, "allowed_signers")
	writeFile(t, allowed, "alice@example.com "+alicePub+"\nbob@example.com "+bobPub+"\n")

	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	r := receipt.Receipt{
		Version:     receipt.CurrentVersion,
		Fingerprint: strings.Repeat("a", 64),
		TreeSHA:     strings.Repeat("b", 40),
		HeadSHA:     strings.Repeat("c", 40),
		BaseSHA:     strings.Repeat("d", 40),
		Step:        "fast",
		Passed:      true,
		DurationMS:  42,
		CreatedAt:   now,
	}

	ctx := context.Background()
	env, err := SignReceipt(ctx, r, aliceKey, "alice@example.com")
	if err != nil {
		t.Fatalf("SignReceipt failed: %v", err)
	}

	info, err := VerifyEnvelope(ctx, env, allowed, []string{"alice@example.com"}, "fast", r.Fingerprint, 24*time.Hour, now.Add(time.Hour))
	if err != nil {
		t.Fatalf("VerifyEnvelope failed: %v", err)
	}
	if info.Signer != "alice@example.com" {
		t.Fatalf("Signer = %q, want alice@example.com", info.Signer)
	}

	// Tampered payload must fail cryptographic verification.
	tampered := env
	tampered.Payload = base64.StdEncoding.EncodeToString([]byte(`{"_type":"https://in-toto.io/Statement/v1"}`))
	if _, err := VerifyEnvelope(ctx, tampered, allowed, nil, "fast", r.Fingerprint, 24*time.Hour, now); err == nil {
		t.Fatal("expected error for tampered payload")
	}

	// Wrong principal allowlist must be rejected even with a valid signature.
	bobEnv, err := SignReceipt(ctx, r, bobKey, "bob@example.com")
	if err != nil {
		t.Fatalf("SignReceipt bob failed: %v", err)
	}
	if _, err := VerifyEnvelope(ctx, bobEnv, allowed, []string{"alice@example.com"}, "fast", r.Fingerprint, 24*time.Hour, now); err == nil {
		t.Fatal("expected error when signer is outside allowed principals")
	}

	// Expired attestation must fail.
	if _, err := VerifyEnvelope(ctx, env, allowed, nil, "fast", r.Fingerprint, time.Hour, now.Add(2*time.Hour)); err == nil {
		t.Fatal("expected error for expired attestation")
	}

	// Signature produced under wrong SSHSIG namespace ("git") must not replay as "quai".
	payloadBytes, _ := base64.StdEncoding.DecodeString(env.Payload)
	wrongNSSig, err := sshSign(ctx, aliceKey, "git", PAE(PayloadType, payloadBytes))
	if err != nil {
		t.Fatalf("sshSign wrong ns failed: %v", err)
	}
	replayEnv := Envelope{
		PayloadType: PayloadType,
		Payload:     env.Payload,
		Signatures:  []Signature{{Sig: base64.StdEncoding.EncodeToString(wrongNSSig)}},
	}
	if _, err := VerifyEnvelope(ctx, replayEnv, allowed, nil, "fast", r.Fingerprint, 24*time.Hour, now); err == nil {
		t.Fatal("expected error when signature uses git namespace instead of quai")
	}
}

func TestGitRefTransportAndOntoMergeTree(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	keyPath, pubKey := genSSHKey(t, tmp, "ci_signer")

	origin := filepath.Join(tmp, "origin.git")
	devRepo := filepath.Join(tmp, "dev")
	ciRepo := filepath.Join(tmp, "ci")

	gitRun(t, tmp, "init", "--bare", "-q", "-b", "main", origin)
	gitRun(t, tmp, "init", "-q", "-b", "main", devRepo)
	gitRun(t, devRepo, "config", "user.name", "dev")
	gitRun(t, devRepo, "config", "user.email", "dev@example.com")
	gitRun(t, devRepo, "remote", "add", "origin", origin)

	writeFile(t, filepath.Join(devRepo, ".quai/allowed_signers"), "dev@example.com "+pubKey+"\n")
	writeFile(t, filepath.Join(devRepo, "src/app.go"), "package app\nfunc V() int { return 1 }\n")
	writeFile(t, filepath.Join(devRepo, "README.md"), "# demo\n")
	gitRun(t, devRepo, "add", "-A")
	gitRun(t, devRepo, "commit", "-q", "-m", "init")
	gitRun(t, devRepo, "push", "-q", "-u", "origin", "main")

	// Branch changes src/app.go and signs attestation for step "code" (inputs: src/**).
	gitRun(t, devRepo, "checkout", "-q", "-b", "feat/work")
	writeFile(t, filepath.Join(devRepo, "src/app.go"), "package app\nfunc V() int { return 2 }\n")
	gitRun(t, devRepo, "add", "src/app.go")
	gitRun(t, devRepo, "commit", "-q", "-m", "update app")
	gitRun(t, devRepo, "push", "-q", "-u", "origin", "feat/work")

	cfg, err := config.Parse([]byte(`
version = 1
base = "main"

[[steps]]
name = "code"
run = "go test ./..."
inputs = ["src/**"]

[attest]
allowed_signers = ".quai/allowed_signers"
max_age = "14d"
`))
	if err != nil {
		t.Fatalf("Parse config: %v", err)
	}

	devGit := repo.NewExecGit(devRepo)
	headSHA, _ := devGit.HeadSHA(ctx)
	treeSHA, _ := devGit.TreeSHA(ctx, "HEAD")
	baseSHA, _ := devGit.MergeBase(ctx, "origin/main", "HEAD")
	fp, err := receipt.Fingerprint(ctx, devGit, cfg.Steps[0], treeSHA, baseSHA)
	if err != nil {
		t.Fatalf("Fingerprint: %v", err)
	}

	ref, _, err := WriteRecord(ctx, devGit, receipt.Receipt{
		Version:     receipt.CurrentVersion,
		Fingerprint: fp,
		TreeSHA:     treeSHA,
		HeadSHA:     headSHA,
		BaseSHA:     baseSHA,
		Step:        "code",
		Passed:      true,
		DurationMS:  120,
		CreatedAt:   time.Now().UTC(),
	}, keyPath, "dev@example.com")
	if err != nil {
		t.Fatalf("WriteRecord: %v", err)
	}

	pushed, err := Push(ctx, devGit, cfg, PushOptions{Remote: "origin"})
	if err != nil {
		t.Fatalf("Push: %v", err)
	}
	if len(pushed.Pushed) != 1 || pushed.Pushed[0] != ref {
		t.Fatalf("Pushed = %v, want [%s]", pushed.Pushed, ref)
	}

	// Simulate an unrelated commit landing on origin/main (README.md outside src/**).
	gitRun(t, devRepo, "checkout", "-q", "main")
	writeFile(t, filepath.Join(devRepo, "README.md"), "# demo updated\n")
	gitRun(t, devRepo, "add", "README.md")
	gitRun(t, devRepo, "commit", "-q", "-m", "docs update on main")
	gitRun(t, devRepo, "push", "-q", "origin", "main")

	// Fresh CI clone checks out feat/work and runs Check --fetch --onto origin/main.
	gitRun(t, tmp, "clone", "-q", "-b", "feat/work", origin, ciRepo)
	ciGit := repo.NewExecGit(ciRepo)

	res, err := Check(ctx, ciGit, cfg, CheckOptions{
		Onto:  "origin/main",
		Fetch: true,
	})
	if err != nil {
		t.Fatalf("Check onto unrelated main failed: %v", err)
	}
	if !res.OK || len(res.Steps) != 1 || res.Steps[0].Status != "verified" || res.Steps[0].Signer != "dev@example.com" {
		t.Fatalf("unexpected Check result: %+v", res)
	}

	// Probabilistic sampling at 100% must force a rerun (OK=false, status=sampled).
	sampled, err := Check(ctx, ciGit, cfg, CheckOptions{
		Onto:   "origin/main",
		Sample: 100,
		Roll:   func() int { return 5 },
	})
	if err != nil {
		t.Fatalf("Check sampled failed: %v", err)
	}
	if sampled.OK || !sampled.Sampled || sampled.Steps[0].Status != "sampled" {
		t.Fatalf("expected sampled rerun, got %+v", sampled)
	}

	// Now main modifies a file inside src/** (disjoint file src/other.go) -> merged fingerprint changes -> missing.
	writeFile(t, filepath.Join(devRepo, "src/other.go"), "package app\nfunc W() int { return 9 }\n")
	gitRun(t, devRepo, "add", "src/other.go")
	gitRun(t, devRepo, "commit", "-q", "-m", "code change on main")
	gitRun(t, devRepo, "push", "-q", "origin", "main")
	gitRun(t, ciRepo, "fetch", "-q", "origin", "main")

	miss, err := Check(ctx, ciGit, cfg, CheckOptions{Onto: "origin/main"})
	if err != nil {
		t.Fatalf("Check after src change on main errored: %v", err)
	}
	if miss.OK || miss.Steps[0].Status != "missing" {
		t.Fatalf("expected missing after main touched step inputs, got %+v", miss)
	}

	// Conflicting edit on main must return RebaseConflictError without dirtying CI working tree.
	writeFile(t, filepath.Join(devRepo, "src/app.go"), "package app\nfunc V() int { return 999 }\n")
	gitRun(t, devRepo, "add", "src/app.go")
	gitRun(t, devRepo, "commit", "-q", "-m", "conflict on main")
	gitRun(t, devRepo, "push", "-q", "origin", "main")
	gitRun(t, ciRepo, "fetch", "-q", "origin", "main")

	_, err = Check(ctx, ciGit, cfg, CheckOptions{Onto: "origin/main"})
	var rce *repo.RebaseConflictError
	if !errors.As(err, &rce) {
		t.Fatalf("expected RebaseConflictError, got %v", err)
	}
	clean, _, err := ciGit.IsClean(ctx)
	if err != nil || !clean {
		t.Fatalf("CI working tree dirtied after merge-tree conflict: clean=%v err=%v", clean, err)
	}
}
