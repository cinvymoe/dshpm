package manager

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cinvymoe/dshpm/internal/config"
	"github.com/cinvymoe/dshpm/internal/model"
	"github.com/cinvymoe/dshpm/internal/remote"
)

func writeProfile(t *testing.T, home, name, packageJSON string) {
	t.Helper()
	dir := filepath.Join(home, "profiles", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(packageJSON), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestEnrichRegistry(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, home, "web", `{"dependencies":{"demo":"1.0.0"}}`)
	dir := filepath.Join(home, "profiles", "web", "node_modules", "demo")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"demo","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"dist-tags":{"latest":"1.2.0"}}`))
	}))
	defer server.Close()
	cfg := config.Resolved{Config: config.Config{DSHHome: home, NPMRegistry: server.URL, PNPM: "pnpm", Git: "git"}}
	instance, err := New(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	instance.Registry = remote.NewRegistry(server.URL)
	plugins := instance.Profile.Plugins()
	if len(plugins) != 1 {
		t.Fatalf("plugins = %d", len(plugins))
	}
	instance.Enrich(&plugins[0])
	if plugins[0].Status != "update" || plugins[0].RemoteVersion != "1.2.0" {
		t.Fatalf("plugin = %+v", plugins[0])
	}
}

func TestUpdateDryRunLocal(t *testing.T) {
	home := t.TempDir()
	upstream := filepath.Join(home, "upstream")
	clone := filepath.Join(home, "external", "demo")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "init", "-b", "main")
	run(t, upstream, "config", "user.email", "test@example.com")
	run(t, upstream, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(upstream, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "-m", "initial")
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, filepath.Dir(clone), "clone", upstream, clone)
	run(t, clone, "config", "user.email", "test@example.com")
	run(t, clone, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(upstream, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "-m", "second")
	writeProfile(t, home, "web", `{"dependencies":{"demo":"link:`+clone+`"}}`)
	cfg := config.Resolved{Config: config.Config{DSHHome: home, BackupDir: filepath.Join(home, "backups"), PNPM: "pnpm", Git: "git"}}
	instance, err := New(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	results, err := instance.Update(context.Background(), UpdateOptions{Names: []string{"demo"}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "would-update" {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Kind != model.SourceLocal {
		t.Fatalf("kind = %q", results[0].Kind)
	}
}

func run(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func TestUpdateLocalConflictRestoresChanges(t *testing.T) {
	home := t.TempDir()
	upstream := filepath.Join(home, "upstream")
	clone := filepath.Join(home, "external", "demo")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "init", "-b", "main")
	run(t, upstream, "config", "user.email", "test@example.com")
	run(t, upstream, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(upstream, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "-m", "initial")
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, filepath.Dir(clone), "clone", upstream, clone)
	run(t, clone, "config", "user.email", "test@example.com")
	run(t, clone, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upstream, "a.txt"), []byte("upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "commit", "-am", "upstream change")
	writeProfile(t, home, "web", `{"dependencies":{"demo":"link:`+clone+`"}}`)
	cfg := config.Resolved{Config: config.Config{DSHHome: home, BackupDir: filepath.Join(home, "backups"), PNPM: "pnpm", Git: "git"}}
	instance, err := New(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	results, err := instance.Update(context.Background(), UpdateOptions{Names: []string{"demo"}, NoBackup: true, NoBuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "conflict" {
		t.Fatalf("results = %+v", results)
	}
	content, err := os.ReadFile(filepath.Join(clone, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "local\n" {
		t.Fatalf("content = %q", content)
	}
}

func TestUpdateLocalReappliesPatch(t *testing.T) {
	home := t.TempDir()
	upstream := filepath.Join(home, "upstream")
	clone := filepath.Join(home, "external", "demo")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "init", "-b", "main")
	run(t, upstream, "config", "user.email", "test@example.com")
	run(t, upstream, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(upstream, "a.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "-m", "initial")
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, filepath.Dir(clone), "clone", upstream, clone)
	run(t, clone, "config", "user.email", "test@example.com")
	run(t, clone, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(clone, "a.txt"), []byte("local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(upstream, "b.txt"), []byte("upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "-m", "upstream change")
	writeProfile(t, home, "web", `{"dependencies":{"demo":"link:`+clone+`"}}`)
	cfg := config.Resolved{Config: config.Config{DSHHome: home, BackupDir: filepath.Join(home, "backups"), PNPM: "pnpm", Git: "git"}}
	instance, err := New(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	results, err := instance.Update(context.Background(), UpdateOptions{Names: []string{"demo"}, NoBackup: true, NoBuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "updated" {
		t.Fatalf("results = %+v", results)
	}
	content, err := os.ReadFile(filepath.Join(clone, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "local\n" {
		t.Fatalf("content = %q", content)
	}
	if _, err := os.Stat(filepath.Join(clone, "b.txt")); err != nil {
		t.Fatal(err)
	}
}
