package forge

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/coding-pr0jects/quai/internal/config"
)

// DefaultRetryDelays is the backoff between attempts of a forge call that failed on a transient network error.
var DefaultRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}

// gh prints Go net/http and crypto/tls errors verbatim, and "HTTP <code>: <text>" for API status errors.
var (
	transientMarkers = []string{"tls handshake timeout", "connection reset", "i/o timeout"}
	eofWord          = regexp.MustCompile(`\bEOF\b`)
	httpServerError  = regexp.MustCompile(`\bHTTP 5\d\d\b`)
	httpClientError  = regexp.MustCompile(`\bHTTP 4\d\d\b`)
)

// IsTransient reports whether err is a network failure worth retrying: a TLS handshake timeout,
// an EOF, a connection reset, an i/o timeout, or an HTTP 5xx. A semantic refusal (a 4xx, a conflict,
// a failing check) and a cancelled context are never transient.
func IsTransient(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := err.Error()
	if httpClientError.MatchString(msg) {
		return false
	}
	lower := strings.ToLower(msg)
	for _, m := range transientMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return eofWord.MatchString(msg) || httpServerError.MatchString(msg)
}

// Retry is a Forge that retries the calls of Forge failing on a transient network error,
// waiting Delays[i] before retry i+1.
type Retry struct {
	Forge  Forge
	Delays []time.Duration
	Log    io.Writer
	Sleep  func(ctx context.Context, d time.Duration) error
}

// NewRetry wraps f with DefaultRetryDelays, reporting each retry to log.
func NewRetry(f Forge, log io.Writer) *Retry {
	return &Retry{Forge: f, Delays: DefaultRetryDelays, Log: log, Sleep: sleepCtx}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func retry[T any](ctx context.Context, r *Retry, call func() (T, error)) (T, error) {
	v, err := call()
	for attempt, delay := range r.Delays {
		if !IsTransient(err) {
			return v, err
		}
		if r.Log != nil {
			fmt.Fprintf(r.Log, "quai: transient forge error, retry %d/%d in %s: %v\n", attempt+1, len(r.Delays), delay, err)
		}
		if sleepErr := r.Sleep(ctx, delay); sleepErr != nil {
			return v, errors.Join(err, sleepErr)
		}
		v, err = call()
	}
	return v, err
}

func (r *Retry) ViewPR(ctx context.Context, number int) (PullRequest, error) {
	return retry(ctx, r, func() (PullRequest, error) { return r.Forge.ViewPR(ctx, number) })
}

func (r *Retry) Checks(ctx context.Context, number int) ([]Check, error) {
	return retry(ctx, r, func() ([]Check, error) { return r.Forge.Checks(ctx, number) })
}

// MergePR retries a merge whose response was lost only after confirming the PR is not already
// merged at expectedHeadSHA, so a merge that reached GitHub is reported as done, not failed.
func (r *Retry) MergePR(ctx context.Context, number int, method config.MergeMethod, expectedHeadSHA string) error {
	tried := false
	_, err := retry(ctx, r, func() (struct{}, error) {
		if tried {
			if pr, err := r.Forge.ViewPR(ctx, number); err == nil && pr.State == "MERGED" && pr.HeadRefOid == expectedHeadSHA {
				return struct{}{}, nil
			}
		}
		tried = true
		return struct{}{}, r.Forge.MergePR(ctx, number, method, expectedHeadSHA)
	})
	return err
}

func (r *Retry) ListPRsByBase(ctx context.Context, baseBranch string) ([]PullRequest, error) {
	return retry(ctx, r, func() ([]PullRequest, error) { return r.Forge.ListPRsByBase(ctx, baseBranch) })
}

func (r *Retry) EditPRBase(ctx context.Context, number int, newBase string) error {
	_, err := retry(ctx, r, func() (struct{}, error) { return struct{}{}, r.Forge.EditPRBase(ctx, number, newBase) })
	return err
}
