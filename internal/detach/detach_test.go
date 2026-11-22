package detach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/execrun"
)

func TestDecideStateAndExitCode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exit      *Exit
		probeable bool
		alive     bool
		want      State
		code      int
	}{
		{"exit 0 record", &Exit{ExitCode: 0}, true, false, Passed, 0},
		{"failing exit record wins over a live pid", &Exit{ExitCode: 1}, true, true, Failed, 1},
		{"stopped run", &Exit{ExitCode: 137, Stopped: true}, true, false, Failed, 1},
		{"live pid without record", nil, true, true, Running, 4},
		{"dead pid without record", nil, true, false, Dead, 1},
		{"other host or pid namespace is never declared dead", nil, false, false, Running, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := decideState(tc.exit, tc.probeable, tc.alive)
			if got != tc.want || ExitCode(got) != tc.code {
				t.Fatalf("decideState = %s (exit %d), want %s (exit %d)", got, ExitCode(got), tc.want, tc.code)
			}
		})
	}
}

func seedRun(t *testing.T, s *Store, id, worktree string, startedAt time.Time, proc execrun.Process) Meta {
	t.Helper()
	if err := os.MkdirAll(s.dir(id), 0o755); err != nil {
		t.Fatal(err)
	}
	m := Meta{Version: metaVersion, ID: id, Args: []string{"verify"}, Worktree: worktree, PID: proc.PID, StartTime: proc.StartTime, StartedAt: startedAt}
	if err := writeJSONAtomic(filepath.Join(s.dir(id), "meta.json"), m); err != nil {
		t.Fatal(err)
	}
	return m
}

func liveProcess(t *testing.T) execrun.Process {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	p, ok := execrun.FindProcess(cmd.Process.Pid)
	if !ok {
		t.Fatal("sleep is not running")
	}
	return p
}

func appendLog(t *testing.T, s *Store, id, text string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(s.dir(id), "output.log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func TestWaitStreamsOnlyNewCompleteLines(t *testing.T) {
	s := NewStore(t.TempDir())
	m := seedRun(t, s, "20261122T100000Z-00000001", "/wt", time.Now(), liveProcess(t))
	ctx := context.Background()
	wait := func(max time.Duration) (string, Report) {
		t.Helper()
		var out bytes.Buffer
		rep, err := s.Wait(ctx, m, max, &out)
		if err != nil {
			t.Fatal(err)
		}
		return out.String(), rep
	}

	appendLog(t, s, m.ID, "one\ntwo\npart")
	if out, rep := wait(0); out != "one\ntwo\n" || rep.State != Running {
		t.Fatalf("first wait = %q %+v, want the complete lines and running", out, rep)
	}
	if out, _ := wait(0); out != "" {
		t.Fatalf("second wait repeated output %q", out)
	}
	appendLog(t, s, m.ID, "ial\nthree\n")
	if out, _ := wait(0); out != "partial\nthree\n" {
		t.Fatalf("third wait = %q, want the completed line", out)
	}

	appendLog(t, s, m.ID, "tail without newline")
	r := &Recorder{dir: s.dir(m.ID)}
	r.SetResult(map[string]bool{"ok": false})
	if err := r.Finish(1); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	out, rep := wait(time.Minute)
	var result map[string]bool
	if rep.Exit != nil {
		_ = json.Unmarshal(rep.Exit.Result, &result)
	}
	if out != "tail without newline" || rep.State != Failed || rep.Exit.ExitCode != 1 || result == nil || result["ok"] {
		t.Fatalf("final wait = %q %+v, want the rest of the log and the failed exit record", out, rep)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("wait on a finished run must return at once")
	}
}

func TestResolvePicksTheLatestRunOfTheWorktree(t *testing.T) {
	s := NewStore(t.TempDir())
	proc := execrun.Process{PID: os.Getpid()}
	now := time.Now()
	seedRun(t, s, "20261122T100000Z-0000000a", "/wt-a", now, proc)
	latestA := seedRun(t, s, "20261122T100000Z-00000000", "/wt-a", now.Add(time.Second), proc)
	b := seedRun(t, s, "20261122T100001Z-0000000b", "/wt-b", now.Add(2*time.Second), proc)

	for _, tc := range []struct{ id, worktree, want string }{
		{"", "/wt-a", latestA.ID},
		{"", "/wt-b", b.ID},
		{b.ID, "/wt-a", b.ID},
	} {
		got, err := s.Resolve(tc.id, tc.worktree)
		if err != nil || got.ID != tc.want {
			t.Errorf("Resolve(%q, %q) = %s, %v; want %s", tc.id, tc.worktree, got.ID, err, tc.want)
		}
	}
	if _, err := s.Resolve("", "/wt-c"); !errors.Is(err, ErrNoRun) {
		t.Errorf("Resolve without runs = %v, want ErrNoRun", err)
	}
	if _, err := s.Resolve("../../locks", "/wt-a"); err == nil || errors.Is(err, ErrNoRun) {
		t.Errorf("Resolve(path) = %v, want an invalid id error", err)
	}
}

func TestPruneKeepsRunningRunsAndTheLatestFinished(t *testing.T) {
	s := NewStore(t.TempDir())
	live := liveProcess(t)
	base := time.Now()
	running := seedRun(t, s, "20261122T090000Z-00000000", "/wt", base, live)
	var finished []Meta
	for i := 1; i <= keepFinished+3; i++ {
		m := seedRun(t, s, fmt.Sprintf("20261122T1000%02dZ-00000000", i), "/wt", base.Add(time.Duration(i)*time.Second), live)
		if err := (&Recorder{dir: s.dir(m.ID)}).Finish(0); err != nil {
			t.Fatal(err)
		}
		finished = append(finished, m)
	}

	s.prune()

	if _, err := os.Stat(s.dir(running.ID)); err != nil {
		t.Fatalf("running run was pruned: %v", err)
	}
	for i, m := range finished {
		_, err := os.Stat(s.dir(m.ID))
		if kept := i >= 3; kept != (err == nil) {
			t.Errorf("finished run %d kept = %v, want %v", i, err == nil, kept)
		}
	}
}
