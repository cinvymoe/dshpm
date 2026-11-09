// Package remotesync backs up and restores DSH and Pi configuration files
// through a private GitHub repository. The repository stores a configuration
// file tree (profiles/<name>/... and pi/...) that mirrors local configuration;
// plugin installation state is no longer part of it. Machine-to-machine
// plugin sync uses the SSH backend (internal/devicesync).
package remotesync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cinvymoe/dshpm/internal/config"
	"github.com/cinvymoe/dshpm/internal/devicesync"
)

const (
	// legacyStateName belonged to the retired plugin-state sync format and is
	// cleaned out of the repository on the next push.
	legacyStateName = "dshpm-state.json"

	profileDirName = "profiles" // repository subtree for DSH profile config
	piDirName      = "pi"       // repository subtree for Pi config
)

// ConfigFile describes one configuration file tracked in the backup repository.
type ConfigFile struct {
	Agent   string `json:"agent"`             // "dsh" or "pi"
	Profile string `json:"profile,omitempty"` // DSH profile name; empty for Pi
	Path    string `json:"path"`              // slash-separated path inside the agent root
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
}

// ConfigChange reports how one repository file differs from local configuration.
type ConfigChange struct {
	ConfigFile
	Target string `json:"target"`
	Status string `json:"status"` // add | update | same
}

// configSource pairs a tracked file with its local path.
type configSource struct {
	entry     ConfigFile
	localPath string
}

// collectConfigSources lists every configuration file that belongs in the
// repository for the requested agents. agent is all, dsh, or pi; local files
// that do not exist are skipped.
func collectConfigSources(cfg config.Resolved, agent string) ([]configSource, error) {
	if agent == "" {
		agent = "all"
	}
	if agent != "all" && agent != "dsh" && agent != "pi" {
		return nil, fmt.Errorf("unknown agent %q (want all, dsh, or pi)", agent)
	}
	var sources []configSource
	if agent == "all" || agent == "dsh" {
		profiles, err := cfg.ListProfiles()
		if err != nil {
			return nil, err
		}
		if len(profiles) == 0 {
			return nil, fmt.Errorf("no DSH profile found under %s", filepath.Join(cfg.DSHHome, "profiles"))
		}
		for _, name := range profiles {
			root := filepath.Join(cfg.DSHHome, "profiles", name)
			for _, rel := range devicesync.DSHFiles {
				src := filepath.Join(root, filepath.FromSlash(rel))
				if _, err := os.Stat(src); err != nil {
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					return nil, err
				}
				sources = append(sources, configSource{entry: ConfigFile{Agent: "dsh", Profile: name, Path: rel}, localPath: src})
			}
		}
	}
	if agent == "all" || agent == "pi" {
		for _, rel := range devicesync.PiFiles {
			src := filepath.Join(cfg.PiHome, filepath.FromSlash(rel))
			if _, err := os.Stat(src); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				return nil, err
			}
			sources = append(sources, configSource{entry: ConfigFile{Agent: "pi", Path: rel}, localPath: src})
		}
	}
	sort.Slice(sources, func(i, j int) bool {
		a, b := sources[i].entry, sources[j].entry
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		return a.Path < b.Path
	})
	return sources, nil
}

// DryRun lists the configuration files that Push would upload, without
// touching the repository.
func DryRun(cfg config.Resolved, agent string) ([]ConfigFile, error) {
	sources, err := collectConfigSources(cfg, agent)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no %s configuration files found", agent)
	}
	entries := make([]ConfigFile, 0, len(sources))
	for _, source := range sources {
		sum, size, err := hashFile(source.localPath)
		if err != nil {
			return nil, err
		}
		entry := source.entry
		entry.Size = size
		entry.SHA256 = sum
		entries = append(entries, entry)
	}
	return entries, nil
}

// repoRelPath returns the repository-relative path for a tracked file.
func repoRelPath(entry ConfigFile) string {
	switch entry.Agent {
	case "dsh":
		return filepath.Join(profileDirName, entry.Profile, filepath.FromSlash(entry.Path))
	case "pi":
		return filepath.Join(piDirName, filepath.FromSlash(entry.Path))
	default:
		return ""
	}
}

// localTargetPath maps a tracked file onto this machine's configuration tree.
func localTargetPath(cfg config.Resolved, entry ConfigFile) string {
	switch entry.Agent {
	case "dsh":
		return filepath.Join(cfg.DSHHome, "profiles", entry.Profile, filepath.FromSlash(entry.Path))
	case "pi":
		return filepath.Join(cfg.PiHome, filepath.FromSlash(entry.Path))
	default:
		return ""
	}
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// GitHub stores the configuration file tree in a private repository.
type GitHub struct {
	GH       string
	Git      string
	Repo     string
	Branch   string
	CacheDir string
}

// ValidateRepo validates an owner/repository pair.
func ValidateRepo(repo string) error {
	if !repoPattern.MatchString(repo) {
		return fmt.Errorf("invalid GitHub repository %q (want owner/name)", repo)
	}
	return nil
}

func (g GitHub) dir() string {
	return filepath.Join(g.CacheDir, strings.ReplaceAll(g.Repo, "/", "__"))
}

func (g GitHub) run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return string(output), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(output), nil
}

// EnsureRepo creates the private repository when missing and prepares a local
// clone at the configured cache directory.
func (g GitHub) EnsureRepo(ctx context.Context) error {
	if err := ValidateRepo(g.Repo); err != nil {
		return err
	}
	gh := g.GH
	if gh == "" {
		gh = "gh"
	}
	if _, err := g.run(ctx, gh, "repo", "view", g.Repo, "--json", "name"); err != nil {
		if _, createErr := g.run(ctx, gh, "repo", "create", g.Repo, "--private"); createErr != nil {
			return fmt.Errorf("create private repository %s: %w", g.Repo, createErr)
		}
	}
	dir := g.dir()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := g.run(ctx, gh, "repo", "clone", g.Repo, dir, "--", "--branch", g.Branch, "--depth", "1"); err != nil {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				return mkErr
			}
			if _, initErr := g.run(ctx, g.Git, "init", "-b", g.Branch, dir); initErr != nil {
				return initErr
			}
			sshURL, viewErr := g.run(ctx, gh, "repo", "view", g.Repo, "--json", "sshUrl", "--jq", ".sshUrl")
			if viewErr != nil {
				return viewErr
			}
			if _, addErr := g.run(ctx, g.Git, "-C", dir, "remote", "add", "origin", strings.TrimSpace(sshURL)); addErr != nil {
				return addErr
			}
		}
		return nil
	}
	if _, err := g.run(ctx, g.Git, "-C", dir, "fetch", "origin"); err != nil {
		return err
	}
	if _, err := g.run(ctx, g.Git, "-C", dir, "checkout", g.Branch); err != nil {
		if _, createErr := g.run(ctx, g.Git, "-C", dir, "checkout", "-B", g.Branch); createErr != nil {
			return createErr
		}
	}
	_, _ = g.run(ctx, g.Git, "-C", dir, "pull", "--ff-only", "origin", g.Branch)
	return nil
}

// Push uploads the local configuration tree into the repository. The managed
// subtrees (profiles/, pi/) are rebuilt from scratch on every push so the
// repository always mirrors local configuration; files that disappeared
// locally are removed. The retired dshpm-state.json is cleaned up in the
// same commit.
func (g GitHub) Push(ctx context.Context, cfg config.Resolved, agent string) ([]ConfigFile, error) {
	sources, err := collectConfigSources(cfg, agent)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("no %s configuration files found", agent)
	}
	if err := g.EnsureRepo(ctx); err != nil {
		return nil, err
	}
	dir := g.dir()
	for _, name := range []string{profileDirName, piDirName} {
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return nil, err
		}
	}
	_ = os.Remove(filepath.Join(dir, legacyStateName))
	entries := make([]ConfigFile, 0, len(sources))
	for _, source := range sources {
		data, err := os.ReadFile(source.localPath)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		entry := source.entry
		entry.Size = int64(len(data))
		entry.SHA256 = hex.EncodeToString(sum[:])
		target := filepath.Join(dir, repoRelPath(entry))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(target, data, 0o644); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if _, err := g.run(ctx, g.Git, "-C", dir, "add", "-A"); err != nil {
		return nil, err
	}
	message := "sync: dshpm config " + time.Now().Format(time.RFC3339)
	if _, err := g.run(ctx, g.Git, "-C", dir, "-c", "user.name=dshpm", "-c", "user.email=dshpm@localhost", "commit", "-m", message); err != nil {
		if !strings.Contains(err.Error(), "nothing to commit") && !strings.Contains(err.Error(), "working tree clean") {
			return nil, err
		}
	}
	if _, err := g.run(ctx, g.Git, "-C", dir, "push", "-u", "origin", "HEAD:"+g.Branch); err != nil {
		return nil, err
	}
	return entries, nil
}

// Fetch refreshes the repository clone and returns the backed-up
// configuration files.
func (g GitHub) Fetch(ctx context.Context, agent string) ([]ConfigFile, error) {
	if err := g.EnsureRepo(ctx); err != nil {
		return nil, err
	}
	return readBackupFiles(g.dir(), agent)
}

// readBackupFiles walks the repository clone and collects the tracked
// configuration entries. Missing subtrees yield an empty list.
func readBackupFiles(dir, agent string) ([]ConfigFile, error) {
	if agent == "" {
		agent = "all"
	}
	if agent != "all" && agent != "dsh" && agent != "pi" {
		return nil, fmt.Errorf("unknown agent %q (want all, dsh, or pi)", agent)
	}
	var entries []ConfigFile
	if agent == "all" || agent == "dsh" {
		root := filepath.Join(dir, profileDirName)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			parts := strings.SplitN(filepath.ToSlash(rel), "/", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return nil // stray file directly under profiles/
			}
			sum, size, err := hashFile(path)
			if err != nil {
				return err
			}
			entries = append(entries, ConfigFile{Agent: "dsh", Profile: parts[0], Path: parts[1], Size: size, SHA256: sum})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	if agent == "all" || agent == "pi" {
		root := filepath.Join(dir, piDirName)
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			sum, size, err := hashFile(path)
			if err != nil {
				return err
			}
			entries = append(entries, ConfigFile{Agent: "pi", Path: filepath.ToSlash(rel), Size: size, SHA256: sum})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Agent != b.Agent {
			return a.Agent < b.Agent
		}
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		return a.Path < b.Path
	})
	return entries, nil
}

func hashFile(path string) (string, int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", 0, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), int64(len(data)), nil
}

// Plan compares repository files with local configuration without changing
// anything. Local files absent from the repository are left untouched.
func Plan(cfg config.Resolved, agent string, files []ConfigFile) ([]ConfigChange, error) {
	changes := make([]ConfigChange, 0, len(files))
	for _, entry := range files {
		target := localTargetPath(cfg, entry)
		status := "add"
		if data, err := os.ReadFile(target); err == nil {
			sum := sha256.Sum256(data)
			status = "update"
			if hex.EncodeToString(sum[:]) == entry.SHA256 {
				status = "same"
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		changes = append(changes, ConfigChange{ConfigFile: entry, Target: target, Status: status})
	}
	return changes, nil
}

// Apply restores repository files into local configuration after preserving
// the existing files in a timestamped backup directory. Files that already
// match the repository are skipped; local files absent from the repository
// are never deleted, so restoring never removes anything.
func (g GitHub) Apply(ctx context.Context, cfg config.Resolved, agent string) ([]ConfigChange, string, error) {
	files, err := g.Fetch(ctx, agent)
	if err != nil {
		return nil, "", err
	}
	changes, err := Plan(cfg, agent, files)
	if err != nil {
		return nil, "", err
	}
	backupDir, err := applyConfigChanges(g.dir(), cfg, changes)
	if err != nil {
		return nil, "", err
	}
	return changes, backupDir, nil
}

// applyConfigChanges writes the changed repository files into local
// configuration, backing up existing files first. Files that already match
// the repository are skipped.
func applyConfigChanges(repoDir string, cfg config.Resolved, changes []ConfigChange) (string, error) {
	backupDir := filepath.Join(cfg.BackupDir, "restore-"+time.Now().Format("20060102-150405"))
	for _, change := range changes {
		if change.Status == "same" {
			continue
		}
		if data, err := os.ReadFile(change.Target); err == nil {
			backupPath := filepath.Join(backupDir, filepath.FromSlash(repoRelPath(change.ConfigFile)))
			if err := os.MkdirAll(filepath.Dir(backupPath), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(backupPath, data, 0o644); err != nil {
				return "", err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		source := filepath.Join(repoDir, repoRelPath(change.ConfigFile))
		data, err := os.ReadFile(source)
		if err != nil {
			return "", err
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != change.SHA256 {
			return "", fmt.Errorf("repository file %s: checksum mismatch", repoRelPath(change.ConfigFile))
		}
		if err := os.MkdirAll(filepath.Dir(change.Target), 0o755); err != nil {
			return "", err
		}
		tmp := change.Target + ".dshpm-tmp"
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, change.Target); err != nil {
			os.Remove(tmp)
			return "", err
		}
	}
	return backupDir, nil
}
