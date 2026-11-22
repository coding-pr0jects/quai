package execrun

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Process names a process by PID and kernel start time, so a reused PID never matches it.
type Process struct {
	PID       int    `json:"pid"`
	StartTime uint64 `json:"start_time,omitempty"`
}

// FindProcess returns the running process with this PID. A zombie counts as exited.
func FindProcess(pid int) (Process, bool) {
	state, start, ok := readStat(pid)
	if !ok {
		return Process{}, false
	}
	if state == 'Z' || state == 'X' {
		return Process{}, false
	}
	return Process{PID: pid, StartTime: start}, true
}

// Alive reports whether p is still running, and not a later process that reused its PID.
func (p Process) Alive() bool {
	cur, ok := FindProcess(p.PID)
	return ok && cur.StartTime == p.StartTime
}

// KillGroup sends SIGKILL to the process group that p leads, unless its PID now names another process.
func (p Process) KillGroup() {
	if p.PID <= 0 {
		return
	}
	if _, start, ok := readStat(p.PID); ok && start != p.StartTime {
		return
	}
	_ = syscall.Kill(-p.PID, syscall.SIGKILL)
}

// StopGroup sends SIGTERM to the process group that p leads, waits up to grace for p to exit,
// then sends SIGKILL to the group.
func (p Process) StopGroup(grace time.Duration) {
	if p.Alive() {
		_ = syscall.Kill(-p.PID, syscall.SIGTERM)
		deadline := time.Now().Add(grace)
		for p.Alive() && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
	}
	p.KillGroup()
}

// Detach starts argv in a new session, so in its own process group, with stdin from /dev/null
// and stdout and stderr appended to logPath, and returns without waiting for it.
func Detach(dir string, argv, env []string, logPath string) (Process, error) {
	if len(argv) == 0 {
		return Process{}, errors.New("empty command")
	}
	log, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return Process{}, err
	}
	defer log.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return Process{}, err
	}
	p := identify(cmd.Process.Pid)
	_ = cmd.Process.Release()
	return p, nil
}

func identify(pid int) Process {
	_, start, _ := readStat(pid)
	return Process{PID: pid, StartTime: start}
}

func readStat(pid int) (state byte, startTime uint64, ok bool) {
	if pid <= 0 {
		return 0, 0, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if _, procErr := os.Stat("/proc/self/stat"); procErr == nil {
			return 0, 0, false
		}
		// Without procfs (macOS, BSD), only kill(pid, 0) can tell, and PID reuse goes undetected.
		killErr := syscall.Kill(pid, 0)
		return 'R', 0, killErr == nil || errors.Is(killErr, syscall.EPERM)
	}
	return parseStat(string(data))
}

// The command name of proc_pid_stat(5) may contain spaces and parentheses, so fields are
// counted from the last ')'.
func parseStat(line string) (state byte, startTime uint64, ok bool) {
	end := strings.LastIndexByte(line, ')')
	if end < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(line[end+1:])
	const startTimeField = 19 // field 22 of proc_pid_stat(5), counted from field 3 (state)
	if len(fields) <= startTimeField || len(fields[0]) != 1 {
		return 0, 0, false
	}
	start, err := strconv.ParseUint(fields[startTimeField], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return fields[0][0], start, true
}
