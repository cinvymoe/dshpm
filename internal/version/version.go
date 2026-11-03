// Package version exposes build metadata for dshpm.
package version

// Version is the semantic version of the running binary. Release builds
// replace it with -ldflags "-X github.com/cinvymoe/dshpm/internal/version.Version=<tag>".
var Version = "0.1.0-dev"

// Commit is the source revision embedded at build time, when available.
var Commit = "unknown"
