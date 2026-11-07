package devicesync

import (
	"context"
	"os"
	"path/filepath"
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
	snapshot, manifest, err := Create(sourceCfg, "web", "all")
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
	changes, err := Plan(targetCfg, "web", pulledManifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Status != "update" {
			t.Fatalf("%s/%s status = %q, want update", change.Agent, change.Path, change.Status)
		}
	}
	backupDir, err := Apply(targetCfg, "web", pulled, pulledManifest)
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
