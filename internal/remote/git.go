package remote

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// GitHead is the remote HEAD of a git repository.
type GitHead struct {
	Ref    string
	Commit string
}

// LsRemote resolves the remote HEAD without modifying any local repository.
func LsRemote(gitURL string) (GitHead, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--symref", gitURL, "HEAD")
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return GitHead{}, fmt.Errorf("git ls-remote %s: timed out", gitURL)
		}
		return GitHead{}, fmt.Errorf("git ls-remote %s: %w", gitURL, err)
	}
	head := GitHead{}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "ref:" && fields[2] == "HEAD" {
			head.Ref = strings.TrimPrefix(fields[1], "refs/heads/")
			continue
		}
		if len(fields) == 2 && fields[1] == "HEAD" {
			head.Commit = fields[0]
		}
	}
	if head.Commit == "" {
		return GitHead{}, fmt.Errorf("git ls-remote %s returned no HEAD", gitURL)
	}
	return head, nil
}
