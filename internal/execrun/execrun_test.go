package execrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/lock"
)

func TestRunCleansUpChildProcessGroupOnCancel(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for i := 0; i < 50; i++ {
			if _, err := os.Stat(pidFile); err == nil {
				cancel()
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
	}()

	var out bytes.Buffer
	code, err := Run(ctx, Request{
		Dir:      dir,
		ShellCmd: fmt.Sprintf("sleep 60 & echo $! > %s; wait", pidFile),
		Stdout:   &out,
		Stderr:   &out,
	})
	if !errors.Is(err, context.Canceled) || code == 0 {
		t.Fatalf("expected context.Canceled and non-zero exit, got code=%d err=%v", code, err)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse pid: %v", err)
	}

	// Give kernel a brief moment to reap the terminated grandchild.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("grandchild pid %d survived process group cleanup", pid)
}

func TestRunPropagatesExitCode(t *testing.T) {
	var out bytes.Buffer
	code, err := Run(context.Background(), Request{
		Dir:     t.TempDir(),
		Argv:    []string{"sh", "-c", "echo hi; exit 7"},
		Stdout:  &out,
		Stderr:  &out,
		Timeout: 5 * time.Second,
	})
	if code != 7 || err == nil {
		t.Fatalf("got code=%d err=%v, want code=7 and error", code, err)
	}
	if strings.TrimSpace(out.String()) != "hi" {
		t.Fatalf("stdout = %q, want hi", out.String())
	}
}

func TestRunReportsSlotAcquisitionOnlyWhenHeld(t *testing.T) {
	ctx := context.Background()
	locksRoot := t.TempDir()
	acquired := 0

	code, err := Run(ctx, Request{
		Dir:           t.TempDir(),
		Argv:          []string{"true"},
		LocksRoot:     locksRoot,
		Pool:          "heavy",
		Slots:         1,
		Capacity:      1,
		LockWait:      time.Second,
		OnSlotAcquire: func() { acquired++ },
	})
	if err != nil || code != 0 || acquired != 1 {
		t.Fatalf("free slot should report acquisition once: code=%d err=%v acquired=%d", code, err, acquired)
	}

	held, err := lock.AcquireSlots(ctx, locksRoot, "heavy", 1, 1, lock.Owner{Command: "test"}, lock.Options{Wait: time.Second})
	if err != nil {
		t.Fatalf("hold slot: %v", err)
	}
	defer held.Release()

	acquired = 0
	_, err = Run(ctx, Request{
		Dir:           t.TempDir(),
		Argv:          []string{"true"},
		LocksRoot:     locksRoot,
		Pool:          "heavy",
		Slots:         1,
		Capacity:      1,
		LockWait:      100 * time.Millisecond,
		OnSlotAcquire: func() { acquired++ },
	})
	var timeout *lock.TimeoutError
	if !errors.As(err, &timeout) || acquired != 0 {
		t.Fatalf("timed-out wait must not report acquisition: err=%v acquired=%d", err, acquired)
	}
}
