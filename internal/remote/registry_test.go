package remote

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRegistryLatest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/@scope%2Fplugin" && r.URL.EscapedPath() != "/@scope%2Fplugin" {
			t.Errorf("path = %q", r.URL.EscapedPath())
		}
		_, _ = w.Write([]byte(`{"dist-tags":{"latest":"1.4.0"},"versions":{"1.3.0":{},"1.4.0":{}}}`))
	}))
	defer server.Close()
	registry := NewRegistry(server.URL)
	latest, err := registry.Latest(context.Background(), "@scope/plugin")
	if err != nil {
		t.Fatal(err)
	}
	if latest != "1.4.0" {
		t.Fatalf("latest = %q", latest)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.4", -1},
		{"1.2.3", "1.2.3", 0},
		{"2.0.0", "1.9.9", 1},
		{"1.2.3-beta.1", "1.2.3", -1},
		{"1.2.3-beta.2", "1.2.3-beta.1", 1},
	}
	for _, tc := range cases {
		if got := CompareVersions(tc.a, tc.b); got != tc.want {
			t.Errorf("CompareVersions(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}
