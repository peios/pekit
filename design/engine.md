# Layer C: the pekit2 engine

Status: specified 2026-10-24 (Claude), before building. The agreed model is
in `core-model.md`; layer B, the GCSL interface the engine drives, is in
`design/action-graph.md`. This note decides how the engine is built, and
splits it into steps that each leave something working.

## What the engine does

`pekit build TARGET` names a definition (`build hello`, or
`pekit2/examples/1/hello`). The engine:

1. loads and checks the workspace through GCSL;
2. evaluates the target's invocation in graph mode;
3. runs the steps the target needs, from the cache where it has them, in
   parallel where their edges allow, each hermetic one in a sandbox that
   holds only its inputs;
4. keeps every step's outputs in a content store and records each step's
   receipt;
5. puts the target's result under `pekit-out/`.

## Decisions

### E1. One GCSL thread, parallel effects

GCSL's values are `Rc`-based, so one thread evaluates everything. What runs
in parallel is the work steps do: programs. A hermetic step's body is itself
evaluated as a graph-mode unit (*a step unit*), so each `exec` in it becomes
an effect node, which the engine runs as a job on a worker pool, in the
step's sandbox. The step's body restarts once the job finishes, as any unit
does, and goes on to its next effect. One GCSL thread therefore drives as
many running programs as the pool has slots.

Rejected: a workspace per worker thread, each running a step's body itself.
A worker would have to rerun every file a step's code needs, top-level
effects included, to have the cells the body reads; and a step's closure
cannot cross threads anyway.

GCSL needs, for this (layer B follow-ups, step C1):
- `Workspace::run_step(graph, node, defers, effects)`: a deferred node's
  call run as a unit of its own in graph mode, numbered from the node, so
  that it is stable across attempts and apart from files' and prepared
  calls' numbers; its effects are nodes of that unit, nested deferred calls
  inline, and its own `#[retry]` and `#[timeout]` not applied (E5);
- the host's `Effects` for every graph-mode unit (E5's layout), not `Direct`;
- `EffectJob`: an effect node's built-in and plain arguments, `Send`, whose
  `run(&(dyn Effects + Sync), deadline)` gives a plain result or a failure
  on any thread, without the workspace;
- `Plain::Sandbox`, since a sandbox is a value a step's body passes between
  its effects.

### E2. Steps and what is a step

A deferred call is a step if the engine takes it as one: `#[hermetic]` (or
`#[hermetic(true)]`), the attribute declared deferring. `#[hermetic(false)]`
runs inline. Effects outside any step are steps of their own, run in
program order, unsandboxed and uncached, as plain GCSL runs them.

### E3. Keys and receipts

A hermetic step's *receipt* is a SHA-256 over:
- its code: the sources of `StepInputs::code_files`, by path and content;
- its arguments, captures and context as plain data, every file and
  directory by its path *and* its content's digest. The path is part of
  what the step sees (it reaches command lines and names outputs), so it
  stays in the key, as Bazel keys an action. Early cutoff comes from keying
  an input by its content, not by the receipt of the step that made it: a
  rebuilt input with the same bytes at the same path keys the same;
- the identities of the secrets it is given, never their values;
- the platform it runs on (E7).

`#[retry]` and `#[timeout]` are not on it. A step's *record*, kept forever,
is its receipt, its result as plain data with every output file by digest,
its duration, and the receipts it used. Only successful steps are recorded.
A result that holds a secret is never recorded, since a secret is never
stored in any cache (`core-model.md`): such a step runs every time. Its
output files are not cached either; signing outputs, which are no secret,
should be returned as files from a step whose result holds no secret.
Nothing that reaches a record, an event or a log holds a secret's value.

### E4. The content store

Content-addressed, under the cache directory: a blob per file by SHA-256,
directories as trees of digests (REAPI's shape, so a remote cache can be
added later without reshaping), records apart from contents. Eviction
(GreedyDual-Size, layer F) evicts only contents; records stay, so a
downstream receipt is computable without the bytes.

### E5. The sandbox

**The logical layout.** Every path a GCSL value holds, everywhere, is in
one layout, the same on every machine and checkout: the workspace at `/w`,
outputs at `/pekit-out`. The engine's `Effects` for every unit, the
target's included, maps that layout to the host (`host_path`) and gives
`/w/<the target's directory>` as the working directory that `file()`
resolves against. So `file("hello.c")` in `pekit2/examples/1` is
`/w/pekit2/examples/1/hello.c` in values, receipts and sandboxes alike. GCSL
must therefore take the host's `Effects` for graph-mode units too (C1), not
only for `evaluate`.

**Step paths.** A step's outputs are under
`/pekit-out/<target>/<step>/`, where `<step>` is the step's function's name
and its number among the target's steps of that name, in discovery order:
`compile`, `compile.2`. Output paths reach downstream keys, so the rule
must give the same path across runs for the same program, which discovery
order does; editing the program can renumber steps, which only reruns them.

A hermetic step runs under bubblewrap, with:
- no network, its own user, pid and mount namespaces;
- the workspace files it is given at their `/w` paths, read-only, and
  outputs of earlier steps at their `/pekit-out` paths, materialised from
  the store;
- a fresh writable working directory, which is the step's
  `/pekit-out/<target>/<step>/` path;
- an empty environment but for what the step's `exec` sets, `PATH` empty.

Its result's files are captured into the store and given back at their
`pekit-out/` paths. A file its result names that is not under its working
directory, or that does not exist, is the step's failure. `Dir.sandbox` in
a step is GCSL's own copy for now; overlay mounts come later.

**Retry and timeout.** A step's `#[retry]` and `#[timeout]` are the
engine's: a retry reruns the step unit in a fresh sandbox, an
infrastructure failure (the sandbox, the store, a lost job) retries
invisibly and never counts, a step that passes only on a retry is reported
flaky, and a timeout spans the step's attempts, killing its running jobs.
GCSL runs neither for a step's own call in graph mode; a `#[retry]` on an
inline call inside a unit retries as plain GCSL would, its failed effects
failing again.

### E6. The scheduler

The build graph is the action graph seen through E2: a hermetic step keeps
its data edges and drops its order edges; anything else keeps both. The
scheduler repeatedly:
- advances a unit only once a node it paused on, or a hole it waits on, is
  resolved: every advance is a rerun from the unit's start;
- takes the needed nodes that are ready;
- for a hermetic step: looks up its receipt, and resolves it from the
  record on a hit; else starts its step unit;
- for an effect of a step unit: queues its job on the pool;
- for any other effect: runs it in program order, one at a time.

The pool's size is `--jobs`, by default the number of CPUs. Each running
program holds one slot; a jobserver shared with `make` (layer F) lets a
step's `make -j` borrow slots.

### E7. Platforms

The execution platform is the machine pekit runs on, until remote execution
(layer F). Its description is on every receipt. `^host_platform` is set at
the root of every target's evaluation to it.

### E8. Events

The engine emits a stream of events (node discovered, ready, started,
cache hit, finished, failed, waited on), which the terminal display, the
Chrome trace (`--trace`) and later the web view consume. It is designed in
now, as `core-model.md` asks.

### E9. Where the def-types come from

The drafts write `def build hello()` and `#[hermetic]` with no import, and
plain GCSL must check the same libraries. So `hermetic`, `scratch`, and the
`build`, `test`, `package` and `script` def-types are declared in a GCSL
standard module plain GCSL also loads, which pekit gives its meaning. Which
module, and whether it is implicit, is settled in layer D with the
libraries; the engine reads them by attribute and def-type identity.

## Steps

Each leaves pekit2 working, and tested, as far as it goes.

1. **C1, GCSL support:** `run_step`, `EffectJob`, `Plain::Sandbox`, and
   graph-mode deadlines (E1).
2. **C2, the crate and loading:** the `pekit2` crate, the workspace loaded
   through `gcsl-disk`, targets found by def-type and path, errors as in
   example 1's notes. Until layer D settles where `hermetic`, `build` and
   `test` are declared (E9), C2's tests declare them in a fixture module.
3. **C3, the content store and records** (E3, E4), with digests of files
   and trees, and receipts.
4. **C4, the sandbox** (E5): staging, running, capturing.
5. **C5, the scheduler** (E1, E2, E6): units, the pool, program order.
6. **C6, events and the Chrome trace** (E8).
7. **C7, platforms** (E7).

`pekit build` is usable after C5.

## Decisions taken without Jack (to surface)

- E1: one GCSL thread and parallel effects, with step bodies as graph-mode
  units, rather than workspaces per thread.
- E3: files keyed by path and content digest; the code key is the needed
  files' sources, coarse but correct; a result holding a secret is never
  recorded.
- E5: the logical layout `/w` and `/pekit-out`, and steps named by function
  and number in discovery order.
- E5: a step's working directory is its `pekit-out/` path, `PATH` empty.
  As built, with the host snapshot below, a step whose `exec` sets no
  `PATH` gets `/usr/bin:/bin:/usr/sbin:/sbin`, since gcc finds its own
  parts by `PATH`; a pinned environment (layer D) will set its own.
- E9: pekit's def-types and attributes from a GCSL standard module.
- As built in C5: steps are named in the order the target's code met them
  (`Graph::nodes`), never in the order they started, so a name, and the
  output paths under it, do not depend on what was cached; and a step's own
  output directory is on its receipt.
- As built in C5, until layer D gives a pinned build environment: the *host
  snapshot*. Every step binds the host's `/usr`, `/bin`, `/lib`, `/lib64`,
  `/sbin`, `/etc/alternatives` and `/etc/ld.so.cache` read-only, with
  `/usr/local` hidden, as no package manager manages it. Its receipts'
  platform carries a fingerprint of `/etc/os-release`, the package
  database (dpkg, rpm, apk or pacman), the alternatives' targets and the
  linker's cache, so an upgraded compiler invalidates what it built. A host
  whose package database pekit does not know records steps but never
  reuses them, and says so. `pekit build` notes that such a build is not
  hermetic.
- As built in C5: the cache directory is `$PEKIT_CACHE_DIR`, else
  `$XDG_CACHE_HOME/pekit`, else `~/.cache/pekit`, holding the store and the
  staging directories (never `/tmp`, which is often memory).
