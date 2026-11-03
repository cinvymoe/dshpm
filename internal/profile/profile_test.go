package profile

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cinvymoe/dshpm/internal/model"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		spec string
		kind model.SourceKind
		dir  string
		url  string
		ref  string
	}{
		{spec: "link:/tmp/plugin", kind: model.SourceLocal, dir: "/tmp/plugin"},
		{spec: "link:../plugin", kind: model.SourceLocal, dir: "/profile/../plugin"},
		{spec: "file:./vendor/plugin", kind: model.SourceLocal, dir: "/profile/vendor/plugin"},
		{spec: "github:owner/repo#v1", kind: model.SourceGit, url: "https://github.com/owner/repo.git", ref: "v1"},
		{spec: "git+https://github.com/owner/repo.git#main", kind: model.SourceGit, url: "https://github.com/owner/repo.git", ref: "main"},
		{spec: "https://example.com/plugin.tgz", kind: model.SourceTarball},
		{spec: "1.2.3", kind: model.SourceRegistry},
		{spec: "^1.2.3", kind: model.SourceRegistry},
		{spec: "latest", kind: model.SourceRegistry},
	}
	for _, tc := range cases {
		got := Classify("plugin", tc.spec, "/profile")
		if got.Kind != tc.kind {
			t.Errorf("Classify(%q).Kind = %q, want %q", tc.spec, got.Kind, tc.kind)
		}
		if tc.dir != "" && got.LocalDir != filepath.Clean(tc.dir) {
			t.Errorf("Classify(%q).LocalDir = %q, want %q", tc.spec, got.LocalDir, tc.dir)
		}
		if tc.url != "" && got.GitURL != tc.url {
			t.Errorf("Classify(%q).GitURL = %q, want %q", tc.spec, got.GitURL, tc.url)
		}
		if tc.ref != "" && got.GitRef != tc.ref {
			t.Errorf("Classify(%q).GitRef = %q, want %q", tc.spec, got.GitRef, tc.ref)
		}
	}
}

func TestLoad(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "profiles", "web")
	if err := os.MkdirAll(filepath.Join(dir, "node_modules", "demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{
		"dependencies": {"demo": "link:../demo", "other": "1.0.0"},
		"dsh": {"profile": {"bundles": ["demo"]}}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "demo", "package.json"), []byte(`{"name":"demo","version":"0.3.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(home, "web")
	if err != nil {
		t.Fatal(err)
	}
	plugins := p.Plugins()
	if len(plugins) != 2 {
		t.Fatalf("plugins = %d, want 2", len(plugins))
	}
	if plugins[0].Name != "demo" || plugins[0].Kind != model.SourceLocal || plugins[0].InstalledVersion != "0.3.0" || !plugins[0].Bundled {
		t.Fatalf("unexpected first plugin: %+v", plugins[0])
	}
}

func TestLockGitCommits(t *testing.T) {
	dir := t.TempDir()
	lock := `lockfileVersion: '9.0'

importers:

  .:
    dependencies:
      '@nanmicoder/dsh-agent-teams':
        specifier: github:NanmiCoder/dsh-agent-teams
        version: https://codeload.github.com/NanmiCoder/dsh-agent-teams/tar.gz/3331b903f0e954d59ac69be1a75fd4fd7e1de20e
      dsh-context7:
        specifier: link:/root/.dsh/external/dsh-context7
        version: link:../../external/dsh-context7
`
	if err := os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), []byte(lock), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LockGitCommits(dir)
	if got["@nanmicoder/dsh-agent-teams"] != "3331b903f0e954d59ac69be1a75fd4fd7e1de20e" {
		t.Fatalf("commit = %q", got["@nanmicoder/dsh-agent-teams"])
	}
	if _, exists := got["dsh-context7"]; exists {
		t.Fatal("link dependency must not have a git commit")
	}
}
