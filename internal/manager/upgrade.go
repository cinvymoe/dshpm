package manager

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/cinvymoe/dshpm/internal/backup"
	"github.com/cinvymoe/dshpm/internal/gitutil"
	"github.com/cinvymoe/dshpm/internal/model"
	"github.com/cinvymoe/dshpm/internal/version"
)

// LocalBuildSource selects how a local checkout is built during upgrade.
type LocalBuildSource string

const (
	// BuildSourceAuto builds a registered remote worktree when present,
	// otherwise builds the local checkout.
	BuildSourceAuto LocalBuildSource = "auto"
	// BuildSourceWorktree builds only the remote worktree created by sync.
	BuildSourceWorktree LocalBuildSource = "worktree"
	// BuildSourceLocal builds a pure local checkout in place. local-remote
	// plugins ignore it and always build their remote worktree.
	BuildSourceLocal LocalBuildSource = "local"
)

// UpgradeOptions controls an upgrade run. Local checkouts are built from the
// source selected by BuildSource; use Sync first to create a remote worktree.
type UpgradeOptions struct {
	All          bool
	Names        []string
	DryRun       bool
	NoBackup     bool
	NoBuild      bool
	BuildSource  LocalBuildSource
	BackupDir    string
	WorktreeRoot string
	BuildTimeout time.Duration
	Out          io.Writer
}

// Upgrade installs the newest registry/git versions and rebuilds local
// checkouts. Pure local checkouts are built in place. local-remote checkouts
// are never built in place: sync must first create their remote worktree.
func (m *Manager) Upgrade(ctx context.Context, options UpgradeOptions) ([]UpdateResult, error) {
	if options.Out == nil {
		options.Out = m.Out
	}
	if options.BuildTimeout == 0 {
		options.BuildTimeout = 10 * time.Minute
	}
	buildSource, err := normalizeBuildSource(options.BuildSource)
	if err != nil {
		return nil, err
	}
	options.BuildSource = buildSource
	plugins := m.Profile.Plugins()
	selected, err := selectPlugins(plugins, UpdateOptions{All: options.All, Names: options.Names})
	if err != nil {
		return nil, err
	}
	selected, unmanaged := m.partitionManaged(selected)
	plans := make([]UpdateResult, 0, len(selected))
	for _, plugin := range selected {
		if plugin.Kind == model.SourceLocal || plugin.Kind == model.SourceLocalRemote {
			plans = append(plans, m.planLocalUpgrade(plugin, options))
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
		if plugin.Kind == model.SourceLocal || plugin.Kind == model.SourceLocalRemote {
			results = append(results, m.applyLocalUpgrade(ctx, plugin, result, options, backupPath))
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

func normalizeBuildSource(value LocalBuildSource) (LocalBuildSource, error) {
	switch strings.TrimSpace(string(value)) {
	case "", string(BuildSourceAuto):
		return BuildSourceAuto, nil
	case string(BuildSourceWorktree):
		return BuildSourceWorktree, nil
	case string(BuildSourceLocal):
		return BuildSourceLocal, nil
	default:
		return "", fmt.Errorf("invalid build source %q (want auto, worktree, or local)", value)
	}
}

func (m *Manager) planLocalUpgrade(plugin model.Plugin, options UpgradeOptions) UpdateResult {
	result := UpdateResult{Name: plugin.Name, Kind: plugin.Kind, From: localLabel(plugin)}
	if plugin.LocalDir == "" {
		result.Status = "skipped"
		result.Message = "local directory is missing"
		return result
	}
	if plugin.Kind == model.SourceLocal && plugin.Dirty > 0 {
		result.Status = "blocked"
		result.Message = fmt.Sprintf("%d local change(s); clean the local checkout before upgrading", plugin.Dirty)
		return result
	}
	worktreeDir := m.worktreeDirFor(plugin.Name, options.WorktreeRoot)
	worktreeReady := false
	if info, err := os.Stat(worktreeDir); err == nil && info.IsDir() && gitutil.WorktreeRegistered(plugin.LocalDir, worktreeDir) {
		worktreeReady = true
	}
	if plugin.Kind == model.SourceLocalRemote {
		// local-remote checkouts are deliberately never built in place. A
		// remote worktree created by sync is the only build source; the local
		// checkout and any local commits or uncommitted changes stay untouched.
		result.BuildSource = BuildSourceWorktree
		if !worktreeReady {
			result.Status = "worktree-missing"
			result.Message = fmt.Sprintf("local-remote has no remote worktree; run dshpm sync %s first", plugin.Name)
			return result
		}
		result.WorktreeDir = worktreeDir
		result.WorktreeRef = gitutil.ShortHead(worktreeDir)
		result.To = "worktree@" + result.WorktreeRef
		result.Status = "planned"
		result.Message = "build remote worktree (local-remote; local checkout untouched)"
		return result
	}
	// A pure local checkout has no remote to synchronize with. Build it in
	// place; do not require Git or rebase it, and do not create a worktree.
	result.BuildSource = BuildSourceLocal
	result.To = plugin.LocalCommit
	result.Status = "planned"
	result.Message = "build local checkout (pure local)"
	return result
}

func (m *Manager) applyLocalUpgrade(ctx context.Context, plugin model.Plugin, plan UpdateResult, options UpgradeOptions, backupPath string) UpdateResult {
	result := plan
	result.BackupPath = backupPath
	if result.BuildSource == BuildSourceWorktree {
		if result.WorktreeDir == "" {
			result.Status = "worktree-missing"
			result.Message = "remote worktree disappeared before upgrade"
			return result
		}
		if !options.NoBuild {
			if err := m.build(ctx, result.WorktreeDir, options.BuildTimeout); err != nil {
				result.Status = "build-failed"
				result.Message = err.Error()
				return result
			}
		}
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
	if err := m.prepareLocalBuild(ctx, plugin, &result); err != nil {
		return result
	}
	if err := m.build(ctx, plugin.LocalDir, options.BuildTimeout); err != nil {
		result.Status = "build-failed"
		result.Message = err.Error()
		return result
	}
	if result.Message != "" {
		result.Message += "; "
	}
	result.Message += "local checkout built"
	result.Status = "upgraded"
	return result
}

// prepareLocalBuild validates a pure local checkout before building it. A
// non-Git local directory builds as-is; a Git checkout must be clean so an
// upgrade never leaves a conflict or mixed working tree behind.
func (m *Manager) prepareLocalBuild(ctx context.Context, plugin model.Plugin, result *UpdateResult) error {
	dir := plugin.LocalDir
	if state := gitutil.RepoState(dir); state != "" {
		result.Status = "blocked"
		result.Message = fmt.Sprintf("local repository has an unfinished %s; finish or abort it before upgrading", state)
		return fmt.Errorf("unfinished %s", state)
	}
	if dirty := gitutil.DirtyCount(dir); dirty > 0 {
		result.Status = "blocked"
		result.Message = fmt.Sprintf("%d local change(s); clean the local checkout before upgrading", dirty)
		return fmt.Errorf("dirty local checkout")
	}
	return nil
}
