package backup

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cinvymoe/dshpm/internal/profile"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func TestCreateAndRestore(t *testing.T) {
	home := t.TempDir()
	pluginDir := filepath.Join(home, "external", "demo")
	profileDir := filepath.Join(home, "profiles", "web")
	if err := os.MkdirAll(pluginDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, pluginDir, "init", "-b", "main")
	runGit(t, pluginDir, "config", "user.email", "test@example.com")
	runGit(t, pluginDir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.txt"), []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, pluginDir, "add", ".")
	runGit(t, pluginDir, "commit", "-m", "initial")
	if err := os.WriteFile(filepath.Join(profileDir, "package.json"), []byte(`{"dependencies":{"demo":"link:../../external/demo"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "cordis.patch.yml"), []byte("patch: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := profile.Load(home, "web")
	if err != nil {
		t.Fatal(err)
	}
	manifest, dir, err := Create(CreateOptions{Profile: loaded, BackupDir: filepath.Join(home, "backups"), ToolVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Plugins) != 1 || manifest.Plugins[0].PatchFile == "" {
		t.Fatalf("manifest = %+v", manifest)
	}
	// Revert, then restore with --force.
	runGit(t, pluginDir, "checkout", "--", "plugin.txt")
	result, err := Restore(RestoreOptions{ArchiveDir: dir, Apply: true, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Conflicts) != 0 {
		t.Fatalf("conflicts: %v", result.Conflicts)
	}
	content, err := os.ReadFile(filepath.Join(pluginDir, "plugin.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "changed\n" {
		t.Fatalf("content = %q", content)
	}
	if _, err := os.Stat(filepath.Join(profileDir, "cordis.patch.yml")); err != nil {
		t.Fatal(err)
	}
}

func TestCreateAndRestoreNonGitLocalFiles(t *testing.T) {
	home := t.TempDir()
	pluginDir := filepath.Join(home, "external", "plain")
	profileDir := filepath.Join(home, "profiles", "web")
	if err := os.MkdirAll(filepath.Join(pluginDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(pluginDir, "node_modules", "dep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "package.json"), []byte(`{"name":"plain","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "src", "index.js"), []byte("export default 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "node_modules", "dep", "index.js"), []byte("dep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "package.json"), []byte(`{"dependencies":{"plain":"link:../../external/plain"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := profile.Load(home, "web")
	if err != nil {
		t.Fatal(err)
	}
	manifest, archive, err := Create(CreateOptions{Profile: loaded, BackupDir: filepath.Join(home, "backups"), ToolVersion: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Plugins) != 1 || manifest.Plugins[0].FilesDir == "" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if _, err := os.Stat(filepath.Join(archive, filepath.FromSlash(manifest.Plugins[0].FilesDir), "src", "index.js")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(archive, filepath.FromSlash(manifest.Plugins[0].FilesDir), "node_modules")); err == nil {
		t.Fatal("node_modules should not be copied")
	}
	if err := os.RemoveAll(pluginDir); err != nil {
		t.Fatal(err)
	}
	result, err := Restore(RestoreOptions{ArchiveDir: archive, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RestoredCheckouts) != 1 {
		t.Fatalf("result = %+v", result)
	}
	content, err := os.ReadFile(filepath.Join(pluginDir, "src", "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "export default 1\n" {
		t.Fatalf("content = %q", content)
	}
}
