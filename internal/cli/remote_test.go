package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemotePeerConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	var out, errOut bytes.Buffer
	if code := Run([]string{"remote", "add", "dev", "example.test", "--config", configPath}, &out, &errOut); code != 0 {
		t.Fatalf("remote add code = %d, stderr = %s", code, errOut.String())
	}
	out.Reset()
	if code := Run([]string{"remote", "list", "--config", configPath}, &out, &errOut); code != 0 {
		t.Fatalf("remote list code = %d, stderr = %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "dev") || !strings.Contains(out.String(), "example.test") {
		t.Fatalf("remote list output = %q", out.String())
	}
	var cfg map[string]any
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	peers, ok := cfg["peers"].(map[string]any)
	if !ok || peers["dev"] != "example.test" {
		t.Fatalf("config peers = %#v", cfg["peers"])
	}
}

func TestRemotePushPullWithFakeSSH(t *testing.T) {
	root := t.TempDir()
	sourceHome := filepath.Join(root, "source")
	targetHome := filepath.Join(root, "target")
	for _, home := range []string{sourceHome, targetHome} {
		if err := os.MkdirAll(filepath.Join(home, ".dsh", "profiles"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(home, ".pi", "agent", "npm"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"web", "lab"} {
		if err := os.MkdirAll(filepath.Join(sourceHome, ".dsh", "profiles", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(sourceHome, ".dsh", "profiles", "web", "package.json"), []byte(`{"dependencies":{"alpha":"^1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceHome, ".dsh", "profiles", "lab", "package.json"), []byte(`{"dependencies":{"beta":"^2.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceHome, ".pi", "agent", "settings.json"), []byte(`{"packages":["npm:alpha"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceHome, ".pi", "agent", "npm", "package.json"), []byte(`{"dependencies":{"alpha":"1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeSSH := filepath.Join(root, "fake-ssh")
	if err := os.WriteFile(fakeSSH, []byte("#!/bin/sh\nshift\nexec sh -c \"$*\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	remoteRoot := filepath.Join(root, "remote")
	writeConfig := func(path, home string) {
		cfg := map[string]string{
			"dshHome":    filepath.Join(home, ".dsh"),
			"piHome":     filepath.Join(home, ".pi", "agent"),
			"remoteRoot": remoteRoot,
			"ssh":        fakeSSH,
		}
		data, _ := json.Marshal(cfg)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sourceConfig := filepath.Join(root, "source.json")
	targetConfig := filepath.Join(root, "target.json")
	writeConfig(sourceConfig, sourceHome)
	writeConfig(targetConfig, targetHome)
	var out, errOut bytes.Buffer
	if code := Run([]string{"remote", "add", "dev", "local", "--config", sourceConfig}, &out, &errOut); code != 0 {
		t.Fatalf("add source peer: %d %s", code, errOut.String())
	}
	out.Reset()
	if code := Run([]string{"remote", "push", "dev", "--config", sourceConfig}, &out, &errOut); code != 0 {
		t.Fatalf("push: %d %s", code, errOut.String())
	}
	pushOut := out.String()
	for _, want := range []string{"web", "lab", "v3", "4 files"} {
		if !strings.Contains(pushOut, want) {
			t.Fatalf("push output missing %q: %s", want, pushOut)
		}
	}
	out.Reset()
	if code := Run([]string{"remote", "add", "dev", "local", "--config", targetConfig}, &out, &errOut); code != 0 {
		t.Fatalf("add target peer: %d %s", code, errOut.String())
	}
	out.Reset()
	if code := Run([]string{"remote", "pull", "dev", "--config", targetConfig, "--apply"}, &out, &errOut); code != 0 {
		t.Fatalf("pull: %d %s", code, errOut.String())
	}
	assertContainsFile(t, filepath.Join(targetHome, ".dsh", "profiles", "web", "package.json"), "alpha")
	assertContainsFile(t, filepath.Join(targetHome, ".dsh", "profiles", "lab", "package.json"), "beta")
	assertContainsFile(t, filepath.Join(targetHome, ".pi", "agent", "settings.json"), "npm:alpha")
}

func assertContainsFile(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s = %q, want %q", path, string(data), want)
	}
}

// TestRemoteSetAndListBackupRepo checks the GitHub backup configuration path
// and that the removed `remote status` command no longer exists.
func TestRemoteSetAndListBackupRepo(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	var out, errOut bytes.Buffer
	if code := Run([]string{"remote", "set", "owner/repo", "--config", configPath}, &out, &errOut); code != 0 {
		t.Fatalf("remote set: %d %s", code, errOut.String())
	}
	out.Reset()
	if code := Run([]string{"remote", "list", "--config", configPath}, &out, &errOut); code != 0 {
		t.Fatalf("remote list: %d %s", code, errOut.String())
	}
	got := out.String()
	if !strings.Contains(got, "owner/repo") {
		t.Fatalf("remote list missing repo: %s", got)
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg["remoteRepo"] != "owner/repo" {
		t.Fatalf("remoteRepo = %#v", cfg["remoteRepo"])
	}
	if code := Run([]string{"remote", "status", "--config", configPath}, &out, &errOut); code == 0 {
		t.Fatal("remote status should be an unknown command now")
	}
}
