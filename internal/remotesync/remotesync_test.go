package remotesync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/cinvymoe/dshpm/internal/config"
)

func testConfig(t *testing.T) config.Resolved {
	t.Helper()
	home := t.TempDir()
	dsh := filepath.Join(home, "dsh")
	pi := filepath.Join(home, "pi")
	profileDir := filepath.Join(dsh, "profiles", "web")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pi, "npm"), 0o755); err != nil {
		t.Fatal(err)
	}
	packageJSON := map[string]any{
		"dependencies": map[string]string{
			"remote":    "1.2.3",
			"gitplugin": "github:owner/repo",
			"local":     "link:" + filepath.Join(home, "local"),
			"unmanaged": "9.9.9",
		},
		"dsh": map[string]any{
			"profile": map[string]any{"bundles": []string{"remote", "local", "unmanaged", "@deepseek-ai/dsh-base"}},
		},
	}
	writeJSON(t, filepath.Join(profileDir, "package.json"), packageJSON)
	writeJSON(t, filepath.Join(pi, "settings.json"), map[string]any{
		"packages": []any{
			"npm:alpha",
			map[string]any{"source": "npm:beta", "extensions": []string{}},
			"file:" + filepath.Join(home, "local-pi"),
			"npm:unmanaged-pi",
		},
	})
	return config.Resolved{
		Config: config.Config{
			DSHHome:          dsh,
			PiHome:           pi,
			BackupDir:        filepath.Join(home, "backups"),
			Unmanaged:        []string{"unmanaged"},
			UnmanagedByAgent: map[string][]string{"pi": {"unmanaged-pi"}},
		},
		ConfigPath: filepath.Join(home, "config.json"),
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBuildExcludesLocalAndUnmanaged(t *testing.T) {
	cfg := testConfig(t)
	state, err := Build(cfg, "", "all")
	if err != nil {
		t.Fatal(err)
	}
	if state.DSH == nil || state.Pi == nil {
		t.Fatalf("state = %#v", state)
	}
	if len(state.DSH.Plugins) != 2 {
		t.Fatalf("DSH plugins = %#v", state.DSH.Plugins)
	}
	if state.DSH.Plugins[0].Name != "gitplugin" || state.DSH.Plugins[0].Kind != "git" {
		t.Fatalf("DSH plugins = %#v", state.DSH.Plugins)
	}
	for _, bundle := range state.DSH.Bundles {
		if bundle == "local" || bundle == "unmanaged" {
			t.Fatalf("local bundle leaked into state: %#v", state.DSH.Bundles)
		}
	}
	if len(state.Pi.Packages) != 2 {
		t.Fatalf("Pi packages = %#v", state.Pi.Packages)
	}
}

func TestApplyPreservesLocalEntries(t *testing.T) {
	cfg := testConfig(t)
	state, err := Build(cfg, "", "all")
	if err != nil {
		t.Fatal(err)
	}
	for i := range state.DSH.Plugins {
		if state.DSH.Plugins[i].Name == "remote" {
			state.DSH.Plugins[i].Spec = "2.0.0"
		}
	}
	state.DSH.Bundles = []string{"remote", "@deepseek-ai/dsh-base", "new-bundle"}
	backup, err := Apply(cfg, state, "web")
	if err != nil {
		t.Fatal(err)
	}
	if backup == "" {
		t.Fatal("empty backup path")
	}
	var pkg map[string]any
	readJSON(t, filepath.Join(cfg.DSHHome, "profiles", "web", "package.json"), &pkg)
	deps := pkg["dependencies"].(map[string]any)
	if deps["remote"] != "2.0.0" {
		t.Fatalf("remote = %#v", deps["remote"])
	}
	if deps["local"] == nil || deps["unmanaged"] == nil {
		t.Fatalf("local dependency was removed: %#v", deps)
	}
	dsh := pkg["dsh"].(map[string]any)
	bundles := dsh["profile"].(map[string]any)["bundles"].([]any)
	foundLocal, foundNew := false, false
	for _, item := range bundles {
		if item == "local" {
			foundLocal = true
		}
		if item == "new-bundle" {
			foundNew = true
		}
	}
	if !foundLocal || !foundNew {
		t.Fatalf("bundles = %#v", bundles)
	}
	var settings map[string]any
	readJSON(t, filepath.Join(cfg.PiHome, "settings.json"), &settings)
	packages := settings["packages"].([]any)
	foundLocalPi := false
	for _, item := range packages {
		if source, ok := item.(string); ok && len(source) > 5 && source[:5] == "file:" {
			foundLocalPi = true
		}
	}
	if !foundLocalPi {
		t.Fatalf("local Pi package was removed: %#v", packages)
	}
}

func TestStatusMarksLocalAndRemote(t *testing.T) {
	cfg := testConfig(t)
	rows, err := Status(cfg, "", "all")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, row := range rows {
		got[row.Agent+"/"+row.Name] = row.Sync
	}
	if got["dsh/remote"] != "remote" || got["dsh/local"] != "local" || got["pi/alpha"] != "remote" {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestValidateRepo(t *testing.T) {
	if err := ValidateRepo("owner/repo"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRepo("owner"); err == nil {
		t.Fatal("expected invalid repo error")
	}
}

func readJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}
