package publish

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckBundlePatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cordis.patch.yml"), []byte("- insert: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config", "cordis.patch.yml"), []byte("- insert: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	parse := func(body string) *packageJSON {
		t.Helper()
		var pkg packageJSON
		if err := json.Unmarshal([]byte(body), &pkg); err != nil {
			t.Fatal(err)
		}
		return &pkg
	}
	tests := []struct {
		name    string
		body    string
		force   bool
		wantErr string
	}{
		{name: "no patch configured", body: `{"name":"a","files":["lib"]}`},
		{name: "patch listed in files", body: `{"name":"a","files":["lib","cordis.patch.yml"],"dsh":{"bundle":{"patch":"./cordis.patch.yml"}}}`},
		{name: "patch missing from files", body: `{"name":"a","files":["lib"],"dsh":{"bundle":{"patch":"./cordis.patch.yml"}}}`, wantErr: "files"},
		{name: "force bypasses files check", body: `{"name":"a","files":["lib"],"dsh":{"bundle":{"patch":"./cordis.patch.yml"}}}`, force: true},
		{name: "no files array ships everything", body: `{"name":"a","dsh":{"bundle":{"patch":"./cordis.patch.yml"}}}`},
		{name: "glob entry covers patch", body: `{"name":"a","files":["lib","*.yml"],"dsh":{"bundle":{"patch":"./cordis.patch.yml"}}}`},
		{name: "directory entry covers patch", body: `{"name":"a","files":["config"],"dsh":{"bundle":{"patch":"config/cordis.patch.yml"}}}`},
		{name: "patch target missing on disk", body: `{"name":"a","files":["lib","cordis.patch.yml"],"dsh":{"bundle":{"patch":"./missing.yml"}}}`, wantErr: "missing.yml"},
		{name: "patch escapes package", body: `{"name":"a","dsh":{"bundle":{"patch":"../shared.yml"}}}`, wantErr: "escapes"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := checkBundlePatch(dir, parse(test.body), test.force)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("checkBundlePatch() = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("checkBundlePatch() = %v, want error containing %q", err, test.wantErr)
			}
		})
	}
}

func TestPublishRejectsUnlistedBundlePatch(t *testing.T) {
	root := t.TempDir()
	plugin := filepath.Join(root, "plugin")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"demo","version":"0.1.0","files":["lib"],"dsh":{"bundle":{"patch":"./cordis.patch.yml"}}}`
	if err := os.WriteFile(filepath.Join(plugin, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "cordis.patch.yml"), []byte("- insert: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Publish(context.Background(), Options{
		Dir:    plugin,
		Owner:  "cinvymoe",
		DryRun: true,
		Git:    "git",
		GH:     fakeGH(t),
	})
	if err == nil || !strings.Contains(err.Error(), "files") {
		t.Fatalf("Publish() = %v, want files error", err)
	}
	result, err := Publish(context.Background(), Options{
		Dir:    plugin,
		Owner:  "cinvymoe",
		DryRun: true,
		Force:  true,
		Git:    "git",
		GH:     fakeGH(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "planned" {
		t.Fatalf("result = %+v", result)
	}
}

func runTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func fakeGH(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gh")
	script := `#!/bin/sh
case "$1 $2" in
  "api user") echo cinvymoe ;;
  "repo view") echo "not found" >&2; exit 1 ;;
  "repo create") echo "created" ;;
  "release view") exit 1 ;;
  "release create") echo "released" ;;
  *) echo "unexpected: $@" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRepoName(t *testing.T) {
	tests := map[string]string{
		"@dsh-external/dsh-demo": "dsh-demo",
		"dsh-demo":               "dsh-demo",
		"@scope/dsh demo":        "dsh-demo",
	}
	for input, want := range tests {
		if got := repoName(input); got != want {
			t.Errorf("repoName(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPublishCreatesRepositoryAndTag(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "dshpm test")
	t.Setenv("GIT_AUTHOR_EMAIL", "dshpm@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "dshpm test")
	t.Setenv("GIT_COMMITTER_EMAIL", "dshpm@example.com")
	root := t.TempDir()
	plugin := filepath.Join(root, "plugin")
	if err := os.MkdirAll(filepath.Join(plugin, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "package.json"), []byte(`{"name":"@dsh-external/dsh-demo","version":"1.2.3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "src", "index.ts"), []byte("export {};\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(root, "remote.git")
	runTest(t, root, "init", "--bare", bare)
	result, err := Publish(context.Background(), Options{
		Dir:        plugin,
		Repo:       "cinvymoe/dsh-demo",
		Visibility: "private",
		Branch:     "main",
		Tag:        true,
		Release:    true,
		GH:         fakeGH(t),
		Git:        "git",
		repoURL:    bare,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" || result.Repo != "cinvymoe/dsh-demo" || result.Remote != "origin" || result.Tag != "v1.2.3" {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(plugin, ".gitignore")); err != nil {
		t.Fatal(err)
	}
	if remote := runTest(t, plugin, "remote", "get-url", "origin"); remote != bare {
		t.Fatalf("origin = %q, want %q", remote, bare)
	}
	log := runTest(t, bare, "log", "--oneline", "main")
	if !strings.Contains(log, result.Commit[:7]) {
		t.Fatalf("bare log = %q, commit = %q", log, result.Commit)
	}
	tags := runTest(t, plugin, "tag", "--list")
	if !strings.Contains(tags, "v1.2.3") {
		t.Fatalf("tags = %q", tags)
	}
}

func TestPublishDryRun(t *testing.T) {
	root := t.TempDir()
	plugin := filepath.Join(root, "plugin")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "package.json"), []byte(`{"name":"demo","version":"0.1.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Publish(context.Background(), Options{
		Dir:        plugin,
		Owner:      "cinvymoe",
		Repo:       "",
		Visibility: "private",
		DryRun:     true,
		Git:        "git",
		GH:         fakeGH(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "planned" || result.Repo != "cinvymoe/demo" {
		t.Fatalf("result = %+v", result)
	}
	if _, err := os.Stat(filepath.Join(plugin, ".git")); !os.IsNotExist(err) {
		t.Fatalf("dry run created .git: %v", err)
	}
}

func TestPublishExistingGitRepository(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "dshpm test")
	t.Setenv("GIT_AUTHOR_EMAIL", "dshpm@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "dshpm test")
	t.Setenv("GIT_COMMITTER_EMAIL", "dshpm@example.com")
	root := t.TempDir()
	plugin := filepath.Join(root, "plugin")
	if err := os.MkdirAll(plugin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "package.json"), []byte(`{"name":"demo","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(root, "remote.git")
	runTest(t, root, "init", "--bare", bare)
	runTest(t, plugin, "init", "-b", "main")
	runTest(t, plugin, "add", "-A")
	runTest(t, plugin, "commit", "-m", "initial")
	runTest(t, plugin, "remote", "add", "origin", bare)
	result, err := Publish(context.Background(), Options{
		Dir:        plugin,
		Repo:       "cinvymoe/demo",
		Visibility: "private",
		Branch:     "main",
		GH:         fakeGH(t),
		Git:        "git",
		repoURL:    bare,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "published" || result.Remote != "origin" {
		t.Fatalf("result = %+v", result)
	}
	if remote := runTest(t, plugin, "remote", "get-url", "origin"); remote != bare {
		t.Fatalf("origin = %q, want %q", remote, bare)
	}
	if log := runTest(t, bare, "log", "--oneline", "main"); !strings.Contains(log, result.Commit[:7]) {
		t.Fatalf("bare log = %q", log)
	}
}

func TestPublishIncludesIgnoredBuildOutput(t *testing.T) {
	t.Setenv("GIT_AUTHOR_NAME", "dshpm test")
	t.Setenv("GIT_AUTHOR_EMAIL", "dshpm@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "dshpm test")
	t.Setenv("GIT_COMMITTER_EMAIL", "dshpm@example.com")
	root := t.TempDir()
	plugin := filepath.Join(root, "plugin")
	if err := os.MkdirAll(filepath.Join(plugin, "lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "package.json"), []byte(`{"name":"@dsh-external/dsh-demo","version":"1.2.3","main":"./lib/index.js"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, ".gitignore"), []byte("node_modules/\nlib/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plugin, "lib", "index.js"), []byte("export {};\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bare := filepath.Join(root, "remote.git")
	runTest(t, root, "init", "--bare", bare)
	if _, err := Publish(context.Background(), Options{
		Dir:        plugin,
		Repo:       "cinvymoe/dsh-demo",
		Visibility: "private",
		Branch:     "main",
		Git:        "git",
		GH:         fakeGH(t),
		repoURL:    bare,
	}); err != nil {
		t.Fatal(err)
	}
	files := runTest(t, bare, "ls-tree", "-r", "--name-only", "main")
	if !strings.Contains(files, "lib/index.js") {
		t.Fatalf("published tree does not include ignored build output: %s", files)
	}
}
