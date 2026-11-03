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
	DSHHome        string `json:"dshHome,omitempty"`
	DefaultProfile string `json:"defaultProfile,omitempty"`
	BackupDir      string `json:"backupDir,omitempty"`
	NPMRegistry    string `json:"npmRegistry,omitempty"`
	PNPMRegistry   string `json:"pnpmRegistry,omitempty"`
	PNPM           string `json:"pnpm,omitempty"`
	Git            string `json:"git,omitempty"`
	GH             string `json:"gh,omitempty"`
}

// Resolved is a Config with empty values replaced by defaults and paths
// expanded to absolute paths.
type Resolved struct {
	Config
	ConfigPath string
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
	if cfg.BackupDir == "" {
		cfg.BackupDir = filepath.Join(home, ".config", "dshpm", "backups")
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
	cfg.DSHHome = expandPath(cfg.DSHHome, home)
	cfg.BackupDir = expandPath(cfg.BackupDir, home)
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
