# Pekit

Pekit v2 is a recipe-driven build and packaging tool. It reads declarative
recipe files, resolves source/version/package selections, builds an explicit
plan, and executes that plan with predictable staging, environment, and artifact
behavior.

The main design goal is predictability. Every command declares which flags it
accepts, every source type declares what capabilities it supports, and every
package operation is planned before side effects happen.

Pekit v2 should preserve useful current Pekit behavior, but it should not
preserve accidental behavior by default. Current behavior is reference material,
not a sacred contract.

## Goals

- Make command behavior predictable and easy to explain.
- Make flags command-scoped, with early errors for unsupported combinations.
- Separate planning from execution so users can understand what Pekit will do
  before it does it.
- Represent git, URL, and local sources through one source capability model.
- Make package discovery, inheritance, and merge precedence explicit.
- Keep recipe formats strict: unknown keys and ambiguous shapes should fail
  early.
- Keep builds reproducible where possible, and clearly mark non-reproducible
  local or unanchored builds.
- Support workspaces as orchestration over normal recipe operations, not as a
  separate behavior universe.
- Keep the implementation modular enough that adding new source types, package
  formats, publish targets, or planners does not require touching CLI dispatch.

## Non-Goals

- Pekit is not a general-purpose shell task runner.
- Pekit is not a full package manager or dependency solver.
- Pekit is not responsible for building every possible upstream project model
  directly; recipes remain responsible for the actual build commands.
- Pekit should not silently accept flags that do nothing.
- Pekit should not rely on command-specific special cases when a generic model
  can express the behavior.
- Pekit v2 does not need to be bug-for-bug compatible with current Pekit.

## Core Model

The core pipeline is:

```text
argv
  -> Invocation
  -> RecipeSet
  -> SourceResolution
  -> VersionSelection
  -> Plan
  -> Executor
  -> Artifacts
```

The important boundary is between `Plan` and `Executor`. Before any side
effects happen, Pekit should know:

- Which recipe or workspace members are selected.
- Which source trees are needed.
- Which versions are selected.
- Which build targets will run or be reused.
- Which packages will be produced.
- Which artifacts will be published.
- Which environment, keyring, and wrapper inputs apply to each command.

## Design Principles

### Explicit Capabilities

Commands, sources, package formats, and publish targets should declare their
capabilities.

Examples:

- A command declares whether it accepts flags such as `--version`, `--local`,
  `--prefer-local`, `--no-build`, `--env`, `--keyring`, `--refresh-source`,
  and `--dry-run`.
- A source declares whether it can be materialized, enumerate versions, provide
  provenance, or support local development.
- A package format declares which manifest fields it supports.

Unsupported combinations should fail during invocation validation, not halfway
through execution.

### Plan Before Execute

Pekit v2 should turn a request into a concrete plan before running shell
commands, cloning repositories, downloading archives, or writing packages.

The plan should be inspectable by humans and testable without running the
world. User-facing dry-run output should fall naturally out of the architecture.

### One Source Model

Git, URL, and local sources should be variants of one source model instead of
branches scattered through command execution.

The model should answer:

- How is this source materialized?
- Can this source enumerate versions?
- What cache key does this source use?
- What provenance can this source provide?
- Does this source have a local development override?

### Strict Inputs

Recipe files should be strict. Unknown keys, misspelled sections, mixed shapes,
and ambiguous configuration should error early.

Strictness is part of predictability. If Pekit accepts input, users should be
able to assume it had meaning.

### Generic Over Specific

Special behavior should be expressed as generic policy where possible.

Examples:

- Command flag applicability should be a command capability table, not scattered
  `if command == ...` checks.
- Package inheritance should be a named merge policy, not ad hoc layering.
- Workspace behavior should reuse normal recipe planning, not duplicate command
  semantics.

## Command Model

Commands should parse into a typed invocation before any recipe loading or side
effects.

Candidate commands:

- `build`
- `test`
- `install`
- `clean`
- `package`
- `publish`
- `workspace`

Each command should define:

- Its positional arguments.
- Its accepted flags.
- Its required capabilities.
- Whether it selects targets, packages, workspace members, or artifacts.
- Whether it can run over one version or many versions.
- Whether it can run in dry-run mode without unresolved side-effect-dependent
  nodes.

Unsupported flags should be rejected with command-specific errors.

### Flag Strictness

Pekit v2 should be strict by default:

- Unknown flags are errors.
- Malformed flag values are errors.
- Recognized flags that are not supported by the selected command are errors.
- Recognized flags that are supported by the command but invalid for the loaded
  recipe are errors.

This avoids current behavior where a flag can be accepted, parsed, and then have
no effect.

### `--allow-unused`

Pekit v2 should have a global `--allow-unused` escape hatch for automation.

Build farms often want to pass one large flag set to many commands or members.
`--allow-unused` allows that without making ordinary interactive use vague.

Semantics:

- `--allow-unused` only suppresses errors for recognized flags that are valid in
  Pekit but unused by the selected command.
- In workspace fan-out, `--allow-unused` may also suppress a per-member missing
  target or package selector when that selector is valid for at least one other
  selected member and the delegated command supports that selector kind.
- Unknown flags still error.
- Malformed flag values still error.
- Selectors with invalid syntax still error.
- A selector that matches no selected workspace member still errors.
- A flag that is supported by the selected command still takes effect and can
  still error normally.
- Suppressed flags or selectors should be reported in dry-run output, and may
  be warned about in normal execution.

Example:

```text
pekit clean --version 1.2.3
```

errors because `clean` does not support version selection.

```text
pekit clean --version 1.2.3 --allow-unused
```

ignores the recognized but unused `--version` flag.

But:

```text
pekit clean --verison 1.2.3 --allow-unused
```

still errors because `--verison` is unknown.

### CLI Grammar

Top-level grammar:

```text
pekit [global flags] <command> [command args and flags]
pekit [global flags] workspace [workspace flags] <command> [command args and flags]
```

Global flags:

- `--recipe <path>` or `--recipe=<path>`
- `--workspace <path>` or `--workspace=<path>`
- `--allow-unused`
- `--dry-run`
- `--quiet`
- `--verbose`
- `--json`

Global flags may appear before or after the command. They still obey command
capability validation where relevant.

Required-value flags support both separated and equals forms:

```text
--version 1.2.3
--version=1.2.3
--recipe /path/to/pkg
--recipe=/path/to/pkg
```

Optional-value flags only accept values with `=`:

```text
--local
--local=../src
--prefer-local
--prefer-local=../src
--no-build
--no-build=main,tools
```

Optional-value flags must not consume the next token. This keeps parsing
unambiguous:

```text
pekit build main --local package
```

means selectors `main` and `package` with local mode enabled; it does not mean
`--local=package`.

Repeatable flags preserve CLI order. This is required for keyring overlay:

```text
--keyring=base --keyring=prod --keyring.tcb.priv=/override
```

`--` stops Pekit flag parsing:

```text
pekit build -- --target-named-like-a-flag
```

Initial v2 does not treat tokens after `--` as arbitrary passthrough arguments
to target commands. Recipe target commands remain recipe-defined. Remaining
tokens are parsed as positional selectors only.

Selector names should reject leading `-` in canonical v2 recipe/package names
so `--` is rarely needed.

Workspace grammar:

```text
pekit workspace [workspace flags] <command> [command args and flags]
```

Workspace flags must appear after `workspace` and before the delegated command:

```text
pekit workspace --jobs 4 --fail-fast package --all
```

This is intentionally valid:

```text
pekit --dry-run workspace --jobs 4 package --all
```

This is invalid:

```text
pekit workspace package --all --jobs 4
```

Pekit should error with a hint to move `--jobs` before the delegated command.
This keeps ownership of workspace flags and delegated command flags strict and
leaves room for future command-specific flags with the same names.

### `--dry-run`

Pekit v2 should use a global `--dry-run` execution mode rather than a separate
`plan` command.

Examples:

```text
pekit package libc --version 2.43 --dry-run
pekit build main --local --dry-run
pekit workspace package --all --dry-run
```

Semantics:

- Parse and validate exactly like the real command.
- Build the same internal plan the real command would execute.
- Print the plan.
- Do not run shell commands.
- Do not write package artifacts.
- Do not publish artifacts.
- Do not remove managed output directories.
- Do not mutate state.

Discovery behavior:

- Dry-run may do non-mutating discovery needed for an accurate plan, such as
  version enumeration from git tags or URL listings.
- Dry-run may read existing local files and already-cached source trees.
- Dry-run should not clone, download, extract, or otherwise materialize missing
  sources by default.
- If a complete plan requires a side effect, dry-run should show an unresolved
  node instead of doing the side effect.

Examples of unresolved dry-run nodes:

- delegated package discovery when the source is not cached
- package instances produced by `[multipack.enum.files]` before the build
  target has run
- file glob expansion inside a missing build stage

### Output Renderers

Pekit should have one internal diagnostic/event model and multiple renderers.
Commands should emit structured events; human output, quiet output, verbose
output, and JSON output are renderers over those events.

Initial global output flags:

```text
--quiet
--verbose
--json
```

Rules:

- `--quiet`, `--verbose`, and `--json` are accepted by every command.
- `--quiet` and `--verbose` are mutually exclusive.
- `--json` selects the JSON renderer.
- `--verbose --json` is valid and emits additional debug events and fields.
- `--quiet --json` is invalid because JSON consumers should receive the full
  structured result. Automation can filter events itself.

Human renderer:

- Pekit diagnostics go to stderr.
- Target stdout and stderr stream live to their normal streams.
- Default output shows timestamped Pekit events and live target output.
- In simple single-target runs, target output may remain mostly raw.
- In workspace, multi-version, or concurrent runs, target output should be line
  prefixed with enough context to identify the source.

Example prefix:

```text
[libc build:main 2.43] checking for gcc...
```

Quiet renderer:

- Suppress progress.
- Keep warnings, errors, artifact paths, and the final summary.
- Do not suppress target output from commands that run, unless a future explicit
  target-output flag is added.

Verbose renderer:

- Emit detailed planning and execution diagnostics intended for debugging.
- Show selected recipe files and workspace members.
- Show loaded package layers and merge decisions.
- Show selected versions, version candidates, and version caps.
- Show source cache keys, cache policy, and cache decisions.
- Show source materialization paths.
- Show target graphs, dependency edges, reuse decisions, stage paths, and
  working directories.
- Show wrapper files, wrapper command shape, and final command strings.
- Show environment variable names and their source.
- Show package file mappings after source-ref normalization.
- Never print keyring values.
- Redact Pekit-known secret values in all renderers.

JSON renderer:

- `--dry-run --json` prints one JSON plan object to stdout.
- Executing with `--json` prints newline-delimited JSON events to stdout.
- With `--json`, Pekit does not pass raw target output through. Target stdout
  and stderr are represented as JSON events so the stream stays parseable.
- Stderr should be reserved for failures that happen before the JSON renderer is
  initialized.

Example execution events:

```json
{"type":"target_start","time":"2026-06-18T10:22:00Z","member":"libc","version":"2.43","target":"main"}
{"type":"target_output","stream":"stdout","target":"main","text":"checking for gcc...\n"}
{"type":"target_success","target":"main","duration_ms":1234}
{"type":"artifact","package":"libc","path":"out/.../libc_2.43_x86_64.peipkg"}
```

The exact JSON schema is implementation-owned, but the contract is part of
initial v2: JSON is not a later build-farm add-on.

### Diagnostics

Diagnostics should be structured before they are rendered.

Each warning or error should carry:

- stable diagnostic code
- severity
- short message
- command/member/version/target/package context when applicable
- relevant path when applicable
- underlying process exit code when applicable
- one actionable hint when Pekit can provide one confidently

Warnings should be deduplicated by diagnostic code and context where possible.

Workspace runs should always finish with a summary event/report:

- members succeeded
- members failed
- members skipped or unresolved
- artifacts produced
- first failure, if `--fail-fast` stopped the run

### Build Ledger

Current Pekit has `--remember-built`, `--bust`, and `pekit.built`. Pekit v2
should drop this feature for now.

The likely user is a build farm, and the right shape is not clear yet. When
build-farm behavior is designed, it should get a first-class model instead of
inheriting the current ledger flags.

Initial stance:

- Remove `--remember-built`.
- Remove `--bust`.
- Do not implement `pekit.built` compatibility in the first v2 design.
- Revisit build-farm state once the farm requirements are real.

### Clean Command

`clean` should become explicit and consistent.

Default behavior:

```text
pekit clean
```

- If a default clean target exists, run it.
- Remove Pekit's managed output directory.
- If no clean target exists, just remove managed output.

Named behavior:

```text
pekit clean <target>
```

- Run the named clean target.
- Remove Pekit's managed output directory.

Additional modes:

```text
pekit clean --output-only
```

- Remove only Pekit's managed output directory.
- Do not run clean targets.

```text
pekit clean --target-only <target>
```

- Run only the selected clean target.
- Do not remove Pekit's managed output directory.

If `--target-only` is used without a positional target, Pekit runs the default
clean target if one exists. If no default clean target exists, planning errors.

Clean target selection should be reviewed separately from build/test/install
target selection. Current Pekit does not fan out across named clean targets on a
bare `pekit clean`, and v2 should probably keep clean conservative unless there
is a clear use case for fan-out.

Because clean targets are shell commands, they should support the same command
environment mechanics as other shell-running commands:

- env files
- wrappers
- keyrings
- timestamps

Those mechanics should apply only when a clean target actually runs. For
`--output-only`, env/wrapper/keyring flags are unused and require
`--allow-unused` if supplied.

### Command and Flag Matrix

This is the first-pass v2 command contract.

| Command | Selects | Version selection | Local source | Build reuse | Env/wrap/keyring | Side effects |
| --- | --- | --- | --- | --- | --- | --- |
| `build` | build targets | yes | yes | yes | yes | runs build targets |
| `test` | test targets | yes, single resolved version | yes | yes, for needed builds | yes | stages needed builds, runs tests |
| `install` | install targets | yes, single resolved version | yes | yes, for needed builds | yes | stages needed builds, runs installs |
| `package` | packages | yes, multiple versions | yes | yes | yes | stages builds, writes package artifacts |
| `publish` | packages | yes, multiple versions | yes | yes | yes | stages builds, writes packages, publishes artifacts |
| `clean` | clean target and/or managed output | no | no | no | only when a clean target runs | runs clean target and/or removes managed output |
| `workspace` | workspace members plus a delegated command | delegated | delegated | delegated | delegated | delegates normal operations per member |

Version behavior:

- `build`, `package`, and `publish` may operate on multiple selected versions.
- `test` and `install` may accept version selection, but it must resolve to one
  version. If a selector expands to multiple versions, that is an error unless a
  future explicit multi-test/multi-install mode is added.
- `clean` does not support version selection in the initial v2 design.

### Target Selection

Target selection should be explicit and conservative. Pekit should not fan out
across every target by default just because multiple targets exist.

Target names:

- Canonical target names match `[A-Za-z0-9_.-]+`.
- Target names must not contain `/` or `:`.
- Target names must not start with `-`.
- Bare `[build]`, `[test]`, `[install]`, or `[clean]` means target `main`.
- Named targets use `[build.<name>]`, `[test.<name>]`, `[install.<name>]`, or
  `[clean.<name>]`.
- Mixing bare and named targets in the same command section is an error.

Build:

```text
pekit build
```

runs `build.main` if it exists.

```text
pekit build main tools
```

runs the selected build targets and their build dependencies.

If no `build.main` exists and multiple build targets exist, bare `pekit build`
errors and lists available targets. Use explicit target selectors.

Test:

```text
pekit test
pekit test unit integration
```

Bare `pekit test` runs `test.main` if it exists. Explicit selectors run the
selected test targets and their required build dependencies.

If no `test.main` exists, bare `pekit test` errors and lists available targets.

Install:

```text
pekit install
pekit install cli service
```

Bare `pekit install` runs `install.main` if it exists. Explicit selectors run
the selected install targets and their required build dependencies.

If no `install.main` exists, bare `pekit install` errors and lists available
targets.

Clean:

```text
pekit clean
```

runs `clean.main` if it exists, then removes managed output. If no `clean.main`
exists, it only removes managed output.

```text
pekit clean generated
```

runs the selected clean target, then removes managed output.

```text
pekit clean --target-only
```

runs `clean.main` only and errors if it does not exist.

```text
pekit clean --output-only
```

removes managed output only.

Clean accepts zero or one target in initial v2. Multiple clean target selectors
are an error unless a future use case justifies explicit clean fan-out.

Dependencies:

- `needs` always names build targets.
- Build target `needs` are build dependencies.
- Test and install target `needs` select build targets required before the
  target command runs.
- Package `builds` select build targets required before packaging.
- `needs` cannot reference test, install, or clean targets.
- Dependency cycles error with the cycle path.

Selected targets execute in dependency order. Independent selected targets may
be parallelized later, but initial v2 may execute them sequentially.

### Workspace Invocation

Workspace keeps an ergonomic command shape:

```text
pekit workspace <command> [command args and flags]
```

Examples:

```text
pekit workspace package --all
pekit workspace publish --latest
pekit workspace build main --local
pekit workspace test unit --version 1.2.3
pekit workspace clean --output-only
```

Rules:

- After `workspace`, the next token is a normal Pekit command.
- The remaining args are parsed as that command's args and flags, plus
  workspace-level flags.
- Workspace planning discovers members from `workspace.pekit.toml`.
- Each member gets a normal per-recipe invocation.
- Workspace should not duplicate command semantics.

Initial workspace flags:

```text
--fail-fast
--jobs N
```

Semantics:

- `--fail-fast` stops after the first failed member.
- Without `--fail-fast`, workspace attempts every selected member and reports a
  summary of failures.
- `--jobs N` allows parallel member execution.
- `N` must be a positive integer.
- The default is `--jobs 1`.
- `--jobs` controls member concurrency only. It does not enable parallel target
  execution inside one recipe.

Do not add member override flags initially. Member selection comes from
`workspace.pekit.toml` include/exclude. Add CLI member filters later if there is
a concrete need.

Strictness:

- Delegated command flags are validated per member.
- If a flag is unsupported for a member, that member errors.
- `--allow-unused` can suppress recognized but unused flags per member.

Example:

```text
pekit workspace package --all-versions
```

If a sourceless member cannot support `--all-versions`, that member errors
unless `--allow-unused` is supplied. This intentionally replaces current Pekit's
workspace-specific sourceless special case with the general strict flag model.

### Workspace Execution

Workspace execution is orchestration over normal per-recipe plans.

Member identity:

- Each member has a stable member id: its workspace-relative directory path.
- Member ids are used in diagnostics, JSON events, line prefixes, and summaries.
- Workspace member order is the sorted order produced by `workspace.pekit.toml`
  discovery.

Planning:

- Workspace discovery and CLI validation happen once before member execution.
- Each member receives a normal per-recipe invocation with the delegated command
  and flags.
- Workspace planning should preserve member order in the human dry-run and JSON
  plan.
- Per-member planning failures are member failures. They do not abort the whole
  workspace unless `--fail-fast` is active.
- Workspace planning should detect cross-member publish destination collisions
  when destination paths are known before execution. Unresolved destinations are
  checked when they resolve.

Scheduling:

- `--jobs N` runs up to `N` members at the same time.
- A member worker owns planning and execution for that member.
- Inside a member, target/package execution follows the normal per-recipe plan.
- Initial v2 does not include inter-member dependencies.
- Independent members may complete in any order when `--jobs > 1`.

Failure policy without `--fail-fast`:

- Every selected member is attempted.
- Failed members do not prevent pending members from starting.
- The workspace command fails at the end if any member failed.
- The final summary lists succeeded, failed, unresolved, and skipped members in
  stable member order.

Failure policy with `--fail-fast`:

- When the first member failure is observed, no new members are started.
- Members already running are allowed to finish in initial v2.
- Pending members that were not started are marked skipped because of
  fail-fast.
- The final summary reports the first failure, completed running members, and
  skipped pending members.

Do not add cancellation of already-running member processes in initial v2.
Stopping a shell command tree portably is a separate process-management design.

Human output:

- Workspace progress diagnostics are timestamped and include the member id.
- Target stdout/stderr from workspace members is line-prefixed with member
  context, even when `--jobs 1`, so logs remain unambiguous.
- With `--jobs > 1`, output may interleave by line.
- Partial lines should be buffered until newline where practical; if a process
  exits with an unterminated line, flush the partial line with its prefix.

Example prefix:

```text
[pkgs/libc build:main 2.43] checking for gcc...
```

JSON output:

- Workspace JSON execution output remains newline-delimited events.
- Each event includes `member` and enough delegated-command context to identify
  what produced it.
- Runtime events are emitted in actual occurrence order.
- Summary events include member results in stable member order.

Exit status:

- Invocation, parse, and workspace-discovery errors are command errors.
- If one or more members fail during planning or execution, the workspace
  command returns a workspace failure after reporting the summary.
- Skips caused by fail-fast do not count as independent member failures, but
  the workspace command still fails because the first failure failed.

### `--no-build`

Keep the current flag name for familiarity.

Semantics:

```text
--no-build
```

Reuse any required build target whose staged output already exists. If a
required target is not staged, build it.

```text
--no-build=target1,target2
```

Reuse only the named required build targets when they are already staged. Other
required build targets build normally.

Rules:

- `--no-build` applies only to build targets.
- Named targets must exist, otherwise the invocation errors.
- A reused target prunes its build dependency subtree.
- The plan should show which targets will be reused and which will be built.
- The name is historical: `--no-build` does not mean "never build anything";
  it means "do not rebuild already-staged selected outputs".

Possible future strict mode:

```text
--require-built
--require-built=target1,target2
```

This would mean "never build selected targets; error if staged output is
missing." Do not add it until there is a real use case.

### Keyring Flags

Keep the current keyring syntax:

```text
--keyring=prod
--keyring.tcb.priv=/secure/key.pem
```

`--keyring.<path>=<value>` injects one literal keyring value. The dotted path is
sanitized into the exported name:

```text
--keyring.tcb.priv=/secure/key.pem
  -> PEKIT_KEYRING_TCB_PRIV=/secure/key.pem
```

`--keyring=<value>` loads a keyring file.

Resolution:

1. If `<value>` is path-like, treat it as a filesystem path immediately.
2. Otherwise search keyring locations for `<value>.keyring.pekit.toml`.
3. If named lookup fails, treat `<value>` as a filesystem path.
4. If no file is found, error.

A value is path-like if it:

- contains `/`
- starts with `.`
- starts with `~`
- is absolute on the host platform

Examples:

```text
--keyring=prod
```

searches for `prod.keyring.pekit.toml`, then falls back to path `prod`.

```text
--keyring=./secrets/prod.toml
--keyring=/run/secrets/pekit/prod.toml
--keyring=../shared/prod.keyring.pekit.toml
```

are treated as paths directly.

Precedence:

```text
first keyring file < later keyring file < literal --keyring.x.y=value
```

Example:

```text
--keyring=base --keyring=prod --keyring.tcb.priv=/override
```

means `base` loads first, `prod` overrides it, then the literal CLI value wins.

Workspace behavior:

- Workspace commands should resolve keyring files once at the workspace
  invocation level.
- Members should receive resolved literal values, not unresolved relative
  keyring paths.
- This prevents relative keyring paths from changing meaning per member.

## Source Model

A source is the thing Pekit builds from. A recipe may have no external source,
one external source, or a local development source override.

Source kinds:

- `git`
- `url`
- `local`

Capabilities:

- `materialize`: produce a source tree or source file area.
- `enumerate`: list available upstream versions.
- `versioned`: render version templates into a concrete source reference.
- `provenance`: provide a stable source reference for package metadata.
- `localOverride`: map a reproducible source to a local working copy for
  development.

The source system should make fallback behavior explicit. For example, a
request for local development may either:

- require local source strictly, or
- prefer local source and fall back to remote source.

That policy should be visible in the invocation or recipe, not hidden inside
source materialization.

### Source Roles

Pekit v2 should distinguish these ideas:

- `recipe root`: the directory containing the recipe files being invoked.
- `source root`: the materialized upstream source tree or local working copy.
- `literal root`: where literal package file sources resolve.
- `provenance root`: the tree or reference used for package provenance.

Current Pekit often lets these collapse together. V2 should name them so each
command can be reasoned about.

For a sourceless recipe:

```text
recipe root = source root = literal root = provenance root
```

For an external source recipe:

```text
recipe root = packaging/build recipe
source root = fetched git checkout, extracted URL tree, or local working copy
literal root = source root for delegate packages unless overridden
provenance root = source root for git sources, recipe root for non-git sources
```

The exact provenance rule can change, but it should be explicit.

### Source Definition

The source definition describes one reproducible source and, optionally, one
local development override.

Decision: v2 uses typed source subtables.

Git source:

```toml
[source.git]
url = "https://github.com/example/app.git"
ref = "v{{version}}"
versions = ">= 1.0"
tag_regex = '^v\d+\.\d+\.\d+$'

[source.local]
path = "../app"
```

URL source:

```toml
[source.url]
url = "https://ftp.gnu.org/gnu/gmp/gmp-{{version}}.tar.xz"
extract = true
root = "gmp-{{version}}"
versions = ">= 6.3"
file_regex = '^gmp-\d+\.\d+\.\d+\.tar\.xz$'
checksum = "sha256:..."

[source.local]
path = "../gmp"
```

Rules:

- Exactly one reproducible source table may be present: `[source.git]` or
  `[source.url]`.
- `[source.local]` is optional and acts as a local override.
- `[source.local]` alone is valid only for dev-only recipes. Commands that need
  a reproducible source must require `--local` when no reproducible source is
  declared.
- `--local` uses `[source.local]` and errors if it is absent or unusable.
- `--local=<path>` uses the CLI path as an invocation-local source override.
- `--prefer-local` uses `[source.local]` if usable, otherwise falls back to the
  reproducible source.
- `--prefer-local=<path>` uses the CLI path if usable, otherwise falls back to
  the reproducible source.

This table shape is preferred over `type = "git"` because invalid mixed-source
states are easier to detect and future source kinds can be added naturally:

```toml
[source.registry]
...

[source.artifact]
...
```

### Source Capabilities

Each source kind should implement these capabilities where applicable:

```text
render(version) -> concrete source
materialize(concrete source, cache policy) -> source root
enumerate() -> available versions
cacheKey(concrete source) -> stable cache scope
provenance(source root) -> provenance metadata
```

Capability rules:

- Git sources can materialize, enumerate tags, cache by ref/version, and provide
  git provenance.
- URL sources can materialize, enumerate from directory listings when the URL is
  templated, cache by rendered URL, and usually cannot provide upstream git
  provenance.
- Local sources can materialize by resolving a path, cannot enumerate upstream
  versions by themselves, and provide local-development provenance.
- Sourceless recipes do not use source materialization and use the recipe root
  for literal files and provenance.

If a command needs a source capability that the selected source does not
provide, planning should error.

### Template Semantics

Pekit templating is a schema-level property. Every string field is one of:

- `literal`: no Pekit template rendering.
- `template`: rendered by Pekit during a declared planning phase.
- `shell`: not Pekit-rendered; shell/env expansion happens when the command
  runs.

A field's type is part of the config schema. Pekit should not treat arbitrary
strings as potentially templated.

Template syntax:

```text
{{version}}
{{major}}
{{minor}}
{{patch}}
{{prerelease}}
{{buildmeta}}
{{multipack}}
```

Rules:

- No implicit environment variable lookup.
- No arbitrary expressions.
- No nested templates.
- Unknown variables error.
- Template syntax errors are decode errors for templated fields.
- Missing partial-version components error when rendered.
- Empty rendered values are allowed only when the field allows empty values.
- Rendered paths are normalized and re-validated after rendering.

For `literal` and `shell` fields, `{{...}}` is ordinary text unless the field
schema says it is a Pekit template. This avoids breaking shell commands,
regular expressions, or data strings that happen to contain braces.

Render phases:

| Phase | Available context |
| --- | --- |
| `recipe_load` | Static recipe/workspace context only. |
| `version_selected` | Version variables. |
| `source_materialized` | Source root and source provenance. |
| `package_instance` | Package selector and multipack instance context. |

A templated field declares its render phase. Referencing a variable unavailable
in that phase is an error.

Initial templated fields:

- `[source.git].ref`
- `[source.url].url`
- `[source.url].root`
- `[package].version`
- selected package metadata strings where version text is useful, such as
  `[package].description`
- `[files]` source refs and destinations
- `excludes`
- `[symlinks]` destinations and target text
- `[publish.localdir].path`

Version variables are available during `version_selected` and later phases.
`{{multipack}}` is available only during `package_instance` and only for package
definitions with `[multipack]`.

Source template rendering belongs to source resolution:

- The recipe is loaded far enough to know the source template and version cap.
- Version selection resolves concrete versions.
- Each concrete version renders the source into a concrete ref or URL.
- The plan contains one source materialization operation per concrete source
  cache key.

Target commands are `shell` fields, not Pekit templates. Recipes should use
Pekit-provided environment variables inside shell commands instead of
`{{...}}`.

Example:

```toml
[build.main]
command = "make VERSION=\"$PEKIT_VERSION\""
```

Deferred behavior should not be a generic string feature. A value is deferred
only because a specific plan operation is unresolved until a later phase, such
as:

- delegated package discovery before the source exists
- build output glob expansion before the build target has run
- multipack file enumeration before the relevant source/build path exists

Dry-run displays unresolved plan nodes. Execution resolves them at the required
phase.

### Version Selection

Pekit v2 keeps current Pekit's useful version semantics but makes the CLI modes
clearer.

Version forms:

```text
MAJOR[.MINOR[.PATCH]][-prerelease][+buildmeta]
```

Examples:

```text
5
2.43
0.34.0
1.36.0-rc1+build5
```

Template variables:

- `{{version}}`
- `{{major}}`
- `{{minor}}`
- `{{patch}}`
- `{{prerelease}}`
- `{{buildmeta}}`

Rules:

- `{{version}}` renders the original version text.
- Partial versions are allowed.
- Referencing a missing `minor` or `patch` component is an error.
- Unknown template variables are errors.

#### Version CLI

Initial v2 flags:

```text
--version <selector>
--latest
--all-versions
```

`--version` accepts:

- exact versions
- comma-separated exact versions
- semver constraints, if the selected source can enumerate versions

Examples:

```text
--version 2.43
--version 2.43.0,2.44.0
--version ">= 2.40, < 2.45"
```

`--latest` selects the newest enumerated version after source version caps.

`--all-versions` selects every enumerated version after source version caps.

Rules:

- A command may allow one version or multiple versions according to the command
  matrix.
- If a single-version command receives a selector that resolves to multiple
  versions, planning errors.
- `--latest`, `--all-versions`, and `--version` are mutually exclusive.
- Constraints, `--latest`, and `--all-versions` require a source that can
  enumerate versions.
- Exact versions do not require enumeration unless trailing-zero ladder
  resolution needs it and a source is available.

#### Trailing-Zero Ladder

Preserve current Pekit's ladder behavior for exact versions:

```text
2.0.0  -> 2.0.0, 2.0, 2
2.43.0 -> 2.43.0, 2.43
1.0    -> 1.0, 1
```

If the exact version has prerelease or build metadata, do not shorten it.

If a source can enumerate versions, Pekit may resolve the exact version to the
first ladder candidate that exists upstream. This supports upstreams such as
glibc that tag `2.43` while users may request `2.43.0`.

If no source can be enumerated, use the exact version literally.

#### Source Version Caps

Version caps belong to the reproducible source:

```toml
[source.git]
versions = ">= 1.0"
```

or:

```toml
[source.url]
versions = ">= 6.3"
```

The cap filters the selected version set after enumeration or exact selection.

If every selected version is filtered out, planning errors.

#### Local Version Selection

For `--local` and `--local=<path>`:

- `--version`, if present, must be an exact version.
- Constraints, `--latest`, and `--all-versions` are not supported because local
  sources cannot enumerate upstream versions.
- If no version is provided, use `0.0.0-localdev`.

For `--prefer-local` and `--prefer-local=<path>`:

- If local source is used, local version rules apply.
- If local source is unavailable and Pekit falls back to reproducible source,
  normal reproducible version selection applies.
- `--prefer-local` with `--latest`, `--all-versions`, or a constraint selector
  is only valid when local source is unavailable or when `--allow-unused`
  permits the local preference to be ignored. Otherwise planning errors because
  the selected local source cannot enumerate versions.

### Version Enumeration

Enumeration is a source capability.

Git enumeration:

- Lists remote tags.
- Matches tags against the source ref template.
- Optionally applies `tag_regex`.
- Extracts semantic versions.

URL enumeration:

- Lists a directory derived from the URL template.
- Extracts candidate entries from the listing.
- Matches candidates against the URL filename/path segment template.
- Optionally applies `file_regex`.
- Extracts semantic versions.

Enumeration should be separate from materialization. Planning should be able to
enumerate versions without cloning or downloading source archives.

### Local Source Policy

Current Pekit's `--local` means "prefer localpath, fall back to remote if local
is unavailable." That is convenient but surprising.

V2 should make the policy explicit.

Proposed flags:

```text
--local
--local=<path>
```

Require the local source override. If the recipe has no local source or the path
is missing, error. When `<path>` is supplied, use that CLI path instead of
`[source.local].path`.

```text
--prefer-local
--prefer-local=<path>
```

Use the local source override when available. If unavailable, use the
reproducible remote source. When `<path>` is supplied, try that CLI path first.

This preserves the useful current fallback behavior while giving it an honest
name.

Decision:

- `--local` means strict local development.
- `--prefer-local` means use local source when available, otherwise use the
  reproducible source.
- `--local=<path>` is strict local development with an invocation-local source
  path.
- `--prefer-local=<path>` prefers an invocation-local source path but may fall
  back to the reproducible source.
- CLI local paths resolve relative to the current working directory, not the
  recipe root.
- CLI local path overrides do not modify recipe config and are visible in the
  plan/provenance as invocation-local inputs.
- Current Pekit's fallback behavior moves from `--local` to `--prefer-local`.
- `--local=<path>` and `--prefer-local=<path>` are invalid for sourceless
  recipes unless `--allow-unused` suppresses the recognized but unused flag.

### Local Versioning

When building from a local source without an explicit version, Pekit needs a
version for templates and package metadata.

Current Pekit uses:

```text
0.0.0-localdev
```

V2 should preserve this default unless a better local-version model is designed.

Rules:

- If a local build provides `--version`, it must be an exact version.
- If no version is provided, use `0.0.0-localdev`.
- Local builds should be visibly marked in provenance.
- Local builds should not be recorded as reproducible build-farm outputs.

### Source Caching

The source cache key and materialization result should be explicit in the plan.

Source materialization produces:

- source kind
- rendered source identity
- resolved immutable identity where available
- source scope
- source root
- provenance
- cache policy
- materialization manifest path

The source root is the tree targets run in and package `@source:` refs read
from. It should be a prepared working tree, not an opaque cache implementation
detail.

Current Pekit trusts existing cache directories blindly. V2 defines internal
cache policies:

- `trust`: if the cache path exists, reuse it.
- `verify`: validate that the cache path corresponds to the expected source.
- `refresh`: remove and re-materialize every time.

Initial user-facing cache flag:

```text
--refresh-source
```

`--refresh-source` applies the internal `refresh` policy to source entries
needed by the invocation. Do not add a full
`--source-cache=trust|verify|refresh` flag until there is a clear use case.

#### Source Cache Layout

Use separate locations for raw reusable source cache and per-invocation working
trees.

Within `out_dir`:

```text
_source_cache/
  git/<repo_key>/repo.git
  url/<url_key>/artifact

<source_scope>/
  source/
  source.pekit.json
  build/<target>/
  package/<package>/
  tmp/
```

`_source_cache` is Pekit-managed and may be removed by `pekit clean` because it
lives under `out_dir`.

`source.pekit.json` is the materialization manifest for the prepared source
root. It records the source kind, rendered identity, immutable identity,
checksum when applicable, extraction settings, and selected source root within
the materialized tree.

Source scopes:

- Git source: repository identity plus resolved commit.
- URL source with checksum: checksum plus rendered URL.
- URL source without checksum: rendered URL.
- Local source: absolute local path.

Scopes should be filesystem-safe and include a short hash. Human-readable hints
such as selected version or URL basename may be included, but correctness must
come from the hash.

#### Git Materialization

Git source planning renders the configured ref after version selection, then
resolves it to a concrete commit.

Rules:

- A branch, tag, or symbolic ref becomes reproducible only after resolving to a
  commit.
- A pinned commit is already immutable, but Pekit should still verify the
  object exists in the cache.
- The raw git cache is a bare repository under `_source_cache/git/<repo_key>`.
- The prepared source root is a worktree or checkout under
  `<source_scope>/source`.

Default git policy is `verify`.

Verification:

- The raw cache must contain the resolved commit.
- If the raw cache lacks the commit, fetch it from the configured remote.
- The prepared source root must have `HEAD` at the resolved commit.
- If the prepared source root is missing or points at the wrong commit,
  recreate it from the raw cache.
- Before running targets, reset and clean the prepared source root so previous
  build mutations do not leak into the next run.
- Verify the configured remote identity recorded in the manifest matches the
  invocation's rendered source identity.

`--refresh-source` for git:

- Updates or recreates the raw git cache from the configured remote.
- Recreates the prepared source root for the resolved commit.

Submodules are not enabled by default.

Future explicit field:

```toml
[source.git]
submodules = true
```

If added, submodules should be initialized recursively after checkout and
included in provenance by commit identity. Do not make submodule behavior
implicit.

#### URL Materialization

URL source planning renders the configured URL after version selection.

The raw URL cache stores the downloaded artifact:

```text
_source_cache/url/<url_key>/artifact
```

Download rules:

- Use HTTP GET for HTTP(S) URLs.
- Follow normal redirect behavior.
- Send a Pekit user agent.
- Treat non-2xx responses as errors.
- Write downloads to a temporary file and atomically rename into cache.
- A failed download must not leave a cache entry that can be reused.

Checksum rules:

- If a checksum is configured, verify the downloaded artifact before any
  extraction.
- If a cached artifact exists and checksum verification fails, delete it and
  download once more. If the new artifact still fails, error.
- If no checksum is configured, the default policy is `trust` and an existing
  cached artifact may be reused without network access.
- Verifying the raw artifact does not prove an existing extracted source root
  is clean. Under `verify`, Pekit should recreate the prepared source root from
  the verified artifact unless the implementation has a reliable content
  verification mechanism for the prepared tree.
- Under `trust`, Pekit may reuse an existing prepared source root when its
  materialization manifest matches the rendered URL and extraction settings.

Extraction:

```toml
[source.url]
url = "https://example.com/app-{{version}}.tar.xz"
extract = true
root = "app-{{version}}"
```

Rules:

- `extract = false` stores the downloaded artifact inside `<source_scope>/source`
  using the URL basename. The source root is that directory.
- `extract = true` extracts the artifact into a temporary directory and renames
  it into `<source_scope>/source`.
- `root` is optional and defaults to `"."`.
- `root` is a path inside the extraction result that becomes `PEKIT_SOURCE_ROOT`
  and the `@source:` root.
- `root` is a templated field rendered after version selection.
- `root` must be relative and must not escape the extraction directory.
- Canonical v2 should not auto-select a single top-level extracted directory.
  Recipes should set `root` explicitly when archives contain a top-level
  directory.

Archive safety:

- Reject archive entries with absolute paths.
- Reject archive entries that escape the extraction directory.
- Do not follow symlinks while extracting later entries.
- Preserve source symlinks as symlinks when safe to do so.
- Reject special file types unless the extractor explicitly supports them.

Supported archive formats should be implementation capabilities. Initial v2
should support at least the formats current Pekit handles where practical:

- `.tar`
- `.tar.gz`
- `.tgz`
- `.tar.xz`
- `.txz`
- `.tar.bz2`
- `.tbz2`
- `.tar.zst`
- `.zip`

Prefer structured archive readers over shelling out. If an implementation has
to shell out for a compressor, extraction must still happen in a temporary
directory and be validated before becoming the source root.

Default URL policies:

- URL without checksum: `trust`.
- URL with checksum: `verify`.

`--refresh-source` for URL:

- Deletes the selected raw artifact cache entry.
- Deletes the prepared source root.
- Downloads and prepares the source again.

Decision: URL checksums belong under `[source.url]`.

Unversioned URL:

```toml
[source.url]
url = "https://example.com/foo.tar.xz"
extract = true
root = "foo"
checksum = "sha256:abc..."
```

Versioned URL:

```toml
[source.url]
url = "https://example.com/foo-{{version}}.tar.xz"
extract = true
root = "foo-{{version}}"

[source.url.checksum]
"1.0.0" = "sha256:abc..."
"1.1.0" = "sha256:def..."
```

Rules:

- Checksum specs initially support `sha256:<hex>`.
- `[source.url].checksum` is for unversioned or single-artifact URL sources.
- `[source.url.checksum]` maps concrete resolved version strings to checksum
  specs.
- If a versioned URL has a checksum table, the resolved version must have a
  checksum entry. Missing entries are errors.
- If no checksum is provided, URL caches use the default trust policy.
- If a checksum is provided, Pekit verifies the downloaded archive before
  extraction and can verify cached archive content when available.

Initial cache policy:

- Git sources default to `verify`.
- URL sources default to `trust` when no checksum is present.
- URL sources default to `verify` when a checksum is present.
- Local sources do not use a Pekit source cache.

#### Local Materialization

Local sources do not use the Pekit source cache and are not copied by default.

Rules:

- `[source.local].path` resolves relative to the recipe root.
- `--local=<path>` and `--prefer-local=<path>` resolve relative to the current
  working directory.
- The resolved local path must be a directory.
- `--local` errors if the local path is absent or not a directory.
- `--prefer-local` falls back to the reproducible source only when the local
  path is absent or not a directory.
- If a local path exists but cannot be read, planning errors instead of falling
  back silently.
- `PEKIT_SOURCE_ROOT` points at the resolved local directory.
- Pekit does not reset, clean, or otherwise mutate the local source tree outside
  target commands chosen by the user.

Local source scope is derived from the absolute local path and should include a
short hash. The local build version sentinel remains `0.0.0-localdev`; the
scope is separate from the version.

### Source Delegation

Current Pekit supports a powerful but implicit source delegation model: a recipe
with `[source]` can borrow build config, env, package base files, and package
members from the fetched source tree.

V2 should keep the concept but make it explicit.

Decision: delegation is boolean, not required/optional policy.

Quick enable all:

```toml
delegate = true
```

Detailed form:

```toml
[delegate]
all = true
env = false
```

Detailed explicit form:

```toml
[delegate]
build = true
packages = true
env = true
wrap = true
```

Semantics:

- `delegate = true` enables every delegation surface.
- `[delegate] all = true` enables every delegation surface, then explicit false
  values opt out.
- `[delegate]` without `all = true` enables only surfaces set to true.
- No `required` or `optional` values are needed.

If delegation is enabled for a surface:

- If the invoking recipe defines that surface, delegated source content is a
  fallback or merge layer.
- If the invoking recipe does not define that surface and the command needs it,
  the delegated source must provide it or planning errors naturally.
- If the command does not need that surface, absence is fine.

Delegation surfaces:

| Surface | Meaning |
| --- | --- |
| `build` | Source `pekit.toml` may provide build targets. |
| `packages` | Source package files may provide package bases and members. |
| `env` | Source `pekit.toml` and selected source env files may provide `[env]` values. |
| `wrap` | Selected source env files may be used as fallback wrappers. |

Desired properties:

- It should be obvious when a recipe is borrowing behavior from source.
- Each borrowed surface can be independently controlled.
- Merge precedence should be documented and testable.
- Delegated source package files should remain self-contained and should not
  accidentally reference files from the higher-level recipe.

### Package Source References

Package file source references should distinguish build outputs from non-build
file roots.

Build output refs keep the familiar syntax:

```toml
[files]
"main:bin/app" = "usr/bin/app"
"tools:helper" = "usr/bin/helper"
":bin/app" = "usr/bin/app"
```

Rules:

- `target:path` means path inside build target `target` staged output.
- `:path` is shorthand for `main:path`.
- Build target names are resolved after recipe/delegation planning. A build
  target may come from the invoking recipe or delegated source; the file ref
  does not care where the target was declared.

Non-build file roots use `@root:path`:

```toml
[files]
"@source:LICENSE" = "usr/share/licenses/app/LICENSE"
"@recipe:files/app.service" = "etc/systemd/system/app.service"
"@workspace:common/licenses/MIT" = "usr/share/licenses/app/LICENSE"
```

Roots:

| Root | Meaning |
| --- | --- |
| `@source:` | The materialized source tree or local source working copy. |
| `@recipe:` | The invoking recipe directory. |
| `@workspace:` | The workspace root. Errors outside a workspace. |

Plain non-build paths are allowed, but their meaning is owner-relative:

- A package file loaded from the invoking recipe treats plain `path` as
  `@recipe:path`.
- A package file loaded from the delegated source treats plain `path` as
  `@source:path`.
- A package file loaded from the workspace root treats plain `path` as
  `@workspace:path`.

This rule is critical:

> The meaning of an unqualified source path is determined by the file that
> declared it, not by the final merged package.

Package files should normalize plain paths to canonical rooted refs when they
are decoded. That way merge order cannot change their meaning.

Example:

```toml
# In a delegated source package file:
[files]
"LICENSE" = "usr/share/licenses/app/LICENSE"
```

normalizes to:

```text
@source:LICENSE
```

not:

```text
@recipe:LICENSE
```

If a delegated source package intentionally needs a file from the higher-level
recipe, it can say so explicitly:

```toml
[files]
"@recipe:files/app.service" = "etc/systemd/system/app.service"
```

This should be rare, but it is logically sound and unambiguous.

All rooted paths must be cleaned and must not escape their root with `..`.
Globs should work under every root. Excludes should use the same source-ref
syntax as file mappings.

### File Mapping and Payload Entries

Package payload planning should be strict and deterministic.

`[files]` keeps the current mapping direction: source ref on the left, package
destination on the right.

String form:

```toml
[files]
":bin/app" = "usr/bin/app"
"@source:LICENSE" = "usr/share/licenses/app/LICENSE"
"@recipe:files/*.service" = "etc/systemd/system/"
```

Object form:

```toml
[files]
"@source:bin/app" = { path = "usr/bin/app" }
"@source:lib/libspecial.so" = { path = "lib/libspecial.so", override = true }
```

The string form is shorthand for `{ path = "..." }`.

`override = true` means this payload entry may bypass the package format's
normal destination layout policy. It is for rare cases where the package format
would normally reject a destination path, such as placing a `peipkg` file in
bare `lib/` instead of an allowed triplet directory.

`override = true` does not bypass basic safety:

- destination paths must be relative
- destination paths must not be empty
- destination paths must not contain `..`
- destination paths must not contain NUL
- destination collisions are still errors
- unsupported file types are still errors
- package formats may still reject entries they cannot represent

Overrides should be visible in verbose output and JSON plans. Package formats
that can record layout overrides should include them in package metadata or
provenance. Formats that do not have layout policy may accept the flag as
harmless, but should not pretend a policy was bypassed.

#### Destination Paths

Package destinations are normalized before validation.

Rules:

- Leading `/` is invalid.
- Empty paths are invalid.
- `..` components are invalid.
- `.` components and duplicate slashes are normalized away.
- A destination ending in `/` is a destination directory.
- Parent directories are synthesized by the package format.
- A file entry cannot collide with another file, symlink, or directory entry.
- A directory entry cannot collide with a file or symlink entry.

The plan should carry destination kind explicitly instead of relying on trailing
slash strings after decode.

#### File Sources

Source refs may resolve to regular files, directories, or symlinks.

Rules:

- Missing literal source files are errors.
- Missing build-output files are errors after the producing build target runs.
- In dry-run, missing build-output paths may be unresolved if the build target
  has not run and checking them would require a side effect.
- Source symlinks are packaged as symlinks by default.
- Symlinked directories are packaged as symlinks, not traversed.
- Directory recursion does not follow symlinked directories.

Directory mapping:

- Mapping a source directory copies its contents recursively into the
  destination directory.
- The source directory name itself is not added unless the destination path
  explicitly includes it.
- Directory metadata is normalized according to the package format.

Single-file mapping:

- A single source file mapped to `usr/bin/app` writes that exact destination.
- A single source file mapped to `usr/bin/` writes `usr/bin/<basename>`.

#### Globs

Supported glob syntax:

- `*`
- `?`
- `[...]`
- `**`

Rules:

- Globs only match inside their source root.
- Glob expansion is sorted lexicographically by normalized source path.
- A glob that matches nothing is an error by default.
- If a glob expands to multiple entries, the destination must be a directory.
- For a glob mapped to a destination directory, each matched path is placed
  under the destination using its path relative to the non-glob source prefix.
- Source symlinks matched by globs remain symlink payload entries.

Future extension:

```toml
allow_empty = true
```

or a per-entry equivalent may be added if empty globs become useful. Do not add
it initially.

#### Excludes

Package definitions may exclude source refs after expansion:

```toml
excludes = [
  "@source:share/**/*.tmp",
  "@source:share/**/.keep",
]
```

Rules:

- Excludes use the same source-ref syntax as `[files]` keys.
- Plain exclude paths are owner-relative, just like plain file source paths.
- Excludes match source paths, not destination paths.
- Excludes apply after glob/directory expansion and before destination
  collision checks.
- An exclude that matches nothing is allowed.

#### Synthetic Symlinks

Packages may declare symlink payload entries directly:

```toml
[symlinks]
"usr/bin/cc" = "gcc"
"usr/lib/triplet/libfoo.so" = { target = "libfoo.so.1" }
"lib/libspecial.so" = { target = "libspecial.so.1", override = true }
```

The table key is the package destination path. The value is either symlink
target text or an object containing `target`.

Rules:

- Symlink target text is stored as link text. Pekit does not resolve it through
  source roots.
- Symlink target text must be non-empty and must not contain NUL.
- `override = true` has the same meaning as file entries: bypass the package
  format's destination layout policy for the symlink destination.
- `override = true` does not bypass path safety, destination collision checks,
  or package-format support for symlinks.
- Package formats may validate symlink targets if they have format-specific
  rules, but the generic Pekit planner does not treat the target as a source
  path.

### Package Discovery and Merge

Package planning should operate on decoded package definitions, not raw TOML
tables. Decode-time context matters because plain file refs bind to the file's
owner.

Package definition owners:

| Owner | Meaning |
| --- | --- |
| `workspace` | Package defaults loaded from the workspace root. |
| `recipe` | Package files loaded from the invoking recipe directory. |
| `source` | Package files loaded from the delegated source tree. |

Each package file should be decoded with an owner. During decode:

- Plain non-build file refs are normalized to `@workspace:`, `@recipe:`, or
  `@source:` according to owner.
- Explicit root refs are validated and kept explicit.
- Build refs are kept as build refs and resolved later against the planned
  build target namespace.

This prevents merge order from changing the meaning of file sources.

#### Package File Locations

Candidate v2 package locations:

```text
package.pekit.toml
packages.pekit/package.pekit.toml
packages.pekit/<name>.package.pekit.toml
```

Proposal:

- `package.pekit.toml` in a root is the base package file.
- `<name>.package.pekit.toml` files are package members.
- `packages.pekit/` is the canonical subdirectory for many-package recipes.
- Current Pekit's `package.pekit/` directory spelling is not supported in
  canonical v2.

Decision: support member files in both the root and `packages.pekit/`.

Root-level members are convenient for small multi-package recipes:

```text
libc.package.pekit.toml
libc-dev.package.pekit.toml
```

`packages.pekit/` is the preferred location for larger package sets.

Duplicate member names across the root and `packages.pekit/` are errors.

#### Bases and Members

A package set has:

- zero or one base package file
- zero or more named member package files

The base file has two roles:

- If no members exist, it is the standalone package definition.
- If members exist, it is shared defaults for the members and is not emitted as
  its own package.

This preserves current Pekit's useful base/member model.

If users need a package whose name is literally the base package, it should be a
member file with an explicit name, not an implicit second role for the base.

Example:

```text
package.pekit.toml                  # shared defaults only
packages.pekit/libc.package.pekit.toml
packages.pekit/libc-dev.package.pekit.toml
```

#### Discovery By Owner

Package discovery should happen separately per owner:

1. Load workspace package defaults if inside a workspace and if workspace
   defaults are enabled.
2. Load recipe package files.
3. If package delegation is enabled, materialize source and load source package
   files.

Duplicate member names are errors within the same owner.

The same member name across source and recipe is not an error; it means the
recipe member overlays the source member.

Workspace package files should be defaults only. Proposal: workspace defaults
should not create a package by themselves. At least one recipe or delegated
source package definition should exist for `package` or `publish` to have work.

Decision: workspace package defaults are implicit when a `workspace.pekit.toml`
exists.

The workspace marker gates inheritance, so a random ancestor
`package.pekit.toml` is never inherited. A sibling `package.pekit.toml` beside
`workspace.pekit.toml` is the workspace default package layer.

#### Merge Order

For a standalone package, merge order is:

```text
workspace base < source base < recipe base
```

Source base is present only when package delegation is enabled.

For a member package, merge order is:

```text
workspace base < source base < recipe base < source member < recipe member
```

The effective package is the result of overlaying later layers on earlier
layers.

If there are source members and recipe members, the selected package member set
is the union of member names:

- source-only member: source member over shared base
- recipe-only member: recipe member over shared base
- both: recipe member over source member over shared base

#### Merge Policy

V2 should name the merge policy instead of encoding it as scattered behavior.

Proposed policy: `package-overlay`.

Rules:

- `[package]` identity/manifest table merges field-by-field.
- `[files]` replaces as a whole section.
- `builds` replaces as a whole value.
- dependency sections replace as whole sections.
- `provides`, `replaces`, `sd_overrides`, and publish targets replace as whole
  sections.
- package format replaces as a scalar.
- multipack config replaces as a whole section.

Rationale:

- Field-level merge is useful for package identity because recipes commonly
  override one field, such as description or license.
- Whole-section merge avoids surprising partial file/dependency inheritance.
- A package member should be able to replace `[files]` without accidentally
  shipping files from a lower layer.

Decision: dependency sections replace wholesale in initial v2.

Additive dependency merge is not included yet. It may be added later with an
explicit syntax, such as add/remove subsections, if package authoring needs it.

#### Package Identity

Package member selection should use a stable selector name, normally the member
filename prefix.

Example:

```text
packages.pekit/libc-dev.package.pekit.toml
```

has selector:

```text
libc-dev
```

The emitted package name is resolved separately:

- Standalone package: `[package].name` if set, otherwise recipe/source default.
- Member package: member selector by default.
- Member package with its own `[package].name`: use that explicit package name.

Inherited `[package].name` from a shared base should not become the emitted name
for every member.

This preserves the current safety rule where a shared base name does not cause
member name collisions.

Decision: CLI package selection is by member selector.

Emitted package name selection is not part of the initial v2 design. If needed
later, add an explicit mode such as `--name`, rather than making positional
selection ambiguous.

Selectors are stable recipe authoring handles; emitted names are package
metadata and may be overridden.

#### Package Selection

Package and publish selection use package selectors, not emitted package names.

Selector names:

- A standalone `package.pekit.toml` has selector `main`.
- A member file uses the filename prefix as its selector.
- `libc.package.pekit.toml` has selector `libc`.
- `packages.pekit/libc-dev.package.pekit.toml` has selector `libc-dev`.
- Canonical package definition selectors use the same simple identifier rules
  as target names and must not contain `:`.

Default behavior:

```text
pekit package
pekit publish
```

If exactly one effective package definition selector exists, package or publish
that selector.

If multiple effective package definition selectors exist, planning errors and
lists available selectors. Pekit should not package everything by default.

Explicit selection:

```text
pekit package libc libc-dev
pekit publish libc
```

packages or publishes exactly those selectors.

All packages:

```text
pekit package --all
pekit publish --all
```

`--all` is the package-selection flag. It selects every effective package
definition selector. It is accepted by `package` and `publish`, and by
`workspace` when delegating to `package` or `publish`.

Version fan-out uses the explicit version flag:

```text
pekit package libc --all-versions
pekit package --all --all-versions
pekit publish --all --latest
```

Rules:

- `--all` means all packages.
- `--all-versions` means all versions.
- `--latest` means the latest version.
- `--all` and positional package selectors are mutually exclusive.
- `--all` does not imply `--all-versions`.
- `--all-versions` does not imply `--all`.

Workspace package selection:

```text
pekit workspace package libc
```

selects package selector `libc` in every workspace member. If a member does not
provide that selector, that member errors unless `--allow-unused` suppresses the
recognized but unused selector.

```text
pekit workspace package --all
```

packages every effective package selector in every workspace member.

Multipack package definitions can be selected by definition selector or by
expanded instance selector. The detailed expansion and selector rules are
defined in the multipack section below.

#### Missing Packages

Planning should error when a command needs packages and no effective package
definition exists.

Cases:

- No recipe package files and package delegation is off: error.
- Package delegation is on but source provides no package files and recipe
  provides none: error.
- Workspace defaults exist but no recipe/source package exists: error under the
  proposed "workspace defaults do not create packages" rule.
- A requested package selector does not exist: error and list available
  selectors.

#### Multipack Interaction

Multipack expansion should happen after package layers merge and before package
instances are planned. V2 should keep multipack as package fan-out, but it
should not deep-copy raw TOML and substitute through every string. Multipack is
a typed package expansion step over decoded package config.

Pipeline:

```text
discover layers
  -> decode with owner-bound refs
  -> merge package layers
  -> expand multipack
  -> validate package instances
  -> plan builds/files/packaging
```

Shape:

```toml
[multipack]
enum = ["mono", "serif", "sans"]
```

File-derived values:

```toml
[multipack.enum.files]
path = ":usr/share/fonts/*"
regex = '^([A-Za-z0-9_.-]+)$'
```

Literal enum rules:

- Values must be non-empty strings.
- Values must be selector-safe: `[A-Za-z0-9_.-]+`.
- Values must not contain `:`.
- Duplicate values are errors.
- Integer enum values are not accepted in canonical v2; write strings
  explicitly.

File enum rules:

- `path` is a package source ref.
- `regex` must compile.
- The regex must have either exactly one capture group or one named `value`
  capture group.
- The regex is applied to each matched path's basename.
- Non-matching files are skipped.
- Empty captures are skipped.
- Extracted values are deduplicated and sorted.
- Extracted values must satisfy the same selector-safe rules as literal enum
  values.
- No glob matches is an error.
- No extracted values is an error.
- Verbose output should show match count, skipped count, and extracted values.

Multipack template variable:

```text
{{multipack}}
```

`{{multipack}}` is available only during the `package_instance` render phase.
If it appears in a templated package field without `[multipack]`, planning
errors.

Example:

```toml
[multipack]
enum = ["mono", "serif"]

[package]
name = "font-{{multipack}}"
description = "{{multipack}} font package"

[files]
":usr/share/fonts/{{multipack}}/*" = "usr/share/fonts/{{multipack}}/"
```

Do not keep current Pekit's special `[multipack].suffix` field in canonical v2.
It is unnecessary when package fields can explicitly template on
`{{multipack}}`.

Default emitted names:

- If `[package].name` is absent, a multipack instance defaults to
  `<base-selector>-<multipack>`.
- If `[package].name` is present, it must render to distinct emitted package
  names across instances.
- Fixed repeated names are errors.

Instance selectors:

- The package definition selector selects all instances generated by that
  definition.
- Individual instances use `<selector>:<multipack>`.
- Only multipack instance selectors use `:`.

Example:

```text
fonts
fonts:mono
fonts:serif
```

`pekit package fonts` selects every instance produced by the `fonts` package
definition. `pekit package fonts:mono` selects only the `mono` instance.

Planning:

- Literal enum expansion can happen during normal package planning.
- File enum expansion becomes an `expand_multipack` plan operation.
- If the enum path is under `@recipe`, `@workspace`, or an already-materialized
  `@source`, expansion can happen without running build targets.
- If the enum path is under a build output, the producing build target is
  planned before `expand_multipack`.
- Dry-run shows file enum expansion as unresolved when the needed path requires
  a side effect.
- Build targets shared across multipack instances deduplicate in the plan.

### Package Formats

Package formats should be pluggable behind a small capability interface.

Initial formats:

- `tar`
- `peipkg`

Each format declares:

- artifact filename convention
- required package fields
- supported manifest fields
- payload validation rules
- supported payload entry kinds
- provenance requirements

#### `tar`

`tar` is a simple archive format.

Rules:

- Artifact path: `<package stage>/<name>.tar`.
- Payload files are written under their package-relative destination paths.
- Payload symlinks are written as symlink entries.
- Parent directories are synthesized.
- Output should be deterministic for identical inputs.
- Timestamps should be normalized.
- Owner/group metadata should be normalized.
- Source files must resolve to regular files or symlinks. Other file types are
  rejected unless explicitly added later.

`tar` cannot express package manifest metadata. If manifest-only fields are set,
planning should error before execution.

Manifest-only fields include:

- version
- architecture
- description
- license
- homepage
- dependencies
- optional dependencies
- conflicts
- provides
- replaces
- side effects
- security descriptor overrides

#### `peipkg`

`peipkg` is the Peios package format.

Rules:

- Artifact path: `<package stage>/<name>_<version>_<architecture>.peipkg`.
- Requires `[package].version`.
- Requires `[package].architecture`.
- Validates payload layout according to peipkg rules.
- Supports regular file and symlink payload entries.
- Supports per-entry `override = true` to bypass peipkg destination layout
  policy for rare paths that intentionally violate the normal layout rules.
- `override = true` does not bypass basic path safety, destination collision
  checks, or payload entry type validation.
- Includes package manifest metadata.
- Includes build provenance according to the provenance model.

Implementation note: the `peipkg` packing API needs to accept per-entry layout
override metadata, not just a flat list of paths, so `peipkg/pack/` will likely
need a small interface change when Pekit v2 is implemented.

Canonical v2 package metadata uses snake_case:

```toml
[package]
name = "app"
version = "1.0.0-1"
architecture = "x86_64"
description = "Example app"
license = "MIT"
homepage = "https://example.com"
side_effects = ["reload-services"]

[dependencies]
libc = "*"

[optional_dependencies]
docs = "*"

[conflicts]
old-app = "< 1.0"

[provides]
app-virtual = "1.0"

[replaces]
older-app = "*"

[sd_overrides]
"usr/bin/app" = "O:SY"
```

Open decision: exact side effect vocabulary belongs to the peipkg/package format
spec, not Pekit's generic design.

### Artifact Lifecycle

Package artifacts are outputs of planned `pack_package` operations. Publishing
operates over those artifacts; it should not reimplement package planning.

Artifact record:

- command/member context
- source scope
- selected version
- package definition selector
- package instance selector, for multipack
- emitted package name
- package format
- artifact path
- artifact basename
- provenance summary

The plan should know the expected artifact path before execution when package
metadata is available. Dry-run should show unresolved artifact paths only when
metadata depends on an unresolved plan node, such as file-derived multipack
expansion.

Package staging:

- Each package instance gets a managed package stage under
  `<out base>/package/<package_instance_scope>`.
- `package_instance_scope` is filesystem-safe and derived from the package
  selector, multipack instance selector, emitted package name, version, and
  format where applicable.
- Package format writers write artifacts inside the package stage.
- Format writers should write to a temporary file in the same stage and
  atomically rename to the final artifact path.
- Existing final artifacts in the managed package stage may be overwritten by a
  new package operation.
- If `clear_out = true`, the package stage is removed before packaging.
- If `clear_out = false`, the package stage is preserved, but the format writer
  still owns and may replace its final artifact path.

Package command:

- `pekit package` builds required targets, resolves payload entries, prepares
  package stages, and writes selected artifacts.
- It does not publish artifacts.
- Artifact paths are reported in normal output and as artifact events in JSON.

Publish command:

- `pekit publish` plans the same selected package artifacts as `pekit package`,
  then plans publish operations for those artifacts.
- It packages selected artifacts first; it does not publish stale existing
  artifacts by default.
- A package selected for publish must have at least one publish target.
- Shared package work is deduplicated. If two publish targets consume the same
  artifact, the artifact is produced once.
- Initial v2 does not include a "publish an already-existing artifact" mode.
  Add an explicit mode later if repository tooling needs it.

Publish execution is not transactional:

- A successfully copied/uploaded artifact is not rolled back if a later publish
  operation fails.
- Failures must report which artifacts were already published and which were not
  attempted.
- Workspace publish follows workspace failure policy: without `--fail-fast`,
  failed members are reported and remaining members may continue.

### Publish Targets

Publishing should be another planned operation over built artifacts.

Initial publish target:

```toml
[[publish.localdir]]
path = "pkgsOut"
overwrite = true
```

Rules:

- `path` is workspace-root-relative when inside a workspace.
- Otherwise `path` is recipe-root-relative.
- The path is cleaned and must not escape its base.
- `path = "."` means the publish base itself.
- Empty `path` is invalid.
- The destination directory is created if needed.
- The artifact is copied there by basename:

```text
<base>/<path>/<artifact basename>
```

- The copy should write a temporary file in the destination directory, then
  atomically rename it into place.
- `overwrite` defaults to true for compatibility with current Pekit localdir
  publishing.
- If `overwrite = false` and the destination exists, publishing errors before
  replacing the file.
- If multiple planned publish operations target the same destination path in one
  invocation, identical source artifact paths are deduplicated and distinct
  source artifact paths are an error.

`path` is a package-instance templated field. It may reference version and
multipack variables available for the package being published. Rendered paths
are normalized and re-validated before execution.

Publish target types should be pluggable.

Future publish targets might include:

- object storage
- HTTP upload
- repository index update
- signing/promotion workflows

Do not design those until the farm/repository requirements are clearer.

### Provenance

Package provenance should be a source capability plus a build context decision.

Internal provenance should be richer than any one package format's manifest.

It should capture:

- source identity
- recipe identity
- whether the build is reproducible
- timestamp source
- any degraded/unanchored status

Provenance classes:

- `git`: stable commit ref and commit timestamp.
- `url`: rendered URL plus optional checksum.
- `local`: localdev marker and wall-clock timestamp.
- `unanchored`: explicit fallback marker when provenance cannot be established.

V2 should avoid silently degrading reproducible provenance. If provenance is
required by a package format and cannot be established, the package format or
invocation should decide whether that is an error.

Rules:

- Git sources should resolve to a concrete commit during planning or
  materialization.
- A git source pinned to a concrete commit is reproducible.
- A git tag or branch becomes reproducible only after resolving to a concrete
  commit.
- URL sources with checksums are reproducible by rendered URL plus checksum.
- URL sources without checksums are not fully reproducible.
- Local sources are non-reproducible local development provenance.
- Unanchored provenance is non-reproducible and should be visible.

URL timestamp rule:

- For URL sources with checksum, use recipe git commit timestamp when the recipe
  is git-anchored.
- If no stable recipe timestamp exists, use `0` rather than wall clock for
  reproducible URL source identity.
- Avoid wall-clock timestamps in reproducible packages.

Local/unanchored policy:

- `--local` creates local provenance and is allowed.
- `package` with unanchored provenance may warn and continue.
- `publish` with unanchored provenance errors by default.
- `publish` may allow unanchored provenance only with:

```text
--allow-unanchored
```

`--allow-unanchored` should be explicit and visible in dry-run output.

Package formats map internal provenance into their own manifest shape. If a
format cannot express the full internal provenance, it should degrade
deliberately and visibly, not by accident.

## Planning Model

A plan is an ordered set of operations with inputs, outputs, and dependencies.

Possible operation types:

- Resolve recipe.
- Resolve source.
- Enumerate versions.
- Select version.
- Materialize source.
- Load inherited recipe/package files.
- Prepare stage.
- Run build target.
- Resolve files.
- Pack package.
- Publish artifact.
- Clean managed output.

Planning should deduplicate shared work. For example, if multiple packages use
one build target, that target should appear once in the plan.

Planning should also decide reuse. `--no-build` should not be an execution-time
surprise; the plan should say whether a target will be reused or rebuilt.

## Execution and Staging

Execution consumes the plan. It should not rediscover policy that planning
already decided.

### Output Layout

For sourceless recipes:

```text
out_dir/
  build/<target>/
  package/<package>/
  tmp/
```

For recipes with a reproducible source:

```text
out_dir/
  _source_cache/
  <source_scope>/
    source/
    source.pekit.json
    build/<target>/
    package/<package>/
    tmp/
```

For strict local builds:

```text
out_dir/
  <local_source_scope>/
    build/<target>/
    package/<package>/
    tmp/
```

`source_scope` should be the source cache key or a stable filesystem-safe form
derived from it. `local_source_scope` should be derived from the absolute local
source path so switching local checkouts cannot accidentally reuse another
checkout's build stages.

### Working Directory Defaults

Command targets should have predictable working directories.

Defaults:

| Target kind | Default working directory |
| --- | --- |
| `build` | source root if a source exists, otherwise recipe root |
| `test` | source root if a source exists, otherwise recipe root |
| `install` | source root if a source exists, otherwise recipe root |
| `clean` | recipe root |

This intentionally changes current Pekit behavior for `test` and `install` with
external sources. In v2, if a recipe builds from an external source, test and
install commands normally run there too.

Possible future target field:

```toml
[test.unit]
workdir = "recipe" # recipe | source
command = "..."
```

Do not add `workdir` until a real recipe needs it.

### Target Environment

Every shell-running target gets a self-contained inner script containing Pekit
exports, keyring exports, `[env]` exports, and then the target command.

Pekit keeps timestamp exports because they are useful for reproducible builds
and package metadata:

```sh
PEKIT_BUILD_TIMESTAMP=<unix seconds when invocation began>
PEKIT_SOURCE_TIMESTAMP=<unix seconds of source commit or 0>
```

`PEKIT_BUILD_TIMESTAMP` is captured once per top-level invocation and shared
across workspace fan-out and multi-version operations.

`PEKIT_SOURCE_TIMESTAMP` is:

- git commit timestamp for git source roots when available
- git commit timestamp for the recipe root for sourceless git recipes
- `0` when no git timestamp is available

When a concrete version is selected, targets also get:

```sh
PEKIT_VERSION=<original selected version text>
PEKIT_VERSION_MAJOR=<major>
PEKIT_VERSION_MINOR=<minor, if present>
PEKIT_VERSION_PATCH=<patch, if present>
PEKIT_VERSION_PRERELEASE=<prerelease, if present>
PEKIT_VERSION_BUILDMETA=<build metadata, if present>
```

Missing optional version components export as empty strings. This keeps shell
commands simple while templated config fields remain strict about referencing
missing components.

Every target with a managed output root gets:

```sh
PEKIT_ROOT=<absolute out base>
PEKIT_RECIPE_ROOT=<absolute recipe root>
PEKIT_SOURCE_ROOT=<absolute source root>
PEKIT_WORKSPACE_ROOT=<absolute workspace root, or empty>
```

For sourceless recipes, `PEKIT_SOURCE_ROOT` is the recipe root.
`PEKIT_WORKSPACE_ROOT` is an empty string outside a workspace.

Build targets get:

```sh
PEKIT_OUT=<absolute out base/build/<target>>
```

Targets with direct build dependencies get:

```sh
PEKIT_<TARGET>_OUT=<absolute out base/build/<target>>
```

Only direct dependencies are exported. Transitive dependency outputs are not
exported unless they are direct needs.

### Environment Assembly

Environment assembly is a plan-time object. Execution should consume the
planned environment script instead of rediscovering env policy.

Inner target script order:

1. Pekit-managed exports.
2. Keyring exports.
3. Merged user `[env]` exports.
4. Target command.

This order lets user env values reference Pekit-managed paths and keyring
values through normal shell expansion:

```toml
[env]
PATH = "$PEKIT_ROOT/tools:$PATH"
TOKEN_PATH = "$PEKIT_KEYRING_API_TOKEN_PATH"
```

User env layers merge in this order:

```text
workspace [env]
  < delegated source pekit.toml [env]
  < delegated source selected env-file [env]
  < recipe pekit.toml [env]
  < recipe selected env-file [env]
```

Rules:

- Workspace `[env]` comes from `workspace.pekit.toml` and is the lowest shared
  default.
- Workspace `[env]` applies to workspace fan-out and to direct recipe commands
  invoked inside a discovered workspace member.
- Source env layers participate only when env delegation is enabled.
- Recipe env layers always override delegated source env layers.
- Selected env-file `[env]` values override same-owner `pekit.toml` `[env]`
  values.
- Within a layer, TOML document order is preserved.
- Duplicate names across layers are allowed; later exports run later and become
  the final shell value.

User env names must match:

```text
[A-Za-z_][A-Za-z0-9_]*
```

User env may not set any variable whose name starts with `PEKIT_`. The whole
`PEKIT_` namespace is owned by Pekit.

`[env]` values are shell fields, not Pekit templates. They are evaluated by the
inner shell when the target script runs, so normal shell parameter expansion is
available. Recipes are responsible for quoting values appropriately.

Keyring exports:

- Keyring values are emitted before user env so user env can reference them.
- Keyring values are never logged.
- Keyring values are not passed to the outer wrapper environment.
- Multiple keyring files and literal keyring values overlay as defined by the
  keyring flag rules.
- If a generated keyring export name collides with any non-keyring Pekit
  managed export name, planning errors.

Managed export collisions:

- Pekit-generated export names must be unique after sanitization.
- Direct dependency output variables that collide after sanitization are errors
  when those dependencies are exported to the same target.
- Keyring-to-keyring collisions are resolved by keyring overlay order.

Outer wrapper environment:

- The wrapper process receives non-secret Pekit-managed exports known for that
  target, including roots, timestamps, version variables, `PEKIT_OUT`, and
  direct dependency output variables where applicable.
- The wrapper process does not receive keyring exports.
- The wrapper process does not receive merged user `[env]` exports. Those are
  evaluated inside the target script so they see the wrapper-provided runtime
  environment.

### Stage Preparation

`clear_out` controls whether target/package stages are removed before being
prepared. It defaults to true.

Planning decides which build targets are:

- built
- reused because of `--no-build`
- unavailable/unresolved in dry-run

Execution follows that plan.

For a reused build target:

- The build command is not run.
- Its build dependency subtree is pruned.
- Its existing stage path is used.

For a built target:

- Its dependencies are prepared first.
- Its stage directory is prepared.
- Its command runs.

If a build target completes and leaves its stage empty, Pekit may warn.

### Wrappers, Env, and Keyrings

All shell-running targets use the same wrapper/env/keyring machinery:

- build
- test
- install
- clean targets

If a command mode does not run a shell target, wrapper/env/keyring flags are
unused and require `--allow-unused` when supplied.

The target script runs under:

```text
sh -euc
```

unless wrapped by an array wrapper that directly invokes another program. The
inner script still uses shell syntax because target commands are shell commands.

String wrappers run through shell templating. Array wrappers run directly as
argv. Both forms are defined in the env files section.

## Recipe Files

Pekit v2 keeps the current split:

- `pekit.toml` for build/test/install/clean targets and source selection.
- `package.pekit.toml` for package definitions.
- `workspace.pekit.toml` for workspace discovery.
- `env.pekit.toml` or named env files for wrappers and selected env overrides.
- `<name>.keyring.pekit.toml` for injected values.

This split is familiar and mostly sound. The redesign should revisit exact
field names and merge rules, but not collapse everything into one file unless
there is a strong reason.

### No Format Version Field

Pekit v2 does not include explicit recipe format versions initially.

The format is still expected to change while Pekit v2 is being designed and
implemented. Strict parsing and clear errors are preferred over premature
compatibility layers.

A format version may be added later when the recipe format is stable enough
that supporting multiple versions is worth the complexity.

### `pekit.toml`

`pekit.toml` owns command targets, command environment, source selection, and
delegation.

Sketch:

```toml
out_dir = "out"
clear_out = true

delegate = true

[source.git]
url = "https://github.com/example/app.git"
ref = "v{{version}}"
versions = ">= 1.0"

[source.local]
path = "../app"

[env]
PATH = "$HOME/.cargo/bin:$PATH"

[build.main]
command = "cargo build --release"

[test.unit]
needs = ["main"]
command = "cargo test"

[install.cli]
needs = ["main"]
command = "install -Dm755 \"$PEKIT_MAIN_OUT/app\" /usr/bin/app"

[clean.generated]
command = "cargo clean"
```

Decision: v2 recipe keys use snake_case consistently.

Examples:

- `out_dir`, not `outDir`
- `clear_out`, not `clearOut`
- `side_effects`, not `sideEffects`
- `optional_dependencies`, not `optionalDependencies`
- `sd_overrides`, not `sdOverrides`

Old camelCase names are not accepted in canonical v2.

Command target shape stays close to current Pekit:

- bare `[build]` remains shorthand for `[build.main]`
- named `[build.<target>]` declares target `<target>`
- mixing bare and named targets in the same section is an error
- `command` is required
- `needs` names build targets, not sibling test/install targets

### `package.pekit.toml`

`package.pekit.toml` owns package format, package metadata, file mappings,
package dependencies, multipack, and publish targets.

Sketch:

```toml
format = "peipkg"
builds = ["main"]

[package]
name = "app"
version = "{{version}}-1"
architecture = "x86_64"
description = "Example app"
license = "MIT"

[dependencies]
libc = "*"

[files]
":bin/app" = "usr/bin/app"
"@source:LICENSE" = "usr/share/licenses/app/LICENSE"
"@recipe:files/app.service" = "etc/systemd/system/app.service"

[[publish.localdir]]
path = "pkgsOut"
```

Package files use the owner-relative source-ref rules described earlier.

### `workspace.pekit.toml`

`workspace.pekit.toml` marks a workspace root and discovers members.

Initial shape:

```toml
include = ["pkgs/*", "tools/*"]
exclude = ["pkgs/_template", "pkgs/.cache"]

[env]
PKG_CONFIG_PATH = "$PEKIT_WORKSPACE_ROOT/sysroot/lib/pkgconfig:$PKG_CONFIG_PATH"
```

Workspace package defaults are implicit: a sibling `package.pekit.toml` beside
`workspace.pekit.toml` is inherited by members as the lowest package layer.
Workspace `[env]` is also a lowest-layer environment default for members.

Rules:

- `include` is required.
- `include` is an array of workspace-root-relative glob patterns.
- `exclude` is an optional array of workspace-root-relative glob patterns.
- Members are directories containing `pekit.toml`.
- Excludes apply after includes.
- Duplicate matches are deduplicated.
- Final members are sorted.
- `[env]` is optional.
- Workspace `[env]` follows the same name and value rules as recipe `[env]`.
- Workspace `[env]` applies to workspace fan-out and direct member commands
  invoked inside the discovered workspace.

Decision: initial v2 does not include named workspace groups. Groups can be
added later if there is a real CLI and planning use case.

### Env Files

Env files remain separate from `pekit.toml` initially:

```text
env.pekit.toml
ci.env.pekit.toml
```

Shape:

```toml
[wrap]
command = "nix develop --command sh -euc {{command}}"

[env]
RUSTFLAGS = "-C target-feature=+crt-static"
```

Env files may contain `[wrap]`, `[env]`, or both. At least one section must be
present.

Env file selection:

- The default env name is `main`.
- `main` maps to `env.pekit.toml`.
- Any other name maps to `<name>.env.pekit.toml`.
- Missing `env.pekit.toml` for `main` is a no-op.
- Missing `<name>.env.pekit.toml` for a non-main env name is an error.
- `--env=none` disables env file loading and wrapping for that invocation.

Decision: `[wrap].command` supports both string and array forms.

String form:

```toml
[wrap]
command = "nix develop --command sh -euc {{command}}"
```

Semantics:

- Runs through `sh -euc`.
- `{{command}}` is replaced with the shell-quoted inner script.
- Shell behavior is available in the wrapper string, including variables,
  quoting, pipes, and command chaining.

Array form:

```toml
[wrap]
command = ["nix", "develop", "--command", "sh", "-euc", "{{command}}"]
```

Semantics:

- The first array element is the program.
- Remaining elements are argv.
- Runs directly with argv, not through `sh -c`.
- `{{command}}` must appear as a complete argument.
- `{{command}}` is replaced with the raw inner script string, not shell-quoted,
  because argv boundaries are already structured.
- If shell composition is needed, use string form.

Common rules:

- `command` must be non-empty.
- Exactly one `{{command}}` placeholder is required.
- Pekit-managed outer env values such as `PEKIT_ROOT` and timestamps are
  available to the wrapper.
- The inner target script still contains Pekit-managed exports so it can survive
  env-scrubbing wrappers.
- `[env]` in an env file participates in the environment assembly layer for
  that selected env name.
- Source env-file `[env]` values participate only when env delegation is
  enabled.
- Source env-file `[wrap]` values participate only when wrap delegation is
  enabled.
- Recipe env files override delegated source env files for both `[env]` and
  `[wrap]`.

### Keyring Files

Keyring files remain TOML and are loaded by `--keyring=<name-or-path>`.

Example:

```toml
[tcb]
priv = "/run/secrets/tcb.pem"
pub = "DEADBEEF"
```

String leaves become `PEKIT_KEYRING_*` values. Non-string leaves are errors.

Decision: initial v2 keyrings are env-var only.

Rules:

- Keyring values are string leaves.
- Values are opaque to Pekit.
- Each string leaf exports `PEKIT_KEYRING_<SANITIZED_PATH>`.
- Keyring values are never logged.
- Multiple keyrings overlay in CLI order.
- Literal `--keyring.x.y=value` flags override keyring files.

Typed or materialized keyring entries are reserved for a future design.

Potential future shapes:

```toml
[tcb]
priv = { path = "/run/secrets/tcb.pem" }
cert = { content = "..." }
```

Initial v2 should reject table leaves with an error that typed keyring entries
are not supported yet.

## Configuration Loading

Configuration loading should be a staged pipeline, not a set of file reads
scattered through planning and execution.

Pipeline:

```text
discover roots
  -> discover candidate files
  -> parse TOML
  -> decode strict typed config
  -> attach owner/context
  -> merge layers
  -> validate effective config
```

This separates where a file came from from what the file means. That is
important for package source refs, delegation, workspace defaults, and error
messages.

### Root Discovery

Auto discovery is the default.

For normal recipe commands:

```text
pekit package
pekit build main
pekit test unit
```

Pekit walks upward from the current directory to find the nearest `pekit.toml`.
The directory containing that file is the recipe root.

For workspace commands:

```text
pekit workspace package
```

Pekit walks upward from the current directory to find the nearest
`workspace.pekit.toml`. The directory containing that file is the workspace
root.

Explicit root flags are deterministic overrides for scripts, CI, and build
farms:

```text
pekit --recipe /path/to/pkg package
pekit --recipe /path/to/pkg/pekit.toml package
pekit --recipe github.com/org/recipes//pkgs/libc@main package
pekit --workspace /path/to/ws workspace package
pekit --workspace /path/to/ws/workspace.pekit.toml workspace package
pekit --workspace github.com/org/recipes//workspace@main workspace package --all
```

Rules:

- `--recipe` points to either a directory containing `pekit.toml` or directly
  to a `pekit.toml` file, or to a remote git recipe locator.
- `--workspace` points to either a directory containing `workspace.pekit.toml`
  or directly to a `workspace.pekit.toml` file, or to a remote git workspace
  locator.
- `--recipe` disables upward recipe discovery for that invocation.
- `--workspace` disables upward workspace discovery for that invocation.
- Normal recipe commands may still discover their containing workspace after
  the recipe root is known, so workspace package defaults can apply.
- Workspace commands require a workspace root, either explicit or discovered.
- `--recipe` and `--workspace` together are invalid in initial v2.

### Remote Recipes

Remote recipes are still supported because the shortcut is useful:

```text
pekit install github.com/peios/x
pekit package github.com/org/recipes//pkgs/libc@main
pekit build github.com/org/recipes//tools/foo@v1.2.3 main
```

V2 should make this a first-class parser rule rather than scanning raw argv
before flag parsing.

Remote recipe shorthand rules:

- Supported for `build`, `test`, `install`, `package`, `publish`, and `clean`.
- After flags are parsed, the first positional argument may be a remote recipe
  locator.
- If present, it becomes the invocation recipe root and is removed from the
  command's target/package selectors.
- Remaining positionals are normal selectors.
- A locator after `--` is treated as a literal selector, not remote shorthand.
  Use `--recipe` for an explicit remote recipe when `--` is also needed.
- A remote recipe locator in any later positional slot is an error with a hint
  to move it first or use `--recipe`.
- `--recipe <remote>` is the explicit equivalent and is preferred for scripts.
- Workspace commands use `--workspace <remote>` in initial v2; do not add a
  positional workspace shorthand until there is a clear grammar for it.

Remote locator syntax:

```text
github.com/peios/x
github.com/org/recipes//pkgs/libc@main
https://github.com/org/recipes.git//pkgs/libc@main
git@github.com:org/recipes.git@main
```

Rules:

- Host/path shorthand such as `github.com/peios/x` expands to a git clone URL.
- Full git URLs are accepted.
- `//subdir` selects a recipe or workspace subdirectory inside the repository.
- `@ref` selects a branch, tag, or commit.
- If no ref is given, use the remote default branch.
- If no subdir is given, use the repository root.
- The selected recipe subdir must contain `pekit.toml`.
- The selected workspace subdir must contain `workspace.pekit.toml`.
- Prefer `//subdir` over current Pekit's implicit extra path segments. It is
  less magical and avoids guessing where the repository path ends.

Remote recipe materialization:

- Treat remote recipe resolution as invocation-root setup, separate from a
  package `[source]`.
- Resolve git refs to a concrete commit.
- Cache checkouts by normalized repository URL plus resolved commit.
- `@recipe:` resolves to the selected recipe subdirectory.
- `@workspace:` resolves to the selected workspace subdirectory for remote
  workspaces.
- Recipe provenance records remote URL, requested ref, resolved commit, and
  selected subdir.

Dry-run behavior:

- Remote recipe or workspace loading may happen before planning, because Pekit
  cannot know the recipe without it.
- `--dry-run` may therefore materialize an explicit or shorthand remote recipe
  root if it is not cached.
- Dry-run must still not materialize package sources, run builds, write
  packages, publish, or clean.
- Verbose and dry-run output should clearly show that remote recipe fetching was
  invocation setup.

Possible future flag:

```text
--offline
```

This would require remote recipes and package sources to already exist in cache.
Do not design it until offline/farm requirements are clearer.

### Candidate Files

Once roots are known, Pekit discovers candidate files by role:

- recipe root: `pekit.toml`, package files, env files, keyring files
- workspace root: `workspace.pekit.toml`, optional workspace package defaults
- source root: delegated `pekit.toml`, package files, and env files only for
  enabled delegation surfaces

Source-delegated files are discovered only after source resolution and
materialization. Dry-run may report delegated files as unresolved when the
source is not already available and materialization would be a side effect.

### Owners and Context

Every decoded config object carries an owner and origin path.

Owners:

| Owner | Meaning |
| --- | --- |
| `workspace` | Loaded from the workspace root. |
| `recipe` | Loaded from the invoking recipe root. |
| `source` | Loaded from a delegated source root. |
| `env` | Loaded from a selected env file. |
| `keyring` | Loaded from a keyring file. |

Origin context should include:

- absolute file path
- root kind and root path
- owner
- package member selector, if applicable
- env name, if applicable
- keyring name/path, if applicable

This context is preserved through merge and validation so diagnostics can point
to the file and field that caused a problem.

### Strict Decode

TOML parsing and strict decode happen before merge.

Rules:

- Invalid TOML errors before any semantic validation.
- Unknown keys error in the file where they appear.
- Wrong value types error in the file where they appear.
- Ambiguous shapes error in the file where they appear.
- Deprecated compatibility aliases are not accepted in canonical v2.

Examples of ambiguous shapes:

- bare `[build]` mixed with named `[build.main]`
- both `delegate = true` and `[delegate]`
- multiple reproducible source tables, such as `[source.git]` and
  `[source.url]`

### Merge and Validation

Merge uses named policies:

- `env-overlay`
- `target-overlay`
- `package-overlay`
- `publish-overlay`

Merge should happen on typed decoded config, not raw TOML tables.

Validation has two phases:

1. Decode-time validation for local file shape and field types.
2. Effective-config validation after merge, when Pekit knows which command,
   source, version, packages, and targets are selected.

Semantic errors should include effective context as well as source location.

Example diagnostic location:

```text
packages.pekit/libc.package.pekit.toml:[package].architecture
```

The validator should prefer one precise error over a cascade of follow-on
errors. When multiple independent errors exist, reporting them together is fine
as long as each has a clear path and field.

## Compatibility With Current Pekit

Current behavior is documented in:

```text
pekit/docs/current-behavior.md
```

For each current behavior, Pekit v2 should mark one of:

- `preserve`: keep the behavior as-is.
- `change`: keep the concept but alter the behavior.
- `remove`: intentionally drop the behavior.
- `undecided`: needs review.

Compatibility should be intentional. The rewrite should not inherit current
edge cases just because they are easy to copy.

Decision: Pekit v2 is a clean command-line and recipe-format break. Do not add
a compatibility mode in the initial v2 design. Current behavior remains useful
as migration reference material, not as an alternate runtime mode.

Initial review candidates:

- Command-scoped flag validation.
- `clean` behavior and whether it should use env/keyring/wrap consistently.
- Whether clean should fan out across named clean targets.
- Whether `test` and `install` with a source should run in the recipe directory
  or source tree.
- Whether local mode should prefer local source or require it strictly.
- Whether literal file sources may escape the literal root.
- Whether absolute `outDir` should be allowed.
- Whether source env/wrap fallback should apply consistently across commands.

## Open Decisions

No open decisions currently require product-level input.

Implementation-owned decisions:

- Define the stable JSON plan object and NDJSON event schemas during
  implementation, following the output renderer contract in this design.
