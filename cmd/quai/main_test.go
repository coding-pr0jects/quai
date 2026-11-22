package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const defaultCLITestConfig = `
version = 1
base = "main"

[slots]
heavy = 2

[[steps]]
name = "fast"
run = "true"
hook = "pre-push"

[[steps]]
name = "verify"
run = "true"
pool = "heavy"
slots = 1

[tiers]
default = "diff-audit"
full_live = ["ui/**"]
gate_only = ["docs/**"]
`

func initCLITestRepo(t *testing.T) string {
	t.Helper()
	return initCLITestRepoWith(t, defaultCLITestConfig)
}

func initCLITestRepoWith(t *testing.T, quaiToml string) string {
	t.Helper()
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
	write(".quai.toml", quaiToml)
	write("docs/a.md", "v1\n")
	write("ui/app.ts", "export const x = 1\n")
	runGit("add", "-A")
	runGit("commit", "-qm", "init")
	return dir
}

func TestCLIVerifyTierExecHooksAndPatchID(t *testing.T) {
	ctx := context.Background()
	dir := initCLITestRepo(t)

	// 1. quai verify --json
	var out, errBuf bytes.Buffer
	code := run(ctx, []string{"verify", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf)
	if code != 0 {
		t.Fatalf("verify exited %d: %s %s", code, out.String(), errBuf.String())
	}
	var v struct {
		OK             bool `json:"ok"`
		ReceiptWritten bool `json:"receipt_written"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || !v.OK || !v.ReceiptWritten {
		t.Fatalf("unexpected verify json: %s (err=%v)", out.String(), err)
	}

	// 2. quai tier --json reports gate-only on clean branch and valid receipts.
	out.Reset()
	code = run(ctx, []string{"tier", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf)
	if code != 0 {
		t.Fatalf("tier exited %d: %s", code, errBuf.String())
	}
	var tr struct {
		Tier     string                    `json:"tier"`
		Receipts map[string]map[string]any `json:"receipts"`
	}
	if err := json.Unmarshal(out.Bytes(), &tr); err != nil || tr.Tier != "gate-only" || tr.Receipts["fast"]["valid"] != true {
		t.Fatalf("unexpected tier json: %s", out.String())
	}

	// 3. quai exec --pool heavy -- echo ok
	out.Reset()
	code = run(ctx, []string{"exec", "--pool", "heavy", "--", "sh", "-c", "echo slot-ok"}, dir, bytes.NewReader(nil), &out, &errBuf)
	if code != 0 || bytes.TrimSpace(out.Bytes()) == nil || string(bytes.TrimSpace(out.Bytes())) != "slot-ok" {
		t.Fatalf("exec failed: code=%d out=%q err=%q", code, out.String(), errBuf.String())
	}

	// 4. quai hooks install & pre-push hits receipt cache.
	out.Reset()
	if code := run(ctx, []string{"hooks", "install", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 0 {
		t.Fatalf("hooks install failed: %d %s", code, errBuf.String())
	}
	out.Reset()
	if code := run(ctx, []string{"hooks", "pre-push", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 0 {
		t.Fatalf("hooks pre-push failed: %d %s", code, errBuf.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(`"cached"`)) {
		t.Fatalf("expected cached status from hooks pre-push, got: %s", out.String())
	}

	// 5. quai patch-id on same HEAD -> SURVIVES (exit 0).
	out.Reset()
	if code := run(ctx, []string{"patch-id", "HEAD", "HEAD", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 0 {
		t.Fatalf("patch-id failed: %d %s %s", code, out.String(), errBuf.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(`"SURVIVES"`)) {
		t.Fatalf("expected SURVIVES, got: %s", out.String())
	}

	// 6. quai status --json & quai doctor --json
	out.Reset()
	errBuf.Reset()
	if code := run(ctx, []string{"status", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 0 {
		t.Fatalf("status failed: %d %s %s", code, out.String(), errBuf.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(`"receipt_count": 2`)) {
		t.Fatalf("expected 2 cached receipts in status json, got: %s", out.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := run(ctx, []string{"doctor", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 0 {
		t.Fatalf("doctor failed: %d %s %s", code, out.String(), errBuf.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(`"ok": true`)) {
		t.Fatalf("expected ok doctor json, got: %s", out.String())
	}

	// 7. Unknown command -> exit 2.
	out.Reset()
	errBuf.Reset()
	if code := run(ctx, []string{"nope"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 2 {
		t.Fatalf("expected exit 2 on unknown command, got %d", code)
	}
}

func TestCLISignPushAndAttestCheck(t *testing.T) {
	t.Run("foreground", func(t *testing.T) { testSignPushAndAttestCheck(t, false) })
	t.Run("detached", func(t *testing.T) { testSignPushAndAttestCheck(t, true) })
}

func testSignPushAndAttestCheck(t *testing.T, detached bool) {
	ctx := context.Background()
	tmp := t.TempDir()
	keyPath := filepath.Join(tmp, "signer")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", keyPath).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, string(out))
	}
	pubRaw, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		t.Fatal(err)
	}

	origin := filepath.Join(tmp, "origin.git")
	devDir := filepath.Join(tmp, "dev")
	ciDir := filepath.Join(tmp, "ci")
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	write := func(dir, rel, body string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	runGit(tmp, "init", "--bare", "-q", "-b", "main", origin)
	runGit(tmp, "init", "-q", "-b", "main", devDir)
	runGit(devDir, "config", "user.name", "alice")
	runGit(devDir, "config", "user.email", "alice@example.com")
	runGit(devDir, "remote", "add", "origin", origin)

	write(devDir, ".quai/allowed_signers", "alice@example.com "+string(bytes.TrimSpace(pubRaw))+"\n")
	write(devDir, ".quai.toml", `
version = 1
base = "main"

[[steps]]
name = "fast"
run = "true"
inputs = ["app/**"]

[attest]
allowed_signers = ".quai/allowed_signers"
signers = ["alice@example.com"]
max_age = "14d"
`)
	write(devDir, "app/main.txt", "v1\n")
	write(devDir, "README.md", "v1\n")
	runGit(devDir, "add", "-A")
	runGit(devDir, "commit", "-qm", "init")
	runGit(devDir, "push", "-q", "-u", "origin", "main")

	runGit(devDir, "checkout", "-q", "-b", "feat/signed")
	write(devDir, "app/main.txt", "v2\n")
	runGit(devDir, "add", "app/main.txt")
	runGit(devDir, "commit", "-qm", "change app")
	runGit(devDir, "push", "-q", "-u", "origin", "feat/signed")

	// Sign and push attestation via quai verify --sign --key ... --push.
	var out, errBuf bytes.Buffer
	var code int
	if detached {
		started := detachRun(t, devDir, "verify", "--sign", "--key", keyPath, "--push")
		code, out, errBuf = runCLIBuffers(t, devDir, "verify", "--wait", started.RunID, "--max", "60s", "--json")
	} else {
		code = run(ctx, []string{"verify", "--sign", "--key", keyPath, "--push", "--json"}, devDir, bytes.NewReader(nil), &out, &errBuf)
	}
	if code != 0 {
		t.Fatalf("verify --sign --push failed (%d): %s %s", code, out.String(), errBuf.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(`"pushed": true`)) || !bytes.Contains(out.Bytes(), []byte(`refs/quai/records/fast-`)) {
		t.Fatalf("expected pushed attestation in verify json: %s", out.String())
	}

	// Unrelated docs commit lands on main.
	runGit(devDir, "checkout", "-q", "main")
	write(devDir, "README.md", "v2\n")
	runGit(devDir, "add", "README.md")
	runGit(devDir, "commit", "-qm", "docs on main")
	runGit(devDir, "push", "-q", "origin", "main")

	// Fresh CI clone verifies signed attestation against virtual merge-tree --onto origin/main.
	runGit(tmp, "clone", "-q", "-b", "feat/signed", origin, ciDir)
	out.Reset()
	errBuf.Reset()
	code = run(ctx, []string{"attest", "check", "--fetch", "--onto", "origin/main", "--json"}, ciDir, bytes.NewReader(nil), &out, &errBuf)
	if code != 0 {
		t.Fatalf("attest check failed (%d): %s %s", code, out.String(), errBuf.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(`"verified"`)) || !bytes.Contains(out.Bytes(), []byte(`alice@example.com`)) {
		t.Fatalf("unexpected attest check output: %s", out.String())
	}

	// Probabilistic audit --sample 100 exits 1 with status "sampled".
	out.Reset()
	errBuf.Reset()
	code = run(ctx, []string{"attest", "check", "--onto", "origin/main", "--sample", "100", "--json"}, ciDir, bytes.NewReader(nil), &out, &errBuf)
	if code != 1 || !bytes.Contains(out.Bytes(), []byte(`"sampled"`)) {
		t.Fatalf("expected exit 1 sampled, got code=%d out=%s err=%s", code, out.String(), errBuf.String())
	}
}
