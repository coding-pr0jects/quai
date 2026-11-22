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

	// External allowed_signers override works without committing any key inside the repository.
	extAllowed := filepath.Join(tmp, "external_allowed_signers")
	writeFile(t, extAllowed, "dev@example.com "+pubKey+"\n")
	cfgNoFile := *cfg
	cfgNoFile.Attest.AllowedSigners = ""
	extRes, err := Check(ctx, ciGit, &cfgNoFile, CheckOptions{
		Onto:           "origin/main",
		AllowedSigners: extAllowed,
	})
	if err != nil || !extRes.OK || extRes.Steps[0].Status != "verified" {
		t.Fatalf("Check with external AllowedSigners failed: res=%+v err=%v", extRes, err)
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

func TestTargetedFetchAndPrune(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	keyPath, pubKey := genSSHKey(t, tmp, "prune_signer")

	origin := filepath.Join(tmp, "origin.git")
	devRepo := filepath.Join(tmp, "dev")
	ciRepo := filepath.Join(tmp, "ci")

	gitRun(t, tmp, "init", "--bare", "-q", "-b", "main", origin)
	gitRun(t, tmp, "init", "-q", "-b", "main", devRepo)
	gitRun(t, devRepo, "config", "user.name", "dev")
	gitRun(t, devRepo, "config", "user.email", "dev@example.com")
	gitRun(t, devRepo, "remote", "add", "origin", origin)

	writeFile(t, filepath.Join(devRepo, ".quai/allowed_signers"), "dev@example.com "+pubKey+"\n")
	writeFile(t, filepath.Join(devRepo, "src/app.go"), "package app\n")
	gitRun(t, devRepo, "add", "-A")
	gitRun(t, devRepo, "commit", "-q", "-m", "init")
	gitRun(t, devRepo, "push", "-q", "-u", "origin", "main")

	cfg, err := config.Parse([]byte(`
version = 1
base = "main"

[[steps]]
name = "fast"
run = "true"
inputs = ["src/**"]

[[steps]]
name = "slow"
run = "true"
inputs = ["src/**"]

[attest]
allowed_signers = ".quai/allowed_signers"
max_age = "7d"
`))
	if err != nil {
		t.Fatalf("Parse config: %v", err)
	}

	devGit := repo.NewExecGit(devRepo)
	headSHA, _ := devGit.HeadSHA(ctx)
	treeSHA, _ := devGit.TreeSHA(ctx, "HEAD")
	baseSHA, _ := devGit.MergeBase(ctx, "origin/main", "HEAD")
	fastFP, err := receipt.Fingerprint(ctx, devGit, cfg.Steps[0], treeSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

	// Write one fresh attestation for "fast" and one expired unrelated attestation.
	freshRef, _, err := WriteRecord(ctx, devGit, receipt.Receipt{
		Version:     receipt.CurrentVersion,
		Fingerprint: fastFP,
		TreeSHA:     treeSHA,
		HeadSHA:     headSHA,
		BaseSHA:     baseSHA,
		Step:        "fast",
		Passed:      true,
		CreatedAt:   now.Add(-24 * time.Hour),
	}, keyPath, "dev@example.com")
	if err != nil {
		t.Fatal(err)
	}
	staleRef, _, err := WriteRecord(ctx, devGit, receipt.Receipt{
		Version:     receipt.CurrentVersion,
		Fingerprint: strings.Repeat("e", 64),
		TreeSHA:     treeSHA,
		HeadSHA:     headSHA,
		BaseSHA:     baseSHA,
		Step:        "fast",
		Passed:      true,
		CreatedAt:   now.Add(-30 * 24 * time.Hour),
	}, keyPath, "dev@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := devGit.PushRefs(ctx, "origin", []string{freshRef, staleRef}); err != nil {
		t.Fatalf("PushRefs: %v", err)
	}

	// Targeted fetch in CI clone requesting both "fast" (exists) and "slow" (missing on remote)
	// must fetch only freshRef and NOT pull staleRef.
	gitRun(t, tmp, "clone", "-q", "-b", "main", origin, ciRepo)
	ciGit := repo.NewExecGit(ciRepo)
	res, err := Check(ctx, ciGit, cfg, CheckOptions{
		Fetch: true,
		Now:   func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Check targeted fetch failed: %v", err)
	}
	if res.OK || len(res.Steps) != 2 || res.Steps[0].Status != "verified" || res.Steps[1].Status != "missing" {
		t.Fatalf("unexpected mixed step check result: %+v", res)
	}
	ciLocalRefs, err := ciGit.ListRefs(ctx, RefPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(ciLocalRefs) != 1 || ciLocalRefs[0] != freshRef {
		t.Fatalf("expected targeted fetch to pull only [%s], got %v", freshRef, ciLocalRefs)
	}

	// Prune --dry-run must report staleRef without deleting it.
	dryRes, err := Prune(ctx, devGit, cfg, PruneOptions{
		DryRun: true,
		Now:    func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Prune dry-run: %v", err)
	}
	if dryRes.Kept != 1 || len(dryRes.PrunedLocal) != 1 || dryRes.PrunedLocal[0] != staleRef || len(dryRes.PrunedRemote) != 1 {
		t.Fatalf("unexpected dry-run prune result: %+v", dryRes)
	}

	// Real prune deletes staleRef locally and remotely while preserving freshRef.
	pruneRes, err := Prune(ctx, devGit, cfg, PruneOptions{
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if pruneRes.Kept != 1 || len(pruneRes.PrunedLocal) != 1 || len(pruneRes.PrunedRemote) != 1 {
		t.Fatalf("unexpected prune result: %+v", pruneRes)
	}

	remainingLocal, _ := devGit.ListRefs(ctx, RefPrefix)
	remainingRemote, _ := devGit.ListRemoteRefs(ctx, "origin", []string{RefPrefix + "*"})
	if len(remainingLocal) != 1 || remainingLocal[0] != freshRef {
		t.Fatalf("remaining local refs = %v, want [%s]", remainingLocal, freshRef)
	}
	if len(remainingRemote) != 1 || remainingRemote[0] != freshRef {
		t.Fatalf("remaining remote refs = %v, want [%s]", remainingRemote, freshRef)
	}
}

func TestConfigRefPreventsPRConfigTampering(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	keyPath, pubKey := genSSHKey(t, tmp, "base_signer")
	extAllowed := filepath.Join(tmp, "allowed_signers")
	writeFile(t, extAllowed, "dev@example.com "+pubKey+"\n")

	origin := filepath.Join(tmp, "origin.git")
	devRepo := filepath.Join(tmp, "dev")
	gitRun(t, tmp, "init", "--bare", "-q", "-b", "main", origin)
	gitRun(t, tmp, "init", "-q", "-b", "main", devRepo)
	gitRun(t, devRepo, "config", "user.name", "dev")
	gitRun(t, devRepo, "config", "user.email", "dev@example.com")
	gitRun(t, devRepo, "remote", "add", "origin", origin)

	baseCfgText := `
version = 1
base = "main"

[[steps]]
name = "verify"
run = "go test ./..."
inputs = ["src/**"]

[attest]
signers = ["dev@example.com"]
max_age = "14d"
`
	writeFile(t, filepath.Join(devRepo, ".quai.toml"), baseCfgText)
	writeFile(t, filepath.Join(devRepo, "src/app.go"), "package app\n")
	gitRun(t, devRepo, "add", "-A")
	gitRun(t, devRepo, "commit", "-q", "-m", "init")
	gitRun(t, devRepo, "push", "-q", "-u", "origin", "main")

	// PR branch weakens .quai.toml (run = "true") and signs an attestation against the weakened config.
	gitRun(t, devRepo, "checkout", "-q", "-b", "feat/weaken")
	weakCfgText := strings.ReplaceAll(baseCfgText, `run = "go test ./..."`, `run = "true"`)
	writeFile(t, filepath.Join(devRepo, ".quai.toml"), weakCfgText)
	writeFile(t, filepath.Join(devRepo, "src/app.go"), "package app\nfunc X() {}\n")
	gitRun(t, devRepo, "add", "-A")
	gitRun(t, devRepo, "commit", "-q", "-m", "weaken quai config")

	g := repo.NewExecGit(devRepo)
	weakCfg, err := config.Load(devRepo)
	if err != nil {
		t.Fatal(err)
	}
	headSHA, _ := g.HeadSHA(ctx)
	treeSHA, _ := g.TreeSHA(ctx, "HEAD")
	baseSHA, _ := g.MergeBase(ctx, "origin/main", "HEAD")
	weakFP, err := receipt.Fingerprint(ctx, g, weakCfg.Steps[0], treeSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteRecord(ctx, g, receipt.Receipt{
		Version:     receipt.CurrentVersion,
		Fingerprint: weakFP,
		TreeSHA:     treeSHA,
		HeadSHA:     headSHA,
		BaseSHA:     baseSHA,
		Step:        "verify",
		Passed:      true,
		CreatedAt:   time.Now().UTC(),
	}, keyPath, "dev@example.com"); err != nil {
		t.Fatal(err)
	}

	// Checking with --config-ref origin/main MUST reject the weakened attestation.
	rejected, err := Check(ctx, g, nil, CheckOptions{
		ConfigRef:      "origin/main",
		AllowedSigners: extAllowed,
	})
	if err != nil {
		t.Fatalf("Check with --config-ref errored: %v", err)
	}
	if rejected.OK || rejected.ConfigRef != "origin/main" || rejected.Steps[0].Status != "missing" {
		t.Fatalf("expected weakened attestation to fail under --config-ref origin/main, got %+v", rejected)
	}

	// Signing against origin/main's real step command succeeds under --config-ref origin/main.
	baseCfg, err := config.Parse([]byte(baseCfgText))
	if err != nil {
		t.Fatal(err)
	}
	realFP, err := receipt.Fingerprint(ctx, g, baseCfg.Steps[0], treeSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := WriteRecord(ctx, g, receipt.Receipt{
		Version:     receipt.CurrentVersion,
		Fingerprint: realFP,
		TreeSHA:     treeSHA,
		HeadSHA:     headSHA,
		BaseSHA:     baseSHA,
		Step:        "verify",
		Passed:      true,
		CreatedAt:   time.Now().UTC(),
	}, keyPath, "dev@example.com"); err != nil {
		t.Fatal(err)
	}

	accepted, err := Check(ctx, g, nil, CheckOptions{
		ConfigRef:      "origin/main",
		AllowedSigners: extAllowed,
	})
	if err != nil || !accepted.OK || accepted.Steps[0].Status != "verified" {
		t.Fatalf("expected real attestation to pass under --config-ref origin/main, got res=%+v err=%v", accepted, err)
	}
}

func TestPruneDryRunDoesNotFetch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	keyPath, _ := genSSHKey(t, tmp, "dryrun_signer")
	origin := filepath.Join(tmp, "origin.git")
	devRepo := filepath.Join(tmp, "dev")
	ciRepo := filepath.Join(tmp, "ci")
	gitRun(t, tmp, "init", "--bare", "-q", "-b", "main", origin)
	gitRun(t, tmp, "init", "-q", "-b", "main", devRepo)
	gitRun(t, devRepo, "config", "user.name", "dev")
	gitRun(t, devRepo, "config", "user.email", "dev@example.com")
	gitRun(t, devRepo, "remote", "add", "origin", origin)
	writeFile(t, filepath.Join(devRepo, "app.go"), "package app\n")
	gitRun(t, devRepo, "add", "-A")
	gitRun(t, devRepo, "commit", "-q", "-m", "init")
	gitRun(t, devRepo, "push", "-q", "-u", "origin", "main")

	cfg, err := config.Parse([]byte("version = 1\n[attest]\nmax_age = \"1h\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	devGit := repo.NewExecGit(devRepo)
	ref, _, err := WriteRecord(ctx, devGit, receipt.Receipt{
		Version:     receipt.CurrentVersion,
		Fingerprint: strings.Repeat("a", 64),
		Step:        "fast",
		Passed:      true,
		CreatedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}, keyPath, "dev@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if err := devGit.PushRefs(ctx, "origin", []string{ref}); err != nil {
		t.Fatal(err)
	}
	gitRun(t, tmp, "clone", "-q", "-b", "main", origin, ciRepo)
	ciGit := repo.NewExecGit(ciRepo)

	res, err := Prune(ctx, ciGit, cfg, PruneOptions{
		DryRun: true,
		Now:    func() time.Time { return time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Prune dry-run: %v", err)
	}
	if len(res.PrunedLocal) != 0 || len(res.PrunedRemote) != 0 {
		t.Fatalf("remote-only refs are not fetchable in a dry run: %+v", res)
	}
	local, err := ciGit.ListRefs(ctx, RefPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(local) != 0 {
		t.Fatalf("dry run fetched refs into the clone: %v", local)
	}
}

func TestPruneRejectsMaxAgeBelowMinimum(t *testing.T) {
	_, err := Prune(context.Background(), nil, &config.Config{}, PruneOptions{MaxAge: 30 * time.Second})
	if err == nil || !strings.Contains(err.Error(), "minimum") {
		t.Fatalf("Prune error = %v, want a minimum max_age error", err)
	}
}

func TestClassifyMiss(t *testing.T) {
	tree := strings.Repeat("a", 40)
	otherTree := strings.Repeat("b", 40)
	base := strings.Repeat("c", 40)
	otherBase := strings.Repeat("d", 40)

	cases := []struct {
		name       string
		candidates []RecordCandidate
		want       string
	}{
		{"no candidates", nil, ReasonNoRecord},
		{
			"same tree and base, different fingerprint",
			[]RecordCandidate{{Fingerprint: "f1", TreeSHA: tree, BaseSHA: base}},
			ReasonConfigMismatch,
		},
		{
			"same tree, different base",
			[]RecordCandidate{{Fingerprint: "f1", TreeSHA: tree, BaseSHA: otherBase}},
			ReasonBaseMismatch,
		},
		{
			"different tree",
			[]RecordCandidate{{Fingerprint: "f1", TreeSHA: otherTree, BaseSHA: base}},
			ReasonTreeMismatch,
		},
		{
			"same-tree base mismatch outranks tree mismatch",
			[]RecordCandidate{
				{Fingerprint: "f1", TreeSHA: otherTree, BaseSHA: base},
				{Fingerprint: "f2", TreeSHA: tree, BaseSHA: otherBase},
			},
			ReasonBaseMismatch,
		},
		{
			"config mismatch outranks base mismatch",
			[]RecordCandidate{
				{Fingerprint: "f1", TreeSHA: tree, BaseSHA: otherBase},
				{Fingerprint: "f2", TreeSHA: tree, BaseSHA: base},
			},
			ReasonConfigMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, detail := ClassifyMiss(tree, base, tc.candidates)
			if reason != tc.want {
				t.Fatalf("reason = %q, want %q (%s)", reason, tc.want, detail)
			}
			if detail == "" {
				t.Fatal("expected a diagnostic detail")
			}
		})
	}
}

func TestCheckDiagnosticReasons(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	aliceKey, alicePub := genSSHKey(t, tmp, "alice")
	bobKey, _ := genSSHKey(t, tmp, "bob")

	repoDir := filepath.Join(tmp, "repo")
	gitRun(t, tmp, "init", "-q", "-b", "main", repoDir)
	gitRun(t, repoDir, "config", "user.name", "alice")
	gitRun(t, repoDir, "config", "user.email", "alice@example.com")
	writeFile(t, filepath.Join(repoDir, ".quai/allowed_signers"), "alice@example.com "+alicePub+"\n")
	writeFile(t, filepath.Join(repoDir, "src/app.go"), "package app\n")
	gitRun(t, repoDir, "add", "-A")
	gitRun(t, repoDir, "commit", "-q", "-m", "init")

	cfg, err := config.Parse([]byte(`
version = 1
base = "main"

[[steps]]
name = "fast"
run = "true"
inputs = ["src/**"]

[attest]
allowed_signers = ".quai/allowed_signers"
max_age = "14d"
`))
	if err != nil {
		t.Fatalf("Parse config: %v", err)
	}

	g := repo.NewExecGit(repoDir)
	headSHA, err := g.HeadSHA(ctx)
	if err != nil {
		t.Fatal(err)
	}
	treeSHA, err := g.TreeSHA(ctx, "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	baseSHA := headSHA
	fp, err := receipt.Fingerprint(ctx, g, cfg.Steps[0], treeSHA, baseSHA)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	writeRecord := func(keyPath, principal string) {
		t.Helper()
		if _, _, err := WriteRecord(ctx, g, receipt.Receipt{
			Version:     receipt.CurrentVersion,
			Fingerprint: fp,
			TreeSHA:     treeSHA,
			HeadSHA:     headSHA,
			BaseSHA:     baseSHA,
			Step:        "fast",
			Passed:      true,
			CreatedAt:   now,
		}, keyPath, principal); err != nil {
			t.Fatalf("WriteRecord(%s): %v", principal, err)
		}
	}
	checkReason := func(name string, opts CheckOptions, wantOK bool, wantReason string) {
		t.Helper()
		res, err := Check(ctx, g, cfg, opts)
		if err != nil {
			t.Fatalf("%s: Check failed: %v", name, err)
		}
		if res.OK != wantOK {
			t.Fatalf("%s: OK = %v, want %v (%+v)", name, res.OK, wantOK, res.Steps)
		}
		if len(res.Steps) != 1 {
			t.Fatalf("%s: expected one step, got %+v", name, res.Steps)
		}
		sc := res.Steps[0]
		if sc.Reason != wantReason {
			t.Fatalf("%s: reason = %q, want %q (%s)", name, sc.Reason, wantReason, sc.Error)
		}
		if sc.TreeSHA != res.TreeSHA || sc.BaseSHA != res.BaseSHA || sc.Fingerprint == "" {
			t.Fatalf("%s: incomplete diagnostics: %+v", name, sc)
		}
	}

	checkReason("no record", CheckOptions{}, false, ReasonNoRecord)

	writeRecord(aliceKey, "alice@example.com")
	checkReason("verified", CheckOptions{}, true, ReasonVerified)

	changed := *cfg
	changed.Steps = append([]config.Step(nil), cfg.Steps...)
	changed.Steps[0].Run = "true # changed"
	res, err := Check(ctx, g, &changed, CheckOptions{})
	if err != nil {
		t.Fatalf("config mismatch Check failed: %v", err)
	}
	if res.Steps[0].Reason != ReasonConfigMismatch {
		t.Fatalf("config change reason = %q, want %q (%s)", res.Steps[0].Reason, ReasonConfigMismatch, res.Steps[0].Error)
	}

	checkReason("expired", CheckOptions{Now: func() time.Time { return now.Add(30 * 24 * time.Hour) }}, false, ReasonExpired)

	writeRecord(bobKey, "bob@example.com")
	checkReason("foreign signer", CheckOptions{}, false, ReasonSignatureInvalid)

	writeRecord(aliceKey, "alice@example.com")
	writeFile(t, filepath.Join(repoDir, "src/app.go"), "package app\nconst V = 2\n")
	gitRun(t, repoDir, "add", "src/app.go")
	gitRun(t, repoDir, "commit", "-q", "-m", "change src")
	checkReason("tree changed", CheckOptions{}, false, ReasonTreeMismatch)
}
