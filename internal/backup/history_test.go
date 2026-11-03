package backup

import (
	"os"
	"path/filepath"
	"testing"
)

func writeArchive(t *testing.T, root, id, profile, created string) string {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"format":1,"createdAt":"` + created + `","profile":"` + profile + `","profileDir":"/tmp/profile","toolVersion":"test","files":[],"plugins":[]}`
	if err := os.WriteFile(filepath.Join(dir, "backup.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestListLatestAndFind(t *testing.T) {
	root := t.TempDir()
	writeArchive(t, root, "20260101-000000-web", "web", "2026-01-01T00:00:00Z")
	writeArchive(t, root, "20260102-000000-web", "web", "2026-01-02T00:00:00Z")
	writeArchive(t, root, "20260103-000000-other", "other", "2026-01-03T00:00:00Z")

	entries, err := List(root, "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].ID != "20260102-000000-web" || entries[1].ID != "20260101-000000-web" {
		t.Fatalf("entries = %+v", entries)
	}

	latest, err := Latest(root, "web")
	if err != nil {
		t.Fatal(err)
	}
	if latest.ID != "20260102-000000-web" {
		t.Fatalf("latest = %+v", latest)
	}

	found, err := Find(root, "web", "20260101")
	if err != nil {
		t.Fatal(err)
	}
	if found.ID != "20260101-000000-web" {
		t.Fatalf("found = %+v", found)
	}

	if _, err := Find(root, "web", "202601"); err == nil {
		t.Fatal("expected ambiguous prefix error")
	}
}
