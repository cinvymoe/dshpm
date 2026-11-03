package manager

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cinvymoe/dshpm/internal/config"
)

func TestRemoveBundleReferences(t *testing.T) {
	input := []byte(`{
  "name": "profile",
  "bundles": ["keep", "drop"],
  "dsh": {"profile": {"bundles": ["keep", "drop", {"name": "other"}, {"id": "third"}]}}
}`)
	updated, changed, err := removeBundleReferences(input, map[string]struct{}{"drop": {}})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected a change")
	}
	var document map[string]any
	if err := json.Unmarshal(updated, &document); err != nil {
		t.Fatal(err)
	}
	top := document["bundles"].([]any)
	if len(top) != 1 || top[0] != "keep" {
		t.Fatalf("top bundles = %#v", top)
	}
	profile := document["dsh"].(map[string]any)["profile"].(map[string]any)
	bundles := profile["bundles"].([]any)
	if len(bundles) != 3 {
		t.Fatalf("profile bundles = %#v", bundles)
	}
}

func TestRemoveInsertBlocks(t *testing.T) {
	content := strings.Join([]string{
		"# keep this comment",
		"- insert:",
		"    - id: keep",
		"      name: keep",
		"# remove this plugin",
		"- insert:",
		"    - id: drop",
		"      name: drop",
		"- insert:",
		"    - id: other",
		"      name: other",
		"",
	}, "\n")
	updated, changed := removeInsertBlocks(content, map[string]struct{}{"drop": {}})
	if !changed {
		t.Fatal("expected a change")
	}
	if strings.Contains(updated, "id: drop") || strings.Contains(updated, "name: drop") {
		t.Fatalf("drop block remains:\n%s", updated)
	}
	if !strings.Contains(updated, "id: keep") || !strings.Contains(updated, "id: other") || !strings.Contains(updated, "# keep this comment") {
		t.Fatalf("unrelated content was lost:\n%s", updated)
	}
}

func TestCleanProfileReferences(t *testing.T) {
	dir := t.TempDir()
	packageJSON := `{"name":"profile","dsh":{"profile":{"bundles":["keep","drop"]}}}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(packageJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	patch := "- insert:\n    - id: drop\n      name: drop\n- insert:\n    - id: keep\n"
	if err := os.WriteFile(filepath.Join(dir, "cordis.patch.yml"), []byte(patch), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := cleanProfileReferences(dir, map[string]struct{}{"drop": {}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "drop") || !strings.Contains(string(data), "keep") {
		t.Fatalf("package.json = %s", data)
	}
	patchData, err := os.ReadFile(filepath.Join(dir, "cordis.patch.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(patchData), "drop") || !strings.Contains(string(patchData), "keep") {
		t.Fatalf("patch = %s", patchData)
	}
}

func TestUninstallDryRun(t *testing.T) {
	home := t.TempDir()
	writeProfile(t, home, "web", `{"dependencies":{"demo":"1.0.0"}}`)
	cfg := config.Resolved{Config: config.Config{DSHHome: home, PNPM: "pnpm", Git: "git"}}
	instance, err := New(cfg, "web")
	if err != nil {
		t.Fatal(err)
	}
	results, err := instance.Uninstall(context.Background(), UninstallOptions{Names: []string{"demo"}, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Status != "planned" {
		t.Fatalf("results = %+v", results)
	}
}
