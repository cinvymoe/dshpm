// Package pi manages plugins declared in Pi's user settings.
package pi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/cinvymoe/dshpm/internal/config"
	"github.com/cinvymoe/dshpm/internal/gitutil"
	"github.com/cinvymoe/dshpm/internal/model"
	"github.com/cinvymoe/dshpm/internal/remote"
)

// Manager reads and updates the packages declared in ~/.pi/agent/settings.json.
type Manager struct {
	Config  config.Resolved
	Home    string
	Offline bool
	Out     io.Writer
}

// New returns a Pi manager for the resolved dshpm configuration.
func New(cfg config.Resolved) (*Manager, error) {
	home := cfg.PiHome
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve home directory: %w", err)
		}
		home = filepath.Join(userHome, ".pi", "agent")
	}
	return &Manager{Config: cfg, Home: home}, nil
}

type settingsDocument struct {
	Packages []json.RawMessage `json:"packages"`
}

type packageEntry struct {
	Source string
}

func loadSettings(path string) ([]packageEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read Pi settings %s: %w", path, err)
	}
	var document settingsDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse Pi settings %s: %w", path, err)
	}
	entries := make([]packageEntry, 0, len(document.Packages))
	for _, raw := range document.Packages {
		var source string
		if err := json.Unmarshal(raw, &source); err == nil {
			if strings.TrimSpace(source) != "" {
				entries = append(entries, packageEntry{Source: source})
			}
			continue
		}
		var object struct {
			Source string `json:"source"`
		}
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, fmt.Errorf("parse Pi package entry %s: %w", string(raw), err)
		}
		if strings.TrimSpace(object.Source) != "" {
			entries = append(entries, packageEntry{Source: object.Source})
		}
	}
	return entries, nil
}

func sourceKind(source string) model.SourceKind {
	value := strings.TrimSpace(source)
	switch {
	case strings.HasPrefix(value, "npm:"):
		return model.SourceRegistry
	case strings.HasPrefix(value, "git:"), strings.HasPrefix(value, "github:"),
		strings.HasPrefix(value, "https://"), strings.HasPrefix(value, "http://"):
		return model.SourceGit
	case strings.HasPrefix(value, "file:"), filepath.IsAbs(value),
		strings.HasPrefix(value, "./"), strings.HasPrefix(value, "../"),
		strings.HasPrefix(value, "~/"):
		return model.SourceLocal
	default:
		return model.SourceRegistry
	}
}

func packageNameFromSource(source string) string {
	value := strings.TrimSpace(source)
	switch {
	case strings.HasPrefix(value, "npm:"):
		name := strings.TrimPrefix(value, "npm:")
		if strings.HasPrefix(name, "@") {
			if index := strings.Index(name[1:], "@"); index >= 0 {
				return name[:index+1]
			}
			return name
		}
		if index := strings.LastIndex(name, "@"); index > 0 {
			return name[:index]
		}
		return name
	case strings.HasPrefix(value, "git:"):
		value = strings.TrimPrefix(value, "git:")
		value = strings.TrimSuffix(value, ".git")
		value = strings.TrimSuffix(value, "/")
		return filepath.Base(value)
	case strings.HasPrefix(value, "https://"), strings.HasPrefix(value, "http://"):
		value = strings.TrimSuffix(value, ".git")
		value = strings.TrimSuffix(value, "/")
		if parsed, err := url.Parse(value); err == nil {
			return filepath.Base(parsed.Path)
		}
		return filepath.Base(value)
	default:
		return filepath.Base(strings.TrimPrefix(value, "file:"))
	}
}

func localSourcePath(source, home string) string {
	value := strings.TrimPrefix(strings.TrimSpace(source), "file:")
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") {
		return filepath.Join(home, value[2:])
	}
	if !filepath.IsAbs(value) {
		return filepath.Join(home, value)
	}
	return value
}

func normalizeGitURL(source string) string {
	value := strings.TrimSpace(source)
	value = strings.TrimPrefix(value, "git:")
	value = strings.TrimPrefix(value, "github:")
	if strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "git@") {
		return value
	}
	return "https://" + value
}

func (m *Manager) executable() string {
	if m.Config.Pi != "" {
		return m.Config.Pi
	}
	if path, err := exec.LookPath("pi"); err == nil {
		return path
	}
	userHome, _ := os.UserHomeDir()
	candidate := filepath.Join(userHome, ".bun", "bin", "pi")
	if _, err := os.Stat(candidate); err == nil {
		return candidate
	}
	return "pi"
}

func (m *Manager) run(ctx context.Context, args ...string) error {
	command := exec.CommandContext(ctx, m.executable(), args...)
	command.Dir = m.Home
	command.Env = append(os.Environ(), "NO_COLOR=1")
	output, err := command.CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message != "" {
			return fmt.Errorf("pi %s: %w: %s", strings.Join(args, " "), err, message)
		}
		return fmt.Errorf("pi %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func (m *Manager) readListPaths() map[string]string {
	paths := make(map[string]string)
	command := exec.Command(m.executable(), "list")
	command.Env = append(os.Environ(), "NO_COLOR=1")
	output, err := command.Output()
	if err != nil {
		return paths
	}
	source := ""
	for _, line := range strings.Split(string(output), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		indent := len(line) - len(trimmed)
		if trimmed == "" {
			continue
		}
		if indent == 2 {
			source = strings.TrimSpace(strings.TrimSuffix(trimmed, "(filtered)"))
			continue
		}
		if indent >= 4 && source != "" {
			paths[source] = strings.TrimSpace(trimmed)
		}
	}
	return paths
}

func readPackageVersion(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return ""
	}
	return pkg.Version
}

func (m *Manager) packages(ctx context.Context, lookupRemote bool) ([]model.Plugin, error) {
	entries, err := loadSettings(filepath.Join(m.Home, "settings.json"))
	if err != nil {
		return nil, err
	}
	paths := m.readListPaths()
	registry := remote.NewRegistry(m.Config.NPMRegistry)
	plugins := make([]model.Plugin, 0, len(entries))
	for _, entry := range entries {
		kind := sourceKind(entry.Source)
		name := packageNameFromSource(entry.Source)
		plugin := model.Plugin{
			Name:    name,
			Agent:   "pi",
			Spec:    entry.Source,
			Kind:    kind,
			Managed: m.Config.IsManagedFor("pi", name),
		}
		switch kind {
		case model.SourceRegistry:
			plugin.LocalDir = filepath.Join(m.Home, "npm", "node_modules", name)
		case model.SourceLocal:
			plugin.LocalDir = localSourcePath(entry.Source, m.Home)
		default:
			plugin.LocalDir = paths[entry.Source]
			if plugin.LocalDir == "" {
				plugin.LocalDir = filepath.Join(m.Home, "npm", "node_modules", name)
			}
			plugin.GitURL = normalizeGitURL(entry.Source)
		}
		if info, err := os.Stat(plugin.LocalDir); err == nil && info.IsDir() {
			plugin.InstalledVersion = readPackageVersion(plugin.LocalDir)
			if gitutil.IsRepository(plugin.LocalDir) {
				if head, err := gitutil.Head(plugin.LocalDir); err == nil {
					plugin.LocalCommit = head
				}
				plugin.Branch = gitutil.Branch(plugin.LocalDir)
				plugin.Dirty = gitutil.DirtyCount(plugin.LocalDir)
				if plugin.GitURL == "" {
					plugin.GitURL = gitutil.RemoteURL(plugin.LocalDir, "origin")
				}
			}
		}
		switch kind {
		case model.SourceRegistry:
			if lookupRemote && !m.Offline {
				latest, err := registry.Latest(ctx, name)
				if err != nil {
					plugin.Error = err.Error()
					plugin.Status = "unknown"
				} else {
					plugin.RemoteVersion = latest
					if plugin.InstalledVersion == "" {
						plugin.Status = "missing"
					} else if remote.CompareVersions(plugin.InstalledVersion, latest) < 0 {
						plugin.Status = "update"
					} else {
						plugin.Status = "up-to-date"
					}
				}
			}
			if plugin.Status == "" {
				if plugin.InstalledVersion == "" {
					plugin.Status = "missing"
				} else {
					plugin.Status = "unknown"
				}
			}
		case model.SourceGit:
			if lookupRemote && !m.Offline && plugin.GitURL != "" {
				head, err := remote.LsRemote(plugin.GitURL)
				if err != nil {
					plugin.Error = err.Error()
					plugin.Status = "unknown"
				} else {
					plugin.RemoteVersion = head.Ref
					plugin.RemoteCommit = head.Commit
					if plugin.LocalCommit == "" {
						plugin.Status = "missing"
					} else if head.Commit != "" && strings.HasPrefix(head.Commit, plugin.LocalCommit) {
						plugin.Status = "up-to-date"
					} else {
						plugin.Status = "update"
					}
				}
			}
			if plugin.Status == "" {
				if plugin.LocalDir == "" {
					plugin.Status = "missing"
				} else {
					plugin.Status = "unknown"
				}
			}
		case model.SourceLocal:
			if plugin.Status == "" {
				plugin.Status = "local"
			}
		}
		if plugin.Status == "" {
			plugin.Status = "unknown"
		}
		plugins = append(plugins, plugin)
	}
	return plugins, nil
}

// List returns every package declared in Pi's user settings.
func (m *Manager) List(ctx context.Context) ([]model.Plugin, error) {
	return m.packages(ctx, true)
}

// PackageCount returns the number of packages declared in Pi settings.
func (m *Manager) PackageCount() (int, error) {
	entries, err := loadSettings(filepath.Join(m.Home, "settings.json"))
	if err != nil {
		return 0, err
	}
	return len(entries), nil
}

// UpgradeOptions controls Pi package upgrades.
type UpgradeOptions struct {
	All       bool
	Names     []string
	DryRun    bool
	NoBackup  bool
	BackupDir string
	Out       io.Writer
}

// UpgradeResult is one Pi package upgrade outcome.
type UpgradeResult struct {
	Name       string           `json:"name"`
	Kind       model.SourceKind `json:"kind"`
	From       string           `json:"from,omitempty"`
	To         string           `json:"to,omitempty"`
	Status     string           `json:"status"`
	Message    string           `json:"message,omitempty"`
	BackupPath string           `json:"backupPath,omitempty"`
}

// Upgrade updates selected Pi packages through the pi CLI.
func (m *Manager) Upgrade(ctx context.Context, options UpgradeOptions) ([]UpgradeResult, error) {
	plugins, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	selected, err := selectPlugins(plugins, options.Names, options.All)
	if err != nil {
		return nil, err
	}
	backupPath := ""
	if !options.DryRun && !options.NoBackup {
		_, dir, err := m.Backup(BackupOptions{BackupDir: options.BackupDir, ToolVersion: "dshpm"})
		if err != nil {
			return nil, fmt.Errorf("create pre-upgrade backup: %w", err)
		}
		backupPath = dir
	}
	results := make([]UpgradeResult, 0, len(selected))
	for _, plugin := range selected {
		result := UpgradeResult{Name: plugin.Name, Kind: plugin.Kind, From: plugin.InstalledVersion, To: plugin.RemoteVersion, BackupPath: backupPath}
		if !plugin.Managed {
			result.Status = "skipped"
			result.Message = "unmanaged; not upgraded"
			results = append(results, result)
			continue
		}
		if plugin.Kind == model.SourceLocal {
			result.Status = "skipped"
			result.Message = "local package; update it in place"
			results = append(results, result)
			continue
		}
		if plugin.RemoteVersion != "" && plugin.InstalledVersion == plugin.RemoteVersion {
			result.Status = "up-to-date"
			results = append(results, result)
			continue
		}
		if options.DryRun {
			result.Status = "would-update"
			results = append(results, result)
			continue
		}
		if err := m.run(ctx, "update", plugin.Spec); err != nil {
			result.Status = "failed"
			result.Message = err.Error()
			results = append(results, result)
			continue
		}
		result.Status = "updated"
		result.To = readPackageVersion(plugin.LocalDir)
		if result.To == "" {
			result.To = plugin.RemoteVersion
		}
		results = append(results, result)
	}
	return results, nil
}

// UninstallOptions controls Pi package removal.
type UninstallOptions struct {
	Names     []string
	DryRun    bool
	NoBackup  bool
	BackupDir string
}

// UninstallResult is one Pi package removal outcome.
type UninstallResult struct {
	Name       string           `json:"name"`
	Kind       model.SourceKind `json:"kind"`
	Status     string           `json:"status"`
	Message    string           `json:"message,omitempty"`
	BackupPath string           `json:"backupPath,omitempty"`
}

// Uninstall removes selected packages through the pi CLI.
func (m *Manager) Uninstall(ctx context.Context, options UninstallOptions) ([]UninstallResult, error) {
	plugins, err := m.List(ctx)
	if err != nil {
		return nil, err
	}
	selected, err := selectPlugins(plugins, options.Names, false)
	if err != nil {
		return nil, err
	}
	backupPath := ""
	if !options.DryRun && !options.NoBackup {
		_, dir, err := m.Backup(BackupOptions{BackupDir: options.BackupDir, ToolVersion: "dshpm"})
		if err != nil {
			return nil, fmt.Errorf("create pre-uninstall backup: %w", err)
		}
		backupPath = dir
	}
	results := make([]UninstallResult, 0, len(selected))
	for _, plugin := range selected {
		result := UninstallResult{Name: plugin.Name, Kind: plugin.Kind, BackupPath: backupPath}
		if options.DryRun {
			result.Status = "would-remove"
			results = append(results, result)
			continue
		}
		if err := m.run(ctx, "remove", plugin.Spec); err != nil {
			result.Status = "failed"
			result.Message = err.Error()
			results = append(results, result)
			continue
		}
		result.Status = "removed"
		results = append(results, result)
	}
	return results, nil
}

func selectPlugins(plugins []model.Plugin, names []string, all bool) ([]model.Plugin, error) {
	if all {
		return plugins, nil
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("at least one plugin name is required (or use --all)")
	}
	byName := make(map[string]model.Plugin, len(plugins))
	for _, plugin := range plugins {
		byName[plugin.Name] = plugin
		byName[plugin.Spec] = plugin
	}
	selected := make([]model.Plugin, 0, len(names))
	seen := make(map[string]bool)
	for _, name := range names {
		plugin, ok := byName[name]
		if !ok {
			return nil, fmt.Errorf("Pi package %q is not declared in settings.json", name)
		}
		if seen[plugin.Spec] {
			continue
		}
		seen[plugin.Spec] = true
		selected = append(selected, plugin)
	}
	return selected, nil
}

// DoctorCheck is one diagnostic result.
type DoctorCheck struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// Doctor checks the Pi executable and settings locations.
func (m *Manager) Doctor() []DoctorCheck {
	checks := []DoctorCheck{}
	if path, err := exec.LookPath(m.executable()); err == nil {
		checks = append(checks, DoctorCheck{Name: "pi executable", Status: "ok", Message: path})
	} else if _, statErr := os.Stat(m.executable()); statErr == nil {
		checks = append(checks, DoctorCheck{Name: "pi executable", Status: "ok", Message: m.executable()})
	} else {
		checks = append(checks, DoctorCheck{Name: "pi executable", Status: "error", Message: "pi not found; set config pi"})
	}
	if _, err := os.Stat(filepath.Join(m.Home, "settings.json")); err == nil {
		checks = append(checks, DoctorCheck{Name: "settings", Status: "ok", Message: filepath.Join(m.Home, "settings.json")})
	} else {
		checks = append(checks, DoctorCheck{Name: "settings", Status: "error", Message: err.Error()})
	}
	if _, err := os.Stat(filepath.Join(m.Home, "npm")); err == nil {
		checks = append(checks, DoctorCheck{Name: "npm directory", Status: "ok", Message: filepath.Join(m.Home, "npm")})
	} else {
		checks = append(checks, DoctorCheck{Name: "npm directory", Status: "warn", Message: "npm package directory not found"})
	}
	return checks
}
