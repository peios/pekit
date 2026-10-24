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
- `Workspace::run_step(graph, node, defers)`: a deferred node's call run as
  a unit of its own in graph mode, its effects nodes of that unit, nested
  deferred calls inline;
- `EffectJob`: an effect node's built-in and plain arguments, `Send`, whose
  `run(&dyn Effects)` gives a plain result or a failure on any thread,
  without the workspace;
- `Plain::Sandbox`, since a sandbox is a value a step's body passes between
  its effects;
- deadlines in graph mode: a step's `#[timeout]` is the engine's to keep,
  across the step's attempts, since a restart would start GCSL's deadline
  afresh.

### E2. Steps and what is a step

A deferred call is a step if the engine takes it as one: `#[hermetic]` (or
`#[hermetic(true)]`), the attribute declared deferring. `#[hermetic(false)]`
runs inline. Effects outside any step are steps of their own, run in
program order, unsandboxed and uncached, as plain GCSL runs them.

### E3. Keys and receipts

A hermetic step's *receipt* is a SHA-256 over:
- its code: the sources of `StepInputs::code_files`, by path and content;
- its arguments, captures and context as plain data, every file and
  directory by its content's digest rather than its path, so that two
  identical inputs at different paths key alike and early cutoff follows;
- the identities of the secrets it is given, never their values;
- the platform it runs on (E7).

`#[retry]` and `#[timeout]` are not on it. A step's *record*, kept forever,
is its receipt, its result as plain data with every output file by digest,
its duration, and the receipts it used. Only successful steps are recorded.

### E4. The content store

Content-addressed, under the cache directory: a blob per file by SHA-256,
directories as trees of digests (REAPI's shape, so a remote cache can be
added later without reshaping), records apart from contents. Eviction
(GreedyDual-Size, layer F) evicts only contents; records stay, so a
downstream receipt is computable without the bytes.

### E5. The sandbox

A hermetic step runs under bubblewrap, with:
- no network, its own user, pid and mount namespaces;
- workspace files at their workspace paths, read-only, materialised from
  the store, and outputs of earlier steps at their `pekit-out/` paths;
- a fresh writable working directory, which is the step's
  `pekit-out/<target>/<step>/` path;
- an empty environment but for what the step's `exec` sets, `PATH` empty.

Its result's files are captured into the store and given back at their
`pekit-out/` paths. A file its result names that is not under its working
directory, or that does not exist, is the step's failure. `Dir.sandbox` in
a step is GCSL's own copy for now; overlay mounts come later.

### E6. The scheduler

The build graph is the action graph seen through E2: a hermetic step keeps
its data edges and drops its order edges; anything else keeps both. The
scheduler repeatedly:
- advances every unit that can progress (the target's, each step's);
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
   example 1's notes.
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
- E3: files keyed by content digest, not path; the code key is the needed
  files' sources, coarse but correct.
- E5: a step's working directory is its `pekit-out/` path, `PATH` empty.
- E9: pekit's def-types and attributes from a GCSL standard module.
