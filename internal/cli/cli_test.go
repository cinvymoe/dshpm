package cli

import (
	"flag"
	"reflect"
	"testing"
)

func TestParseInterspersed(t *testing.T) {
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	profile := set.String("profile", "", "profile")
	dryRun := set.Bool("dry-run", false, "dry run")
	positionals, err := parseInterspersed(set, []string{"pkg-a", "--profile", "web", "pkg-b", "--dry-run"}, map[string]bool{"profile": true})
	if err != nil {
		t.Fatal(err)
	}
	if *profile != "web" || !*dryRun {
		t.Fatalf("profile=%q dryRun=%v", *profile, *dryRun)
	}
	if !reflect.DeepEqual(positionals, []string{"pkg-a", "pkg-b"}) {
		t.Fatalf("positionals=%v", positionals)
	}
}

func TestParseInterspersedDoubleDash(t *testing.T) {
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	positionals, err := parseInterspersed(set, []string{"--", "--profile", "web"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(positionals, []string{"--profile", "web"}) {
		t.Fatalf("positionals=%v", positionals)
	}
}
