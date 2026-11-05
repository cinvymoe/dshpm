// Package manager implements listing and updating DSH profile plugins.
package manager

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/cinvymoe/dshpm/internal/backup"
	"github.com/cinvymoe/dshpm/internal/config"
	"github.com/cinvymoe/dshpm/internal/gitutil"
	"github.com/cinvymoe/dshpm/internal/model"
	"github.com/cinvymoe/dshpm/internal/profile"
	"github.com/cinvymoe/dshpm/internal/remote"
	"github.com/cinvymoe/dshpm/internal/version"
)

// Manager coordinates one profile and its remote metadata.
type Manager struct {
	Config   config.Resolved
	Profile  *profile.Profile
	Registry *remote.Registry
	Out      io.Writer
	Offline  bool
}

// New loads a profile and returns a manager.
func New(cfg config.Resolved, profileName string) (*Manager, error) {
	name, err := cfg.ResolveProfileName(profileName)
	if err != nil {
		return nil, err
	}
	loaded, err := profile.Load(cfg.DSHHome, name)
	if err != nil {
		return nil, err
	}
	return &Manager{
		Config:   cfg,
		Profile:  loaded,
		Registry: remote.NewRegistry(cfg.NPMRegistry),
		Out:      os.Stdout,
	}, nil
}

// Enrich fills remote versions for a plugin. It is safe for concurrent use.
func (m *Manager) Enrich(plugin *model.Plugin) {
	if m.Offline {
		plugin.Status = "unknown"
		return
	}
	lockCommits := profile.LockGitCommits(m.Profile.Dir)
	switch plugin.Kind {
	case model.SourceRegistry:
		latest, err := m.Registry.Latest(context.Background(), remote.PackageNameFromSpec(plugin.Name, plugin.Spec))
		if err != nil {
			plugin.Status = "error"
			plugin.Error = err.Error()
			return
		}
		plugin.RemoteVersion = latest
		switch {
		case plugin.InstalledVersion == "":
			plugin.Status = "missing"
		case remote.CompareVersions(plugin.InstalledVersion, latest) < 0:
			plugin.Status = "update"
		default:
			plugin.Status = "up-to-date"
		}
	case model.SourceGit:
		if plugin.LocalCommit == "" {
			plugin.LocalCommit = lockCommits[plugin.Name]
		}
		head, err := remote.LsRemote(plugin.GitURL)
		if err != nil {
			plugin.Status = "error"
			plugin.Error = err.Error()
			return
		}
		plugin.RemoteCommit = head.Commit
		plugin.RemoteVersion = head.Ref
		switch {
		case plugin.LocalCommit == "":
			plugin.Status = "missing"
		case strings.HasPrefix(head.Commit, plugin.LocalCommit) || strings.HasPrefix(plugin.LocalCommit, head.Commit):
			plugin.Status = "up-to-date"
		default:
			plugin.Status = "update"
		}
	case model.SourceLocal:
		if plugin.LocalDir == "" {
			plugin.Status = "missing"
			return
		}
		worktree := m.worktreeDirFor(plugin.Name, "")
		if gitutil.WorktreeRegistered(plugin.LocalDir, worktree) {
			if info, err := os.Stat(worktree); err == nil && info.IsDir() {
				plugin.WorktreeDir = worktree
				plugin.WorktreeRef = gitutil.ShortHead(worktree)
				plugin.WorktreeUsed = true
			}
		}
		gitURL := plugin.GitURL
		if gitURL == "" {
			gitURL = gitutil.RemoteURL(plugin.LocalDir, "origin")
		}
		if gitURL == "" {
			plugin.Status = "local-only"
			return
		}
		head, err := remote.LsRemote(gitURL)
		if err != nil {
			plugin.Status = "error"
			plugin.Error = err.Error()
			return
		}
		plugin.GitURL = gitURL
		plugin.RemoteCommit = head.Commit
		plugin.RemoteVersion = head.Ref
		fullHead, _ := gitutil.Head(plugin.LocalDir)
		switch {
		case plugin.Dirty > 0:
			plugin.Status = "dirty"
		case plugin.Ahead > 0 && plugin.Behind > 0:
			plugin.Status = "diverged"
		case plugin.Ahead > 0:
			plugin.Status = "ahead"
		case plugin.Behind > 0:
			plugin.Status = "update"
		case fullHead != "" && (strings.HasPrefix(head.Commit, fullHead) || strings.HasPrefix(fullHead, head.Commit)):
			plugin.Status = "up-to-date"
		default:
			plugin.Status = "remote-diff"
		}
		if plugin.WorktreeUsed {
			plugin.Status = "worktree"
		}
	case model.SourceTarball:
		plugin.Status = "pinned"
	}
}

// List returns all plugins with remote metadata filled, using a bounded
// worker pool for network lookups.
func (m *Manager) List(ctx context.Context) ([]model.Plugin, error) {
	plugins := m.Profile.Plugins()
	semaphore := make(chan struct{}, 8)
	var wait sync.WaitGroup
	for index := range plugins {
		wait.Add(1)
		go func(plugin *model.Plugin) {
			defer wait.Done()
			select {
			case semaphore <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-semaphore }()
			plugin.Managed = m.Config.IsManaged(plugin.Name)
			m.Enrich(plugin)
		}(&plugins[index])
	}
	wait.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return plugins, nil
}

// UpdateOptions controls an update run.
type UpdateOptions struct {
	All          bool
	Names        []string
	DryRun       bool
	NoBackup     bool
	NoBuild      bool
	BackupDir    string
	BuildTimeout time.Duration
	Out          io.Writer
}

// UpdateResult is the outcome for one plugin.
type UpdateResult struct {
	Name        string           `json:"name"`
	Kind        model.SourceKind `json:"kind"`
	From        string           `json:"from,omitempty"`
	To          string           `json:"to,omitempty"`
	Status      string           `json:"status"`
	Message     string           `json:"message,omitempty"`
	BackupPath  string           `json:"backupPath,omitempty"`
	WorktreeDir string           `json:"worktreeDir,omitempty"`
	WorktreeRef string           `json:"worktreeRef,omitempty"`
}

// Update updates selected plugins. Registry and git dependencies are updated
// through pnpm; local checkouts are fast-forwarded with patches reapplied.
func (m *Manager) Update(ctx context.Context, options UpdateOptions) ([]UpdateResult, error) {
	if options.Out == nil {
		options.Out = m.Out
	}
	if options.BuildTimeout == 0 {
		options.BuildTimeout = 10 * time.Minute
	}
	plugins := m.Profile.Plugins()
	selected, err := selectPlugins(plugins, options)
	if err != nil {
		return nil, err
	}
	selected, unmanaged := m.partitionManaged(selected)
	results := make([]UpdateResult, 0, len(selected)+len(unmanaged))
	needsBackup := false
	for _, plugin := range selected {
		result, candidate, err := m.planOne(plugin, options)
		if err != nil {
			result = UpdateResult{Name: plugin.Name, Kind: plugin.Kind, From: localLabel(plugin), Status: "error", Message: err.Error()}
			results = append(results, result)
			continue
		}
		results = append(results, result)
		if candidate {
			needsBackup = true
		}
	}
	if !needsBackup {
		return append(results, unmanagedUpdateResults(unmanaged, "updated")...), nil
	}
	if options.DryRun {
		for index := range results {
			if results[index].Status == "planned" {
				results[index].Status = "would-update"
			}
		}
		return append(results, unmanagedUpdateResults(unmanaged, "updated")...), nil
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
			return nil, fmt.Errorf("pre-update backup failed: %w", err)
		}
		backupPath = dir
	}
	for index := range results {
		if results[index].Status != "planned" {
			continue
		}
		result := m.applyOne(ctx, selected[index], options, backupPath)
		results[index] = result
	}
	return append(results, unmanagedUpdateResults(unmanaged, "updated")...), nil
}

func (m *Manager) planOne(plugin model.Plugin, options UpdateOptions) (UpdateResult, bool, error) {
	result := UpdateResult{Name: plugin.Name, Kind: plugin.Kind, From: localLabel(plugin)}
	switch plugin.Kind {
	case model.SourceRegistry:
		latest, err := m.Registry.Latest(context.Background(), remote.PackageNameFromSpec(plugin.Name, plugin.Spec))
		if err != nil {
			return result, false, err
		}
		result.To = latest
		if plugin.InstalledVersion != "" && remote.CompareVersions(plugin.InstalledVersion, latest) >= 0 {
			result.Status = "up-to-date"
			return result, false, nil
		}
		result.Status = "planned"
	case model.SourceGit:
		head, err := remote.LsRemote(plugin.GitURL)
		if err != nil {
			return result, false, err
		}
		lockCommits := profile.LockGitCommits(m.Profile.Dir)
		local := plugin.LocalCommit
		if local == "" {
			local = lockCommits[plugin.Name]
		}
		result.From = versionAndCommit(plugin.InstalledVersion, local)
		result.To = head.Ref + "@" + gitutil.Shorten(head.Commit)
		if local != "" && (strings.HasPrefix(head.Commit, local) || strings.HasPrefix(local, head.Commit)) {
			result.Status = "up-to-date"
			return result, false, nil
		}
		result.Status = "planned"
	case model.SourceLocal:
		updated, result, candidate, err := m.planLocal(plugin)
		if err != nil {
			return result, false, err
		}
		_ = updated
		return result, candidate, nil
	case model.SourceTarball:
		result.Status = "pinned"
		result.Message = "tarball dependencies have no update source"
	default:
		result.Status = "skipped"
	}
	return result, result.Status == "planned", nil
}

func (m *Manager) planLocal(plugin model.Plugin) (model.Plugin, UpdateResult, bool, error) {
	result := UpdateResult{Name: plugin.Name, Kind: plugin.Kind, From: versionAndCommit(plugin.InstalledVersion, plugin.LocalCommit)}
	if plugin.LocalDir == "" || !gitutil.IsRepository(plugin.LocalDir) {
		result.Status = "skipped"
		result.Message = "local checkout is not a git repository"
		return plugin, result, false, nil
	}
	if gitutil.Branch(plugin.LocalDir) == "HEAD" {
		result.Status = "detached"
		result.Message = "detached HEAD; check out a branch before updating"
		return plugin, result, false, nil
	}
	if plugin.Dirty > 0 {
		result.Message = fmt.Sprintf("%d local change(s) will be saved as a patch", plugin.Dirty)
	}
	if err := gitutil.Fetch(plugin.LocalDir); err != nil {
		return plugin, result, false, fmt.Errorf("fetch %s: %w", plugin.Name, err)
	}
	target, err := gitutil.ResolveUpdateTarget(plugin.LocalDir)
	if err != nil {
		result.Status = "local-only"
		result.Message = err.Error()
		return plugin, result, false, nil
	}
	targetCommit, err := gitutil.RevParse(plugin.LocalDir, target)
	if err != nil {
		return plugin, result, false, err
	}
	head, err := gitutil.Head(plugin.LocalDir)
	if err != nil {
		return plugin, result, false, err
	}
	result.To = target + "@" + gitutil.Shorten(targetCommit)
	if head == targetCommit {
		result.Status = "up-to-date"
		return plugin, result, false, nil
	}
	ahead, behind, err := gitutil.AheadBehind(plugin.LocalDir, target)
	if err != nil {
		return plugin, result, false, err
	}
	if ahead > 0 {
		result.Status = "local-commits"
		result.Message = fmt.Sprintf("%d local commit(s) ahead; not discarded automatically", ahead)
		return plugin, result, false, nil
	}
	if behind == 0 {
		result.Status = "up-to-date"
		return plugin, result, false, nil
	}
	result.Status = "planned"
	return plugin, result, true, nil
}

func (m *Manager) applyOne(ctx context.Context, plugin model.Plugin, options UpdateOptions, backupPath string) UpdateResult {
	result := UpdateResult{Name: plugin.Name, Kind: plugin.Kind, From: localLabel(plugin), BackupPath: backupPath}
	switch plugin.Kind {
	case model.SourceRegistry, model.SourceGit:
		result.To = ""
		args := []string{"add", plugin.Name + "@" + plugin.Spec}
		expectedCommit := ""
		if plugin.Kind == model.SourceRegistry {
			latest, err := m.Registry.Latest(context.Background(), remote.PackageNameFromSpec(plugin.Name, plugin.Spec))
			if err != nil {
				result.Status = "failed"
				result.Message = err.Error()
				return result
			}
			result.To = latest
			args = []string{"add", plugin.Name + "@" + latest}
		} else {
			head, err := remote.LsRemote(plugin.GitURL)
			if err != nil {
				result.Status = "failed"
				result.Message = err.Error()
				return result
			}
			result.To = head.Ref + "@" + gitutil.Shorten(head.Commit)
			expectedCommit = head.Commit
			// pnpm add with the unchanged spec is a no-op for git deps: the
			// lockfile keeps the old commit. pnpm update re-resolves the branch
			// head without rewriting the manifest spec.
			args = []string{"update", plugin.Name}
		}
		if m.Config.PNPMRegistry != "" {
			args = append(args, "--registry", m.Config.PNPMRegistry)
		}
		if err := m.run(ctx, m.Profile.Dir, m.Config.PNPM, args...); err != nil {
			result.Status = "failed"
			result.Message = err.Error()
			return result
		}
		if plugin.Kind == model.SourceGit {
			locked := profile.LockGitCommits(m.Profile.Dir)[plugin.Name]
			if locked == "" || (!strings.HasPrefix(expectedCommit, locked) && !strings.HasPrefix(locked, expectedCommit)) {
				result.Status = "failed"
				result.Message = fmt.Sprintf("pnpm update did not move the lockfile to %s (locked at %q)", gitutil.Shorten(expectedCommit), locked)
				return result
			}
		}
		result.Status = "updated"
		return result
	case model.SourceLocal:
		return m.applyLocal(ctx, plugin, options, backupPath)
	default:
		result.Status = "skipped"
		return result
	}
}

func (m *Manager) applyLocal(ctx context.Context, plugin model.Plugin, options UpdateOptions, backupPath string) UpdateResult {
	result := UpdateResult{Name: plugin.Name, Kind: plugin.Kind, From: versionAndCommit(plugin.InstalledVersion, plugin.LocalCommit), BackupPath: backupPath}
	if plugin.LocalDir == "" || !gitutil.IsRepository(plugin.LocalDir) {
		result.Status = "skipped"
		return result
	}
	if gitutil.Branch(plugin.LocalDir) == "HEAD" {
		result.Status = "detached"
		result.Message = "detached HEAD; check out a branch before updating"
		return result
	}
	oldHead, err := gitutil.Head(plugin.LocalDir)
	if err != nil {
		result.Status = "failed"
		result.Message = err.Error()
		return result
	}
	patch, err := gitutil.CapturePatch(plugin.LocalDir)
	if err != nil {
		result.Status = "failed"
		result.Message = "capture patch: " + err.Error()
		return result
	}
	target, err := gitutil.ResolveUpdateTarget(plugin.LocalDir)
	if err != nil {
		result.Status = "local-only"
		result.Message = err.Error()
		return result
	}
	targetCommit, err := gitutil.RevParse(plugin.LocalDir, target)
	if err != nil {
		result.Status = "failed"
		result.Message = err.Error()
		return result
	}
	ahead, _, err := gitutil.AheadBehind(plugin.LocalDir, targetCommit)
	if err != nil {
		result.Status = "failed"
		result.Message = err.Error()
		return result
	}
	if ahead > 0 {
		result.Status = "local-commits"
		result.Message = fmt.Sprintf("%d local commit(s) appeared after planning; not discarded automatically", ahead)
		return result
	}
	result.To = target + "@" + gitutil.Shorten(targetCommit)
	if _, err := gitutil.CreateBackupRef(plugin.LocalDir, time.Now().Format("20060102-150405")); err != nil {
		result.Status = "failed"
		result.Message = "create backup ref: " + err.Error()
		return result
	}
	if err := gitutil.ResetHard(plugin.LocalDir, targetCommit); err != nil {
		result.Status = "failed"
		result.Message = "reset: " + err.Error()
		return result
	}
	if err := gitutil.CleanUntracked(plugin.LocalDir); err != nil {
		result.Status = "failed"
		result.Message = "clean: " + err.Error()
		return result
	}
	if len(patch) > 0 {
		if err := gitutil.ApplyPatch(plugin.LocalDir, patch); err != nil {
			conflicts := gitutil.ConflictFiles(plugin.LocalDir)
			if restoreErr := restoreLocalChanges(plugin.LocalDir, oldHead, patch); restoreErr != nil {
				result.Status = "conflict"
				result.Message = "patch did not apply cleanly and local changes could not be restored: " + restoreErr.Error()
				return result
			}
			result.Status = "conflict"
			result.Message = "patch did not apply cleanly; local changes restored"
			if len(conflicts) > 0 {
				result.Message += ": " + strings.Join(conflicts, ", ")
			}
			return result
		}
	}
	if !options.NoBuild {
		if err := m.build(ctx, plugin.LocalDir, options.BuildTimeout); err != nil {
			if restoreErr := restoreLocalChanges(plugin.LocalDir, oldHead, patch); restoreErr != nil {
				result.Status = "build-failed"
				result.Message = err.Error() + "; rollback failed: " + restoreErr.Error()
				return result
			}
			result.Status = "build-failed"
			result.Message = err.Error() + "; local changes restored"
			return result
		}
	}
	if len(patch) > 0 {
		result.Message = "local changes reapplied"
	}
	result.Status = "updated"
	return result
}

func restoreLocalChanges(dir, head string, patch []byte) error {
	if err := gitutil.ResetHard(dir, head); err != nil {
		return err
	}
	if err := gitutil.CleanUntracked(dir); err != nil {
		return err
	}
	if len(patch) == 0 {
		return nil
	}
	return gitutil.ApplyPatch(dir, patch)
}

func (m *Manager) build(ctx context.Context, dir string, timeout time.Duration) error {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil
	}
	if _, ok := pkg.Scripts["build"]; !ok {
		return nil
	}
	buildCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(buildCtx, m.Config.PNPM, "run", "build")
	command.Dir = dir
	var output bytes.Buffer
	command.Stdout = io.MultiWriter(m.Out, &output)
	command.Stderr = command.Stdout
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(output.String())
		if len(message) > 800 {
			message = message[len(message)-800:]
		}
		return fmt.Errorf("build failed: %v: %s", err, message)
	}
	return nil
}

func (m *Manager) run(ctx context.Context, dir, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = dir
	command.Stdout = m.Out
	command.Stderr = m.Out
	if err := command.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func selectPlugins(plugins []model.Plugin, options UpdateOptions) ([]model.Plugin, error) {
	if options.All {
		return plugins, nil
	}
	if len(options.Names) == 0 {
		return nil, fmt.Errorf("choose plugins by name or pass --all")
	}
	wanted := make(map[string]bool, len(options.Names))
	for _, name := range options.Names {
		wanted[name] = true
	}
	selected := make([]model.Plugin, 0, len(options.Names))
	for _, plugin := range plugins {
		if wanted[plugin.Name] {
			selected = append(selected, plugin)
			delete(wanted, plugin.Name)
		}
	}
	if len(wanted) > 0 {
		missing := make([]string, 0, len(wanted))
		for name := range wanted {
			missing = append(missing, name)
		}
		return nil, fmt.Errorf("unknown plugin(s): %s", strings.Join(missing, ", "))
	}
	return selected, nil
}

// partitionManaged separates plugins dshpm may update from user-marked
// exceptions that remain visible but are never updated automatically.
func (m *Manager) partitionManaged(plugins []model.Plugin) (managed, unmanaged []model.Plugin) {
	managed = make([]model.Plugin, 0, len(plugins))
	for _, plugin := range plugins {
		if m.Config.IsManaged(plugin.Name) {
			managed = append(managed, plugin)
			continue
		}
		unmanaged = append(unmanaged, plugin)
	}
	return managed, unmanaged
}

func unmanagedUpdateResults(plugins []model.Plugin, action string) []UpdateResult {
	results := make([]UpdateResult, 0, len(plugins))
	for _, plugin := range plugins {
		results = append(results, UpdateResult{
			Name:    plugin.Name,
			Kind:    plugin.Kind,
			From:    localLabel(plugin),
			Status:  "skipped",
			Message: fmt.Sprintf("unmanaged; not %s", action),
		})
	}
	return results
}

func localLabel(plugin model.Plugin) string {
	return versionAndCommit(plugin.InstalledVersion, plugin.LocalCommit)
}

func versionAndCommit(version, commit string) string {
	if commit != "" {
		return strings.TrimSpace(version + "@" + gitutil.Shorten(commit))
	}
	return version
}

// DoctorCheck is one environment check.
type DoctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// Doctor verifies the runtime environment.
func (m *Manager) Doctor() []DoctorCheck {
	checks := []DoctorCheck{}
	profilesDir := filepath.Join(m.Config.DSHHome, "profiles")
	if info, err := os.Stat(profilesDir); err == nil && info.IsDir() {
		checks = append(checks, DoctorCheck{Name: "dsh-home", OK: true, Detail: m.Config.DSHHome})
	} else {
		checks = append(checks, DoctorCheck{Name: "dsh-home", OK: false, Detail: fmt.Sprintf("%s: %v", profilesDir, err)})
	}
	checks = append(checks, DoctorCheck{Name: "profile", OK: true, Detail: m.Profile.Name + " (" + m.Profile.Dir + ")"})
	for _, tool := range []string{"git", m.Config.PNPM} {
		if path, err := exec.LookPath(tool); err == nil {
			checks = append(checks, DoctorCheck{Name: tool, OK: true, Detail: path})
		} else {
			checks = append(checks, DoctorCheck{Name: tool, OK: false, Detail: "not found in PATH"})
		}
	}
	return checks
}
