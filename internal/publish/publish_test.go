package publish

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

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
