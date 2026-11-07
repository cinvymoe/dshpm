// Package config resolves dshpm's filesystem locations and user configuration.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Config is the optional user configuration stored at ~/.config/dshpm/config.json.
type Config struct {
	DSHHome          string              `json:"dshHome,omitempty"`
	DefaultProfile   string              `json:"defaultProfile,omitempty"`
	DefaultAgent     string              `json:"defaultAgent,omitempty"`
	PiHome           string              `json:"piHome,omitempty"`
	BackupDir        string              `json:"backupDir,omitempty"`
	WorktreeRoot     string              `json:"worktreeRoot,omitempty"`
	Unmanaged        []string            `json:"unmanaged,omitempty"`
	UnmanagedByAgent map[string][]string `json:"unmanagedByAgent,omitempty"`
	Modes            map[string]string   `json:"modes,omitempty"`
	NPMRegistry      string              `json:"npmRegistry,omitempty"`
	PNPMRegistry     string              `json:"pnpmRegistry,omitempty"`
	PNPM             string              `json:"pnpm,omitempty"`
	Git              string              `json:"git,omitempty"`
	GH               string              `json:"gh,omitempty"`
	Pi               string              `json:"pi,omitempty"`
	SSH              string              `json:"ssh,omitempty"`
	RemoteRoot       string              `json:"remoteRoot,omitempty"`
	RemoteRepo       string              `json:"remoteRepo,omitempty"`
	RemoteBranch     string              `json:"remoteBranch,omitempty"`
	RemoteCacheDir   string              `json:"remoteCacheDir,omitempty"`
	Peers            map[string]string   `json:"peers,omitempty"`
}

// Resolved is a Config with empty values replaced by defaults and paths
// expanded to absolute paths.
type Resolved struct {
	Config
	ConfigPath string
}

// IsManaged reports whether dshpm may update the named DSH plugin.
func (c Resolved) IsManaged(name string) bool {
	return c.IsManagedFor("dsh", name)
}

// IsManagedFor reports whether dshpm may update the named plugin for one
// agent. DSH keeps using the historical top-level unmanaged list.
func (c Resolved) IsManagedFor(agent, name string) bool {
	if agent == "" || agent == "dsh" {
		for _, unmanaged := range c.Unmanaged {
			if unmanaged == name {
				return false
			}
		}
	}
	for _, unmanaged := range c.UnmanagedByAgent[agent] {
		if unmanaged == name {
			return false
		}
	}
	return true
}

// SetManaged adds or removes a DSH plugin from the unmanaged list.
func (c *Resolved) SetManaged(name string, managed bool) {
	c.SetManagedFor("dsh", name, managed)
}

// SetManagedFor adds or removes a plugin from one agent's unmanaged list.
func (c *Resolved) SetManagedFor(agent, name string, managed bool) {
	if agent == "" || agent == "dsh" {
		if managed {
			kept := c.Unmanaged[:0]
			for _, unmanaged := range c.Unmanaged {
				if unmanaged != name {
					kept = append(kept, unmanaged)
				}
			}
			c.Unmanaged = kept
		} else if c.IsManaged(name) {
			c.Unmanaged = append(c.Unmanaged, name)
		}
		return
	}
	list := c.UnmanagedByAgent[agent]
	if managed {
		kept := list[:0]
		for _, unmanaged := range list {
			if unmanaged != name {
				kept = append(kept, unmanaged)
			}
		}
		if len(kept) == 0 {
			delete(c.UnmanagedByAgent, agent)
			if len(c.UnmanagedByAgent) == 0 {
				c.UnmanagedByAgent = nil
			}
			return
		}
		c.UnmanagedByAgent[agent] = kept
		return
	}
	for _, unmanaged := range list {
		if unmanaged == name {
			return
		}
	}
	if c.UnmanagedByAgent == nil {
		c.UnmanagedByAgent = make(map[string][]string)
	}
	c.UnmanagedByAgent[agent] = append(list, name)
}

// Mode returns the configured local mode for a plugin. An empty value means
// automatic classification.
func (c Resolved) Mode(name string) string {
	return c.Modes[name]
}

// SetMode sets or clears a plugin's local mode. Supported modes are "local"
// and "auto"; an empty mode is treated as "auto".
func (c *Resolved) SetMode(name, mode string) error {
	switch strings.TrimSpace(mode) {
	case "", "auto":
		delete(c.Modes, name)
		if len(c.Modes) == 0 {
			c.Modes = nil
		}
		return nil
	case "local":
		if c.Modes == nil {
			c.Modes = make(map[string]string)
		}
		c.Modes[name] = "local"
		return nil
	default:
		return fmt.Errorf("invalid mode %q (want local or auto)", mode)
	}
}

// Save writes the effective configuration to ConfigPath.
func (c Resolved) Save() error {
	if c.ConfigPath == "" {
		return errors.New("config path is empty")
	}
	data, err := json.MarshalIndent(c.Config, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(c.ConfigPath), 0o755); err != nil {
		return err
	}
	return os.WriteFile(c.ConfigPath, append(data, '\n'), 0o644)
}

// Load reads the default config file when present and applies defaults.
func Load() (Resolved, error) {
	return LoadFile("")
}

// LoadFile reads the given config file when present and applies defaults.
// An empty path selects ~/.config/dshpm/config.json.
func LoadFile(configPath string) (Resolved, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Resolved{}, fmt.Errorf("resolve home directory: %w", err)
	}
	if configPath == "" {
		configPath = filepath.Join(home, ".config", "dshpm", "config.json")
	}
	cfg := Config{}
	data, err := os.ReadFile(configPath)
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Resolved{}, fmt.Errorf("parse %s: %w", configPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Resolved{}, fmt.Errorf("read %s: %w", configPath, err)
	}

	if cfg.DSHHome == "" {
		cfg.DSHHome = filepath.Join(home, ".dsh")
	}
	if cfg.PiHome == "" {
		cfg.PiHome = filepath.Join(home, ".pi", "agent")
	}
	if cfg.DefaultAgent == "" {
		cfg.DefaultAgent = "dsh"
	}
	if cfg.BackupDir == "" {
		cfg.BackupDir = filepath.Join(home, ".config", "dshpm", "backups")
	}
	if cfg.WorktreeRoot == "" {
		cfg.WorktreeRoot = filepath.Join("/data", "project", "dshpm-worktrees")
	}
	if cfg.NPMRegistry == "" {
		cfg.NPMRegistry = "https://registry.npmjs.org"
	}
	if cfg.PNPM == "" {
		cfg.PNPM = "pnpm"
	}
	if cfg.Git == "" {
		cfg.Git = "git"
	}
	if cfg.GH == "" {
		cfg.GH = "gh"
	}
	if cfg.Pi == "" {
		cfg.Pi = "pi"
	}
	if cfg.SSH == "" {
		cfg.SSH = "ssh"
	}
	if cfg.RemoteRoot == "" {
		cfg.RemoteRoot = filepath.Join(home, ".cache", "dshpm", "peers")
	}
	if cfg.RemoteBranch == "" {
		cfg.RemoteBranch = "main"
	}
	if cfg.RemoteCacheDir == "" {
		cfg.RemoteCacheDir = filepath.Join(home, ".cache", "dshpm", "github")
	}
	cfg.DSHHome = expandPath(cfg.DSHHome, home)
	cfg.PiHome = expandPath(cfg.PiHome, home)
	cfg.BackupDir = expandPath(cfg.BackupDir, home)
	cfg.WorktreeRoot = expandPath(cfg.WorktreeRoot, home)
	cfg.RemoteRoot = expandPath(cfg.RemoteRoot, home)
	cfg.RemoteCacheDir = expandPath(cfg.RemoteCacheDir, home)
	return Resolved{Config: cfg, ConfigPath: configPath}, nil
}

// ResolveProfileName chooses the profile to operate on. Precedence:
// explicit flag, DSHPM_PROFILE, config default, a profile named "web", or the
// only profile that declares dependencies.
func (c Resolved) ResolveProfileName(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if env := os.Getenv("DSHPM_PROFILE"); env != "" {
		return env, nil
	}
	if c.DefaultProfile != "" {
		return c.DefaultProfile, nil
	}
	entries, err := os.ReadDir(filepath.Join(c.DSHHome, "profiles"))
	if err != nil {
		return "", fmt.Errorf("read DSH profiles under %s: %w", filepath.Join(c.DSHHome, "profiles"), err)
	}
	var candidates []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(c.DSHHome, "profiles", entry.Name(), "package.json")); err != nil {
			continue
		}
		candidates = append(candidates, entry.Name())
	}
	for _, name := range candidates {
		if name == "web" {
			return name, nil
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no DSH profile found under %s", filepath.Join(c.DSHHome, "profiles"))
	}
	return "", fmt.Errorf("multiple DSH profiles found (%s); use --profile", strings.Join(candidates, ", "))
}

func expandPath(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}
