// Package model defines the data exchanged between dshpm components.
package model

// SourceKind classifies how a plugin dependency is installed.
type SourceKind string

const (
	// SourceRegistry is an npm registry dependency (a version or range).
	SourceRegistry SourceKind = "registry"
	// SourceLocal is a filesystem dependency such as link: or file: that
	// has no remote Git repository.
	SourceLocal SourceKind = "local"
	// SourceLocalRemote is a filesystem dependency whose checkout tracks a
	// remote Git repository. dshpm never modifies or rebases this checkout;
	// sync and upgrade operate on a separate remote worktree.
	SourceLocalRemote SourceKind = "local-remote"
	// SourceGit is a git dependency such as github:owner/repo.
	SourceGit SourceKind = "git"
	// SourceTarball is a remote tarball dependency.
	SourceTarball SourceKind = "tarball"
)

// Plugin is one declared dependency of a DSH profile.
type Plugin struct {
	Name             string     `json:"name"`
	Agent            string     `json:"agent,omitempty"`
	Spec             string     `json:"spec"`
	Kind             SourceKind `json:"kind"`
	Managed          bool       `json:"managed"`
	LocalDir         string     `json:"localDir,omitempty"`
	GitURL           string     `json:"gitUrl,omitempty"`
	GitRef           string     `json:"gitRef,omitempty"`
	InstalledVersion string     `json:"installedVersion,omitempty"`
	LocalCommit      string     `json:"localCommit,omitempty"`
	RemoteVersion    string     `json:"remoteVersion,omitempty"`
	RemoteCommit     string     `json:"remoteCommit,omitempty"`
	Branch           string     `json:"branch,omitempty"`
	Upstream         string     `json:"upstream,omitempty"`
	Dirty            int        `json:"dirty"`
	Ahead            int        `json:"ahead"`
	Behind           int        `json:"behind"`
	WorktreeDir      string     `json:"worktreeDir,omitempty"`
	WorktreeRef      string     `json:"worktreeRef,omitempty"`
	WorktreeUsed     bool       `json:"worktreeUsed,omitempty"`
	Status           string     `json:"status"`
	Error            string     `json:"error,omitempty"`
	Bundled          bool       `json:"bundled"`
}

// BackupPlugin records one local plugin checkout inside a backup.
type BackupPlugin struct {
	Name      string `json:"name"`
	Dir       string `json:"dir"`
	Version   string `json:"version,omitempty"`
	Branch    string `json:"branch,omitempty"`
	Head      string `json:"head,omitempty"`
	Upstream  string `json:"upstream,omitempty"`
	Dirty     int    `json:"dirty"`
	PatchFile string `json:"patchFile,omitempty"`
	PatchSize int    `json:"patchSize,omitempty"`
	FilesDir  string `json:"filesDir,omitempty"`
	FilesSize int    `json:"filesSize,omitempty"`
}

// BackupManifest describes a profile backup.
type BackupManifest struct {
	Format      int            `json:"format"`
	CreatedAt   string         `json:"createdAt"`
	Profile     string         `json:"profile"`
	ProfileDir  string         `json:"profileDir"`
	ToolVersion string         `json:"toolVersion"`
	DSHVersion  string         `json:"dshVersion,omitempty"`
	Files       []string       `json:"files"`
	Plugins     []BackupPlugin `json:"plugins"`
}
