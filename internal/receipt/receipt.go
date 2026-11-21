package receipt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
	"github.com/coding-pr0jects/quai/internal/glob"
	"github.com/coding-pr0jects/quai/internal/repo"
)

const (
	CurrentVersion  = 1
	DefaultMaxFiles = 200
	DefaultMaxAge   = 14 * 24 * time.Hour
)

type Receipt struct {
	Version     int       `json:"version"`
	Fingerprint string    `json:"fingerprint"`
	TreeSHA     string    `json:"tree_sha"`
	HeadSHA     string    `json:"head_sha"`
	BaseSHA     string    `json:"base_sha"`
	Step        string    `json:"step"`
	Passed      bool      `json:"passed"`
	DurationMS  int64     `json:"duration_ms"`
	CreatedAt   time.Time `json:"created_at"`
}

type Store struct {
	Dir      string
	MaxFiles int
	MaxAge   time.Duration
	Now      func() time.Time
}

func NewStore(commonDir string) *Store {
	return &Store{
		Dir:      filepath.Join(commonDir, "quai", "receipts"),
		MaxFiles: DefaultMaxFiles,
		MaxAge:   DefaultMaxAge,
		Now:      time.Now,
	}
}

func (s *Store) pathFor(stepName, fingerprint string) string {
	return filepath.Join(s.Dir, fmt.Sprintf("%s-%s.json", stepName, fingerprint))
}

// Lookup returns a valid passing receipt for (stepName, fingerprint) if present.
func (s *Store) Lookup(stepName, fingerprint string) (*Receipt, bool, error) {
	if fingerprint == "" {
		return nil, false, nil
	}
	data, err := os.ReadFile(s.pathFor(stepName, fingerprint))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("read receipt: %w", err)
	}
	var r Receipt
	if err := json.Unmarshal(data, &r); err != nil {
		return nil, false, nil
	}
	if r.Version != CurrentVersion || !r.Passed || r.Step != stepName || r.Fingerprint != fingerprint {
		return nil, false, nil
	}
	return &r, true, nil
}

// Write atomically persists a passing receipt and prunes stale files.
func (s *Store) Write(r Receipt) error {
	if !r.Passed {
		return errors.New("refusing to write a failing receipt")
	}
	if r.Fingerprint == "" || r.Step == "" {
		return errors.New("receipt requires step and fingerprint")
	}
	if r.Version == 0 {
		r.Version = CurrentVersion
	}
	if r.CreatedAt.IsZero() {
		r.CreatedAt = s.now().UTC()
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return fmt.Errorf("create receipts dir: %w", err)
	}
	data, err := json.MarshalIndent(r, "", "\t")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Dir, ".receipt-*")
	if err != nil {
		return fmt.Errorf("create temp receipt: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.pathFor(r.Step, r.Fingerprint)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return s.Prune()
}

// Prune removes receipts older than MaxAge and trims to at most MaxFiles newest entries.
func (s *Store) Prune() error {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	maxAge := s.MaxAge
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	maxFiles := s.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}
	cutoff := s.now().Add(-maxAge)

	type fileAge struct {
		path string
		ts   time.Time
	}
	var kept []fileAge
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		full := filepath.Join(s.Dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		ts := info.ModTime()
		if raw, err := os.ReadFile(full); err == nil {
			var r Receipt
			if json.Unmarshal(raw, &r) == nil && !r.CreatedAt.IsZero() {
				ts = r.CreatedAt
			}
		}
		if ts.Before(cutoff) {
			_ = os.Remove(full)
			continue
		}
		kept = append(kept, fileAge{path: full, ts: ts})
	}
	if len(kept) <= maxFiles {
		return nil
	}
	sort.Slice(kept, func(i, j int) bool {
		return kept[i].ts.After(kept[j].ts)
	})
	for _, old := range kept[maxFiles:] {
		_ = os.Remove(old.path)
	}
	return nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Fingerprint computes a content-addressed sha256 over step.Run and all tracked files
// in treeSHA matching step.Inputs or step.Lockfiles. When step.Run references {{base}},
// the matching input hashes from baseSHA are also included so unrelated base moves preserve cache hits.
func Fingerprint(ctx context.Context, g repo.Git, step config.Step, treeSHA, baseSHA string) (string, error) {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "v%d\nstep:%s\nrun:%s\n", CurrentVersion, step.Name, step.Run)

	usesBase := strings.Contains(step.Run, "{{base}}")
	if isWholeTree(step.Inputs) && len(step.Lockfiles) == 0 {
		_, _ = fmt.Fprintf(h, "tree:%s\n", treeSHA)
		if usesBase {
			_, _ = fmt.Fprintf(h, "base:%s\n", baseSHA)
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}

	if err := hashMatchingTree(ctx, g, h, "tree", treeSHA, step.Inputs, step.Lockfiles); err != nil {
		return "", err
	}
	if usesBase && baseSHA != "" {
		baseTree, err := g.TreeSHA(ctx, baseSHA)
		if err != nil {
			return "", fmt.Errorf("resolve base tree %s: %w", baseSHA, err)
		}
		if err := hashMatchingTree(ctx, g, h, "base", baseTree, step.Inputs, step.Lockfiles); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashMatchingTree(ctx context.Context, g repo.Git, h interface{ Write([]byte) (int, error) }, label, treeSHA string, inputs, lockfiles []string) error {
	entries, err := g.ListTree(ctx, treeSHA)
	if err != nil {
		return fmt.Errorf("list tree %s: %w", treeSHA, err)
	}
	var matched []repo.TreeEntry
	for _, e := range entries {
		if glob.MatchAny(inputs, e.Path) || glob.MatchAny(lockfiles, e.Path) {
			matched = append(matched, e)
		}
	}
	sort.Slice(matched, func(i, j int) bool { return matched[i].Path < matched[j].Path })
	_, _ = fmt.Fprintf(h, "%s:\n", label)
	for _, e := range matched {
		_, _ = fmt.Fprintf(h, "%s:%s\n", e.Path, e.BlobSHA)
	}
	return nil
}

func isWholeTree(inputs []string) bool {
	if len(inputs) == 0 {
		return true
	}
	for _, p := range inputs {
		if p == "**" {
			return true
		}
	}
	return false
}
