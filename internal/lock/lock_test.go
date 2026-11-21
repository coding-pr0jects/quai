package lock

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireDirAndDeadPIDTakeover(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "land.lock")
	ctx := context.Background()

	l1, err := AcquireDir(ctx, dir, Owner{Command: "first"}, Options{Wait: time.Second, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}

	// Second acquire while live PID holds it must time out.
	_, err = AcquireDir(ctx, dir, Owner{Command: "second"}, Options{Wait: 60 * time.Millisecond, PollInterval: 10 * time.Millisecond})
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected TimeoutError while lock is held, got %v", err)
	}

	if err := l1.Release(); err != nil {
		t.Fatalf("release failed: %v", err)
	}
	// Release is idempotent.
	if err := l1.Release(); err != nil {
		t.Fatalf("second release failed: %v", err)
	}

	// Simulate a crashed holder by spawning a short-lived process, killing it with SIGKILL, and recording its dead PID.
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	deadPID := cmd.Process.Pid
	_ = cmd.Process.Kill()
	_ = cmd.Wait()

	l2, err := AcquireDir(ctx, dir, Owner{PID: deadPID, Command: "crashed"}, Options{Wait: time.Second})
	if err != nil {
		t.Fatalf("seed dead-pid lock: %v", err)
	}
	_ = l2 // intentionally not released to simulate crash

	l3, err := AcquireDir(ctx, dir, Owner{Command: "rescuer"}, Options{Wait: time.Second, PollInterval: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("expected takeover of dead PID %d, got error: %v", deadPID, err)
	}
	defer l3.Release()
}

func TestAcquireSlotsBoundsConcurrency(t *testing.T) {
	locksRoot := t.TempDir()
	ctx := context.Background()
	const capacity = 2
	const workers = 6

	var active atomic.Int32
	var maxSeen atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			sl, err := AcquireSlots(ctx, locksRoot, "heavy", 1, capacity, Owner{Command: "worker"}, Options{
				Wait:         5 * time.Second,
				PollInterval: 15 * time.Millisecond,
			})
			if err != nil {
				t.Errorf("worker %d failed to acquire slot: %v", id, err)
				return
			}
			defer sl.Release()
			cur := active.Add(1)
			for {
				prev := maxSeen.Load()
				if cur <= prev || maxSeen.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(40 * time.Millisecond)
			active.Add(-1)
		}(i)
	}
	wg.Wait()

	if got := maxSeen.Load(); got > capacity {
		t.Fatalf("max concurrent slot holders = %d, exceeded capacity %d", got, capacity)
	}
	if got := maxSeen.Load(); got < 2 {
		t.Fatalf("expected parallel utilization of 2 slots, maxSeen = %d", got)
	}
}
