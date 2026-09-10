package pekit

import (
	"fmt"
	"runtime/debug"
	"sort"
	"strings"
)

// Help is generated from the same tables the parser reads — the command
// map and commandFlags — so a command or flag group cannot exist without
// appearing here. The one-line descriptions are the only hand-written part.

const helpReference = "https://learn.peios.org/pekit/reference/cli"

// commandOrder is the order commands are listed in; every key of commands
// must appear (helpCoversEveryCommand checks).
var commandOrder = []Command{
	CommandBuild, CommandTest, CommandInstall, CommandPackage, CommandPublish,
	CommandClean, CommandGen, CommandVerify, CommandLock, CommandLint,
	CommandWorkspace, CommandHelp, CommandVersion,
}

var commandSummary = map[Command]string{
	CommandBuild:     "run build targets and stage their output under out_dir",
	CommandTest:      "stage the builds a test target needs, then run it",
	CommandInstall:   "stage the builds an install target needs, then run it",
	CommandPackage:   "build and write package artifacts (.peipkg, tar)",
	CommandPublish:   "package, then publish to the configured destinations",
	CommandClean:     "run the clean target and/or remove managed output",
	CommandGen:       "run a gen target: write generated source into the tree",
	CommandVerify:    "run gen verify_commands: a read-only drift check",
	CommandLock:      "fetch and pin source versions in pekit.lock without building",
	CommandLint:      "check the recipe, and with a version its staged payload, against lint.pekit.toml",
	CommandWorkspace: "run one of the above across every workspace member",
	CommandHelp:      "show this overview, or `help <command>` for one command",
	CommandVersion:   "print the pekit version",
}

// commandSelectors says what a command's positional arguments select.
var commandSelectors = map[Command]string{
	CommandBuild:     "build target names (default: main)",
	CommandTest:      "test target names (default: main)",
	CommandInstall:   "install target names (default: main)",
	CommandPackage:   "package selectors, `<package>` or `<package>:<instance>` (default: the only package, or --all)",
	CommandPublish:   "package selectors, as for package",
	CommandClean:     "at most one clean target name",
	CommandGen:       "gen target names (default: main, or --all)",
	CommandVerify:    "gen target names (default: main, or --all)",
	CommandLock:      "none",
	CommandLint:      "none: every package the recipe defines is checked",
	CommandWorkspace: "the delegated command's selectors, applied to every member",
	CommandHelp:      "a command name",
	CommandVersion:   "none",
}

// flagGroupHelp lists each flag group's flags with a one-line meaning, in
// the order flagUse declares them.
var flagGroupHelp = map[flagUse][][2]string{
	flagVersion: {
		{"--version <v>, -V <v>", "select exact versions (comma-separated) or a constraint such as \">= 1.2\""},
		{"--latest", "select the newest version discovery finds"},
		{"--all-versions", "select every version discovery finds"},
	},
	flagLocal: {
		{"--local[=<path>]", "use the local source instead of resolving one"},
		{"--prefer-local[=<path>]", "use the local source when present, otherwise resolve"},
	},
	flagNoBuild:         {{"--no-build[=<targets>]", "reuse already-staged build targets (all, or the named ones)"}},
	flagNoVerify:        {{"--no-verify[=<gen targets>]", "skip the gen drift-check pre-flight (all, or the named ones)"}},
	flagEnv:             {{"--env <name>", "select env-file layers: main (default), none, or <name>.env.pekit.toml"}},
	flagKeyring:         {{"--keyring <name|path>", "load a keyring file (repeatable)"}, {"--keyring.<path>=<value>", "set one keyring value inline (repeatable)"}},
	flagRefreshSource:   {{"--refresh-source", "re-fetch the source, ignoring the cache; the lock still applies"}},
	flagAllowUnanchored: {{"--allow-unanchored", "permit publishing from a source with no checksum and no lock entry"}},
	flagAllowUnsigned:   {{"--allow-unsigned", "permit publishing peipkg packages without a signing key"}},
	flagAll:             {{"--all", "act on every package, or every gen target"}},
	flagCleanMode:       {{"--output-only", "remove managed output without running the clean target"}, {"--target-only", "run the clean target without removing managed output"}},
	flagRepin:           {{"--repin", "replace the lock entry for the selected exact version"}},
}

var globalFlagHelp = [][2]string{
	{"--recipe <path>", "recipe file or directory to run (default: nearest pekit.toml upward)"},
	{"--workspace <path>", "workspace file or directory (workspace command only)"},
	{"--dry-run", "plan and report; run nothing"},
	{"--json", "line-delimited JSON events instead of text"},
	{"--quiet", "only warnings, artifacts and summaries"},
	{"--verbose", "extra detail: recipe, source and per-file events"},
	{"--allow-unused", "warn instead of failing when a recognised flag does not apply"},
	{"--help, -h", "show help for pekit or, after a command, for that command"},
}

var workspaceFlagHelp = [][2]string{
	{"--jobs <n>", "run this many members at once (default 1)"},
	{"--fail-fast", "stop scheduling members after the first failure"},
}

// usageText renders the overview (topic "") or one command's help.
func usageText(topic string) string {
	var b strings.Builder
	if topic == "" {
		b.WriteString("pekit — build, test, package and publish from pekit.toml recipes\n\n")
		b.WriteString("Usage:\n")
		b.WriteString("  pekit [global flags] <command> [flags] [selectors]\n")
		b.WriteString("  pekit [global flags] workspace [--jobs <n>] [--fail-fast] <command> [flags] [selectors]\n\n")
		b.WriteString("Commands:\n")
		writeRows(&b, commandRows())
		b.WriteString("\nGlobal flags (accepted by every command):\n")
		writeRows(&b, globalFlagHelp)
		b.WriteString("\nRun `pekit help <command>` for a command's flags and selectors.\n")
		b.WriteString("Full reference: " + helpReference + "\n")
		return b.String()
	}
	cmd := Command(topic)
	fmt.Fprintf(&b, "pekit %s — %s\n\n", cmd, commandSummary[cmd])
	switch cmd {
	case CommandWorkspace:
		b.WriteString("Usage:\n  pekit [global flags] workspace [--jobs <n>] [--fail-fast] <command> [flags] [selectors]\n\n")
		b.WriteString("Workspace flags (before the delegated command):\n")
		writeRows(&b, workspaceFlagHelp)
		b.WriteString("\nThe delegated command takes its own flags, after its name.\n")
	case CommandHelp:
		b.WriteString("Usage:\n  pekit help [<command>]\n  pekit --help\n  pekit <command> --help\n")
	case CommandVersion:
		b.WriteString("Usage:\n  pekit version\n")
	default:
		fmt.Fprintf(&b, "Usage:\n  pekit [global flags] %s [flags] [selectors]\n\n", cmd)
		fmt.Fprintf(&b, "Selectors: %s\n", commandSelectors[cmd])
		var rows [][2]string
		for _, group := range flagGroupOrder() {
			if commandFlags[cmd][group] {
				rows = append(rows, flagGroupHelp[group]...)
			}
		}
		if len(rows) > 0 {
			b.WriteString("\nFlags:\n")
			writeRows(&b, rows)
		} else {
			b.WriteString("\nNo command flags; global flags only.\n")
		}
	}
	b.WriteString("\nFull reference: " + helpReference + "\n")
	return b.String()
}

func commandRows() [][2]string {
	rows := make([][2]string, 0, len(commandOrder))
	for _, cmd := range commandOrder {
		rows = append(rows, [2]string{string(cmd), commandSummary[cmd]})
	}
	return rows
}

func flagGroupOrder() []flagUse {
	groups := make([]flagUse, 0, len(flagGroupHelp))
	for g := range flagGroupHelp {
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i] < groups[j] })
	return groups
}

func writeRows(b *strings.Builder, rows [][2]string) {
	width := 0
	for _, r := range rows {
		if len(r[0]) > width {
			width = len(r[0])
		}
	}
	for _, r := range rows {
		fmt.Fprintf(b, "  %-*s  %s\n", width, r[0], r[1])
	}
}

// versionString reports the module version and, when the binary was built
// from a checkout, the commit it was built from.
func versionString() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "pekit (unknown build)"
	}
	version := info.Main.Version
	if version == "" {
		version = "(devel)"
	}
	rev, modified := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	// A pseudo-version already names the commit and a +dirty suffix already
	// says it was modified; only add what the version string lacks.
	out := "pekit " + version
	if len(rev) > 12 {
		rev = rev[:12]
	}
	var extra []string
	if rev != "" && !strings.Contains(version, rev) {
		extra = append(extra, rev)
	}
	if modified && !strings.HasSuffix(version, "+dirty") {
		extra = append(extra, "modified")
	}
	if len(extra) > 0 {
		out += " (" + strings.Join(extra, ", ") + ")"
	}
	return out
}
