package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	semver "github.com/Masterminds/semver/v3"
)

// Target is a single runnable target within a command section.
type Target struct {
	Command string
	// Needs names build targets that must be staged before this one runs —
	// always [build] targets, regardless of the section this target is declared
	// in (a [test]/[install] target depends on builds, never on sibling tests
	// or installs). The build subgraph is acyclic. Each direct dependency's
	// staged output dir is exported to this target's script as PEKIT_<NAME>_OUT
	// — the name uppercased with every non-alphanumeric byte turned into '_'
	// (see envTargetName). Shared dependencies run once per invocation.
	Needs []string
}

// EnvVar is one [env] entry. Order matters: later values may reference
// earlier ones, so Config.Env preserves document order.
type EnvVar struct {
	Name  string
	Value string
}

// Config is a parsed pekit.toml.
type Config struct {
	// Env is exported (in order) at the top of every command script.
	Env []EnvVar
	// OutDir is the pekit-managed staging directory; empty when unset.
	// Each build target gets OutDir/<name>, exported as $PEKIT_OUT.
	OutDir string
	// ClearOut wipes a target's staging dir before its build. Default true.
	ClearOut bool
	// Commands holds the command-running verbs: verb -> target name -> target.
	Commands map[string]map[string]Target
	// Source, when set, is upstream source the build fetches and builds
	// in instead of the project directory; nil = the cwd is the source.
	Source *Source
	// Wrap, when non-empty, is the [wrap] command template from the selected
	// env file (env.pekit.toml / <name>.env.pekit.toml). It contains the
	// placeholder {{command}}; each target's self-contained script is
	// shell-quoted and substituted in, and the result is what runs. Empty =
	// no wrapping (--env none, or no env file under the default --env main).
	// Set by applyEnv, not parsed from pekit.toml.
	Wrap string
	// Keyring holds injected values from --keyring.<name>=<value>, keyed by the
	// env var name they are exported as (PEKIT_KEYRING_<NAME>). Each is baked
	// into every command's script as an export (like PEKIT_OUT) so it survives a
	// wrapped environment. pekit treats values as opaque — a path, a literal,
	// whatever; the command decides. Not parsed from pekit.toml.
	Keyring map[string]string
}

// Source is a [source] block: upstream the build checks out before
// running. The checkout lands in OutDir/source/<rev> and becomes the
// build's working directory.
type Source struct {
	Git string
	Rev string
	// URL, when set, is a tarball/file download (templated with {{version}})
	// instead of a git clone — for upstreams that publish release archives
	// rather than tags (gmp, most GNU projects). Mutually exclusive with git.
	// Enumeration lists the URL's parent directory and matches the filename
	// template (see enumerateURLVersions); Versions/TagRegex still apply.
	URL string
	// Extract unpacks a downloaded URL archive (tar.*, tgz, zip) into the
	// source dir; the build then runs in the unpacked tree (its single
	// top-level dir, if there is exactly one). Only valid with URL; without
	// it the file is downloaded into the source dir as-is.
	Extract bool
	// Versions, when set, is a semver constraint bounding which upstream
	// versions this recipe builds. The resolved --version set (including
	// an enumerated "*") is intersected with it, so tags the recipe can't
	// build (e.g. releases predating its packaging files) are skipped
	// rather than attempted. Empty = no bound.
	Versions string
	// TagRegex, when set, is a regexp an upstream tag must match to be
	// enumerated — a discovery filter beyond the rev template's shape, for
	// excluding tags a semver bound can't. glibc's three-component ".9000"
	// dev snapshots sort below the next release, so Versions can't drop them
	// generically; tag_regex = '^glibc-\d+\.\d+$' keeps only the
	// two-component release tags. It filters enumeration (constraints, "*",
	// --latest, the trailing-zero ladder); an explicit exact --version
	// bypasses enumeration and so is taken at its word. Empty = no filter.
	// Applies to a git source only (a url source filters with FileRegex).
	TagRegex string
	// FileRegex is the url-source analogue of TagRegex: a regexp a directory
	// listing entry must match to be enumerated, beyond the url filename
	// template's shape. file_regex = '^gmp-\d+\.\d+\.\d+\.tar\.xz$' keeps only
	// three-component gmp release tarballs, excluding betas or other archives
	// the bare template would also match. Empty = no filter.
	FileRegex string
	// LocalPath, when set, is a local working copy (relative to the recipe
	// dir) usable in place of git/rev under --local — for compiling
	// in-development packages without a tag. The farm never passes
	// --local, so it always uses git.
	LocalPath string
	// Local is a runtime flag (not parsed): --local set it, switching the
	// source to build LocalPath in place rather than cloning git@rev.
	Local bool
}

// LoadConfig reads, templates, and parses a pekit.toml file.
func LoadConfig(path string, ver *Version) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rendered, err := renderTemplate(string(data), ver)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg, err := ParseConfig(rendered)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// applyEnv resolves the --env selection into cfg.Wrap. envValue is "none"
// (no wrap), "main" (env.pekit.toml, beside the recipe), or any other name
// (<name>.env.pekit.toml). A missing file under the default "main" is a no-op;
// a missing file for an explicitly-named env is an error.
func applyEnv(cfg *Config, envValue string) error {
	wrap, err := loadWrap(envValue)
	if err != nil {
		return err
	}
	cfg.Wrap = wrap
	return nil
}

// loadWrap reads the [wrap] command template for the given --env value from the
// env file beside the recipe.
func loadWrap(envValue string) (string, error) {
	if envValue == "none" {
		return "", nil
	}
	path := "env.pekit.toml"
	mainDefault := envValue == "main"
	if !mainDefault {
		path = envValue + ".env.pekit.toml"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// "main" is the default; a recipe with no env file just runs unwrapped.
		// An explicitly-named env that is missing is a mistake.
		if os.IsNotExist(err) && mainDefault {
			return "", nil
		}
		return "", fmt.Errorf("--env %s: %w", envValue, err)
	}
	return parseWrap(path, string(data))
}

// parseWrap parses an env file. Its only supported content is a [wrap] section
// with a command template that must contain {{command}}.
func parseWrap(path, src string) (string, error) {
	var raw map[string]any
	if _, err := toml.Decode(src, &raw); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	for k := range raw {
		if k != "wrap" {
			return "", fmt.Errorf("%s: unknown section %q (only [wrap] is supported)", path, k)
		}
	}
	wrapRaw, ok := raw["wrap"]
	if !ok {
		return "", fmt.Errorf("%s: missing [wrap] section", path)
	}
	wrapTable, ok := wrapRaw.(map[string]any)
	if !ok {
		return "", fmt.Errorf("%s: [wrap] must be a table", path)
	}
	for k := range wrapTable {
		if k != "command" {
			return "", fmt.Errorf("%s: [wrap] has unknown key %q (only command is supported)", path, k)
		}
	}
	command, ok := wrapTable["command"].(string)
	if !ok || command == "" {
		return "", fmt.Errorf("%s: [wrap] command must be a non-empty string", path)
	}
	if !strings.Contains(command, "{{command}}") {
		return "", fmt.Errorf("%s: [wrap] command must contain {{command}} (else it discards the wrapped command)", path)
	}
	return command, nil
}

// keyringSpec is the keyring selection from the command line: zero or more
// <name>.keyring.pekit.toml files (--keyring=<name>) plus individual
// --keyring.<a.b>=<value> overrides. resolveKeyring turns it into the final
// PEKIT_KEYRING_<NAME> env-var map.
type keyringSpec struct {
	files []string          // <name>, in order; loads <name>.keyring.pekit.toml
	vars  map[string]string // PEKIT_KEYRING_<NAME> -> value, from --keyring.<a.b>=<v>
}

// resolveKeyring loads the named keyring files (left-to-right, later overriding
// earlier) and overlays the individual --keyring flags on top, yielding the
// env-var map baked into every command. Returns nil when nothing was selected.
func resolveKeyring(spec keyringSpec) (map[string]string, error) {
	out := map[string]string{}
	for _, name := range spec.files {
		m, err := loadKeyringFile(name)
		if err != nil {
			return nil, err
		}
		for k, v := range m {
			out[k] = v
		}
	}
	for k, v := range spec.vars {
		out[k] = v // an explicit --keyring.a.b=v overrides a file value
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// loadKeyringFile reads <name>.keyring.pekit.toml beside the recipe and flattens
// it to a PEKIT_KEYRING_<NAME> map. Each string leaf becomes one var, keyed by
// its dotted path: [tcb] pub = "x" -> PEKIT_KEYRING_TCB_PUB; [tcb] priv.path =
// "y" -> PEKIT_KEYRING_TCB_PRIV_PATH.
func loadKeyringFile(name string) (map[string]string, error) {
	path := name + ".keyring.pekit.toml"
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--keyring=%s: %w", name, err)
	}
	var raw map[string]any
	if _, err := toml.Decode(string(data), &raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := map[string]string{}
	if err := flattenKeyring(path, "", raw, out); err != nil {
		return nil, err
	}
	return out, nil
}

// flattenKeyring walks a decoded keyring table, emitting PEKIT_KEYRING_<NAME>
// for each string leaf (name = dotted key path, uppercased, non-alphanumerics
// to '_'). Tables recurse; any other leaf type is an error.
func flattenKeyring(path, prefix string, m map[string]any, out map[string]string) error {
	for _, k := range sortedKeys(m) {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch val := m[k].(type) {
		case string:
			out["PEKIT_KEYRING_"+envTargetName(key)] = val
		case map[string]any:
			if err := flattenKeyring(path, key, val, out); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s: key %q must be a string or table, got %T", path, key, val)
		}
	}
	return nil
}

// ParseConfig parses pekit.toml source. Every section comes in exactly one
// of two shapes: bare ([build] with fields directly on it, sugar for a
// single target named "main") or named ([build.<name>] subtables). Mixing
// the shapes within a section is an error. Sections are independent — one
// may be bare while another is named.
func ParseConfig(src string) (*Config, error) {
	var raw map[string]any
	md, err := toml.Decode(src, &raw)
	if err != nil {
		return nil, err
	}

	cfg := &Config{ClearOut: true, Commands: make(map[string]map[string]Target)}
	clearOutSet := false

	keys := sortedKeys(raw)
	for _, key := range keys {
		if table, isTable := raw[key].(map[string]any); isTable {
			switch key {
			case "build", "test", "install", "clean":
				targets, err := parseSection(key, table, parseTarget)
				if err != nil {
					return nil, err
				}
				cfg.Commands[key] = targets
			case "package":
				return nil, fmt.Errorf("[package] has moved to package.pekit.toml")
			case "env":
				env, err := parseEnv(table, md)
				if err != nil {
					return nil, err
				}
				cfg.Env = env
			case "source":
				source, err := parseSource(table)
				if err != nil {
					return nil, err
				}
				cfg.Source = source
			default:
				return nil, fmt.Errorf("unknown section %q", key)
			}
			continue
		}

		switch key {
		case "outDir":
			s, ok := raw[key].(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("outDir must be a non-empty string")
			}
			// pekit clean does RemoveAll(outDir); these values would
			// make that "delete the project" (or worse).
			switch filepath.Clean(s) {
			case ".", "..", "/":
				return nil, fmt.Errorf("outDir must name a subdirectory (got %q)", s)
			}
			cfg.OutDir = s
		case "clearOut":
			b, ok := raw[key].(bool)
			if !ok {
				return nil, fmt.Errorf("clearOut must be a boolean")
			}
			cfg.ClearOut = b
			clearOutSet = true
		case "build", "test", "install", "clean", "package", "env", "source":
			return nil, fmt.Errorf("[%s] must be a table", key)
		default:
			return nil, fmt.Errorf("unknown root key %q", key)
		}
	}

	if clearOutSet && cfg.OutDir == "" {
		return nil, fmt.Errorf("clearOut requires outDir to be set")
	}
	if cfg.Source != nil && cfg.OutDir == "" {
		return nil, fmt.Errorf("[source] requires outDir to be set (the checkout lands under it)")
	}
	// needs always names a build target (in any section), so dependency
	// validation runs once, after every section is parsed.
	if err := validateDeps(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// parseSource parses a [source] block. A source provides git (then rev is
// required; rev is verbatim — any git-checkout-able ref) and/or localpath
// (a working copy for --local). At least one is required; a recipe may
// carry both (git for the farm, localpath for dev). rev is reproducible
// only as far as an immutable ref is passed, which an orchestrator does.
func parseSource(table map[string]any) (*Source, error) {
	var src Source
	for _, key := range sortedKeys(table) {
		switch key {
		case "git":
			s, err := stringValue("source", key, table[key])
			if err != nil {
				return nil, err
			}
			src.Git = s
		case "rev":
			s, err := stringValue("source", key, table[key])
			if err != nil {
				return nil, err
			}
			src.Rev = s
		case "url":
			s, err := stringValue("source", key, table[key])
			if err != nil {
				return nil, err
			}
			src.URL = s
		case "extract":
			b, ok := table[key].(bool)
			if !ok {
				return nil, fmt.Errorf("[source]: extract must be a boolean")
			}
			src.Extract = b
		case "localpath":
			s, err := stringValue("source", key, table[key])
			if err != nil {
				return nil, err
			}
			src.LocalPath = s
		case "versions":
			s, err := stringValue("source", key, table[key])
			if err != nil {
				return nil, err
			}
			if _, err := semver.NewConstraint(s); err != nil {
				return nil, fmt.Errorf("[source]: versions %q is not a valid semver constraint: %w", s, err)
			}
			src.Versions = s
		case "tag_regex":
			s, err := stringValue("source", key, table[key])
			if err != nil {
				return nil, err
			}
			if _, err := regexp.Compile(s); err != nil {
				return nil, fmt.Errorf("[source]: tag_regex %q is not a valid regexp: %w", s, err)
			}
			src.TagRegex = s
		case "file_regex":
			s, err := stringValue("source", key, table[key])
			if err != nil {
				return nil, err
			}
			if _, err := regexp.Compile(s); err != nil {
				return nil, fmt.Errorf("[source]: file_regex %q is not a valid regexp: %w", s, err)
			}
			src.FileRegex = s
		default:
			return nil, fmt.Errorf("[source]: unknown key %q", key)
		}
	}
	if src.Git == "" && src.URL == "" && src.LocalPath == "" {
		return nil, fmt.Errorf("[source]: needs %q, %q or %q", "git", "url", "localpath")
	}
	if src.Git != "" && src.URL != "" {
		return nil, fmt.Errorf("[source]: %q and %q are mutually exclusive (git clone vs. download)", "git", "url")
	}
	if src.Git != "" && src.Rev == "" {
		return nil, fmt.Errorf("[source]: %q requires %q", "git", "rev")
	}
	if src.URL != "" && src.Rev != "" {
		return nil, fmt.Errorf("[source]: %q does not apply to a %q source", "rev", "url")
	}
	if src.Extract && src.URL == "" {
		return nil, fmt.Errorf("[source]: %q only applies to a %q source", "extract", "url")
	}
	if src.FileRegex != "" && src.URL == "" {
		return nil, fmt.Errorf("[source]: %q only applies to a %q source (a git source filters with %q)", "file_regex", "url", "tag_regex")
	}
	if src.TagRegex != "" && src.URL != "" {
		return nil, fmt.Errorf("[source]: %q does not apply to a %q source (use %q)", "tag_regex", "url", "file_regex")
	}
	return &src, nil
}

// parseSection applies the shared shape rules (bare vs named, never mixed)
// and parses each target table with parseOne.
func parseSection[T any](section string, table map[string]any, parseOne func(string, map[string]any) (T, error)) (map[string]T, error) {
	// Subtables are named targets; scalar keys are bare-form target fields.
	// The presence of both means the two shapes are mixed.
	var fields, subtables []string
	for key, val := range table {
		if _, isTable := val.(map[string]any); isTable {
			subtables = append(subtables, key)
		} else {
			fields = append(fields, key)
		}
	}
	sort.Strings(fields)
	sort.Strings(subtables)

	if len(fields) > 0 && len(subtables) > 0 {
		return nil, fmt.Errorf(
			"[%s] mixes a bare target (key %q) with named targets ([%s.%s]); use one shape or the other",
			section, fields[0], section, subtables[0])
	}

	targets := make(map[string]T)

	if len(subtables) == 0 {
		target, err := parseOne(section, table)
		if err != nil {
			return nil, err
		}
		targets["main"] = target
		return targets, nil
	}

	for _, name := range subtables {
		target, err := parseOne(section+"."+name, table[name].(map[string]any))
		if err != nil {
			return nil, err
		}
		targets[name] = target
	}
	return targets, nil
}

func parseTarget(section string, table map[string]any) (Target, error) {
	var target Target
	for _, key := range sortedKeys(table) {
		switch key {
		case "command":
			s, err := stringValue(section, key, table[key])
			if err != nil {
				return target, err
			}
			target.Command = s
		case "needs":
			vals, ok := table[key].([]any)
			if !ok {
				return target, fmt.Errorf("[%s]: needs must be an array of target names", section)
			}
			for _, v := range vals {
				s, ok := v.(string)
				if !ok || s == "" {
					return target, fmt.Errorf("[%s]: needs must be an array of non-empty target names", section)
				}
				target.Needs = append(target.Needs, s)
			}
		default:
			return target, fmt.Errorf("[%s]: unknown key %q", section, key)
		}
	}
	if target.Command == "" {
		return target, fmt.Errorf("[%s]: missing required key %q", section, "command")
	}
	return target, nil
}

// envTargetName maps a target name to the variable component of its
// PEKIT_<NAME>_OUT env var: uppercased, with every byte that is not a letter
// or digit replaced by '_'. The constant PEKIT_ prefix keeps the result a
// valid sh identifier even when the name starts with a digit.
func envTargetName(name string) string {
	b := make([]byte, 0, len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z':
			b = append(b, c-('a'-'A'))
		case (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'):
			b = append(b, c)
		default:
			b = append(b, '_')
		}
	}
	return string(b)
}

// validateDeps checks every section's dependency graph after all sections are
// parsed. A `needs` entry always names a build target — regardless of the
// section it is declared in — so existence is checked against [build]. The
// PEKIT_<NAME>_OUT collision check applies to every section, since each exports
// its direct needs' staged outputs into the command env. Cycles are only
// possible within [build] (test/install/clean needs point outward into build,
// and build nodes are never depended on from outside), so cycle detection runs
// on the build section alone.
func validateDeps(cfg *Config) error {
	build := cfg.Commands["build"]
	// Fixed section order keeps a reported error deterministic.
	for _, section := range []string{"build", "test", "install", "clean"} {
		targets, ok := cfg.Commands[section]
		if !ok {
			continue
		}
		for _, name := range sortedNames(targets) {
			seen := map[string]string{} // env var component -> dep that produced it
			for _, dep := range targets[name].Needs {
				if _, ok := build[dep]; !ok {
					return fmt.Errorf("[%s.%s]: needs %q, which is not a build target", section, name, dep)
				}
				ev := envTargetName(dep)
				if prev, dup := seen[ev]; dup {
					return fmt.Errorf("[%s.%s]: needs %q and %q both map to PEKIT_%s_OUT; rename one", section, name, prev, dep, ev)
				}
				seen[ev] = dep
			}
		}
	}
	return validateBuildCycles(build)
}

// validateBuildCycles fails on a dependency cycle among build targets. needs
// edges only ever point into [build], so [build] is the only section that can
// contain one.
func validateBuildCycles(targets map[string]Target) error {
	// DFS three-colouring. Sorted starts keep the reported cycle deterministic.
	const (
		white = iota
		gray
		black
	)
	color := map[string]int{}
	var visit func(string) error
	visit = func(n string) error {
		color[n] = gray
		for _, dep := range targets[n].Needs {
			switch color[dep] {
			case gray:
				return fmt.Errorf("[build]: dependency cycle through %q", dep)
			case white:
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		color[n] = black
		return nil
	}
	for _, name := range sortedNames(targets) {
		if color[name] == white {
			if err := visit(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// envNameRe matches valid sh identifiers. Anything else in an export
// line would be a syntax error at best and script injection at worst.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseEnv parses [env] preserving document order (TOML maps lose it,
// so order comes from the decoder metadata). Values are verbatim sh
// double-quoted-string contents; only names are validated.
func parseEnv(table map[string]any, md toml.MetaData) ([]EnvVar, error) {
	var env []EnvVar
	for _, key := range md.Keys() {
		if len(key) != 2 || key[0] != "env" {
			continue
		}
		name := key[1]
		if !envNameRe.MatchString(name) {
			return nil, fmt.Errorf("[env]: invalid variable name %q", name)
		}
		if name == "PEKIT_OUT" {
			return nil, fmt.Errorf("[env]: PEKIT_OUT is set by pekit and cannot be overridden")
		}
		val, ok := table[name].(string)
		if !ok {
			return nil, fmt.Errorf("[env]: %s must be a string", name)
		}
		env = append(env, EnvVar{Name: name, Value: val})
	}
	return env, nil
}

func stringValue(section, key string, val any) (string, error) {
	s, ok := val.(string)
	if !ok {
		return "", fmt.Errorf("[%s]: %s must be a string", section, key)
	}
	if s == "" {
		return "", fmt.Errorf("[%s]: %s must not be empty", section, key)
	}
	return s, nil
}

func sortedKeys(table map[string]any) []string {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedNames[T any](targets map[string]T) []string {
	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
