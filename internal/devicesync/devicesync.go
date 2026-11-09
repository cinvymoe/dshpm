// Package devicesync creates and transfers snapshots of DSH and Pi plugin
// configuration between devices over SSH.
package devicesync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/cinvymoe/dshpm/internal/config"
)

const manifestName = "manifest.json"

// localAgent namespaces source-tree entries of link:/file: DSH plugins in a
// snapshot. Snapshot version 2 carries these entries plus their roots.
const localAgent = "plugin"

// maxLocalFileBytes skips absurdly large files inside plugin sources.
const maxLocalFileBytes = 16 << 20

// DSHFiles lists the profile-level configuration files captured for DSH.
// Every managed profile contributes its own copy under agent "dsh".
var DSHFiles = []string{
	"package.json",
	"pnpm-lock.yaml",
	"pnpm-workspace.yaml",
	"cordis.patch.yml",
	"cordis.yml",
	"compatibility.json",
}

// PiFiles lists the configuration files captured for Pi under agent "pi".
var PiFiles = []string{
	"settings.json",
	"settings-extensions.json",
	"npm/package.json",
	"npm/package-lock.json",
	"npm/bun.lock",
}

// File describes one file in a configuration snapshot. Profile is the DSH
// profile the file belongs to (empty for Pi and local plugin sources).
type File struct {
	Agent   string `json:"agent"`
	Profile string `json:"profile,omitempty"`
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	SHA256  string `json:"sha256"`
	Mode    uint32 `json:"mode,omitempty"`
}

// perm returns the file mode to create the target with, defaulting to 0o644
// for snapshots recorded before the mode field existed.
func (f File) perm() os.FileMode {
	if f.Mode != 0 {
		return os.FileMode(f.Mode) & 0o777
	}
	return 0o644
}

// Manifest describes a configuration snapshot. Version 3 snapshots carry a
// Profile field on every DSH file; DSHProfile only exists for legacy v1/v2
// snapshots and is ignored when reading v3.
type Manifest struct {
	Version    int       `json:"version"`
	CreatedAt  time.Time `json:"createdAt"`
	DSHProfile string    `json:"dshProfile,omitempty"`
	Plugins    []Plugin  `json:"plugins,omitempty"`
	Files      []File    `json:"files"`
}

// Plugin records one local (link:/file:) plugin source directory that the
// snapshot carries under agent "plugin". Profile names the DSH profile whose
// package.json references it. Root is the absolute path on the creating
// device; DSHHome is that device's dshHome when known, so receivers with a
// different dshHome remap roots that live inside it.
type Plugin struct {
	Profile string `json:"profile,omitempty"`
	Name    string `json:"name"`
	Root    string `json:"root"`
	DSHHome string `json:"dshHome,omitempty"`
}

// localPluginExcludedDirs are never captured from a plugin source tree.
// .git stays so local-remote classification survives on the receiver.
var localPluginExcludedDirs = map[string]bool{"node_modules": true}

// pluginDirEscape converts a plugin name into one safe path segment.
func pluginDirEscape(name string) string {
	return strings.ReplaceAll(name, "/", "%")
}

func pluginDirUnescape(seg string) string {
	return strings.ReplaceAll(seg, "%", "/")
}

// localPluginSpecs lists link:/file: dependencies of a profile package.json.
func localPluginSpecs(pkgPath string) ([]Plugin, error) {
	data, err := os.ReadFile(pkgPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var pkg struct {
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, nil // malformed package.json: config file itself still syncs
	}
	var plugins []Plugin
	for _, set := range []map[string]string{pkg.Dependencies, pkg.DevDependencies} {
		for name, spec := range set {
			root, ok := strings.CutPrefix(spec, "link:")
			if !ok {
				root, ok = strings.CutPrefix(spec, "file:")
			}
			if !ok || root == "" {
				continue
			}
			if !filepath.IsAbs(root) {
				root = filepath.Join(filepath.Dir(pkgPath), root)
			}
			root = filepath.Clean(root)
			info, err := os.Stat(root)
			if err != nil || !info.IsDir() {
				continue // broken or absent link target: nothing to carry
			}
			plugins = append(plugins, Plugin{Name: name, Root: root})
		}
	}
	sort.Slice(plugins, func(i, j int) bool { return plugins[i].Name < plugins[j].Name })
	return plugins, nil
}

// capturePlugin copies one plugin source tree into the snapshot and returns
// its manifest file entries.
func capturePlugin(dir string, p Plugin) ([]File, error) {
	var files []File
	prefix := pluginDirEscape(p.Name)
	err := filepath.Walk(p.Root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(p.Root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if info.IsDir() {
			if localPluginExcludedDirs[info.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() || info.Size() > maxLocalFileBytes {
			return nil
		}
		if rel == ".git/index" || strings.HasSuffix(filepath.ToSlash(rel), "/.git/index") {
			return nil // git staging state is device-local
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		dst := filepath.Join(dir, localAgent, filepath.FromSlash(prefix+"/"+filepath.ToSlash(rel)))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
			return err
		}
		files = append(files, File{Agent: localAgent, Path: prefix + "/" + filepath.ToSlash(rel), Size: info.Size(), SHA256: hex.EncodeToString(hash[:]), Mode: uint32(info.Mode().Perm())})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// Change reports how a remote snapshot differs from local configuration.
type Change struct {
	File
	Target string
	Status string
}

// snapshotVersion is the manifest version written by this build. Version 3
// snapshots carry a Profile field on every DSH file; readers still accept
// legacy v1 (single profile) and v2 (single profile + plugin sources).
const snapshotVersion = 3

// snapshotStagingPath returns where a file lives inside a snapshot directory.
// DSH files with a profile are staged per profile so snapshots with several
// profiles never overwrite each other; plugin and Pi files are flat.
func snapshotStagingPath(dir string, file File) string {
	if file.Agent == "dsh" && file.Profile != "" {
		return filepath.Join(dir, file.Agent, file.Profile, filepath.FromSlash(file.Path))
	}
	return filepath.Join(dir, file.Agent, filepath.FromSlash(file.Path))
}

// Create writes a snapshot of the selected agent configuration into a
// temporary directory and returns its path and manifest. DSH snapshots cover
// every managed profile, not just the default one.
func Create(cfg config.Resolved, agent string) (string, *Manifest, error) {
	if agent == "" {
		agent = "all"
	}
	if agent != "all" && agent != "dsh" && agent != "pi" {
		return "", nil, fmt.Errorf("unknown agent %q (want all, dsh, or pi)", agent)
	}
	dir, err := os.MkdirTemp("", "dshpm-snapshot-*")
	if err != nil {
		return "", nil, err
	}
	manifest := &Manifest{Version: snapshotVersion, CreatedAt: time.Now()}
	add := func(agentName, profile, root, rel string) error {
		src := filepath.Join(root, rel)
		info, err := os.Stat(src)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		hash := sha256.Sum256(data)
		dst := snapshotStagingPath(dir, File{Agent: agentName, Profile: profile, Path: rel})
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, File{Agent: agentName, Profile: profile, Path: rel, Size: info.Size(), SHA256: hex.EncodeToString(hash[:]), Mode: uint32(info.Mode().Perm())})
		return nil
	}
	if agent == "all" || agent == "dsh" {
		profiles, err := cfg.ListProfiles()
		if err != nil {
			return "", nil, err
		}
		if len(profiles) == 0 {
			return "", nil, fmt.Errorf("no DSH profile found under %s", filepath.Join(cfg.DSHHome, "profiles"))
		}
		seen := map[string]string{} // escaped plugin name -> source root
		for _, name := range profiles {
			root := filepath.Join(cfg.DSHHome, "profiles", name)
			for _, rel := range DSHFiles {
				if err := add("dsh", name, root, rel); err != nil {
					return "", nil, err
				}
			}
			if !cfg.LocalSourcesSynced() {
				continue
			}
			plugins, err := localPluginSpecs(filepath.Join(root, "package.json"))
			if err != nil {
				return "", nil, fmt.Errorf("scan local plugin sources: %w", err)
			}
			for _, p := range plugins {
				key := pluginDirEscape(p.Name)
				if prev, ok := seen[key]; ok {
					if prev != p.Root {
						return "", nil, fmt.Errorf("plugin %s resolves to multiple roots across profiles (%s, %s)", p.Name, prev, p.Root)
					}
					continue // already captured from an earlier profile
				}
				files, err := capturePlugin(dir, p)
				if err != nil {
					return "", nil, fmt.Errorf("capture plugin source %s: %w", p.Name, err)
				}
				if len(files) == 0 {
					continue
				}
				seen[key] = p.Root
				p.Profile = name
				p.DSHHome = cfg.DSHHome
				manifest.Plugins = append(manifest.Plugins, p)
				manifest.Files = append(manifest.Files, files...)
			}
		}
	}
	if agent == "all" || agent == "pi" {
		for _, rel := range PiFiles {
			if err := add("pi", "", cfg.PiHome, rel); err != nil {
				return "", nil, err
			}
		}
	}
	if len(manifest.Files) == 0 {
		return "", nil, fmt.Errorf("no %s configuration files found", agent)
	}
	sort.Slice(manifest.Files, func(i, j int) bool {
		if manifest.Files[i].Agent != manifest.Files[j].Agent {
			return manifest.Files[i].Agent < manifest.Files[j].Agent
		}
		return manifest.Files[i].Path < manifest.Files[j].Path
	})
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, manifestName), append(data, '\n'), 0o644); err != nil {
		return "", nil, err
	}
	return dir, manifest, nil
}

// Read loads and validates a snapshot directory.
func Read(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, fmt.Errorf("read snapshot manifest: %w", err)
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse snapshot manifest: %w", err)
	}
	if manifest.Version < 1 || manifest.Version > snapshotVersion {
		return nil, fmt.Errorf("unsupported snapshot version %d", manifest.Version)
	}
	pluginDirs := map[string]bool{}
	for _, p := range manifest.Plugins {
		pluginDirs[pluginDirEscape(p.Name)] = true
	}
	for _, file := range manifest.Files {
		if file.Agent == localAgent {
			dirName, _, ok := strings.Cut(file.Path, "/")
			if !ok || !pluginDirs[dirName] {
				return nil, fmt.Errorf("snapshot plugin file %s: no matching plugin root", file.Path)
			}
		}
		path := snapshotStagingPath(dir, file)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read snapshot file %s: %w", file.Path, err)
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != file.SHA256 {
			return nil, fmt.Errorf("snapshot file %s: checksum mismatch", file.Path)
		}
	}
	return &manifest, nil
}

// Plan compares a snapshot with local configuration without changing anything.
func Plan(cfg config.Resolved, profileName string, manifest *Manifest) ([]Change, error) {
	changes := make([]Change, 0, len(manifest.Files))
	for _, file := range manifest.Files {
		target, err := targetPath(cfg, profileName, file, manifest)
		if err != nil {
			return nil, err
		}
		status := "update"
		if data, err := os.ReadFile(target); err == nil {
			hash := sha256.Sum256(data)
			if hex.EncodeToString(hash[:]) == file.SHA256 {
				status = "same"
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		changes = append(changes, Change{File: file, Target: target, Status: status})
	}
	return changes, nil
}

// Apply copies snapshot files into local configuration after preserving the
// existing files in a timestamped backup directory.
func Apply(cfg config.Resolved, profileName, snapshotDir string, manifest *Manifest) (string, error) {
	backupDir := filepath.Join(cfg.BackupDir, "sync-"+time.Now().Format("20060102-150405"))
	for _, file := range manifest.Files {
		if file.Agent == "pi" && cfg.PiHome == "" {
			return "", fmt.Errorf("snapshot contains Pi files but piHome is not configured")
		}
		target, err := targetPath(cfg, profileName, file, manifest)
		if err != nil {
			return "", err
		}
		if data, err := os.ReadFile(target); err == nil {
			// Keep the profile in the backup layout so files from
			// different profiles cannot overwrite each other.
			backupRel := file.Path
			if file.Profile != "" {
				backupRel = filepath.Join(file.Profile, file.Path)
			}
			backupPath := filepath.Join(backupDir, file.Agent, backupRel)
			if err := os.MkdirAll(filepath.Dir(backupPath), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(backupPath, data, 0o644); err != nil {
				return "", err
			}
		}
		source := snapshotStagingPath(snapshotDir, file)
		data, err := os.ReadFile(source)
		if err != nil {
			return "", err
		}
		hash := sha256.Sum256(data)
		if hex.EncodeToString(hash[:]) != file.SHA256 {
			return "", fmt.Errorf("snapshot file %s: checksum mismatch", file.Path)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return "", err
		}
		tmp := target + ".dshpm-tmp"
		if err := os.WriteFile(tmp, data, file.perm()); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, target); err != nil {
			os.Remove(tmp)
			return "", err
		}
	}
	return backupDir, nil
}

func targetPath(cfg config.Resolved, profileName string, file File, manifest *Manifest) (string, error) {
	switch file.Agent {
	case "dsh":
		name := file.Profile
		if name == "" {
			// Legacy v1/v2 snapshots carry no per-file profile; fall back to
			// the flag, then the manifest's recorded profile, then resolution.
			resolved, err := cfg.ResolveProfileName(profileName)
			if err != nil {
				if manifest.DSHProfile == "" {
					return "", err
				}
				resolved = manifest.DSHProfile
			}
			name = resolved
		}
		return filepath.Join(cfg.DSHHome, "profiles", name, filepath.FromSlash(file.Path)), nil
	case "pi":
		return filepath.Join(cfg.PiHome, filepath.FromSlash(file.Path)), nil
	case localAgent:
		dirName, rel, ok := strings.Cut(file.Path, "/")
		if !ok || rel == "" {
			return "", fmt.Errorf("snapshot plugin file %q: malformed path", file.Path)
		}
		for _, p := range manifest.Plugins {
			if pluginDirEscape(p.Name) != dirName {
				continue
			}
			return filepath.Join(pluginTargetRoot(cfg, p), filepath.FromSlash(rel)), nil
		}
		return "", fmt.Errorf("snapshot plugin file %q: no matching plugin root", file.Path)
	default:
		return "", fmt.Errorf("unknown snapshot agent %q", file.Agent)
	}
}

// pluginTargetRoot remaps a source root recorded on one device onto this
// device: roots inside the sender's dshHome follow the receiver's dshHome,
// anything else keeps its absolute path.
func pluginTargetRoot(cfg config.Resolved, p Plugin) string {
	if p.DSHHome != "" && cfg.DSHHome != "" {
		prefix := filepath.Clean(p.DSHHome) + string(filepath.Separator)
		if rel, ok := strings.CutPrefix(filepath.Clean(p.Root), prefix); ok {
			return filepath.Join(cfg.DSHHome, rel)
		}
	}
	return filepath.Clean(p.Root)
}

// Client transfers snapshots to and from one peer through SSH.
type Client struct {
	SSH        string
	Peer       string
	RemoteRoot string
}

func (c Client) remoteDir() (string, error) {
	if c.Peer == "" {
		return "", errors.New("peer is empty")
	}
	if strings.ContainsAny(c.Peer, "/\\") || c.Peer == "." || c.Peer == ".." {
		return "", fmt.Errorf("invalid peer name %q", c.Peer)
	}
	if c.RemoteRoot == "" {
		return "", errors.New("remote root is empty")
	}
	return filepath.ToSlash(filepath.Join(c.RemoteRoot, c.Peer)), nil
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

// Push uploads a snapshot directory to the peer.
func (c Client) Push(ctx context.Context, snapshotDir string) error {
	remoteDir, err := c.remoteDir()
	if err != nil {
		return err
	}
	tarFile, err := os.CreateTemp("", "dshpm-snapshot-*.tar.gz")
	if err != nil {
		return err
	}
	defer os.Remove(tarFile.Name())
	if err := writeTarGz(tarFile, snapshotDir); err != nil {
		tarFile.Close()
		return err
	}
	if _, err := tarFile.Seek(0, 0); err != nil {
		tarFile.Close()
		return err
	}
	defer tarFile.Close()
	cmd := exec.CommandContext(ctx, c.SSH, c.Peer, "mkdir -p "+shellQuote(remoteDir)+" && tar xzf - -C "+shellQuote(remoteDir))
	cmd.Stdin = tarFile
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("ssh push to %s: %w: %s", c.Peer, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// Pull downloads a snapshot directory from the peer into dest.
func (c Client) Pull(ctx context.Context, dest string) error {
	remoteDir, err := c.remoteDir()
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, c.SSH, c.Peer, "tar czf - -C "+shellQuote(remoteDir)+" .")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	extractErr := extractTarGz(stdout, dest)
	waitErr := cmd.Wait()
	if extractErr != nil {
		return extractErr
	}
	if waitErr != nil {
		return fmt.Errorf("ssh pull from %s: %w: %s", c.Peer, waitErr, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func writeTarGz(w io.Writer, root string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	defer func() {
		tw.Close()
		gz.Close()
	}()
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if info.IsDir() {
			header.Name += "/"
		}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(tw, file)
		return err
	})
}

func extractTarGz(r io.Reader, dest string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(filepath.FromSlash(header.Name))
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe snapshot path %q", header.Name)
		}
		target := filepath.Join(dest, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			file, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(header.Mode)&0o777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(file, tr)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		default:
			return fmt.Errorf("unsupported snapshot entry %q", header.Name)
		}
	}
}
