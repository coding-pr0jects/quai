package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	DefaultHeartbeatInterval = 5 * time.Second
	DefaultStaleAfter        = 30 * time.Second
	DefaultMissingOwnerWarn  = 10 * time.Second

	// AgingInterval is the wait that earns a queued waiter one point of aging boost.
	AgingInterval = 30 * time.Second
	// MaxAgingBoost caps the aging boost, so a waiter of priority 0 catches up with
	// a quai land waiter (priority 10) after 5 minutes and never passes it on priority alone.
	MaxAgingBoost = 10
)

// Role names the quai command that holds or waits for a lock.
type Role string

const (
	RoleLand   Role = "land"
	RoleVerify Role = "verify"
	RoleExec   Role = "exec"
)

type Owner struct {
	PID         int       `json:"pid"`
	Host        string    `json:"host,omitempty"`
	PIDNS       string    `json:"pid_ns,omitempty"`
	Token       string    `json:"token,omitempty"`
	Role        Role      `json:"role,omitempty"`
	Priority    int       `json:"priority"`
	Command     string    `json:"command"`
	Worktree    string    `json:"worktree"`
	AcquiredAt  time.Time `json:"acquired_at"`
	HeartbeatAt time.Time `json:"heartbeat_at,omitempty"`
}

type Options struct {
	Wait              time.Duration
	PollInterval      time.Duration
	ReportEvery       time.Duration
	MissingOwnerWarn  time.Duration
	HeartbeatInterval time.Duration
	StaleAfter        time.Duration
	MinFreeMemBytes   uint64
	AvailableMem      func() (uint64, bool)
	StatusWriter      io.Writer
}

type InspectState struct {
	Held    bool    `json:"held"`
	Alive   bool    `json:"alive"`
	Stale   bool    `json:"stale"`
	Path    string  `json:"path"`
	Owner   *Owner  `json:"owner,omitempty"`
	Waiters []Owner `json:"waiters,omitempty"`
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
	token    string
	mu       sync.Mutex
	released bool
	stopHB   context.CancelFunc
	hbDone   chan struct{}
}

// Release stops the lease heartbeat and frees the lock only if this lease still owns it.
// Safe to call multiple times.
func (l *Lease) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.released {
		l.mu.Unlock()
		return nil
	}
	l.released = true
	stop := l.stopHB
	done := l.hbDone
	l.mu.Unlock()

	if stop != nil {
		stop()
	}
	if done != nil {
		<-done
	}

	owner, err := readOwner(l.dir)
	if err == nil && !sameLease(owner, l.pid, l.token) {
		return nil
	}
	_ = os.Remove(filepath.Join(l.dir, "owner"))
	if err := os.Remove(l.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("release lock %s: %w", l.dir, err)
	}
	return nil
}

func (l *Lease) Path() string { return l.dir }

func (l *Lease) startHeartbeat(parent context.Context, owner Owner, interval time.Duration) {
	if interval <= 0 || owner.PID != os.Getpid() {
		return
	}
	hbCtx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	l.mu.Lock()
	l.stopHB = cancel
	l.hbDone = done
	l.mu.Unlock()

	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-ticker.C:
				cur, err := readOwner(l.dir)
				if err != nil || !sameLease(cur, l.pid, l.token) {
					return
				}
				cur.HeartbeatAt = time.Now().UTC()
				if err := writeOwnerAtomic(l.dir, cur); err != nil {
					return
				}
			}
		}
	}()
}

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

// AcquireDir atomically creates lockDir in queue order, taking over dead or stale holders.
func AcquireDir(ctx context.Context, lockDir string, owner Owner, opts Options) (*Lease, error) {
	if err := os.MkdirAll(filepath.Dir(lockDir), 0o755); err != nil {
		return nil, fmt.Errorf("create lock parent dir: %w", err)
	}
	owner = normalizeOwner(owner)
	opts = normalizeOptions(opts, 500*time.Millisecond)
	localHost, localPIDNS := LocalHost(), LocalPIDNS()
	queueDir := lockDir + ".queue"

	// Fast path when no live waiters are queued.
	if !hasLiveWaiters(queueDir, localHost, localPIDNS, opts.StaleAfter, time.Now().UTC()) {
		lease, ok, err := tryAcquireOnce(ctx, lockDir, owner, localHost, localPIDNS, opts)
		if err != nil {
			return nil, err
		}
		if ok {
			return lease, nil
		}
	}

	ticketPath, err := enqueueTicket(queueDir, owner)
	if err != nil {
		return nil, err
	}
	defer removeTicket(queueDir, ticketPath)

	start := time.Now()
	var deadline time.Time
	if opts.Wait > 0 {
		deadline = start.Add(opts.Wait)
	}
	lastReport := start
	lastTicketBeat := start
	warnedMissing := false

	for {
		now := time.Now().UTC()
		if time.Since(lastTicketBeat) >= opts.HeartbeatInterval {
			lastTicketBeat = time.Now()
			owner.HeartbeatAt = now
			_ = writeTicketAtomic(ticketPath, owner)
		}

		if isFirstInQueue(queueDir, ticketPath, localHost, localPIDNS, opts.StaleAfter, now) {
			lease, ok, err := tryAcquireOnce(ctx, lockDir, owner, localHost, localPIDNS, opts)
			if err != nil {
				return nil, err
			}
			if ok {
				return lease, nil
			}
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

// AcquireSlots acquires count distinct slots in pool (capacity total) inside locksRoot in queue order.
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
	owner = normalizeOwner(owner)
	opts = normalizeOptions(opts, 500*time.Millisecond)
	localHost, localPIDNS := LocalHost(), LocalPIDNS()
	queueDir := filepath.Join(locksRoot, fmt.Sprintf("slot-%s.queue", pool))
	memFn := opts.AvailableMem
	if memFn == nil {
		memFn = ReadAvailableMemory
	}
	memSatisfied := func() (bool, uint64) {
		if opts.MinFreeMemBytes == 0 {
			return true, 0
		}
		avail, ok := memFn()
		if !ok {
			return true, 0
		}
		return avail >= opts.MinFreeMemBytes, avail
	}

	tryAll := func() (*SlotLease, bool, error) {
		if okMem, _ := memSatisfied(); !okMem {
			return nil, false, nil
		}
		var held []*Lease
		for i := 0; i < capacity && len(held) < count; i++ {
			dir := filepath.Join(locksRoot, fmt.Sprintf("slot-%s-%d.lock", pool, i))
			l, ok, err := tryAcquireOnce(ctx, dir, owner, localHost, localPIDNS, opts)
			if err != nil {
				for _, h := range held {
					_ = h.Release()
				}
				return nil, false, err
			}
			if ok {
				held = append(held, l)
			}
		}
		if len(held) == count {
			return &SlotLease{leases: held}, true, nil
		}
		for _, h := range held {
			_ = h.Release()
		}
		return nil, false, nil
	}

	if !hasLiveWaiters(queueDir, localHost, localPIDNS, opts.StaleAfter, time.Now().UTC()) {
		sl, ok, err := tryAll()
		if err != nil {
			return nil, err
		}
		if ok {
			return sl, nil
		}
	}

	ticketPath, err := enqueueTicket(queueDir, owner)
	if err != nil {
		return nil, err
	}
	defer removeTicket(queueDir, ticketPath)

	start := time.Now()
	var deadline time.Time
	if opts.Wait > 0 {
		deadline = start.Add(opts.Wait)
	}
	lastReport := start
	lastTicketBeat := start

	for {
		now := time.Now().UTC()
		if time.Since(lastTicketBeat) >= opts.HeartbeatInterval {
			lastTicketBeat = time.Now()
			owner.HeartbeatAt = now
			_ = writeTicketAtomic(ticketPath, owner)
		}

		if isFirstInQueue(queueDir, ticketPath, localHost, localPIDNS, opts.StaleAfter, now) {
			sl, ok, err := tryAll()
			if err != nil {
				return nil, err
			}
			if ok {
				return sl, nil
			}
		}

		if !deadline.IsZero() && time.Now().After(deadline) {
			return nil, &TimeoutError{
				Path: filepath.Join(locksRoot, fmt.Sprintf("slot-%s-*", pool)),
				Wait: opts.Wait,
			}
		}

		if opts.StatusWriter != nil && time.Since(lastReport) >= opts.ReportEvery {
			lastReport = time.Now()
			if okMem, avail := memSatisfied(); !okMem {
				fmt.Fprintf(opts.StatusWriter, "quai lock: waiting for >= %s free memory for pool %q (currently %s, %s elapsed)\n",
					FormatBytes(opts.MinFreeMemBytes), pool, FormatBytes(avail), time.Since(start).Round(time.Second))
			} else {
				fmt.Fprintf(opts.StatusWriter, "quai lock: waiting for %d slot(s) in pool %q (%s elapsed)\n", count, pool, time.Since(start).Round(time.Second))
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

// InspectDir reports read-only state for a single directory lock and its queue.
func InspectDir(lockDir string, staleAfter time.Duration) InspectState {
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	localHost, localPIDNS := LocalHost(), LocalPIDNS()
	now := time.Now().UTC()
	st := InspectState{Path: lockDir}
	if _, err := os.Stat(lockDir); err == nil {
		st.Held = true
		if owner, err := readOwner(lockDir); err == nil {
			o := owner
			st.Owner = &o
			alive := OwnerAlive(owner, localHost, localPIDNS, staleAfter, now, processAlive)
			st.Alive = alive
			st.Stale = !alive
		} else {
			st.Stale = true
		}
	}
	st.Waiters = readLiveWaiters(lockDir+".queue", localHost, localPIDNS, staleAfter, now)
	return st
}

// ReleaseDead removes every lock under locksRoot whose lease names pid on this host and PID
// namespace, and every queue ticket of that pid, provided no process with that PID is alive.
// It returns the released lock directories.
func ReleaseDead(locksRoot string, pid int) ([]string, error) {
	if pid <= 0 || processAlive(pid) {
		return nil, nil
	}
	entries, err := os.ReadDir(locksRoot)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read locks dir %s: %w", locksRoot, err)
	}
	localHost, localPIDNS := LocalHost(), LocalPIDNS()
	ownedHere := func(o Owner) bool {
		return o.PID == pid && (o.Host == "" || o.Host == localHost) && (o.PIDNS == "" || o.PIDNS == localPIDNS)
	}
	var released []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(locksRoot, e.Name())
		switch {
		case strings.HasSuffix(e.Name(), ".lock"):
			ok, err := releaseDeadLease(dir, ownedHere)
			if err != nil {
				return released, err
			}
			if ok {
				released = append(released, dir)
			}
		case strings.HasSuffix(e.Name(), ".queue"):
			tickets, _ := os.ReadDir(dir)
			for _, t := range tickets {
				p := filepath.Join(dir, t.Name())
				raw, err := os.ReadFile(p)
				if err != nil {
					continue
				}
				var o Owner
				if json.Unmarshal(raw, &o) == nil && ownedHere(o) {
					removeTicket(dir, p)
				}
			}
		}
	}
	return released, nil
}

func releaseDeadLease(lockDir string, ownedHere func(Owner) bool) (bool, error) {
	cur, err := readOwner(lockDir)
	if err != nil || !ownedHere(cur) {
		return false, nil
	}
	takeoverDir := lockDir + ".takeover"
	if err := os.Mkdir(takeoverDir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("mkdir %s: %w", takeoverDir, err)
	}
	defer os.Remove(takeoverDir)
	again, err := readOwner(lockDir)
	if err != nil || !sameOwnerIdentity(again, cur) {
		return false, nil
	}
	if err := os.Remove(filepath.Join(lockDir, "owner")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("release dead lock %s: %w", lockDir, err)
	}
	if err := os.Remove(lockDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("release dead lock %s: %w", lockDir, err)
	}
	return true, nil
}

// InspectWaiters returns the live queued waiters of a slot pool queue directory in service order.
func InspectWaiters(queueDir string, staleAfter time.Duration) []Owner {
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	return readLiveWaiters(queueDir, LocalHost(), LocalPIDNS(), staleAfter, time.Now().UTC())
}

// RemoveStaleDir removes a directory lock whose lease is dead, or whose owner file has
// been absent longer than MissingOwnerWarn. Removal happens under the lock's takeover
// directory and re-reads the lease first, so a live holder is never deleted.
func RemoveStaleDir(lockDir string, opts Options) (bool, error) {
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = DefaultStaleAfter
	}
	if opts.MissingOwnerWarn <= 0 {
		opts.MissingOwnerWarn = DefaultMissingOwnerWarn
	}
	info, err := os.Stat(lockDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !info.IsDir() {
		return false, nil
	}
	takeover := lockDir + ".takeover"
	if err := os.Mkdir(takeover, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, err
	}
	defer os.Remove(takeover)

	info, err = os.Stat(lockDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	now := time.Now().UTC()
	owner, readErr := readOwner(lockDir)
	if readErr == nil {
		if OwnerAlive(owner, LocalHost(), LocalPIDNS(), opts.StaleAfter, now, processAlive) {
			return false, nil
		}
	} else if now.Sub(info.ModTime()) < opts.MissingOwnerWarn {
		return false, nil
	}
	if err := os.RemoveAll(lockDir); err != nil {
		return false, err
	}
	return true, nil
}

// OwnerAlive decides whether a lock owner is still live, using kill(pid, 0) only within
// the same host and PID namespace and falling back to heartbeat freshness across namespaces.
func OwnerAlive(cur Owner, localHost, localPIDNS string, staleAfter time.Duration, now time.Time, pidAlive func(int) bool) bool {
	if staleAfter <= 0 {
		staleAfter = DefaultStaleAfter
	}
	sameHost := cur.Host == "" || localHost == "" || cur.Host == localHost
	sameNS := cur.PIDNS == "" || localPIDNS == "" || cur.PIDNS == localPIDNS
	if sameHost && sameNS {
		if cur.PID <= 0 || !pidAlive(cur.PID) {
			return false
		}
		if !cur.HeartbeatAt.IsZero() && now.Sub(cur.HeartbeatAt) > staleAfter {
			return false
		}
		return true
	}
	lastBeat := cur.HeartbeatAt
	if lastBeat.IsZero() {
		lastBeat = cur.AcquiredAt
	}
	if lastBeat.IsZero() {
		return true
	}
	return now.Sub(lastBeat) <= staleAfter
}

func normalizeOwner(owner Owner) Owner {
	now := time.Now().UTC()
	if owner.PID == 0 {
		owner.PID = os.Getpid()
	}
	if owner.Host == "" {
		owner.Host = LocalHost()
	}
	if owner.PIDNS == "" {
		owner.PIDNS = LocalPIDNS()
	}
	if owner.Token == "" {
		owner.Token = newToken()
	}
	if owner.AcquiredAt.IsZero() {
		owner.AcquiredAt = now
	}
	if owner.HeartbeatAt.IsZero() {
		owner.HeartbeatAt = now
	}
	return owner
}

func normalizeOptions(opts Options, defaultPoll time.Duration) Options {
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPoll
	}
	if opts.ReportEvery <= 0 {
		opts.ReportEvery = 30 * time.Second
	}
	if opts.MissingOwnerWarn <= 0 {
		opts.MissingOwnerWarn = DefaultMissingOwnerWarn
	}
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if opts.StaleAfter <= 0 {
		opts.StaleAfter = DefaultStaleAfter
	}
	return opts
}

func tryAcquireOnce(ctx context.Context, lockDir string, owner Owner, localHost, localPIDNS string, opts Options) (*Lease, bool, error) {
	owner.HeartbeatAt = time.Now().UTC()
	err := os.Mkdir(lockDir, 0o755)
	if err == nil {
		if err := writeOwnerAtomic(lockDir, owner); err != nil {
			_ = os.Remove(lockDir)
			return nil, false, err
		}
		lease := &Lease{dir: lockDir, pid: owner.PID, token: owner.Token}
		lease.startHeartbeat(ctx, owner, opts.HeartbeatInterval)
		return lease, true, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, false, fmt.Errorf("mkdir %s: %w", lockDir, err)
	}

	cur, readErr := readOwner(lockDir)
	if readErr == nil && !OwnerAlive(cur, localHost, localPIDNS, opts.StaleAfter, time.Now().UTC(), processAlive) {
		taken, takeErr := takeOverDead(lockDir, cur, owner, localHost, localPIDNS, opts.StaleAfter)
		if takeErr != nil {
			return nil, false, takeErr
		}
		if taken {
			lease := &Lease{dir: lockDir, pid: owner.PID, token: owner.Token}
			lease.startHeartbeat(ctx, owner, opts.HeartbeatInterval)
			return lease, true, nil
		}
	}
	return nil, false, nil
}

func takeOverDead(lockDir string, dead Owner, newOwner Owner, localHost, localPIDNS string, staleAfter time.Duration) (bool, error) {
	takeoverDir := lockDir + ".takeover"
	if err := os.Mkdir(takeoverDir, 0o755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return false, nil
		}
		return false, fmt.Errorf("mkdir %s: %w", takeoverDir, err)
	}
	defer os.Remove(takeoverDir)

	cur, err := readOwner(lockDir)
	if err != nil || !sameOwnerIdentity(cur, dead) || OwnerAlive(cur, localHost, localPIDNS, staleAfter, time.Now().UTC(), processAlive) {
		return false, nil
	}
	newOwner.HeartbeatAt = time.Now().UTC()
	if err := writeOwnerAtomic(lockDir, newOwner); err != nil {
		return false, err
	}
	return true, nil
}

func enqueueTicket(queueDir string, owner Owner) (string, error) {
	if err := os.MkdirAll(queueDir, 0o755); err != nil {
		return "", fmt.Errorf("create queue dir %s: %w", queueDir, err)
	}
	name := fmt.Sprintf("%020d-%s.ticket", time.Now().UTC().UnixNano(), owner.Token)
	p := filepath.Join(queueDir, name)
	if err := writeTicketAtomic(p, owner); err != nil {
		return "", err
	}
	return p, nil
}

func writeTicketAtomic(ticketPath string, owner Owner) error {
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	dir := filepath.Dir(ticketPath)
	tmp, err := os.CreateTemp(dir, ".ticket-*")
	if err != nil {
		return fmt.Errorf("create temp ticket in %s: %w", dir, err)
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
	if err := os.Rename(tmpName, ticketPath); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func removeTicket(queueDir, ticketPath string) {
	_ = os.Remove(ticketPath)
	// Remove queueDir opportunistically if empty; ignore errors when other tickets remain.
	_ = os.Remove(queueDir)
}

func hasLiveWaiters(queueDir, localHost, localPIDNS string, staleAfter time.Duration, now time.Time) bool {
	return len(readLiveWaiters(queueDir, localHost, localPIDNS, staleAfter, now)) > 0
}

func readLiveWaiters(queueDir, localHost, localPIDNS string, staleAfter time.Duration, now time.Time) []Owner {
	queued := readQueue(queueDir, "", false, localHost, localPIDNS, staleAfter, now)
	var live []Owner
	for _, w := range queued {
		live = append(live, w.owner)
	}
	return live
}

func isFirstInQueue(queueDir, myTicket, localHost, localPIDNS string, staleAfter time.Duration, now time.Time) bool {
	myName := filepath.Base(myTicket)
	queued := readQueue(queueDir, myName, true, localHost, localPIDNS, staleAfter, now)
	return len(queued) == 0 || queued[0].name == myName
}

type waiter struct {
	name       string
	enqueuedAt time.Time
	owner      Owner
}

func readQueue(queueDir, ownTicket string, pruneDead bool, localHost, localPIDNS string, staleAfter time.Duration, now time.Time) []waiter {
	entries, err := os.ReadDir(queueDir)
	if err != nil {
		return nil
	}
	var live []waiter
	for _, e := range entries {
		enqueuedAt, ok := ticketEnqueuedAt(e.Name())
		if e.IsDir() || !ok {
			continue
		}
		p := filepath.Join(queueDir, e.Name())
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var o Owner
		err = json.Unmarshal(raw, &o)
		if e.Name() != ownTicket && (err != nil || !OwnerAlive(o, localHost, localPIDNS, staleAfter, now, processAlive)) {
			if pruneDead {
				_ = os.Remove(p)
			}
			continue
		}
		live = append(live, waiter{name: e.Name(), enqueuedAt: enqueuedAt, owner: o})
	}
	sortWaiters(live, now)
	return live
}

func ticketEnqueuedAt(name string) (time.Time, bool) {
	stamp, _, ok := strings.Cut(name, "-")
	if !ok || !strings.HasSuffix(name, ".ticket") {
		return time.Time{}, false
	}
	ns, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, ns).UTC(), true
}

func sortWaiters(ws []waiter, now time.Time) {
	sort.Slice(ws, func(i, j int) bool {
		pi := EffectivePriority(ws[i].owner.Priority, now.Sub(ws[i].enqueuedAt))
		pj := EffectivePriority(ws[j].owner.Priority, now.Sub(ws[j].enqueuedAt))
		if pi != pj {
			return pi > pj
		}
		if !ws[i].enqueuedAt.Equal(ws[j].enqueuedAt) {
			return ws[i].enqueuedAt.Before(ws[j].enqueuedAt)
		}
		return ws[i].name < ws[j].name
	})
}

// EffectivePriority is priority plus one point per AgingInterval waited, capped at MaxAgingBoost.
func EffectivePriority(priority int, waited time.Duration) int {
	if waited <= 0 {
		return priority
	}
	return priority + int(min(waited/AgingInterval, MaxAgingBoost))
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

func sameLease(cur Owner, pid int, token string) bool {
	if token != "" && cur.Token != "" {
		return cur.Token == token
	}
	return cur.PID == pid
}

func sameOwnerIdentity(a, b Owner) bool {
	if a.Token != "" && b.Token != "" {
		return a.Token == b.Token
	}
	return a.PID == b.PID && a.Host == b.Host && a.PIDNS == b.PIDNS
}

// LocalHost is the host name that leases record.
func LocalHost() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}

// LocalPIDNS is the Linux PID namespace that leases record, empty outside Linux.
func LocalPIDNS() string {
	ns, err := os.Readlink("/proc/self/ns/pid")
	if err != nil {
		return ""
	}
	return ns
}

func newToken() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
