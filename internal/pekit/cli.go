package pekit

import (
	"fmt"
	"strconv"
	"strings"
)

type flagUse int

const (
	flagVersion flagUse = iota
	flagLocal
	flagNoBuild
	flagNoVerify
	flagEnv
	flagKeyring
	flagRefreshSource
	flagAllowUnanchored
	flagAll
	flagCleanMode
	flagRepin
)

var commandFlags = map[Command]map[flagUse]bool{
	CommandBuild: {
		flagVersion: true, flagLocal: true, flagNoBuild: true, flagNoVerify: true, flagEnv: true, flagKeyring: true, flagRefreshSource: true,
	},
	CommandTest: {
		flagVersion: true, flagLocal: true, flagNoBuild: true, flagNoVerify: true, flagEnv: true, flagKeyring: true, flagRefreshSource: true,
	},
	CommandInstall: {
		flagVersion: true, flagLocal: true, flagNoBuild: true, flagNoVerify: true, flagEnv: true, flagKeyring: true, flagRefreshSource: true,
	},
	CommandPackage: {
		flagVersion: true, flagLocal: true, flagNoBuild: true, flagNoVerify: true, flagEnv: true, flagKeyring: true, flagRefreshSource: true, flagAll: true,
	},
	CommandPublish: {
		flagVersion: true, flagLocal: true, flagNoBuild: true, flagNoVerify: true, flagEnv: true, flagKeyring: true, flagRefreshSource: true, flagAllowUnanchored: true, flagAll: true,
	},
	CommandClean: {
		flagEnv: true, flagKeyring: true, flagCleanMode: true,
	},
	// gen runs a source-generating target; verify runs a gen target's
	// verify_command. Neither resolves versions or a remote source, so they
	// carry only the env/keyring/wrap inputs plus --all (every gen target).
	CommandGen: {
		flagEnv: true, flagKeyring: true, flagAll: true,
	},
	CommandVerify: {
		flagEnv: true, flagKeyring: true, flagAll: true,
	},
	// lock fetches and pins source inputs without building. --repin is the
	// explicit accept-changed-upstream-bytes ceremony and demands an exact
	// --version; --refresh-source re-downloads before verifying.
	CommandLock: {
		flagVersion: true, flagRefreshSource: true, flagRepin: true,
	},
}

func ParseInvocation(args []string, cwd string) (Invocation, error) {
	inv := Invocation{Args: append([]string(nil), args...), Cwd: cwd, Jobs: 1, KeyringValues: map[string]string{}, ResolvedKeyringEnv: map[string]string{}}
	var used []flagUse
	i := 0
	commandSeen := false
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			i++
			inv.AfterDoubleDash = append(inv.AfterDoubleDash, args[i:]...)
			break
		}
		if !commandSeen {
			if handled, next, use, err := parseGlobalOrCommandFlag(&inv, args, i); err != nil {
				return Invocation{}, err
			} else if handled {
				if use >= 0 {
					used = append(used, use)
				}
				i = next
				continue
			}
			cmd, ok := commands[arg]
			if !ok {
				return Invocation{}, diag("unknown_command", "unknown command %q", arg)
			}
			inv.Command = cmd
			commandSeen = true
			i++
			if cmd == CommandWorkspace {
				inv.WorkspaceMode = true
				if err := parseWorkspaceTail(&inv, args[i:], &used); err != nil {
					return Invocation{}, err
				}
				return inv, validateInvocation(&inv, used)
			}
			continue
		}
		if handled, next, use, err := parseGlobalOrCommandFlag(&inv, args, i); err != nil {
			return Invocation{}, err
		} else if handled {
			if use >= 0 {
				used = append(used, use)
			}
			i = next
			continue
		}
		inv.Positionals = append(inv.Positionals, arg)
		i++
	}
	if !commandSeen {
		return Invocation{}, diag("missing_command", "missing command")
	}
	return inv, validateInvocation(&inv, used)
}

func parseWorkspaceTail(inv *Invocation, args []string, used *[]flagUse) error {
	i := 0
	for i < len(args) {
		arg := args[i]
		switch {
		case arg == "--fail-fast":
			inv.FailFast = true
			i++
		case arg == "--jobs":
			if i+1 >= len(args) {
				return diag("missing_flag_value", "--jobs requires a value")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n <= 0 {
				return diag("invalid_flag_value", "--jobs requires a positive integer")
			}
			inv.Jobs = n
			i += 2
		case strings.HasPrefix(arg, "--jobs="):
			n, err := strconv.Atoi(strings.TrimPrefix(arg, "--jobs="))
			if err != nil || n <= 0 {
				return diag("invalid_flag_value", "--jobs requires a positive integer")
			}
			inv.Jobs = n
			i++
		default:
			if handled, next, use, err := parseGlobalOrCommandFlag(inv, args, i); err != nil {
				return err
			} else if handled {
				if use >= 0 {
					return diag("missing_workspace_command", "workspace command flag %s must appear after the delegated command", flagUseName(use))
				}
				i = next
				continue
			}
			cmd, ok := commands[arg]
			if !ok || cmd == CommandWorkspace {
				return diag("missing_workspace_command", "workspace requires a delegated command after workspace flags")
			}
			inv.DelegateCommand = cmd
			tail := append([]string{string(cmd)}, args[i+1:]...)
			sub, err := ParseInvocation(tail, inv.Cwd)
			if err != nil {
				return err
			}
			copyDelegated(inv, sub)
			*used = append(*used, delegatedUsedFlags(sub)...)
			return nil
		}
	}
	return diag("missing_workspace_command", "workspace requires a delegated command")
}

func copyDelegated(inv *Invocation, sub Invocation) {
	inv.DelegateCommand = sub.Command
	inv.Positionals = sub.Positionals
	inv.AfterDoubleDash = sub.AfterDoubleDash
	inv.Version = sub.Version
	inv.Latest = sub.Latest
	inv.AllVersions = sub.AllVersions
	inv.SuppressUnsupportedVersion = sub.SuppressUnsupportedVersion
	inv.Local = sub.Local
	inv.PreferLocal = sub.PreferLocal
	inv.NoBuild = sub.NoBuild
	inv.NoVerify = sub.NoVerify
	inv.EnvName = sub.EnvName
	inv.Keyrings = sub.Keyrings
	inv.KeyringValues = sub.KeyringValues
	inv.ResolvedKeyringEnv = sub.ResolvedKeyringEnv
	inv.RefreshSource = sub.RefreshSource
	inv.AllowUnanchored = sub.AllowUnanchored
	inv.Repin = sub.Repin
	inv.All = sub.All
	inv.OutputOnly = sub.OutputOnly
	inv.TargetOnly = sub.TargetOnly
	inv.AllowUnused = inv.AllowUnused || sub.AllowUnused
	inv.Suppressed = append(inv.Suppressed, sub.Suppressed...)
	inv.DryRun = inv.DryRun || sub.DryRun
	inv.Quiet = inv.Quiet || sub.Quiet
	inv.Verbose = inv.Verbose || sub.Verbose
	inv.JSON = inv.JSON || sub.JSON
	if sub.RecipeFlag != "" {
		inv.RecipeFlag = sub.RecipeFlag
	}
	if sub.WorkspaceFlag != "" {
		inv.WorkspaceFlag = sub.WorkspaceFlag
	}
}

func delegatedUsedFlags(inv Invocation) []flagUse {
	var out []flagUse
	if inv.Version != "" || inv.Latest || inv.AllVersions {
		out = append(out, flagVersion)
	}
	if inv.Local != nil || inv.PreferLocal != nil {
		out = append(out, flagLocal)
	}
	if inv.NoBuild != nil {
		out = append(out, flagNoBuild)
	}
	if inv.NoVerify != nil {
		out = append(out, flagNoVerify)
	}
	if inv.EnvName != "" {
		out = append(out, flagEnv)
	}
	if len(inv.Keyrings) > 0 || len(inv.KeyringValues) > 0 {
		out = append(out, flagKeyring)
	}
	if inv.RefreshSource {
		out = append(out, flagRefreshSource)
	}
	if inv.AllowUnanchored {
		out = append(out, flagAllowUnanchored)
	}
	if inv.Repin {
		out = append(out, flagRepin)
	}
	if inv.All {
		out = append(out, flagAll)
	}
	if inv.OutputOnly || inv.TargetOnly {
		out = append(out, flagCleanMode)
	}
	return out
}

func parseGlobalOrCommandFlag(inv *Invocation, args []string, i int) (bool, int, flagUse, error) {
	arg := args[i]
	if !strings.HasPrefix(arg, "-") || arg == "-" {
		return false, i, -1, nil
	}
	if strings.HasPrefix(arg, "--keyring.") {
		k, v, ok := strings.Cut(strings.TrimPrefix(arg, "--keyring."), "=")
		if !ok || k == "" {
			return false, i, -1, diag("invalid_keyring_flag", "--keyring.<path>=<value> requires a dotted path and value")
		}
		inv.KeyringValues[k] = v
		return true, i + 1, flagKeyring, nil
	}
	name, value, hasValue := strings.Cut(arg, "=")
	switch name {
	case "--recipe":
		v, next, err := flagValue(args, i, value, hasValue, name)
		if err != nil {
			return false, i, -1, err
		}
		inv.RecipeFlag = v
		return true, next, -1, nil
	case "--workspace":
		v, next, err := flagValue(args, i, value, hasValue, name)
		if err != nil {
			return false, i, -1, err
		}
		inv.WorkspaceFlag = v
		return true, next, -1, nil
	case "--allow-unused":
		if hasValue {
			return false, i, -1, diag("unexpected_flag_value", "--allow-unused does not take a value")
		}
		inv.AllowUnused = true
		return true, i + 1, -1, nil
	case "--dry-run":
		if hasValue {
			return false, i, -1, diag("unexpected_flag_value", "--dry-run does not take a value")
		}
		inv.DryRun = true
		return true, i + 1, -1, nil
	case "--quiet":
		if hasValue {
			return false, i, -1, diag("unexpected_flag_value", "--quiet does not take a value")
		}
		inv.Quiet = true
		return true, i + 1, -1, nil
	case "--verbose":
		if hasValue {
			return false, i, -1, diag("unexpected_flag_value", "--verbose does not take a value")
		}
		inv.Verbose = true
		return true, i + 1, -1, nil
	case "--json":
		if hasValue {
			return false, i, -1, diag("unexpected_flag_value", "--json does not take a value")
		}
		inv.JSON = true
		return true, i + 1, -1, nil
	case "--version", "-V":
		v, next, err := flagValue(args, i, value, hasValue, name)
		if err != nil {
			return false, i, -1, err
		}
		inv.Version = v
		return true, next, flagVersion, nil
	case "--latest":
		if hasValue {
			return false, i, -1, diag("unexpected_flag_value", "--latest does not take a value")
		}
		inv.Latest = true
		return true, i + 1, flagVersion, nil
	case "--all-versions":
		if hasValue {
			return false, i, -1, diag("unexpected_flag_value", "--all-versions does not take a value")
		}
		inv.AllVersions = true
		return true, i + 1, flagVersion, nil
	case "--local":
		v := ""
		if hasValue {
			v = value
		}
		inv.Local = &v
		return true, i + 1, flagLocal, nil
	case "--prefer-local":
		v := ""
		if hasValue {
			v = value
		}
		inv.PreferLocal = &v
		return true, i + 1, flagLocal, nil
	case "--no-build":
		v := ""
		if hasValue {
			v = value
		}
		inv.NoBuild = &v
		return true, i + 1, flagNoBuild, nil
	case "--no-verify":
		v := ""
		if hasValue {
			v = value
		}
		inv.NoVerify = &v
		return true, i + 1, flagNoVerify, nil
	case "--env":
		v, next, err := flagValue(args, i, value, hasValue, name)
		if err != nil {
			return false, i, -1, err
		}
		inv.EnvName = v
		return true, next, flagEnv, nil
	case "--keyring":
		v, next, err := flagValue(args, i, value, hasValue, name)
		if err != nil {
			return false, i, -1, err
		}
		inv.Keyrings = append(inv.Keyrings, v)
		return true, next, flagKeyring, nil
	case "--refresh-source":
		if hasValue {
			return false, i, -1, diag("unexpected_flag_value", "--refresh-source does not take a value")
		}
		inv.RefreshSource = true
		return true, i + 1, flagRefreshSource, nil
	case "--allow-unanchored":
		if hasValue {
			return false, i, -1, diag("unexpected_flag_value", "--allow-unanchored does not take a value")
		}
		inv.AllowUnanchored = true
		return true, i + 1, flagAllowUnanchored, nil
	case "--repin":
		if hasValue {
			return false, i, -1, diag("unexpected_flag_value", "--repin does not take a value")
		}
		inv.Repin = true
		return true, i + 1, flagRepin, nil
	case "--all":
		if hasValue {
			return false, i, -1, diag("unexpected_flag_value", "--all does not take a value")
		}
		inv.All = true
		return true, i + 1, flagAll, nil
	case "--output-only":
		if hasValue {
			return false, i, -1, diag("unexpected_flag_value", "--output-only does not take a value")
		}
		inv.OutputOnly = true
		return true, i + 1, flagCleanMode, nil
	case "--target-only":
		if hasValue {
			return false, i, -1, diag("unexpected_flag_value", "--target-only does not take a value")
		}
		inv.TargetOnly = true
		return true, i + 1, flagCleanMode, nil
	default:
		return false, i, -1, diag("unknown_flag", "unknown flag %q", arg)
	}
}

func flagValue(args []string, i int, value string, hasValue bool, name string) (string, int, error) {
	if hasValue {
		if value == "" {
			return "", i, diag("missing_flag_value", "%s requires a non-empty value", name)
		}
		return value, i + 1, nil
	}
	if i+1 >= len(args) || args[i+1] == "--" || strings.HasPrefix(args[i+1], "-") {
		return "", i, diag("missing_flag_value", "%s requires a value", name)
	}
	return args[i+1], i + 2, nil
}

func validateInvocation(inv *Invocation, used []flagUse) error {
	if inv.Quiet && inv.Verbose {
		return diag("invalid_flags", "--quiet and --verbose cannot be used together")
	}
	if inv.Quiet && inv.JSON {
		return diag("invalid_flags", "--quiet and --json cannot be used together")
	}
	if inv.RecipeFlag != "" && inv.WorkspaceFlag != "" {
		return diag("invalid_flags", "--recipe and --workspace cannot be used together")
	}
	if inv.WorkspaceFlag != "" && !inv.WorkspaceMode {
		return diag("invalid_flags", "--workspace can only be used with the workspace command")
	}
	cmd := inv.EffectiveCommand()
	if cmd == "" {
		return diag("missing_command", "missing command")
	}
	if (boolCount(inv.Version != "", inv.Latest, inv.AllVersions)) > 1 {
		return diag("invalid_flags", "--version, --latest, and --all-versions are mutually exclusive")
	}
	if inv.Local != nil && inv.PreferLocal != nil {
		return diag("invalid_flags", "--local and --prefer-local are mutually exclusive")
	}
	if inv.OutputOnly && inv.TargetOnly {
		return diag("invalid_flags", "--output-only and --target-only are mutually exclusive")
	}
	caps := commandFlags[cmd]
	for _, u := range used {
		if !caps[u] {
			if !inv.AllowUnused {
				return diag("unsupported_flag", "%s does not support %s", cmd, flagUseName(u))
			}
			inv.Suppressed = append(inv.Suppressed, fmt.Sprintf("%s does not support %s", cmd, flagUseName(u)))
		}
	}
	if cmd == CommandClean && inv.OutputOnly && !inv.AllowUnused {
		for _, u := range used {
			if u == flagEnv || u == flagKeyring {
				return diag("unsupported_flag", "clean --output-only does not run a target, so %s is unused", flagUseName(u))
			}
		}
	} else if cmd == CommandClean && inv.OutputOnly && inv.AllowUnused {
		for _, u := range used {
			if u == flagEnv || u == flagKeyring {
				inv.Suppressed = append(inv.Suppressed, fmt.Sprintf("clean --output-only does not run a target, so %s is unused", flagUseName(u)))
			}
		}
	}
	selectors := inv.selectors()
	switch cmd {
	case CommandClean:
		if len(selectors) > 1 {
			return diag("invalid_selector", "clean accepts at most one target selector")
		}
	case CommandPackage, CommandPublish:
		if inv.All && len(selectors) > 0 {
			return diag("invalid_flags", "--all cannot be combined with package selectors")
		}
	case CommandGen, CommandVerify:
		if inv.All && len(selectors) > 0 {
			return diag("invalid_flags", "--all cannot be combined with %s target selectors", cmd)
		}
	case CommandLock:
		if len(selectors) > 0 {
			return diag("invalid_selector", "lock does not accept selectors")
		}
	default:
		if inv.All && !inv.AllowUnused {
			return diag("unsupported_flag", "%s does not support --all", cmd)
		}
	}
	for _, s := range selectors {
		if strings.HasPrefix(s, "-") {
			return diag("invalid_selector", "selector %q starts with '-'; use -- before selector-like values", s)
		}
	}
	return nil
}

func boolCount(values ...bool) int {
	n := 0
	for _, v := range values {
		if v {
			n++
		}
	}
	return n
}

func flagUseName(u flagUse) string {
	switch u {
	case flagVersion:
		return "version selection"
	case flagLocal:
		return "local source flags"
	case flagNoBuild:
		return "--no-build"
	case flagNoVerify:
		return "--no-verify"
	case flagEnv:
		return "--env"
	case flagKeyring:
		return "--keyring"
	case flagRefreshSource:
		return "--refresh-source"
	case flagAllowUnanchored:
		return "--allow-unanchored"
	case flagRepin:
		return "--repin"
	case flagAll:
		return "--all"
	case flagCleanMode:
		return "clean mode flags"
	default:
		return fmt.Sprintf("flag group %d", u)
	}
}
