package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/cinvymoe/dshpm/internal/backup"
	"github.com/cinvymoe/dshpm/internal/pi"
	"github.com/cinvymoe/dshpm/internal/profile"
)

const piUsage = `dshpm pi manages packages declared in Pi's user settings.

Usage:
  dshpm pi <command> [options]

Commands:
  list                 Show Pi packages, versions, and management status
  status               Alias for list
  manage <names...>    Mark packages as managed by dshpm
  unmanage <names...>  Mark packages as unmanaged (skipped by upgrade)
  backup               Back up Pi settings and local package files
  restore <archive>    Restore a Pi backup (plan only unless --apply)
  upgrade [names...]   Upgrade Pi packages through pi
  update [names...]    Alias for upgrade
  uninstall <names...> Remove Pi packages (keeps local sources)
  history              List Pi backups
  profiles             Show the Pi user profile and package count
  doctor               Check the Pi runtime and settings
  help                 Show this help

Common options:
  --pi-home DIR        Pi agent directory (default: ~/.pi/agent)
  --config FILE        Config file (default: ~/.config/dshpm/config.json)
  --json               Machine-readable output
  --offline            Skip remote version lookups
`

func runPi(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stdout, piUsage)
		return 0
	}
	command := args[0]
	rest := args[1:]
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, piUsage)
		return 0
	case "list", "ls", "status":
		return runPiList(rest, stdout, stderr)
	case "manage":
		return runPiManage(rest, stdout, stderr, true)
	case "unmanage":
		return runPiManage(rest, stdout, stderr, false)
	case "backup":
		return runPiBackup(rest, stdout, stderr)
	case "restore":
		return runPiRestore(rest, stdout, stderr)
	case "upgrade", "update":
		return runPiUpgrade(rest, stdout, stderr)
	case "uninstall", "remove":
		return runPiUninstall(rest, stdout, stderr)
	case "history":
		return runPiHistory(rest, stdout, stderr)
	case "profiles":
		return runPiProfiles(rest, stdout, stderr)
	case "doctor":
		return runPiDoctor(rest, stdout, stderr)
	default:
		fmt.Fprintf(stderr, "dshpm pi: unknown command %q\n\n%s", command, piUsage)
		return 2
	}
}

func newPiManager(options commonOptions) (*pi.Manager, error) {
	cfg, err := loadConfig(options)
	if err != nil {
		return nil, err
	}
	instance, err := pi.New(cfg)
	if err != nil {
		return nil, err
	}
	instance.Offline = options.offline
	return instance, nil
}

func runPiList(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("pi list", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	instance, err := newPiManager(options)
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
	fmt.Fprintf(stdout, "PROFILE pi  (%s)\n\n", instance.Home)
	writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
	fmt.Fprintln(writer, "PACKAGE\tMANAGED\tKIND\tLOCAL VERSION\tREMOTE VERSION\tCHANGES\tSTATUS")
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

func runPiManage(args []string, stdout, stderr io.Writer, managed bool) int {
	command := "manage"
	state := "MANAGED"
	if !managed {
		command = "unmanage"
		state = "UNMANAGED"
	}
	set := flag.NewFlagSet("pi "+command, flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	positionals, err := parseInterspersed(set, args, map[string]bool{"pi-home": true, "config": true})
	if err != nil {
		return 2
	}
	if len(positionals) == 0 {
		fmt.Fprintf(stderr, "dshpm pi %s: at least one package name is required\n", command)
		return 2
	}
	instance, err := newPiManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	plugins, err := instance.List(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	known := make(map[string]bool, len(plugins))
	for _, plugin := range plugins {
		known[plugin.Name] = true
	}
	for _, name := range positionals {
		if !known[name] {
			fmt.Fprintf(stderr, "dshpm pi: package %q is not declared in %s\n", name, filepath.Join(instance.Home, "settings.json"))
			return 1
		}
		instance.Config.SetManagedFor("pi", name, managed)
	}
	if err := instance.Config.Save(); err != nil {
		fmt.Fprintf(stderr, "dshpm: save config: %v\n", err)
		return 1
	}
	if options.json {
		writeJSON(stdout, map[string]any{"state": state, "packages": positionals})
		return 0
	}
	for _, name := range positionals {
		fmt.Fprintf(stdout, "%s\t%s\n", state, name)
	}
	return 0
}

func runPiBackup(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("pi backup", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	outDir := set.String("out", "", "backup root directory")
	if err := set.Parse(args); err != nil {
		return 2
	}
	instance, err := newPiManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	backupDir := *outDir
	if backupDir == "" {
		backupDir = instance.Config.BackupDir
	}
	manifest, dir, err := instance.Backup(pi.BackupOptions{BackupDir: backupDir, ToolVersion: "dshpm"})
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	if options.json {
		writeJSON(stdout, map[string]any{"path": dir, "manifest": manifest})
		return 0
	}
	fmt.Fprintf(stdout, "Backup created: %s\n", dir)
	fmt.Fprintf(stdout, "Settings files: %d\n", len(manifest.Files))
	fmt.Fprintf(stdout, "Local packages: %d\n", len(manifest.Plugins))
	return 0
}

func runPiRestore(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("pi restore", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	apply := set.Bool("apply", false, "apply the restore")
	force := set.Bool("force", false, "overwrite local changes")
	target := set.String("target-profile-dir", "", "restore into this Pi agent directory")
	positionals, err := parseInterspersed(set, args, map[string]bool{"pi-home": true, "config": true, "target-profile-dir": true})
	if err != nil {
		return 2
	}
	if len(positionals) != 1 {
		fmt.Fprintln(stderr, "dshpm pi restore: exactly one backup directory is required")
		return 2
	}
	instance, err := newPiManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	result, err := backup.Restore(backup.RestoreOptions{
		ArchiveDir:       positionals[0],
		TargetProfileDir: firstNonEmpty(*target, instance.Home),
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
	if len(result.RestoredCheckouts) > 0 {
		fmt.Fprintf(stdout, "Local packages: %s\n", strings.Join(result.RestoredCheckouts, ", "))
	}
	if len(result.Conflicts) > 0 {
		fmt.Fprintf(stdout, "Conflicts: %s\n", strings.Join(result.Conflicts, ", "))
		return 1
	}
	return 0
}

func runPiUpgrade(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("pi upgrade", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	all := set.Bool("all", false, "upgrade all packages")
	dryRun := set.Bool("dry-run", false, "plan only")
	noBackup := set.Bool("no-backup", false, "skip the pre-upgrade backup")
	positionals, err := parseInterspersed(set, args, map[string]bool{"pi-home": true, "config": true})
	if err != nil {
		return 2
	}
	instance, err := newPiManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	results, err := instance.Upgrade(context.Background(), pi.UpgradeOptions{
		All:       *all,
		Names:     positionals,
		DryRun:    *dryRun,
		NoBackup:  *noBackup,
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
	fmt.Fprintln(writer, "PACKAGE\tKIND\tFROM\tTO\tRESULT")
	failures := 0
	for _, result := range results {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", result.Name, result.Kind, result.From, result.To, piResultLabel(result.Status, result.Message))
		if result.Status == "failed" || result.Status == "error" {
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
		fmt.Fprintln(stdout, "\nRestart Pi to load upgraded packages.")
	}
	if failures > 0 {
		return 1
	}
	return 0
}

func runPiUninstall(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("pi uninstall", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	dryRun := set.Bool("dry-run", false, "plan only")
	noBackup := set.Bool("no-backup", false, "skip the pre-uninstall backup")
	positionals, err := parseInterspersed(set, args, map[string]bool{"pi-home": true, "config": true})
	if err != nil {
		return 2
	}
	if len(positionals) == 0 {
		fmt.Fprintln(stderr, "dshpm pi uninstall: at least one package name is required")
		return 2
	}
	instance, err := newPiManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	results, err := instance.Uninstall(context.Background(), pi.UninstallOptions{
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
	fmt.Fprintln(writer, "PACKAGE\tKIND\tRESULT")
	for _, result := range results {
		fmt.Fprintf(writer, "%s\t%s\t%s\n", result.Name, result.Kind, piResultLabel(result.Status, result.Message))
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
		fmt.Fprintln(stdout, "\nRestart Pi to unload removed packages.")
	}
	return 0
}

func runPiHistory(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("pi history", flag.ContinueOnError)
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
	profileName := "pi"
	if *all {
		profileName = ""
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
		fmt.Fprintln(stdout, "No Pi backups found.")
		return 0
	}
	writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tCREATED\tPROFILE\tFILES\tPLUGINS")
	for _, entry := range entries {
		fmt.Fprintf(writer, "%s\t%s\t%s\t%d\t%d\n", entry.ID, entry.Manifest.CreatedAt, entry.Manifest.Profile, len(entry.Manifest.Files), len(entry.Manifest.Plugins))
	}
	_ = writer.Flush()
	return 0
}

func runPiProfiles(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("pi profiles", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	instance, err := newPiManager(options)
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	count, err := instance.PackageCount()
	if err != nil {
		fmt.Fprintf(stderr, "dshpm: %v\n", err)
		return 1
	}
	type row struct {
		Name     string `json:"name"`
		Home     string `json:"home"`
		Packages int    `json:"packages"`
	}
	data := row{Name: "user", Home: instance.Home, Packages: count}
	if options.json {
		writeJSON(stdout, data)
		return 0
	}
	writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
	fmt.Fprintln(writer, "PROFILE\tHOME\tPACKAGES")
	fmt.Fprintf(writer, "%s\t%s\t%d\n", data.Name, data.Home, data.Packages)
	_ = writer.Flush()
	return 0
}

func runPiDoctor(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("pi doctor", flag.ContinueOnError)
	set.SetOutput(stderr)
	var options commonOptions
	addCommonFlags(set, &options)
	if err := set.Parse(args); err != nil {
		return 2
	}
	instance, err := newPiManager(options)
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
	fmt.Fprintln(writer, "CHECK\tSTATUS\tMESSAGE")
	failures := 0
	for _, check := range checks {
		fmt.Fprintf(writer, "%s\t%s\t%s\n", check.Name, check.Status, check.Message)
		if check.Status == "error" {
			failures++
		}
	}
	_ = writer.Flush()
	if failures > 0 {
		return 1
	}
	return 0
}

func piResultLabel(status, message string) string {
	if message == "" {
		return status
	}
	return status + " (" + message + ")"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func runAgents(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet("agents", flag.ContinueOnError)
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
	type row struct {
		Name     string `json:"name"`
		Home     string `json:"home"`
		Detected bool   `json:"detected"`
		Packages int    `json:"packages"`
	}
	rows := []row{{Name: "dsh", Home: cfg.DSHHome}}
	if entries, err := os.ReadDir(filepath.Join(cfg.DSHHome, "profiles")); err == nil {
		rows[0].Detected = true
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			if loaded, err := profile.Load(cfg.DSHHome, entry.Name()); err == nil {
				rows[0].Packages += len(loaded.Dependencies)
			}
		}
	}
	piRow := row{Name: "pi", Home: cfg.PiHome}
	if instance, err := pi.New(cfg); err == nil {
		if count, err := instance.PackageCount(); err == nil {
			piRow.Detected = true
			piRow.Packages = count
		}
	}
	rows = append(rows, piRow)
	if options.json {
		writeJSON(stdout, rows)
		return 0
	}
	writer := tabwriter.NewWriter(stdout, 2, 2, 2, ' ', 0)
	fmt.Fprintln(writer, "AGENT\tHOME\tDETECTED\tPACKAGES")
	for _, item := range rows {
		fmt.Fprintf(writer, "%s\t%s\t%t\t%d\n", item.Name, item.Home, item.Detected, item.Packages)
	}
	_ = writer.Flush()
	return 0
}
