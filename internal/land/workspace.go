package land

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/coding-pr0jects/quai/internal/forge"
	"github.com/coding-pr0jects/quai/internal/repo"
)

// TempWorktreePrefix starts the name of the temporary worktree under <git-common-dir>/quai/
// in which quai land rebases and verifies a PR whose branch the caller does not have checked out.
const TempWorktreePrefix = "land-worktree-"

const tempWorktreeRemoveTimeout = 30 * time.Second

type workspace struct {
	caller     repo.Git
	onPRBranch bool
	tempPath   string
	temp       repo.Git
}

func newWorkspace(g repo.Git, commonDir, branch string, prNumber int, prHead string) *workspace {
	return &workspace{
		caller:     g,
		onPRBranch: branch == prHead,
		tempPath:   filepath.Join(commonDir, "quai", fmt.Sprintf("%s%d-%d", TempWorktreePrefix, prNumber, os.Getpid())),
	}
}

func (w *workspace) prHead(ctx context.Context, pr forge.PullRequest) (bool, string, error) {
	if w.onPRBranch {
		clean, _, err := w.caller.IsClean(ctx)
		if err != nil {
			return false, "", err
		}
		headSHA, err := w.caller.HeadSHA(ctx)
		return clean, headSHA, err
	}
	remoteRef := "refs/remotes/origin/" + pr.HeadRefName
	if err := w.caller.Fetch(ctx, "origin", "+refs/heads/"+pr.HeadRefName+":"+remoteRef); err != nil {
		return false, "", err
	}
	headSHA, err := w.caller.RevParse(ctx, remoteRef)
	return true, headSHA, err
}

func (w *workspace) open(ctx context.Context, headSHA string) (repo.Git, error) {
	if w.onPRBranch {
		return w.caller, nil
	}
	if w.temp != nil {
		return w.temp, w.temp.ResetHard(ctx, headSHA)
	}
	w.temp = repo.NewExecGit(w.tempPath)
	if err := w.caller.AddDetachedWorktree(ctx, w.tempPath, headSHA); err != nil {
		return nil, fmt.Errorf("create temporary land worktree: %w", err)
	}
	return w.temp, nil
}

func (w *workspace) close(ctx context.Context, stderr io.Writer) {
	if w.temp == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tempWorktreeRemoveTimeout)
	defer cancel()
	if err := w.caller.RemoveWorktree(cleanupCtx, w.tempPath); err != nil && stderr != nil {
		fmt.Fprintf(stderr, "quai land: remove temporary worktree %s: %v; run git worktree remove --force %s\n", w.tempPath, err, w.tempPath)
	}
	w.temp = nil
}
