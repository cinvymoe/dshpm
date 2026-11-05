package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cinvymoe/dshpm/internal/backup"
	"github.com/cinvymoe/dshpm/internal/model"
	"github.com/cinvymoe/dshpm/internal/version"
)

// UninstallOptions controls plugin removal.
type UninstallOptions struct {
	Names     []string
	DryRun    bool
	NoBackup  bool
	BackupDir string
}

// UninstallResult is the outcome for one removed plugin.
type UninstallResult struct {
	Name       string           `json:"name"`
	Kind       model.SourceKind `json:"kind"`
	Status     string           `json:"status"`
	Message    string           `json:"message,omitempty"`
	BackupPath string           `json:"backupPath,omitempty"`
}

// Uninstall removes dependencies from the profile, then cleans bundle and
// patch references that would otherwise keep pointing at missing packages.
// The local source directory is intentionally left in place.
func (m *Manager) Uninstall(ctx context.Context, options UninstallOptions) ([]UninstallResult, error) {
	selected, err := selectPlugins(m.Profile.Plugins(), UpdateOptions{Names: options.Names})
	if err != nil {
		return nil, err
	}
	results := make([]UninstallResult, 0, len(selected))
	for _, plugin := range selected {
		results = append(results, UninstallResult{Name: plugin.Name, Kind: plugin.Kind, Status: "planned"})
	}
	if options.DryRun {
		return results, nil
	}

	backupPath := ""
	if !options.NoBackup {
		backupDir := options.BackupDir
		if backupDir == "" {
			backupDir = m.Config.BackupDir
		}
		_, dir, err := backup.Create(backup.CreateOptions{
			Profile:     m.Profile,
			BackupDir:   backupDir,
			ToolVersion: version.Version,
		})
		if err != nil {
			return nil, fmt.Errorf("pre-uninstall backup failed: %w", err)
		}
		backupPath = dir
	}

	names := make([]string, 0, len(selected))
	identifiers := make(map[string]struct{}, len(selected)*2)
	for _, plugin := range selected {
		names = append(names, plugin.Name)
		identifiers[plugin.Name] = struct{}{}
		if (plugin.Kind == model.SourceLocal || plugin.Kind == model.SourceLocalRemote) && plugin.LocalDir != "" {
			if name := localPackageName(plugin.LocalDir); name != "" {
				identifiers[name] = struct{}{}
			}
		}
	}
	args := append([]string{"remove"}, names...)
	if err := m.run(ctx, m.Profile.Dir, m.Config.PNPM, args...); err != nil {
		return nil, fmt.Errorf("remove plugins: %w", err)
	}
	if err := removeLocalSymlinks(m.Profile.Dir, names); err != nil {
		return nil, fmt.Errorf("remove local plugin links: %w (backup: %s)", err, backupPath)
	}
	if err := cleanProfileReferences(m.Profile.Dir, identifiers); err != nil {
		return nil, fmt.Errorf("clean profile references: %w (backup: %s)", err, backupPath)
	}
	for index := range results {
		results[index].Status = "removed"
		results[index].BackupPath = backupPath
	}
	return results, nil
}

// Install materializes the profile dependency tree from its lockfile. It is
// used after restoring a historical backup.
func (m *Manager) Install(ctx context.Context) error {
	return m.run(ctx, m.Profile.Dir, m.Config.PNPM, "install", "--frozen-lockfile")
}

// removeLocalSymlinks removes leftover link: targets that pnpm can keep in
// node_modules after their dependency declaration is removed.
func removeLocalSymlinks(profileDir string, names []string) error {
	for _, name := range names {
		path := filepath.Join(profileDir, "node_modules", filepath.FromSlash(name))
		info, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

func localPackageName(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	return pkg.Name
}

func cleanProfileReferences(profileDir string, identifiers map[string]struct{}) error {
	packagePath := filepath.Join(profileDir, "package.json")
	data, err := os.ReadFile(packagePath)
	if err != nil {
		return fmt.Errorf("read package.json: %w", err)
	}
	updated, changed, err := removeBundleReferences(data, identifiers)
	if err != nil {
		return fmt.Errorf("remove bundle references: %w", err)
	}
	if changed {
		if err := writeFileAtomic(packagePath, updated, 0o644); err != nil {
			return err
		}
	}

	patchPath := filepath.Join(profileDir, "cordis.patch.yml")
	patch, err := os.ReadFile(patchPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read cordis.patch.yml: %w", err)
	}
	cleaned, changed := removePluginPatchBlocks(string(patch), identifiers)
	if changed {
		if err := writeFileAtomic(patchPath, []byte(cleaned), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func removeBundleReferences(data []byte, identifiers map[string]struct{}) ([]byte, bool, error) {
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, false, err
	}
	changed := false
	filter := func(holder map[string]any) {
		bundles, ok := holder["bundles"].([]any)
		if !ok {
			return
		}
		kept := make([]any, 0, len(bundles))
		for _, entry := range bundles {
			if _, match := identifiers[bundleReference(entry)]; match {
				changed = true
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) != len(bundles) {
			holder["bundles"] = kept
		}
	}
	filter(document)
	if dsh, ok := document["dsh"].(map[string]any); ok {
		if profile, ok := dsh["profile"].(map[string]any); ok {
			filter(profile)
		}
	}
	if !changed {
		return data, false, nil
	}
	updated, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(updated, '\n'), true, nil
}

func bundleReference(entry any) string {
	switch value := entry.(type) {
	case string:
		return value
	case map[string]any:
		if name, ok := value["name"].(string); ok {
			return name
		}
		if id, ok := value["id"].(string); ok {
			return id
		}
	}
	return ""
}

var insertNamePattern = regexp.MustCompile(`(?m)(?:^|\s)(?:name|id)\s*:\s*["']?([^"'\s#\n]+)["']?`)

// removePluginPatchBlocks drops top-level patch entries that name an
// uninstalled plugin, including insert lists and id-targeted overrides.
func removePluginPatchBlocks(content string, identifiers map[string]struct{}) (string, bool) {
	blocks := splitPatchBlocks(content)
	kept := make([]string, 0, len(blocks))
	changed := false
	for _, block := range blocks {
		if blockMatchesPlugin(block, identifiers) {
			changed = true
			continue
		}
		kept = append(kept, block)
	}
	if !changed {
		return content, false
	}
	return strings.Join(kept, ""), true
}

func splitPatchBlocks(content string) []string {
	lines := strings.Split(content, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	blocks := make([]string, 0)
	current := ""
	for _, line := range lines {
		if strings.HasPrefix(line, "- ") && strings.TrimSpace(current) != "" {
			blocks = append(blocks, current)
			current = line + "\n"
			continue
		}
		current += line + "\n"
	}
	if strings.TrimSpace(current) != "" {
		blocks = append(blocks, current)
	}
	return blocks
}

func blockMatchesPlugin(block string, identifiers map[string]struct{}) bool {
	if !strings.HasPrefix(block, "- ") {
		return false
	}
	subject := block
	if !strings.Contains(block, "insert:") {
		subject, _, _ = strings.Cut(block, "\n")
	}
	for _, match := range insertNamePattern.FindAllStringSubmatch(subject, -1) {
		if _, ok := identifiers[match[1]]; ok {
			return true
		}
	}
	return false
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".dshpm-*")
	if err != nil {
		return err
	}
	name := temp.Name()
	defer os.Remove(name)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
