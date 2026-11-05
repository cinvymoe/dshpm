package cli

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/cinvymoe/dshpm/internal/model"
)

func TestParseInterspersed(t *testing.T) {
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	profile := set.String("profile", "", "profile")
	dryRun := set.Bool("dry-run", false, "dry run")
	positionals, err := parseInterspersed(set, []string{"pkg-a", "--profile", "web", "pkg-b", "--dry-run"}, map[string]bool{"profile": true})
	if err != nil {
		t.Fatal(err)
	}
	if *profile != "web" || !*dryRun {
		t.Fatalf("profile=%q dryRun=%v", *profile, *dryRun)
	}
	if !reflect.DeepEqual(positionals, []string{"pkg-a", "pkg-b"}) {
		t.Fatalf("positionals=%v", positionals)
	}
}

func TestParseInterspersedDoubleDash(t *testing.T) {
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	positionals, err := parseInterspersed(set, []string{"--", "--profile", "web"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(positionals, []string{"--profile", "web"}) {
		t.Fatalf("positionals=%v", positionals)
	}
}

func TestSelectLocalPlugins(t *testing.T) {
	plugins := []model.Plugin{
		{Name: "local-a", Kind: model.SourceLocal, LocalDir: "/tmp/a"},
		{Name: "registry-a", Kind: model.SourceRegistry},
		{Name: "local-b", Kind: model.SourceLocal, LocalDir: "/tmp/b"},
	}
	selected, err := selectLocalPlugins(plugins, nil, true)
	if err != nil || len(selected) != 2 {
		t.Fatalf("all local = %+v, %v", selected, err)
	}
	selected, err = selectLocalPlugins(plugins, []string{"local-a"}, false)
	if err != nil || len(selected) != 1 || selected[0].Name != "local-a" {
		t.Fatalf("named = %+v, %v", selected, err)
	}
	if _, err := selectLocalPlugins(plugins, []string{"registry-a"}, false); err == nil {
		t.Fatal("expected non-local error")
	}
	if _, err := selectLocalPlugins(plugins, []string{"missing"}, false); err == nil {
		t.Fatal("expected unknown error")
	}
}

func TestRunPublishDryRun(t *testing.T) {
	home := t.TempDir()
	profileDir := filepath.Join(home, ".dsh", "profiles", "web")
	pluginDir := filepath.Join(home, "plugin")
	for _, dir := range []string{profileDir, pluginDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "package.json"), []byte(`{"name":"@scope/local-a","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `{"dependencies":{"@scope/local-a":"link:` + pluginDir + `"},"dsh":{"profile":{"bundles":["@scope/local-a"]}}}`
	if err := os.WriteFile(filepath.Join(profileDir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run([]string{"publish", "@scope/local-a", "--dsh-home", filepath.Join(home, ".dsh"), "--profile", "web", "--config", filepath.Join(home, "config.json"), "--owner", "test", "--dry-run"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "test/local-a") || !strings.Contains(stdout.String(), "planned") {
		t.Fatalf("stdout=%s", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(pluginDir, ".git")); !os.IsNotExist(err) {
		t.Fatalf("dry run created .git: %v", err)
	}
}

func TestRunManageWritesConfig(t *testing.T) {
	home := t.TempDir()
	profileDir := filepath.Join(home, "profiles", "web")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "package.json"), []byte(`{"dependencies":{"demo":"1.0.0"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.json")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"unmanage", "demo", "--dsh-home", home, "--profile", "web", "--config", configPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"demo"`) {
		t.Fatalf("config = %s", data)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run([]string{"manage", "demo", "--dsh-home", home, "--profile", "web", "--config", configPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestRunModeWritesConfig(t *testing.T) {
	home := t.TempDir()
	profileDir := filepath.Join(home, "profiles", "web")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileDir, "package.json"), []byte(`{"dependencies":{"demo":"link:/tmp/demo"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.json")
	var stdout, stderr bytes.Buffer
	code := Run([]string{"mode", "local", "demo", "--dsh-home", home, "--profile", "web", "--config", configPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"modes"`) || !strings.Contains(string(data), `"demo": "local"`) {
		t.Fatalf("config = %s", data)
	}
}
