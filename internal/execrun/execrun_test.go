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
	res, err := Run(ctx, Request{
		Dir:      dir,
		ShellCmd: fmt.Sprintf("sleep 60 & echo $! > %s; wait", pidFile),
		Stdout:   &out,
		Stderr:   &out,
	})
	if !errors.Is(err, context.Canceled) || res.ExitCode == 0 || res.TimedOut {
		t.Fatalf("expected context.Canceled and non-zero exit, got res=%+v err=%v", res, err)
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
	res, err := Run(context.Background(), Request{
		Dir:     t.TempDir(),
		Argv:    []string{"sh", "-c", "echo hi; exit 7"},
		Stdout:  &out,
		Stderr:  &out,
		Timeout: 5 * time.Second,
	})
	if res.ExitCode != 7 || err == nil || res.TimedOut || res.KilledBySIGKILL {
		t.Fatalf("got res=%+v err=%v, want exit code 7 and error", res, err)
	}
	if strings.TrimSpace(out.String()) != "hi" {
		t.Fatalf("stdout = %q, want hi", out.String())
	}
}

func TestRunReportsSlotAcquisitionOnlyWhenHeld(t *testing.T) {
	ctx := context.Background()
	locksRoot := t.TempDir()
	acquired := 0

	res, err := Run(ctx, Request{
		Dir:           t.TempDir(),
		Argv:          []string{"true"},
		LocksRoot:     locksRoot,
		Pool:          "heavy",
		Slots:         1,
		Capacity:      1,
		LockWait:      time.Second,
		OnSlotAcquire: func() { acquired++ },
	})
	if err != nil || res.ExitCode != 0 || acquired != 1 {
		t.Fatalf("free slot should report acquisition once: res=%+v err=%v acquired=%d", res, err, acquired)
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

func TestRunDetectsTimeout(t *testing.T) {
	start := time.Now()
	res, err := Run(context.Background(), Request{
		Dir:      t.TempDir(),
		ShellCmd: "sleep 60",
		Timeout:  100 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) || !res.TimedOut || res.KilledBySIGKILL {
		t.Fatalf("got res=%+v err=%v, want timed out", res, err)
	}
	if elapsed := time.Since(start); elapsed > 6*time.Second {
		t.Fatalf("timeout cleanup took %s, want under the 5s grace period", elapsed)
	}
}

func TestRunDetectsSIGKILL(t *testing.T) {
	var out bytes.Buffer
	res, err := Run(context.Background(), Request{
		Dir:     t.TempDir(),
		Argv:    []string{"sh", "-c", "kill -9 $$"},
		Stdout:  &out,
		Stderr:  &out,
		Timeout: 5 * time.Second,
	})
	if err == nil || !res.KilledBySIGKILL || res.ExitCode != 137 {
		t.Fatalf("got res=%+v err=%v, want SIGKILL with exit code 137", res, err)
	}
}
