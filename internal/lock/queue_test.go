package lock

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if os.Getenv("QUAI_LOCK_TEST_WAITER") == "1" {
		os.Exit(runSlotWaiter())
	}
	os.Exit(m.Run())
}

func runSlotWaiter() int {
	priority, err := strconv.Atoi(os.Getenv("QUAI_LOCK_TEST_PRIORITY"))
	if err != nil {
		return 3
	}
	sl, err := AcquireSlots(context.Background(), os.Getenv("QUAI_LOCK_TEST_ROOT"), "heavy", 1, 1, Owner{
		Role:     Role(os.Getenv("QUAI_LOCK_TEST_ROLE")),
		Priority: priority,
		Command:  os.Getenv("QUAI_LOCK_TEST_LABEL"),
	}, Options{Wait: 20 * time.Second, PollInterval: 10 * time.Millisecond})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 4
	}
	defer sl.Release()
	f, err := os.OpenFile(os.Getenv("QUAI_LOCK_TEST_LOG"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 5
	}
	fmt.Fprintln(f, os.Getenv("QUAI_LOCK_TEST_LABEL"))
	_ = f.Close()
	time.Sleep(30 * time.Millisecond)
	return 0
}

func TestEffectivePriority(t *testing.T) {
	cases := []struct {
		priority int
		waited   time.Duration
		want     int
	}{
		{0, 0, 0},
		{0, AgingInterval - time.Nanosecond, 0},
		{0, AgingInterval, 1},
		{0, 5 * time.Minute, MaxAgingBoost},
		{0, 24 * time.Hour, MaxAgingBoost},
		{10, -time.Minute, 10},
		{-5, 3 * AgingInterval, -2},
	}
	for _, tc := range cases {
		if got := EffectivePriority(tc.priority, tc.waited); got != tc.want {
			t.Errorf("EffectivePriority(%d, %s) = %d, want %d", tc.priority, tc.waited, got, tc.want)
		}
	}
}

func TestSortWaiters(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	at := func(name string, priority int, ago time.Duration) waiter {
		return waiter{name: name, enqueuedAt: now.Add(-ago), owner: Owner{Priority: priority}}
	}
	cases := []struct {
		name string
		in   []waiter
		want []string
	}{
		{
			name: "equal priority keeps FIFO",
			in:   []waiter{at("c", 0, time.Second), at("a", 0, 3*time.Second), at("b", 0, 2*time.Second)},
			want: []string{"a", "b", "c"},
		},
		{
			name: "land passes older verify waiters",
			in:   []waiter{at("verify-1", 0, 20*time.Second), at("land", 10, time.Second), at("verify-2", 0, 10*time.Second)},
			want: []string{"land", "verify-1", "verify-2"},
		},
		{
			name: "aging lifts a verify waiter past a lower priority",
			in:   []waiter{at("exec", 3, time.Second), at("verify", 0, 4*AgingInterval)},
			want: []string{"verify", "exec"},
		},
		{
			name: "fully aged verify ties land and wins by enqueue time",
			in:   []waiter{at("land", 10, time.Second), at("verify", 0, 10*AgingInterval)},
			want: []string{"verify", "land"},
		},
		{
			name: "capped aging never passes a higher priority",
			in:   []waiter{at("land", 11, time.Second), at("verify", 0, time.Hour)},
			want: []string{"land", "verify"},
		},
		{
			name: "same enqueue time falls back to ticket name",
			in:   []waiter{at("y", 0, time.Second), at("x", 0, time.Second)},
			want: []string{"x", "y"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sortWaiters(tc.in, now)
			var got []string
			for _, w := range tc.in {
				got = append(got, w.name)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("order = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSlotQueueOrderAcrossProcesses(t *testing.T) {
	type spec struct {
		label    string
		role     Role
		priority int
	}
	cases := []struct {
		name    string
		waiters []spec
		want    []string
	}{
		{
			name:    "land preempts a queued verify",
			waiters: []spec{{"verify-2", RoleVerify, 0}, {"land", RoleLand, 10}},
			want:    []string{"land", "verify-2"},
		},
		{
			name:    "equal priority keeps FIFO",
			waiters: []spec{{"verify-1", RoleVerify, 0}, {"exec-2", RoleExec, 0}, {"verify-3", RoleVerify, 0}},
			want:    []string{"verify-1", "exec-2", "verify-3"},
		},
		{
			name: "priority first, FIFO within a priority",
			waiters: []spec{
				{"verify-1", RoleVerify, 0}, {"land-2", RoleLand, 10}, {"exec-3", RoleExec, 0}, {"exec-4", RoleExec, 5},
			},
			want: []string{"land-2", "exec-4", "verify-1", "exec-3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			locksRoot := t.TempDir()
			logPath := filepath.Join(t.TempDir(), "order.log")
			holder, err := AcquireSlots(ctx, locksRoot, "heavy", 1, 1, Owner{Role: RoleVerify, Command: "holder"}, Options{Wait: time.Second})
			if err != nil {
				t.Fatalf("holder acquire: %v", err)
			}
			defer holder.Release()

			var cmds []*exec.Cmd
			for i, w := range tc.waiters {
				cmd := exec.Command(os.Args[0], "-test.run=^$")
				cmd.Env = append(os.Environ(),
					"QUAI_LOCK_TEST_WAITER=1",
					"QUAI_LOCK_TEST_ROOT="+locksRoot,
					"QUAI_LOCK_TEST_LOG="+logPath,
					"QUAI_LOCK_TEST_LABEL="+w.label,
					"QUAI_LOCK_TEST_ROLE="+string(w.role),
					"QUAI_LOCK_TEST_PRIORITY="+strconv.Itoa(w.priority),
				)
				var stderr strings.Builder
				cmd.Stderr = &stderr
				if err := cmd.Start(); err != nil {
					t.Fatalf("start waiter %s: %v", w.label, err)
				}
				t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
				cmds = append(cmds, cmd)
				waitForTickets(t, filepath.Join(locksRoot, "slot-heavy.queue"), i+1)
			}

			queued := InspectWaiters(filepath.Join(locksRoot, "slot-heavy.queue"), 0)
			var queuedLabels []string
			for _, o := range queued {
				queuedLabels = append(queuedLabels, o.Command)
			}
			if strings.Join(queuedLabels, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("InspectWaiters order = %v, want %v", queuedLabels, tc.want)
			}

			if err := holder.Release(); err != nil {
				t.Fatalf("holder release: %v", err)
			}
			for i, cmd := range cmds {
				if err := cmd.Wait(); err != nil {
					t.Fatalf("waiter %s: %v: %s", tc.waiters[i].label, err, cmd.Stderr)
				}
			}
			raw, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Fields(string(raw)); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("grant order = %v, want %v", got, tc.want)
			}
		})
	}
}

func waitForTickets(t *testing.T, queueDir string, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		entries, _ := os.ReadDir(queueDir)
		count := 0
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".ticket") {
				count++
			}
		}
		if count >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d tickets in %s", n, queueDir)
}
