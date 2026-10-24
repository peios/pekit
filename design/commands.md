# Layer E: the commands

This is the design for layer E of `PLAN.md`: `pekit`'s commands and their
shared flags. It applies what `core-model.md` agreed in "Commands, scripts
and release", "Packaging" and, for tests, "Open questions" 2 (example 7),
to the engine (`engine.md`) and libraries (`libraries.md`) as built. Where
the agreed design leaves a choice, this takes one and lists it at the end,
for Jack to change.

## Decisions

### M1. Targets are names, qualified names or directory globs

Every command that takes targets takes any number. A target is named as
in example 1: `hello`, or qualified by its directory, `app/hello`. A
directory glob, `pkgs/*` or `**` (as imports' globs are, `*` one segment,
`**` any number), selects every definition of the command's def-type in
the directories it matches, relative to the current directory. `pekit
build` needs at least one target; `pekit test` and `pekit package`, with
none, take `**` from the current directory, every test or package below
it, as running the tests of where you are is the common case.

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
