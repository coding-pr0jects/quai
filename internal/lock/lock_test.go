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

func spawnDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	deadPID := cmd.Process.Pid
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	return deadPID
}

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

	deadPID := spawnDeadPID(t)
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

func TestCrossNamespaceHeartbeatProtectsAndExpiresLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "container.lock")
	ctx := context.Background()
	deadPID := spawnDeadPID(t)

	// Seed a lock owned by a foreign PID namespace whose PID is dead on this host
	// but whose heartbeat is fresh.
	foreign, err := AcquireDir(ctx, dir, Owner{
		PID:         deadPID,
		Host:        "container-host",
		PIDNS:       "pid:[999999999]",
		Command:     "container-agent",
		HeartbeatAt: time.Now().UTC(),
	}, Options{Wait: time.Second, StaleAfter: 120 * time.Millisecond})
	if err != nil {
		t.Fatalf("seed foreign lock: %v", err)
	}
	_ = foreign

	// Must NOT be stolen while heartbeat is fresh, even though PID does not exist locally.
	_, err = AcquireDir(ctx, dir, Owner{Command: "host-agent"}, Options{
		Wait:         50 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		StaleAfter:   120 * time.Millisecond,
	})
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected foreign lock with fresh heartbeat to resist takeover, got %v", err)
	}

	// Once heartbeat ages past StaleAfter, takeover must succeed.
	l2, err := AcquireDir(ctx, dir, Owner{Command: "host-agent"}, Options{
		Wait:         500 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		StaleAfter:   120 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("expected stale foreign lock takeover, got %v", err)
	}
	defer l2.Release()

	st := InspectDir(dir, 120*time.Millisecond)
	if !st.Held || !st.Alive || st.Stale || st.Owner == nil || st.Owner.Command != "host-agent" {
		t.Fatalf("unexpected inspect state: %+v", st)
	}
}

func TestHeartbeatKeepsLiveLeaseFresh(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hb.lock")
	ctx := context.Background()

	l1, err := AcquireDir(ctx, dir, Owner{Command: "holder"}, Options{
		Wait:              time.Second,
		HeartbeatInterval: 20 * time.Millisecond,
		StaleAfter:        80 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("acquire failed: %v", err)
	}
	defer l1.Release()

	// Sleep longer than StaleAfter; heartbeat must keep the lease fresh.
	time.Sleep(110 * time.Millisecond)

	_, err = AcquireDir(ctx, dir, Owner{Command: "contender"}, Options{
		Wait:         40 * time.Millisecond,
		PollInterval: 10 * time.Millisecond,
		StaleAfter:   80 * time.Millisecond,
	})
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected heartbeat to prevent takeover, got %v", err)
	}
}

func TestAcquireDirFIFOOrder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fifo.lock")
	ctx := context.Background()

	holder, err := AcquireDir(ctx, dir, Owner{Command: "holder"}, Options{Wait: time.Second})
	if err != nil {
		t.Fatalf("initial acquire: %v", err)
	}

	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup

	for i := 1; i <= 3; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			l, err := AcquireDir(ctx, dir, Owner{Command: "waiter"}, Options{
				Wait:         5 * time.Second,
				PollInterval: 10 * time.Millisecond,
			})
			if err != nil {
				t.Errorf("waiter %d acquire failed: %v", id, err)
				return
			}
			mu.Lock()
			order = append(order, id)
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			_ = l.Release()
		}(i)
		// Stagger enqueue timestamps cleanly so ticket ordering is deterministic.
		time.Sleep(25 * time.Millisecond)
	}

	_ = holder.Release()
	wg.Wait()

	if len(order) != 3 || order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Fatalf("expected FIFO order [1 2 3], got %v", order)
	}
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
