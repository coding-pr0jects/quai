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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/detach"
	"github.com/coding-pr0jects/quai/internal/execrun"
)

// A detached run re-executes os.Executable(), which under go test is this test binary.
func TestMain(m *testing.M) {
	if _, ok := os.LookupEnv(detach.EnvRunID); ok {
		main()
	}
	os.Exit(m.Run())
}

type runReport struct {
	RunID         string   `json:"run_id"`
	PID           int      `json:"pid"`
	State         string   `json:"state"`
	Output        string   `json:"output"`
	ReleasedLocks []string `json:"released_locks"`
	KilledGroups  []int    `json:"killed_groups"`
	Exit          *struct {
		ExitCode int  `json:"exit_code"`
		Stopped  bool `json:"stopped"`
		Result   *struct {
			OK             bool `json:"ok"`
			ReceiptWritten bool `json:"receipt_written"`
			Pushed         bool `json:"pushed"`
			Steps          []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"steps"`
		} `json:"result"`
	} `json:"exit"`
}

func runCLI(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	code, out, errBuf := runCLIBuffers(t, dir, args...)
	return code, out.String(), errBuf.String()
}

func runCLIBuffers(t *testing.T, dir string, args ...string) (int, bytes.Buffer, bytes.Buffer) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := run(context.Background(), args, dir, bytes.NewReader(nil), &out, &errBuf)
	return code, out, errBuf
}

func runCLIJSON(t *testing.T, dir string, args ...string) (int, runReport) {
	t.Helper()
	code, out, stderr := runCLI(t, dir, args...)
	var rep runReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("quai %v exited %d with non-JSON output %q (stderr %q)", args, code, out, stderr)
	}
	return code, rep
}

func detachRun(t *testing.T, dir string, args ...string) runReport {
	t.Helper()
	code, rep := runCLIJSON(t, dir, append(args, "--detach", "--json")...)
	if code != 0 || rep.RunID == "" || rep.PID <= 0 || rep.State != "running" {
		t.Fatalf("quai %v --detach exited %d: %+v", args, code, rep)
	}
	reapAtCleanup(t, rep.PID)
	return rep
}

// This test process stays the parent of every detached run it starts, so it must reap them.
func reapAtCleanup(t *testing.T, pid int) {
	t.Cleanup(func() {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &ws, 0, nil)
	})
}

func readPIDFile(t *testing.T, path string) execrun.Process {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(raw), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			p, ok := execrun.FindProcess(pid)
			if !ok {
				t.Fatalf("step process %d is not running", pid)
			}
			return p
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the step never wrote %s", path)
	return execrun.Process{}
}

func assertGone(t *testing.T, p execrun.Process) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.Alive() {
		if time.Now().After(deadline) {
			t.Fatalf("step process %d outlived its detached run", p.PID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func lockNames(t *testing.T, dir string) []string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(strings.TrimSpace(string(out)), "quai", "locks"))
	var names []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") {
			names = append(names, e.Name())
		}
	}
	return names
}

func longStepConfig(pidFile string) string {
	return fmt.Sprintf(`
version = 1
base = "main"

[slots]
heavy = 1

[[steps]]
name = "fast"
run = "true"

[[steps]]
name = "verify"
run = "sleep 300 & echo $! > %s; wait"
pool = "heavy"
slots = 1
`, pidFile)
}

func TestCLIDetachedVerifyPassesLikeForeground(t *testing.T) {
	dir := initCLITestRepo(t)

	code, out, stderr := runCLI(t, dir, "verify", "--detach")
	m := regexp.MustCompile(`^Started quai run (\S+): quai verify --wait (\S+)\n$`).FindStringSubmatch(out)
	if code != 0 || m == nil || m[1] != m[2] {
		t.Fatalf("verify --detach = %d %q %q, want the started line", code, out, stderr)
	}

	code, rep := runCLIJSON(t, dir, "verify", "--wait", "--max", "60s", "--json")
	reapAtCleanup(t, rep.PID)
	if code != 0 || rep.RunID != m[1] || rep.State != "passed" || rep.Exit == nil || rep.Exit.Result == nil {
		t.Fatalf("verify --wait = %d %+v, want the latest run of this worktree passed", code, rep)
	}
	if res := rep.Exit.Result; !res.OK || !res.ReceiptWritten || len(res.Steps) != 2 || res.Steps[1].Status != "passed" {
		t.Fatalf("detached verify result = %+v, want both steps passed with receipts", res)
	}
	if !strings.Contains(rep.Output, "pass    verify") {
		t.Fatalf("wait output %q lacks the step lines", rep.Output)
	}

	code, again := runCLIJSON(t, dir, "verify", "--wait", m[1], "--json")
	if code != 0 || again.Output != "" {
		t.Fatalf("second wait = %d, output %q; want 0 and no repeated output", code, again.Output)
	}

	code, out, _ = runCLI(t, dir, "verify", "--json")
	if code != 0 || strings.Count(out, `"status": "cached"`) != 2 {
		t.Fatalf("foreground verify after the detached run = %d %s, want both steps cached", code, out)
	}
	code, out, _ = runCLI(t, dir, "status", "--json")
	if code != 0 || strings.Contains(out, `"runs"`) {
		t.Fatalf("status lists a finished run: %s", out)
	}
}

func TestCLIDetachedVerifyFailingStepReturnsAtOnce(t *testing.T) {
	dir := initCLITestRepoWith(t, `
version = 1
base = "main"

[[steps]]
name = "fast"
run = "echo boom; exit 3"

[[steps]]
name = "verify"
run = "sleep 300"
`)
	detachRun(t, dir, "verify")

	start := time.Now()
	code, out, _ := runCLI(t, dir, "verify", "--wait", "--max", "2m")
	if code != 1 || !strings.Contains(out, "boom") || !strings.Contains(out, `step "fast" failed: exit status 3`) {
		t.Fatalf("verify --wait = %d %q, want 1 with the failure summary", code, out)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("verify --wait took %s on a failed run", elapsed)
	}
}

func TestCLIKilledDetachedRunIsDeadAndCleanedUp(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "step.pid")
	dir := initCLITestRepoWith(t, longStepConfig(pidFile))
	started := detachRun(t, dir, "verify")
	step := readPIDFile(t, pidFile)
	if code, rep := runCLIJSON(t, dir, "verify", "--wait", "--max", "0", "--json"); code != 4 || rep.State != "running" {
		t.Fatalf("verify --wait --max 0 = %d %+v, want 4 running", code, rep)
	}

	if err := syscall.Kill(started.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(started.PID, &ws, 0, nil)

	code, out, _ := runCLI(t, dir, "status", "--json")
	if code != 0 || !strings.Contains(out, `"state": "dead"`) {
		t.Fatalf("status = %s, want the dead run listed", out)
	}
	code, rep := runCLIJSON(t, dir, "verify", "--wait", "--json")
	if code != 1 || rep.State != "dead" || len(rep.KilledGroups) != 1 || len(rep.ReleasedLocks) != 2 {
		t.Fatalf("verify --wait = %d %+v, want 1 dead with its step group killed and its slot and in-flight locks released", code, rep)
	}
	assertGone(t, step)
	if locks := lockNames(t, dir); len(locks) != 0 {
		t.Fatalf("locks left behind: %v", locks)
	}
}

func TestCLIStopDetachedRun(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "step.pid")
	dir := initCLITestRepoWith(t, longStepConfig(pidFile))
	started := detachRun(t, dir, "verify")
	step := readPIDFile(t, pidFile)

	code, out, _ := runCLI(t, dir, "status")
	if code != 0 || !strings.Contains(out, "run "+started.RunID+": running") {
		t.Fatalf("status = %q, want the running run", out)
	}

	code, rep := runCLIJSON(t, dir, "verify", "--stop", started.RunID, "--json")
	if code != 0 || rep.State != "failed" || rep.Exit == nil || !rep.Exit.Stopped {
		t.Fatalf("verify --stop = %d %+v, want 0 and a stopped run", code, rep)
	}
	assertGone(t, step)
	if locks := lockNames(t, dir); len(locks) != 0 {
		t.Fatalf("locks left behind: %v", locks)
	}
	if code, _, _ := runCLI(t, dir, "verify", "--stop"); code != 0 {
		t.Fatalf("stopping a finished run exited %d, want 0", code)
	}
	if code, _, _ := runCLI(t, dir, "verify", "--wait", "--max", "0"); code != 1 {
		t.Fatalf("waiting on a stopped run exited %d, want 1", code)
	}
}

func TestCLIDetachedRunsInTwoWorktreesStayIsolated(t *testing.T) {
	dir := initCLITestRepoWith(t, `
version = 1
base = "main"

[[steps]]
name = "fast"
run = "for i in $(seq 1 200); do echo \"marker-$(cat who) $i\"; done"
`)
	gitIn := func(d string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", d}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(dir, "worktree", "add", "-q", "-b", "feat", wt)
	for d, who := range map[string]string{dir: "main", wt: "feat"} {
		if err := os.WriteFile(filepath.Join(d, "who"), []byte(who+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(d, "add", "who")
		gitIn(d, "commit", "-qm", "who")
	}

	var wg sync.WaitGroup
	for _, d := range []string{dir, wt} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			detachRun(t, d, "verify")
		}()
	}
	wg.Wait()

	for d, who := range map[string]string{dir: "main", wt: "feat"} {
		code, rep := runCLIJSON(t, d, "verify", "--wait", "--max", "60s", "--json")
		if code != 0 {
			t.Fatalf("wait in %s = %d %+v", who, code, rep)
		}
		lines := strings.Count(rep.Output, "marker-")
		if lines != 200 || strings.Count(rep.Output, "marker-"+who+" ") != 200 {
			t.Fatalf("worktree %s saw %d marker lines, want its own 200 only:\n%s", who, lines, rep.Output)
		}
	}
}

func TestCLIExecDetach(t *testing.T) {
	dir := initCLITestRepo(t)
	code, out, stderr := runCLI(t, dir, "exec", "--detach", "--pool", "heavy", "--", "sh", "-c", "echo exec-ok; exit 5")
	if code != 0 || !strings.HasPrefix(out, "Started quai run ") {
		t.Fatalf("exec --detach = %d %q %q", code, out, stderr)
	}
	code, rep := runCLIJSON(t, dir, "verify", "--wait", "--max", "60s", "--json")
	reapAtCleanup(t, rep.PID)
	if code != 1 || rep.State != "failed" || rep.Exit == nil || rep.Exit.ExitCode != 5 || rep.Output != "exec-ok\n" {
		t.Fatalf("wait on the exec run = %d %+v, want 1 with the command's exit code 5 and output", code, rep)
	}
}

func TestCLIRunControlFlagErrors(t *testing.T) {
	dir := initCLITestRepo(t)
	for _, args := range [][]string{
		{"verify", "--max", "1m"},
		{"verify", "--wait", "--step", "fast"},
		{"verify", "--wait", "--stop"},
		{"verify", "--detach", "--wait"},
		{"verify", "--stop", "--max", "1m"},
		{"verify", "--wait"},
		{"verify", "--wait", "20261122T100000Z-00000000"},
		{"verify", "--wait", "../locks"},
		{"verify", "--wait", "a", "b"},
	} {
		if code, out, stderr := runCLI(t, dir, args...); code != 2 {
			t.Errorf("quai %v = %d %q %q, want 2", args, code, out, stderr)
		}
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"verify", "--wait", "--priority", "5"}, "--priority does not apply to --wait or --stop: pass it with --detach"},
		{[]string{"verify", "--stop", "--priority", "5", "--no-cache"}, "--no-cache, --priority do not apply to --wait or --stop: pass them with --detach"},
	} {
		if code, _, stderr := runCLI(t, dir, tc.args...); code != 2 || !strings.Contains(stderr, tc.want) {
			t.Errorf("quai %v = %d %q, want 2 with %q", tc.args, code, stderr, tc.want)
		}
	}
}
