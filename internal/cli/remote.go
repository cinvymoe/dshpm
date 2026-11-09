package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/cinvymoe/dshpm/internal/devicesync"
	"github.com/cinvymoe/dshpm/internal/remotesync"
)

const remoteUsage = `dshpm remote backs up and restores configuration between devices.

Usage:
  dshpm remote <command> [options]

Commands:
  list                          List configured GitHub repository and SSH peers
  set <owner/repo>              Use a private GitHub repository for config backup
  unset                         Disable GitHub config backup
  add <name> <ssh-target>       Add or replace an SSH peer
  remove <name>                 Remove an SSH peer
  push [name] [options]         Upload config to GitHub or send a snapshot to an SSH peer
  pull [name] [options]         Restore config from GitHub or import a snapshot from an SSH peer

GitHub backup stores configuration files only: every DSH profile (package.json,
lockfiles, cordis config) plus the Pi configuration. It never stores plugin
installation state. Pull restores these files locally (existing files are
backed up first; nothing is deleted), which is the cold-start path for a new
machine.

SSH peer snapshots carry the same configuration for every profile plus the
source trees of local (link:/file:) DSH plugins listed in each profile's
package.json, so receivers can load them after a pull. Set "syncLocalSources":
false in the config to transfer only the configuration files. Machine-to-
machine sync is SSH-only.

Push options (GitHub):
  --agent all|dsh|pi            Sections to upload (default: all)
  --dry-run                     Show the files without uploading

Push options (SSH peer):
  --agent all|dsh|pi            Sections to transfer (default: all)
  --dry-run                     Show the snapshot without sending

Pull options:
  --agent all|dsh|pi            Sections to restore (default: all)
  --apply                       Apply the restore (default: plan only)
  --dry-run                     Show the plan without applying

Common options:
  --config FILE                 Config file (default: ~/.config/dshpm/config.json)
  --json                        Machine-readable output
`

type remoteOptions struct {
	commonOptions
	agent  string
	branch string
	apply  bool
	dryRun bool
}

func addRemoteFlags(set *flag.FlagSet, options *remoteOptions) {
	addCommonFlags(set, &options.commonOptions)
	set.StringVar(&options.agent, "agent", "all", "sections to transfer: all, dsh, or pi")
	set.StringVar(&options.branch, "branch", "", "GitHub branch (default: config or main)")
	set.BoolVar(&options.apply, "apply", false, "apply the pulled snapshot")
	set.BoolVar(&options.dryRun, "dry-run", false, "plan without writing")
}

func runRemote(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, remoteUsage)
		return 0
	}
	command := args[0]
	rest := args[1:]
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, remoteUsage)
		return 0
	case "list", "ls":
		return runRemoteList(rest, stdout, stderr)
	case "set":
		return runRemoteSet(rest, stdout, stderr)
	case "unset":
		return runRemoteUnset(rest, stdout, stderr)
	case "add":
		return runRemoteAdd(rest, stdout, stderr)
	case "remove", "rm":
		return runRemoteRemove(rest, stdout, stderr)
	case "push":
		return runRemotePush(rest, stdout, stderr)
	case "pull":
		return runRemotePull(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "dshpm remote: unknown command %q\n\n%s", command, remoteUsage)
		return 2
	}
}

func runRemoteList(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("remote list", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options remoteOptions
	addRemoteFlags(set, &options)
	if _, err := parseInterspersed(set, args, nil); err != nil {
		return 2
	}
	cfg, err := loadConfig(options.commonOptions)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	names := make([]string, 0, len(cfg.Peers))
	for name := range cfg.Peers {
		names = append(names, name)
	}
	sort.Strings(names)
	if options.json {
		rows := make([]map[string]string, 0, len(names)+1)
		if cfg.RemoteRepo != "" {
			rows = append(rows, map[string]string{"type": "github", "repo": cfg.RemoteRepo, "branch": cfg.RemoteBranch})
		}
		for _, name := range names {
			rows = append(rows, map[string]string{"type": "peer", "name": name, "target": cfg.Peers[name]})
		}
		writeJSON(stdout, rows)
		return 0
	}
	writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
	fmt.Fprintln(writer, "TYPE\tNAME\tTARGET")
	if cfg.RemoteRepo != "" {
		fmt.Fprintf(writer, "github\t%s\t%s\n", cfg.RemoteRepo, cfg.RemoteBranch)
	}
	for _, name := range names {
		fmt.Fprintf(writer, "peer\t%s\t%s\n", name, cfg.Peers[name])
	}
	_ = writer.Flush()
	return 0
}

func runRemoteSet(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("remote set", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options remoteOptions
	addRemoteFlags(set, &options)
	positionals, err := parseInterspersed(set, args, map[string]bool{"branch": true})
	if err != nil {
		return 2
	}
	if len(positionals) == 2 && positionals[0] == "github" {
		positionals = positionals[1:]
	}
	if len(positionals) != 1 {
		fmt.Fprintln(stderr, "dshpm remote set: usage: dshpm remote set <owner/repo> [--branch main]")
		return 2
	}
	repo := strings.TrimSpace(positionals[0])
	if err := remotesync.ValidateRepo(repo); err != nil {
		fmt.Fprintf(stderr, "dshpm remote set: %v\n", err)
		return 2
	}
	cfg, err := loadConfig(options.commonOptions)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	cfg.RemoteRepo = repo
	if options.branch != "" {
		cfg.RemoteBranch = options.branch
	}
	if cfg.RemoteBranch == "" {
		cfg.RemoteBranch = "main"
	}
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(stderr, "dshpm: save config: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "GITHUB\t%s\t%s\n", cfg.RemoteRepo, cfg.RemoteBranch)
	return 0
}

func runRemoteUnset(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("remote unset", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options remoteOptions
	addRemoteFlags(set, &options)
	if _, err := parseInterspersed(set, args, nil); err != nil {
		return 2
	}
	cfg, err := loadConfig(options.commonOptions)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	cfg.RemoteRepo = ""
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(stderr, "dshpm: save config: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "GITHUB\tunset")
	return 0
}

func runRemoteAdd(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("remote add", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options remoteOptions
	addRemoteFlags(set, &options)
	positionals, err := parseInterspersed(set, args, nil)
	if err != nil {
		return 2
	}
	if len(positionals) != 2 {
		fmt.Fprintln(stderr, "dshpm remote add: usage: dshpm remote add <name> <ssh-target>")
		return 2
	}
	name, target := positionals[0], positionals[1]
	if err := validatePeerName(name); err != nil {
		fmt.Fprintf(stderr, "dshpm remote add: %v\n", err)
		return 2
	}
	if strings.TrimSpace(target) == "" {
		fmt.Fprintln(stderr, "dshpm remote add: ssh target is empty")
		return 2
	}
	cfg, err := loadConfig(options.commonOptions)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	if cfg.Peers == nil {
		cfg.Peers = make(map[string]string)
	}
	cfg.Peers[name] = target
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(stderr, "dshpm: save config: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "PEER\t%s\t%s\n", name, target)
	return 0
}

func runRemoteRemove(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("remote remove", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options remoteOptions
	addRemoteFlags(set, &options)
	positionals, err := parseInterspersed(set, args, nil)
	if err != nil {
		return 2
	}
	if len(positionals) != 1 {
		fmt.Fprintln(stderr, "dshpm remote remove: usage: dshpm remote remove <name>")
		return 2
	}
	name := positionals[0]
	cfg, err := loadConfig(options.commonOptions)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	if _, ok := cfg.Peers[name]; !ok {
		fmt.Fprintf(stderr, "dshpm remote remove: unknown peer %q\n", name)
		return 1
	}
	delete(cfg.Peers, name)
	if err := cfg.Save(); err != nil {
		fmt.Fprintf(stderr, "dshpm: save config: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "REMOVED\t%s\n", name)
	return 0
}

func runRemotePush(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("remote push", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options remoteOptions
	addRemoteFlags(set, &options)
	positionals, err := parseInterspersed(set, args, map[string]bool{"agent": true, "profile": true, "branch": true})
	if err != nil {
		return 2
	}
	if len(positionals) > 1 {
		fmt.Fprintln(stderr, "dshpm remote push: usage: dshpm remote push [name] [options]")
		return 2
	}
	cfg, err := loadConfig(options.commonOptions)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	if len(positionals) == 0 {
		if cfg.RemoteRepo == "" {
			fmt.Fprintln(stderr, "dshpm remote push: no GitHub repository configured (run 'dshpm remote set <owner/repo>')")
			return 2
		}
		entries, err := remotesync.DryRun(cfg, options.agent)
		if err != nil {
			fmt.Fprintf(stderr, "dshpm remote push: %v\n", err)
			return 1
		}
		if options.json {
			writeJSON(stdout, entries)
		} else {
			fmt.Fprintf(stdout, "GITHUB\t%s\t%s\n", cfg.RemoteRepo, cfg.RemoteBranch)
			fmt.Fprintf(stdout, "FILES\t%d\n", len(entries))
			writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
			fmt.Fprintln(writer, "AGENT\tPROFILE\tPATH\tSIZE")
			for _, entry := range entries {
				fmt.Fprintf(writer, "%s\t%s\t%s\t%d\n", entry.Agent, entry.Profile, entry.Path, entry.Size)
			}
			_ = writer.Flush()
		}
		if options.dryRun {
			fmt.Fprintln(stdout, "Dry run: no files were uploaded.")
			return 0
		}
		client := remotesync.GitHub{GH: cfg.GH, Git: cfg.Git, Repo: cfg.RemoteRepo, Branch: cfg.RemoteBranch, CacheDir: cfg.RemoteCacheDir}
		if _, err := client.Push(context.Background(), cfg, options.agent); err != nil {
			fmt.Fprintf(stderr, "dshpm remote push: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "PUSHED\t%s\t%s\n", cfg.RemoteRepo, cfg.RemoteBranch)
		return 0
	}
	name := positionals[0]
	target, ok := cfg.Peers[name]
	if !ok {
		fmt.Fprintf(stderr, "dshpm remote push: unknown peer %q\n", name)
		return 1
	}
	snapshotDir, manifest, err := devicesync.Create(cfg, options.agent)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm remote push: %v\n", err)
		return 1
	}
	defer os.RemoveAll(snapshotDir)
	if options.json {
		writeJSON(stdout, manifest)
	} else {
		fmt.Fprintf(stdout, "SNAPSHOT\t%s\tv%d\t%d files\n", manifest.CreatedAt.Format("2006-01-02 15:04:05"), manifest.Version, len(manifest.Files))
		for _, file := range manifest.Files {
			fmt.Fprintf(stdout, "FILE\t%s\t%s\t%s\t%d\n", file.Agent, file.Profile, file.Path, file.Size)
		}
	}
	if options.dryRun {
		fmt.Fprintln(stdout, "Dry run: no files were sent.")
		return 0
	}
	client := devicesync.Client{SSH: cfg.SSH, Peer: target, RemoteRoot: cfg.RemoteRoot}
	if err := client.Push(context.Background(), snapshotDir); err != nil {
		fmt.Fprintf(stderr, "dshpm remote push: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "PUSHED\t%s\t%s\n", name, cfg.RemoteRoot)
	return 0
}

func runRemotePull(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("remote pull", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options remoteOptions
	addRemoteFlags(set, &options)
	positionals, err := parseInterspersed(set, args, map[string]bool{"agent": true, "profile": true, "branch": true})
	if err != nil {
		return 2
	}
	if len(positionals) > 1 {
		fmt.Fprintln(stderr, "dshpm remote pull: usage: dshpm remote pull [name] [options]")
		return 2
	}
	if options.apply && options.dryRun {
		fmt.Fprintln(stderr, "dshpm remote pull: --apply and --dry-run cannot be combined")
		return 2
	}
	cfg, err := loadConfig(options.commonOptions)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	if len(positionals) == 0 {
		if cfg.RemoteRepo == "" {
			fmt.Fprintln(stderr, "dshpm remote pull: no GitHub repository configured (run 'dshpm remote set <owner/repo>')")
			return 2
		}
		client := remotesync.GitHub{GH: cfg.GH, Git: cfg.Git, Repo: cfg.RemoteRepo, Branch: cfg.RemoteBranch, CacheDir: cfg.RemoteCacheDir}
		files, err := client.Fetch(context.Background(), options.agent)
		if err != nil {
			fmt.Fprintf(stderr, "dshpm remote pull: %v\n", err)
			return 1
		}
		changes, err := remotesync.Plan(cfg, options.agent, files)
		if err != nil {
			fmt.Fprintf(stderr, "dshpm remote pull: %v\n", err)
			return 1
		}
		if options.json {
			writeJSON(stdout, changes)
		} else {
			writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
			fmt.Fprintln(writer, "AGENT\tPROFILE\tPATH\tTARGET\tSTATUS")
			for _, change := range changes {
				fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", change.Agent, change.Profile, change.Path, change.Target, change.Status)
			}
			_ = writer.Flush()
		}
		if !options.apply || options.dryRun {
			fmt.Fprintln(stdout, "Plan only: rerun with --apply to restore configuration from GitHub.")
			return 0
		}
		_, backupDir, err := client.Apply(context.Background(), cfg, options.agent)
		if err != nil {
			fmt.Fprintf(stderr, "dshpm remote pull: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "APPLIED\t%s\t%s\nBACKUP\t%s\n", cfg.RemoteRepo, cfg.RemoteBranch, backupDir)
		fmt.Fprintln(stdout, "Configuration restored; install or restart the agent runtime to apply it.")
		return 0
	}
	name := positionals[0]
	target, ok := cfg.Peers[name]
	if !ok {
		fmt.Fprintf(stderr, "dshpm remote pull: unknown peer %q\n", name)
		return 1
	}
	tmp, err := os.MkdirTemp("", "dshpm-pull-*")
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	defer os.RemoveAll(tmp)
	client := devicesync.Client{SSH: cfg.SSH, Peer: target, RemoteRoot: cfg.RemoteRoot}
	if err := client.Pull(context.Background(), tmp); err != nil {
		fmt.Fprintf(stderr, "dshpm remote pull: %v\n", err)
		return 1
	}
	manifest, err := devicesync.Read(tmp)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm remote pull: %v\n", err)
		return 1
	}
	// Profile resolution happens per file; the flag no longer selects one.
	changes, err := devicesync.Plan(cfg, "", manifest)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm remote pull: %v\n", err)
		return 1
	}
	if options.json {
		writeJSON(stdout, changes)
	} else {
		writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
		fmt.Fprintln(writer, "AGENT\tPROFILE\tPATH\tTARGET\tSTATUS")
		for _, change := range changes {
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", change.Agent, change.Profile, change.Path, change.Target, change.Status)
		}
		_ = writer.Flush()
	}
	if !options.apply || options.dryRun {
		fmt.Fprintln(stdout, "Plan only: rerun with --apply to import the snapshot.")
		return 0
	}
	backupDir, err := devicesync.Apply(cfg, "", tmp, manifest)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm remote pull: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "APPLIED\t%s\nBACKUP\t%s\n", name, backupDir)
	fmt.Fprintln(stdout, "Configuration imported; install or restart the agent runtime to apply it.")
	return 0
}

func validatePeerName(name string) error {
	if name == "" || name == "." || name == ".." {
		return fmt.Errorf("invalid peer name %q", name)
	}
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, " ") {
		return fmt.Errorf("peer name %q contains an invalid character", name)
	}
	return nil
}
