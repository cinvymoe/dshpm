package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cinvymoe/dshpm/internal/config"
)

func TestSwitchToGitRewritesDependencyAndInstalls(t *testing.T) {
	home := t.TempDir()
	local := filepath.Join(home, "external", "demo")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "package.json"), []byte(`{"name":"@demo/plugin"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, home, "web", `{"dependencies":{"@demo/plugin":"link:`+local+`"}}`)
	logPath := filepath.Join(home, "pnpm.log")
	pnpm := filepath.Join(home, "pnpm")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + logPath + "\n"
	if err := os.WriteFile(pnpm, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Resolved{Config: config.Config{DSHHome: home, PNPM: pnpm, Git: "git"}}
	instance, err := New(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.SwitchToGit(context.Background(), "@demo/plugin", "owner/repo"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "profiles", "web", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"github:owner/repo"`) {
		t.Fatalf("package.json = %s", data)
	}
	logged, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(logged)) != "install" {
		t.Fatalf("pnpm args = %q", logged)
	}
}

func TestSwitchToGitRestoresSpecWhenInstallFails(t *testing.T) {
	home := t.TempDir()
	local := filepath.Join(home, "external", "demo")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "package.json"), []byte(`{"name":"demo"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, home, "web", `{"dependencies":{"demo":"link:`+local+`"}}`)
	lockPath := filepath.Join(home, "profiles", "web", "pnpm-lock.yaml")
	if err := os.WriteFile(lockPath, []byte("old-lock\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pnpm := filepath.Join(home, "pnpm")
	script := "#!/bin/sh\nprintf 'changed-lock\\n' > pnpm-lock.yaml\nexit 1\n"
	if err := os.WriteFile(pnpm, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Resolved{Config: config.Config{DSHHome: home, PNPM: pnpm, Git: "git"}}
	instance, err := New(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.SwitchToGit(context.Background(), "demo", "owner/repo"); err == nil {
		t.Fatal("expected install failure")
	}
	data, err := os.ReadFile(filepath.Join(home, "profiles", "web", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"link:`+local+`"`) {
		t.Fatalf("spec was not restored: %s", data)
	}
	lock, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(lock) != "old-lock\n" {
		t.Fatalf("lockfile was not restored: %q", lock)
	}
}
