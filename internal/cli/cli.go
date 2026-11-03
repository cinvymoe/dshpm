// Package cli implements the dshpm command line interface.
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/cinvymoe/dshpm/internal/backup"
	"github.com/cinvymoe/dshpm/internal/config"
	"github.com/cinvymoe/dshpm/internal/manager"
	"github.com/cinvymoe/dshpm/internal/model"
	"github.com/cinvymoe/dshpm/internal/version"
)

const usage = `dshpm manages plugins installed in a DeepSeek Harness profile.

Usage:
  dshpm <command> [options]

Commands:
  list                 Show plugins with local and remote versions
  status               Alias for list
  backup               Back up profile manifests and local plugin patches
  restore <archive>    Restore a backup (plan only unless --apply)
  update [names...]    Update plugins from their remote source
  sync                 Alias for update
  profiles             List DSH profiles and dependency counts
  doctor               Check the runtime environment
  init                 Write a default config file
  version              Print the dshpm version
  help                 Show this help

Common options:
  --profile NAME       DSH profile (default: config, DSHPM_PROFILE, or web)
  --dsh-home DIR       DSH home (default: ~/.dsh)
  --config FILE        Config file (default: ~/.config/dshpm/config.json)
  --json               Machine-readable output
  --offline            Skip remote version lookups
`

// Run executes one CLI invocation and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return 0
	}
	command := args[0]
	rest := args[1:]
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "dshpm %s (%s)\n", version.Version, version.Commit)
		return 0
	case "list", "ls", "status":
		return runList(rest, stdout, stderr)
	case "backup":
		return runBackup(rest, stdout, stderr)
	case "restore":
		return runRestore(rest, stdout, stderr)
	case "update", "sync":
		return runUpdate(rest, stdout, stderr)
	case "profiles":
		return runProfiles(rest, stdout, stderr)
	case "doctor":
		return runDoctor(rest, stdout, stderr)
	case "init":
		return runInit(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "dshpm: unknown command %q\n\n%s", command, usage)
		return 2
	}
}

type commonOptions struct {
	profile string
	dshHome string
	config  string
	json    bool
	offline bool
}

func addCommonFlags(set *flag.FlagSet, options *commonOptions) {
	set.StringVar(&options.profile, "profile", "", "DSH profile name")
	set.StringVar(&options.dshHome, "dsh-home", "", "DSH home directory")
	set.StringVar(&options.config, "config", "", "config file path")
	set.BoolVar(&options.json, "json", false, "machine-readable output")
	set.BoolVar(&options.offline, "offline", false, "skip remote lookups")
}

func loadConfig(options commonOptions) (config.Resolved, error) {
	cfg, err := config.LoadFile(options.config)
	if err != nil {
		return config.Resolved{}, err
	}
	if options.dshHome != "" {
		cfg.DSHHome = options.dshHome
	}
	if options.config != "" {
		cfg.ConfigPath = options.config
	}
	return cfg, nil
}

func newManager(options commonOptions) (*manager.Manager, error) {
	cfg, err := loadConfig(options)
	if err != nil {
		return nil, err
	}
	instance, err := manager.New(cfg, options.profile)
	if err != nil {
		return nil, err
	}
	instance.Offline = options.offline
	return instance, nil
}

func runList(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("list", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	instance, err := newManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	plugins, err := instance.List(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	if options.json {
		writeJSON(stdout, plugins)
		return 0
	}
	fmt.Fprintf(stdout, "PROFILE %s  (%s)\n\n", instance.Profile.Name, instance.Profile.Dir)
	writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
	fmt.Fprintln(writer, "PLUGIN\tKIND\tLOCAL VERSION\tREMOTE VERSION\tCHANGES\tSTATUS")
	for _, plugin := range plugins {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n",
			plugin.Name,
			plugin.Kind,
			localVersion(plugin),
			remoteVersion(plugin),
			changes(plugin),
			statusLabel(plugin),
		)
	}
	_ = writer.Flush()
	return 0
}

func localVersion(plugin model.Plugin) string {
	label := plugin.InstalledVersion
	if plugin.LocalCommit != "" {
		if label != "" {
			label += "@"
		}
		label += shortCommit(plugin.LocalCommit)
	}
	if label == "" {
		return "-"
	}
	return label
}

func remoteVersion(plugin model.Plugin) string {
	if plugin.RemoteVersion == "" && plugin.RemoteCommit == "" {
		return "-"
	}
	label := plugin.RemoteVersion
	if plugin.RemoteCommit != "" {
		if label != "" {
			label += "@"
		}
		label += shortCommit(plugin.RemoteCommit)
	}
	if label == "" {
		return "-"
	}
	return label
}

func shortCommit(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}

func changes(plugin model.Plugin) string {
	parts := []string{}
	if plugin.Dirty > 0 {
		parts = append(parts, fmt.Sprintf("dirty:%d", plugin.Dirty))
	}
	if plugin.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("ahead:%d", plugin.Ahead))
	}
	if plugin.Behind > 0 {
		parts = append(parts, fmt.Sprintf("behind:%d", plugin.Behind))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, " ")
}

func statusLabel(plugin model.Plugin) string {
	if plugin.Status == "" {
		return "unknown"
	}
	if plugin.Error != "" {
		return plugin.Status + " (" + plugin.Error + ")"
	}
	return plugin.Status
}

func runBackup(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("backup", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	outDir := set.String("out", "", "backup root directory")
	if err := set.Parse(args); err != nil {
		return 2
	}
	instance, err := newManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	backupDir := *outDir
	if backupDir == "" {
		backupDir = instance.Config.BackupDir
	}
	manifest, dir, err := backup.Create(backup.CreateOptions{
		Profile:     instance.Profile,
		BackupDir:   backupDir,
		ToolVersion: version.Version,
		Now:         time.Now(),
	})
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	if options.json {
		writeJSON(stdout, map[string]any{"path": dir, "manifest": manifest})
		return 0
	}
	fmt.Fprintf(stdout, "Backup created: %s\n", dir)
	fmt.Fprintf(stdout, "Profile files: %d\n", len(manifest.Files))
	patched := 0
	for _, plugin := range manifest.Plugins {
		if plugin.PatchFile != "" {
			patched++
			fmt.Fprintf(stdout, "  patch %s (%d bytes)\n", plugin.Name, plugin.PatchSize)
		}
	}
	fmt.Fprintf(stdout, "Local checkouts: %d, with patches: %d\n", len(manifest.Plugins), patched)
	return 0
}

func runRestore(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("restore", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	apply := set.Bool("apply", false, "apply the restore")
	force := set.Bool("force", false, "overwrite local changes")
	target := set.String("target-profile-dir", "", "restore into this profile directory")
	positionals, err := parseInterspersed(set, args, map[string]bool{"profile": true, "dsh-home": true, "config": true, "target-profile-dir": true})
	if err != nil {
		return 2
	}
	if len(positionals) != 1 {
		fmt.Fprintln(stderr, "dshpm restore: exactly one backup directory is required")
		return 2
	}
	result, err := backup.Restore(backup.RestoreOptions{
		ArchiveDir:       positionals[0],
		TargetProfileDir: *target,
		Apply:            *apply,
		Force:            *force,
		Out:              stdout,
	})
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	if options.json {
		writeJSON(stdout, result)
		return 0
	}
	mode := "PLAN (pass --apply to restore)"
	if *apply {
		mode = "RESTORED"
	}
	fmt.Fprintf(stdout, "%s %s\n", mode, positionals[0])
	fmt.Fprintf(stdout, "Profile: %s\n", result.Manifest.Profile)
	if len(result.RestoredFiles) > 0 {
		fmt.Fprintf(stdout, "Files: %s\n", strings.Join(result.RestoredFiles, ", "))
	}
	if len(result.AppliedPatches) > 0 {
		fmt.Fprintf(stdout, "Patches: %s\n", strings.Join(result.AppliedPatches, ", "))
	}
	if len(result.RestoredCheckouts) > 0 {
		fmt.Fprintf(stdout, "Plugin files: %s\n", strings.Join(result.RestoredCheckouts, ", "))
	}
	if len(result.Skipped) > 0 {
		fmt.Fprintf(stdout, "Skipped: %s\n", strings.Join(result.Skipped, ", "))
	}
	if len(result.Conflicts) > 0 {
		fmt.Fprintf(stdout, "Conflicts: %s\n", strings.Join(result.Conflicts, ", "))
		return 1
	}
	if *apply {
		fmt.Fprintln(stdout, "Restart DSH to apply the restored state.")
	}
	return 0
}

func runUpdate(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("update", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	all := set.Bool("all", false, "update all plugins")
	dryRun := set.Bool("dry-run", false, "plan only")
	noBackup := set.Bool("no-backup", false, "skip the pre-update backup")
	noBuild := set.Bool("no-build", false, "skip building local checkouts")
	positionals, err := parseInterspersed(set, args, map[string]bool{"profile": true, "dsh-home": true, "config": true})
	if err != nil {
		return 2
	}
	instance, err := newManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	results, err := instance.Update(context.Background(), manager.UpdateOptions{
		All:       *all,
		Names:     positionals,
		DryRun:    *dryRun,
		NoBackup:  *noBackup,
		NoBuild:   *noBuild,
		BackupDir: instance.Config.BackupDir,
		Out:       stdout,
	})
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	if options.json {
		writeJSON(stdout, results)
		return 0
	}
	writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
	fmt.Fprintln(writer, "PLUGIN\tKIND\tFROM\tTO\tRESULT")
	failures := 0
	for _, result := range results {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", result.Name, result.Kind, result.From, result.To, resultLabel(result))
		if result.Status == "failed" || result.Status == "error" || result.Status == "conflict" || result.Status == "build-failed" {
			failures++
		}
	}
	_ = writer.Flush()
	if len(results) > 0 && results[0].BackupPath != "" {
		fmt.Fprintf(stdout, "\nBackup: %s\n", results[0].BackupPath)
	}
	if *dryRun {
		fmt.Fprintln(stdout, "\nDry run: no files were changed.")
	} else {
		fmt.Fprintln(stdout, "\nRestart DSH to load updated plugins.")
	}
	if failures > 0 {
		return 1
	}
	return 0
}

func resultLabel(result manager.UpdateResult) string {
	if result.Message == "" {
		return result.Status
	}
	return result.Status + " (" + result.Message + ")"
}

// parseInterspersed parses flags that may appear before or after positional
// arguments. valueFlags names flags that consume the following argument.
func parseInterspersed(set *flag.FlagSet, args []string, valueFlags map[string]bool) ([]string, error) {
	var flagArgs, positional []string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		if arg == "--" {
			positional = append(positional, args[index+1:]...)
			break
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			flagArgs = append(flagArgs, arg)
			name := strings.TrimLeft(arg, "-")
			if equals := strings.IndexByte(name, '='); equals >= 0 {
				name = name[:equals]
			}
			if valueFlags[name] && !strings.Contains(arg, "=") && index+1 < len(args) {
				index++
				flagArgs = append(flagArgs, args[index])
			}
			continue
		}
		positional = append(positional, arg)
	}
	if err := set.Parse(flagArgs); err != nil {
		return nil, err
	}
	return positional, nil
}

func runProfiles(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("profiles", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfig(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	entries, err := os.ReadDir(filepath.Join(cfg.DSHHome, "profiles"))
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	type row struct {
		Name string `json:"name"`
		Deps int    `json:"dependencies"`
	}
	rows := []row{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		loaded, err := profileLoad(cfg.DSHHome, entry.Name())
		if err != nil {
			continue
		}
		rows = append(rows, row{Name: entry.Name(), Deps: len(loaded.Dependencies)})
	}
	if options.json {
		writeJSON(stdout, rows)
		return 0
	}
	writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
	fmt.Fprintln(writer, "PROFILE\tDEPENDENCIES")
	for _, row := range rows {
		fmt.Fprintf(writer, "%s\t%d\n", row.Name, row.Deps)
	}
	_ = writer.Flush()
	return 0
}

func profileLoad(dshHome, name string) (*profileProfile, error) {
	return loadProfile(dshHome, name)
}

type profileProfile struct {
	Dependencies map[string]string
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("doctor", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	instance, err := newManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	checks := instance.Doctor()
	if options.json {
		writeJSON(stdout, checks)
		return 0
	}
	writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
	fmt.Fprintln(writer, "CHECK\tSTATUS\tDETAIL")
	failed := false
	for _, check := range checks {
		status := "ok"
		if !check.OK {
			status = "FAIL"
			failed = true
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\n", check.Name, status, check.Detail)
	}
	_ = writer.Flush()
	if failed {
		return 1
	}
	return 0
}

func runInit(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("init", flag.ContinueOnError)
	set.SetOutput(stderr)
	force := set.Bool("force", false, "overwrite an existing config")
	if err := set.Parse(args); err != nil {
		return 2
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	path := filepath.Join(home, ".config", "dshpm", "config.json")
	if _, err := os.Stat(path); err == nil && !*force {
		fmt.Fprintf(stderr, "dshpm: %s already exists (use --force)\n", path)
		return 1
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	cfg := config.Config{
		DSHHome:        filepath.Join(home, ".dsh"),
		DefaultProfile: "web",
		BackupDir:      filepath.Join(home, ".config", "dshpm", "backups"),
		NPMRegistry:    "https://registry.npmjs.org",
		PNPM:           "pnpm",
		Git:            "git",
	}
	data, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Wrote %s\n", path)
	return 0
}

func writeJSON(writer io.Writer, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintf(writer, "{\"error\":%q}\n", err.Error())
		return
	}
	fmt.Fprintln(writer, string(data))
}
