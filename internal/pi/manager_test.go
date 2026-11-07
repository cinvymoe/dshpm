package pi

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cinvymoe/dshpm/internal/config"
	"github.com/cinvymoe/dshpm/internal/model"
)

func writeJSONFile(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) (*Manager, string) {
	t.Helper()
	home := t.TempDir()
	settings := map[string]any{
		"packages": []any{
			"npm:alpha",
			map[string]any{"source": "npm:@scope/beta", "extensions": []string{"beta"}},
			"file:localpkg",
			"git:github.com/acme/gitpkg",
		},
	}
	writeJSONFile(t, filepath.Join(home, "settings.json"), settings)
	writeJSONFile(t, filepath.Join(home, "npm", "node_modules", "alpha", "package.json"), map[string]any{"name": "alpha", "version": "1.0.0"})
	writeJSONFile(t, filepath.Join(home, "npm", "node_modules", "@scope", "beta", "package.json"), map[string]any{"name": "@scope/beta", "version": "2.0.0"})
	writeJSONFile(t, filepath.Join(home, "localpkg", "package.json"), map[string]any{"name": "localpkg", "version": "0.1.0"})
	writeJSONFile(t, filepath.Join(home, "npm", "node_modules", "gitpkg", "package.json"), map[string]any{"name": "gitpkg", "version": "0.2.0"})
	writeJSONFile(t, filepath.Join(home, "npm", "package.json"), map[string]any{"name": "pi-agent", "dependencies": map[string]string{"alpha": "1.0.0"}})
	cfg := config.Resolved{Config: config.Config{
		PiHome:      home,
		BackupDir:   filepath.Join(home, "backups"),
		NPMRegistry: "http://127.0.0.1:1",
		Pi:          "pi",
	}}
	manager, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	manager.Offline = true
	return manager, home
}

func TestListReadsPiSettingsAndPackageMetadata(t *testing.T) {
	manager, _ := newFixture(t)
	plugins, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 4 {
		t.Fatalf("got %d plugins, want 4", len(plugins))
	}
	byName := make(map[string]model.Plugin)
	for _, plugin := range plugins {
		byName[plugin.Name] = plugin
	}
	if byName["alpha"].Kind != model.SourceRegistry || byName["alpha"].InstalledVersion != "1.0.0" {
		t.Fatalf("alpha: %+v", byName["alpha"])
	}
	if byName["@scope/beta"].Kind != model.SourceRegistry || byName["@scope/beta"].InstalledVersion != "2.0.0" {
		t.Fatalf("beta: %+v", byName["@scope/beta"])
	}
	if byName["localpkg"].Kind != model.SourceLocal {
		t.Fatalf("localpkg: %+v", byName["localpkg"])
	}
	if byName["gitpkg"].Kind != model.SourceGit || byName["gitpkg"].GitURL != "https://github.com/acme/gitpkg" {
		t.Fatalf("gitpkg: %+v", byName["gitpkg"])
	}
}

func TestUpgradeInvokesPiAndReportsUpdated(t *testing.T) {
	manager, home := newFixture(t)
	bin := filepath.Join(home, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(home, "pi.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$PI_LOG\"\nexit 0\n"
	piPath := filepath.Join(bin, "pi")
	if err := os.WriteFile(piPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_LOG", logPath)
	manager.Config.Pi = piPath
	results, err := manager.Upgrade(context.Background(), UpgradeOptions{Names: []string{"alpha"}, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "updated" {
		t.Fatalf("results: %+v", results)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "update npm:alpha") {
		t.Fatalf("pi log: %s", data)
	}
}

func TestBackupIncludesSettingsAndPackageManifest(t *testing.T) {
	manager, _ := newFixture(t)
	manifest, dir, err := manager.Backup(BackupOptions{BackupDir: filepath.Join(t.TempDir(), "backups"), ToolVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Profile != "pi" || len(manifest.Files) < 2 {
		t.Fatalf("manifest: %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(dir, "profile", "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "profile", "npm", "package.json")); err != nil {
		t.Fatal(err)
	}
}
