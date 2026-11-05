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
	// BuildSourceLocal rebases the local checkout onto its upstream before
	// building it.
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
// checkouts. Local build source defaults to an existing worktree, then the
// local checkout. A local build rebases first; a rebase failure skips the
// upgrade and leaves the checkout untouched for manual handling.
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
		if plugin.Kind == model.SourceLocal {
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
		if plugin.Kind == model.SourceLocal {
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
	if plugin.LocalDir == "" || !gitutil.IsRepository(plugin.LocalDir) {
		result.Status = "skipped"
		result.Message = "local checkout is not a git repository"
		return result
	}
	worktreeDir := m.worktreeDirFor(plugin.Name, options.WorktreeRoot)
	worktreeReady := false
	if info, err := os.Stat(worktreeDir); err == nil && info.IsDir() && gitutil.WorktreeRegistered(plugin.LocalDir, worktreeDir) {
		worktreeReady = true
	}
	source := options.BuildSource
	if source == "" {
		source = BuildSourceAuto
	}
	if source == BuildSourceAuto {
		if worktreeReady {
			source = BuildSourceWorktree
		} else {
			source = BuildSourceLocal
		}
	}
	result.BuildSource = source
	if source == BuildSourceWorktree {
		if !worktreeReady {
			result.Status = "worktree-missing"
			result.Message = fmt.Sprintf("no remote worktree; run dshpm sync %s first, or use --build-source local", plugin.Name)
			return result
		}
		result.WorktreeDir = worktreeDir
		result.WorktreeRef = gitutil.ShortHead(worktreeDir)
		result.To = "worktree@" + result.WorktreeRef
		result.Status = "planned"
		result.Message = "build remote worktree"
		return result
	}
	result.To = plugin.LocalCommit
	hasRemote := gitutil.RemoteURL(plugin.LocalDir, "origin") != ""
	if hasRemote && plugin.Dirty > 0 {
		result.Status = "rebase-blocked"
		result.Message = fmt.Sprintf("%d local change(s) prevent rebase; commit or stash them and run dshpm sync; upgrade skipped, manual handling required", plugin.Dirty)
		return result
	}
	result.Status = "planned"
	if !hasRemote {
		result.Message = "build local checkout (no remote)"
		return result
	}
	switch {
	case plugin.Ahead > 0 && plugin.Behind > 0:
		result.Message = fmt.Sprintf("rebase %d local commit(s) onto remote, then build local checkout", plugin.Ahead)
	case plugin.Behind > 0:
		result.Message = fmt.Sprintf("fast-forward %d commit(s), then build local checkout", plugin.Behind)
	default:
		result.Message = "build local checkout"
	}
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

// prepareLocalBuild rebases or fast-forwards a local checkout onto its
// upstream before a local build. It never discards local changes: a dirty
// checkout or a rebase conflict stops the upgrade for manual handling.
func (m *Manager) prepareLocalBuild(ctx context.Context, plugin model.Plugin, result *UpdateResult) error {
	dir := plugin.LocalDir
	if state := gitutil.RepoState(dir); state != "" {
		result.Status = "blocked"
		result.Message = fmt.Sprintf("local repository has an unfinished %s; finish or abort it before upgrading", state)
		return fmt.Errorf("unfinished %s", state)
	}
	if gitutil.RemoteURL(dir, "origin") == "" {
		return nil
	}
	if dirty := gitutil.DirtyCount(dir); dirty > 0 {
		result.Status = "rebase-blocked"
		result.Message = fmt.Sprintf("%d local change(s) prevent rebase; commit or stash them and run dshpm sync; upgrade skipped, manual handling required", dirty)
		return fmt.Errorf("local changes prevent rebase")
	}
	if err := gitutil.Fetch(dir); err != nil {
		result.Status = "failed"
		result.Message = "fetch: " + err.Error()
		return err
	}
	target, err := gitutil.ResolveUpdateTarget(dir)
	if err != nil {
		result.Status = "failed"
		result.Message = err.Error()
		return err
	}
	targetCommit, err := gitutil.RevParse(dir, target)
	if err != nil {
		result.Status = "failed"
		result.Message = err.Error()
		return err
	}
	head, err := gitutil.Head(dir)
	if err != nil {
		result.Status = "failed"
		result.Message = err.Error()
		return err
	}
	if head == targetCommit {
		if plugin.LocalCommit != "" {
			result.To = plugin.LocalCommit
		}
		return nil
	}
	ahead, behind, err := gitutil.AheadBehind(dir, targetCommit)
	if err != nil {
		result.Status = "failed"
		result.Message = err.Error()
		return err
	}
	if ahead > 0 && behind > 0 {
		if err := gitutil.Rebase(dir, target); err != nil {
			gitutil.RebaseAbort(dir)
			result.Status = "rebase-failed"
			result.Message = "rebase conflict; upgrade skipped; local checkout left untouched; manual handling required: " + firstLine(err.Error())
			return err
		}
		result.Message = fmt.Sprintf("rebased %d local commit(s)", ahead)
	} else if behind > 0 {
		if err := gitutil.MergeFFOnly(dir, target); err != nil {
			result.Status = "failed"
			result.Message = "fast-forward: " + err.Error()
			return err
		}
		result.Message = fmt.Sprintf("fast-forwarded %d commit(s)", behind)
	}
	result.To = target + "@" + gitutil.Shorten(targetCommit)
	return nil
}
