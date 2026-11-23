package execrun

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/coding-pr0jects/quai/internal/lock"
)

const gracePeriod = 5 * time.Second

type Request struct {
	Dir              string
	Argv             []string
	ShellCmd         string // if non-empty, runs via sh -c <ShellCmd>
	Timeout          time.Duration
	Env              []string // KEY=value pairs added to the inherited environment; a later pair wins
	UnsetEnv         []string // names removed from the inherited environment before Env is applied
	Stdin            io.Reader
	Stdout           io.Writer
	Stderr           io.Writer
	LocksRoot        string
	Pool             string
	Slots            int
	Capacity         int
	MinFreeMemBytes  uint64
	MinFreeDiskBytes uint64
	LockWait         time.Duration
	Owner            lock.Owner
	OnStart          func(Process)
	OnSlotAcquire    func()
}

// Result reports how a child command ended.
type Result struct {
	ExitCode        int
	TimedOut        bool
	KilledBySIGKILL bool
}

// Run executes a command inside its own process group, optionally bounded by a slot pool,
// and cleans up the entire process group on context cancellation or timeout.
func Run(ctx context.Context, req Request) (Result, error) {
	if req.Slots > 0 {
		sl, err := lock.AcquireSlots(ctx, req.LocksRoot, req.Pool, req.Slots, req.Capacity, req.Owner, lock.Options{
			Wait:             req.LockWait,
			PollInterval:     200 * time.Millisecond,
			MinFreeMemBytes:  req.MinFreeMemBytes,
			MinFreeDiskBytes: req.MinFreeDiskBytes,
			DiskPath:         req.Dir,
			StatusWriter:     req.Stderr,
		})
		if err != nil {
			return Result{ExitCode: 1}, err
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
			return Result{ExitCode: 2}, errors.New("empty command")
		}
		cmd = exec.Command(req.Argv[0], req.Argv[1:]...)
	}
	cmd.Dir = req.Dir
	if len(req.Env) > 0 || len(req.UnsetEnv) > 0 {
		env := os.Environ()
		if len(req.UnsetEnv) > 0 {
			env = environWithout(req.UnsetEnv)
		}
		cmd.Env = append(env, req.Env...)
	}
	cmd.Stdin = req.Stdin
	cmd.Stdout = req.Stdout
	cmd.Stderr = req.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return Result{ExitCode: 1}, err
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
		return completed(cmd, err), err
	case <-runCtx.Done():
		pgid := cmd.Process.Pid
		_ = syscall.Kill(-pgid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(gracePeriod):
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
			<-done
		}
		return Result{ExitCode: 1, TimedOut: errors.Is(runCtx.Err(), context.DeadlineExceeded)}, runCtx.Err()
	}
}

func completed(cmd *exec.Cmd, err error) Result {
	res := Result{ExitCode: exitCodeOf(err)}
	if cmd.ProcessState != nil {
		if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.ExitCode = 128 + int(ws.Signal())
			res.KilledBySIGKILL = ws.Signal() == syscall.SIGKILL
		}
	}
	if res.ExitCode == 137 {
		res.KilledBySIGKILL = true
	}
	return res
}

func environWithout(unset []string) []string {
	drop := make(map[string]struct{}, len(unset))
	for _, k := range unset {
		drop[k] = struct{}{}
	}
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if _, ok := drop[key]; ok {
			continue
		}
		out = append(out, kv)
	}
	return out
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
