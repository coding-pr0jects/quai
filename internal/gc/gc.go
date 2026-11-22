package gc

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/coding-pr0jects/quai/internal/attest"
	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/lock"
	"github.com/coding-pr0jects/quai/internal/receipt"
	"github.com/coding-pr0jects/quai/internal/repo"
	"github.com/coding-pr0jects/quai/internal/verdict"
)

const (
	StampFileName = "last-prune"
	LockFileName  = "prune.lock"
)

// Item is one maintenance candidate. Kind is receipt, attestation_ref, verdict,
// verdict_ref, or lock; Location is local or remote; Reason is expired, unreadable,
// or stale.
type Item struct {
	Kind     string `json:"kind"`
	Name     string `json:"name"`
	Location string `json:"location,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

type Result struct {
	OK       bool     `json:"ok"`
	DryRun   bool     `json:"dry_run"`
	MaxAge   string   `json:"max_age"`
	Remote   string   `json:"remote,omitempty"`
	Removed  []Item   `json:"removed,omitempty"`
	Kept     int      `json:"kept"`
	Warnings []string `json:"warnings,omitempty"`
}

type Options struct {
	MaxAge time.Duration
	Remote string
	DryRun bool
	Now    func() time.Time
}

// Run computes and applies one maintenance pass. Every category is best effort: a
// transient git or filesystem error becomes a warning so the caller's primary command
// and the rest of the pass still complete. A dry run lists without mutating any ref,
// file, or directory.
func Run(ctx context.Context, g repo.Git, cfg *config.Config, opts Options) (Result, error) {
	if cfg == nil {
		return Result{}, errors.New("config is required")
	}
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = cfg.Attest.MaxAgeDur
	}
	if maxAge <= 0 {
		maxAge = 14 * 24 * time.Hour
	}
	remote := opts.Remote
	if remote == "" {
		remote = cfg.Attest.Remote
	}
	if remote == "" {
		remote = "origin"
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn().UTC()

	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		return Result{}, err
	}
	res := Result{
		OK:     true,
		DryRun: opts.DryRun,
		MaxAge: maxAge.String(),
		Remote: remote,
	}

	collect := func(kind, name, location, reason string) {
		res.Removed = append(res.Removed, Item{Kind: kind, Name: name, Location: location, Reason: reason})
	}
	warn := func(format string, args ...any) {
		res.Warnings = append(res.Warnings, fmt.Sprintf(format, args...))
	}

	store := receipt.NewStore(commonDir)
	store.MaxAge = maxAge
	store.Now = nowFn
	expiredReceipts, kept, err := store.Plan()
	if err != nil {
		warn("receipts: %v", err)
	} else {
		res.Kept += kept
		for _, path := range expiredReceipts {
			if opts.DryRun {
				collect("receipt", path, "local", "expired")
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				warn("receipt %s: %v", path, err)
				continue
			}
			collect("receipt", path, "local", "expired")
		}
	}

	pruned, err := attest.Prune(ctx, g, cfg, attest.PruneOptions{
		MaxAge: maxAge,
		Remote: remote,
		DryRun: opts.DryRun,
		Now:    nowFn,
	})
	if err != nil {
		warn("attestation refs: %v", err)
	}
	res.Kept += pruned.Kept
	for _, ref := range pruned.PrunedLocal {
		collect("attestation_ref", ref, "local", "expired")
	}
	for _, ref := range pruned.PrunedRemote {
		collect("attestation_ref", ref, "remote", "expired")
	}

	res.Kept += pruneVerdicts(ctx, g, commonDir, maxAge, now, opts.DryRun, collect, warn)
	res.Kept += pruneLocks(commonDir, now, opts.DryRun, collect, warn)

	sort.Slice(res.Removed, func(i, j int) bool {
		a, b := res.Removed[i], res.Removed[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Location < b.Location
	})
	return res, nil
}

func pruneVerdicts(
	ctx context.Context,
	g repo.Git,
	commonDir string,
	maxAge time.Duration,
	now time.Time,
	dryRun bool,
	collect func(kind, name, location, reason string),
	warn func(string, ...any),
) int {
	kept := 0
	dir := filepath.Join(commonDir, "quai", "verdicts")
	if entries, err := os.ReadDir(dir); err == nil {
		cutoff := now.Add(-maxAge)
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			ts, ok := verdictFileTime(entry, path)
			if !ok || !ts.Before(cutoff) {
				kept++
				continue
			}
			if dryRun {
				collect("verdict", path, "local", "expired")
				continue
			}
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				warn("verdict %s: %v", path, err)
				kept++
				continue
			}
			collect("verdict", path, "local", "expired")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		warn("verdicts: %v", err)
	}

	refs, err := g.ListRefs(ctx, verdict.RefPrefix)
	if err != nil {
		warn("verdict refs: %v", err)
		return kept
	}
	for _, ref := range refs {
		expired, reason := verdictRefExpired(ctx, g, ref, maxAge, now)
		if !expired {
			kept++
			continue
		}
		if dryRun {
			collect("verdict_ref", ref, "local", reason)
			continue
		}
		if err := g.DeleteLocalRef(ctx, ref); err != nil {
			warn("verdict ref %s: %v", ref, err)
			kept++
			continue
		}
		collect("verdict_ref", ref, "local", reason)
	}
	return kept
}

func verdictFileTime(entry os.DirEntry, path string) (time.Time, bool) {
	info, err := entry.Info()
	if err != nil {
		return time.Time{}, false
	}
	ts := info.ModTime()
	if raw, err := os.ReadFile(path); err == nil {
		var rec verdict.Record
		if json.Unmarshal(raw, &rec) == nil && !rec.CreatedAt.IsZero() {
			ts = rec.CreatedAt
		}
	}
	return ts, true
}

func verdictRefExpired(ctx context.Context, g repo.Git, ref string, maxAge time.Duration, now time.Time) (bool, string) {
	raw, ok, err := g.ReadRefBlob(ctx, ref, verdict.BlobFileName)
	if err != nil || !ok {
		return true, "unreadable"
	}
	var env attest.Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return true, "unreadable"
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return true, "unreadable"
	}
	var stmt verdict.Statement
	if err := json.Unmarshal(payload, &stmt); err != nil {
		return true, "unreadable"
	}
	created := stmt.Predicate.CreatedAt
	if created.IsZero() {
		return true, "unreadable"
	}
	if now.Sub(created) > maxAge {
		return true, "expired"
	}
	return false, ""
}

func pruneLocks(
	commonDir string,
	now time.Time,
	dryRun bool,
	collect func(kind, name, location, reason string),
	warn func(string, ...any),
) int {
	locksRoot := filepath.Join(commonDir, "quai", "locks")
	entries, err := os.ReadDir(locksRoot)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			warn("locks: %v", err)
		}
		return 0
	}
	kept := 0
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || !strings.HasSuffix(name, ".lock") {
			continue
		}
		path := filepath.Join(locksRoot, name)
		st := lock.InspectDir(path, lock.DefaultStaleAfter)
		if !st.Held || !st.Stale {
			kept++
			continue
		}
		if st.Owner == nil {
			info, statErr := os.Stat(path)
			if statErr != nil || now.Sub(info.ModTime()) < lock.DefaultMissingOwnerWarn {
				kept++
				continue
			}
		}
		if dryRun {
			collect("lock", path, "local", "stale")
			continue
		}
		removed, err := lock.RemoveStaleDir(path, lock.Options{})
		if err != nil {
			warn("lock %s: %v", path, err)
			kept++
			continue
		}
		if !removed {
			kept++
			continue
		}
		collect("lock", path, "local", "stale")
	}
	return kept
}

// StampPath returns the rate-limit file under <git-common-dir>/quai/.
func StampPath(commonDir string) string {
	return filepath.Join(commonDir, "quai", StampFileName)
}

// ReadStamp returns the last completed prune time, or false when absent or invalid.
func ReadStamp(commonDir string) (time.Time, bool) {
	raw, err := os.ReadFile(StampPath(commonDir))
	if err != nil {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(raw)))
	if err != nil {
		return time.Time{}, false
	}
	return ts.UTC(), true
}

// Due reports whether a prune should run now: it is due when the stamp is absent,
// invalid, or at least interval old.
func Due(last time.Time, ok bool, now time.Time, interval time.Duration) bool {
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	if !ok {
		return true
	}
	return !now.Before(last.Add(interval))
}

// WriteStamp records a completed prune atomically as an RFC3339Nano timestamp.
func WriteStamp(commonDir string, now time.Time) error {
	dir := filepath.Join(commonDir, "quai")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".last-prune-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := fmt.Fprintln(tmp, now.UTC().Format(time.RFC3339Nano)); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, StampPath(commonDir)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// Acquire serializes maintenance passes through the prune lock. Callers must release it.
func Acquire(ctx context.Context, g repo.Git, command string, wait time.Duration) (*lock.Lease, error) {
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		return nil, err
	}
	worktree, _ := g.TopLevel(ctx)
	return lock.AcquireDir(
		ctx,
		filepath.Join(commonDir, "quai", "locks", LockFileName),
		lock.Owner{Command: command, Worktree: worktree},
		lock.Options{Wait: wait, PollInterval: 25 * time.Millisecond},
	)
}

// MaybeAuto runs the best-effort prune after a primary command when [attest].auto_prune
// is enabled, at most once per [attest].prune_interval. The policy is read from the
// remote default branch, resolved strictly as refs/remotes/origin/<base>, so a PR branch
// cannot widen a destructive maintenance pass. It never returns an error and never
// changes the primary command's outcome.
func MaybeAuto(ctx context.Context, g repo.Git, cfg *config.Config, stderr io.Writer) {
	if cfg == nil {
		return
	}
	warn := func(format string, args ...any) {
		if stderr != nil {
			fmt.Fprintf(stderr, "quai: auto-prune: "+format+"\n", args...)
		}
	}
	baseCfg, err := configFromBase(ctx, g, cfg.Base)
	if err != nil {
		if cfg.Attest.AutoPrune {
			warn("skipped: %v", err)
		}
		return
	}
	if !baseCfg.Attest.AutoPrune {
		return
	}
	commonDir, err := g.CommonDir(ctx)
	if err != nil {
		warn("%v", err)
		return
	}
	interval := baseCfg.Attest.PruneIntervalDur
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	now := time.Now().UTC()
	if last, ok := ReadStamp(commonDir); !Due(last, ok, now, interval) {
		return
	}

	lease, err := Acquire(ctx, g, "quai auto-prune", 100*time.Millisecond)
	if err != nil {
		return
	}
	defer lease.Release()

	if last, ok := ReadStamp(commonDir); !Due(last, ok, time.Now().UTC(), interval) {
		return
	}
	res, err := Run(ctx, g, baseCfg, Options{Now: func() time.Time { return now }})
	if err != nil {
		warn("%v", err)
	}
	for _, w := range res.Warnings {
		warn("%s", w)
	}
	if ctx.Err() != nil {
		return
	}
	if err := WriteStamp(commonDir, now); err != nil {
		warn("write %s: %v", StampFileName, err)
	}
}

func configFromBase(ctx context.Context, g repo.Git, base string) (*config.Config, error) {
	ref, err := policyRef(ctx, g, base)
	if err != nil {
		return nil, err
	}
	raw, err := g.ShowFile(ctx, ref, config.FileName)
	if err != nil {
		return nil, err
	}
	return config.Parse(raw)
}

func policyRef(ctx context.Context, g repo.Git, base string) (string, error) {
	if err := plainBase(ctx, g, base); err != nil {
		return "", err
	}
	ref := "refs/remotes/origin/" + base
	if _, err := g.RevParse(ctx, ref); err != nil {
		return "", fmt.Errorf("base ref %s is unavailable: %w", ref, err)
	}
	defaultRef, err := g.SymbolicRef(ctx, "refs/remotes/origin/HEAD")
	if err != nil {
		return "", fmt.Errorf("cannot resolve refs/remotes/origin/HEAD (run git remote set-head origin --auto): %w", err)
	}
	if defaultRef != ref {
		return "", fmt.Errorf("base %q resolves to %s, not the remote default branch %s", base, ref, defaultRef)
	}
	return ref, nil
}

// BaseAutoPrune reports [attest].auto_prune from refs/remotes/origin/<base> without
// requiring refs/remotes/origin/HEAD, so callers can diagnose a missing default-branch ref.
func BaseAutoPrune(ctx context.Context, g repo.Git, cfg *config.Config) (bool, error) {
	if cfg == nil {
		return false, nil
	}
	if err := plainBase(ctx, g, cfg.Base); err != nil {
		return false, err
	}
	raw, err := g.ShowFile(ctx, "refs/remotes/origin/"+cfg.Base, config.FileName)
	if err != nil {
		return false, err
	}
	baseCfg, err := config.Parse(raw)
	if err != nil {
		return false, err
	}
	return baseCfg.Attest.AutoPrune, nil
}

func plainBase(ctx context.Context, g repo.Git, base string) error {
	if base == "" || base == "@" || base == "HEAD" || strings.Contains(base, "@{") {
		return fmt.Errorf("base %q is not a plain branch name", base)
	}
	if err := g.CheckRefFormatBranch(ctx, base); err != nil {
		return fmt.Errorf("base %q is not a plain branch name: %w", base, err)
	}
	return nil
}
