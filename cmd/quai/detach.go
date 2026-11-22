package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coding-pr0jects/quai/internal/detach"
	"github.com/coding-pr0jects/quai/internal/repo"
	"github.com/coding-pr0jects/quai/internal/verify"
)

func startDetached(ctx context.Context, g *repo.ExecGit, worktree, cwd string, args []string, jsonOut bool, stdout, stderr io.Writer) int {
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "quai %s: %v\n", args[0], err)
		return 2
	}
	exe, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "quai %s: locate the quai executable: %v\n", args[0], err)
		return 1
	}
	dir, err := filepath.Abs(cwd)
	if err != nil {
		dir = worktree
	}
	store := detach.NewStore(commonDir)
	m, err := store.Start(dir, exe, args, worktree)
	if err != nil {
		fmt.Fprintf(stderr, "quai %s: %v\n", args[0], err)
		return 1
	}
	if jsonOut {
		writeJSON(stdout, store.Inspect(m))
	} else {
		fmt.Fprintf(stdout, "Started quai run %s: quai verify --wait %s\n", m.ID, m.ID)
	}
	return 0
}

func runRunControl(ctx context.Context, cwd, runID string, wait bool, maxWait time.Duration, jsonOut bool, stdout, stderr io.Writer) int {
	action := "--stop"
	if wait {
		action = "--wait"
	}
	g := repo.NewExecGit(cwd)
	top, err := g.TopLevel(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "quai verify %s: not inside a git repository: %v\n", action, err)
		return 2
	}
	g.Dir = top
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "quai verify %s: %v\n", action, err)
		return 2
	}
	store := detach.NewStore(commonDir)
	m, err := store.Resolve(runID, top)
	if err != nil {
		fmt.Fprintf(stderr, "quai verify %s: %v\n", action, err)
		return 2
	}

	var rep detach.Report
	if wait {
		var captured bytes.Buffer
		out := stdout
		if jsonOut {
			out = &captured
		}
		rep, err = store.Wait(ctx, m, maxWait, out)
		rep.Output = captured.String()
	} else {
		rep, err = store.Stop(m, detach.StopGrace)
	}
	if err != nil {
		fmt.Fprintf(stderr, "quai verify %s: run %s: %v\n", action, m.ID, err)
		return 1
	}
	if jsonOut {
		writeJSON(stdout, rep)
	} else {
		fmt.Fprintln(stdout, describeRun(rep))
	}
	if !wait {
		if rep.State == detach.Running {
			return 1
		}
		return 0
	}
	return detach.ExitCode(rep.State)
}

func describeRun(rep detach.Report) string {
	head := fmt.Sprintf("quai run %s: %s", rep.ID, rep.State)
	switch rep.State {
	case detach.Running:
		return fmt.Sprintf("%s; rerun quai verify --wait %s", head, rep.ID)
	case detach.Dead:
		return fmt.Sprintf("%s: pid %d exited without an exit record; killed %d process group(s), released %d lock(s)",
			head, rep.PID, len(rep.KilledGroups), len(rep.ReleasedLocks))
	case detach.Failed:
		parts := []string{fmt.Sprintf("%s (exit %d)", head, rep.Exit.ExitCode)}
		if rep.Exit.Stopped {
			parts = append(parts, "stopped")
		}
		var res verify.Result
		if json.Unmarshal(rep.Exit.Result, &res) == nil {
			for _, s := range res.Steps {
				if s.Status == "failed" || s.Status == "mutated" {
					parts = append(parts, fmt.Sprintf("step %q %s: %s", s.Name, s.Status, s.Error))
				}
			}
		}
		return strings.Join(parts, "; ")
	default:
		return head
	}
}

func checkRunControlFlags(fs *flag.FlagSet, detachRun, wait, stop bool) string {
	if detachRun || (wait && stop) {
		return "use only one of --detach, --wait, and --stop"
	}
	if stop && isFlagSet(fs, "max") {
		return "--max applies only to --wait"
	}
	var extra []string
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "wait", "stop", "max", "json":
		default:
			extra = append(extra, "--"+f.Name)
		}
	})
	if len(extra) > 0 {
		return fmt.Sprintf("%s do not apply to --wait or --stop: pass them with --detach", strings.Join(extra, ", "))
	}
	return ""
}

func isFlagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

func forwardFlags(fs *flag.FlagSet, skip ...string) []string {
	var out []string
	fs.Visit(func(f *flag.Flag) {
		for _, s := range skip {
			if f.Name == s {
				return
			}
		}
		if values, ok := f.Value.(*multiFlag); ok {
			for _, v := range *values {
				out = append(out, "--"+f.Name+"="+v)
			}
			return
		}
		out = append(out, "--"+f.Name+"="+f.Value.String())
	})
	return out
}
