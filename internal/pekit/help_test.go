package pekit

import (
	"bytes"
	"strings"
	"testing"
)

func TestHelpParses(t *testing.T) {
	cases := map[string][]string{
		"":          {"--help"},
		"build":     {"build", "--help"},
		"package":   {"-h", "package"},
		"lint":      {"help", "lint"},
		"workspace": {"workspace", "--help"},
		"publish":   {"workspace", "--jobs", "2", "publish", "--help"},
		"help":      {"help", "help"},
	}
	for topic, args := range cases {
		inv, err := ParseInvocation(args, "/tmp")
		if err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if !inv.Help || inv.HelpTopic != topic {
			t.Fatalf("%v: help=%v topic=%q, want topic %q", args, inv.Help, inv.HelpTopic, topic)
		}
	}
	// Help wins over validation: a bad flag combination still gets help.
	if inv, err := ParseInvocation([]string{"clean", "--version", "1", "--help"}, "/tmp"); err != nil || !inv.Help {
		t.Fatalf("help should bypass validation: %v", err)
	}
	if _, err := ParseInvocation([]string{"help", "bogus"}, "/tmp"); diagCode(err) != "unknown_command" {
		t.Fatalf("want unknown_command, got %v", err)
	}
	if _, err := ParseInvocation([]string{"workspace", "version"}, "/tmp"); diagCode(err) != "missing_workspace_command" {
		t.Fatalf("workspace must not delegate to version: %v", err)
	}
	if _, err := ParseInvocation([]string{"version", "extra"}, "/tmp"); diagCode(err) != "invalid_selector" {
		t.Fatalf("version takes no selectors: %v", err)
	}
	if _, err := ParseInvocation([]string{"version", "--all"}, "/tmp"); diagCode(err) != "unsupported_flag" {
		t.Fatalf("version takes no command flags: %v", err)
	}
}

// Every command and every flag the parser knows must be documented, and
// every documented command must exist, so help cannot drift from the parser.
func TestHelpCoversEveryCommandAndFlag(t *testing.T) {
	overview := usageText("")
	seen := map[Command]bool{}
	for _, cmd := range commandOrder {
		if _, ok := commands[string(cmd)]; !ok {
			t.Errorf("help lists %q, which is not a command", cmd)
		}
		seen[cmd] = true
	}
	for name, cmd := range commands {
		if !seen[cmd] {
			t.Errorf("command %q has no help entry", name)
		}
		if !strings.Contains(overview, "  "+name+" ") {
			t.Errorf("overview does not list %q", name)
		}
		if commandSummary[cmd] == "" || commandSelectors[cmd] == "" {
			t.Errorf("command %q lacks a summary or selector line", name)
		}
		if page := usageText(name); !strings.Contains(page, "pekit "+name) {
			t.Errorf("help %s renders nothing useful:\n%s", name, page)
		}
	}
	for group := flagVersion; group <= flagRepin; group++ {
		if len(flagGroupHelp[group]) == 0 {
			t.Errorf("flag group %s has no help rows", flagUseName(group))
		}
	}
	for cmd, groups := range commandFlags {
		page := usageText(string(cmd))
		for group := range groups {
			for _, row := range flagGroupHelp[group] {
				if !strings.Contains(page, row[0]) {
					t.Errorf("help %s omits %s", cmd, row[0])
				}
			}
		}
	}
	// Every flag the parser accepts appears somewhere in the help.
	for _, flag := range []string{"--recipe", "--workspace", "--allow-unused", "--dry-run", "--quiet", "--verbose", "--json",
		"--version", "-V", "--latest", "--all-versions", "--local", "--prefer-local", "--no-build", "--no-verify",
		"--env", "--keyring", "--keyring.", "--refresh-source", "--allow-unanchored", "--allow-unsigned", "--repin",
		"--all", "--output-only", "--target-only", "--help", "-h", "--jobs", "--fail-fast"} {
		found := strings.Contains(overview, flag)
		for name := range commands {
			found = found || strings.Contains(usageText(name), flag)
		}
		if !found {
			t.Errorf("flag %s is documented nowhere in help", flag)
		}
	}
}

func TestHelpAndVersionRun(t *testing.T) {
	var stdout, stderr bytes.Buffer
	app := &App{Stdout: &stdout, Stderr: &stderr}
	if err := app.Run([]string{"help"}); err != nil || !strings.HasPrefix(stdout.String(), "pekit — ") {
		t.Fatalf("help: err=%v stdout=%q", err, stdout.String())
	}
	stdout.Reset()
	if err := app.Run([]string{"build", "-h"}); err != nil || !strings.Contains(stdout.String(), "--no-build") {
		t.Fatalf("build -h: err=%v stdout=%q", err, stdout.String())
	}
	stdout.Reset()
	if err := app.Run([]string{"version"}); err != nil || !strings.HasPrefix(stdout.String(), "pekit ") {
		t.Fatalf("version: err=%v stdout=%q", err, stdout.String())
	}
	stdout.Reset()
	if err := app.Run(nil); diagCode(err) != "missing_command" || !strings.Contains(stderr.String(), "Commands:") || stdout.Len() != 0 {
		t.Fatalf("bare pekit: err=%v stderr=%q", err, stderr.String())
	}
}
