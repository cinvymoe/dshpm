package manager

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cinvymoe/dshpm/internal/config"
	"github.com/cinvymoe/dshpm/internal/gitutil"
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

func syncFixture(t *testing.T) (home, upstream, clone string) {
	t.Helper()
	home = t.TempDir()
	upstream = filepath.Join(home, "upstream")
	clone = filepath.Join(home, "external", "demo")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "init", "-b", "main")
	run(t, upstream, "config", "user.email", "test@example.com")
	run(t, upstream, "config", "user.name", "test")
	writeFile(t, filepath.Join(upstream, "a.txt"), "base\n")
	writeFile(t, filepath.Join(upstream, "package.json"), `{"name":"demo","scripts":{"build":"true"}}`)
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "-m", "initial")
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, filepath.Dir(clone), "clone", upstream, clone)
	run(t, clone, "config", "user.email", "test@example.com")
	run(t, clone, "config", "user.name", "test")
	return home, upstream, clone
}

func pureLocalFixture(t *testing.T) (home, clone string) {
	t.Helper()
	home = t.TempDir()
	clone = filepath.Join(home, "external", "demo")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	run(t, clone, "init", "-b", "main")
	run(t, clone, "config", "user.email", "test@example.com")
	run(t, clone, "config", "user.name", "test")
	writeFile(t, filepath.Join(clone, "package.json"), `{"name":"demo","scripts":{"build":"true"}}`)
	writeFile(t, filepath.Join(clone, "a.txt"), "base\n")
	run(t, clone, "add", ".")
	run(t, clone, "commit", "-m", "initial")
	return home, clone
}

func newTestManager(t *testing.T, home, clone string) *Manager {
	t.Helper()
	writeProfile(t, home, "web", `{"dependencies":{"demo":"link:`+clone+`"}}`)
	cfg := config.Resolved{Config: config.Config{
		DSHHome:      home,
		BackupDir:    filepath.Join(home, "backups"),
		WorktreeRoot: filepath.Join(home, "worktrees"),
		PNPM:         "pnpm",
		Git:          "git",
	}}
	instance, err := New(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	return instance
}

func TestSyncLocalRemoteUsesWorktree(t *testing.T) {
	home, upstream, clone := syncFixture(t)
	writeFile(t, filepath.Join(upstream, "b.txt"), "second\n")
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "-m", "second")
	instance := newTestManager(t, home, clone)
	results, err := instance.Sync(context.Background(), SyncOptions{Names: []string{"demo"}, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "worktree" {
		t.Fatalf("results = %+v", results)
	}
	if _, err := os.Stat(filepath.Join(clone, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("local checkout should be untouched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(results[0].WorktreeDir, "b.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestSyncLocalRemoteLeavesLocalCommitUntouched(t *testing.T) {
	home, upstream, clone := syncFixture(t)
	writeFile(t, filepath.Join(clone, "local.txt"), "local\n")
	run(t, clone, "add", ".")
	run(t, clone, "commit", "-m", "local")
	writeFile(t, filepath.Join(upstream, "upstream.txt"), "upstream\n")
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "-m", "upstream")
	before, err := gitutil.Head(clone)
	if err != nil {
		t.Fatal(err)
	}
	instance := newTestManager(t, home, clone)
	results, err := instance.Sync(context.Background(), SyncOptions{Names: []string{"demo"}, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "worktree" {
		t.Fatalf("results = %+v", results)
	}
	after, err := gitutil.Head(clone)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("local HEAD changed: %s -> %s", before, after)
	}
	if _, err := os.Stat(filepath.Join(clone, "upstream.txt")); !os.IsNotExist(err) {
		t.Fatalf("local checkout should be untouched: %v", err)
	}
	if _, err := os.Stat(filepath.Join(results[0].WorktreeDir, "upstream.txt")); err != nil {
		t.Fatal(err)
	}
}

func TestSyncConflictCreatesWorktreeWithoutTouchingLocal(t *testing.T) {
	home, upstream, clone := syncFixture(t)
	writeFile(t, filepath.Join(clone, "a.txt"), "local\n")
	run(t, clone, "commit", "-am", "local")
	writeFile(t, filepath.Join(upstream, "a.txt"), "upstream\n")
	run(t, upstream, "commit", "-am", "upstream")
	before, err := gitutil.Head(clone)
	if err != nil {
		t.Fatal(err)
	}
	instance := newTestManager(t, home, clone)
	results, err := instance.Sync(context.Background(), SyncOptions{Names: []string{"demo"}, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "worktree" {
		t.Fatalf("results = %+v", results)
	}
	after, err := gitutil.Head(clone)
	if err != nil {
		t.Fatal(err)
	}
	if before != after {
		t.Fatalf("local HEAD changed: %s -> %s", before, after)
	}
	content, err := os.ReadFile(filepath.Join(clone, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "local\n" {
		t.Fatalf("local content = %q", content)
	}
	worktree := results[0].WorktreeDir
	if worktree == "" || !gitutil.WorktreeRegistered(clone, worktree) {
		t.Fatalf("worktree not registered: %q", worktree)
	}
	worktreeContent, err := os.ReadFile(filepath.Join(worktree, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(worktreeContent) != "upstream\n" {
		t.Fatalf("worktree content = %q", worktreeContent)
	}
}

func TestSyncDirtyCheckoutUsesWorktree(t *testing.T) {
	home, upstream, clone := syncFixture(t)
	writeFile(t, filepath.Join(clone, "a.txt"), "dirty\n")
	writeFile(t, filepath.Join(upstream, "b.txt"), "upstream\n")
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "-m", "upstream")
	instance := newTestManager(t, home, clone)
	results, err := instance.Sync(context.Background(), SyncOptions{Names: []string{"demo"}, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "worktree" {
		t.Fatalf("results = %+v", results)
	}
	content, err := os.ReadFile(filepath.Join(clone, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "dirty\n" {
		t.Fatalf("local content = %q", content)
	}
}

func TestSyncLocalOnly(t *testing.T) {
	home, _, clone := syncFixture(t)
	run(t, clone, "remote", "remove", "origin")
	instance := newTestManager(t, home, clone)
	results, err := instance.Sync(context.Background(), SyncOptions{Names: []string{"demo"}, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "local-only" {
		t.Fatalf("results = %+v", results)
	}
}

func TestUpgradeUsesWorktreeWhenPresent(t *testing.T) {
	home, upstream, clone := syncFixture(t)
	writeFile(t, filepath.Join(clone, "a.txt"), "local\n")
	run(t, clone, "commit", "-am", "local")
	writeFile(t, filepath.Join(upstream, "a.txt"), "upstream\n")
	run(t, upstream, "commit", "-am", "upstream")
	instance := newTestManager(t, home, clone)
	syncResults, err := instance.Sync(context.Background(), SyncOptions{Names: []string{"demo"}, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if syncResults[0].Status != "worktree" {
		t.Fatalf("sync = %+v", syncResults)
	}
	upgradeResults, err := instance.Upgrade(context.Background(), UpgradeOptions{Names: []string{"demo"}, NoBackup: true, NoBuild: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(upgradeResults) != 1 || upgradeResults[0].WorktreeDir == "" {
		t.Fatalf("upgrade = %+v", upgradeResults)
	}
	if upgradeResults[0].WorktreeDir != syncResults[0].WorktreeDir {
		t.Fatalf("worktree mismatch: %q vs %q", upgradeResults[0].WorktreeDir, syncResults[0].WorktreeDir)
	}
}

func TestUpgradeSwitchesProfileToWorktree(t *testing.T) {
	home, upstream, clone := syncFixture(t)
	writeFile(t, filepath.Join(clone, "a.txt"), "local\n")
	run(t, clone, "commit", "-am", "local")
	writeFile(t, filepath.Join(upstream, "a.txt"), "upstream\n")
	run(t, upstream, "commit", "-am", "upstream")
	instance := newTestManager(t, home, clone)
	syncResults, err := instance.Sync(context.Background(), SyncOptions{Names: []string{"demo"}, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if syncResults[0].Status != "worktree" {
		t.Fatalf("sync = %+v", syncResults)
	}
	logPath := filepath.Join(home, "pnpm.log")
	pnpm := filepath.Join(home, "pnpm")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + logPath + "\n"
	if err := os.WriteFile(pnpm, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	instance.Config.PNPM = pnpm
	results, err := instance.Upgrade(context.Background(), UpgradeOptions{Names: []string{"demo"}, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "upgraded" {
		t.Fatalf("upgrade = %+v", results)
	}
	data, err := os.ReadFile(filepath.Join(home, "profiles", "web", "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"link:`+syncResults[0].WorktreeDir+`"`) {
		t.Fatalf("profile was not switched to worktree: %s", data)
	}
}

func useFakePNPM(t *testing.T, home string, instance *Manager) string {
	t.Helper()
	logPath := filepath.Join(home, "pnpm.log")
	pnpm := filepath.Join(home, "pnpm")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + logPath + "\n"
	if err := os.WriteFile(pnpm, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	instance.Config.PNPM = pnpm
	return logPath
}

func TestUpgradeBuildsPureLocalInPlace(t *testing.T) {
	home, clone := pureLocalFixture(t)
	writeFile(t, filepath.Join(clone, "a.txt"), "dirty\n")
	instance := newTestManager(t, home, clone)
	logPath := useFakePNPM(t, home, instance)
	results, err := instance.Upgrade(context.Background(), UpgradeOptions{
		Names:       []string{"demo"},
		NoBackup:    true,
		BuildSource: BuildSourceLocal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "upgraded" {
		t.Fatalf("results = %+v", results)
	}
	if results[0].BuildSource != BuildSourceLocal {
		t.Fatalf("build source = %q", results[0].BuildSource)
	}
	if _, err := os.Stat(filepath.Join(clone, "a.txt")); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil || !strings.Contains(string(log), "build") {
		t.Fatalf("build was not invoked: %q (%v)", log, err)
	}
}

func TestUpgradePureLocalDoesNotRequireGit(t *testing.T) {
	home := t.TempDir()
	clone := filepath.Join(home, "external", "plain")
	if err := os.MkdirAll(clone, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(clone, "package.json"), `{"name":"plain","scripts":{"build":"true"}}`)
	writeProfile(t, home, "web", `{"dependencies":{"plain":"link:`+clone+`"}}`)
	cfg := config.Resolved{Config: config.Config{DSHHome: home, BackupDir: filepath.Join(home, "backups"), WorktreeRoot: filepath.Join(home, "worktrees"), PNPM: "pnpm", Git: "git"}}
	instance, err := New(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	logPath := useFakePNPM(t, home, instance)
	results, err := instance.Upgrade(context.Background(), UpgradeOptions{Names: []string{"plain"}, NoBackup: true, BuildSource: BuildSourceLocal})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "upgraded" {
		t.Fatalf("results = %+v", results)
	}
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("build was not invoked: %v", err)
	}
}

func TestUpgradeWorktreeSourceRequiresWorktree(t *testing.T) {
	home, _, clone := syncFixture(t)
	instance := newTestManager(t, home, clone)
	results, err := instance.Upgrade(context.Background(), UpgradeOptions{
		Names:       []string{"demo"},
		NoBackup:    true,
		BuildSource: BuildSourceWorktree,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "worktree-missing" {
		t.Fatalf("results = %+v", results)
	}
	if !strings.Contains(results[0].Message, "dshpm sync") {
		t.Fatalf("message = %q", results[0].Message)
	}
}

func TestSyncSkipsUnmanagedPlugin(t *testing.T) {
	home, _, clone := syncFixture(t)
	instance := newTestManager(t, home, clone)
	instance.Config.Unmanaged = []string{"demo"}
	results, err := instance.Sync(context.Background(), SyncOptions{Names: []string{"demo"}, NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "skipped" {
		t.Fatalf("results = %+v", results)
	}
	if !strings.Contains(results[0].Message, "unmanaged") {
		t.Fatalf("message = %q", results[0].Message)
	}
}
