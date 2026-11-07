// Package remotesync synchronizes the managed remote plugin set through a
// private GitHub repository. Local and unmanaged plugins stay device-local.
package remotesync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/cinvymoe/dshpm/internal/config"
	"github.com/cinvymoe/dshpm/internal/model"
	"github.com/cinvymoe/dshpm/internal/profile"
)

const stateName = "dshpm-state.json"

// State is the portable description of managed remote plugins.
type State struct {
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"createdAt"`
	DSH       *DSHState `json:"dsh,omitempty"`
	Pi        *PiState  `json:"pi,omitempty"`
}

// DSHState is the remote plugin inventory for one DSH profile.
type DSHState struct {
	Profile string   `json:"profile"`
	Plugins []Plugin `json:"plugins"`
	Bundles []string `json:"bundles,omitempty"`
}

// PiState is the remote package inventory for Pi.
type PiState struct {
	Packages []Package `json:"packages"`
}

// Plugin is one remote DSH dependency.
type Plugin struct {
	Name    string `json:"name"`
	Spec    string `json:"spec"`
	Kind    string `json:"kind"`
	Version string `json:"version,omitempty"`
}

// Package is one remote Pi package entry. Raw preserves object forms such as
// extension filters; Source is the canonical identity used for merging.
type Package struct {
	Name    string          `json:"name"`
	Source  string          `json:"source"`
	Kind    string          `json:"kind"`
	Version string          `json:"version,omitempty"`
	Raw     json.RawMessage `json:"raw,omitempty"`
}

// Change describes one plugin entry that would change on pull.
type Change struct {
	Agent      string `json:"agent"`
	Name       string `json:"name"`
	LocalSpec  string `json:"localSpec,omitempty"`
	RemoteSpec string `json:"remoteSpec,omitempty"`
	Status     string `json:"status"`
}

// StatusRow reports whether one managed plugin is synchronized remotely.
type StatusRow struct {
	Agent   string `json:"agent"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Managed bool   `json:"managed"`
	Sync    string `json:"sync"`
	Spec    string `json:"spec"`
}

// Build creates a state containing only managed remote plugins.
func Build(cfg config.Resolved, profileName, agent string) (*State, error) {
	if agent == "" {
		agent = "all"
	}
	if agent != "all" && agent != "dsh" && agent != "pi" {
		return nil, fmt.Errorf("unknown agent %q (want all, dsh, or pi)", agent)
	}
	state := &State{Version: 1, CreatedAt: time.Now().UTC()}
	if agent == "all" || agent == "dsh" {
		dsh, err := buildDSH(cfg, profileName)
		if err != nil {
			return nil, err
		}
		state.DSH = dsh
	}
	if agent == "all" || agent == "pi" {
		pi, err := buildPi(cfg)
		if err != nil {
			return nil, err
		}
		state.Pi = pi
	}
	if state.DSH == nil && state.Pi == nil {
		return nil, errors.New("no managed remote plugins found")
	}
	return state, nil
}

func buildDSH(cfg config.Resolved, profileName string) (*DSHState, error) {
	name, err := cfg.ResolveProfileName(profileName)
	if err != nil {
		return nil, err
	}
	p, err := profile.Load(cfg.DSHHome, name)
	if err != nil {
		return nil, err
	}
	plugins := p.Plugins()
	local := make(map[string]bool)
	result := &DSHState{Profile: name}
	for _, plugin := range plugins {
		if plugin.Kind == model.SourceLocal || plugin.Kind == model.SourceLocalRemote || !cfg.IsManagedFor("dsh", plugin.Name) {
			local[plugin.Name] = true
			continue
		}
		result.Plugins = append(result.Plugins, Plugin{
			Name: plugin.Name, Spec: plugin.Spec, Kind: string(plugin.Kind), Version: plugin.InstalledVersion,
		})
	}
	for _, bundle := range p.Bundles {
		if local[bundle] || !cfg.IsManagedFor("dsh", bundle) {
			continue
		}
		result.Bundles = append(result.Bundles, bundle)
	}
	if len(result.Plugins) == 0 && len(result.Bundles) == 0 {
		return nil, nil
	}
	sort.Slice(result.Plugins, func(i, j int) bool { return result.Plugins[i].Name < result.Plugins[j].Name })
	return result, nil
}

type piEntry struct {
	Source string
	Name   string
	Kind   model.SourceKind
	Raw    json.RawMessage
}

func buildPi(cfg config.Resolved) (*PiState, error) {
	entries, err := readPiEntries(cfg.PiHome)
	if err != nil {
		return nil, err
	}
	result := &PiState{}
	for _, entry := range entries {
		if entry.Kind == model.SourceLocal || entry.Kind == model.SourceLocalRemote || !cfg.IsManagedFor("pi", entry.Name) {
			continue
		}
		version := readPackageVersion(filepath.Join(cfg.PiHome, "npm", "node_modules", filepath.FromSlash(entry.Name), "package.json"))
		result.Packages = append(result.Packages, Package{
			Name: entry.Name, Source: entry.Source, Kind: string(entry.Kind), Version: version, Raw: entry.Raw,
		})
	}
	if len(result.Packages) == 0 {
		return nil, nil
	}
	sort.Slice(result.Packages, func(i, j int) bool { return result.Packages[i].Name < result.Packages[j].Name })
	return result, nil
}

func readPiEntries(home string) ([]piEntry, error) {
	data, err := os.ReadFile(filepath.Join(home, "settings.json"))
	if err != nil {
		return nil, fmt.Errorf("read Pi settings: %w", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("parse Pi settings: %w", err)
	}
	raw, ok := root["packages"]
	if !ok {
		return nil, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("parse Pi packages: %w", err)
	}
	entries := make([]piEntry, 0, len(items))
	for _, item := range items {
		source, err := packageSource(item)
		if err != nil {
			return nil, err
		}
		if source == "" {
			continue
		}
		entries = append(entries, piEntry{Source: source, Name: piPackageName(source), Kind: classifyPiSource(source, home), Raw: item})
	}
	return entries, nil
}

func packageSource(raw json.RawMessage) (string, error) {
	var source string
	if err := json.Unmarshal(raw, &source); err == nil {
		return strings.TrimSpace(source), nil
	}
	var object struct {
		Source string `json:"source"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return "", fmt.Errorf("parse Pi package entry: %w", err)
	}
	return strings.TrimSpace(object.Source), nil
}

func piPackageName(source string) string {
	value := source
	for _, prefix := range []string{"npm:", "git+", "git:", "github:"} {
		if strings.HasPrefix(value, prefix) {
			value = strings.TrimPrefix(value, prefix)
			break
		}
	}
	if index := strings.LastIndex(value, "#"); index >= 0 {
		value = value[:index]
	}
	if strings.HasPrefix(value, "http://") || strings.HasPrefix(value, "https://") || strings.HasPrefix(value, "ssh://") {
		value = strings.TrimSuffix(value, "/")
		value = strings.TrimSuffix(value, ".git")
		if index := strings.LastIndex(value, "/"); index >= 0 {
			return value[index+1:]
		}
	}
	if strings.HasPrefix(value, "@") {
		// npm scoped name, optionally followed by @version.
		at := strings.LastIndex(value, "@")
		if at > 0 {
			return value[:at]
		}
		return value
	}
	if index := strings.LastIndex(value, "@"); index > 0 {
		value = value[:index]
	}
	value = strings.TrimSuffix(strings.TrimSuffix(value, "/"), ".git")
	if index := strings.LastIndex(value, "/"); index >= 0 {
		value = value[index+1:]
	}
	return value
}

func classifyPiSource(source, home string) model.SourceKind {
	switch {
	case strings.HasPrefix(source, "npm:"):
		return model.SourceRegistry
	case strings.HasPrefix(source, "git:"), strings.HasPrefix(source, "git+"), strings.HasPrefix(source, "github:"),
		strings.HasSuffix(strings.Split(source, "#")[0], ".git"):
		return model.SourceGit
	case strings.HasPrefix(source, "file:"), strings.HasPrefix(source, "./"), strings.HasPrefix(source, "../"),
		strings.HasPrefix(source, "/"), strings.HasPrefix(source, "~/"):
		return model.SourceLocal
	default:
		return model.SourceRegistry
	}
}

func readPackageVersion(path string) string {
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

// ReadState reads a state file.
func ReadState(path string) (*State, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, fmt.Errorf("parse state: %w", err)
	}
	if state.Version != 1 {
		return nil, fmt.Errorf("unsupported state version %d", state.Version)
	}
	return &state, nil
}

// WriteState writes a state file atomically.
func WriteState(path string, state *State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Plan compares a remote state with local configuration.
func Plan(cfg config.Resolved, state *State, profileName string) ([]Change, error) {
	var changes []Change
	if state.DSH != nil {
		name := state.DSH.Profile
		if profileName != "" {
			name = profileName
		}
		p, err := profile.Load(cfg.DSHHome, name)
		if err != nil {
			return nil, err
		}
		local := p.Dependencies
		remote := make(map[string]bool)
		for _, plugin := range state.DSH.Plugins {
			remote[plugin.Name] = true
			current, ok := local[plugin.Name]
			status := "add"
			if ok && current == plugin.Spec {
				status = "same"
			} else if ok {
				status = "update"
			}
			changes = append(changes, Change{Agent: "dsh", Name: plugin.Name, LocalSpec: current, RemoteSpec: plugin.Spec, Status: status})
		}
		for name, spec := range local {
			kind := profile.Classify(name, spec, p.Dir).Kind
			if (kind == model.SourceLocal || kind == model.SourceLocalRemote) || !cfg.IsManagedFor("dsh", name) || remote[name] {
				continue
			}
			changes = append(changes, Change{Agent: "dsh", Name: name, LocalSpec: spec, Status: "remove"})
		}
	}
	if state.Pi != nil {
		entries, err := readPiEntries(cfg.PiHome)
		if err != nil {
			return nil, err
		}
		local := make(map[string]string)
		for _, entry := range entries {
			local[entry.Name] = entry.Source
		}
		remote := make(map[string]bool)
		for _, pkg := range state.Pi.Packages {
			remote[pkg.Name] = true
			current, ok := local[pkg.Name]
			status := "add"
			if ok && current == pkg.Source {
				status = "same"
			} else if ok {
				status = "update"
			}
			changes = append(changes, Change{Agent: "pi", Name: pkg.Name, LocalSpec: current, RemoteSpec: pkg.Source, Status: status})
		}
		for _, entry := range entries {
			if entry.Kind == model.SourceLocal || entry.Kind == model.SourceLocalRemote || !cfg.IsManagedFor("pi", entry.Name) || remote[entry.Name] {
				continue
			}
			changes = append(changes, Change{Agent: "pi", Name: entry.Name, LocalSpec: entry.Source, Status: "remove"})
		}
	}
	sort.Slice(changes, func(i, j int) bool {
		if changes[i].Agent != changes[j].Agent {
			return changes[i].Agent < changes[j].Agent
		}
		return changes[i].Name < changes[j].Name
	})
	return changes, nil
}

// Apply merges the remote state into local package manifests. Local and
// unmanaged entries are preserved.
func Apply(cfg config.Resolved, state *State, profileName string) (string, error) {
	backupDir := filepath.Join(cfg.BackupDir, "github-"+time.Now().Format("20060102-150405"))
	if state.DSH != nil {
		name := state.DSH.Profile
		if profileName != "" {
			name = profileName
		} else if resolved, err := cfg.ResolveProfileName(""); err == nil {
			name = resolved
		}
		dir := filepath.Join(cfg.DSHHome, "profiles", name)
		source := filepath.Join(dir, "package.json")
		if data, err := os.ReadFile(source); err == nil {
			dst := filepath.Join(backupDir, "dsh", "package.json")
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				return "", err
			}
		}
		if err := applyDSH(cfg, dir, state.DSH); err != nil {
			return "", err
		}
	}
	if state.Pi != nil {
		source := filepath.Join(cfg.PiHome, "settings.json")
		if data, err := os.ReadFile(source); err == nil {
			dst := filepath.Join(backupDir, "pi", "settings.json")
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				return "", err
			}
		}
		if err := applyPi(cfg, state.Pi); err != nil {
			return "", err
		}
	}
	return backupDir, nil
}

func applyDSH(cfg config.Resolved, dir string, remote *DSHState) error {
	path := filepath.Join(dir, "package.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	deps := stringMap(root["dependencies"])
	dev := stringMap(root["devDependencies"])
	if deps == nil {
		deps = map[string]string{}
	}
	if dev == nil {
		dev = map[string]string{}
	}
	remoteNames := make(map[string]string, len(remote.Plugins))
	for _, plugin := range remote.Plugins {
		remoteNames[plugin.Name] = plugin.Spec
	}
	remove := func(items map[string]string) {
		for name, spec := range items {
			kind := profile.Classify(name, spec, dir).Kind
			if kind == model.SourceLocal || kind == model.SourceLocalRemote || !cfg.IsManagedFor("dsh", name) {
				continue
			}
			if _, ok := remoteNames[name]; !ok {
				delete(items, name)
			}
		}
	}
	remove(deps)
	remove(dev)
	for name, spec := range remoteNames {
		if _, ok := deps[name]; ok {
			deps[name] = spec
		} else if _, ok := dev[name]; ok {
			dev[name] = spec
		} else {
			deps[name] = spec
		}
	}
	root["dependencies"] = mustMarshal(deps)
	if len(dev) > 0 {
		root["devDependencies"] = mustMarshal(dev)
	}
	if err := setBundles(root, remote.Bundles, deps, dev, cfg, dir); err != nil {
		return err
	}
	return writeJSONFile(path, root)
}

func setBundles(root map[string]json.RawMessage, remote []string, deps, dev map[string]string, cfg config.Resolved, dir string) error {
	var dsh map[string]json.RawMessage
	if raw, ok := root["dsh"]; ok {
		if err := json.Unmarshal(raw, &dsh); err != nil {
			return fmt.Errorf("parse dsh: %w", err)
		}
	}
	if dsh == nil {
		dsh = map[string]json.RawMessage{}
	}
	var profileField map[string]json.RawMessage
	if raw, ok := dsh["profile"]; ok {
		if err := json.Unmarshal(raw, &profileField); err != nil {
			return fmt.Errorf("parse dsh.profile: %w", err)
		}
	}
	if profileField == nil {
		profileField = map[string]json.RawMessage{}
	}
	preserve := map[string]bool{}
	for name, spec := range deps {
		kind := profile.Classify(name, spec, dir).Kind
		if kind == model.SourceLocal || kind == model.SourceLocalRemote || !cfg.IsManagedFor("dsh", name) {
			preserve[name] = true
		}
	}
	for name, spec := range dev {
		kind := profile.Classify(name, spec, dir).Kind
		if kind == model.SourceLocal || kind == model.SourceLocalRemote || !cfg.IsManagedFor("dsh", name) {
			preserve[name] = true
		}
	}
	seen := map[string]bool{}
	bundles := make([]string, 0, len(remote)+len(preserve))
	for _, name := range remote {
		if !seen[name] {
			bundles = append(bundles, name)
			seen[name] = true
		}
	}
	// Keep local/unmanaged bundle entries that are still declared.
	if raw, ok := profileField["bundles"]; ok {
		var current []string
		if err := json.Unmarshal(raw, &current); err == nil {
			for _, name := range current {
				if preserve[name] && !seen[name] {
					bundles = append(bundles, name)
					seen[name] = true
				}
			}
		}
	}
	profileField["bundles"] = mustMarshal(bundles)
	dsh["profile"] = mustMarshal(profileField)
	root["dsh"] = mustMarshal(dsh)
	return nil
}

func applyPi(cfg config.Resolved, remote *PiState) error {
	path := filepath.Join(cfg.PiHome, "settings.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	var current []json.RawMessage
	if raw, ok := root["packages"]; ok {
		if err := json.Unmarshal(raw, &current); err != nil {
			return fmt.Errorf("parse Pi packages: %w", err)
		}
	}
	remoteByName := make(map[string]Package, len(remote.Packages))
	for _, pkg := range remote.Packages {
		remoteByName[pkg.Name] = pkg
	}
	used := map[string]bool{}
	entries := make([]json.RawMessage, 0, len(current)+len(remote.Packages))
	for _, raw := range current {
		source, err := packageSource(raw)
		if err != nil {
			return err
		}
		name := piPackageName(source)
		kind := classifyPiSource(source, cfg.PiHome)
		if kind == model.SourceLocal || kind == model.SourceLocalRemote || !cfg.IsManagedFor("pi", name) {
			entries = append(entries, raw)
			continue
		}
		if pkg, ok := remoteByName[name]; ok {
			entries = append(entries, rawForPackage(pkg))
			used[name] = true
		}
	}
	for _, pkg := range remote.Packages {
		if !used[pkg.Name] {
			entries = append(entries, rawForPackage(pkg))
		}
	}
	root["packages"] = mustMarshal(entries)
	return writeJSONFile(path, root)
}

func rawForPackage(pkg Package) json.RawMessage {
	if len(pkg.Raw) > 0 {
		return pkg.Raw
	}
	data, _ := json.Marshal(pkg.Source)
	return data
}

func stringMap(raw json.RawMessage) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	var values map[string]string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	return values
}

func mustMarshal(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

func writeJSONFile(path string, root map[string]json.RawMessage) error {
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".dshpm-tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Status lists managed plugins and whether each belongs to the remote set.
func Status(cfg config.Resolved, profileName, agent string) ([]StatusRow, error) {
	if agent == "" {
		agent = "all"
	}
	var rows []StatusRow
	if agent == "all" || agent == "dsh" {
		name, err := cfg.ResolveProfileName(profileName)
		if err != nil {
			return nil, err
		}
		p, err := profile.Load(cfg.DSHHome, name)
		if err != nil {
			return nil, err
		}
		for _, plugin := range p.Plugins() {
			managed := cfg.IsManagedFor("dsh", plugin.Name)
			sync := "local"
			if managed && plugin.Kind != model.SourceLocal && plugin.Kind != model.SourceLocalRemote {
				sync = "remote"
			}
			rows = append(rows, StatusRow{Agent: "dsh", Name: plugin.Name, Kind: string(plugin.Kind), Managed: managed, Sync: sync, Spec: plugin.Spec})
		}
	}
	if agent == "all" || agent == "pi" {
		entries, err := readPiEntries(cfg.PiHome)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			managed := cfg.IsManagedFor("pi", entry.Name)
			sync := "local"
			if managed && entry.Kind != model.SourceLocal && entry.Kind != model.SourceLocalRemote {
				sync = "remote"
			}
			rows = append(rows, StatusRow{Agent: "pi", Name: entry.Name, Kind: string(entry.Kind), Managed: managed, Sync: sync, Spec: entry.Source})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Agent != rows[j].Agent {
			return rows[i].Agent < rows[j].Agent
		}
		return rows[i].Name < rows[j].Name
	})
	return rows, nil
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// GitHub stores one state file in a private repository.
type GitHub struct {
	GH       string
	Git      string
	Repo     string
	Branch   string
	CacheDir string
}

// ValidateRepo validates an owner/repository pair.
func ValidateRepo(repo string) error {
	if !repoPattern.MatchString(repo) {
		return fmt.Errorf("invalid GitHub repository %q (want owner/name)", repo)
	}
	return nil
}

func (g GitHub) dir() string {
	return filepath.Join(g.CacheDir, strings.ReplaceAll(g.Repo, "/", "__"))
}

func (g GitHub) run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return string(output), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(output), nil
}

// EnsureRepo creates the private repository when missing and prepares a local
// clone at the configured cache directory.
func (g GitHub) EnsureRepo(ctx context.Context) error {
	if err := ValidateRepo(g.Repo); err != nil {
		return err
	}
	gh := g.GH
	if gh == "" {
		gh = "gh"
	}
	if _, err := g.run(ctx, gh, "repo", "view", g.Repo, "--json", "name"); err != nil {
		if _, createErr := g.run(ctx, gh, "repo", "create", g.Repo, "--private"); createErr != nil {
			return fmt.Errorf("create private repository %s: %w", g.Repo, createErr)
		}
	}
	dir := g.dir()
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if _, err := g.run(ctx, gh, "repo", "clone", g.Repo, dir, "--", "--branch", g.Branch, "--depth", "1"); err != nil {
			if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
				return mkErr
			}
			if _, initErr := g.run(ctx, g.Git, "init", "-b", g.Branch, dir); initErr != nil {
				return initErr
			}
			sshURL, viewErr := g.run(ctx, gh, "repo", "view", g.Repo, "--json", "sshUrl", "--jq", ".sshUrl")
			if viewErr != nil {
				return viewErr
			}
			if _, addErr := g.run(ctx, g.Git, "-C", dir, "remote", "add", "origin", strings.TrimSpace(sshURL)); addErr != nil {
				return addErr
			}
		}
		return nil
	}
	if _, err := g.run(ctx, g.Git, "-C", dir, "fetch", "origin"); err != nil {
		return err
	}
	if _, err := g.run(ctx, g.Git, "-C", dir, "checkout", g.Branch); err != nil {
		if _, createErr := g.run(ctx, g.Git, "-C", dir, "checkout", "-B", g.Branch); createErr != nil {
			return createErr
		}
	}
	_, _ = g.run(ctx, g.Git, "-C", dir, "pull", "--ff-only", "origin", g.Branch)
	return nil
}

// Push writes the state into the repository and commits/pushes it.
func (g GitHub) Push(ctx context.Context, state *State) error {
	if err := g.EnsureRepo(ctx); err != nil {
		return err
	}
	dir := g.dir()
	if err := WriteState(filepath.Join(dir, stateName), state); err != nil {
		return err
	}
	if _, err := g.run(ctx, g.Git, "-C", dir, "add", stateName); err != nil {
		return err
	}
	if _, err := g.run(ctx, g.Git, "-C", dir, "-c", "user.name=dshpm", "-c", "user.email=dshpm@localhost", "commit", "-m", "sync: dshpm plugin state "+state.CreatedAt.Format(time.RFC3339)); err != nil {
		if !strings.Contains(err.Error(), "nothing to commit") && !strings.Contains(err.Error(), "working tree clean") {
			return err
		}
	}
	if _, err := g.run(ctx, g.Git, "-C", dir, "push", "-u", "origin", "HEAD:"+g.Branch); err != nil {
		return err
	}
	return nil
}

// Pull fetches and reads the repository state.
func (g GitHub) Pull(ctx context.Context) (*State, error) {
	if err := g.EnsureRepo(ctx); err != nil {
		return nil, err
	}
	return ReadState(filepath.Join(g.dir(), stateName))
}
