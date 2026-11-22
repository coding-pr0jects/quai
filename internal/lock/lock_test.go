package lock

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestReadAvailableDisk(t *testing.T) {
	if got, ok := ReadAvailableDisk(t.TempDir()); !ok || got == 0 {
		t.Fatalf("ReadAvailableDisk = (%d, %v), want a positive byte count", got, ok)
	}
	if _, ok := ReadAvailableDisk(filepath.Join(t.TempDir(), "missing")); ok {
		t.Fatal("expected ReadAvailableDisk to fail on a missing path")
	}
}

func TestParseMeminfoAvailableAndMemoryGuard(t *testing.T) {
	sample := "MemTotal:       16384000 kB\nMemFree:         2048000 kB\nMemAvailable:    6144000 kB\n"
	got, ok := ParseMeminfoAvailable(sample)
	if !ok || got != 6144000*1024 {
		t.Fatalf("ParseMeminfoAvailable = (%d, %v), want (%d, true)", got, ok, 6144000*1024)
	}
	fallback, ok := ParseMeminfoAvailable("MemTotal: 8000 kB\nMemFree: 1000 kB\n")
	if !ok || fallback != 1000*1024 {
		t.Fatalf("MemFree fallback = (%d, %v)", fallback, ok)
	}

	// AcquireSlots must wait when available RAM is below MinFreeMemBytes and proceed once memory recovers.
	locksRoot := t.TempDir()
	ctx := context.Background()
	var freeMem atomic.Uint64
	freeMem.Store(1 * 1024 * 1024 * 1024) // 1 GiB

	const threshold = uint64(4 * 1024 * 1024 * 1024) // 4 GiB
	_, err := AcquireSlots(ctx, locksRoot, "heavy", 1, 2, Owner{Command: "blocked"}, Options{
		Wait:            60 * time.Millisecond,
		PollInterval:    10 * time.Millisecond,
		MinFreeMemBytes: threshold,
		AvailableMem:    func() (uint64, bool) { return freeMem.Load(), true },
	})
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected TimeoutError under low memory, got %v", err)
	}

	go func() {
		time.Sleep(45 * time.Millisecond)
		freeMem.Store(6 * 1024 * 1024 * 1024) // recover to 6 GiB
	}()

	sl, err := AcquireSlots(ctx, locksRoot, "heavy", 1, 2, Owner{Command: "recovered"}, Options{
		Wait:            time.Second,
		PollInterval:    10 * time.Millisecond,
		MinFreeMemBytes: threshold,
		AvailableMem:    func() (uint64, bool) { return freeMem.Load(), true },
	})
	if err != nil {
		t.Fatalf("expected slot acquisition after memory recovery, got %v", err)
	}
	defer sl.Release()
}

func TestReleaseDeadRemovesOnlyTheDeadPIDsLeasesAndTickets(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	deadPID := spawnDeadPID(t)

	dead, err := AcquireDir(ctx, filepath.Join(root, "slot-heavy-0.lock"), Owner{PID: deadPID, Command: "crashed"}, Options{Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	_ = dead
	live, err := AcquireDir(ctx, filepath.Join(root, "slot-heavy-1.lock"), Owner{Command: "live"}, Options{Wait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer live.Release()
	queue := filepath.Join(root, "slot-heavy.queue")
	deadTicket, err := enqueueTicket(queue, normalizeOwner(Owner{PID: deadPID}))
	if err != nil {
		t.Fatal(err)
	}
	liveTicket, err := enqueueTicket(queue, normalizeOwner(Owner{}))
	if err != nil {
		t.Fatal(err)
	}

	if got, err := ReleaseDead(root, os.Getpid()); err != nil || len(got) != 0 {
		t.Fatalf("ReleaseDead(live pid) = %v, %v; want nothing released", got, err)
	}
	got, err := ReleaseDead(root, deadPID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0]) != "slot-heavy-0.lock" {
		t.Fatalf("released %v, want only slot-heavy-0.lock", got)
	}
	for path, want := range map[string]bool{
		filepath.Join(root, "slot-heavy-0.lock"): false,
		filepath.Join(root, "slot-heavy-1.lock"): true,
		deadTicket:                               false,
		liveTicket:                               true,
	} {
		if _, err := os.Stat(path); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", path, err == nil, want)
		}
	}
}
func TestAcquireSlotsDiskGuard(t *testing.T) {
	locksRoot := t.TempDir()
	ctx := context.Background()
	var freeDisk atomic.Uint64
	freeDisk.Store(500 * 1024 * 1024)

	const threshold = uint64(2 * 1024 * 1024 * 1024)
	opts := Options{
		Wait:             60 * time.Millisecond,
		PollInterval:     10 * time.Millisecond,
		MinFreeDiskBytes: threshold,
		AvailableDisk:    func(string) (uint64, bool) { return freeDisk.Load(), true },
	}
	_, err := AcquireSlots(ctx, locksRoot, "heavy", 1, 2, Owner{Command: "blocked"}, opts)
	var te *TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("expected TimeoutError under low disk, got %v", err)
	}
	if !strings.Contains(te.Error(), "min_free_disk") || !strings.Contains(te.Error(), "500.0 MiB") {
		t.Fatalf("expected clear low-disk message, got %q", te.Error())
	}

	go func() {
		time.Sleep(45 * time.Millisecond)
		freeDisk.Store(3 * 1024 * 1024 * 1024)
	}()
	opts.Wait = time.Second
	sl, err := AcquireSlots(ctx, locksRoot, "heavy", 1, 2, Owner{Command: "recovered"}, opts)
	if err != nil {
		t.Fatalf("expected slot acquisition after disk recovery, got %v", err)
	}
	sl.Release()
}
