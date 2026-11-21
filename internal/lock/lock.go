package lock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

type Owner struct {
	PID        int       `json:"pid"`
	Command    string    `json:"command"`
	Worktree   string    `json:"worktree"`
	AcquiredAt time.Time `json:"acquired_at"`
}

type Options struct {
	Wait             time.Duration
	PollInterval     time.Duration
	ReportEvery      time.Duration
	MissingOwnerWarn time.Duration
	StatusWriter     io.Writer
}

type TimeoutError struct {
	Path  string
	Wait  time.Duration
	Owner *Owner
}

func (e *TimeoutError) Error() string {
	if e.Owner != nil {
		return fmt.Sprintf("timed out after %s waiting for lock %s (held by pid %d: %s)", e.Wait, e.Path, e.Owner.PID, e.Owner.Command)
	}
	return fmt.Sprintf("timed out after %s waiting for lock %s", e.Wait, e.Path)
}

type Lease struct {
	dir      string
	pid      int
	mu       sync.Mutex
	released bool
}

// Release frees the lock only if this lease's PID still owns it. Safe to call multiple times.
func (l *Lease) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true

	owner, err := readOwner(l.dir)
	if err == nil && owner.PID != l.pid {
		return nil
	}
	_ = os.Remove(filepath.Join(l.dir, "owner"))
	if err := os.Remove(l.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("release lock %s: %w", l.dir, err)
	}
	return nil
}

func (l *Lease) Path() string { return l.dir }

type SlotLease struct {
	leases []*Lease
}

func (s *SlotLease) Release() error {
	if s == nil {
		return nil
	}
	var firstErr error
	for _, l := range s.leases {
		if err := l.Release(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	s.leases = nil
	return firstErr
}

// AcquireDir atomically creates lockDir or takes it over if the holding PID is dead.
func AcquireDir(ctx context.Context, lockDir string, owner Owner, opts Options) (*Lease, error) {
	if err := os.MkdirAll(filepath.Dir(lockDir), 0o755); err != nil {
		return nil, fmt.Errorf("create lock parent dir: %w", err)
	}
	if owner.PID == 0 {
		owner.PID = os.Getpid()
	}
	if owner.AcquiredAt.IsZero() {
		owner.AcquiredAt = time.Now().UTC()
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 500 * time.Millisecond
	}
	if opts.ReportEvery <= 0 {
		opts.ReportEvery = 30 * time.Second
	}
	if opts.MissingOwnerWarn <= 0 {
		opts.MissingOwnerWarn = 10 * time.Second
	}

	start := time.Now()
	var deadline time.Time
	if opts.Wait > 0 {
		deadline = start.Add(opts.Wait)
	}
	lastReport := start
	warnedMissing := false

	for {
		lease, ok, err := tryAcquireOnce(lockDir, owner)
		if err != nil {
			return nil, err
		}
		if ok {
			return lease, nil
		}

		cur, readErr := readOwner(lockDir)
		if readErr != nil {
			if !warnedMissing && time.Since(start) >= opts.MissingOwnerWarn {
				warnedMissing = true
				if opts.StatusWriter != nil {
					fmt.Fprintf(opts.StatusWriter, "quai lock: warning: %s has no owner file after %s; not breaking it\n", lockDir, opts.MissingOwnerWarn)
				}
			}
		}

		if !deadline.IsZero() && time.Now().After(deadline) {
			var holder *Owner
			if readErr == nil {
				holder = &cur
			}
			return nil, &TimeoutError{Path: lockDir, Wait: opts.Wait, Owner: holder}
		}

		if opts.StatusWriter != nil && time.Since(lastReport) >= opts.ReportEvery {
			lastReport = time.Now()
			if readErr == nil {
				fmt.Fprintf(opts.StatusWriter, "quai lock: waiting for %s (held by pid %d: %s, %s elapsed)\n", lockDir, cur.PID, cur.Command, time.Since(start).Round(time.Second))
			} else {
				fmt.Fprintf(opts.StatusWriter, "quai lock: waiting for %s (%s elapsed)\n", lockDir, time.Since(start).Round(time.Second))
			}
		}

		timer := time.NewTimer(opts.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// AcquireSlots acquires count distinct slots in pool (capacity total) inside locksRoot.
func AcquireSlots(ctx context.Context, locksRoot, pool string, count, capacity int, owner Owner, opts Options) (*SlotLease, error) {
	if count <= 0 {
		return &SlotLease{}, nil
	}
	if count > capacity {
		return nil, fmt.Errorf("requested %d slots in pool %q with capacity %d", count, pool, capacity)
	}
	if err := os.MkdirAll(locksRoot, 0o755); err != nil {
		return nil, fmt.Errorf("create locks root: %w", err)
	}
	if owner.PID == 0 {
		owner.PID = os.Getpid()
	}
	if owner.AcquiredAt.IsZero() {
		owner.AcquiredAt = time.Now().UTC()
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 500 * time.Millisecond
	}
	if opts.ReportEvery <= 0 {
		opts.ReportEvery = 30 * time.Second
	}

	start := time.Now()
	var deadline time.Time
	if opts.Wait > 0 {
		deadline = start.Add(opts.Wait)
	}
	lastReport := start

	for {
		var held []*Lease
		for i := 0; i < capacity && len(held) < count; i++ {
			dir := filepath.Join(locksRoot, fmt.Sprintf("slot-%s-%d.lock", pool, i))
			l, ok, err := tryAcquireOnce(dir, owner)
			if err != nil {
				for _, h := range held {
					_ = h.Release()
				}
				return nil, err
			}
			if ok {
				held = append(held, l)
			}
		}
		if len(held) == count {
			return &SlotLease{leases: held}, nil
		}
		for _, h := range held {
			_ = h.Release()
		}

		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, &TimeoutError{
				Path: filepath.Join(locksRoot, fmt.Sprintf("slot-%s-*", pool)),
				Wait: opts.Wait,
			}
		}

		if opts.StatusWriter != nil && time.Since(lastReport) >= opts.ReportEvery {
			lastReport = time.Now()
			fmt.Fprintf(opts.StatusWriter, "quai lock: waiting for %d slot(s) in pool %q (%s elapsed)\n", count, pool, time.Since(start).Round(time.Second))
		}

		timer := time.NewTimer(opts.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func tryAcquireOnce(lockDir string, owner Owner) (*Lease, bool, error) {
	err := os.Mkdir(lockDir, 0o755)
	if err == nil {
		if err := writeOwnerAtomic(lockDir, owner); err != nil {
			_ = os.Remove(lockDir)
			return nil, false, err
		}
		return &Lease{dir: lockDir, pid: owner.PID}, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, fmt.Errorf("mkdir %s: %w", lockDir, err)
	}

	cur, readErr := readOwner(lockDir)
	if readErr == nil && cur.PID > 0 && !processAlive(cur.PID) {
		taken, takeErr := takeOverDead(lockDir, cur.PID, owner)
		if takeErr != nil {
			return nil, false, takeErr
		}
		if taken {
			return &Lease{dir: lockDir, pid: owner.PID}, true, nil
		}
	}
	return nil, false, nil
}

func takeOverDead(lockDir string, deadPID int, newOwner Owner) (bool, error) {
	takeoverDir := lockDir + ".takeover"
	if err := os.Mkdir(takeoverDir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("mkdir %s: %w", takeoverDir, err)
	}
	defer os.Remove(takeoverDir)

	cur, err := readOwner(lockDir)
	if err != nil || cur.PID != deadPID || processAlive(deadPID) {
		return false, nil
	}
	if err := writeOwnerAtomic(lockDir, newOwner); err != nil {
		return false, err
	}
	return true, nil
}

func writeOwnerAtomic(lockDir string, owner Owner) error {
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(lockDir, ".owner-*")
	if err != nil {
		return fmt.Errorf("create temp owner in %s: %w", lockDir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(lockDir, "owner")); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func readOwner(lockDir string) (Owner, error) {
	data, err := os.ReadFile(filepath.Join(lockDir, "owner"))
	if err != nil {
		return Owner{}, err
	}
	var owner Owner
	if err := json.Unmarshal(data, &owner); err != nil {
		return Owner{}, err
	}
	return owner, nil
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
