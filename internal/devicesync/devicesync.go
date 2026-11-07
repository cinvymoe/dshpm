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

var dshFiles = []string{
	"package.json",
	"pnpm-lock.yaml",
	"pnpm-workspace.yaml",
	"cordis.patch.yml",
	"cordis.yml",
	"compatibility.json",
}

var piFiles = []string{
	"settings.json",
	"settings-extensions.json",
	"npm/package.json",
	"npm/package-lock.json",
	"npm/bun.lock",
}

// File describes one file in a configuration snapshot.
type File struct {
	Agent  string `json:"agent"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest describes a configuration snapshot.
type Manifest struct {
	Version    int       `json:"version"`
	CreatedAt  time.Time `json:"createdAt"`
	DSHProfile string    `json:"dshProfile,omitempty"`
	Files      []File    `json:"files"`
}

// Change reports how a remote snapshot differs from local configuration.
type Change struct {
	File
	Target string
	Status string
}

// Create writes a snapshot of the selected agent configuration into a
// temporary directory and returns its path and manifest.
func Create(cfg config.Resolved, profileName, agent string) (string, *Manifest, error) {
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
	manifest := &Manifest{Version: 1, CreatedAt: time.Now()}
	add := func(agentName, root, rel string) error {
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
		dst := filepath.Join(dir, agentName, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, info.Mode().Perm()); err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, File{Agent: agentName, Path: rel, Size: info.Size(), SHA256: hex.EncodeToString(hash[:])})
		return nil
	}
	if agent == "all" || agent == "dsh" {
		name, err := cfg.ResolveProfileName(profileName)
		if err != nil {
			return "", nil, err
		}
		manifest.DSHProfile = name
		root := filepath.Join(cfg.DSHHome, "profiles", name)
		for _, rel := range dshFiles {
			if err := add("dsh", root, rel); err != nil {
				return "", nil, err
			}
		}
	}
	if agent == "all" || agent == "pi" {
		for _, rel := range piFiles {
			if err := add("pi", cfg.PiHome, rel); err != nil {
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
	if manifest.Version != 1 {
		return nil, fmt.Errorf("unsupported snapshot version %d", manifest.Version)
	}
	for _, file := range manifest.Files {
		path := filepath.Join(dir, file.Agent, filepath.FromSlash(file.Path))
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
		target, err := targetPath(cfg, profileName, file, manifest.DSHProfile)
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
		target, err := targetPath(cfg, profileName, file, manifest.DSHProfile)
		if err != nil {
			return "", err
		}
		if data, err := os.ReadFile(target); err == nil {
			backupPath := filepath.Join(backupDir, file.Agent, filepath.FromSlash(file.Path))
			if err := os.MkdirAll(filepath.Dir(backupPath), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(backupPath, data, 0o644); err != nil {
				return "", err
			}
		}
		source := filepath.Join(snapshotDir, file.Agent, filepath.FromSlash(file.Path))
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
		if err := os.WriteFile(tmp, data, 0o644); err != nil {
			return "", err
		}
		if err := os.Rename(tmp, target); err != nil {
			os.Remove(tmp)
			return "", err
		}
	}
	return backupDir, nil
}

func targetPath(cfg config.Resolved, profileName string, file File, fallbackProfile string) (string, error) {
	switch file.Agent {
	case "dsh":
		name, err := cfg.ResolveProfileName(profileName)
		if err != nil {
			if fallbackProfile == "" {
				return "", err
			}
			name = fallbackProfile
		}
		return filepath.Join(cfg.DSHHome, "profiles", name, filepath.FromSlash(file.Path)), nil
	case "pi":
		return filepath.Join(cfg.PiHome, filepath.FromSlash(file.Path)), nil
	default:
		return "", fmt.Errorf("unknown snapshot agent %q", file.Agent)
	}
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
