package remotesync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cinvymoe/dshpm/internal/config"
)

func testConfig(t *testing.T) config.Resolved {
	t.Helper()
	home := t.TempDir()
	dsh := filepath.Join(home, "dsh")
	pi := filepath.Join(home, "pi")
	for _, name := range []string{"web", "lab"} {
		if err := os.MkdirAll(filepath.Join(dsh, "profiles", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(pi, "npm"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dsh, "profiles", "web", "package.json"), `{"dependencies":{"remote":"1.2.3"}}`)
	writeFile(t, filepath.Join(dsh, "profiles", "web", "cordis.yml"), "web: true\n")
	writeFile(t, filepath.Join(dsh, "profiles", "lab", "package.json"), `{"dependencies":{"labplugin":"^1.0.0"}}`)
	writeFile(t, filepath.Join(pi, "settings.json"), `{"packages":["npm:alpha"]}`)
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func entryKey(entry ConfigFile) string {
	return entry.Agent + "/" + entry.Profile + "/" + entry.Path
}

func TestValidateRepo(t *testing.T) {
	if err := ValidateRepo("owner/repo"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRepo("owner"); err == nil {
		t.Fatal("expected invalid repo error")
	}
}

func TestCollectConfigSourcesCoversAllProfiles(t *testing.T) {
	cfg := testConfig(t)
	sources, err := collectConfigSources(cfg, "all")
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]bool{}
	for _, source := range sources {
		keys[entryKey(source.entry)] = true
	}
	for _, want := range []string{"dsh/web/package.json", "dsh/web/cordis.yml", "dsh/lab/package.json", "pi//settings.json"} {
		if !keys[want] {
			t.Fatalf("missing %s in %#v", want, keys)
		}
	}
	if keys["pi/npm/package.json"] {
		t.Fatal("absent Pi file must not be collected")
	}
	dshOnly, err := collectConfigSources(cfg, "dsh")
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range dshOnly {
		if source.entry.Agent != "dsh" {
			t.Fatalf("agent filter leaked %s", entryKey(source.entry))
		}
	}
}

func TestDryRunListsFilesWithHashes(t *testing.T) {
	cfg := testConfig(t)
	entries, err := DryRun(cfg, "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Fatal("no entries")
	}
	for _, entry := range entries {
		if len(entry.SHA256) != 64 {
			t.Fatalf("entry %s has no hash", entryKey(entry))
		}
		if entry.Size == 0 {
			t.Fatalf("entry %s has no size", entryKey(entry))
		}
	}
}

// fakeRepo builds a repository clone subtree with the given tracked files.
func fakeRepo(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		writeFile(t, filepath.Join(dir, filepath.FromSlash(rel)), content)
	}
}

func TestPlanAndApplyRestoreConfig(t *testing.T) {
	cfg := testConfig(t)
	repo := t.TempDir()
	fakeRepo(t, repo, map[string]string{
		// differs from local -> update
		"profiles/web/package.json": `{"dependencies":{"remote":"2.0.0"}}`,
		// identical to local -> same
		"profiles/lab/package.json": `{"dependencies":{"labplugin":"^1.0.0"}}`,
		// missing locally -> add (also creates the profile directory)
		"profiles/newprofile/package.json": `{"dependencies":{}}`,
		"pi/settings.json":                 `{"packages":["npm:alpha","npm:beta"]}`,
	})
	files, err := readBackupFiles(repo, "all")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, entry := range files {
		got[entryKey(entry)] = entry.Agent
	}
	for _, want := range []string{"dsh/web/package.json", "dsh/lab/package.json", "dsh/newprofile/package.json", "pi//settings.json"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("missing %s in %#v", want, got)
		}
	}
	changes, err := Plan(cfg, "all", files)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, change := range changes {
		status[entryKey(change.ConfigFile)] = change.Status
	}
	if status["dsh/web/package.json"] != "update" {
		t.Fatalf("web package.json = %s", status["dsh/web/package.json"])
	}
	if status["dsh/lab/package.json"] != "same" {
		t.Fatalf("lab package.json = %s", status["dsh/lab/package.json"])
	}
	if status["dsh/newprofile/package.json"] != "add" {
		t.Fatalf("newprofile = %s", status["dsh/newprofile/package.json"])
	}
	if status["pi//settings.json"] != "update" {
		t.Fatalf("pi settings.json = %s", status["pi/settings.json"])
	}

	// Local-only file must survive the restore untouched.
	extra := filepath.Join(cfg.DSHHome, "profiles", "web", "pnpm-lock.yaml")
	writeFile(t, extra, "lockfile-v9\n")

	backupDir, err := applyConfigChanges(repo, cfg, changes)
	if err != nil {
		t.Fatal(err)
	}
	assertContains(t, filepath.Join(cfg.DSHHome, "profiles", "web", "package.json"), `"remote":"2.0.0"`)
	assertContains(t, filepath.Join(cfg.DSHHome, "profiles", "lab", "package.json"), `"labplugin":"^1.0.0"`)
	assertContains(t, filepath.Join(cfg.DSHHome, "profiles", "newprofile", "package.json"), `{"dependencies":{}}`)
	assertContains(t, filepath.Join(cfg.PiHome, "settings.json"), "npm:beta")
	if data, err := os.ReadFile(extra); err != nil || string(data) != "lockfile-v9\n" {
		t.Fatalf("local-only file disturbed: %q %v", data, err)
	}
	old, err := os.ReadFile(filepath.Join(backupDir, "profiles", "web", "package.json"))
	if err != nil || string(old) != `{"dependencies":{"remote":"1.2.3"}}` {
		t.Fatalf("backup of replaced file = %q %v", old, err)
	}
	oldLab, err := os.ReadFile(filepath.Join(backupDir, "profiles", "lab", "package.json"))
	if err == nil {
		t.Fatalf("unchanged (same) file must not be backed up, got %q", oldLab)
	}
}

func TestPlanIgnoresEmptyRepository(t *testing.T) {
	cfg := testConfig(t)
	files, err := readBackupFiles(t.TempDir(), "all")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("files = %#v", files)
	}
	changes, err := Plan(cfg, "all", files)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("changes = %#v", changes)
	}
}

func assertContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s = %q, want contains %q", path, string(data), want)
	}
}
