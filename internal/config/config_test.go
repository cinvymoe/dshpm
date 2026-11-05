package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveProfileNamePrefersWeb(t *testing.T) {
	home := t.TempDir()
	profiles := filepath.Join(home, ".dsh", "profiles")
	for _, name := range []string{"acp", "web", "headless"} {
		dir := filepath.Join(profiles, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := Resolved{Config: Config{DSHHome: filepath.Join(home, ".dsh")}}
	got, err := cfg.ResolveProfileName("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "web" {
		t.Fatalf("profile = %q, want web", got)
	}
}

func TestResolveProfileNameExplicitWins(t *testing.T) {
	cfg := Resolved{Config: Config{DSHHome: t.TempDir()}}
	got, err := cfg.ResolveProfileName("custom")
	if err != nil {
		t.Fatal(err)
	}
	if got != "custom" {
		t.Fatalf("profile = %q, want custom", got)
	}
}

func TestLoadDefaultsWorktreeRootToDataProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := LoadFile(filepath.Join(home, "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorktreeRoot != "/data/project/dshpm-worktrees" {
		t.Fatalf("worktree root = %q, want /data/project/dshpm-worktrees", cfg.WorktreeRoot)
	}
}

func TestManagedDefaultsAndSetManaged(t *testing.T) {
	cfg := Resolved{Config: Config{Unmanaged: []string{"demo"}}}
	if cfg.IsManaged("demo") {
		t.Fatal("demo should be unmanaged")
	}
	if !cfg.IsManaged("other") {
		t.Fatal("other should be managed")
	}
	cfg.SetManaged("demo", true)
	if !cfg.IsManaged("demo") {
		t.Fatal("demo should be managed after SetManaged")
	}
	cfg.SetManaged("other", false)
	if cfg.IsManaged("other") {
		t.Fatal("other should be unmanaged after SetManaged")
	}
}

func TestSaveWritesUnmanagedList(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Resolved{Config: Config{Unmanaged: []string{"demo"}}, ConfigPath: path}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.IsManaged("demo") {
		t.Fatal("saved config should preserve unmanaged demo")
	}
}
