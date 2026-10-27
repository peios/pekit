# Layer B: the action-graph evaluator and embedding API

Status: specified 2026-10-24 (Claude), before building. The agreed model is
in `core-model.md` ("The model", "Engine design", "What GCSL needs"). This
note makes it precise for GCSL, records the mechanism chosen for pausing,
and splits the work into steps that each leave GCSL working.

## What exists (survey, 2026-10-24)

- A workspace runs whole files, eagerly: every cell of a file is computed
  into one shared frame, and a file runs only once its needed files have.
  An invocation (`Workspace::invoke`) runs a synthesised call on the same
  frame and memo tables.
- Effects run directly inside the evaluator (`call_builtin`, `system.rs`,
  `files.rs`, `sandbox.rs`). There is no host hook.
- Attributes reach the IR (`FnCode::attributes`); the host sees them only on
  a closure it already holds.
- `Unwind` already unwinds for break, continue and return; deadlines and
  errors travel as marked diagnostics.
- Nothing is `Send`: values, IR and memo tables are `Rc`-based, and
  evaluation is single-threaded.
- The checker already places an `Await` wherever a value that may be a
  future is used as its bits, and impure signatures' bare types are the
  either shape, `T | Future<T>`.

## Decisions

### D1. Pause points come from typing: deferring attributes

A call that becomes a node must give back a stand-in, and the code after it
must stop only where the result is used. GCSL already knows those places
for futures. So an attribute declaration can mark itself *deferring*:

```gcsl
#[attribute] #[defers] type hermetic = {on: Bool = true}
```

A call of a function carrying a deferring attribute, whatever its
arguments, has an either-shaped result: `File | Future<File>` for a
function declared `-> File`. The checker therefore awaits it where it is
used, as it awaits any future. A pure function cannot carry one, because a
pure function's calls are never nodes.

- Plain GCSL runs such a call at once. Its result is present, and every
  await of it is a no-op, so behaviour is unchanged.
- In graph mode the call may give back a future that the host resolves
  (D3). Whether it does is the host's choice at run time, from the
  attribute's arguments: `#[hermetic(false)]` can run inline.
- Typing depends only on the declaration, never on the host, so a library
  type-checks identically under every host. This is the constraint from
  example 6.

The either shape is on the **function's result type**, as if it were
declared `-> File | Future<File>`, not on the call expression. So
`let g = compile; g(x)` awaits too, and a function value carries it.
Checked 2026-10-24: an either-shaped value is awaited implicitly wherever a
present value is needed, a pure function's argument, a typed `let`, a
record field or an operand, so libraries that hand a result to a pure
helper keep working. The sites that build a function's result type must
all honour it: the signature's type, `inferred_result`, callback
narrowing, and a def-type reference's type.

**Def-types.** For `#[def_type] #[hermetic] def build(...)`, the step is the
*invocation*, `build main(...)`: a call of the def-type, which carries the
attribute, so its result is either-shaped. A definition inherits the
attribute as a default, but its own type stays its def-type's `F` exactly:
inheriting a deferring attribute does not change a definition's type.

Rejected: making every impure call's result either-shaped. That changes
typing everywhere for every program. Also rejected: detecting the use of a
stand-in in every typed walker at run time. That would touch every place
that takes a value apart, and a missed place would give a wrong
internal error instead of a pause.

### D2. Resuming: restart, as Bazel's Skyframe does

**Superseded on 2026-10-27 by `suspension.md`.** Restart departed from
Jack's model, and its cost grows with the square of a unit's effects. This
section is kept as history.

When graph-mode evaluation awaits a future the host has not resolved, the
evaluation *unit* stops. Once the host resolves what it waited on, the unit
runs again from its start. Everything before the pause recomputes to the
same values, the calls that made nodes find those nodes, and the await
that stopped it now gets its value.

This is safe because a graph-mode unit performs no effects of its own
(D4): it only computes, makes nodes and reads their results. It is
deterministic for the same reason.

Why not suspend the evaluator instead:

- An `async` evaluator boxes every recursive call. That would slow plain
  GCSL as well as graph mode, and it rewrites 9,000 lines of evaluator.
- Threads cannot share GCSL's `Rc` values. Units on separate threads would
  need every value that crosses between them copied as plain data,
  including half-computed cells.
- Bazel restarts for the same reasons and keeps restarts cheap by keeping
  units small. Pure evaluation is fast next to the steps it waits on.

The cost is recomputing a unit's prefix at each restart. A unit that waits
on *n* results in sequence restarts *n* times. Units are files and
invocations today, and later can be single cells (see Later).

A new `Unwind::Pending(node)` carries the stop. Nothing catches it but the
unit's root: not `recover`, not `#[retry]`, not a loop or a function
boundary. It takes every cleanup path an error takes (memo running-call
records, `with` frames, deadlines, a `lazy`'s running flag), which tests
cover. A `#[timeout]` running in a unit measures an attempt, not time spent
waiting on nodes.

**Restart terminates only if every key is reproducible across attempts.**
This is an invariant, guarded: a unit whose attempts outnumber the nodes it
has made, plus one, is the internal error "evaluation did not converge",
never a loop. Key equality is reflexive: floats compare by their bits, with
`-0.0` as `0.0`, so a `NaN` argument still finds its node. Nothing in a key
reads a `with` frame's identity, which is new each time a `with` runs.

### D3. Nodes and their identity

A *node* is one call instance the host must deal with. There are two kinds:

- **Deferred call:** a call of a closure that carries a deferring
  attribute, its arguments, and the context it was made in.
- **Effect:** a call of an effectful built-in made by graph-mode code
  (D4): `exec` and its kin, and reads as well as writes, `fs.*`,
  `File.read`, `Dir.files`, `Dir.sandbox`, `Sandbox.exec`, `Sandbox.file`
  and the like. A read is an effect too: the host must see what
  non-hermetic code reads, and a restart must not read again.

A node's arguments may hold futures the host has not resolved yet: the
*holes* of the value. Holes are found by a deep walk through records,
lists, variants, secrets, futures' captures and closures' captures. They
are the node's **data edges**. Each node also records the node made just
before it in its unit, in program order: its **order edge**. GCSL reports
both. The host decides which to honour, and the core model's rule (keep
order edges for nodes that are not isolated) is pekit's.

Identity must survive restarts. Closures made during an attempt are new
values each time, and memoised calls may hit on one attempt and miss on
another. So:

- A **deferred call**'s key is structural: its function's code (IR and
  function number) and captures, its arguments, and its context. Closures
  compare by code and captures; GCSL's own futures by code and captures
  while unrun, and by value once run; holes by the node they stand for. Two
  calls with equal keys in one evaluation are one node. A deferred call is
  meant to depend only on its key, and the host caches by it anyway. Only
  calls the host accepts as nodes dedupe: one it runs inline
  (`hermetic(false)`) runs each time, and the host must answer the same for
  a key throughout an evaluation.
- An **effect**'s key is its built-in, arguments and context, plus its
  occurrence: the *k*th effect of that key within its *scope*. A scope is
  the unit, or a memoised call inside it, by that call's key. A memo hit
  then skips a whole scope without shifting the count of any other, and two
  identical `exec`s in a row stay two nodes.

The context is the flattened current context: every key bound by a `with`
frame, including the host's root frame (10c), with its value. This keys a
node on context its function never reads, which is correct but
conservative. Narrowing it to what the function can read is a later
refinement that changes no rule.

### D4. Graph-mode units perform no effects

In graph mode, every effectful built-in called by unit code becomes an
effect node. A unit therefore pauses at each effect, which runs only when
the host asks GCSL to run it (D5). That is what makes restarting safe, and
it is how non-hermetic code keeps plain program order: each effect waits
for the host, and its order edge says what came before it.

A deferred call the host chooses to run inline is evaluated in the unit,
in graph mode, so its effects become nodes too.

### D5. Running a node

The host either supplies a node's value (a cache hit), or asks GCSL to
evaluate it under an *effect context*:

- **A deferred call:** GCSL calls the closure with its arguments, holes
  filled, in its recorded context, *sequentially*. This is plain GCSL
  evaluation, not graph mode: nested deferred calls run inline, `#[retry]`
  and `#[timeout]` apply, and effects go through the effect context.
- **An effect:** GCSL runs the built-in with its arguments through the
  effect context.

The **effect context** is a host trait, `Effects`, through which every
effectful built-in runs: running a program, the file-system operations,
temporary directories and sandboxes. Plain GCSL's implementation, `Direct`,
is today's behaviour. Pekit's routes `exec` into a node's bubblewrap
sandbox and roots `fs` there. GCSL never needs to know what a sandbox is.

A node fails with a diagnostic. An await of a failed node raises that
failure where the await is, so `recover` around it catches it. A `#[retry]`
around the await re-raises the same failure: it never re-runs the node.
Retrying a step is the business of `#[retry]` on the deferred function
itself, which applies while the node is evaluated.

### D6. Plain values, files and code identity

The host needs to stage inputs, compute keys and store results:

- **Plain values:** a `Send` form of a value: scalars, quantities,
  strings, lists, records by field name, variants by key, files and
  directories by path, and secrets with their identity, which is all a
  hermetic step can return. `Value` converts to it, and back through the
  workspace's layouts and tags. A node result holding a function or an unrun
  future is the runtime error "a step's result must be plain data" at the
  node, not a check-time rule.
- **Files of a value:** every `File` and `Dir` a node's arguments hold, for
  staging and content hashing.
- **Code identity:** a stable hash of what a node's function can run. The
  first version is the hash of the source of every file its file needs,
  which is coarse but correct. Editing a library then reruns every node
  that uses it, which is what the per-`exec` cache (layer F) fixes.

### D7. The host's root values (10c)

`invoke_with` already takes a root context frame at run time. 10c adds the
check-time half: the host names the contexts it provides, and the checker
counts them as bound at the top level *and* in definitions (see
`gcsl-language.md`, 10c). A root value is checked against its context's
type.

## Demand

Calling creates a step; needing it runs it. The host runs only *needed*
nodes, never every ready one, so `let compiler = build toolchain()` at a
file's top level costs nothing unless something needs `compiler`:

- A unit *demands* the node it paused on.
- A unit that finishes demands the holes left in its value. It is *done*
  only once its value has no holes; until then the host sees the holes it
  waits on.
- `needed(unit)` is the transitive closure of a unit's demands over data
  edges, and the host may run a needed node once its own holes are filled.

## The embedding API (sketch)

```rust
// Graph mode for a workspace, with the deferring attributes' semantics left
// to the host.
let mut graph = workspace.graph();
let unit = graph.invoke(definition, args, root_contexts)?;   // or a file
loop {
    match graph.advance()? {          // runs every unit that can progress
        Advance::Done(unit, value) => ...,
        Advance::Waiting => {
            for node in graph.ready() {           // needed nodes whose holes are filled
                match host_cache.get(graph.key(node)) {
                    Some(plain) => graph.resolve(node, Ok(graph.value_from_plain(&plain)?)),
                    None => {
                        let outcome = graph.evaluate(node, &effects);
                        graph.resolve(node, outcome);
                    }
                }
            }
        }
    }
}
```

Every call is on one thread. Graph state lives beside the workspace's frame
and memo tables. The host may evaluate nodes in any order its edges allow;
parallel execution of the processes behind them is the host's (its
`Effects` can start a process and return while it runs, in a later step).

## Steps

Each step leaves plain GCSL unchanged and fully tested.

1. **B1, deferring attributes.** `#[defers]` on an attribute declaration;
   either-shaped results for calls of functions that carry one; pure
   functions refused. Docs and tests.
2. **B2, the `Effects` trait.** Every effectful built-in goes through it;
   `Direct` is today's behaviour. A refactor with no behaviour change.
3. **B3, graph-mode units.** Host futures, `Unwind::Pending`, the node
   table with structural and scoped identities, data and order edges,
   restart, and failure at awaits.
4. **B4, running nodes.** `evaluate` under an effect context, `resolve`
   with a value or a failure.
5. **B5, plain values, files and code identity.**
6. **B6, the host's root values (10c).**
7. **B7, documentation:** the embedding guide in Learn and the `#[defers]`
   reference.

## Later (not ruled out by anything above)

- Units of single cells, demand-driven: evaluate only the cells a target
  needs, so that awaiting an unneeded result at a file's top level runs
  nothing, and a pause restarts one cell, not a file.
- Narrowing a node's context to what its function can read.
- A finer code identity per function, and the per-`exec` cache.
- Asynchronous `Effects`, so that one GCSL thread drives many running
  processes.

## Decisions taken without Jack (to surface)

- Restart rather than suspension (D2). Reversed on 2026-10-27; see
  `suspension.md`.
- `#[defers]` as the marker, with typing that depends only on the
  declaration (D1).
- Effect nodes for every effect of graph-mode code (D4), so a unit never
  performs an effect itself.
- A node's context keys on the whole bound context (D3).
- Code identity by needed source files at first (D6).

## As built (2026-10-24)

GCSL `b6c4d3d` (B1), `f880d38` (B2), `83c29af` (B3 and B4), `03bbef1`
(B5), `53c3183` (B6) and `63ec533` (B7); fixes found along the way in
`b9e197a` (a malformed attribute commented out its line) and `35a2866`
(a method on a receiver whose elements may be futures).

- **D1.** As specified. A deferring function's result keeps the either
  shape because `check_fn` skips the narrowing it gives other impure
  functions; a present result type, or a pure function, is an error.
- **D2.** The stop is an error marked `Mark::PENDING`, not a new
  `Unwind` variant: every catcher already ignores marked errors (`recover`,
  `#[retry]`, secret masking), and every cleanup path is an error's. The
  node waited on is the run's, not the diagnostic's.
- **D3.** Effects are found by *position*, not by key and occurrence: a
  scope's path and the number of nodes met in it, where a memoised call's
  body and a `lazy`'s are scopes keyed by their code, captures and
  arguments. Every node met is recorded at its position with its key, and
  another key there is "evaluation did not converge". This replaced the
  attempt-count guard, which a key that changed every attempt would not
  have tripped.
- **D4.** `file()` and `dir()` are not effect nodes: they only read the
  working directory, fixed for an evaluation. Every other impure built-in
  is one.
- **Discovery.** An impure function's bare parameters take futures, so in
  `link([compile("a"), compile("b")])` one attempt finds all three steps:
  the link's data edges are the compiles, and nothing stops.
- **D5.** `Workspace::evaluate` runs a node with plain GCSL's evaluator
  through the host's `Effects`, in the node's recorded context and caller;
  a deferred node's callee is called with its arguments in its parameter
  slots, `#[retry]` and `#[timeout]` applying.
- **D6.** Code identity is material, not a hash: the file numbers whose
  sources make a step's code (`StepInputs::code_files`), for the host to
  hash, since a stable cryptographic hash is the host's business.
  `StepInputs` also gives the callee's captures, which its code reads.
- **D7.** A prepared call's checker counts the contexts its root frame
  binds as met at its top level. Definitions keep the rule that they need
  no context; contexts a host provides should have defaults, so that plain
  GCSL runs the same libraries.
- **Units.** Files, and prepared calls (`prepare_call`, `run_prepared`),
  which are lowered once and numbered after every file, so that every
  attempt runs the same code.

Still to do, not ruled out: units of single cells, demand-driven; a
node's context narrowed to what it reads; a finer code identity; and
asynchronous `Effects`.
