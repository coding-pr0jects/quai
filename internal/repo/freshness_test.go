package repo

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStaleBaseWarning(t *testing.T) {
	cases := []struct {
		name     string
		baseRef  string
		fetchAge time.Duration
		fetched  bool
		want     bool
	}{
		{"local base never warns", "main", 0, false, false},
		{"recent fetch is fresh", "origin/main", time.Minute, true, false},
		{"old fetch warns", "origin/main", StaleFetchAfter + time.Minute, true, true},
		{"missing FETCH_HEAD warns", "origin/main", 0, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := StaleBaseWarning(tc.baseRef, tc.fetchAge, tc.fetched)
			if (got != "") != tc.want {
				t.Fatalf("StaleBaseWarning(%q, %s, %v) = %q, want warning=%v", tc.baseRef, tc.fetchAge, tc.fetched, got, tc.want)
			}
			if tc.want && !strings.Contains(got, tc.baseRef) {
				t.Fatalf("warning %q does not name %q", got, tc.baseRef)
			}
		})
	}
}

func TestFetchHeadAge(t *testing.T) {
	ctx := context.Background()
	_, g := initTestRepo(t)

	age, fetched, err := FetchHeadAge(ctx, g, time.Now())
	if err != nil {
		t.Fatalf("FetchHeadAge: %v", err)
	}
	if fetched {
		t.Fatalf("expected no FETCH_HEAD, got age %s", age)
	}

	fetchHead, err := g.GitPath(ctx, "FETCH_HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fetchHead, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-2 * StaleFetchAfter).Truncate(time.Second)
	if err := os.Chtimes(fetchHead, when, when); err != nil {
		t.Fatal(err)
	}
	age, fetched, err = FetchHeadAge(ctx, g, time.Now())
	if err != nil || !fetched {
		t.Fatalf("FetchHeadAge = (%s, %v, %v)", age, fetched, err)
	}
	if age < StaleFetchAfter {
		t.Fatalf("age = %s, want at least %s", age, StaleFetchAfter)
	}
}
