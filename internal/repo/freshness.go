package repo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// StaleFetchAfter is the age at which this worktree's last fetch no longer vouches
// for origin/<base>.
const StaleFetchAfter = 10 * time.Minute

// FetchHeadAge reports how long ago this worktree fetched, using its FETCH_HEAD file.
func FetchHeadAge(ctx context.Context, g Git, now time.Time) (time.Duration, bool, error) {
	p, err := g.GitPath(ctx, "FETCH_HEAD")
	if err != nil {
		return 0, false, err
	}
	info, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	age := now.Sub(info.ModTime())
	if age < 0 {
		age = 0
	}
	return age, true, nil
}

// StaleBaseWarning warns when baseRef is a remote-tracking ref and this worktree's
// last fetch is unknown or older than StaleFetchAfter.
func StaleBaseWarning(baseRef string, fetchAge time.Duration, fetched bool) string {
	if !strings.HasPrefix(baseRef, "origin/") {
		return ""
	}
	if !fetched {
		return fmt.Sprintf("%s may be stale: this worktree has no FETCH_HEAD; run git fetch origin", baseRef)
	}
	if fetchAge > StaleFetchAfter {
		return fmt.Sprintf(
			"%s may be stale: last fetch was %s ago (threshold %s); run git fetch origin",
			baseRef, fetchAge.Round(time.Second), StaleFetchAfter,
		)
	}
	return ""
}
