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

func initCLITestRepo(t *testing.T) string {
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
	write(".quai.toml", `
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
`)
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
		Tier     string                       `json:"tier"`
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

	// 6. Unknown command -> exit 2.
	out.Reset()
	errBuf.Reset()
	if code := run(ctx, []string{"nope"}, dir, bytes.NewReader(nil), &out, &errBuf); code != 2 {
		t.Fatalf("expected exit 2 on unknown command, got %d", code)
	}
}
