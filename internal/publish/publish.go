// Package publish pushes local DSH plugin checkouts to GitHub through gh.
package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Options controls publishing one local plugin checkout.
type Options struct {
	Dir           string
	Repo          string
	Owner         string
	Visibility    string
	Branch        string
	Remote        string
	CommitMessage string
	Tag           bool
	Release       bool
	DryRun        bool
	Force         bool
	Git           string
	GH            string

	// repoURL overrides the GitHub clone URL. Tests use a local bare
	// repository so the push path is exercised without network access.
	repoURL string
}

// Result describes one publish operation.
type Result struct {
	Plugin     string `json:"plugin"`
	Dir        string `json:"dir"`
	Repo       string `json:"repo"`
	URL        string `json:"url"`
	Remote     string `json:"remote"`
	Branch     string `json:"branch"`
	Commit     string `json:"commit,omitempty"`
	Tag        string `json:"tag,omitempty"`
	Visibility string `json:"visibility"`
	Status     string `json:"status"`
}

type packageJSON struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Publish initializes a plugin repository when needed, commits pending files,
// creates the GitHub repository when missing, pushes the branch, and
// optionally creates a tag and release.
func Publish(ctx context.Context, options Options) (*Result, error) {
	if options.Dir == "" {
		return nil, fmt.Errorf("plugin directory is required")
	}
	if options.Git == "" {
		options.Git = "git"
	}
	if options.GH == "" {
		options.GH = "gh"
	}
	if options.Branch == "" {
		options.Branch = "main"
	}
	if options.CommitMessage == "" {
		options.CommitMessage = "chore: publish via dshpm"
	}
	visibility, err := normalizeVisibility(options.Visibility)
	if err != nil {
		return nil, err
	}
	pkg, err := readPackage(options.Dir)
	if err != nil {
		return nil, err
	}
	repo, _, err := resolveRepo(ctx, &options, pkg)
	if err != nil {
		return nil, err
	}
	result := &Result{
		Plugin:     pkg.Name,
		Dir:        options.Dir,
		Repo:       repo,
		URL:        "https://github.com/" + repo,
		Visibility: visibility,
		Branch:     options.Branch,
		Status:     "planned",
	}

	hasGit, err := hasGitRepository(ctx, &options)
	if err != nil {
		return nil, err
	}
	if options.DryRun {
		if !hasGit {
			result.Remote = options.Remote
			return result, nil
		}
		return result, nil
	}
	if !hasGit {
		if _, err := run(ctx, options.Dir, options.Git, "init", "-b", options.Branch); err != nil {
			return nil, err
		}
	}
	root, err := run(ctx, options.Dir, options.Git, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, err
	}
	if filepath.Clean(root) != filepath.Clean(options.Dir) {
		return nil, fmt.Errorf("%s is inside repository %s; publish a single plugin directory, not a monorepo", options.Dir, root)
	}
	if err := ensureGitignore(options.Dir); err != nil {
		return nil, err
	}
	if _, err := run(ctx, options.Dir, options.Git, "add", "-A"); err != nil {
		return nil, err
	}
	if err := checkStaged(options.Dir, options.Git, options.Force); err != nil {
		return nil, err
	}
	status, err := run(ctx, options.Dir, options.Git, "status", "--porcelain")
	if err != nil {
		return nil, err
	}
	_, headErr := run(ctx, options.Dir, options.Git, "rev-parse", "--verify", "HEAD")
	if strings.TrimSpace(status) != "" || headErr != nil {
		if _, err := run(ctx, options.Dir, options.Git, "commit", "-m", options.CommitMessage); err != nil {
			return nil, err
		}
	}
	head, err := run(ctx, options.Dir, options.Git, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	result.Commit = strings.TrimSpace(head)

	if err := ensureRepository(ctx, &options, repo); err != nil {
		return nil, err
	}
	remote, err := ensureRemote(ctx, &options, repo)
	if err != nil {
		return nil, err
	}
	result.Remote = remote
	if _, err := run(ctx, options.Dir, options.Git, "push", "-u", remote, "HEAD:"+options.Branch); err != nil {
		return nil, err
	}
	if options.Tag || options.Release {
		tag, err := publishTag(ctx, &options, repo, remote, pkg.Version)
		if err != nil {
			return nil, err
		}
		result.Tag = tag
	}
	result.Status = "published"
	return result, nil
}

func readPackage(dir string) (*packageJSON, error) {
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return nil, fmt.Errorf("read %s/package.json: %w", dir, err)
	}
	var pkg packageJSON
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil, fmt.Errorf("parse package.json: %w", err)
	}
	if pkg.Name == "" {
		return nil, fmt.Errorf("package.json has no name")
	}
	return &pkg, nil
}

func normalizeVisibility(value string) (string, error) {
	switch strings.ToLower(value) {
	case "", "private":
		return "private", nil
	case "public":
		return "public", nil
	case "internal":
		return "internal", nil
	default:
		return "", fmt.Errorf("visibility must be private, public, or internal")
	}
}

func resolveRepo(ctx context.Context, options *Options, pkg *packageJSON) (string, string, error) {
	if options.Repo != "" {
		parts := strings.Split(options.Repo, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("--repo must look like owner/name")
		}
		return options.Repo, parts[0], nil
	}
	owner := options.Owner
	if owner == "" {
		output, err := run(ctx, ".", options.GH, "api", "user", "--jq", ".login")
		if err != nil {
			return "", "", fmt.Errorf("resolve GitHub owner: %w", err)
		}
		owner = strings.TrimSpace(output)
	}
	if owner == "" {
		return "", "", fmt.Errorf("GitHub owner is empty")
	}
	name := repoName(pkg.Name)
	if name == "" {
		name = filepath.Base(options.Dir)
	}
	return owner + "/" + name, owner, nil
}

func repoName(pkgName string) string {
	name := pkgName
	if index := strings.LastIndex(name, "/"); index >= 0 {
		name = name[index+1:]
	}
	var builder strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			builder.WriteRune(r)
		default:
			builder.WriteRune('-')
		}
	}
	return strings.Trim(builder.String(), "-._")
}

func hasGitRepository(ctx context.Context, options *Options) (bool, error) {
	_, err := run(ctx, options.Dir, options.Git, "rev-parse", "--is-inside-work-tree")
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "not a git repository") || strings.Contains(err.Error(), "exit status 128") {
		return false, nil
	}
	return false, err
}

func ensureGitignore(dir string) error {
	path := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	content := "node_modules/\nlib/\ndist/\n*.tgz\n*.tsbuildinfo\n.DS_Store\n"
	return os.WriteFile(path, []byte(content), 0o644)
}

func checkStaged(dir, git string, force bool) error {
	output, err := run(context.Background(), dir, git, "diff", "--cached", "--name-only", "-z")
	if err != nil {
		return err
	}
	files := strings.Split(output, "\x00")
	total := int64(0)
	count := 0
	for _, name := range files {
		if name == "" {
			continue
		}
		count++
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			continue
		}
		total += info.Size()
		if !force && info.Size() > 50<<20 {
			return fmt.Errorf("staged file %s is larger than 50 MiB; use --force to publish anyway", name)
		}
	}
	if !force && (count > 5000 || total > 200<<20) {
		return fmt.Errorf("staged content is large (%d files, %d MiB); use --force to publish anyway", count, total>>20)
	}
	return nil
}

func ensureRepository(ctx context.Context, options *Options, repo string) error {
	if _, err := run(ctx, ".", options.GH, "repo", "view", repo, "--json", "nameWithOwner"); err == nil {
		return nil
	}
	visibility := "--" + options.Visibility
	if _, err := run(ctx, ".", options.GH, "repo", "create", repo, visibility); err != nil {
		return fmt.Errorf("create GitHub repository %s: %w", repo, err)
	}
	return nil
}

func ensureRemote(ctx context.Context, options *Options, repo string) (string, error) {
	output, err := run(ctx, options.Dir, options.Git, "remote")
	if err != nil {
		return "", err
	}
	remotes := strings.Fields(output)
	target := options.Repo
	if options.repoURL != "" {
		target = options.repoURL
	} else {
		target = "https://github.com/" + repo + ".git"
	}
	if options.Remote != "" {
		if contains(remotes, options.Remote) {
			current, err := run(ctx, options.Dir, options.Git, "remote", "get-url", options.Remote)
			if err != nil {
				return "", err
			}
			if strings.TrimSpace(current) != target && !options.Force {
				return "", fmt.Errorf("remote %s points to %s, not %s", options.Remote, strings.TrimSpace(current), target)
			}
			if strings.TrimSpace(current) != target {
				if _, err := run(ctx, options.Dir, options.Git, "remote", "set-url", options.Remote, target); err != nil {
					return "", err
				}
			}
			return options.Remote, nil
		}
		if _, err := run(ctx, options.Dir, options.Git, "remote", "add", options.Remote, target); err != nil {
			return "", err
		}
		return options.Remote, nil
	}
	for _, name := range remotes {
		current, err := run(ctx, options.Dir, options.Git, "remote", "get-url", name)
		if err == nil && strings.TrimSpace(current) == target {
			return name, nil
		}
	}
	name := "origin"
	if len(remotes) > 0 {
		name = "dshpm"
	}
	if contains(remotes, name) {
		if !options.Force {
			return "", fmt.Errorf("remote %s already exists; choose another --remote", name)
		}
		if _, err := run(ctx, options.Dir, options.Git, "remote", "set-url", name, target); err != nil {
			return "", err
		}
		return name, nil
	}
	if _, err := run(ctx, options.Dir, options.Git, "remote", "add", name, target); err != nil {
		return "", err
	}
	return name, nil
}

func publishTag(ctx context.Context, options *Options, repo, remote, version string) (string, error) {
	version = strings.TrimSpace(strings.TrimPrefix(version, "v"))
	if version == "" {
		return "", fmt.Errorf("package.json has no version; cannot create a release")
	}
	tag := "v" + version
	if _, err := run(ctx, options.Dir, options.Git, "rev-parse", "--verify", "refs/tags/"+tag); err != nil {
		if _, err := run(ctx, options.Dir, options.Git, "tag", "-a", tag, "-m", "dshpm "+tag); err != nil {
			return "", err
		}
	}
	if _, err := run(ctx, options.Dir, options.Git, "push", remote, tag); err != nil {
		return "", err
	}
	if options.Release {
		if _, err := run(ctx, ".", options.GH, "release", "view", tag, "--repo", repo); err != nil {
			if _, err := run(ctx, ".", options.GH, "release", "create", tag, "--repo", repo, "--title", tag, "--generate-notes"); err != nil {
				return "", fmt.Errorf("create release %s: %w", tag, err)
			}
		}
	}
	return tag, nil
}

func contains(values []string, value string) bool {
	for _, item := range values {
		if item == value {
			return true
		}
	}
	return false
}

func run(ctx context.Context, dir, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if text == "" {
			return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, text)
	}
	return text, nil
}
