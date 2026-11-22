package forge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
)

func TestIsTransient(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"gh pr view 7: exit status 1 (Post \"https://api.github.com/graphql\": net/http: TLS handshake timeout)", true},
		{"gh pr checks 7: exit status 1 (Post \"https://api.github.com/graphql\": EOF)", true},
		{"gh pr view 7: exit status 1 (read tcp 10.0.0.2:5555->140.82.121.6:443: read: connection reset by peer)", true},
		{"gh pr view 7: exit status 1 (dial tcp 140.82.121.6:443: i/o timeout)", true},
		{"gh pr merge 7: exit status 1 (HTTP 502: Bad Gateway (https://api.github.com/graphql))", true},
		{"gh pr view 7: exit status 1 (HTTP 503: Service Unavailable)", true},
		{"gh pr merge 7: exit status 1 (HTTP 403: Resource not accessible by integration)", false},
		{"gh pr merge 7: exit status 1 (HTTP 422: Head branch was modified. Review and try the merge again.)", false},
		{"gh pr merge 7: exit status 1 (Pull request is not mergeable: the merge commit cannot be cleanly created. CONFLICTING)", false},
		{"gh pr merge 7: exit status 1 (Required status check \"ci\" is failing)", false},
		{"gh pr view 7: exit status 1 (GraphQL: Could not resolve to a PullRequest with the number of 7.)", false},
		{"parse gh pr view 7: invalid character 'E' looking for beginning of value", false},
	}
	for _, tc := range cases {
		if got := IsTransient(errors.New(tc.msg)); got != tc.want {
			t.Errorf("IsTransient(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
	if IsTransient(nil) {
		t.Error("IsTransient(nil) = true")
	}
	if IsTransient(fmt.Errorf("gh pr view 7: %w (EOF)", context.Canceled)) {
		t.Error("a cancelled context is never transient")
	}
}

type flakyForge struct {
	viewErrs  []error
	mergeErrs []error
	merged    bool
	headSHA   string
	views     int
	merges    int
}

func (f *flakyForge) ViewPR(ctx context.Context, number int) (PullRequest, error) {
	f.views++
	if len(f.viewErrs) > 0 {
		err := f.viewErrs[0]
		f.viewErrs = f.viewErrs[1:]
		return PullRequest{}, err
	}
	state := "OPEN"
	if f.merged {
		state = "MERGED"
	}
	return PullRequest{Number: number, State: state, HeadRefOid: f.headSHA}, nil
}

func (f *flakyForge) Checks(ctx context.Context, number int) ([]Check, error) { return nil, nil }

func (f *flakyForge) MergePR(ctx context.Context, number int, method config.MergeMethod, expectedHeadSHA string) error {
	f.merges++
	if len(f.mergeErrs) > 0 {
		err := f.mergeErrs[0]
		f.mergeErrs = f.mergeErrs[1:]
		return err
	}
	f.merged = true
	return nil
}

func (f *flakyForge) ListPRsByBase(ctx context.Context, baseBranch string) ([]PullRequest, error) {
	return nil, nil
}

func (f *flakyForge) EditPRBase(ctx context.Context, number int, newBase string) error { return nil }

func newTestRetry(f Forge) (*Retry, *[]time.Duration, *bytes.Buffer) {
	var slept []time.Duration
	var log bytes.Buffer
	return &Retry{
		Forge:  f,
		Delays: DefaultRetryDelays,
		Log:    &log,
		Sleep: func(ctx context.Context, d time.Duration) error {
			slept = append(slept, d)
			return ctx.Err()
		},
	}, &slept, &log
}

var (
	errTLS  = errors.New("gh pr view 7: exit status 1 (net/http: TLS handshake timeout)")
	err502  = errors.New("gh pr view 7: exit status 1 (HTTP 502: Bad Gateway)")
	err422  = errors.New("gh pr merge 7: exit status 1 (HTTP 422: Head branch was modified)")
	errLost = errors.New("gh pr merge 7: exit status 1 (Post \"https://api.github.com/graphql\": EOF)")
)

func TestRetryRecoversTransientErrors(t *testing.T) {
	ff := &flakyForge{viewErrs: []error{errTLS, err502}, headSHA: "abc"}
	r, slept, log := newTestRetry(ff)
	pr, err := r.ViewPR(context.Background(), 7)
	if err != nil || pr.Number != 7 {
		t.Fatalf("ViewPR = %+v, %v; want recovery after two transient errors", pr, err)
	}
	if ff.views != 3 || len(*slept) != 2 || (*slept)[0] != 2*time.Second || (*slept)[1] != 5*time.Second {
		t.Fatalf("views=%d slept=%v; want 3 calls with 2s then 5s backoff", ff.views, *slept)
	}
	if !bytes.Contains(log.Bytes(), []byte("retry 1/3 in 2s")) {
		t.Fatalf("retry not reported: %q", log.String())
	}
}

func TestRetryGivesUpAfterLastDelay(t *testing.T) {
	ff := &flakyForge{viewErrs: []error{errTLS, errTLS, errTLS, errTLS, errTLS}}
	r, slept, _ := newTestRetry(ff)
	if _, err := r.ViewPR(context.Background(), 7); !errors.Is(err, errTLS) {
		t.Fatalf("ViewPR error = %v; want the last transient error", err)
	}
	if ff.views != 4 || len(*slept) != 3 || (*slept)[2] != 10*time.Second {
		t.Fatalf("views=%d slept=%v; want 4 calls with 2s, 5s, 10s backoff", ff.views, *slept)
	}
}

func TestRetryNeverRetriesSemanticRefusal(t *testing.T) {
	ff := &flakyForge{mergeErrs: []error{err422}}
	r, slept, _ := newTestRetry(ff)
	if err := r.MergePR(context.Background(), 7, config.MergeSquash, "abc"); !errors.Is(err, err422) {
		t.Fatalf("MergePR error = %v; want the 422 refusal", err)
	}
	if ff.merges != 1 || len(*slept) != 0 {
		t.Fatalf("merges=%d slept=%v; a semantic refusal must not be retried", ff.merges, *slept)
	}
}

func TestRetryMergeConfirmsLostResponse(t *testing.T) {
	ff := &flakyForge{mergeErrs: []error{errLost}, headSHA: "abc"}
	r, _, _ := newTestRetry(&lostResponseForge{flakyForge: ff})
	if err := r.MergePR(context.Background(), 7, config.MergeSquash, "abc"); err != nil {
		t.Fatalf("MergePR = %v; want success once GitHub reports the PR merged", err)
	}
	if ff.merges != 1 {
		t.Fatalf("merges=%d; a merge GitHub already applied must not be sent again", ff.merges)
	}
}

// lostResponseForge applies the merge but loses gh's response, as on a dropped connection.
type lostResponseForge struct {
	*flakyForge
}

func (f *lostResponseForge) MergePR(ctx context.Context, number int, method config.MergeMethod, expectedHeadSHA string) error {
	err := f.flakyForge.MergePR(ctx, number, method, expectedHeadSHA)
	f.merged = true
	return err
}

func TestRetryStopsOnCancel(t *testing.T) {
	ff := &flakyForge{viewErrs: []error{errTLS, errTLS}}
	r, _, _ := newTestRetry(ff)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.ViewPR(ctx, 7); !errors.Is(err, context.Canceled) {
		t.Fatalf("ViewPR error = %v; want cancellation", err)
	}
	if ff.views != 1 {
		t.Fatalf("views=%d; a cancelled context must stop retrying", ff.views)
	}
}
