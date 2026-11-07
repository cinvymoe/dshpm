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
	if err := os.MkdirAll(filepath.Join(sourceHome, ".dsh", "profiles", "web"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sourceHome, ".dsh", "profiles", "web", "package.json"), []byte(`{"dependencies":{"alpha":"^1.0.0"}}`), 0o644); err != nil {
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
	out.Reset()
	if code := Run([]string{"remote", "add", "dev", "local", "--config", targetConfig}, &out, &errOut); code != 0 {
		t.Fatalf("add target peer: %d %s", code, errOut.String())
	}
	out.Reset()
	if code := Run([]string{"remote", "pull", "dev", "--config", targetConfig, "--apply"}, &out, &errOut); code != 0 {
		t.Fatalf("pull: %d %s", code, errOut.String())
	}
	assertContainsFile(t, filepath.Join(targetHome, ".dsh", "profiles", "web", "package.json"), "alpha")
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

func TestRemoteSetAndStatusClassifyRemoteAndLocal(t *testing.T) {
	root := t.TempDir()
	dshHome := filepath.Join(root, "dsh")
	piHome := filepath.Join(root, "pi")
	profileDir := filepath.Join(dshHome, "profiles", "web")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(piHome, "npm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "package.json"), []byte(`{"dependencies":{"remote":"1.0.0","local":"link:/tmp/local"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(piHome, "settings.json"), []byte(`{"packages":["npm:alpha","file:/tmp/pi-local"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "config.json")
	var out, errOut bytes.Buffer
	if code := Run([]string{"remote", "set", "owner/repo", "--config", configPath}, &out, &errOut); code != 0 {
		t.Fatalf("remote set: %d %s", code, errOut.String())
	}
	out.Reset()
	if code := Run([]string{"remote", "status", "--config", configPath, "--dsh-home", dshHome, "--pi-home", piHome}, &out, &errOut); code != 0 {
		t.Fatalf("remote status: %d %s", code, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"dsh", "remote", "local", "alpha"} {
		if !strings.Contains(got, want) {
			t.Fatalf("remote status missing %q: %s", want, got)
		}
	}
	if !strings.Contains(got, "pi") || !strings.Contains(got, "alpha") {
		t.Fatalf("remote status missing Pi: %s", got)
	}
}
