// Package gitutil wraps the git commands dshpm needs for local checkouts.
package gitutil

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// ErrNotRepository is returned when a directory is not inside a git work tree.
var ErrNotRepository = errors.New("not a git repository")

// Output runs git in dir and returns trimmed stdout.
func Output(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Run executes git in dir and streams combined output to the caller's streams.
func Run(dir string, stdout, stderr *bytes.Buffer, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// IsRepository reports whether dir is a git work tree.
func IsRepository(dir string) bool {
	_, err := Output(dir, "rev-parse", "--git-dir")
	return err == nil
}

// Head returns the full HEAD commit.
func Head(dir string) (string, error) {
	return Output(dir, "rev-parse", "HEAD")
}

// ShortHead returns an abbreviated HEAD commit.
func ShortHead(dir string) string {
	head, err := Head(dir)
	if err != nil {
		return ""
	}
	return Shorten(head)
}

// Shorten abbreviates a commit hash to seven characters.
func Shorten(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

// Branch returns the current branch, or "HEAD" when detached.
func Branch(dir string) string {
	value, err := Output(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return ""
	}
	return value
}

// Upstream returns the configured upstream ref, or empty.
func Upstream(dir string) string {
	value, err := Output(dir, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	if err != nil {
		return ""
	}
	return value
}

// DirtyCount counts changed tracked and untracked paths.
func DirtyCount(dir string) int {
	value, err := Output(dir, "status", "--porcelain")
	if err != nil || value == "" {
		return 0
	}
	return len(strings.Split(value, "\n"))
}

// AheadBehind returns commits ahead and behind upstream.
func AheadBehind(dir, upstream string) (int, int, error) {
	if upstream == "" {
		return 0, 0, nil
	}
	value, err := Output(dir, "rev-list", "--left-right", "--count", "HEAD..."+upstream)
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(value)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output %q", value)
	}
	ahead, _ := strconv.Atoi(fields[0])
	behind, _ := strconv.Atoi(fields[1])
	return ahead, behind, nil
}

// Fetch fetches all remotes and prunes deleted branches.
func Fetch(dir string) error {
	if _, err := Output(dir, "fetch", "--all", "--prune"); err != nil {
		return err
	}
	return nil
}

// RemoteURL returns the URL of the given remote, defaulting to origin.
func RemoteURL(dir, remote string) string {
	if remote == "" {
		remote = "origin"
	}
	value, err := Output(dir, "remote", "get-url", remote)
	if err != nil {
		return ""
	}
	return value
}

// ResolveUpdateTarget returns the ref that a local checkout should fast-forward
// to: its configured upstream, then origin/HEAD, then origin/main or origin/master.
func ResolveUpdateTarget(dir string) (string, error) {
	if upstream := Upstream(dir); upstream != "" {
		return upstream, nil
	}
	for _, candidate := range []string{"origin/HEAD", "origin/main", "origin/master", "main", "master"} {
		if _, err := Output(dir, "rev-parse", "--verify", candidate); err == nil {
			if candidate == "origin/HEAD" {
				resolved, err := Output(dir, "rev-parse", "--symbolic-full-name", candidate)
				if err == nil && resolved != "" {
					return resolved, nil
				}
			}
			return candidate, nil
		}
	}
	return "", errors.New("no upstream branch found")
}

// ResetHard discards index and worktree changes at the given ref.
func ResetHard(dir, ref string) error {
	_, err := Output(dir, "reset", "--hard", ref)
	return err
}

// CleanUntracked removes untracked files, leaving ignored files alone.
func CleanUntracked(dir string) error {
	_, err := Output(dir, "clean", "-fd")
	return err
}

// CreateBackupRef points a ref under refs/dshpm/backup at the current HEAD.
func CreateBackupRef(dir, label string) (string, error) {
	ref := "refs/dshpm/backup/" + label
	_, err := Output(dir, "update-ref", ref, "HEAD")
	return ref, err
}

// RevParse resolves a revision to a full commit hash.
func RevParse(dir, rev string) (string, error) {
	return Output(dir, "rev-parse", "--verify", rev)
}

// CapturePatch captures tracked and untracked changes without touching the
// real index. The returned patch is binary-safe and applies against HEAD.
func CapturePatch(dir string) ([]byte, error) {
	tempDir, err := os.MkdirTemp("", "dshpm-index-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)
	indexPath := filepath.Join(tempDir, "index")
	env := append(os.Environ(), "GIT_INDEX_FILE="+indexPath, "GIT_TERMINAL_PROMPT=0")

	run := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			message := strings.TrimSpace(stderr.String())
			if message == "" {
				message = err.Error()
			}
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), message)
		}
		return strings.TrimSpace(stdout.String()), nil
	}
	if _, err := run("read-tree", "HEAD"); err != nil {
		return nil, err
	}
	if _, err := run("add", "-A"); err != nil {
		return nil, err
	}
	cmd := exec.Command("git", "diff", "--cached", "--binary", "HEAD")
	cmd.Dir = dir
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("git diff --cached: %s", message)
	}
	return stdout.Bytes(), nil
}

// ApplyPatch applies a previously captured patch in dir using a three-way
// merge. It leaves nothing staged on success.
func ApplyPatch(dir string, patch []byte) error {
	if len(patch) == 0 {
		return nil
	}
	tempFile, err := os.CreateTemp("", "dshpm-patch-*.patch")
	if err != nil {
		return err
	}
	defer os.Remove(tempFile.Name())
	if _, err := tempFile.Write(patch); err != nil {
		tempFile.Close()
		return err
	}
	if err := tempFile.Close(); err != nil {
		return err
	}
	if _, err := Output(dir, "apply", "--3way", "--whitespace=nowarn", tempFile.Name()); err != nil {
		return err
	}
	_, _ = Output(dir, "reset")
	return nil
}

// ConflictFiles returns the paths git reports as unmerged.
func ConflictFiles(dir string) []string {
	value, err := Output(dir, "diff", "--name-only", "--diff-filter=U")
	if err != nil || value == "" {
		return nil
	}
	return strings.Split(value, "\n")
}

// RepoState reports an unfinished git operation that makes automatic sync
// unsafe, such as "rebase", "merge", "cherry-pick", or "bisect".
func RepoState(dir string) string {
	gitDir, err := Output(dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return ""
	}
	for _, state := range []struct {
		path string
		name string
	}{
		{"rebase-merge", "rebase"},
		{"rebase-apply", "rebase"},
		{"MERGE_HEAD", "merge"},
		{"CHERRY_PICK_HEAD", "cherry-pick"},
		{"REVERT_HEAD", "revert"},
		{"BISECT_LOG", "bisect"},
	} {
		if _, err := os.Stat(filepath.Join(gitDir, state.path)); err == nil {
			return state.name
		}
	}
	return ""
}

// Rebase replays the current branch on top of ref.
func Rebase(dir, ref string) error {
	var stdout, stderr bytes.Buffer
	if err := Run(dir, &stdout, &stderr, "rebase", ref); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = strings.TrimSpace(stdout.String())
		}
		if message == "" {
			message = err.Error()
		}
		return fmt.Errorf("git rebase %s: %s", ref, message)
	}
	return nil
}

// RebaseAbort aborts an in-progress rebase.
func RebaseAbort(dir string) {
	_, _ = Output(dir, "rebase", "--abort")
}

// MergeFFOnly fast-forwards the current branch to ref without creating a
// merge commit.
func MergeFFOnly(dir, ref string) error {
	_, err := Output(dir, "merge", "--ff-only", ref)
	return err
}

// WorktreeRegistered reports whether path is a registered worktree of dir.
func WorktreeRegistered(dir, path string) bool {
	value, err := Output(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return false
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(value, "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		candidate, err := filepath.Abs(strings.TrimSpace(strings.TrimPrefix(line, "worktree ")))
		if err == nil && filepath.Clean(candidate) == filepath.Clean(absolute) {
			return true
		}
	}
	return false
}

// EnsureWorktree checks out ref in a detached worktree at path. An existing
// worktree of the same repository is refreshed; an unrelated directory is
// never deleted or reused.
func EnsureWorktree(dir, path, ref string) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("worktree path is empty")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if info, err := os.Stat(absolute); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("worktree path is not a directory: %s", absolute)
		}
		if !WorktreeRegistered(dir, absolute) {
			return fmt.Errorf("worktree path exists but is not registered to %s: %s", dir, absolute)
		}
		_, err := Output(absolute, "checkout", "--force", "--detach", ref)
		return err
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o755); err != nil {
		return err
	}
	_, _ = Output(dir, "worktree", "prune")
	if _, err := Output(dir, "worktree", "add", "--force", "--detach", absolute, ref); err != nil {
		return err
	}
	return nil
}
