// Package remote resolves latest versions from npm and git remotes.
package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Registry queries an npm-compatible registry.
type Registry struct {
	BaseURL string
	Client  *http.Client
}

// NewRegistry returns a registry client using baseURL and a short timeout.
func NewRegistry(baseURL string) *Registry {
	return &Registry{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{Timeout: 15 * time.Second},
	}
}

type registryDocument struct {
	DistTags map[string]string          `json:"dist-tags"`
	Versions map[string]json.RawMessage `json:"versions"`
}

// Latest returns the dist-tag latest version, falling back to the highest
// semantic version in the document.
func (r *Registry) Latest(ctx context.Context, packageName string) (string, error) {
	escaped := strings.ReplaceAll(packageName, "/", "%2F")
	endpoint := r.BaseURL + "/" + escaped
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.npm.registry+json")
	response, err := r.Client.Do(request)
	if err != nil {
		return "", fmt.Errorf("query npm registry for %s: %w", packageName, err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("package %s not found in registry", packageName)
	}
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return "", fmt.Errorf("npm registry returned %s for %s: %s", response.Status, packageName, strings.TrimSpace(string(body)))
	}
	var document registryDocument
	if err := json.NewDecoder(response.Body).Decode(&document); err != nil {
		return "", fmt.Errorf("decode npm registry response for %s: %w", packageName, err)
	}
	if latest := document.DistTags["latest"]; latest != "" {
		return latest, nil
	}
	best := ""
	for version := range document.Versions {
		if version == "" || strings.ContainsAny(version, "[]()<>|* ") {
			continue
		}
		if best == "" || CompareVersions(version, best) > 0 {
			best = version
		}
	}
	if best == "" {
		return "", fmt.Errorf("package %s has no published versions", packageName)
	}
	return best, nil
}

// PackageNameFromSpec returns the registry package name for npm: aliases.
func PackageNameFromSpec(name, spec string) string {
	if strings.HasPrefix(spec, "npm:") {
		value := strings.TrimPrefix(spec, "npm:")
		if index := strings.LastIndex(value, "@"); index > 0 {
			return value[:index]
		}
		return value
	}
	return name
}

var _ = url.PathEscape
