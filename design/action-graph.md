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

Rejected: making every impure call's result either-shaped. That changes
typing everywhere for every program. Also rejected: detecting the use of a
stand-in in every typed walker at run time. That would touch every place
that takes a value apart, and a missed place would give a wrong
internal error instead of a pause.

### D2. Resuming: restart, as Bazel's Skyframe does

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
boundary. A `#[timeout]` running in a unit measures an attempt, not time
spent waiting on nodes.

### D3. Nodes and their identity

A *node* is one call instance the host must deal with. There are two kinds:

- **Deferred call:** a call of a closure that carries a deferring
  attribute, its arguments, and the context it was made in.
- **Effect:** a call of an effectful built-in (`exec` and its kin, `fs.*`,
  `file`, `dir`, `Dir.sandbox`, `Sandbox.exec` and so on) made by
  graph-mode code (D4).

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
  function number) and captures, its arguments, and its context, with
  closures compared by code and captures, and holes by the node they stand
  for. Two calls with equal keys in one evaluation are one node. A deferred
  call is meant to depend only on its key, and the host caches by it anyway.
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
failure where the await is, so `recover` around it catches it and an
enclosing `#[retry]` retries it, as with any failure there.

### D6. Plain values, files and code identity

The host needs to stage inputs, compute keys and store results:

- **Plain values:** a `Send` form of a value: scalars, quantities,
  strings, lists, records by field name, variants by key, files and
  directories by path, and secrets with their identity, which is all a
  hermetic step can return. `Value` converts to it, failing for functions
  and futures, and back through the workspace's layouts and tags.
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
            for node in graph.ready() {           // nodes whose holes are filled
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

- Restart rather than suspension (D2).
- `#[defers]` as the marker, with typing that depends only on the
  declaration (D1).
- Effect nodes for every effect of graph-mode code (D4), so a unit never
  performs an effect itself.
- A node's context keys on the whole bound context (D3).
- Code identity by needed source files at first (D6).
