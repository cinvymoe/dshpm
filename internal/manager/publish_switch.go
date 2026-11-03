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
	if selected.Kind != model.SourceLocal {
		return fmt.Errorf("%s is %s, not a local plugin", name, selected.Kind)
	}
	oldSpec := selected.Spec
	lockPath := filepath.Join(m.Profile.Dir, "pnpm-lock.yaml")
	oldLock, lockErr := os.ReadFile(lockPath)
	spec := "github:" + repo
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
