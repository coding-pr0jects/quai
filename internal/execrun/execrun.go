package execrun

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"syscall"
	"time"

	"github.com/coding-pr0jects/quai/internal/lock"
)

const gracePeriod = 5 * time.Second

type Request struct {
	Dir             string
	Argv            []string
	ShellCmd        string // if non-empty, runs via sh -c <ShellCmd>
	Timeout         time.Duration
	Stdin           io.Reader
	Stdout          io.Writer
	Stderr          io.Writer
	LocksRoot       string
	Pool            string
	Slots           int
	Capacity        int
	MinFreeMemBytes uint64
	LockWait        time.Duration
	Owner           lock.Owner
	OnStart         func(Process)
	OnSlotAcquire   func()
}

// Run executes a command inside its own process group, optionally bounded by a slot pool,
// and cleans up the entire process group on context cancellation or timeout.
func Run(ctx context.Context, req Request) (int, error) {
	if req.Slots > 0 {
		sl, err := lock.AcquireSlots(ctx, req.LocksRoot, req.Pool, req.Slots, req.Capacity, req.Owner, lock.Options{
			Wait:            req.LockWait,
			PollInterval:    200 * time.Millisecond,
			MinFreeMemBytes: req.MinFreeMemBytes,
			StatusWriter:    req.Stderr,
		})
		if err != nil {
			return 1, err
		}
		defer sl.Release()
		if req.OnSlotAcquire != nil {
			req.OnSlotAcquire()
		}
	}

	runCtx := ctx
	var cancel context.CancelFunc
	if req.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}

	var cmd *exec.Cmd
	if req.ShellCmd != "" {
		cmd = exec.Command("sh", "-c", req.ShellCmd)
	} else {
		if len(req.Argv) == 0 {
			return 2, errors.New("empty command")
		}
		cmd = exec.Command(req.Argv[0], req.Argv[1:]...)
	}
	cmd.Dir = req.Dir
	cmd.Stdin = req.Stdin
	cmd.Stdout = req.Stdout
	cmd.Stderr = req.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return 1, err
	}
	if req.OnStart != nil {
		req.OnStart(identify(cmd.Process.Pid))
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		return exitCodeOf(err), err
	case <-runCtx.Done():
		pgid := cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(gracePeriod):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-done
		}
		return 1, runCtx.Err()
	}
}

func exitCodeOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
