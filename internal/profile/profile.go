// Package profile loads a DSH profile manifest and classifies its plugin
// dependencies as registry, local, git, or tarball sources.
package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/cinvymoe/dshpm/internal/model"
)

// Profile is a loaded DSH profile.
type Profile struct {
	Name         string
	Dir          string
	Dependencies map[string]string
	Bundles      []string
	Modes        map[string]string
	raw          map[string]json.RawMessage
}

type packageJSON struct {
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	DSH             struct {
		Profile struct {
			Bundles []string `json:"bundles"`
		} `json:"profile"`
	} `json:"dsh"`
}

// Load reads <dshHome>/profiles/<name>/package.json.
func Load(dshHome, name string) (*Profile, error) {
	dir := filepath.Join(dshHome, "profiles", name)
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, fmt.Errorf("read profile %s: %w", name, err)
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("parse profile %s package.json: %w", name, err)
	}
	deps := make(map[string]string, len(pkg.Dependencies)+len(pkg.DevDependencies))
	for name, spec := range pkg.Dependencies {
		deps[name] = spec
	}
	for name, spec := range pkg.DevDependencies {
		if _, exists := deps[name]; !exists {
			deps[name] = spec
		}
	}
	return &Profile{
		Name:         name,
		Dir:          dir,
		Dependencies: deps,
		Bundles:      pkg.DSH.Profile.Bundles,
	}, nil
}

// SetDependency replaces the spec of one declared dependency in
// package.json and updates the in-memory dependency map. The file is written
// atomically so a failed switch cannot leave a partially written manifest.
func (p *Profile) SetDependency(name, spec string) error {
	packagePath := filepath.Join(p.Dir, "package.json")
	data, err := os.ReadFile(packagePath)
	if err != nil {
		return fmt.Errorf("read profile package.json: %w", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse profile package.json: %w", err)
	}
	found := false
	for _, section := range []string{"dependencies", "devDependencies"} {
		raw, ok := root[section]
		if !ok {
			continue
		}
		var dependencies map[string]string
		if err := json.Unmarshal(raw, &dependencies); err != nil {
			return fmt.Errorf("parse %s: %w", section, err)
		}
		if _, ok := dependencies[name]; !ok {
			continue
		}
		dependencies[name] = spec
		encoded, err := json.Marshal(dependencies)
		if err != nil {
			return err
		}
		root[section] = encoded
		found = true
		break
	}
	if !found {
		return fmt.Errorf("dependency %s is not declared in package.json", name)
	}
	updated, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	updated = append(updated, '\n')
	temporary, err := os.CreateTemp(p.Dir, ".package.json.*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(updated); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, packagePath); err != nil {
		return err
	}
	if p.Dependencies != nil {
		p.Dependencies[name] = spec
	}
	return nil
}

// Plugins returns one entry per declared dependency, enriched with locally
// observable facts. Remote facts are intentionally not filled here.
func (p *Profile) Plugins() []model.Plugin {
	bundled := make(map[string]bool, len(p.Bundles))
	for _, name := range p.Bundles {
		bundled[name] = true
	}
	plugins := make([]model.Plugin, 0, len(p.Dependencies))
	for name, spec := range p.Dependencies {
		plugin := Classify(name, spec, p.Dir)
		plugin.Bundled = bundled[name]
		plugin.InstalledVersion = readInstalledVersion(filepath.Join(p.Dir, "node_modules", filepath.FromSlash(name), "package.json"))
		if plugin.Kind == model.SourceLocal && plugin.LocalDir != "" {
			FillLocalGitFacts(&plugin)
			if plugin.GitURL != "" {
				plugin.Kind = model.SourceLocalRemote
			}
		}
		if p.Modes[name] == "local" {
			plugin.Kind = model.SourceLocal
		}
		plugins = append(plugins, plugin)
	}
	sortPlugins(plugins)
	return plugins
}

// Classify determines a dependency's source kind and normalizes its location.
func Classify(name, spec, profileDir string) model.Plugin {
	plugin := model.Plugin{Name: name, Spec: spec, Kind: model.SourceRegistry}
	switch {
	case strings.HasPrefix(spec, "link:"):
		plugin.Kind = model.SourceLocal
		plugin.LocalDir = resolveLocal(strings.TrimPrefix(spec, "link:"), profileDir)
	case strings.HasPrefix(spec, "file:"):
		value := strings.TrimPrefix(spec, "file:")
		if isTarball(value) {
			plugin.Kind = model.SourceTarball
		} else {
			plugin.Kind = model.SourceLocal
			plugin.LocalDir = resolveLocal(value, profileDir)
		}
	case strings.HasPrefix(spec, "workspace:"):
		plugin.Kind = model.SourceLocal
		plugin.LocalDir = profileDir
	case strings.HasPrefix(spec, "github:"):
		plugin.Kind = model.SourceGit
		plugin.GitURL, plugin.GitRef = parseGitHub(spec)
	case strings.HasPrefix(spec, "git+"):
		plugin.Kind = model.SourceGit
		plugin.GitURL, plugin.GitRef = parseGitURL(strings.TrimPrefix(spec, "git+"))
	case strings.HasPrefix(spec, "git://"), strings.HasPrefix(spec, "ssh://"):
		plugin.Kind = model.SourceGit
		plugin.GitURL, plugin.GitRef = parseGitURL(spec)
	case strings.HasPrefix(spec, "http://"), strings.HasPrefix(spec, "https://"):
		if isTarball(spec) {
			plugin.Kind = model.SourceTarball
		} else if strings.HasSuffix(strings.Split(spec, "#")[0], ".git") {
			plugin.Kind = model.SourceGit
			plugin.GitURL, plugin.GitRef = parseGitURL(spec)
		} else {
			plugin.Kind = model.SourceTarball
		}
	case strings.HasPrefix(spec, "npm:"):
		plugin.Kind = model.SourceRegistry
	}
	return plugin
}

func resolveLocal(value, profileDir string) string {
	value = strings.TrimPrefix(value, "//")
	if value == "" {
		return ""
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Clean(filepath.Join(profileDir, value))
}

func parseGitHub(spec string) (url, ref string) {
	value := strings.TrimPrefix(spec, "github:")
	value, ref = splitRef(value)
	value = strings.TrimSuffix(value, "/")
	return "https://github.com/" + value + ".git", ref
}

func parseGitURL(value string) (url, ref string) {
	value, ref = splitRef(value)
	return value, ref
}

func splitRef(value string) (string, string) {
	if index := strings.LastIndex(value, "#"); index >= 0 {
		return value[:index], value[index+1:]
	}
	return value, ""
}

func isTarball(value string) bool {
	value = strings.Split(value, "#")[0]
	lower := strings.ToLower(value)
	return strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tar.gz") || strings.HasSuffix(lower, ".tar")
}

func readInstalledVersion(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return ""
	}
	return pkg.Version
}

func sortPlugins(plugins []model.Plugin) {
	for i := 1; i < len(plugins); i++ {
		for j := i; j > 0 && plugins[j].Name < plugins[j-1].Name; j-- {
			plugins[j], plugins[j-1] = plugins[j-1], plugins[j]
		}
	}
}

var commitPattern = regexp.MustCompile(`\b[0-9a-f]{40}\b`)

// LockGitCommits parses pnpm-lock.yaml importer entries and returns the
// resolved commit for git-hosted dependencies.
func LockGitCommits(profileDir string) map[string]string {
	result := make(map[string]string)
	data, err := os.ReadFile(filepath.Join(profileDir, "pnpm-lock.yaml"))
	if err != nil {
		return result
	}
	lines := strings.Split(string(data), "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(trimmed, "specifier:") {
			continue
		}
		spec := strings.TrimSpace(strings.TrimPrefix(trimmed, "specifier:"))
		if !strings.HasPrefix(spec, "github:") && !strings.HasPrefix(spec, "git+") {
			continue
		}
		name := ""
		for j := i - 1; j >= 0; j-- {
			candidate := strings.TrimSpace(lines[j])
			if candidate == "" {
				continue
			}
			if strings.HasSuffix(candidate, ":") && !strings.HasPrefix(candidate, "specifier:") {
				name = strings.TrimSuffix(candidate, ":")
				name = strings.Trim(name, "'\"")
				break
			}
			break
		}
		if name == "" {
			continue
		}
		for j := i + 1; j < len(lines) && j <= i+3; j++ {
			if strings.Contains(lines[j], "version:") {
				if match := commitPattern.FindString(lines[j]); match != "" {
					result[name] = match
				}
				break
			}
		}
	}
	return result
}
