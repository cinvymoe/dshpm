package manager

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cinvymoe/dshpm/internal/config"
	"github.com/cinvymoe/dshpm/internal/model"
	"github.com/cinvymoe/dshpm/internal/profile"
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

// fakePNPM returns a pnpm stand-in. In "ok" mode it moves pnpm-lock.yaml to
// $FAKE_HEAD on `pnpm update demo`; in "stale" mode it exits 0 without
// touching the lockfile, reproducing the no-op that made updates lie.
func fakePNPM(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pnpm")
	script := `#!/bin/sh
if [ "$1" = "update" ] && [ "$FAKE_PNPM_MODE" = "ok" ]; then
  cat > pnpm-lock.yaml <<EOF
importers:
  .:
    dependencies:
      demo:
        specifier: git+https://example.com/demo.git
        version: git+https://example.com/demo.git#$FAKE_HEAD
EOF
fi
exit 0
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func gitHead(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git rev-parse HEAD: %v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func setupGitUpstream(t *testing.T, home string) (string, string) {
	t.Helper()
	seed := filepath.Join(home, "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, seed, "init", "-b", "main")
	run(t, seed, "config", "user.email", "test@example.com")
	run(t, seed, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, seed, "add", ".")
	run(t, seed, "commit", "-m", "initial")
	head := gitHead(t, seed)
	bare := filepath.Join(home, "upstream.git")
	run(t, home, "clone", "--bare", seed, bare)
	return bare, head
}

func TestApplyGitUpdateVerifiesLockfile(t *testing.T) {
	home := t.TempDir()
	bare, head := setupGitUpstream(t, home)
	t.Setenv("FAKE_PNPM_MODE", "ok")
	t.Setenv("FAKE_HEAD", head)
	profileDir := filepath.Join(home, "profiles", "web")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		Config:  config.Resolved{Config: config.Config{DSHHome: home, PNPM: fakePNPM(t), Git: "git"}},
		Profile: &profile.Profile{Name: "web", Dir: profileDir},
		Out:     io.Discard,
	}
	plugin := model.Plugin{Name: "demo", Kind: model.SourceGit, GitURL: bare, Spec: "git+https://example.com/demo.git"}
	result := m.applyOne(context.Background(), plugin, UpdateOptions{}, "")
	if result.Status != "updated" {
		t.Fatalf("result = %+v", result)
	}
	if !strings.HasSuffix(result.To, "@"+head[:7]) {
		t.Fatalf("to = %q, want commit %s", result.To, head[:7])
	}
}

func TestApplyGitUpdateRejectsStaleLockfile(t *testing.T) {
	home := t.TempDir()
	bare, _ := setupGitUpstream(t, home)
	t.Setenv("FAKE_PNPM_MODE", "stale")
	profileDir := filepath.Join(home, "profiles", "web")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := strings.Repeat("a", 40)
	lockfile := "importers:\n  .:\n    dependencies:\n      demo:\n        specifier: git+https://example.com/demo.git\n        version: git+https://example.com/demo.git#" + stale + "\n"
	if err := os.WriteFile(filepath.Join(profileDir, "pnpm-lock.yaml"), []byte(lockfile), 0o644); err != nil {
		t.Fatal(err)
	}
	m := &Manager{
		Config:  config.Resolved{Config: config.Config{DSHHome: home, PNPM: fakePNPM(t), Git: "git"}},
		Profile: &profile.Profile{Name: "web", Dir: profileDir},
		Out:     io.Discard,
	}
	plugin := model.Plugin{Name: "demo", Kind: model.SourceGit, GitURL: bare, Spec: "git+https://example.com/demo.git"}
	result := m.applyOne(context.Background(), plugin, UpdateOptions{}, "")
	if result.Status != "failed" || !strings.Contains(result.Message, "lockfile") {
		t.Fatalf("result = %+v", result)
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

func TestListMarksUnmanagedPlugin(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, home, "web", `{"dependencies":{"demo":"1.0.0"}}`)
	cfg := config.Resolved{Config: config.Config{DSHHome: home, Unmanaged: []string{"demo"}, PNPM: "pnpm", Git: "git"}}
	instance, err := New(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	instance.Offline = true
	plugins, err := instance.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 1 || plugins[0].Managed {
		t.Fatalf("plugins = %+v", plugins)
	}
}

func TestUpdateSkipsUnmanagedPlugin(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, home, "web", `{"dependencies":{"demo":"1.0.0"}}`)
	cfg := config.Resolved{Config: config.Config{DSHHome: home, Unmanaged: []string{"demo"}, PNPM: "pnpm", Git: "git"}}
	instance, err := New(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	results, err := instance.Update(context.Background(), UpdateOptions{Names: []string{"demo"}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "skipped" {
		t.Fatalf("results = %+v", results)
	}
}

func TestUpgradeSkipsUnmanagedPlugin(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, home, "web", `{"dependencies":{"demo":"1.0.0"}}`)
	cfg := config.Resolved{Config: config.Config{DSHHome: home, Unmanaged: []string{"demo"}, PNPM: "pnpm", Git: "git"}}
	instance, err := New(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	results, err := instance.Upgrade(context.Background(), UpgradeOptions{Names: []string{"demo"}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "skipped" {
		t.Fatalf("results = %+v", results)
	}
}

func TestListSortsUnmanagedLast(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, home, "web", `{"dependencies":{"aaa":"1.0.0","zzz":"1.0.0"}}`)
	cfg := config.Resolved{Config: config.Config{DSHHome: home, Unmanaged: []string{"aaa"}, PNPM: "pnpm", Git: "git"}}
	instance, err := New(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	instance.Offline = true
	plugins, err := instance.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plugins) != 2 || plugins[0].Name != "zzz" || plugins[1].Name != "aaa" {
		t.Fatalf("order = %+v", plugins)
	}
}
