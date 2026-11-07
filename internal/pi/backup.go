package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cinvymoe/dshpm/internal/model"
)

// backupFiles are copied from the Pi agent directory on every backup.
var backupFiles = []string{
	"settings.json",
	"settings-extensions.json",
	"npm/package.json",
	"npm/package-lock.json",
	"npm/bun.lock",
}

// BackupOptions controls Pi backup creation.
type BackupOptions struct {
	BackupDir   string
	ToolVersion string
	Now         time.Time
}

// Backup writes a Pi settings backup and returns its manifest plus directory.
func (m *Manager) Backup(options BackupOptions) (*model.BackupManifest, string, error) {
	now := options.Now
	if now.IsZero() {
		now = time.Now()
	}
	if options.BackupDir == "" {
		return nil, "", fmt.Errorf("backup directory is required")
	}
	if err := os.MkdirAll(options.BackupDir, 0o755); err != nil {
		return nil, "", fmt.Errorf("create backup directory: %w", err)
	}
	base := fmt.Sprintf("%s-pi", now.Format("20060102-150405"))
	dir := filepath.Join(options.BackupDir, base)
	for suffix := 2; ; suffix++ {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		dir = filepath.Join(options.BackupDir, fmt.Sprintf("%s-%d", base, suffix))
	}
	if err := os.MkdirAll(filepath.Join(dir, "profile"), 0o755); err != nil {
		return nil, "", err
	}
	manifest := &model.BackupManifest{
		Format:      1,
		CreatedAt:   now.Format(time.RFC3339),
		Profile:     "pi",
		ProfileDir:  m.Home,
		ToolVersion: options.ToolVersion,
	}
	for _, name := range backupFiles {
		source := filepath.Join(m.Home, filepath.FromSlash(name))
		data, err := os.ReadFile(source)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, "", fmt.Errorf("read %s: %w", source, err)
		}
		destination := filepath.Join(dir, "profile", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return nil, "", err
		}
		if err := os.WriteFile(destination, data, 0o644); err != nil {
			return nil, "", fmt.Errorf("write backup of %s: %w", name, err)
		}
		manifest.Files = append(manifest.Files, name)
	}
	plugins, err := m.packages(context.Background(), false)
	if err == nil {
		for _, plugin := range plugins {
			if plugin.Kind != model.SourceLocal || plugin.LocalDir == "" {
				continue
			}
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
		}
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
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative := path[len(source)+1:]
		target := filepath.Join(destination, relative)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
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
