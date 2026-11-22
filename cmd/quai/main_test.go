package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/exitkind"
	"github.com/coding-pr0jects/quai/internal/land"
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
		OK             bool   `json:"ok"`
		ExitKind       string `json:"exit_kind"`
		ReceiptWritten bool   `json:"receipt_written"`
	}
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || !v.OK || !v.ReceiptWritten || v.ExitKind != "pass" {
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

	out.Reset()
	code = run(ctx, []string{"exec", "--pool", "heavy", "--priority", "5", "--", "true"}, dir, bytes.NewReader(nil), &out, &errBuf)
	if code != 0 {
		t.Fatalf("exec --priority 5 exited %d: %s", code, errBuf.String())
	}
	for _, args := range [][]string{
		{"exec", "--pool", "heavy", "--priority", "1001", "--", "true"},
		{"verify", "--priority", "-1001"},
	} {
		errBuf.Reset()
		if code := run(ctx, args, dir, bytes.NewReader(nil), &out, &errBuf); code != 2 || !bytes.Contains(errBuf.Bytes(), []byte("out of range")) {
			t.Fatalf("%v: expected exit 2 with out of range, got %d: %s", args, code, errBuf.String())
		}
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
	if !bytes.Contains(out.Bytes(), []byte(`"receipt_count": 2`)) || !bytes.Contains(out.Bytes(), []byte(`"available_disk_bytes"`)) {
		t.Fatalf("expected 2 cached receipts and free disk in status json, got: %s", out.String())
	}
	out.Reset()
	errBuf.Reset()
	if code := run(ctx, []string{"doctor", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 0 {
		t.Fatalf("doctor failed: %d %s %s", code, out.String(), errBuf.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(`"ok": true`)) || !bytes.Contains(out.Bytes(), []byte(`"available_disk_bytes"`)) || !bytes.Contains(out.Bytes(), []byte(`"available_mem_bytes"`)) {
		t.Fatalf("expected ok doctor json with free memory and disk, got: %s", out.String())
	}

	// 6b. Human verify output names the exit kind.
	out.Reset()
	errBuf.Reset()
	if code := run(ctx, []string{"verify", "--step", "fast"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 0 {
		t.Fatalf("verify human run failed: %d %s %s", code, out.String(), errBuf.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("exit_kind: pass")) {
		t.Fatalf("expected exit_kind in human output, got: %s", out.String())
	}

	// 7. Unknown command -> exit 2.
	out.Reset()
	errBuf.Reset()
	if code := run(ctx, []string{"nope"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 2 {
		t.Fatalf("expected exit 2 on unknown command, got %d", code)
	}

	for _, args := range [][]string{{"land"}, {"land", "7", "#7"}, {"land", "7", "x"}} {
		errBuf.Reset()
		if code := run(ctx, args, dir, bytes.NewReader(nil), &out, &errBuf); code != 2 {
			t.Fatalf("%v: expected exit 2, got %d (%s)", args, code, errBuf.String())
		}
	}
}

func TestPrintBatchHumanIncludesExitKindPerPR(t *testing.T) {
	res := land.BatchResult{ExitKind: exitkind.StepFailed, PRs: []land.BatchEntry{
		{Result: land.Result{ExitKind: exitkind.StepFailed, PR: 102, Action: land.ActionOptimisticMerge, Reason: "boom"}, Status: land.BatchFailed},
		{Result: land.Result{ExitKind: exitkind.Pass, PR: 103, Action: land.ActionDirectMerge, Reason: "ok"}, Status: land.BatchLanded},
	}}
	var buf bytes.Buffer
	printBatchHuman(&buf, res)
	want := "#102: failed optimistic-merge (boom)\nexit_kind: step_failed\n" +
		"#103: landed direct-merge (ok)\nexit_kind: pass\n"
	if buf.String() != want {
		t.Fatalf("printBatchHuman =\n%q\nwant\n%q", buf.String(), want)
	}
}

func TestCLILandHumanOutputOnForgeFailurePrintsExitKind(t *testing.T) {
	ctx := context.Background()
	dir := initCLITestRepo(t)
	fakeBin := t.TempDir()
	if err := os.WriteFile(filepath.Join(fakeBin, "gh"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out, errBuf bytes.Buffer
	code := run(ctx, []string{"land", "1", "--dry-run"}, dir, bytes.NewReader(nil), &out, &errBuf)
	if code != 1 {
		t.Fatalf("land exited %d, want 1: stdout=%s stderr=%s", code, out.String(), errBuf.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("exit_kind: infra_error")) {
		t.Fatalf("expected exit_kind in land human output, got: %s", out.String())
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

	out.Reset()
	errBuf.Reset()
	code = run(ctx, []string{"attest", "check", "--onto", "origin/main"}, ciDir, bytes.NewReader(nil), &out, &errBuf)
	if line := out.String(); code != 0 || !regexp.MustCompile(`^verified fast \([0-9a-f]{12} by alice@example\.com, `).MatchString(line) {
		t.Fatalf("human attest check: code=%d out=%q err=%s", code, line, errBuf.String())
	}

	// Probabilistic audit --sample 100 exits 1 with status "sampled".
	out.Reset()
	errBuf.Reset()
	code = run(ctx, []string{"attest", "check", "--onto", "origin/main", "--sample", "100", "--json"}, ciDir, bytes.NewReader(nil), &out, &errBuf)
	if code != 1 || !bytes.Contains(out.Bytes(), []byte(`"sampled"`)) {
		t.Fatalf("expected exit 1 sampled, got code=%d out=%s err=%s", code, out.String(), errBuf.String())
	}
}

func TestCLIGCAndAutoPrune(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin.git")
	dir := filepath.Join(tmp, "dev")
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
	runGitIn := func(workdir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = workdir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	runGitIn(tmp, "init", "--bare", "-q", "-b", "main", origin)
	runGitIn(tmp, "init", "-q", "-b", "main", dir)
	runGit("config", "user.name", "t")
	runGit("config", "user.email", "t@example.com")
	runGit("remote", "add", "origin", origin)
	write(".quai.toml", `
version = 1
base = "main"

[[steps]]
name = "fast"
run = "true"
inputs = ["app/**"]

[attest]
max_age = "1h"
auto_prune = true
prune_interval = "1h"
`)
	write("app/main.txt", "v1\n")
	runGit("add", "-A")
	runGit("commit", "-qm", "init")
	runGit("push", "-q", "-u", "origin", "main")
	runGit("remote", "set-head", "origin", "main")

	commonOut, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	commonDir := string(bytes.TrimSpace(commonOut))
	receiptsDir := filepath.Join(commonDir, "quai", "receipts")
	if err := os.MkdirAll(receiptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeReceipt := func(fingerprint string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{
			"version":     1,
			"fingerprint": fingerprint,
			"step":        "fast",
			"passed":      true,
			"created_at":  time.Now().Add(-2 * time.Hour).UTC(),
		})
		path := filepath.Join(receiptsDir, "fast-"+fingerprint+".json")
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	expired := writeReceipt("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	locksRoot := filepath.Join(commonDir, "quai", "locks")
	staleLock := filepath.Join(locksRoot, "land.lock")
	if err := os.MkdirAll(staleLock, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleLock, "owner"), []byte(`{"pid":0,"command":"dead"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := run(ctx, []string{"gc", "--dry-run", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf)
	if code != 0 {
		t.Fatalf("gc --dry-run exited %d: %s %s", code, out.String(), errBuf.String())
	}
	var dry struct {
		DryRun  bool `json:"dry_run"`
		Removed []struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"removed"`
	}
	if err := json.Unmarshal(out.Bytes(), &dry); err != nil {
		t.Fatalf("gc --dry-run json: %v: %s", err, out.String())
	}
	if !dry.DryRun || len(dry.Removed) < 2 {
		t.Fatalf("expected dry-run removals, got %s", out.String())
	}
	if _, err := os.Stat(expired); err != nil {
		t.Fatalf("dry run removed %s: %v", expired, err)
	}
	if _, err := os.Stat(staleLock); err != nil {
		t.Fatalf("dry run removed lock %s: %v", staleLock, err)
	}
	if _, err := os.Stat(filepath.Join(commonDir, "quai", "last-prune")); !os.IsNotExist(err) {
		t.Fatal("dry run wrote last-prune stamp")
	}

	out.Reset()
	errBuf.Reset()
	if code := run(ctx, []string{"gc", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 0 {
		t.Fatalf("gc exited %d: %s %s", code, out.String(), errBuf.String())
	}
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("gc did not remove %s", expired)
	}
	if _, err := os.Stat(staleLock); !os.IsNotExist(err) {
		t.Fatalf("gc did not remove %s", staleLock)
	}
	if _, err := os.Stat(filepath.Join(commonDir, "quai", "last-prune")); err != nil {
		t.Fatalf("gc did not write last-prune stamp: %v", err)
	}

	out.Reset()
	errBuf.Reset()
	if code := run(ctx, []string{"status", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 0 {
		t.Fatalf("status exited %d: %s %s", code, out.String(), errBuf.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(`"auto_prune": true`)) || !bytes.Contains(out.Bytes(), []byte(`"last_prune"`)) {
		t.Fatalf("status missing prune state: %s", out.String())
	}

	// A remote failure during automatic pruning warns but never fails verify.
	runGit("remote", "set-url", "origin", filepath.Join(tmp, "missing.git"))
	if err := os.WriteFile(filepath.Join(commonDir, "quai", "last-prune"), []byte(time.Now().Add(-2*time.Hour).UTC().Format(time.RFC3339Nano)), 0o644); err != nil {
		t.Fatal(err)
	}
	second := writeReceipt("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	out.Reset()
	errBuf.Reset()
	if code := run(ctx, []string{"verify", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 0 {
		t.Fatalf("verify with failing remote prune exited %d: %s %s", code, out.String(), errBuf.String())
	}
	if _, err := os.Stat(second); !os.IsNotExist(err) {
		t.Fatalf("auto prune did not remove local receipt %s", second)
	}
	if !bytes.Contains(errBuf.Bytes(), []byte("auto-prune")) {
		t.Fatalf("expected auto-prune warning on stderr, got %q", errBuf.String())
	}

	// A fresh stamp rate-limits the next command.
	third := writeReceipt("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")
	out.Reset()
	errBuf.Reset()
	if code := run(ctx, []string{"verify", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 0 {
		t.Fatalf("second verify exited %d: %s %s", code, out.String(), errBuf.String())
	}
	if _, err := os.Stat(third); err != nil {
		t.Fatalf("rate-limited auto prune removed %s: %v", third, err)
	}
}

func TestCLIRejectsOptionInjectionInConfig(t *testing.T) {
	ctx := context.Background()
	cases := map[string]string{
		"base": `version = 1
base = "--upload-pack=touch %s;true"

[[steps]]
name = "fast"
run = "true"
`,
		"attest remote": `version = 1
base = "main"

[[steps]]
name = "fast"
run = "true"

[attest]
signers = ["someone@example.com"]
remote = "--upload-pack=touch %s;true"
`,
	}
	for name, tmpl := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(t.TempDir(), "pwned")
			runGit := func(args ...string) {
				t.Helper()
				cmd := exec.Command("git", args...)
				cmd.Dir = dir
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, string(out))
				}
			}
			runGit("init", "-q", "-b", "main")
			runGit("config", "user.name", "t")
			runGit("config", "user.email", "t@example.com")
			if err := os.WriteFile(filepath.Join(dir, ".quai.toml"), []byte(fmt.Sprintf(tmpl, marker)), 0o644); err != nil {
				t.Fatal(err)
			}
			runGit("add", "-A")
			runGit("commit", "-qm", "init")

			var out, errBuf bytes.Buffer
			if code := run(ctx, []string{"verify", "--fetch-base", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 2 {
				t.Fatalf("verify --fetch-base exited %d, want 2: %s %s", code, out.String(), errBuf.String())
			}
			if !bytes.Contains(errBuf.Bytes(), []byte("must not start with")) {
				t.Fatalf("verify error does not say what to fix: %s", errBuf.String())
			}

			out.Reset()
			errBuf.Reset()
			if code := run(ctx, []string{"doctor", "--json"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 1 {
				t.Fatalf("doctor exited %d, want 1 for invalid config: %s %s", code, out.String(), errBuf.String())
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("option injection executed: marker %s exists", marker)
			}
		})
	}
}
