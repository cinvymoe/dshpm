package manager

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cinvymoe/dshpm/internal/model"
)

// SwitchToGit points a local dependency at its published GitHub repository
// and materializes the remote checkout. The previous spec is restored if
// installation fails, so a failed switch leaves the profile usable.
func (m *Manager) SwitchToGit(ctx context.Context, name, repo string) error {
	repo = strings.TrimSuffix(strings.TrimSpace(repo), ".git")
	if repo == "" {
		return fmt.Errorf("published repository is empty")
	}
	return m.switchDependency(ctx, name, "github:"+repo)
}

// SwitchToWorktree points a local dependency at a remote worktree checkout.
// The worktree is built in place; the original local checkout is untouched.
func (m *Manager) SwitchToWorktree(ctx context.Context, name, worktree string) error {
	worktree = strings.TrimSpace(worktree)
	if worktree == "" {
		return fmt.Errorf("worktree path is empty")
	}
	return m.switchDependency(ctx, name, "link:"+worktree)
}

func (m *Manager) switchDependency(ctx context.Context, name, spec string) error {
	var selected *model.Plugin
	for _, plugin := range m.Profile.Plugins() {
		if plugin.Name == name {
			copy := plugin
			selected = &copy
			break
		}
	}
	if selected == nil {
		return fmt.Errorf("unknown plugin: %s", name)
	}
	if selected.Kind != model.SourceLocal && selected.Kind != model.SourceLocalRemote {
		return fmt.Errorf("%s is %s, not a local plugin", name, selected.Kind)
	}
	oldSpec := selected.Spec
	if oldSpec == spec {
		return nil
	}
	lockPath := filepath.Join(m.Profile.Dir, "pnpm-lock.yaml")
	oldLock, lockErr := os.ReadFile(lockPath)
	if err := m.Profile.SetDependency(name, spec); err != nil {
		return err
	}
	if err := m.run(ctx, m.Profile.Dir, m.Config.PNPM, "install"); err != nil {
		restoreErr := m.Profile.SetDependency(name, oldSpec)
		if lockErr == nil {
			if writeErr := os.WriteFile(lockPath, oldLock, 0o644); writeErr != nil && restoreErr == nil {
				restoreErr = writeErr
			}
		}
		if restoreErr != nil {
			return fmt.Errorf("install remote plugin: %v (restore %s: %v)", err, oldSpec, restoreErr)
		}
		return fmt.Errorf("install remote plugin: %w", err)
	}
	return nil
}
