package status

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/coding-pr0jects/quai/internal/attest"
	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/detach"
	"github.com/coding-pr0jects/quai/internal/gc"
	"github.com/coding-pr0jects/quai/internal/hooks"
	"github.com/coding-pr0jects/quai/internal/land"
	"github.com/coding-pr0jects/quai/internal/lock"
	"github.com/coding-pr0jects/quai/internal/repo"
)

type PoolStatus struct {
	Name            string              `json:"name"`
	Capacity        int                 `json:"capacity"`
	MinFreeMemBytes uint64              `json:"min_free_mem_bytes,omitempty"`
	Held            int                 `json:"held"`
	Stale           int                 `json:"stale"`
	Slots           []lock.InspectState `json:"slots,omitempty"`
	Waiters         []lock.Owner        `json:"waiters,omitempty"`
}

type InFlightStatus struct {
	Key   string            `json:"key"`
	State lock.InspectState `json:"state"`
}

type Report struct {
	OK                bool              `json:"ok"`
	RepoRoot          string            `json:"repo_root"`
	CommonDir         string            `json:"common_dir"`
	AvailableMemBytes uint64            `json:"available_mem_bytes,omitempty"`
	LandLock          lock.InspectState `json:"land_lock"`
	Pools             []PoolStatus      `json:"pools,omitempty"`
	InFlight          []InFlightStatus  `json:"in_flight,omitempty"`
	LandWorktrees     []string          `json:"land_worktrees,omitempty"`
	ReceiptCount      int               `json:"receipt_count"`
	AttestCount       int               `json:"attestation_count"`
	Runs              []detach.Status   `json:"runs,omitempty"`
	AutoPrune         bool              `json:"auto_prune"`
	PruneInterval     string            `json:"prune_interval,omitempty"`
	LastPrune         *time.Time        `json:"last_prune,omitempty"`
}

type CheckItem struct {
	Name    string `json:"name"`
	Status  string `json:"status"` // ok | warn | fail
	Message string `json:"message"`
	Fix     string `json:"fix,omitempty"`
}

type DoctorReport struct {
	OK     bool        `json:"ok"`
	Checks []CheckItem `json:"checks"`
}

var gitVersionRe = regexp.MustCompile(`git version (\d+)\.(\d+)`)

// ParseGitVersion extracts major and minor numbers from `git --version` output.
func ParseGitVersion(output string) (major, minor int, ok bool) {
	m := gitVersionRe.FindStringSubmatch(output)
	if len(m) != 3 {
		return 0, 0, false
	}
	maj, err1 := strconv.Atoi(m[1])
	min, err2 := strconv.Atoi(m[2])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return maj, min, true
}

// Inspect collects read-only state for active locks, slot pools, in-flight verifications,
// temporary land worktrees, local receipts, and local signed attestations.
func Inspect(ctx context.Context, g repo.Git, cfg *config.Config) (Report, error) {
	topLevel, err := g.TopLevel(ctx)
	if err != nil {
		return Report{}, err
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		return Report{}, err
	}
	locksRoot := filepath.Join(commonDir, "quai", "locks")
	receiptsDir := filepath.Join(commonDir, "quai", "receipts")

	rep := Report{
		OK:        true,
		RepoRoot:  topLevel,
		CommonDir: commonDir,
		LandLock:  lock.InspectDir(filepath.Join(locksRoot, "land.lock"), lock.DefaultStaleAfter),
	}
	if avail, ok := lock.ReadAvailableMemory(); ok {
		rep.AvailableMemBytes = avail
	}

	if cfg != nil && len(cfg.Slots) > 0 {
		names := make([]string, 0, len(cfg.Slots))
		for name := range cfg.Slots {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, pool := range names {
			sp := cfg.Slots[pool]
			ps := PoolStatus{
				Name:            pool,
				Capacity:        sp.Capacity,
				MinFreeMemBytes: sp.MinFreeMemBytes,
				Waiters:         lock.InspectWaiters(filepath.Join(locksRoot, fmt.Sprintf("slot-%s.queue", pool)), lock.DefaultStaleAfter),
			}
			for i := 0; i < sp.Capacity; i++ {
				dir := filepath.Join(locksRoot, fmt.Sprintf("slot-%s-%d.lock", pool, i))
				st := lock.InspectDir(dir, lock.DefaultStaleAfter)
				if st.Held {
					ps.Held++
					if st.Stale {
						ps.Stale++
					}
					ps.Slots = append(ps.Slots, st)
				}
			}
			rep.Pools = append(rep.Pools, ps)
		}
	}

	if entries, err := os.ReadDir(locksRoot); err == nil {
		var inflightNames []string
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), "inflight-") && strings.HasSuffix(e.Name(), ".lock") {
				inflightNames = append(inflightNames, e.Name())
			}
		}
		sort.Strings(inflightNames)
		for _, name := range inflightNames {
			key := strings.TrimSuffix(strings.TrimPrefix(name, "inflight-"), ".lock")
			rep.InFlight = append(rep.InFlight, InFlightStatus{
				Key:   key,
				State: lock.InspectDir(filepath.Join(locksRoot, name), lock.DefaultStaleAfter),
			})
		}
	}

	if entries, err := os.ReadDir(filepath.Join(commonDir, "quai")); err == nil {
		for _, e := range entries {
			if e.IsDir() && strings.HasPrefix(e.Name(), land.TempWorktreePrefix) {
				rep.LandWorktrees = append(rep.LandWorktrees, filepath.Join(commonDir, "quai", e.Name()))
			}
		}
	}

	if entries, err := os.ReadDir(receiptsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
				rep.ReceiptCount++
			}
		}
	}

	if refs, err := g.ListRefs(ctx, attest.RefPrefix); err == nil {
		rep.AttestCount = len(refs)
	}
	rep.Runs = detach.NewStore(commonDir).Active()

	if cfg != nil {
		rep.AutoPrune = cfg.Attest.AutoPrune
		if cfg.Attest.PruneIntervalDur > 0 {
			rep.PruneInterval = cfg.Attest.PruneIntervalDur.String()
		}
	}
	if last, ok := gc.ReadStamp(commonDir); ok {
		rep.LastPrune = &last
	}

	return rep, nil
}

// Doctor checks git version, .quai.toml, hook wiring, external tools, attestation setup,
// stale locks, and leftover land worktrees in the current repository.
func Doctor(ctx context.Context, g repo.Git) DoctorReport {
	rep := DoctorReport{OK: true}
	add := func(item CheckItem) {
		if item.Status == "fail" {
			rep.OK = false
		}
		rep.Checks = append(rep.Checks, item)
	}

	// 1. git >= 2.38
	if out, err := exec.CommandContext(ctx, "git", "--version").Output(); err != nil {
		add(CheckItem{Name: "git", Status: "fail", Message: fmt.Sprintf("git not executable: %v", err), Fix: "Install git 2.38 or newer"})
	} else if maj, min, ok := ParseGitVersion(string(out)); !ok {
		add(CheckItem{Name: "git", Status: "warn", Message: fmt.Sprintf("unrecognized git version %q", strings.TrimSpace(string(out)))})
	} else if maj < 2 || (maj == 2 && min < 38) {
		add(CheckItem{Name: "git", Status: "fail", Message: fmt.Sprintf("git %d.%d is older than 2.38", maj, min), Fix: "Upgrade git to 2.38+ for git merge-tree --write-tree"})
	} else {
		add(CheckItem{Name: "git", Status: "ok", Message: fmt.Sprintf("git %d.%d", maj, min)})
	}

	// 2. Repository & .quai.toml
	topLevel, err := g.TopLevel(ctx)
	if err != nil {
		add(CheckItem{Name: "repo", Status: "fail", Message: fmt.Sprintf("not inside a git repository: %v", err)})
		return rep
	}
	cfg, cfgErr := config.Load(topLevel)
	if cfgErr != nil {
		add(CheckItem{Name: "config", Status: "fail", Message: cfgErr.Error(), Fix: "Create a valid .quai.toml at the repository root"})
		return rep
	}
	add(CheckItem{Name: "config", Status: "ok", Message: fmt.Sprintf(".quai.toml valid (%d step(s), %d slot pool(s))", len(cfg.Steps), len(cfg.Slots))})

	add(checkAutoPrune(ctx, g, cfg))

	// 3. pre-push hook wiring
	if len(cfg.PrePushSteps()) > 0 {
		add(checkPrePushHook(ctx, g, topLevel))
	} else {
		add(CheckItem{Name: "hooks", Status: "ok", Message: "no pre-push steps configured"})
	}

	// 4. gh CLI
	if ghPath, err := exec.LookPath("gh"); err != nil {
		add(CheckItem{Name: "gh", Status: "warn", Message: "gh CLI not found on PATH (needed only for quai land)", Fix: "Install GitHub CLI (gh) and run gh auth login"})
	} else {
		add(CheckItem{Name: "gh", Status: "ok", Message: ghPath})
	}

	// 5. ssh-keygen & attestation readiness
	attestUsed := cfg.Attest.AllowedSigners != "" || cfg.Attest.SigningKey != "" || len(cfg.Attest.Signers) > 0 || cfg.Attest.AutoSign || cfg.Attest.AutoPush || os.Getenv("QUAI_ALLOWED_SIGNERS") != ""
	if sshPath, err := exec.LookPath("ssh-keygen"); err != nil {
		st := "warn"
		if attestUsed {
			st = "fail"
		}
		add(CheckItem{Name: "ssh-keygen", Status: st, Message: "ssh-keygen not found on PATH", Fix: "Install OpenSSH client tools"})
	} else {
		add(CheckItem{Name: "ssh-keygen", Status: "ok", Message: sshPath})
	}

	if attestUsed {
		if cfg.Attest.SigningKey != "" || cfg.Attest.AutoSign || cfg.Attest.AutoPush || os.Getenv("QUAI_SIGNING_KEY") != "" {
			keyPath, err := attest.ResolveSigningKey(ctx, g, cfg, "")
			if err != nil {
				add(CheckItem{Name: "attest-signing-key", Status: "warn", Message: err.Error(), Fix: "Set git config --global user.signingkey ~/.ssh/id_ed25519.pub or QUAI_SIGNING_KEY"})
			} else if _, statErr := os.Stat(keyPath); statErr != nil {
				add(CheckItem{Name: "attest-signing-key", Status: "fail", Message: fmt.Sprintf("signing key not found: %s", keyPath), Fix: "Check [attest].signing_key, QUAI_SIGNING_KEY, or git user.signingkey"})
			} else {
				add(CheckItem{Name: "attest-signing-key", Status: "ok", Message: keyPath})
			}
		}
		if cfg.Attest.AllowedSigners != "" || os.Getenv("QUAI_ALLOWED_SIGNERS") != "" {
			allowedPath, err := attest.ResolveAllowedSigners(topLevel, cfg, "")
			if err != nil {
				add(CheckItem{Name: "attest-allowed-signers", Status: "fail", Message: err.Error()})
			} else if _, statErr := os.Stat(allowedPath); statErr != nil {
				add(CheckItem{Name: "attest-allowed-signers", Status: "fail", Message: fmt.Sprintf("allowed_signers file not found: %s", allowedPath), Fix: "Create the allowed_signers file or unset QUAI_ALLOWED_SIGNERS"})
			} else {
				add(CheckItem{Name: "attest-allowed-signers", Status: "ok", Message: allowedPath})
			}
		}
	}

	// 6. Lock health
	if st, err := Inspect(ctx, g, cfg); err == nil {
		staleCount := 0
		if st.LandLock.Held && st.LandLock.Stale {
			staleCount++
		}
		for _, p := range st.Pools {
			staleCount += p.Stale
		}
		for _, f := range st.InFlight {
			if f.State.Held && f.State.Stale {
				staleCount++
			}
		}
		if staleCount > 0 {
			add(CheckItem{Name: "locks", Status: "warn", Message: fmt.Sprintf("%d stale lock(s) detected (will be recovered automatically on next acquire)", staleCount)})
		} else {
			add(CheckItem{Name: "locks", Status: "ok", Message: "no stale locks"})
		}
		for _, r := range st.Runs {
			if r.State == detach.Dead {
				add(CheckItem{Name: "detached-runs", Status: "warn", Message: fmt.Sprintf("detached run %s died without an exit record (pid %d)", r.ID, r.PID), Fix: "quai verify --wait " + r.ID})
			}
		}
		if len(st.LandWorktrees) > 0 && !(st.LandLock.Held && !st.LandLock.Stale) {
			add(CheckItem{
				Name:    "land-worktrees",
				Status:  "warn",
				Message: fmt.Sprintf("%d temporary land worktree(s) left while no quai land holds land.lock: %s", len(st.LandWorktrees), strings.Join(st.LandWorktrees, ", ")),
				Fix:     "If no quai land --dry-run is running either, remove each with git worktree remove --force <path>",
			})
		}
	}

	return rep
}

func checkAutoPrune(ctx context.Context, g repo.Git, cfg *config.Config) CheckItem {
	const name = "auto-prune"
	if head, err := g.SymbolicRef(ctx, "refs/remotes/origin/HEAD"); err == nil {
		return CheckItem{Name: name, Status: "ok", Message: "remote default branch " + head}
	}
	enabled, err := gc.BaseAutoPrune(ctx, g, cfg)
	if err != nil {
		return CheckItem{Name: name, Status: "ok", Message: "no trusted base policy to check"}
	}
	if enabled {
		return CheckItem{
			Name:    name,
			Status:  "warn",
			Message: "automatic pruning is skipped because refs/remotes/origin/HEAD is unset",
			Fix:     "Run git remote set-head origin --auto",
		}
	}
	return CheckItem{Name: name, Status: "ok", Message: "automatic pruning is not enabled on the base branch"}
}

func checkPrePushHook(ctx context.Context, g repo.Git, topLevel string) CheckItem {
	customHooksPath, _ := g.ConfigGet(ctx, "core.hooksPath")
	var hookPath string
	if customHooksPath != "" {
		if filepath.IsAbs(customHooksPath) {
			hookPath = filepath.Join(customHooksPath, "pre-push")
		} else {
			hookPath = filepath.Join(topLevel, customHooksPath, "pre-push")
		}
	} else {
		p, err := g.GitPath(ctx, "hooks/pre-push")
		if err != nil {
			return CheckItem{Name: "hooks", Status: "warn", Message: fmt.Sprintf("could not locate pre-push hook: %v", err)}
		}
		hookPath = p
	}
	raw, err := os.ReadFile(hookPath)
	if err != nil {
		return CheckItem{Name: "hooks", Status: "warn", Message: fmt.Sprintf("pre-push hook not installed at %s", hookPath), Fix: "Run quai hooks install"}
	}
	body := string(raw)
	if strings.Contains(body, hooks.ManagedMarker) || strings.Contains(body, "quai hooks pre-push") {
		return CheckItem{Name: "hooks", Status: "ok", Message: fmt.Sprintf("pre-push wired at %s", hookPath)}
	}
	return CheckItem{Name: "hooks", Status: "warn", Message: fmt.Sprintf("%s exists but does not call quai hooks pre-push", hookPath), Fix: "Chain: " + hooks.ChainSnippet}
}
