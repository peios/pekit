# Layer E: the commands

This is the design for layer E of `PLAN.md`: `pekit`'s commands and their
shared flags. It applies what `core-model.md` agreed in "Commands, scripts
and release", "Packaging" and, for tests, "Open questions" 2 (example 7),
to the engine (`engine.md`) and libraries (`libraries.md`) as built. Where
the agreed design leaves a choice, this takes one and lists it at the end,
for Jack to change.

## Decisions

### M1. Targets are names, qualified names or globs

Every command that takes targets takes any number. A target is named as
in example 1: `hello`, or qualified by its directory, `app/hello`. A glob
selects every definition of the command's def-type it matches, relative to
the current directory, and reads as a qualified name does: its last part
the name, the parts before it the directory. `app/*` is every definition
of `app`, `app/test_*` some of them, `pkgs/*/*` every one a directory of
`pkgs` holds; `*` matches any run of characters in a part. `**` matches
any number of directories and, as the last part, every name in them, so
`**` and `pkgs/**` are every definition below; `.` is the current
directory's. `pekit build` needs at least one target; `pekit test` and
`pekit package`, with none, take `**` from the current directory, every
test or package below it, as running the tests of where you are is the
common case.

Globs were first directory globs, every segment a directory, as imports'
are: `pkgs/*` was every definition one level below `pkgs`, and `app/*`
nothing when `app` had no directories below it. Jack chose, after the
pekit course of 2026-10-26, that a glob read as a qualified name does, so
that `app/*` is `app`'s definitions; `pkgs/*` is now `pkgs/*/*`.

Several targets are evaluated as one evaluation, one graph: a step two
targets reach runs once, and steps of different targets run at once,
tests among them. Each target is a prepared call of its own, a unit of
its own, driven together; a step belongs to the target whose unit met it
first, or to the target of the step that met it, and its directory is
under that target's. A target that fails does not stop the others; the
command reports each and exits 1 if any failed.

### M2. The shared flags

- `--jobs N`, `--trace FILE` and `--profile NAME`, as built.
- `--platform ARCH[-OS]` sets `^platform` at the root, where the workspace
  declares it: what is built for, as `with { platform = ... }` would.
  `^host_platform` stays the machine.
- `--rebuild` runs every step, reading nothing from the store, and records
  each as ever.
- `--check-reproducible` builds, then builds every step again reading
  nothing from the store, and compares each step's result and outputs
  byte for byte with the first's. A mismatch is reported, naming the step
  and the files that differ, and the command fails.
- `--no-dynamic` fails a target whose evaluation must wait for a step's
  result, a dynamic dependency (example 9), at the point it would: so CI
  can hold a part of the tree to a graph known before anything runs. Only
  a pause for a step counts; a target's own fetches and unpacks are
  effects it waits for as ever.
- `--strict` builds as a release does (M7): steps borrow no host tools, a
  target's own effects other than fetches and unpacks are refused, and
  `#[scratch]` directories start empty.

### M3. `def test`: a test is a step whose failure is a result

The prelude declares `test` beside `build` and `package`:

```text
type TestResult = {name: String, passed: Bool, message: String}
#[def_type] #[hermetic] def test(name: String, f: fn() -> {}) -> TestResult
```

A test's body is the definition's own function, so `#[hermetic]` on the
def-type makes each a step by default, and a test that cannot be hermetic
says `#[hermetic(false)]` (as agreed). Its parameters' defaults build what
it needs, as example 7 draws it, so a test is given its program. The
def-type's own body calls it under `recover`, awaiting it there, outside
the step, so a failure after its retries is the result `passed = false`,
its message the failure's, never a broken build (agreed: the `recover`
sits outside the hermetic step). Only a passing test's step is recorded,
so a failed test runs again next time (agreed).

Spiking this found two things GCSL lacked, both since built
(`06a5236`): a deferring attribute on a def-type deferred the def-type's
own invocation, its body in the step, where it now defers each
definition and never the def-type; and a definition could not declare
parameters of its own, where `def test runs(prog: File = build app())`
now gives the step the built program as its argument, the defaults
evaluated where the def-type calls the definition.

`pekit test` runs the tests it selects, one evaluation, and reports each:
`passed`, `passed (cached)`, `flaky`, passing only on a retry, or `FAILED`
with its message. It exits 1 if any failed.

### M4. `pekit run`: scripts, and builds that give a program

The prelude declares `script`. `pekit run NAME` evaluates the script `NAME`
outside any sandbox, with the terminal and the environment, its effects
in program order, its builds built as `pekit build` builds them. `pekit
run` of a `build` whose result is one executable `File` builds it and runs
the file, with the rest of the command line as its arguments. A `build`
and a `script` of one name in one directory are an error, since `pekit run
NAME` would be ambiguous (agreed).

A build, a test or a package cannot call a script, checked when the file is
checked, so that building never boots or publishes (agreed). It is checked
with GCSL's needs: the `script` def-type's own body reads a context that
nothing binds but `pekit run`, `^script_session`, which has no default, so
calling a script needs it; `build`, `test` and `package` take functions
that need nothing; so a build that calls a script, through any number of
functions, does not check. A spike confirmed it: a `build` calling a
script, directly or through a helper, is "context `script_session` has no
value in this definition" when the file is checked. A script's own
function type must be allowed to need `^script_session` so that scripts
can call scripts, which the same spike showed fails without GCSL's
written syntax for needs, a follow-up of layer A that this layer does.

### M5. `pekit package` and `pekit publish`: the configured packagers

`pekitconf.packagers` lists the workspace's packagers, each a record of
two functions, `pack` and `publish`, as agreed; the ppk library gives
`ppk.packager()`. They are a context, `^packagers`, of a standard
`packaging` module, set from `pekitconf` as the toolchains are, and that
module's `pack(pkg)` packs a package with each. Pekit cannot call a
function value from outside GCSL, so it prepares the call
`packaging.pack(package NAME())`: GCSL gains a prepared call wrapped in a
top-level function. `pekit package` packs every selected package with every
packager, each `pack` a hermetic step, and shows where each went. `pekit
publish TARGETS --to STORE` packs them and then calls each packager's
`publish` with the packed result and the store, in program order, outside
any step: the only command but `run` and `release` that changes the world.
The ppk library's `publish` copies the artifact directory into the store
with `ppk fetch`, which refuses a version whose content changed.

### M6. `pekit clean`

`pekit clean` removes `pekit-out/`. `pekit clean --cache` also removes the
store, every record and blob, and the scratch directory. Eviction by cost
(GreedyDual-Size) is layer F.

### M7. `pekit release NAME`

As agreed: the tree must be clean and committed, and is built from `HEAD`;
strict mode (M2), the release profile, `--check-reproducible` against the
store, then publish, then the tag `name@version`, only ever added, and an
existing tag on another commit is an error. `version` is the package's. It
comes after M1 to M6, since it is all of them at once.

### M8. `pekit query`

`pekit query deps TARGET` lists the steps a target's evaluation reaches and
the workspace files each reads; `pekit query rdeps FILE` lists the targets,
among those selected, whose evaluation reads the file, the question CI asks
to run only the tests a change reaches (example 7). Both evaluate without
running steps where they can, and report a dynamic dependency as unknown
rather than building to find out.

### M9. `pekit lock`

`pekit.lock` pins what a build would otherwise ask the outside world for:
libraries by commit (L7), fetched into `.gcsl/libraries/`, and in time
packages from a repository (example 10). `pekit lock` resolves and writes
it; every other command reads it as written, and `--strict` fails where it
is missing or stale.

## Steps

Each leaves pekit working, tested and documented as far as it goes.

0. **E0, targets and flags** (M1, M2 without `--strict` and
   `--check-reproducible`): several targets, globs, `--platform`,
   `--rebuild`, `--no-dynamic`, and reports per target.
1. **E1, `def test` and `pekit test`** (M3): example 7 runs.
2. **E2, `pekit clean`** (M6).
3. **E3, packagers, `pekit package` and `pekit publish`** (M5): example 5
   through the commands.
4. **E4, GCSL's written needs, `def script` and `pekit run`** (M4).
5. **E5, `--check-reproducible` and `--strict`** (M2).
6. **E6, `pekit query`** (M8).
7. **E7, `pekit lock`** (M9).
8. **E8, `pekit release`** (M7).

Example 10 then needs Peios's own packages built through pekit2, which is
its own piece of work, after layer E.

## Decisions taken without Jack (to surface)

- M1: `test` and `package` default to every definition below the current
  directory; `build` needs a target.
- M2: `--strict` refuses a target's own effects but fetches and unpacks,
  and gives steps no host tools.
- M3: the `TestResult` record, and `pekit test`'s four outcomes.
- M4: scripts kept from builds by a context only `pekit run` binds,
  checked through GCSL's needs; GCSL gains written needs for it.
- M5: `publish` runs in program order outside any step, with `--to`.
- M6: `pekit clean` leaves the store unless `--cache`.

## As built

- E0: several targets are one `Run`, each a root of its own unit; a step
  is the root's whose unit met it, recorded by unit, so a step met in a
  step's unit is of that step's target. Outcomes come in `select`'s
  order, by qualified name. A glob `dir/*` is the directories below
  `dir`, `dir/**` `dir` and everything below.
- E1: the prelude's `test` awaits its definition under `recover`; a
  definition's function is named by its path's last segment, so a test's
  step is named after it, which is how `pekit test` finds whether it was
  found in the store. A test whose program fails to build fails, with
  the build's error, since its parameters' defaults run inside the
  `recover`. A failure shows its message alone; the values an `assert`
  labels its sides with are a follow-up.
- E2: `pekit clean` removes `pekit-out/`, and with `--cache` the store and
  its scratch, never the state directory, which holds the keychain's key.
- E3: `Workspace::prepare_call_wrapped` gives a target's call to a
  top-level function, with arguments of its own after it:
  `packaging.pack(package NAME())` and `packaging.publish(package NAME(),
  "STORE")`. The store is an argument, not a root context, since a root
  context keys every step under it: publishing into a second store packs
  nothing again. A `publish` fails a package alone, as any target's
  failure does; the packages before it in program order stay published.
  `ppk fetch` makes a store directory that is not there.
- E4: GCSL's function types can be written needing contexts, `fn(^arch)
  -> T`, after their parameters, as they are shown; a declared `type`
  cannot need one yet, since it is resolved in each file using it. The
  prelude's `script<T>(name, f: fn(^script_session) -> T) -> T`, with
  `ScriptSession = {args: List<String>}`, is the def-type. `pekit run NAME
  ARGS...` runs the script `NAME`, its own effects on the host view, the
  progress line cleared while one runs, since it may have the terminal;
  or builds the build `NAME` and runs the one executable `File` it gives
  on the same view, in pekit's directory, exiting with its status. The
  host view is a bubblewrap, so a script's programs cannot gain
  privileges: `sudo` fails in one. A build and a script of one name in
  one directory fail every command at load; of one name in two
  directories, `pekit run` asks for the directory. Two engine changes
  came with it, to keep a script's builds those of `pekit build`: steps
  keyed by the context they can read, and named under the definition
  whose call made them (`engine.md`, as built in E4).
- E5: `--check-reproducible` builds, then builds again with every step
  run and compared, not recorded, where its receipt has a record
  (`Settings::compare`): only steps given the same inputs are compared,
  so one difference is one report, each differing file named, a
  directory's looked into through its trees. `pekit run` and `pekit
  publish` refuse it, since their effects would be done twice.
  `--strict` gives steps no host tools and keys them apart, `<platform>
  no-host-tools` and no host fingerprint, so their records are any
  machine's of the platform; a target's own effects may fetch, unpack,
  ask the keychain and read `/w` and `/pekit-out`, and nothing else,
  `which` among the refused, so the host toolchains fail a strict build.
  Reads and secrets are allowed beyond M2's "fetches and unpacks": reads
  of the workspace and outputs are fixed by the build's inputs (example
  9 needs them), and a release signs. `#[scratch]` is not built yet, so
  there is no scratch directory to start empty.
- E6: `pekit query deps TARGET... [--kind KIND]` and `pekit query rdeps
  FILE [TARGET...] [--kind KIND]`, `build` by default, over the engine's
  query mode (`query_all`): a target's own effects run, a fetch's or a
  read's, no step starts, and a target that waits for a step's result is
  done there, with what it reached and where it waits. A target reads a
  file if its call's code needs it (`PreparedCall::files` and
  `Workspace::files_needed`), a step's code does
  (`Workspace::node_code_files`), its own effects read it, a step is
  given it, or a directory given holds it; and a GCSL file is read by
  every target whose code is in its directory or below, its definitions
  seen there. `rdeps` lists a target that waits as reading every file,
  noted on standard error: unknown is never no.
- With E6, a step's naming was made independent of what else is built:
  a prepared call binds its own definition as a reference's call does
  (GCSL's `run_prepared`), so two targets that make the same call in
  their own code each have their step, named by its definition, rather
  than sharing one named after whichever met it first, whose name, and
  so key, depended on what else was built. Work two targets share is
  shared by putting it in a build both call, as Bazel's actions belong
  to their targets. A decision taken without Jack, to surface.
- E7: `pekit.lock` is JSON, written by `pekit lock`: `sources`, a prefix of
  import paths each with a git repository, the directory in it holding
  the prefix's libraries and the revision followed, editable by hand,
  and `libraries`, each pinned by path, repository, directory, commit
  and the digest of its exported tree. A library no source names follows
  Go's convention, `HOST/OWNER/REPO` the repository `https://HOST/OWNER/REPO`
  at `HEAD`, its source added to the lock. Repositories are bare clones
  in the cache's `git/`, fetched only to resolve a revision or for a
  commit missing; a library is exported with `git archive` into
  `.gcsl/libraries/PATH`, whole or as it was. `pekit lock` pins what is
  not pinned, `--update` everything, and drops what is no longer
  imported, its directory with it. Every command that builds first puts
  back a pinned library that is missing, and warns of one that differs;
  `--strict` fails on that, on an imported library not pinned, on no
  lock, and on `$GCSL_LIBRARY_PATH` set. The pinning of packages from a
  repository (example 10) waits for example 10.
- E8: `pekit release PACKAGE --to STORE [--jobs] [--trace]`: the workspace
  must be in a git repository and match `HEAD` but for `pekit-out/` and
  `.gcsl/`; `HEAD`'s tree of the workspace is exported with `git archive`
  into the cache's scratch, and built there with `--strict`,
  `--check-reproducible` and the `release` profile, the package's value
  giving its name and version. The tag `NAME@VERSION`, a lightweight one,
  needing no identity, is checked before publishing: on another commit,
  an error, nothing published; on `HEAD`, published again, not tagged
  again. Publishing is a target's own effect, which strict refuses, so
  the publishing pass keeps the strict build's step settings, no host
  tools and its platform key, so that its steps are found in the store,
  and allows the target's effects, already checked by the strict pass.
- Found with E8: a step found in the store had its outputs recorded but
  not placed in `pekit-out/`, so after `pekit clean`, or in a release's
  fresh tree, a target's own code reading one, or publishing it, found
  nothing. They are placed now, as a step's that ran are. A follow-up:
  placing every cached step's outputs re-links them on every build,
  whether or not they are already there.
