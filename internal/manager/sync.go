package manager

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/cinvymoe/dshpm/internal/backup"
	"github.com/cinvymoe/dshpm/internal/gitutil"
	"github.com/cinvymoe/dshpm/internal/model"
	"github.com/cinvymoe/dshpm/internal/version"
)

// SyncOptions controls a repository synchronization run.
type SyncOptions struct {
	All          bool
	Names        []string
	DryRun       bool
	NoBackup     bool
	BackupDir    string
	WorktreeRoot string
	Out          io.Writer
}

// SyncResult is the outcome for one plugin.
type SyncResult struct {
	Name        string           `json:"name"`
	Kind        model.SourceKind `json:"kind"`
	From        string           `json:"from,omitempty"`
	To          string           `json:"to,omitempty"`
	Status      string           `json:"status"`
	Message     string           `json:"message,omitempty"`
	BackupPath  string           `json:"backupPath,omitempty"`
	WorktreeDir string           `json:"worktreeDir,omitempty"`
	WorktreeRef string           `json:"worktreeRef,omitempty"`
	Action      string           `json:"action,omitempty"`
}

type syncPlan struct {
	result  SyncResult
	action  string
	changed bool
}

// Sync synchronizes local git checkouts with their remote branch. It never
// captures or reapplies a patch: a dirty checkout or a rebase conflict is
// handled by checking the remote revision out into a separate worktree and
// leaving the local checkout untouched.
func (m *Manager) Sync(ctx context.Context, options SyncOptions) ([]SyncResult, error) {
	if options.Out == nil {
		options.Out = m.Out
	}
	plugins := m.Profile.Plugins()
	selected, err := selectPlugins(plugins, UpdateOptions{All: options.All, Names: options.Names})
	if err != nil {
		return nil, err
	}
	plans := make([]syncPlan, 0, len(selected))
	needsBackup := false
	for _, plugin := range selected {
		plan := m.planSync(plugin, options.WorktreeRoot)
		if plan.result.Status == "planned" {
			needsBackup = true
		}
		plans = append(plans, plan)
	}
	results := make([]SyncResult, len(plans))
	for index := range plans {
		results[index] = plans[index].result
	}
	if !needsBackup || options.DryRun {
		if options.DryRun {
			for index := range results {
				if results[index].Status == "planned" {
					results[index].Status = "would-sync"
				}
			}
		}
		return results, nil
	}
	backupPath := ""
	if !options.NoBackup {
		manifest, dir, err := backup.Create(backup.CreateOptions{
			Profile:     m.Profile,
			BackupDir:   options.BackupDir,
			ToolVersion: version.Version,
			Now:         time.Now(),
		})
		_ = manifest
		if err != nil {
			return nil, fmt.Errorf("pre-sync backup failed: %w", err)
		}
		backupPath = dir
	}
	for index := range plans {
		if plans[index].result.Status != "planned" {
			results[index].BackupPath = backupPath
			continue
		}
		results[index] = m.applySync(ctx, selected[index], plans[index], options, backupPath)
	}
	return results, nil
}

func (m *Manager) planSync(plugin model.Plugin, worktreeRoot string) syncPlan {
	plan := syncPlan{result: SyncResult{Name: plugin.Name, Kind: plugin.Kind, Status: "skipped"}}
	if plugin.Kind != model.SourceLocal {
		plan.result.Message = "only local checkouts need repository sync"
		return plan
	}
	if plugin.LocalDir == "" || !gitutil.IsRepository(plugin.LocalDir) {
		plan.result.Message = "local checkout is not a git repository"
		return plan
	}
	if state := gitutil.RepoState(plugin.LocalDir); state != "" {
		plan.result.Status = "blocked"
		plan.result.Message = fmt.Sprintf("local repository has an unfinished %s; finish or abort it before syncing", state)
		return plan
	}
	plan.result.From = versionAndCommit(plugin.InstalledVersion, plugin.LocalCommit)
	if gitutil.RemoteURL(plugin.LocalDir, "origin") == "" {
		plan.result.Status = "local-only"
		plan.result.Message = "no remote repository configured"
		return plan
	}
	if err := gitutil.Fetch(plugin.LocalDir); err != nil {
		plan.result.Status = "failed"
		plan.result.Message = "fetch: " + err.Error()
		return plan
	}
	target, err := gitutil.ResolveUpdateTarget(plugin.LocalDir)
	if err != nil {
		plan.result.Status = "local-only"
		plan.result.Message = err.Error()
		return plan
	}
	targetCommit, err := gitutil.RevParse(plugin.LocalDir, target)
	if err != nil {
		plan.result.Status = "failed"
		plan.result.Message = err.Error()
		return plan
	}
	head, err := gitutil.Head(plugin.LocalDir)
	if err != nil {
		plan.result.Status = "failed"
		plan.result.Message = err.Error()
		return plan
	}
	plan.result.To = target + "@" + gitutil.Shorten(targetCommit)
	ahead, behind, err := gitutil.AheadBehind(plugin.LocalDir, targetCommit)
	if err != nil {
		plan.result.Status = "failed"
		plan.result.Message = err.Error()
		return plan
	}
	if head == targetCommit {
		plan.result.Status = "up-to-date"
		if plugin.Dirty > 0 {
			plan.result.Message = fmt.Sprintf("%d local change(s); remote is unchanged", plugin.Dirty)
		}
		return plan
	}
	if ahead > 0 && behind == 0 {
		plan.result.Status = "ahead"
		plan.result.Message = fmt.Sprintf("%d local commit(s) are not on the remote; nothing to sync", ahead)
		return plan
	}
	if plugin.Dirty > 0 {
		plan.action = "worktree"
		plan.result.Status = "planned"
		plan.result.Action = "worktree"
		plan.result.Message = fmt.Sprintf("%d local change(s) prevent rebase; remote will be checked out in a worktree", plugin.Dirty)
		return plan
	}
	if ahead > 0 && behind > 0 {
		plan.action = "rebase"
		plan.result.Status = "planned"
		plan.result.Action = "rebase"
		plan.result.Message = fmt.Sprintf("rebase %d local commit(s) onto %s", ahead, target)
		return plan
	}
	if behind > 0 {
		plan.action = "fast-forward"
		plan.result.Status = "planned"
		plan.result.Action = "fast-forward"
		plan.result.Message = fmt.Sprintf("fast-forward %d commit(s)", behind)
		return plan
	}
	plan.action = "worktree"
	plan.result.Status = "planned"
	plan.result.Action = "worktree"
	plan.result.Message = "local and remote histories diverged; remote will be checked out in a worktree"
	return plan
}

func (m *Manager) applySync(ctx context.Context, plugin model.Plugin, plan syncPlan, options SyncOptions, backupPath string) SyncResult {
	result := plan.result
	result.BackupPath = backupPath
	action := plan.action
	if action == "fast-forward" {
		target, err := gitutil.ResolveUpdateTarget(plugin.LocalDir)
		if err != nil {
			result.Status = "failed"
			result.Message = err.Error()
			return result
		}
		if err := gitutil.MergeFFOnly(plugin.LocalDir, target); err != nil {
			result.Status = "failed"
			result.Message = "fast-forward: " + err.Error()
			return result
		}
		if err := m.refreshWorktree(plugin, options, target); err != nil {
			result.Status = "failed"
			result.Message = "refresh worktree: " + err.Error()
			return result
		}
		result.Status = "synced"
		return result
	}
	if action == "rebase" {
		target, err := gitutil.ResolveUpdateTarget(plugin.LocalDir)
		if err != nil {
			result.Status = "failed"
			result.Message = err.Error()
			return result
		}
		if err := gitutil.Rebase(plugin.LocalDir, target); err != nil {
			gitutil.RebaseAbort(plugin.LocalDir)
			action = "worktree"
			result.Action = "worktree"
			result.Message = "rebase conflict; local checkout left untouched, remote checked out in worktree: " + firstLine(err.Error())
		} else {
			if err := m.refreshWorktree(plugin, options, target); err != nil {
				result.Status = "failed"
				result.Message = "refresh worktree: " + err.Error()
				return result
			}
			result.Status = "synced"
			result.Message = "local commits rebased onto remote"
			return result
		}
	}
	if action == "worktree" {
		target, err := gitutil.ResolveUpdateTarget(plugin.LocalDir)
		if err != nil {
			result.Status = "failed"
			result.Message = err.Error()
			return result
		}
		worktreeDir := m.worktreeDirFor(plugin.Name, options.WorktreeRoot)
		if err := gitutil.EnsureWorktree(plugin.LocalDir, worktreeDir, target); err != nil {
			result.Status = "failed"
			result.Message = "create worktree: " + err.Error()
			return result
		}
		result.Status = "worktree"
		result.Action = "worktree"
		result.WorktreeDir = worktreeDir
		result.WorktreeRef = gitutil.ShortHead(worktreeDir)
		if result.Message == "" {
			result.Message = "remote checked out in worktree; local checkout untouched"
		}
		return result
	}
	result.Status = "failed"
	result.Message = "unsupported sync action " + action
	return result
}

func firstLine(value string) string {
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		return value[:index]
	}
	return value
}

func (m *Manager) refreshWorktree(plugin model.Plugin, options SyncOptions, target string) error {
	worktreeDir := m.worktreeDirFor(plugin.Name, options.WorktreeRoot)
	if !gitutil.WorktreeRegistered(plugin.LocalDir, worktreeDir) {
		return nil
	}
	return gitutil.EnsureWorktree(plugin.LocalDir, worktreeDir, target)
}

func (m *Manager) worktreeDirFor(name, root string) string {
	if root == "" {
		root = m.Config.WorktreeRoot
	}
	if root == "" {
		root = filepath.Join("/data", "project", "dshpm-worktrees")
	}
	return filepath.Join(root, m.Profile.Name, safePathName(name))
}

func safePathName(name string) string {
	var builder strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			builder.WriteRune(r)
		default:
			builder.WriteRune('_')
		}
	}
	value := builder.String()
	if value == "" {
		return "plugin"
	}
	return value
}
