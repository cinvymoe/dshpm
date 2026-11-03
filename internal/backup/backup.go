// Package backup creates and restores profile-level backups that include
// manifest files and patches from local plugin checkouts.
package backup

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cinvymoe/dshpm/internal/gitutil"
	"github.com/cinvymoe/dshpm/internal/model"
	"github.com/cinvymoe/dshpm/internal/profile"
)

// Files copied from the profile on every backup.
var profileFiles = []string{
	"package.json",
	"pnpm-lock.yaml",
	"pnpm-workspace.yaml",
	"cordis.patch.yml",
	"cordis.yml",
	"compatibility.json",
}

// CreateOptions controls backup creation.
type CreateOptions struct {
	Profile     *profile.Profile
	BackupDir   string
	ToolVersion string
	DSHVersion  string
	Now         time.Time
}

// Create writes a backup and returns its manifest plus directory path.
func Create(options CreateOptions) (*model.BackupManifest, string, error) {
	if options.Profile == nil {
		return nil, "", fmt.Errorf("profile is required")
	}
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	if err := os.MkdirAll(options.BackupDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("create backup directory: %w", err)
	}
	base := fmt.Sprintf("%s-%s", now.Format("20060102-150405"), options.Profile.Name)
	dir := filepath.Join(options.BackupDir, base)
	for suffix := 2; ; suffix++ {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		dir = filepath.Join(options.BackupDir, fmt.Sprintf("%s-%d", base, suffix))
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", fmt.Errorf("create backup %s: %w", dir, err)
	}

	manifest := &model.BackupManifest{
		Format:      1,
		CreatedAt:   now.Format(time.RFC3339),
		Profile:     options.Profile.Name,
		ProfileDir:  options.Profile.Dir,
		ToolVersion: options.ToolVersion,
		DSHVersion:  options.DSHVersion,
	}
	profileDir := filepath.Join(dir, "profile")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return nil, "", err
	}
	for _, name := range profileFiles {
		source := filepath.Join(options.Profile.Dir, name)
		data, err := os.ReadFile(source)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("read %s: %w", source, err)
		}
		if err := os.WriteFile(filepath.Join(profileDir, name), data, 0o644); err != nil {
			return nil, "", fmt.Errorf("write backup of %s: %w", name, err)
		}
		manifest.Files = append(manifest.Files, name)
	}

	pluginsDir := filepath.Join(dir, "plugins")
	for _, plugin := range options.Profile.Plugins() {
		if plugin.Kind != model.SourceLocal || plugin.LocalDir == "" {
			continue
		}
		if !gitutil.IsRepository(plugin.LocalDir) {
			relative := filepath.ToSlash(filepath.Join("plugins", safeName(plugin.Name), "files"))
			size, err := copyTree(plugin.LocalDir, filepath.Join(dir, filepath.FromSlash(relative)))
			if err != nil {
				return nil, "", fmt.Errorf("copy files for %s: %w", plugin.Name, err)
			}
			manifest.Plugins = append(manifest.Plugins, model.BackupPlugin{
				Name:      plugin.Name,
				Dir:       plugin.LocalDir,
				Version:   plugin.InstalledVersion,
				FilesDir:  relative,
				FilesSize: size,
			})
			continue
		}
		entry := model.BackupPlugin{
			Name:     plugin.Name,
			Dir:      plugin.LocalDir,
			Version:  plugin.InstalledVersion,
			Branch:   plugin.Branch,
			Head:     plugin.LocalCommit,
			Upstream: plugin.Upstream,
			Dirty:    plugin.Dirty,
		}
		if plugin.Dirty > 0 {
			patch, err := gitutil.CapturePatch(plugin.LocalDir)
			if err != nil {
				return nil, "", fmt.Errorf("capture patch for %s: %w", plugin.Name, err)
			}
			if len(patch) > 0 {
				pluginDir := filepath.Join(pluginsDir, safeName(plugin.Name))
				if err := os.MkdirAll(pluginDir, 0o755); err != nil {
					return nil, "", err
				}
				relative := filepath.ToSlash(filepath.Join("plugins", safeName(plugin.Name), "changes.patch"))
				if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(relative)), patch, 0o644); err != nil {
					return nil, "", fmt.Errorf("write patch for %s: %w", plugin.Name, err)
				}
				entry.PatchFile = relative
				entry.PatchSize = len(patch)
			}
		}
		manifest.Plugins = append(manifest.Plugins, entry)
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "backup.json"), append(data, '\n'), 0o644); err != nil {
		return nil, "", fmt.Errorf("write backup manifest: %w", err)
	}
	return manifest, dir, nil
}

// ReadManifest loads backup.json from an archive directory.
func ReadManifest(dir string) (*model.BackupManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "backup.json"))
	if err != nil {
		return nil, fmt.Errorf("read backup manifest: %w", err)
	}
	var manifest model.BackupManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse backup manifest: %w", err)
	}
	if manifest.Format != 1 {
		return nil, fmt.Errorf("unsupported backup format %d", manifest.Format)
	}
	return &manifest, nil
}

// RestoreOptions controls restore behavior.
type RestoreOptions struct {
	ArchiveDir       string
	TargetProfileDir string
	Apply            bool
	Force            bool
	Out              io.Writer
}

// RestoreResult summarizes a restore.
type RestoreResult struct {
	Manifest          *model.BackupManifest
	RestoredFiles     []string
	AppliedPatches    []string
	RestoredCheckouts []string
	Conflicts         []string
	Skipped           []string
	DryRun            bool
}

// Restore restores profile files and local patches. Without Apply it only
// reports what would happen.
func Restore(options RestoreOptions) (*RestoreResult, error) {
	manifest, err := ReadManifest(options.ArchiveDir)
	if err != nil {
		return nil, err
	}
	result := &RestoreResult{Manifest: manifest, DryRun: !options.Apply}
	target := options.TargetProfileDir
	if target == "" {
		target = manifest.ProfileDir
	}
	if target == "" {
		return nil, fmt.Errorf("target profile directory is required")
	}
	if options.Apply {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return nil, fmt.Errorf("create target profile: %w", err)
		}
		preserve := filepath.Join(target, ".dshpm-restore-"+time.Now().Format("20060102-150405"))
		for _, name := range manifest.Files {
			source := filepath.Join(options.ArchiveDir, "profile", name)
			data, err := os.ReadFile(source)
			if err != nil {
				return nil, fmt.Errorf("read backup file %s: %w", name, err)
			}
			destination := filepath.Join(target, name)
			if current, err := os.ReadFile(destination); err == nil {
				if err := os.MkdirAll(preserve, 0o755); err != nil {
					return nil, err
				}
				if err := os.WriteFile(filepath.Join(preserve, name), current, 0o644); err != nil {
					return nil, fmt.Errorf("preserve current %s: %w", name, err)
				}
			}
			if err := os.WriteFile(destination, data, 0o644); err != nil {
				return nil, fmt.Errorf("restore %s: %w", name, err)
			}
			result.RestoredFiles = append(result.RestoredFiles, name)
		}
	}

	for _, plugin := range manifest.Plugins {
		if plugin.FilesDir != "" {
			if !options.Apply {
				result.RestoredCheckouts = append(result.RestoredCheckouts, plugin.Name)
				continue
			}
			if _, err := os.Stat(plugin.Dir); err == nil && !options.Force {
				result.Skipped = append(result.Skipped, plugin.Name+" (checkout exists; use --force)")
				continue
			}
			if _, err := copyTree(filepath.Join(options.ArchiveDir, filepath.FromSlash(plugin.FilesDir)), plugin.Dir); err != nil {
				return nil, fmt.Errorf("restore files for %s: %w", plugin.Name, err)
			}
			result.RestoredCheckouts = append(result.RestoredCheckouts, plugin.Name)
			continue
		}
		if plugin.PatchFile == "" {
			continue
		}
		if _, err := os.Stat(plugin.Dir); err != nil {
			result.Skipped = append(result.Skipped, plugin.Name+" (missing checkout)")
			continue
		}
		if !options.Apply {
			result.AppliedPatches = append(result.AppliedPatches, plugin.Name)
			continue
		}
		if dirty := gitutil.DirtyCount(plugin.Dir); dirty > 0 && !options.Force {
			result.Conflicts = append(result.Conflicts, plugin.Name+" (checkout has local changes)")
			continue
		}
		patch, err := os.ReadFile(filepath.Join(options.ArchiveDir, filepath.FromSlash(plugin.PatchFile)))
		if err != nil {
			return nil, fmt.Errorf("read patch for %s: %w", plugin.Name, err)
		}
		if err := gitutil.ApplyPatch(plugin.Dir, patch); err != nil {
			result.Conflicts = append(result.Conflicts, plugin.Name+" ("+err.Error()+")")
			continue
		}
		result.AppliedPatches = append(result.AppliedPatches, plugin.Name)
	}
	return result, nil
}

// copyTree copies regular files from source to destination. Git metadata and
// node_modules are excluded because they are either rebuilt or not portable.
func copyTree(source, destination string) (int, error) {
	if err := os.MkdirAll(destination, 0o755); err != nil {
		return 0, err
	}
	size := 0
	err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == source {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(destination, path[len(source)+1:]), 0o755)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative := path[len(source)+1:]
		target := filepath.Join(destination, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, info.Mode().Perm()); err != nil {
			return err
		}
		size += len(data)
		return nil
	})
	return size, err
}

func safeName(name string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", "@", "_", ":", "_")
	return replacer.Replace(name)
}

// Entry is one stored backup.
type Entry struct {
	ID       string                `json:"id"`
	Path     string                `json:"path"`
	Manifest *model.BackupManifest `json:"manifest"`
}

// List returns backups under root, newest first. An empty profile includes
// every profile; an empty root returns no entries.
func List(root, profile string) ([]Entry, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read backup directory %s: %w", root, err)
	}
	result := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		manifest, err := ReadManifest(dir)
		if err != nil {
			continue
		}
		if profile != "" && manifest.Profile != profile {
			continue
		}
		result = append(result, Entry{ID: entry.Name(), Path: dir, Manifest: manifest})
	}
	sort.Slice(result, func(i, j int) bool {
		left := result[i].Manifest.CreatedAt
		right := result[j].Manifest.CreatedAt
		if left == right {
			return result[i].ID > result[j].ID
		}
		return left > right
	})
	return result, nil
}

// Latest returns the newest backup for a profile.
func Latest(root, profile string) (*Entry, error) {
	entries, err := List(root, profile)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("no backup found for profile %s under %s", profile, root)
	}
	return &entries[0], nil
}

// Find resolves a backup by archive path, full id, or unique id prefix.
func Find(root, profile, reference string) (*Entry, error) {
	if info, err := os.Stat(reference); err == nil && info.IsDir() {
		manifest, err := ReadManifest(reference)
		if err != nil {
			return nil, err
		}
		return &Entry{ID: filepath.Base(reference), Path: reference, Manifest: manifest}, nil
	}
	entries, err := List(root, profile)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.ID == reference {
			copy := entry
			return &copy, nil
		}
	}
	var matches []Entry
	for _, entry := range entries {
		if strings.HasPrefix(entry.ID, reference) {
			matches = append(matches, entry)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("backup %q not found for profile %s", reference, profile)
	case 1:
		copy := matches[0]
		return &copy, nil
	default:
		return nil, fmt.Errorf("backup id %q is ambiguous; use a full id", reference)
	}
}
