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
	"github.com/cinvymoe/dshpm/internal/publish"
	"github.com/cinvymoe/dshpm/internal/version"
)

const usage = `dshpm manages plugins installed in a DeepSeek Harness profile.

Usage:
  dshpm <command> [options]

Commands:
  list                 Show plugins, versions, and management status
  status               Alias for list
  manage <names...>    Mark plugins as managed by dshpm
  unmanage <names...>  Mark plugins as unmanaged (skipped by update/sync/upgrade)
  backup               Back up profile manifests and local plugin patches
  restore <archive>    Restore a backup (plan only unless --apply)
  uninstall <names...> Remove plugins from the profile (keeps local sources)
  remove               Alias for uninstall
  history              List backups for the selected profile
  rollback [id|path]   Restore a historical backup (plan unless --apply)
  publish [names...]   Publish local plugins to GitHub through gh
  sync [names...]      Sync local git checkouts with remote (rebase/worktree)
  upgrade [names...]   Upgrade plugins; choose --build-source auto|worktree|local
  update [names...]    Update plugins with the legacy patch-based flow
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
	case "manage":
		return runManage(rest, stdout, stderr, true)
	case "unmanage":
		return runManage(rest, stdout, stderr, false)
	case "backup":
		return runBackup(rest, stdout, stderr)
	case "restore":
		return runRestore(rest, stdout, stderr)
	case "uninstall", "remove":
		return runUninstall(rest, stdout, stderr)
	case "history":
		return runHistory(rest, stdout, stderr)
	case "rollback":
		return runRollback(rest, stdout, stderr)
	case "publish":
		return runPublish(rest, stdout, stderr)
	case "sync":
		return runSync(rest, stdout, stderr)
	case "upgrade":
		return runUpgrade(rest, stdout, stderr)
	case "update":
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
	fmt.Fprintln(writer, "PLUGIN\tMANAGED\tKIND\tLOCAL VERSION\tREMOTE VERSION\tCHANGES\tSTATUS")
	for _, plugin := range plugins {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			plugin.Name,
			managedLabel(plugin),
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

func runManage(args []string, stdout, stderr io.Writer, managed bool) int {
	command := "manage"
	state := "MANAGED"
	if !managed {
		command = "unmanage"
		state = "UNMANAGED"
	}
	set := flag.NewFlagSet(command, flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	positionals, err := parseInterspersed(set, args, map[string]bool{"profile": true, "dsh-home": true, "config": true})
	if err != nil {
		return 2
	}
	if len(positionals) == 0 {
		fmt.Fprintf(stderr, "dshpm %s: at least one plugin name is required\n", command)
		return 2
	}
	instance, err := newManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	for _, name := range positionals {
		if _, ok := instance.Profile.Dependencies[name]; !ok {
			fmt.Fprintf(stderr, "dshpm %s: plugin %q is not a dependency of profile %s\n", command, name, instance.Profile.Name)
			return 1
		}
		instance.Config.SetManaged(name, managed)
	}
	if err := instance.Config.Save(); err != nil {
		fmt.Fprintf(stderr, "dshpm: save config: %v\n", err)
		return 1
	}
	if options.json {
		writeJSON(stdout, map[string]any{"profile": instance.Profile.Name, "managed": managed, "plugins": positionals})
		return 0
	}
	for _, name := range positionals {
		fmt.Fprintf(stdout, "%s\t%s\n", state, name)
	}
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
	if plugin.WorktreeUsed {
		ref := plugin.WorktreeRef
		if ref == "" {
			ref = "remote"
		}
		parts = append(parts, "worktree:"+ref)
	}
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

func managedLabel(plugin model.Plugin) string {
	if plugin.Managed {
		return "yes"
	}
	return "no"
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

func runSync(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("sync", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	all := set.Bool("all", false, "sync all local checkouts")
	dryRun := set.Bool("dry-run", false, "plan only")
	noBackup := set.Bool("no-backup", false, "skip the pre-sync backup")
	worktreeRoot := set.String("worktree-root", "", "worktree root directory")
	positionals, err := parseInterspersed(set, args, map[string]bool{"profile": true, "dsh-home": true, "config": true, "worktree-root": true})
	if err != nil {
		return 2
	}
	instance, err := newManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	results, err := instance.Sync(context.Background(), manager.SyncOptions{
		All:          *all,
		Names:        positionals,
		DryRun:       *dryRun,
		NoBackup:     *noBackup,
		BackupDir:    instance.Config.BackupDir,
		WorktreeRoot: *worktreeRoot,
		Out:          stdout,
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
	fmt.Fprintln(writer, "PLUGIN\tKIND\tFROM\tTO\tACTION\tRESULT\tWORKTREE")
	failures := 0
	for _, result := range results {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			result.Name, result.Kind, result.From, result.To, result.Action,
			syncResultLabel(result), result.WorktreeDir)
		if result.Status == "failed" || result.Status == "error" || result.Status == "blocked" {
			failures++
		}
	}
	_ = writer.Flush()
	for _, result := range results {
		if result.BackupPath != "" {
			fmt.Fprintf(stdout, "\nBackup: %s\n", result.BackupPath)
			break
		}
	}
	if *dryRun {
		fmt.Fprintln(stdout, "\nDry run: no files were changed.")
	} else {
		fmt.Fprintln(stdout, "\nSync complete. Restart DSH if plugin sources changed.")
	}
	if failures > 0 {
		return 1
	}
	return 0
}

func syncResultLabel(result manager.SyncResult) string {
	if result.Message == "" {
		return result.Status
	}
	message := strings.ReplaceAll(result.Message, "\n", " ")
	if len(message) > 140 {
		message = message[:137] + "..."
	}
	return result.Status + " (" + message + ")"
}

func runUpgrade(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("upgrade", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	all := set.Bool("all", false, "upgrade all plugins")
	dryRun := set.Bool("dry-run", false, "plan only")
	noBackup := set.Bool("no-backup", false, "skip the pre-upgrade backup")
	noBuild := set.Bool("no-build", false, "skip building local checkouts")
	buildSource := set.String("build-source", string(manager.BuildSourceAuto), "local build source: auto, worktree, or local")
	worktreeRoot := set.String("worktree-root", "", "worktree root directory")
	positionals, err := parseInterspersed(set, args, map[string]bool{"profile": true, "dsh-home": true, "config": true, "build-source": true, "worktree-root": true})
	if err != nil {
		return 2
	}
	instance, err := newManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	results, err := instance.Upgrade(context.Background(), manager.UpgradeOptions{
		All:          *all,
		Names:        positionals,
		DryRun:       *dryRun,
		NoBackup:     *noBackup,
		NoBuild:      *noBuild,
		BuildSource:  manager.LocalBuildSource(*buildSource),
		BackupDir:    instance.Config.BackupDir,
		WorktreeRoot: *worktreeRoot,
		Out:          stdout,
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
	fmt.Fprintln(writer, "PLUGIN\tKIND\tFROM\tTO\tBUILD\tRESULT\tWORKTREE")
	failures := 0
	for _, result := range results {
		build := string(result.BuildSource)
		if build == "" {
			build = "-"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			result.Name, result.Kind, result.From, result.To, build, resultLabel(result), result.WorktreeDir)
		switch result.Status {
		case "failed", "error", "build-failed", "rebase-blocked", "rebase-failed", "worktree-missing":
			failures++
		}
	}
	_ = writer.Flush()
	for _, result := range results {
		if result.BackupPath != "" {
			fmt.Fprintf(stdout, "\nBackup: %s\n", result.BackupPath)
			break
		}
	}
	if *dryRun {
		fmt.Fprintln(stdout, "\nDry run: no files were changed.")
	} else {
		fmt.Fprintln(stdout, "\nRestart DSH to load upgraded plugins.")
	}
	if failures > 0 {
		return 1
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
		WorktreeRoot:   filepath.Join("/data", "project", "dshpm-worktrees"),
		NPMRegistry:    "https://registry.npmjs.org",
		PNPM:           "pnpm",
		Git:            "git",
		GH:             "gh",
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

func runUninstall(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	dryRun := set.Bool("dry-run", false, "plan only")
	noBackup := set.Bool("no-backup", false, "skip the pre-uninstall backup")
	positionals, err := parseInterspersed(set, args, map[string]bool{"profile": true, "dsh-home": true, "config": true})
	if err != nil {
		return 2
	}
	if len(positionals) == 0 {
		fmt.Fprintln(stderr, "dshpm uninstall: at least one plugin name is required")
		return 2
	}
	instance, err := newManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	results, err := instance.Uninstall(context.Background(), manager.UninstallOptions{
		Names:     positionals,
		DryRun:    *dryRun,
		NoBackup:  *noBackup,
		BackupDir: instance.Config.BackupDir,
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
	fmt.Fprintln(writer, "PLUGIN\tKIND\tRESULT")
	for _, result := range results {
		fmt.Fprintf(writer, "%s\t%s\t%s\n", result.Name, result.Kind, result.Status)
	}
	_ = writer.Flush()
	if len(results) > 0 && results[0].BackupPath != "" {
		fmt.Fprintf(stdout, "\nBackup: %s\n", results[0].BackupPath)
	}
	if *dryRun {
		fmt.Fprintln(stdout, "\nDry run: no files were changed.")
	} else {
		if *noBackup {
			fmt.Fprintln(stdout, "\nWarning: pre-uninstall backup was skipped.")
		}
		fmt.Fprintln(stdout, "Local source directories were kept. Restart DSH to unload the plugins.")
	}
	return 0
}

func runHistory(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("history", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	all := set.Bool("all", false, "include every profile")
	limit := set.Int("limit", 20, "maximum entries to show")
	if err := set.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadConfig(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	profileName := ""
	if !*all {
		profileName, err = cfg.ResolveProfileName(options.profile)
		if err != nil {
			fmt.Fprintf(stderr, "dshpm: %v\n", err)
			return 1
		}
	}
	entries, err := backup.List(cfg.BackupDir, profileName)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	if *limit > 0 && len(entries) > *limit {
		entries = entries[:*limit]
	}
	if options.json {
		writeJSON(stdout, entries)
		return 0
	}
	if len(entries) == 0 {
		if *all {
			fmt.Fprintln(stdout, "No backups found.")
		} else {
			fmt.Fprintf(stdout, "No backups found for profile %s.\n", profileName)
		}
		return 0
	}
	writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tCREATED\tPROFILE\tFILES\tPLUGINS\tPATCHES")
	for _, entry := range entries {
		patches := 0
		for _, plugin := range entry.Manifest.Plugins {
			if plugin.PatchFile != "" {
				patches++
			}
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%d\t%d\n",
			entry.ID,
			entry.Manifest.CreatedAt,
			entry.Manifest.Profile,
			len(entry.Manifest.Files),
			len(entry.Manifest.Plugins),
			patches,
		)
	}
	_ = writer.Flush()
	return 0
}

func runRollback(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("rollback", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	apply := set.Bool("apply", false, "apply the restore")
	force := set.Bool("force", false, "overwrite local changes")
	noInstall := set.Bool("no-install", false, "skip pnpm install after restoring")
	noBackup := set.Bool("no-backup", false, "skip the checkpoint backup")
	positionals, err := parseInterspersed(set, args, map[string]bool{"profile": true, "dsh-home": true, "config": true})
	if err != nil {
		return 2
	}
	if len(positionals) > 1 {
		fmt.Fprintln(stderr, "dshpm rollback: at most one backup id or path is allowed")
		return 2
	}
	instance, err := newManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	var entry *backup.Entry
	if len(positionals) == 0 {
		entry, err = backup.Latest(instance.Config.BackupDir, instance.Profile.Name)
	} else {
		entry, err = backup.Find(instance.Config.BackupDir, instance.Profile.Name, positionals[0])
	}
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	if entry.Manifest.Profile != instance.Profile.Name {
		fmt.Fprintf(stderr, "dshpm: backup belongs to profile %s, not %s\n", entry.Manifest.Profile, instance.Profile.Name)
		return 1
	}
	if !*apply {
		result, err := backup.Restore(backup.RestoreOptions{ArchiveDir: entry.Path, Apply: false})
		if err != nil {
			fmt.Fprintf(stderr, "dshpm: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "PLAN (pass --apply to roll back) %s\n", entry.ID)
		fmt.Fprintf(stdout, "Profile: %s\n", result.Manifest.Profile)
		if len(result.AppliedPatches) > 0 {
			fmt.Fprintf(stdout, "Patches: %s\n", strings.Join(result.AppliedPatches, ", "))
		}
		if len(result.RestoredCheckouts) > 0 {
			fmt.Fprintf(stdout, "Plugin files: %s\n", strings.Join(result.RestoredCheckouts, ", "))
		}
		return 0
	}

	checkpoint := ""
	if !*noBackup {
		_, dir, err := backup.Create(backup.CreateOptions{
			Profile:     instance.Profile,
			BackupDir:   instance.Config.BackupDir,
			ToolVersion: version.Version,
		})
		if err != nil {
			fmt.Fprintf(stderr, "dshpm: checkpoint backup failed: %v\n", err)
			return 1
		}
		checkpoint = dir
	}
	result, err := backup.Restore(backup.RestoreOptions{
		ArchiveDir:       entry.Path,
		Apply:            true,
		Force:            *force,
		Out:              stdout,
		TargetProfileDir: instance.Profile.Dir,
	})
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "RESTORED %s\n", entry.ID)
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
	if len(result.Conflicts) > 0 {
		fmt.Fprintf(stdout, "Conflicts: %s\n", strings.Join(result.Conflicts, ", "))
		return 1
	}
	if checkpoint != "" {
		fmt.Fprintf(stdout, "Checkpoint: %s\n", checkpoint)
	}
	if !*noInstall {
		if err := instance.Install(context.Background()); err != nil {
			fmt.Fprintf(stderr, "dshpm: install restored dependencies: %v\n", err)
			return 1
		}
		fmt.Fprintln(stdout, "Dependencies installed.")
	}
	fmt.Fprintln(stdout, "Restart DSH to apply the restored state.")
	return 0
}

type publishOutcome struct {
	Result      *publish.Result `json:"result,omitempty"`
	Switch      string          `json:"switch,omitempty"`
	SwitchError string          `json:"switchError,omitempty"`
	Error       string          `json:"error,omitempty"`
}

func runPublish(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("publish", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	allLocal := set.Bool("all-local", false, "publish every local plugin")
	visibility := set.String("visibility", "private", "GitHub visibility: private, public, or internal")
	repo := set.String("repo", "", "GitHub repository owner/name (single plugin only)")
	owner := set.String("owner", "", "GitHub owner (default: authenticated gh user)")
	branch := set.String("branch", "main", "branch to push")
	remote := set.String("remote", "", "git remote name (default: origin or dshpm)")
	message := set.String("message", "", "commit message for pending changes")
	tag := set.Bool("tag", false, "create and push a v<version> tag")
	release := set.Bool("release", false, "create a GitHub release (implies --tag)")
	dryRun := set.Bool("dry-run", false, "plan only")
	force := set.Bool("force", false, "allow large files, remote replacement, or a bundle patch missing from files")
	noBackup := set.Bool("no-backup", false, "skip the pre-publish backup")
	noSwitch := set.Bool("no-switch", false, "keep the local link instead of switching to the published repository")
	positionals, err := parseInterspersed(set, args, map[string]bool{"profile": true, "dsh-home": true, "config": true, "visibility": true, "repo": true, "owner": true, "branch": true, "remote": true, "message": true})
	if err != nil {
		return 2
	}
	if *allLocal && len(positionals) > 0 {
		fmt.Fprintln(stderr, "dshpm publish: --all-local cannot be combined with plugin names")
		return 2
	}
	instance, err := newManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	selected, err := selectLocalPlugins(instance.Profile.Plugins(), positionals, *allLocal)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	if *repo != "" && len(selected) > 1 {
		fmt.Fprintln(stderr, "dshpm publish: --repo can only be used with one plugin")
		return 2
	}
	if !*dryRun && !*noBackup {
		_, dir, err := backup.Create(backup.CreateOptions{
			Profile:     instance.Profile,
			BackupDir:   instance.Config.BackupDir,
			ToolVersion: version.Version,
		})
		if err != nil {
			fmt.Fprintf(stderr, "dshpm: pre-publish backup failed: %v\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "Backup: %s\n", dir)
	}
	outcomes := make([]publishOutcome, 0, len(selected))
	exit := 0
	for _, plugin := range selected {
		result, err := publish.Publish(context.Background(), publish.Options{
			Dir:           plugin.LocalDir,
			Repo:          *repo,
			Owner:         *owner,
			Visibility:    *visibility,
			Branch:        *branch,
			Remote:        *remote,
			CommitMessage: *message,
			Tag:           *tag || *release,
			Release:       *release,
			DryRun:        *dryRun,
			Force:         *force,
			Git:           instance.Config.Git,
			GH:            instance.Config.GH,
		})
		if err != nil {
			outcomes = append(outcomes, publishOutcome{Error: err.Error()})
			fmt.Fprintf(stderr, "dshpm: publish %s: %v\n", plugin.Name, err)
			exit = 1
			continue
		}
		outcome := publishOutcome{Result: result, Switch: "remote"}
		if *noSwitch {
			outcome.Switch = "kept-local"
		} else if *dryRun {
			outcome.Switch = "planned"
		} else if err := instance.SwitchToGit(context.Background(), plugin.Name, result.Repo); err != nil {
			outcome.Switch = "failed"
			outcome.SwitchError = err.Error()
			fmt.Fprintf(stderr, "dshpm: switch %s to remote: %v\n", plugin.Name, err)
			exit = 1
		}
		outcomes = append(outcomes, outcome)
	}
	if options.json {
		writeJSON(stdout, outcomes)
		return exit
	}
	writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
	fmt.Fprintln(writer, "PLUGIN\tREPO\tVISIBILITY\tSTATUS\tSWITCH\tURL")
	for _, outcome := range outcomes {
		if outcome.Result == nil {
			continue
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\n", outcome.Result.Plugin, outcome.Result.Repo, outcome.Result.Visibility, outcome.Result.Status, outcome.Switch, outcome.Result.URL)
	}
	_ = writer.Flush()
	if *dryRun {
		fmt.Fprintln(stdout, "\nDry run: no files or repositories were changed.")
	}
	return exit
}

func selectLocalPlugins(plugins []model.Plugin, names []string, all bool) ([]model.Plugin, error) {
	if all {
		selected := make([]model.Plugin, 0)
		for _, plugin := range plugins {
			if plugin.Kind == model.SourceLocal {
				selected = append(selected, plugin)
			}
		}
		if len(selected) == 0 {
			return nil, fmt.Errorf("no local plugins found")
		}
		return selected, nil
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("choose local plugins by name or pass --all-local")
	}
	wanted := make(map[string]bool, len(names))
	for _, name := range names {
		wanted[name] = true
	}
	selected := make([]model.Plugin, 0, len(names))
	for _, plugin := range plugins {
		if !wanted[plugin.Name] {
			continue
		}
		if plugin.Kind != model.SourceLocal {
			return nil, fmt.Errorf("plugin %s is %s, not a local plugin", plugin.Name, plugin.Kind)
		}
		selected = append(selected, plugin)
		delete(wanted, plugin.Name)
	}
	if len(wanted) > 0 {
		missing := make([]string, 0, len(wanted))
		for name := range wanted {
			missing = append(missing, name)
		}
		return nil, fmt.Errorf("unknown plugin(s): %s", strings.Join(missing, ", "))
	}
	return selected, nil
}
