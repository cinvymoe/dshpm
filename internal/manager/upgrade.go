package manager

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/cinvymoe/dshpm/internal/backup"
	"github.com/cinvymoe/dshpm/internal/gitutil"
	"github.com/cinvymoe/dshpm/internal/model"
	"github.com/cinvymoe/dshpm/internal/version"
)

// UpgradeOptions controls an upgrade run. Local checkouts are built as they
// are; use Sync first to move them to the remote revision.
type UpgradeOptions struct {
	All          bool
	Names        []string
	DryRun       bool
	NoBackup     bool
	NoBuild      bool
	BackupDir    string
	WorktreeRoot string
	BuildTimeout time.Duration
	Out          io.Writer
}

// Upgrade installs the newest registry/git versions and rebuilds local
// checkouts. A local checkout that has a remote worktree from Sync is built
// from that worktree, leaving the local checkout untouched.
func (m *Manager) Upgrade(ctx context.Context, options UpgradeOptions) ([]UpdateResult, error) {
	if options.Out == nil {
		options.Out = m.Out
	}
	if options.BuildTimeout == 0 {
		options.BuildTimeout = 10 * time.Minute
	}
	plugins := m.Profile.Plugins()
	selected, err := selectPlugins(plugins, UpdateOptions{All: options.All, Names: options.Names})
	if err != nil {
		return nil, err
	}
	selected, unmanaged := m.partitionManaged(selected)
	plans := make([]UpdateResult, 0, len(selected))
	for _, plugin := range selected {
		if plugin.Kind == model.SourceLocal {
			plans = append(plans, m.planLocalUpgrade(plugin, options.WorktreeRoot))
			continue
		}
		result, _, err := m.planOne(plugin, UpdateOptions{All: options.All, Names: options.Names})
		if err != nil {
			result = UpdateResult{Name: plugin.Name, Kind: plugin.Kind, From: localLabel(plugin), Status: "error", Message: err.Error()}
		}
		plans = append(plans, result)
	}
	needsBackup := false
	for _, result := range plans {
		if result.Status == "planned" {
			needsBackup = true
		}
	}
	if options.DryRun {
		for index := range plans {
			if plans[index].Status == "planned" {
				plans[index].Status = "would-upgrade"
			}
		}
		return append(plans, unmanagedUpdateResults(unmanaged, "upgraded")...), nil
	}
	backupPath := ""
	if needsBackup && !options.NoBackup {
		manifest, dir, err := backup.Create(backup.CreateOptions{
			Profile:     m.Profile,
			BackupDir:   options.BackupDir,
			ToolVersion: version.Version,
			Now:         time.Now(),
		})
		_ = manifest
		if err != nil {
			return nil, fmt.Errorf("pre-upgrade backup failed: %w", err)
		}
		backupPath = dir
	}
	results := make([]UpdateResult, 0, len(plans))
	for index, result := range plans {
		if result.Status != "planned" {
			results = append(results, result)
			continue
		}
		plugin := selected[index]
		if plugin.Kind == model.SourceLocal {
			results = append(results, m.applyLocalUpgrade(ctx, plugin, options, backupPath))
			continue
		}
		results = append(results, m.applyOne(ctx, plugin, UpdateOptions{
			All:          options.All,
			Names:        options.Names,
			NoBackup:     options.NoBackup,
			NoBuild:      options.NoBuild,
			BackupDir:    options.BackupDir,
			BuildTimeout: options.BuildTimeout,
			Out:          options.Out,
		}, backupPath))
	}
	return append(results, unmanagedUpdateResults(unmanaged, "upgraded")...), nil
}

func (m *Manager) planLocalUpgrade(plugin model.Plugin, worktreeRoot string) UpdateResult {
	result := UpdateResult{Name: plugin.Name, Kind: plugin.Kind, From: localLabel(plugin)}
	if plugin.LocalDir == "" || !gitutil.IsRepository(plugin.LocalDir) {
		result.Status = "skipped"
		result.Message = "local checkout is not a git repository"
		return result
	}
	if worktreeDir := m.worktreeDirFor(plugin.Name, worktreeRoot); gitutil.WorktreeRegistered(plugin.LocalDir, worktreeDir) {
		if info, err := os.Stat(worktreeDir); err == nil && info.IsDir() {
			result.WorktreeDir = worktreeDir
			result.WorktreeRef = gitutil.ShortHead(worktreeDir)
			result.To = "worktree@" + result.WorktreeRef
			result.Status = "planned"
			result.Message = "build remote worktree"
			return result
		}
	}
	if plugin.LocalCommit != "" {
		result.To = plugin.LocalCommit
	}
	result.Status = "planned"
	result.Message = "build local checkout"
	return result
}

func (m *Manager) applyLocalUpgrade(ctx context.Context, plugin model.Plugin, options UpgradeOptions, backupPath string) UpdateResult {
	result := UpdateResult{
		Name:        plugin.Name,
		Kind:        plugin.Kind,
		From:        localLabel(plugin),
		BackupPath:  backupPath,
		WorktreeDir: "",
	}
	dir := plugin.LocalDir
	if worktreeDir := m.worktreeDirFor(plugin.Name, options.WorktreeRoot); gitutil.WorktreeRegistered(plugin.LocalDir, worktreeDir) {
		if info, err := os.Stat(worktreeDir); err == nil && info.IsDir() {
			dir = worktreeDir
			result.WorktreeDir = worktreeDir
			result.WorktreeRef = gitutil.ShortHead(worktreeDir)
			result.To = "worktree@" + result.WorktreeRef
		}
	}
	if result.To == "" {
		result.To = plugin.LocalCommit
	}
	if !options.NoBuild {
		if err := m.build(ctx, dir, options.BuildTimeout); err != nil {
			result.Status = "build-failed"
			result.Message = err.Error()
			return result
		}
	}
	if result.WorktreeDir != "" {
		if err := m.SwitchToWorktree(ctx, plugin.Name, result.WorktreeDir); err != nil {
			result.Status = "failed"
			result.Message = "switch profile to worktree: " + err.Error()
			return result
		}
		result.Status = "upgraded"
		result.Message = "built remote worktree and switched profile to it; local checkout untouched"
		return result
	}
	if options.NoBuild {
		result.Status = "upgraded"
		result.Message = "build skipped"
		return result
	}
	result.Status = "upgraded"
	result.Message = "local checkout built"
	return result
}
