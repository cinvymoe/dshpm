package gitutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func TestCaptureAndApplyPatch(t *testing.T) {
	dir := t.TempDir()
	run(t, dir, "init", "-b", "main")
	run(t, dir, "config", "user.email", "test@example.com")
	run(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", ".")
	run(t, dir, "commit", "-m", "initial")
	head := run(t, dir, "rev-parse", "HEAD")
	head = strings.TrimSpace(head)

	if err := os.WriteFile(filepath.Join(dir, "tracked.txt"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	patch, err := CapturePatch(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(patch) == 0 {
		t.Fatal("patch is empty")
	}
	if err := ResetHard(dir, head); err != nil {
		t.Fatal(err)
	}
	if err := CleanUntracked(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); !os.IsNotExist(err) {
		t.Fatal("new.txt should have been removed")
	}
	if err := ApplyPatch(dir, patch); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(dir, "tracked.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "one\ntwo\n" {
		t.Fatalf("tracked content = %q", content)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err != nil {
		t.Fatalf("new.txt not restored: %v", err)
	}
	status := run(t, dir, "status", "--porcelain")
	if !strings.Contains(status, "tracked.txt") {
		t.Fatalf("expected tracked change in status: %s", status)
	}
}

func TestAheadBehind(t *testing.T) {
	upstream := t.TempDir()
	run(t, upstream, "init", "-b", "main")
	run(t, upstream, "config", "user.email", "test@example.com")
	run(t, upstream, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(upstream, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "-m", "initial")
	clone := filepath.Join(t.TempDir(), "clone")
	run(t, filepath.Dir(clone), "clone", upstream, clone)
	run(t, clone, "config", "user.email", "test@example.com")
	run(t, clone, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(upstream, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, upstream, "add", ".")
	run(t, upstream, "commit", "-m", "second")
	if err := Fetch(clone); err != nil {
		t.Fatal(err)
	}
	ahead, behind, err := AheadBehind(clone, "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if ahead != 0 || behind != 1 {
		t.Fatalf("ahead=%d behind=%d, want 0/1", ahead, behind)
	}
}
