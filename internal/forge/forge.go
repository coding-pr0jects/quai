package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/coding-pr0jects/quai/internal/config"
)

type PullRequest struct {
	Number            int    `json:"number"`
	State             string `json:"state"`
	IsDraft           bool   `json:"isDraft"`
	IsCrossRepository bool   `json:"isCrossRepository"`
	BaseRefName       string `json:"baseRefName"`
	HeadRefName       string `json:"headRefName"`
	HeadRefOid        string `json:"headRefOid"`
	MergeStateStatus  string `json:"mergeStateStatus"`
	URL               string `json:"url"`
	MergeCommit       *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
}

type Check struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Bucket string `json:"bucket"` // pass | fail | pending | skipping | cancel
}

type ChecksVerdict struct {
	Kind    string   `json:"kind"` // green | pending | red
	Missing []string `json:"missing,omitempty"`
	Failing []string `json:"failing,omitempty"`
	Pending []string `json:"pending,omitempty"`
}

// Forge abstracts GitHub PR operations behind a testable boundary.
type Forge interface {
	ViewPR(ctx context.Context, number int) (PullRequest, error)
	Checks(ctx context.Context, number int) ([]Check, error)
	MergePR(ctx context.Context, number int, method config.MergeMethod, expectedHeadSHA string) error
	ListPRsByBase(ctx context.Context, baseBranch string) ([]PullRequest, error)
	EditPRBase(ctx context.Context, number int, newBase string) error
}

// EvaluateChecks computes whether all required checks are green, still pending, or failing.
func EvaluateChecks(checks []Check, required []string) ChecksVerdict {
	pendingSet := make(map[string]struct{})
	seenSet := make(map[string]struct{})
	passedSet := make(map[string]struct{})
	statesByName := make(map[string][]string)
	failingSet := make(map[string]struct{})

	for _, ch := range checks {
		seenSet[ch.Name] = struct{}{}
		statesByName[ch.Name] = append(statesByName[ch.Name], ch.State)
		switch ch.Bucket {
		case "pending":
			pendingSet[ch.Name] = struct{}{}
		case "pass":
			passedSet[ch.Name] = struct{}{}
		case "fail", "cancel":
			failingSet[fmt.Sprintf("%s (%s)", ch.Name, ch.State)] = struct{}{}
		}
	}

	var missing []string
	for _, req := range required {
		if _, ok := seenSet[req]; !ok {
			missing = append(missing, req)
			continue
		}
		if _, pend := pendingSet[req]; pend {
			continue
		}
		if _, pass := passedSet[req]; !pass {
			failingSet[fmt.Sprintf("%s (%s)", req, strings.Join(statesByName[req], ", "))] = struct{}{}
		}
	}

	var failing []string
	for f := range failingSet {
		failing = append(failing, f)
	}
	var pending []string
	for p := range pendingSet {
		pending = append(pending, p)
	}
	sort.Strings(missing)
	sort.Strings(failing)
	sort.Strings(pending)

	if len(failing) > 0 || len(missing) > 0 {
		return ChecksVerdict{Kind: "red", Missing: missing, Failing: failing, Pending: pending}
	}
	if len(pending) > 0 {
		return ChecksVerdict{Kind: "pending", Pending: pending}
	}
	return ChecksVerdict{Kind: "green"}
}

type GHForge struct {
	Dir string
}

func NewGHForge(dir string) *GHForge {
	return &GHForge{Dir: dir}
}

func (f *GHForge) run(ctx context.Context, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Dir = f.Dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func (f *GHForge) ViewPR(ctx context.Context, number int) (PullRequest, error) {
	fields := "number,state,isDraft,isCrossRepository,baseRefName,headRefName,headRefOid,mergeStateStatus,url,mergeCommit"
	out, errOut, err := f.run(ctx, "pr", "view", strconv.Itoa(number), "--json", fields)
	if err != nil {
		return PullRequest{}, fmt.Errorf("gh pr view %d: %w (%s)", number, err, strings.TrimSpace(string(errOut)))
	}
	var pr PullRequest
	if err := json.Unmarshal(out, &pr); err != nil {
		return PullRequest{}, fmt.Errorf("parse gh pr view %d: %w", number, err)
	}
	return pr, nil
}

func (f *GHForge) Checks(ctx context.Context, number int) ([]Check, error) {
	out, errOut, err := f.run(ctx, "pr", "checks", strconv.Itoa(number), "--json", "name,state,bucket")
	if err != nil {
		errText := strings.TrimSpace(string(errOut))
		if strings.Contains(strings.ToLower(errText), "no checks reported") {
			return nil, nil
		}
		return nil, fmt.Errorf("gh pr checks %d: %w (%s)", number, err, errText)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var checks []Check
	if err := json.Unmarshal(out, &checks); err != nil {
		return nil, fmt.Errorf("parse gh pr checks %d: %w", number, err)
	}
	return checks, nil
}

// MergePR merges the pull request pinned to expectedHeadSHA. Never passes --admin or --delete-branch.
func (f *GHForge) MergePR(ctx context.Context, number int, method config.MergeMethod, expectedHeadSHA string) error {
	args := []string{
		"pr", "merge", strconv.Itoa(number),
		"--" + string(method),
		"--match-head-commit", expectedHeadSHA,
	}
	out, errOut, err := f.run(ctx, args...)
	if err != nil {
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return fmt.Errorf("gh pr merge %d: %w (%s)", number, err, msg)
	}
	return nil
}

// ListPRsByBase returns open pull requests targeting baseBranch.
func (f *GHForge) ListPRsByBase(ctx context.Context, baseBranch string) ([]PullRequest, error) {
	fields := "number,state,isDraft,isCrossRepository,baseRefName,headRefName,headRefOid,mergeStateStatus,url"
	out, errOut, err := f.run(ctx, "pr", "list", "--state", "open", "--base", baseBranch, "--json", fields)
	if err != nil {
		return nil, fmt.Errorf("gh pr list --base %s: %w (%s)", baseBranch, err, strings.TrimSpace(string(errOut)))
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return nil, nil
	}
	var prs []PullRequest
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, fmt.Errorf("parse gh pr list --base %s: %w", baseBranch, err)
	}
	return prs, nil
}

// EditPRBase retargets a pull request to a new base branch.
func (f *GHForge) EditPRBase(ctx context.Context, number int, newBase string) error {
	out, errOut, err := f.run(ctx, "pr", "edit", strconv.Itoa(number), "--base", newBase)
	if err != nil {
		msg := strings.TrimSpace(string(errOut))
		if msg == "" {
			msg = strings.TrimSpace(string(out))
		}
		return fmt.Errorf("gh pr edit %d --base %s: %w (%s)", number, newBase, err, msg)
	}
	return nil
}
