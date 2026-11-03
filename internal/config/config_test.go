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
