package devicesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cinvymoe/dshpm/internal/config"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotRoundTripOverFakeSSH(t *testing.T) {
	root := t.TempDir()
	sourceDSH := filepath.Join(root, "source-dsh")
	sourcePi := filepath.Join(root, "source-pi")
	targetDSH := filepath.Join(root, "target-dsh")
	targetPi := filepath.Join(root, "target-pi")
	writeFile(t, filepath.Join(sourceDSH, "profiles", "web", "package.json"), `{"dependencies":{"alpha":"^1.0.0"}}`)
	writeFile(t, filepath.Join(sourcePi, "settings.json"), `{"packages":["npm:alpha"]}`)
	writeFile(t, filepath.Join(sourcePi, "npm", "package.json"), `{"dependencies":{"alpha":"1.0.0"}}`)

	fakeSSH := filepath.Join(root, "fake-ssh")
	if err := os.WriteFile(fakeSSH, []byte("#!/bin/sh\nshift\nexec sh -c \"$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	remoteRoot := filepath.Join(root, "remote")
	sourceCfg := config.Resolved{Config: config.Config{
		DSHHome: sourceDSH, PiHome: sourcePi, BackupDir: filepath.Join(root, "backups"), RemoteRoot: remoteRoot,
	}}
	snapshot, manifest, err := Create(sourceCfg, "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) < 3 {
		t.Fatalf("expected at least 3 files, got %d", len(manifest.Files))
	}
	client := Client{SSH: fakeSSH, Peer: "local", RemoteRoot: remoteRoot}
	if err := client.Push(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	pulled := filepath.Join(root, "pulled")
	if err := os.MkdirAll(pulled, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := client.Pull(context.Background(), pulled); err != nil {
		t.Fatal(err)
	}
	pulledManifest, err := Read(pulled)
	if err != nil {
		t.Fatal(err)
	}
	if len(pulledManifest.Files) != len(manifest.Files) {
		t.Fatalf("manifest file count = %d, want %d", len(pulledManifest.Files), len(manifest.Files))
	}
	targetCfg := config.Resolved{Config: config.Config{
		DSHHome: targetDSH, PiHome: targetPi, BackupDir: filepath.Join(root, "target-backups"),
	}}
	changes, err := Plan(targetCfg, "", pulledManifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Status != "update" {
			t.Fatalf("%s/%s status = %q, want update", change.Agent, change.Path, change.Status)
		}
	}
	backupDir, err := Apply(targetCfg, "", pulled, pulledManifest)
	if err != nil {
		t.Fatal(err)
	}
	if backupDir == "" {
		t.Fatal("empty backup directory")
	}
	assertFileContains(t, filepath.Join(targetDSH, "profiles", "web", "package.json"), `"alpha"`)
	assertFileContains(t, filepath.Join(targetPi, "settings.json"), `npm:alpha`)
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(string(data), want) {
		t.Fatalf("%s = %q, want contains %q", path, string(data), want)
	}
}

func contains(value, want string) bool {
	for i := 0; i+len(want) <= len(value); i++ {
		if value[i:i+len(want)] == want {
			return true
		}
	}
	return false
}

func TestLocalPluginSourcesSync(t *testing.T) {
	root := t.TempDir()
	sourceDSH := filepath.Join(root, "src-dsh")
	targetDSH := filepath.Join(root, "dst-dsh")
	alpha := filepath.Join(sourceDSH, "external", "dsh-alpha")
	ext := filepath.Join(root, "outside", "dsh-ext")
	scoped := filepath.Join(root, "third", "scoped")
	writeFile(t, filepath.Join(alpha, "index.js"), "alpha")
	writeFile(t, filepath.Join(alpha, "node_modules", "junk.js"), "junk")
	writeFile(t, filepath.Join(alpha, ".git", "config"), "[remote \"origin\"]\nurl = x\n")
	writeFile(t, filepath.Join(ext, "main.go"), "ext")
	writeFile(t, filepath.Join(scoped, "lib.js"), "scoped")
	if err := os.Chmod(filepath.Join(ext, "main.go"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sourceDSH, "profiles", "web", "package.json"),
		`{"dependencies":{"dsh-alpha":"link:`+alpha+`","dsh-ext":"link:`+ext+`","@s/mod":"link:`+scoped+`","plain":"^1.0.0","broken":"link:`+filepath.Join(root, "gone")+`"}}`)

	sourceCfg := config.Resolved{Config: config.Config{DSHHome: sourceDSH, BackupDir: filepath.Join(root, "backups")}}
	snapshot, manifest, err := Create(sourceCfg, "dsh")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != snapshotVersion {
		t.Fatalf("expected version %d, got %d", snapshotVersion, manifest.Version)
	}
	if len(manifest.Plugins) != 3 {
		t.Fatalf("expected 3 plugin roots (broken link skipped), got %+v", manifest.Plugins)
	}
	var sawGit, sawNode bool
	var extMode os.FileMode
	for _, f := range manifest.Files {
		if f.Agent != localAgent {
			continue
		}
		if strings.HasSuffix(f.Path, "/.git/config") {
			sawGit = true
		}
		if strings.Contains(f.Path, "node_modules") {
			sawNode = true
		}
		if strings.HasSuffix(f.Path, "main.go") {
			extMode = f.perm()
		}
	}
	if !sawGit || sawNode || extMode != 0o755 {
		t.Fatalf("capture filter wrong: sawGit=%v sawNode=%v extMode=%v", sawGit, sawNode, extMode)
	}

	roundTripped, err := Read(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	targetCfg := config.Resolved{Config: config.Config{DSHHome: targetDSH, BackupDir: filepath.Join(root, "backups")}}
	if _, err := Apply(targetCfg, "", snapshot, roundTripped); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		filepath.Join(targetDSH, "external", "dsh-alpha", "index.js"):       "alpha",
		filepath.Join(targetDSH, "external", "dsh-alpha", ".git", "config"): "[remote \"origin\"]\nurl = x\n",
		filepath.Join(root, "outside", "dsh-ext", "main.go"):                "ext",
		filepath.Join(root, "third", "scoped", "lib.js"):                    "scoped",
	} {
		data, err := os.ReadFile(path)
		if err != nil || string(data) != want {
			t.Fatalf("unpack %s: %v (%q)", path, err, data)
		}
	}
	if _, err := os.Stat(filepath.Join(targetDSH, "external", "dsh-alpha", "node_modules")); err == nil {
		t.Fatal("node_modules must not be unpacked")
	}
	info, err := os.Stat(filepath.Join(root, "outside", "dsh-ext", "main.go"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("exec bit lost: %v %v", info.Mode(), err)
	}
}

func TestReadVersionGate(t *testing.T) {
	dir := t.TempDir()
	file := File{Agent: "dsh", Profile: "web", Path: "package.json", Size: 2, SHA256: "a11f2d62ed73149e5fb049129e0d3a3c9cbe73bd923a6c2adaba1c731b3c786e"}
	data, _ := json.Marshal(Manifest{Version: snapshotVersion + 1, Files: []File{file}})
	if err := os.WriteFile(filepath.Join(dir, manifestName), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(dir); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unsupported snapshot version %d", snapshotVersion+1)) {
		t.Fatalf("expected version gate error, got %v", err)
	}
}

// TestCreateCoversAllProfiles checks that one snapshot carries every profile
// without staging collisions: two profiles' package.json must survive as
// distinct files both on disk and through Read.
func TestCreateCoversAllProfiles(t *testing.T) {
	root := t.TempDir()
	dsh := filepath.Join(root, "dsh")
	pi := filepath.Join(root, "pi")
	writeFile(t, filepath.Join(dsh, "profiles", "web", "package.json"), `{"dependencies":{"alpha":"^1.0.0"}}`)
	writeFile(t, filepath.Join(dsh, "profiles", "lab", "package.json"), `{"dependencies":{"beta":"^2.0.0"}}`)
	writeFile(t, filepath.Join(dsh, "profiles", "lab", "cordis.yml"), "labs: true\n")
	writeFile(t, filepath.Join(pi, "settings.json"), `{"packages":[]}`)
	cfg := config.Resolved{Config: config.Config{DSHHome: dsh, PiHome: pi, BackupDir: filepath.Join(root, "backups")}}
	dir, manifest, err := Create(cfg, "all")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != snapshotVersion {
		t.Fatalf("expected version %d, got %d", snapshotVersion, manifest.Version)
	}
	perProfile := map[string]string{} // profile -> staged package.json content
	profiles := map[string]bool{}
	for _, f := range manifest.Files {
		if f.Agent != "dsh" {
			continue
		}
		if f.Profile == "" {
			t.Fatalf("dsh file %s has no profile", f.Path)
		}
		profiles[f.Profile] = true
		if f.Path == "package.json" {
			data, err := os.ReadFile(snapshotStagingPath(dir, f))
			if err != nil {
				t.Fatal(err)
			}
			perProfile[f.Profile] = string(data)
		}
	}
	if !profiles["web"] || !profiles["lab"] {
		t.Fatalf("profiles captured = %#v", profiles)
	}
	if perProfile["web"] == perProfile["lab"] {
		t.Fatal("both profiles' package.json collapsed into one staged file")
	}
	roundTripped, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(roundTripped.Files) != len(manifest.Files) {
		t.Fatalf("round trip file count = %d, want %d", len(roundTripped.Files), len(manifest.Files))
	}
	// Applying routes each file to its own profile directory.
	target := config.Resolved{Config: config.Config{DSHHome: filepath.Join(root, "target-dsh"), PiHome: filepath.Join(root, "target-pi"), BackupDir: filepath.Join(root, "tb")}}
	changes, err := Plan(target, "", roundTripped)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Agent == "dsh" && !strings.Contains(filepath.ToSlash(change.Target), "/profiles/"+change.Profile+"/") {
			t.Fatalf("%s target %s does not include profile %s", change.Path, change.Target, change.Profile)
		}
	}
	if _, err := Apply(target, "", dir, roundTripped); err != nil {
		t.Fatal(err)
	}
	assertFileContains(t, filepath.Join(root, "target-dsh", "profiles", "web", "package.json"), `"alpha"`)
	assertFileContains(t, filepath.Join(root, "target-dsh", "profiles", "lab", "package.json"), `"beta"`)
	assertFileContains(t, filepath.Join(root, "target-dsh", "profiles", "lab", "cordis.yml"), "labs: true")
}

// TestLegacySnapshotProfileFallback checks that v1/v2 snapshots (files without
// a Profile) still apply: the recorded DSHProfile wins when profile resolution
// has nothing to work with.
func TestLegacySnapshotProfileFallback(t *testing.T) {
	root := t.TempDir()
	sourceDSH := filepath.Join(root, "src-dsh")
	writeFile(t, filepath.Join(sourceDSH, "profiles", "web", "package.json"), `{"dependencies":{"alpha":"^1.0.0"}}`)

	// Build a v3 snapshot on the source, then downgrade its manifest to v1
	// layout to emulate an old device.
	dir, _, err := Create(config.Resolved{Config: config.Config{DSHHome: sourceDSH, BackupDir: filepath.Join(root, "b")}}, "dsh")
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Version    int    `json:"version"`
		CreatedAt  string `json:"createdAt"`
		DSHProfile string `json:"dshProfile,omitempty"`
		Files      []struct {
			Agent  string `json:"agent"`
			Path   string `json:"path"`
			Size   int64  `json:"size"`
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	data, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Version = 1
	manifest.DSHProfile = "web"
	for _, f := range manifest.Files {
		if f.Agent == "dsh" {
			// Move staged file back to the flat v1 layout.
			if _, err := os.Stat(filepath.Join(dir, f.Agent, f.Path)); errors.Is(err, os.ErrNotExist) {
				if err := os.Rename(filepath.Join(dir, f.Agent, "web", f.Path), filepath.Join(dir, f.Agent, f.Path)); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	legacy, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, manifestName), legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	if old.DSHProfile != "web" {
		t.Fatalf("dshProfile = %q", old.DSHProfile)
	}
	// Target with no profiles at all: resolution must fall back to DSHProfile.
	target := config.Resolved{Config: config.Config{DSHHome: filepath.Join(root, "dst-dsh"), BackupDir: filepath.Join(root, "tb")}}
	if _, err := Apply(target, "", dir, old); err != nil {
		t.Fatal(err)
	}
	assertFileContains(t, filepath.Join(root, "dst-dsh", "profiles", "web", "package.json"), `"alpha"`)
}
