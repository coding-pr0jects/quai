package gc

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/attest"
	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/lock"
	"github.com/coding-pr0jects/quai/internal/receipt"
	"github.com/coding-pr0jects/quai/internal/repo"
	"github.com/coding-pr0jects/quai/internal/verdict"
)

func setupRepo(t *testing.T) (string, *repo.ExecGit, string) {
	t.Helper()
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "repo")
	origin := filepath.Join(tmp, "origin.git")
	runGit := func(workdir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = workdir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
	runGit(tmp, "init", "--bare", "-q", "-b", "main", origin)
	runGit(tmp, "init", "-q", "-b", "main", dir)
	runGit(dir, "config", "user.name", "t")
	runGit(dir, "config", "user.email", "t@example.com")
	runGit(dir, "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(dir, "add", "-A")
	runGit(dir, "commit", "-qm", "init")
	runGit(dir, "push", "-q", "-u", "origin", "main")
	runGit(dir, "remote", "set-head", "origin", "main")
	return dir, repo.NewExecGit(dir), origin
}

func baseConfigText(extra string) string {
	return `
version = 1
base = "main"

[[steps]]
name = "fast"
run = "true"

[attest]
max_age = "1h"
remote = "origin"
` + extra
}

func testConfig(t *testing.T, extra string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte(baseConfigText(extra)))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return cfg
}

func pushBaseConfig(t *testing.T, dir, extra string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".quai.toml"), []byte(baseConfigText(extra)), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", ".quai.toml"},
		{"commit", "-qm", "base config"},
		{"push", "-q", "origin", "main"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, string(out))
		}
	}
}

func writeReceipt(t *testing.T, commonDir, step, fingerprint string, created time.Time) string {
	t.Helper()
	dir := filepath.Join(commonDir, "quai", "receipts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(receipt.Receipt{
		Version:     receipt.CurrentVersion,
		Fingerprint: fingerprint,
		Step:        step,
		Passed:      true,
		CreatedAt:   created,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, step+"-"+fingerprint+".json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeRecordRef(t *testing.T, ctx context.Context, g *repo.ExecGit, step, fingerprint string, created time.Time) string {
	t.Helper()
	stmt := attest.Statement{
		Type:          attest.StatementType,
		PredicateType: attest.PredicateType,
		Predicate: attest.Predicate{
			Version:     receipt.CurrentVersion,
			Step:        step,
			Fingerprint: fingerprint,
			Passed:      true,
			CreatedAt:   created,
		},
	}
	payload, err := json.Marshal(stmt)
	if err != nil {
		t.Fatal(err)
	}
	env := attest.Envelope{
		PayloadType: attest.PayloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	ref := attest.RefName(step, fingerprint)
	if err := g.WriteRefBlob(ctx, ref, attest.BlobFileName, raw); err != nil {
		t.Fatal(err)
	}
	return ref
}

func writeVerdictFile(t *testing.T, commonDir, patchID string, created time.Time) string {
	t.Helper()
	dir := filepath.Join(commonDir, "quai", "verdicts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(verdict.Record{
		Version:   verdict.CurrentVersion,
		PatchID:   patchID,
		Tier:      config.TierDiffAudit,
		Status:    verdict.StatusApproved,
		CreatedAt: created,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, patchID+".json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeVerdictRef(t *testing.T, ctx context.Context, g *repo.ExecGit, patchID string, created time.Time) string {
	t.Helper()
	stmt := verdict.Statement{
		Type:          attest.StatementType,
		PredicateType: verdict.PredicateType,
		Predicate: verdict.Record{
			Version:   verdict.CurrentVersion,
			PatchID:   patchID,
			Tier:      config.TierDiffAudit,
			Status:    verdict.StatusApproved,
			CreatedAt: created,
		},
	}
	payload, err := json.Marshal(stmt)
	if err != nil {
		t.Fatal(err)
	}
	env := attest.Envelope{
		PayloadType: attest.PayloadType,
		Payload:     base64.StdEncoding.EncodeToString(payload),
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	ref := verdict.RefName(patchID)
	if err := g.WriteRefBlob(ctx, ref, verdict.BlobFileName, raw); err != nil {
		t.Fatal(err)
	}
	return ref
}

func writeStaleLock(t *testing.T, commonDir, name string) string {
	t.Helper()
	path := filepath.Join(commonDir, "quai", "locks", name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	owner := lock.Owner{PID: 0, Command: "dead", Worktree: "/tmp"}
	raw, err := json.Marshal(owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "owner"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunDryRunListsWithoutMutating(t *testing.T) {
	ctx := context.Background()
	_, g, _ := setupRepo(t)
	cfg := testConfig(t, "")
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

	expiredReceipt := writeReceipt(t, commonDir, "fast", strings.Repeat("a", 64), now.Add(-2*time.Hour))
	freshReceipt := writeReceipt(t, commonDir, "fast", strings.Repeat("b", 64), now.Add(-30*time.Minute))
	expiredRef := writeRecordRef(t, ctx, g, "fast", strings.Repeat("c", 64), now.Add(-2*time.Hour))
	freshRef := writeRecordRef(t, ctx, g, "fast", strings.Repeat("d", 64), now.Add(-30*time.Minute))
	if err := g.PushRefs(ctx, "origin", []string{expiredRef, freshRef}); err != nil {
		t.Fatal(err)
	}
	expiredVerdict := writeVerdictFile(t, commonDir, strings.Repeat("e", 40), now.Add(-2*time.Hour))
	freshVerdict := writeVerdictFile(t, commonDir, strings.Repeat("f", 40), now.Add(-30*time.Minute))
	expiredVerdictRef := writeVerdictRef(t, ctx, g, strings.Repeat("1", 40), now.Add(-2*time.Hour))
	freshVerdictRef := writeVerdictRef(t, ctx, g, strings.Repeat("2", 40), now.Add(-30*time.Minute))
	staleLock := writeStaleLock(t, commonDir, "land.lock")

	res, err := Run(ctx, g, cfg, Options{DryRun: true, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.DryRun || !res.OK {
		t.Fatalf("unexpected result header: %+v", res)
	}
	got := map[string]bool{}
	for _, item := range res.Removed {
		got[item.Kind+"|"+item.Name+"|"+item.Location] = true
	}
	want := []string{
		"receipt|" + expiredReceipt + "|local",
		"attestation_ref|" + expiredRef + "|local",
		"attestation_ref|" + expiredRef + "|remote",
		"verdict|" + expiredVerdict + "|local",
		"verdict_ref|" + expiredVerdictRef + "|local",
		"lock|" + staleLock + "|local",
	}
	for _, key := range want {
		if !got[key] {
			t.Errorf("dry run missing %q; got %+v", key, res.Removed)
		}
	}
	for _, path := range []string{expiredReceipt, freshReceipt, expiredVerdict, freshVerdict, staleLock} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("dry run mutated %s: %v", path, err)
		}
	}
	for _, ref := range []string{expiredRef, freshRef, expiredVerdictRef, freshVerdictRef} {
		blob := attest.BlobFileName
		if strings.HasPrefix(ref, verdict.RefPrefix) {
			blob = verdict.BlobFileName
		}
		if _, ok, err := g.ReadRefBlob(ctx, ref, blob); err != nil || !ok {
			t.Errorf("dry run removed ref %s (ok=%v err=%v)", ref, ok, err)
		}
	}
	remoteRefs, err := g.ListRemoteRefs(ctx, "origin", []string{attest.RefPrefix + "*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(remoteRefs) != 2 {
		t.Errorf("dry run mutated remote refs: %v", remoteRefs)
	}
	if _, err := os.Stat(StampPath(commonDir)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dry run wrote the last-prune stamp")
	}
}

func TestRunRemovesExpiredAndKeepsLiveLock(t *testing.T) {
	ctx := context.Background()
	_, g, _ := setupRepo(t)
	cfg := testConfig(t, "")
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

	expiredReceipt := writeReceipt(t, commonDir, "fast", strings.Repeat("a", 64), now.Add(-2*time.Hour))
	freshReceipt := writeReceipt(t, commonDir, "fast", strings.Repeat("b", 64), now.Add(-30*time.Minute))
	expiredRef := writeRecordRef(t, ctx, g, "fast", strings.Repeat("c", 64), now.Add(-2*time.Hour))
	freshRef := writeRecordRef(t, ctx, g, "fast", strings.Repeat("d", 64), now.Add(-30*time.Minute))
	if err := g.PushRefs(ctx, "origin", []string{expiredRef, freshRef}); err != nil {
		t.Fatal(err)
	}
	staleLock := writeStaleLock(t, commonDir, "land.lock")

	locksRoot := filepath.Join(commonDir, "quai", "locks")
	liveLease, err := lock.AcquireDir(ctx, filepath.Join(locksRoot, "slot-heavy-0.lock"), lock.Owner{Command: "live", Worktree: "/tmp"}, lock.Options{Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer liveLease.Release()

	res, err := Run(ctx, g, cfg, Options{Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Removed) == 0 {
		t.Fatalf("expected removals, got %+v", res)
	}
	for _, path := range []string{expiredReceipt, staleLock} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should be removed, stat err=%v", path, err)
		}
	}
	for _, path := range []string{freshReceipt, filepath.Join(locksRoot, "slot-heavy-0.lock")} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s should survive: %v", path, err)
		}
	}
	if _, ok, err := g.ReadRefBlob(ctx, expiredRef, attest.BlobFileName); err != nil || ok {
		t.Errorf("expired local ref %s should be removed (ok=%v err=%v)", expiredRef, ok, err)
	}
	if _, ok, err := g.ReadRefBlob(ctx, freshRef, attest.BlobFileName); err != nil || !ok {
		t.Errorf("fresh ref %s should survive (ok=%v err=%v)", freshRef, ok, err)
	}
	remoteRefs, err := g.ListRemoteRefs(ctx, "origin", []string{attest.RefPrefix + "*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(remoteRefs) != 1 || remoteRefs[0] != freshRef {
		t.Errorf("remote refs = %v, want [%s]", remoteRefs, freshRef)
	}
}

func TestMaybeAutoRateLimitAndLocalPrune(t *testing.T) {
	ctx := context.Background()
	dir, g, _ := setupRepo(t)
	cfg := testConfig(t, "auto_prune = true\nprune_interval = \"1h\"\n")
	pushBaseConfig(t, dir, "auto_prune = true\nprune_interval = \"1h\"\n")
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}

	first := writeReceipt(t, commonDir, "fast", strings.Repeat("a", 64), time.Now().Add(-2*time.Hour))
	record := writeRecordRef(t, ctx, g, "fast", strings.Repeat("e", 64), time.Now().Add(-2*time.Hour))
	if err := g.PushRefs(ctx, "origin", []string{record}); err != nil {
		t.Fatal(err)
	}
	MaybeAuto(ctx, g, cfg, io.Discard)
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("first auto prune should remove %s, stat err=%v", first, err)
	}
	if _, ok, err := g.ReadRefBlob(ctx, record, attest.BlobFileName); err != nil || ok {
		t.Fatalf("auto prune should remove local record %s (ok=%v err=%v)", record, ok, err)
	}
	remote, err := g.ListRemoteRefs(ctx, "origin", []string{attest.RefPrefix + "*"})
	if err != nil {
		t.Fatal(err)
	}
	if len(remote) != 0 {
		t.Fatalf("auto prune should remove remote record %s, remote=%v", record, remote)
	}
	if _, ok := ReadStamp(commonDir); !ok {
		t.Fatal("auto prune did not write the last-prune stamp")
	}

	second := writeReceipt(t, commonDir, "fast", strings.Repeat("b", 64), time.Now().Add(-2*time.Hour))
	MaybeAuto(ctx, g, cfg, io.Discard)
	if _, err := os.Stat(second); err != nil {
		t.Fatalf("rate-limited auto prune removed %s: %v", second, err)
	}

	if err := WriteStamp(commonDir, time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	MaybeAuto(ctx, g, cfg, io.Discard)
	if _, err := os.Stat(second); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired stamp should allow prune of %s, stat err=%v", second, err)
	}
}

func TestMaybeAutoWarnsOnRemoteFailureAndStillPrunesLocal(t *testing.T) {
	ctx := context.Background()
	dir, g, _ := setupRepo(t)
	cfg := testConfig(t, "auto_prune = true\n")
	pushBaseConfig(t, dir, "auto_prune = true\n")
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "remote", "set-url", "origin", filepath.Join(dir, "missing-origin.git"))
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set-url: %v\n%s", err, string(out))
	}

	expired := writeReceipt(t, commonDir, "fast", strings.Repeat("a", 64), time.Now().Add(-2*time.Hour))
	var stderr bytes.Buffer
	MaybeAuto(ctx, g, cfg, &stderr)
	if _, err := os.Stat(expired); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("local receipt should be pruned despite remote failure, stat err=%v", err)
	}
	if !strings.Contains(stderr.String(), "auto-prune") {
		t.Fatalf("expected auto-prune warning, got %q", stderr.String())
	}
	if _, ok := ReadStamp(commonDir); !ok {
		t.Fatal("failed remote prune should still stamp the interval")
	}
}

func TestMaybeAutoIgnoresBranchOnlyAutoPrune(t *testing.T) {
	ctx := context.Background()
	dir, g, _ := setupRepo(t)
	writeBranchCfg := baseConfigText("auto_prune = true\nprune_interval = \"1h\"\n")
	if err := os.WriteFile(filepath.Join(dir, ".quai.toml"), []byte(writeBranchCfg), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, "auto_prune = true\n")
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expired := writeReceipt(t, commonDir, "fast", strings.Repeat("a", 64), time.Now().Add(-2*time.Hour))
	MaybeAuto(ctx, g, cfg, io.Discard)
	if _, err := os.Stat(expired); err != nil {
		t.Fatalf("working-tree auto_prune must not govern automatic pruning: %v", err)
	}
}

func TestMaybeAutoRejectsUntrustedBaseRef(t *testing.T) {
	ctx := context.Background()
	dir, g, _ := setupRepo(t)
	pushBaseConfig(t, dir, "auto_prune = true\nprune_interval = \"1h\"\n")
	cmd := exec.Command("git", "push", "-q", "origin", "main:refs/heads/feat/evil")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("push feat/evil: %v\n%s", err, string(out))
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		base string
	}{
		{"at", "@"},
		{"head", "HEAD"},
		{"own branch", "feat/evil"},
		{"qualified", "origin/main"},
		{"upstream", "main@{upstream}"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := config.Parse([]byte(fmt.Sprintf(`
version = 1
base = %q

[[steps]]
name = "fast"
run = "true"

[attest]
max_age = "1h"
auto_prune = true
`, tc.base)))
			if err != nil {
				t.Fatalf("parse config: %v", err)
			}
			fingerprint := fmt.Sprintf("%064d", i)
			expired := writeReceipt(t, commonDir, "fast", fingerprint, time.Now().Add(-2*time.Hour))
			var stderr bytes.Buffer
			MaybeAuto(ctx, g, cfg, &stderr)
			if _, err := os.Stat(expired); err != nil {
				t.Fatalf("untrusted base %q must skip pruning: %v", tc.base, err)
			}
			if !strings.Contains(stderr.String(), "auto-prune") || !strings.Contains(stderr.String(), "skipped") {
				t.Fatalf("untrusted base %q should warn, got %q", tc.base, stderr.String())
			}
		})
	}
}

func TestMaybeAutoWarnsToSetOriginHead(t *testing.T) {
	ctx := context.Background()
	dir, g, _ := setupRepo(t)
	cfg := testConfig(t, "auto_prune = true\n")
	pushBaseConfig(t, dir, "auto_prune = true\n")
	cmd := exec.Command("git", "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("delete origin/HEAD: %v\n%s", err, string(out))
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		t.Fatal(err)
	}
	expired := writeReceipt(t, commonDir, "fast", strings.Repeat("a", 64), time.Now().Add(-2*time.Hour))
	var stderr bytes.Buffer
	MaybeAuto(ctx, g, cfg, &stderr)
	if _, err := os.Stat(expired); err != nil {
		t.Fatalf("unset origin/HEAD must skip pruning: %v", err)
	}
	if !strings.Contains(stderr.String(), "git remote set-head origin --auto") {
		t.Fatalf("warning must name the fix, got %q", stderr.String())
	}
	cmd = exec.Command("git", "remote", "set-head", "origin", "main")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("set-head: %v\n%s", err, string(out))
	}
	MaybeAuto(ctx, g, cfg, io.Discard)
	if _, err := os.Stat(expired); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("after set-head auto prune should remove %s, stat err=%v", expired, err)
	}
}

func TestDue(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		last time.Time
		ok   bool
		want bool
	}{
		{"missing", time.Time{}, false, true},
		{"fresh", now.Add(-time.Hour), true, false},
		{"old", now.Add(-25 * time.Hour), true, true},
		{"future", now.Add(time.Hour), true, false},
	}
	for _, tc := range cases {
		if got := Due(tc.last, tc.ok, now, 24*time.Hour); got != tc.want {
			t.Errorf("%s: Due = %v, want %v", tc.name, got, tc.want)
		}
	}
}
