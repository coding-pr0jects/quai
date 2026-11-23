package repo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
)

var commitSHARe = regexp.MustCompile(`^[0-9a-f]{40}$`)

type FileChange struct {
	Status  string // A, M, D, R100, etc.
	Path    string
	OldPath string // populated on renames
}

type TreeEntry struct {
	Path    string
	BlobSHA string
}

type Worktree struct {
	Path     string
	Head     string
	Branch   string
	Bare     bool
	Detached bool
}

type MergeTreeResult struct {
	Clean         bool
	TreeSHA       string
	ConflictFiles []string
}

type RebaseConflictError struct {
	ConflictFiles []string
	Output        string
}

func (e *RebaseConflictError) Error() string {
	if len(e.ConflictFiles) == 0 {
		return "git rebase conflict"
	}
	return fmt.Sprintf("git rebase conflict in %s", strings.Join(e.ConflictFiles, ", "))
}

// Git abstracts all repository git invocations behind a testable boundary.
type Git interface {
	TopLevel(ctx context.Context) (string, error)
	CommonDir(ctx context.Context) (string, error)
	CurrentBranch(ctx context.Context) (string, error)
	HeadSHA(ctx context.Context) (string, error)
	TreeSHA(ctx context.Context, rev string) (string, error)
	RevParse(ctx context.Context, rev string) (string, error)
	MergeBase(ctx context.Context, a, b string) (string, error)
	ResolveBaseRef(ctx context.Context, base string) (string, error)
	IsClean(ctx context.Context) (bool, []string, error)
	StatusPorcelain(ctx context.Context) (string, error)
	DiffNameStatus(ctx context.Context, from, to string, detectRenames bool) ([]FileChange, error)
	DiffNameOnly(ctx context.Context, from, to string) ([]string, error)
	DiffFilePatch(ctx context.Context, from, to, filePath string) ([]byte, error)
	DiffBinary(ctx context.Context, from, to string) ([]byte, error)
	DiffBinaryPaths(ctx context.Context, from, to string, paths []string) ([]byte, error)
	PatchID(ctx context.Context, diff []byte) (string, error)
	UntrackedFiles(ctx context.Context) ([]string, error)
	ListTree(ctx context.Context, treeOrRev string) ([]TreeEntry, error)
	MergeTree(ctx context.Context, baseRev, headRev string) (MergeTreeResult, error)
	Fetch(ctx context.Context, remote, ref string) error
	Rebase(ctx context.Context, onto string) error
	RebaseOnto(ctx context.Context, newBase, oldUpstream string) error
	PushForceWithLease(ctx context.Context, remote, branch, expectedSHA string, env map[string]string) error
	GitPath(ctx context.Context, rel string) (string, error)
	ConfigGet(ctx context.Context, key string) (string, error)
	WriteRefBlob(ctx context.Context, refName, fileName string, content []byte) error
	ReadRefBlob(ctx context.Context, refName, fileName string) ([]byte, bool, error)
	PushRefs(ctx context.Context, remote string, refs []string) error
	FetchRefs(ctx context.Context, remote, refspec string) error
	FetchRefspecs(ctx context.Context, remote string, refspecs []string) error
	ListRefs(ctx context.Context, prefix string) ([]string, error)
	ListRemoteRefs(ctx context.Context, remote string, patterns []string) ([]string, error)
	DeleteLocalRef(ctx context.Context, refName string) error
	DeleteRemoteRefs(ctx context.Context, remote string, refs []string) error
	ShowFile(ctx context.Context, rev, filePath string) ([]byte, error)
	Worktrees(ctx context.Context) ([]Worktree, error)
	AddDetachedWorktree(ctx context.Context, path, rev string) error
	RemoveWorktree(ctx context.Context, path string) error
	ResetHard(ctx context.Context, rev string) error
	CheckRefFormatBranch(ctx context.Context, name string) error
	SymbolicRef(ctx context.Context, name string) (string, error)
}

type ExecGit struct {
	Dir string
}

func NewExecGit(dir string) *ExecGit {
	return &ExecGit{Dir: dir}
}

var strippedGitEnv = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
}

func cleanEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if strippedGitEnv[k] || k == "LC_ALL" || k == "LANG" || k == "LANGUAGE" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "LC_ALL=C")
}

func (g *ExecGit) run(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	return g.runEnv(ctx, nil, stdin, args...)
}

func (g *ExecGit) runEnv(ctx context.Context, env map[string]string, stdin []byte, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.Dir
	cmd.Env = appendEnv(cleanEnv(), env)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}

func appendEnv(base []string, extra map[string]string) []string {
	if len(extra) == 0 {
		return base
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		base = append(base, k+"="+extra[k])
	}
	return base
}

func (g *ExecGit) output(ctx context.Context, args ...string) (string, error) {
	out, errOut, err := g.run(ctx, nil, args...)
	if err != nil {
		return "", fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(errOut)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *ExecGit) TopLevel(ctx context.Context) (string, error) {
	return g.output(ctx, "rev-parse", "--show-toplevel")
}

func (g *ExecGit) CommonDir(ctx context.Context) (string, error) {
	return g.output(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
}

func (g *ExecGit) CurrentBranch(ctx context.Context) (string, error) {
	out, _, err := g.run(ctx, nil, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

func (g *ExecGit) HeadSHA(ctx context.Context) (string, error) {
	return g.output(ctx, "rev-parse", "HEAD")
}

func (g *ExecGit) TreeSHA(ctx context.Context, rev string) (string, error) {
	return g.output(ctx, "rev-parse", "--verify", "--end-of-options", rev+"^{tree}")
}

func (g *ExecGit) RevParse(ctx context.Context, rev string) (string, error) {
	return g.output(ctx, "rev-parse", "--verify", "--end-of-options", rev+"^{commit}")
}

func (g *ExecGit) MergeBase(ctx context.Context, a, b string) (string, error) {
	return g.output(ctx, "merge-base", "--end-of-options", a, b)
}

func (g *ExecGit) ResolveBaseRef(ctx context.Context, base string) (string, error) {
	for _, candidate := range []string{"origin/" + base, base} {
		if _, err := g.RevParse(ctx, candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("cannot resolve base ref %q or %q", "origin/"+base, base)
}

func (g *ExecGit) StatusPorcelain(ctx context.Context) (string, error) {
	out, errOut, err := g.run(ctx, nil, "status", "--porcelain")
	if err != nil {
		return "", fmt.Errorf("git status --porcelain: %w (%s)", err, strings.TrimSpace(string(errOut)))
	}
	return string(out), nil
}

func (g *ExecGit) IsClean(ctx context.Context) (bool, []string, error) {
	raw, err := g.StatusPorcelain(ctx)
	if err != nil {
		return false, nil, err
	}
	if strings.TrimSpace(raw) == "" {
		return true, nil, nil
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(raw, "\n"), "\n") {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return false, lines, nil
}

func (g *ExecGit) DiffNameStatus(ctx context.Context, from, to string, detectRenames bool) ([]FileChange, error) {
	args := []string{"diff", "--name-status", "-z"}
	if detectRenames {
		args = append(args, "-M100%")
	} else {
		args = append(args, "--no-renames")
	}
	args = append(args, "--end-of-options")
	if to == "" {
		args = append(args, from)
	} else {
		args = append(args, from, to)
	}
	out, errOut, err := g.run(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(errOut)))
	}
	return parseNameStatusZ(out), nil
}

func parseNameStatusZ(data []byte) []FileChange {
	tokens := bytes.Split(data, []byte{0})
	var changes []FileChange
	for i := 0; i < len(tokens); {
		status := string(tokens[i])
		if status == "" {
			i++
			continue
		}
		if strings.HasPrefix(status, "R") || strings.HasPrefix(status, "C") {
			if i+2 >= len(tokens) {
				break
			}
			changes = append(changes, FileChange{
				Status:  status,
				OldPath: string(tokens[i+1]),
				Path:    string(tokens[i+2]),
			})
			i += 3
		} else {
			if i+1 >= len(tokens) {
				break
			}
			changes = append(changes, FileChange{
				Status: status,
				Path:   string(tokens[i+1]),
			})
			i += 2
		}
	}
	return changes
}

func (g *ExecGit) DiffNameOnly(ctx context.Context, from, to string) ([]string, error) {
	args := []string{"diff", "--name-only", "--no-renames", "-z", "--end-of-options"}
	if to == "" {
		args = append(args, from)
	} else {
		args = append(args, from, to)
	}
	out, errOut, err := g.run(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("git %s: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(errOut)))
	}
	return splitNull(out), nil
}

func (g *ExecGit) DiffFilePatch(ctx context.Context, from, to, filePath string) ([]byte, error) {
	args := []string{"diff", "--binary", "--no-renames", "-U0", "--end-of-options"}
	if to == "" {
		args = append(args, from)
	} else {
		args = append(args, from, to)
	}
	args = append(args, "--", filePath)
	out, errOut, err := g.run(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("git diff %s: %w (%s)", filePath, err, strings.TrimSpace(string(errOut)))
	}
	return out, nil
}

func (g *ExecGit) DiffBinary(ctx context.Context, from, to string) ([]byte, error) {
	args := []string{"diff", "--binary", "--no-renames", "--end-of-options", from, to}
	out, errOut, err := g.run(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("git diff --binary: %w (%s)", err, strings.TrimSpace(string(errOut)))
	}
	return out, nil
}

// DiffBinaryPaths is DiffBinary limited to paths, each taken literally (the git
// ":(literal)" pathspec magic). No paths yields an empty diff, never the whole one.
func (g *ExecGit) DiffBinaryPaths(ctx context.Context, from, to string, paths []string) ([]byte, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	args := []string{"diff", "--binary", "--no-renames", "--end-of-options", from, to, "--"}
	for _, p := range paths {
		args = append(args, ":(literal)"+p)
	}
	out, errOut, err := g.run(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("git diff --binary -- <paths>: %w (%s)", err, strings.TrimSpace(string(errOut)))
	}
	return out, nil
}

func (g *ExecGit) PatchID(ctx context.Context, diff []byte) (string, error) {
	if len(bytes.TrimSpace(diff)) == 0 {
		return "", nil
	}
	out, errOut, err := g.run(ctx, diff, "patch-id", "--stable")
	if err != nil {
		return "", fmt.Errorf("git patch-id: %w (%s)", err, strings.TrimSpace(string(errOut)))
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return "", nil
	}
	return fields[0], nil
}

func (g *ExecGit) UntrackedFiles(ctx context.Context) ([]string, error) {
	out, errOut, err := g.run(ctx, nil, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w (%s)", err, strings.TrimSpace(string(errOut)))
	}
	return splitNull(out), nil
}

func (g *ExecGit) ListTree(ctx context.Context, treeOrRev string) ([]TreeEntry, error) {
	out, errOut, err := g.run(ctx, nil, "ls-tree", "-r", "-z", "--end-of-options", treeOrRev)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s: %w (%s)", treeOrRev, err, strings.TrimSpace(string(errOut)))
	}
	var entries []TreeEntry
	for _, record := range bytes.Split(out, []byte{0}) {
		if len(record) == 0 {
			continue
		}
		meta, filePath, ok := strings.Cut(string(record), "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(meta)
		if len(fields) < 3 {
			continue
		}
		entries = append(entries, TreeEntry{
			Path:    filePath,
			BlobSHA: fields[2],
		})
	}
	return entries, nil
}

// MergeTree performs a virtual merge without touching HEAD or the working tree (requires git 2.38+).
func (g *ExecGit) MergeTree(ctx context.Context, baseRev, headRev string) (MergeTreeResult, error) {
	out, errOut, err := g.run(ctx, nil, "merge-tree", "--write-tree", "--name-only", "--no-messages", "--end-of-options", baseRev, headRev)
	lines := nonEmptyLines(string(out))
	if err == nil {
		if len(lines) == 0 {
			return MergeTreeResult{}, fmt.Errorf("git merge-tree returned empty output")
		}
		return MergeTreeResult{Clean: true, TreeSHA: lines[0]}, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		tree := ""
		var conflicts []string
		if len(lines) > 0 {
			tree = lines[0]
			conflicts = append(conflicts, lines[1:]...)
		}
		return MergeTreeResult{Clean: false, TreeSHA: tree, ConflictFiles: conflicts}, nil
	}
	return MergeTreeResult{}, fmt.Errorf("git merge-tree: %w (%s)", err, strings.TrimSpace(string(errOut)))
}

func (g *ExecGit) Fetch(ctx context.Context, remote, ref string) error {
	_, errOut, err := g.run(ctx, nil, "fetch", "--quiet", "--end-of-options", remote, ref)
	if err != nil {
		return fmt.Errorf("git fetch %s %s: %w (%s)", remote, ref, err, strings.TrimSpace(string(errOut)))
	}
	return nil
}

func (g *ExecGit) Rebase(ctx context.Context, onto string) error {
	out, errOut, err := g.run(ctx, nil, "rebase", "--end-of-options", onto)
	if err == nil {
		return nil
	}
	conflicts, _ := g.unmergedFiles(context.WithoutCancel(ctx))
	_, _, _ = g.run(context.WithoutCancel(ctx), nil, "rebase", "--abort")
	return &RebaseConflictError{
		ConflictFiles: conflicts,
		Output:        strings.TrimSpace(string(out) + "\n" + string(errOut)),
	}
}

// RebaseOnto replays commits after oldUpstream onto newBase, aborting cleanly on conflict.
func (g *ExecGit) RebaseOnto(ctx context.Context, newBase, oldUpstream string) error {
	out, errOut, err := g.run(ctx, nil, "rebase", "--onto", newBase, "--end-of-options", oldUpstream)
	if err == nil {
		return nil
	}
	conflicts, _ := g.unmergedFiles(context.WithoutCancel(ctx))
	_, _, _ = g.run(context.WithoutCancel(ctx), nil, "rebase", "--abort")
	return &RebaseConflictError{
		ConflictFiles: conflicts,
		Output:        strings.TrimSpace(string(out) + "\n" + string(errOut)),
	}
}

func (g *ExecGit) unmergedFiles(ctx context.Context) ([]string, error) {
	out, _, err := g.run(ctx, nil, "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	return splitNull(out), nil
}

// PushForceWithLease pushes HEAD to branch with a lease pinned to expectedSHA, adding env
// to the git process so a pre-push hook sees it.
func (g *ExecGit) PushForceWithLease(ctx context.Context, remote, branch, expectedSHA string, env map[string]string) error {
	lease := fmt.Sprintf("--force-with-lease=refs/heads/%s:%s", branch, expectedSHA)
	_, errOut, err := g.runEnv(ctx, env, nil, "push", lease, "--end-of-options", remote, "HEAD:refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("git push %s: %w (%s)", lease, err, strings.TrimSpace(string(errOut)))
	}
	return nil
}

func (g *ExecGit) GitPath(ctx context.Context, rel string) (string, error) {
	return g.output(ctx, "rev-parse", "--path-format=absolute", "--git-path", rel)
}

func (g *ExecGit) ConfigGet(ctx context.Context, key string) (string, error) {
	out, _, err := g.run(ctx, nil, "config", "--get", key)
	if err != nil {
		return "", nil
	}
	return strings.TrimSpace(string(out)), nil
}

// WriteRefBlob stores content as a standalone commit under refName containing fileName
// without touching HEAD, branches, the index, or the working tree.
func (g *ExecGit) WriteRefBlob(ctx context.Context, refName, fileName string, content []byte) error {
	out, errOut, err := g.run(ctx, content, "hash-object", "-w", "--stdin")
	if err != nil {
		return fmt.Errorf("git hash-object: %w (%s)", err, strings.TrimSpace(string(errOut)))
	}
	blobSHA := strings.TrimSpace(string(out))

	treeLine := fmt.Sprintf("100644 blob %s\t%s\n", blobSHA, fileName)
	treeOut, errOut, err := g.run(ctx, []byte(treeLine), "mktree")
	if err != nil {
		return fmt.Errorf("git mktree: %w (%s)", err, strings.TrimSpace(string(errOut)))
	}
	treeSHA := strings.TrimSpace(string(treeOut))

	commitOut, errOut, err := g.run(
		ctx,
		nil,
		"-c", "user.name=quai",
		"-c", "user.email=quai@localhost",
		"commit-tree", treeSHA,
		"-m", "quai attestation",
	)
	if err != nil {
		return fmt.Errorf("git commit-tree: %w (%s)", err, strings.TrimSpace(string(errOut)))
	}
	commitSHA := strings.TrimSpace(string(commitOut))

	if _, errOut, err := g.run(ctx, nil, "update-ref", "--end-of-options", refName, commitSHA); err != nil {
		return fmt.Errorf("git update-ref %s: %w (%s)", refName, err, strings.TrimSpace(string(errOut)))
	}
	return nil
}

// ReadRefBlob reads fileName from refName if the ref exists.
func (g *ExecGit) ReadRefBlob(ctx context.Context, refName, fileName string) ([]byte, bool, error) {
	if _, _, err := g.run(ctx, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", refName+"^{commit}"); err != nil {
		return nil, false, nil
	}
	out, errOut, err := g.run(ctx, nil, "show", "--end-of-options", refName+":"+fileName)
	if err != nil {
		return nil, false, fmt.Errorf("git show %s:%s: %w (%s)", refName, fileName, err, strings.TrimSpace(string(errOut)))
	}
	return out, true, nil
}

// PushRefs pushes explicit quai refs to remote without running the pre-push hook, so the
// hook's own attestation sync can never nest. Only refs/quai/* pass, so this can never
// skip a hook for a branch.
func (g *ExecGit) PushRefs(ctx context.Context, remote string, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	for _, r := range refs {
		if !strings.HasPrefix(r, "refs/quai/") {
			return fmt.Errorf("refusing to push non-quai ref %q with hooks disabled", r)
		}
	}
	args := make([]string, 0, len(refs)+4)
	args = append(args, "push", "--no-verify", "--end-of-options", remote)
	for _, r := range refs {
		args = append(args, r+":"+r)
	}
	if _, errOut, err := g.run(ctx, nil, args...); err != nil {
		return fmt.Errorf("git push %s: %w (%s)", remote, err, strings.TrimSpace(string(errOut)))
	}
	return nil
}

// FetchRefs fetches a refspec from remote. Missing remote refs are treated as an empty no-op.
func (g *ExecGit) FetchRefs(ctx context.Context, remote, refspec string) error {
	return g.FetchRefspecs(ctx, remote, []string{refspec})
}

// FetchRefspecs fetches one or more explicit refspecs from remote.
// Missing remote refs are treated as an empty no-op.
func (g *ExecGit) FetchRefspecs(ctx context.Context, remote string, refspecs []string) error {
	if len(refspecs) == 0 {
		return nil
	}
	args := make([]string, 0, len(refspecs)+4)
	args = append(args, "fetch", "--quiet", "--end-of-options", remote)
	args = append(args, refspecs...)
	_, errOut, err := g.run(ctx, nil, args...)
	if err != nil {
		msg := strings.TrimSpace(string(errOut))
		if strings.Contains(msg, "couldn't find remote ref") || strings.Contains(msg, "no matching remote") {
			return nil
		}
		return fmt.Errorf("git fetch %s %s: %w (%s)", remote, strings.Join(refspecs, " "), err, msg)
	}
	return nil
}

// ListRefs returns all local ref names matching prefix.
func (g *ExecGit) ListRefs(ctx context.Context, prefix string) ([]string, error) {
	out, errOut, err := g.run(ctx, nil, "for-each-ref", "--format=%(refname)", "--end-of-options", prefix)
	if err != nil {
		return nil, fmt.Errorf("git for-each-ref %s: %w (%s)", prefix, err, strings.TrimSpace(string(errOut)))
	}
	return nonEmptyLines(string(out)), nil
}

// ListRemoteRefs queries remote via ls-remote for refs matching patterns and returns their ref names.
func (g *ExecGit) ListRemoteRefs(ctx context.Context, remote string, patterns []string) ([]string, error) {
	args := make([]string, 0, len(patterns)+4)
	args = append(args, "ls-remote", "--quiet", "--end-of-options", remote)
	args = append(args, patterns...)
	out, errOut, err := g.run(ctx, nil, args...)
	if err != nil {
		return nil, fmt.Errorf("git ls-remote %s: %w (%s)", remote, err, strings.TrimSpace(string(errOut)))
	}
	var refs []string
	for _, line := range nonEmptyLines(string(out)) {
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			refs = append(refs, fields[1])
		}
	}
	return refs, nil
}

// DeleteLocalRef removes a single local ref.
func (g *ExecGit) DeleteLocalRef(ctx context.Context, refName string) error {
	if _, errOut, err := g.run(ctx, nil, "update-ref", "-d", "--end-of-options", refName); err != nil {
		return fmt.Errorf("git update-ref -d %s: %w (%s)", refName, err, strings.TrimSpace(string(errOut)))
	}
	return nil
}

// DeleteRemoteRefs deletes the given refs on remote in a single push.
func (g *ExecGit) DeleteRemoteRefs(ctx context.Context, remote string, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
	args := make([]string, 0, len(refs)+3)
	args = append(args, "push", "--end-of-options", remote)
	for _, r := range refs {
		args = append(args, ":"+r)
	}
	if _, errOut, err := g.run(ctx, nil, args...); err != nil {
		return fmt.Errorf("git push delete %s: %w (%s)", remote, err, strings.TrimSpace(string(errOut)))
	}
	return nil
}

// ShowFile returns the raw bytes of filePath at git revision rev.
func (g *ExecGit) ShowFile(ctx context.Context, rev, filePath string) ([]byte, error) {
	out, errOut, err := g.run(ctx, nil, "show", "--end-of-options", rev+":"+filePath)
	if err != nil {
		return nil, fmt.Errorf("git show %s:%s: %w (%s)", rev, filePath, err, strings.TrimSpace(string(errOut)))
	}
	return out, nil
}

// Worktrees returns all git worktrees reported by `git worktree list --porcelain`.
func (g *ExecGit) Worktrees(ctx context.Context) ([]Worktree, error) {
	out, errOut, err := g.run(ctx, nil, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %w (%s)", err, strings.TrimSpace(string(errOut)))
	}
	return ParseWorktreeList(string(out)), nil
}

// AddDetachedWorktree checks rev out at path in a new linked worktree with a detached HEAD,
// which git allows even when a branch at rev is checked out in another worktree.
func (g *ExecGit) AddDetachedWorktree(ctx context.Context, path, rev string) error {
	_, err := g.output(ctx, "worktree", "add", "--quiet", "--detach", "--end-of-options", path, rev)
	return err
}

// RemoveWorktree deletes the linked worktree at path, discarding its changes and any
// rebase left in progress, then prunes its administrative files.
func (g *ExecGit) RemoveWorktree(ctx context.Context, path string) error {
	_, removeErr := g.output(ctx, "worktree", "remove", "--force", "--force", "--end-of-options", path)
	if removeErr != nil {
		if err := os.RemoveAll(path); err != nil {
			return errors.Join(removeErr, err)
		}
	}
	_, err := g.output(ctx, "worktree", "prune")
	return err
}

// ResetHard moves HEAD, the index, and the tracked files of the worktree to rev.
// git reset has no --end-of-options, so rev must already be a 40-character commit SHA.
func (g *ExecGit) ResetHard(ctx context.Context, rev string) error {
	if !commitSHARe.MatchString(rev) {
		return fmt.Errorf("reset --hard requires a 40-character commit SHA, got %q", rev)
	}
	_, err := g.output(ctx, "reset", "--quiet", "--hard", rev)
	return err
}

// CheckRefFormatBranch reports whether name is a valid branch name.
// git check-ref-format has no --end-of-options, so an option-shaped name is refused here.
func (g *ExecGit) CheckRefFormatBranch(ctx context.Context, name string) error {
	if name == "" || strings.HasPrefix(name, "-") {
		return fmt.Errorf("invalid branch name %q: must not be empty or start with \"-\"", name)
	}
	if _, errOut, err := g.run(ctx, nil, "check-ref-format", "--branch", name); err != nil {
		return fmt.Errorf("git check-ref-format --branch %q: %w (%s)", name, err, strings.TrimSpace(string(errOut)))
	}
	return nil
}

// SymbolicRef resolves a symbolic ref such as refs/remotes/origin/HEAD to its target.
func (g *ExecGit) SymbolicRef(ctx context.Context, name string) (string, error) {
	out, errOut, err := g.run(ctx, nil, "symbolic-ref", "--quiet", "--end-of-options", name)
	if err != nil {
		return "", fmt.Errorf("git symbolic-ref %s: %w (%s)", name, err, strings.TrimSpace(string(errOut)))
	}
	return strings.TrimSpace(string(out)), nil
}

// ParseWorktreeList parses `git worktree list --porcelain` output into Worktree structs.
func ParseWorktreeList(raw string) []Worktree {
	var out []Worktree
	var cur *Worktree
	flush := func() {
		if cur != nil && cur.Path != "" {
			out = append(out, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "worktree ") {
			flush()
			cur = &Worktree{Path: strings.TrimPrefix(line, "worktree ")}
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			ref := strings.TrimPrefix(line, "branch ")
			cur.Branch = strings.TrimPrefix(ref, "refs/heads/")
		case line == "bare":
			cur.Bare = true
		case line == "detached":
			cur.Detached = true
		}
	}
	flush()
	return out
}

func splitNull(data []byte) []string {
	var out []string
	for _, b := range bytes.Split(data, []byte{0}) {
		if len(b) > 0 {
			out = append(out, string(b))
		}
	}
	return out
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			out = append(out, t)
		}
	}
	return out
}
