package execrun

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestParseStat(t *testing.T) {
	tail := " 1 1 1 0 -1 4194560 100 0 0 0 0 0 0 0 20 0 1 0 424242 1000 10"
	for _, tc := range []struct {
		name  string
		line  string
		state byte
		start uint64
		ok    bool
	}{
		{"plain", "123 (sleep) S" + tail, 'S', 424242, true},
		{"name with spaces and parens", "123 (a) b (c)) Z" + tail, 'Z', 424242, true},
		{"truncated", "123 (sleep) S 1 1", 0, 0, false},
		{"no command name", "garbage", 0, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, start, ok := parseStat(tc.line)
			if state != tc.state || start != tc.start || ok != tc.ok {
				t.Fatalf("parseStat = (%q, %d, %v), want (%q, %d, %v)", state, start, ok, tc.state, tc.start, tc.ok)
			}
		})
	}
}

func TestProcessAliveRejectsAReusedPID(t *testing.T) {
	self, ok := FindProcess(os.Getpid())
	if !ok || !self.Alive() {
		t.Fatalf("FindProcess(self) = %+v, %v; want alive", self, ok)
	}
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("PID reuse detection needs procfs")
	}
	reused := Process{PID: self.PID, StartTime: self.StartTime + 1}
	if reused.Alive() {
		t.Fatal("a process with another start time must not count as alive")
	}
}

func TestDetachThenStopGroupKillsTheWholeGroup(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")
	logPath := filepath.Join(dir, "output.log")
	p, err := Detach(dir, []string{"sh", "-c", "trap '' TERM; sleep 60 & echo $! > " + pidFile + "; echo started; wait"}, os.Environ(), logPath)
	if err != nil {
		t.Fatal(err)
	}
	grandchild := waitForPID(t, pidFile)
	if !p.Alive() {
		t.Fatal("detached process should be alive")
	}

	p.StopGroup(300 * time.Millisecond)
	var ws syscall.WaitStatus
	_, _ = syscall.Wait4(p.PID, &ws, 0, nil)

	if p.Alive() {
		t.Fatal("detached process survived StopGroup")
	}
	waitGone(t, grandchild)
	if log, _ := os.ReadFile(logPath); !strings.Contains(string(log), "started") {
		t.Fatalf("output.log = %q, want the detached output", log)
	}
}

func waitForPID(t *testing.T, path string) Process {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(path); err == nil && strings.HasSuffix(string(raw), "\n") {
			pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			p, ok := FindProcess(pid)
			if !ok {
				t.Fatalf("pid %d is not running", pid)
			}
			return p
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("no pid written to %s", path)
	return Process{}
}

func waitGone(t *testing.T, p Process) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for p.Alive() {
		if time.Now().After(deadline) {
			t.Fatalf("pid %d survived the group kill", p.PID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
