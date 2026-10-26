# pekit2 core model

This is a working note for designing pekit as a general build system, with
Bazel as the north star. It records the concepts, what is agreed, what is only
proposed, and what is still open. The decision history lives on Acta task
PEI-1594; this note is the current picture.

## Glossary

**Build graph.** A map of every piece of work in a build and what each piece
needs first. Pekit reads the map, runs everything whose needs are met, and
keeps going until nothing is left. Two pieces that don't need each other can
run at the same time.

**Step.** One node in the build graph: a piece of work pekit runs as a unit,
caches as a unit and can run in parallel with other steps. In pekit2, a step is
a call to a hermetic function (see below).

**Input** and **output.** Everything a step reads, and everything it produces.
An input can be a file, a directory or a plain value such as a string or a
number.

**Content hash.** A short fingerprint of some data, such as a SHA-256 digest.
If the data changes even slightly, the fingerprint changes. Two files with the
same fingerprint are treated as identical.

**Cache key.** A fingerprint of everything that could affect a step's result:
the step's code and the content hashes of all its inputs. If the key matches a
previous run, pekit reuses that run's output instead of running the step again.

**Hermetic.** A step depends only on its declared inputs, and nothing else:
not environment variables, not files it wasn't given, not the network, not the
time. Hermeticity is what makes caching *correct*. A non-hermetic step can have
a matching cache key while its real inputs have changed, and pekit then serves
a wrong result without noticing.

**Independent.** A step doesn't change anything that another step uses. This is
what makes it safe to run steps in *parallel*.

**Deterministic.** The same inputs always give byte-for-byte the same output.
This is what makes builds *reproducible*. It is not needed for correct caching:
a compiler that embeds a timestamp is not deterministic, but it can still be
cached correctly if it is hermetic.

**Sandbox.** An isolated environment that a step runs in, containing only the
step's declared inputs. The sandbox is how pekit *enforces* hermeticity and
independence, rather than trusting a promise.

**Evaluation** and **execution.** Evaluation is running the GCSL build files to
find out what work exists. Execution is doing that work. In pekit2 the two
interleave: GCSL evaluates as far as it can, pekit executes the steps that are
ready, and evaluation resumes where it was waiting on a result.

**Dynamic dependency.** A dependency that is only known once the build is under
way, such as a list of files a tool only reports after it has run. Bazel
handles these poorly. The pekit2 model handles them naturally, because
evaluation can wait on a step's result.

**Granularity.** How big a step is. It can be coarse, such as "run busybox's
whole `make`", or fine, such as "compile this one `.c` file". Pekit2 supports
both in one graph. Fine steps mean that editing one file rebuilds only what
that file affects.

## The model

1. **The build language stays GCSL.** Build files read as much like a script as
   possible.
2. **Pure GCSL code runs immediately.** This is code with no effects:
   arithmetic, strings, records, choosing between options. GCSL's effect typing
   already tells pekit which code is pure.
3. **Hermetic functions are the steps.** A function marked `#[hermetic]` (working
   name; see open question 2) is one step. Inside it, the code reads like
   a script: lines run in order, and `sh` can be used freely.
   - Pekit runs the function in a sandbox that contains only the function's
     arguments. It caches the result under a key made from the function's code
     and the contents of its arguments.
   - Calling a hermetic function gives back a *future*: a stand-in for a result
     that doesn't exist yet. GCSL code can pass a future along freely. If code
     needs to look inside it, that code waits until pekit has run the step.
4. **Hermetic calls that don't need each other run in parallel.** Pekit sees every
   pending hermetic call and runs all the ones whose inputs are ready.

   *Calling creates a step; needing it runs it.* A hermetic call, or a
   `fetch`, only adds a node to the graph. Pekit runs the node only if a
   requested target's result depends on it. So
   `let compiler = build toolchain()` at the top level of a file costs
   nothing unless a requested target uses `compiler`. Undeclared effects
   inside a body still follow plain GCSL rules (point 5): they run whenever
   the code containing them is evaluated.

   *Dependencies are on values, not targets.* Each field of a target's result
   becomes ready on its own. In example 2, `main`'s compile needs only
   `g.headers`, which are workspace files and ready at once. It therefore
   runs alongside `greet`'s compile and archive, and only the link waits for
   `g.archive`. Bazel gets the same effect from its explicitly declared
   providers. Here it falls out of the language.
5. **Undeclared effects keep plain GCSL behaviour.** Effectful code that is not
   hermetic runs one thing at a time in program order, uncached, exactly as GCSL
   runs it today. It is always safe, just slow:
   - A badly written build file doesn't break; it just doesn't parallelise or
     cache.
   - Pekit reports how much of a build ran uncached. A strict or release mode
     may refuse undeclared effects.
6. **The few unavoidable non-hermetic operations are special built-ins.**
   Fetching from the network is the main one, and it is pinned by content hash
   so that the result is still fixed.
7. **Libraries make recipes short.** A library of hermetic functions such as
   `make(...)` or `cc(...)` keeps recipes short and is how pekit learns new
   languages. Bazel calls these *rules*. Libraries are plain GCSL, so they are
   not tied to pekit.

## The guarantee

A hermetic step's sandbox contains only its arguments, and its receipt
lists exactly its arguments. A step therefore cannot read anything that is
not on its receipt: if it tries, the thing is not there and the step fails.
Correct caching is guaranteed by construction, not by authors writing build
files correctly.

So an author's mistake costs either a loud error or lost caching, never a
wrong cached result:
- reading something undeclared: the step fails, because the sandbox does
  not contain it;
- leaving work unmarked: it runs uncached every time, which is correct and
  reported.

Every new feature must keep this property. Pekit should also explain these
failures. For example, when code inside a hermetic step tries to start a
build, pekit says so and shows the parameter-default form.

## Status

### Agreed (Jack, 2026-10-23)

- Bazel is the north star. It decides what the architecture must allow, not
  what gets built now.
- GCSL stays the build language, and build files read like scripts.
- GCSL evaluates whatever it can at once. Work that needs the result of an
  effect is handed to pekit as a graph, and pekit parallelises and caches it.
  This was "option A".
- Parallelism is opt-in: only explicitly declared steps are parallelised.
  Undeclared effects run sequentially and are always safe.
- Design proceeds through a planned sequence of examples (see below).

### Proposed, not yet agreed

- The declaration is a single marker, `hermetic`, and the sandbox enforces it.
  Separate *independent*, *hermetic* and *deterministic* markers are not
  needed:
  - Running in a sandbox gives a step both independence and hermeticity.
  - Determinism is checked separately, for example with
    `pekit build --check-reproducible`, which builds twice and compares.

### Open questions

1. **Files versus strings.** Pekit must know which arguments are files or
   directories to put in the sandbox, and which are just values.

   *Direction (Jack, 2026-10-23):* `file(path)` is an ordinary GCSL function
   returning a `File` record, used method-style (`f.read()`, `f.exec(...)`).
   Pekit uses `File` values at the hermetic boundary:
   - a `File` among a hermetic function's arguments is moved into its sandbox;
   - a `File` in its result is taken back out into pekit's store.

   GCSL today has no `File` type or methods; `fs.*` takes path strings.

   Constraints Claude raised:
   - Methods must be resolved from the value's type, not stored as closures in
     the record. Hermetic arguments must be plain data, and GCSL closures can
     be neither copied nor compared.
   - Pekit finds `File`s by walking arguments and results deeply, through
     lists and records.
   - In pekit a `File` needs a content identity (performance point 5), not just
     a path.
   - A path passed as a plain `String` is not moved, so it fails loudly in the
     sandbox.
2. **How the marker reaches GCSL.** The working name is `hermetic`. GCSL does
   not know what it means (Jack, 2026-10-23):
   - GCSL gains attributes that a host declares, for example in
     `pekit-include.gcsl`, so that a misspelt attribute is still an error.
     Today GCSL has a closed set of three attributes with no arguments.
   - GCSL passes the attribute through to the action graph, where pekit reads
     it.
   - A host-declared attribute such as `#[hermetic]` must be allowed on
     private `fn`s as well as public `def`s, unlike `#[def_type]`, which is
     limited to top-level `def`s. A one-off hermetic helper in a build file
     should be private (example 3).

   *Attributes versus metadata (Jack, example 7).* Anything that tells the
   scheduler how to run a step is an attribute, which pekit reads from the
   action graph, for example `#[hermetic]`, `#[retry(3)]` and
   `#[timeout(30s)]`. The scheduler never reads `defmeta`. Metadata is data
   that GCSL code reads, such as a build's source or a package's files.
   Consequences:
   - Host-declared attributes take typed arguments, declared with the
     attribute.
   - Attributes on a def-type are defaults for its definitions, and a
     definition's own attributes override them. The `test` def-type is
     `#[hermetic]`, and one test opts out with `#[hermetic(false)]`.
   - Retry and timeout never go on a receipt.
   - Where an attribute is declared (proposal, Claude): `#[retry]` and
     `#[timeout]` mean the same under any host, so they are core GCSL, and
     plain GCSL honours them too. In plain GCSL, retrying an impure
     function repeats its effects, so `#[retry]` there is the author's
     statement that rerunning is safe. Under pekit, a hermetic step's
     sandbox makes retrying always safe. `#[hermetic]` has no meaning
     outside pekit, but libraries that use it must still check under plain
     GCSL. So its declaration lives in a small importable module that
     plain GCSL accepts and ignores, not only in `pekit-include.gcsl`.
   - Failure (agreed, example 7) is any panic from a step's own code,
     including a non-zero `exec` exit or a failed `assert`, or exceeding
     its timeout. Infrastructure errors, such as a lost worker, a sandbox
     that cannot be set up or a full disk, are not the step's failure:
     pekit always retries them, invisibly, without counting them against
     `#[retry]`. For a hermetic step, a panic should be deterministic, so
     `#[retry(n)]` declares a step known to be flaky, whether a build or a
     test. Pekit reports a step that passed only on a retry as flaky.
   - Only successful steps are cached (agreed, example 7), as in Bazel.
     A failed test or build reruns next time.
   - A test's `recover` sits outside the hermetic step. It catches the
     final panic, after any retries, where the test def-type awaits the
     step's result. Catching it inside the step would hide the failure
     from the scheduler, and `#[retry]` would never fire.

   Claude first argued that GCSL needed a generic `host_call` category. Under
   the engine design below it does not: every impure call is already an
   action-graph node that the host schedules. The rule that a hermetic
   function's arguments and result must be plain data can be checked by pekit.
3. **Where outputs live.** A hermetic function's outputs are the `File`s in its
   result, which pekit captures into its store. Requested targets are made
   available under a `pekit-out/` directory (Jack, 2026-10-23).

   Workspace files keep their workspace paths in every sandbox. A step's
   outputs get a path that names the target and step that produced them, such
   as `pekit-out/main/run_embed/message.h` (agreed, example 3). Two steps
   can therefore both produce a `message.h` and feed one compile without
   colliding, and `gcslib/gcc` derives `-I` from these paths as it does for
   workspace headers. This is the same idea as Bazel's `bazel-out/`.

   *Changed 2026-10-26 (Jack, PEI-1596):* a step's outputs are named by what
   the step is, `pekit-out/.steps/<function>/<identity>/`, not by the target
   that met it, so that a step two targets reach is one step; each
   target's results are linked under its own name (`design/engine.md`, E5).

   A `FileSet` is simply a list of `File`s, so that `files(...)` and
   `[f1, f2]` are the same kind of value (agreed, example 3).
4. **Toolchains.** Jack does not want pekit to be specific to ppk. Claude's
   proposal splits this into two problems:
   - *Getting the toolchain's files.* A toolchain is just a `FileSet`, and pekit
     core has no toolchain concept. The files arrive like any other input:
     fetched as an archive pinned by hash, built from source in the graph, or
     returned by a library such as a ppk library that sits outside pekit core.
     Pinned archives and built toolchains already have a fingerprint, so pekit
     never hashes thousands of files on each build. A snapshot of the host's
     compiler stays available as an explicit, non-hermetic escape hatch.
   - *Choosing which toolchain a target uses*, per architecture and
     configuration. This is example 6.

   *The constraint (Jack, example 6):* GCSL libraries must work equally in a
   pekit build and in a plain GCSL script run with `gcsl build.gcsl`. A plain
   script gets no parallelism and no pekit command line, but it still needs a
   toolchain. Pekit must change how libraries are written as little as
   possible. Where it does change something, the change must be transparent
   in plain GCSL, as `#[hermetic]` is ignored outside pekit. So toolchain
   resolution must satisfy all of these:
   - a library asks for a tool, such as a C compiler, in the same way under
     either host, and never checks which host it runs under;
   - in plain GCSL, the answer comes from the machine, as
     `toolchain_from_PATH` does;
   - in pekit, the answer comes from the toolchains registered in the
     workspace and the current configuration;
   - in pekit, the chosen toolchain is an input to every step that uses it;
   - switching configuration, as in "build this for the build machine", is
     expressed without reference to pekit. Under plain GCSL it may only
     support building for the current machine, but it must not break.

   *Proposal (Jack, example 6): context.* GCSL sets a few global
   variables, such as `arch`, which defaults to the running machine's
   architecture. A new keyword, `with`, overrides them for one call and
   everything that call does: `build embed() with { arch = "x86_64" }`.
   The globals a function reads are its *context*. If a call site cannot
   supply all of a function's context, that is an error. `with` is for
   values that must reach deep without being passed, not a general
   alternative to passing arguments.

   Claude's response: this is better than either earlier option. It gives
   explicit passing's checking without its threading, and it gives pekit
   exact receipts, because a hermetic step's identity includes exactly the
   context values it reads. Points Claude raised:
   - Context values are read-only and are changed only by `with`, so
     parallel evaluation sees no mutation.
   - A function's context is part of its type, as its effect is, so a
     function passed as a value carries its requirements with it.
   - Context is captured when work is deferred, in a future or an
     action-graph node, and read at call time otherwise. GCSL already does
     this for `fileroot.caller`.
   - There should be two globals, `arch` (what is being built for) and
     `host_arch` (where steps run). Under pekit, `host_arch` is the
     execution platform, which may be a remote worker, not the machine
     running pekit.
   - Context variables are declared explicitly, with or without a default.
     One without a default must come from an enclosing `with`.
   - Precise receipts need the toolchain lookup itself to read the context,
     as `toolchain.get("cc")` would. A library that runs `cc` by name still
     works, with pekit falling back to coarse receipts.

   *Agreed design (Jack and Claude, 2026-10-23): context.* This refines the
   proposal above:

   | Syntax | Meaning |
   |---|---|
   | `context arch: String = <default>` | declares a context variable and its default; a name is declared once |
   | `^arch` in an expression | reads the current value |
   | `fn f(^arch: String)` | sugar for `fn f(arch: String = ^arch)`; the default is evaluated at the call site, so it reads the caller's context, and it can still be passed explicitly |
   | `call() with { arch = "x86_64" }`, `with { ... } { ... }` | sets values for everything evaluated inside |

   - Resolution is dynamic. Functions in between neither declare nor pass
     the value, and the checker infers their requirements.
   - Context names live in their own namespace, so an ordinary `let arch`
     can never satisfy `^arch`. Jack considered lexical resolution
     (`^param` taking a value from the call site's scope); it was dropped
     because a forgotten declaration in an intermediate function silently
     picks up an unrelated outer value.
   - The root of every evaluation has an implicit `with`. Its values come
     from the declared defaults (GCSL built-ins such as `arch` and
     `host_arch`, then workspace and library declarations). Under pekit,
     they come from pekit: `arch` from the command line or target,
     `host_arch` from where steps execute.
   - A missing value with no default is an error, never a silent fallback.

   *Toolchain resolution (agreed, example 6, 2026-10-23).* The full draft
   is in `examples/6/build.gcsl`, under "Consolidated".
   - The platforms are context records. `^host_platform` is where steps
     run and `^platform` is what is being built for; it defaults to
     `host_platform`.
   - Toolchain kinds are variants of an extensible enum,
     `enum Toolchain extensible<{ *: File | Dir }>`. A new kind is one
     line, such as `enum Toolchain::C<{ root: Dir, cc: File, ... }>`. The
     bound keeps every toolchain plain data that can cross the hermetic
     boundary. A kind that needs its install in the sandbox includes a
     `root: Dir`.
   - Toolchains are definitions in the def-type style (Jack):
     `def Toolchain::C gcc_x86_64() { host_platform, platform, provides }`.
     One generic def-type serves every kind, checks `provides` against the
     kind's layout, and wraps it in its variant, so the registry is one
     list of a single type.
   - `context toolchains` is that list, in order, and the first match
     wins. Plain GCSL defaults to `system_c`, built from the host's `PATH`.
     Pekit supplies the workspace's registered toolchains, as it does
     packagers.
   - `toolchain.get(Toolchain::C)` is pure. The variant is the typed key,
     a toolchain matches on partial platform records, and it returns just
     the kind's layout, `T`.
   - Libraries resolve a toolchain in a parameter default
     (`cchain = toolchain.get(Toolchain::C)`). Resolution therefore
     happens at the call site, the toolchain is an ordinary argument, and
     receipts are precise with no special mechanism.
   - Pekit does not fill `PATH` as a fallback (Jack). A library that does
     not use `toolchain.get` is not supported under pekit; fork it. Such a
     library fails loudly and never builds wrongly.
   - Pekit wraps every target it evaluates in an implicit
     `with { host_platform = <execution platform> }` (Jack). That allows the
     execution platform to be chosen per target, for example in a mixed
     x86 and ARM remote pool, as Bazel chooses one per target.
   - The choice is made before evaluation, because toolchains resolve in
     parameter defaults while the target is evaluated. If a toolchain
     cannot be resolved on one platform, pekit tries the next.
   - Cache-aware choice (Jack). Pekit prefers the execution platform whose
     evaluation hits the cache most. Evaluation is pure and cheap, so pekit
     can evaluate under each candidate and compare cache hits. Bazel only
     takes the first match. Claude's caveat: different execution platforms
     resolve different toolchains, such as a cross compiler versus a native
     one, so the bytes produced can depend on what happened to be cached.
     Both builds are correct, but the build is not reproducible from its
     inputs alone. Proposal: cache-aware choice by default, and a fixed
     preference order in release and strict modes.

   Claude's observation: this is the pattern already used for `File`,
   `#[hermetic]` and `Dir.sandbox`. *GCSL defines the question; the host
   answers it.* Toolchain resolution needs a GCSL-level way to ask the
   environment for a tool, with a plain-GCSL default answer and a pekit
   answer.
5. **Step identity.** A step's cache key needs a stable identity for its code.
   GCSL compares closures by identity, so this must come from the definition's
   name and source, not from the function value.
6. **Re-evaluation cost.** Each build re-runs the pure GCSL from scratch and
   skips only cached steps. That is fine for a long time; at very large scale,
   evaluation itself may need caching.

## Engine design (Jack's proposal, 2026-10-23, under discussion)

There are two graphs and a clear split of responsibilities.

**GCSL produces the action graph.** Pekit calls GCSL through the embedding API
in an action-graph mode:

1. GCSL evaluates everything pure whose inputs are known, folding it into
   values.
2. What remains is the action graph. Its nodes are *call instances*: one node
   for each time an impure function is actually called, with its arguments.
   A function called 100 times in a loop gives 100 nodes. Nodes also cover the
   pure code waiting on those calls' results. Each node carries the
   attributes of its function, such as `hermetic`.
3. There are two kinds of edge:
   - *data edges*: this node needs that node's result;
   - *order edges*: plain GCSL would run this effect after that one.

**Pekit derives the build graph and schedules it.** The build graph is the
action graph seen through pekit's rules:

- For a hermetic node, pekit keeps the data edges and drops the order edges,
  because the sandbox isolates the node.
- For a node that is not hermetic, pekit keeps both kinds of edge, so it runs in
  plain program order.

Pekit then repeatedly takes every node whose remaining edges are satisfied and
does one of two things:

- **Cache hit:** pekit gives GCSL the node's value from the cache.
- **Otherwise:** pekit tells GCSL to evaluate the node in a given *context*.

**GCSL evaluates a node sequentially.** Inside a node GCSL runs exactly as it
does today, so a hermetic body reads like a script. The context is how GCSL
performs that node's effects:

- `exec` runs inside the node's sandbox, with the given environment and working
  directory;
- `fs.*` is rooted in the sandbox.

GCSL never needs to know what a sandbox is, and pekit never needs to know how to
run GCSL code.

**GCSL trusts the host.** GCSL reports the order plain GCSL would follow. The
host may relax it only for nodes it isolates.

**The graph grows.** When a node completes, GCSL evaluates whatever pure code
now has its inputs. That can add new nodes, for example when an `if` or a loop
depended on the result. This is how dynamic dependencies (example 9) work.

### Points raised by Claude

- **Not the AST.** The host should get an intermediate graph of call
  instances, not the syntax tree. The syntax tree has a function once, however
  many times it is called, and caching and scheduling work on individual calls.
- **Pausing mid-function is still the main GCSL work.** Take an impure function
  that calls two hermetic functions and then uses their results. To run those
  two calls in parallel, GCSL must stop that function where the results are
  first *used*, not where the calls are made, and keep the rest of the function
  as a pending node. In Jack's framing that pending remainder is just a
  graph node, but GCSL's evaluator must be able to represent it.
- **Parallel evaluation.** GCSL's evaluator is single-threaded (`Value` uses
  `Rc`). Hermetic bodies spend almost all their time waiting on `exec`, so a
  single GCSL thread can interleave many nodes, or pekit can run several
  evaluators. Because node arguments are plain data, they can be copied
  between evaluators and eventually machines.
- **Nested hermetic calls.** When a hermetic body calls another hermetic
  function, does the inner call run inline in the same sandbox (simplest), or
  as a node of its own?
- **One graph or two.** Whether pekit keeps the build graph as its own copy or
  as a view over GCSL's graph is an implementation choice. A copy helps with
  planning and with `pekit query`; a view cannot drift out of sync.

### Performance review (Claude, 2026-10-23)

The model has no fundamental performance ceiling compared with Bazel,
provided the following hold. Each is cheap to design in now and expensive to
add later.

1. **Demand-driven evaluation.** Pekit evaluates only what the requested
   targets need, working backwards from them. It does not walk the whole
   workspace and fold everything pure. Eager whole-tree evaluation would make
   every build cost the size of the repository.
2. **Keys from input content.** A node's cache key uses the content hashes of
   its inputs, not the keys of the steps that produced them. If a rebuilt
   input comes out byte-identical, nothing downstream reruns. Bazel calls this
   *early cutoff*. Nix's input-addressed model lacks it.
3. **A second cache level, for each `exec`.** A hermetic node's key includes
   its GCSL code, so editing a library would otherwise invalidate every node
   that uses it. Bazel keys an action by its concrete command line, so a rule
   change that yields the same commands rebuilds nothing. Caching each `exec`
   inside a node by its command, environment and input content gives pekit the
   same behaviour.
4. **Evaluation in identifiable units.** Pure evaluation must happen in units
   with stable identities and recorded inputs, such as definition invocations
   and files. They can then be cached across runs, or kept warm in a daemon.
   This is how Bazel's Skyframe makes a no-op build of a huge repository take
   moments. It is not needed early, but it must not be ruled out.
5. **File values are content handles, not paths.** Outputs are referred to by
   content hash and only fetched or written to disk when something needs the
   bytes. This keeps remote execution from downloading every intermediate
   output. Bazel calls this *build without the bytes*.

Implementation limits rather than design limits:

- GCSL's evaluator is single-threaded (`Rc`). Bazel's analysis is heavily
  parallel. In a large repository, single-threaded evaluation becomes the
  bottleneck. Point 4 gives units that separate evaluators could share out.
- Paused functions are in-memory GCSL state, so they cannot be saved to disk
  or sent to another machine. This is acceptable: point 4 caches work at unit
  boundaries, and a daemon keeps live state.

Points that are hard to reason about:

- **Silent serialisation.** Code that inspects a result early forces a wait.
  Logging it, or checking a field, is enough. It quietly removes parallelism,
  and users will not know why their build is slow. Pekit needs tools that
  show the critical path and why each step waited.
- **Ordering of output.** If order is relaxed for hermetic nodes, a
  `print("done")` written after a hermetic call can run before that call
  finishes. That is surprising in code that reads like a script. A decision is
  needed on whether effects that are not hermetic wait for earlier hermetic
  calls in program order.
- **Dynamic dependencies versus queries.** Bazel's analysis cannot see build
  outputs, so it can answer "what depends on X" without building anything.
  Large-repository CI depends on that to choose which tests to run. Wherever
  a pekit build branches on a result, nothing beyond that point is known
  until it has run. Pekit should make such points visible, and a strict mode
  could restrict them.

### Observability (proposed, 2026-10-23)

Jack suggested a small web app that renders the build graph and shows the
build running.

- The engine emits a structured event stream, for example: node discovered,
  ready, started, cache hit, finished, failed, and waited on another node.
- The web app is one consumer of that stream. Others are the terminal
  progress display, CI logs, and saved profiles.
- Bazel's equivalent is the Build Event Protocol, which tools such as
  BuildBuddy consume.

The stream is architecture and is cheap to design in now; the web app is a
later feature. An early, nearly free step is exporting Chrome trace format, so
that Perfetto can show a timeline before any custom user interface exists.

Large graphs cannot usefully be drawn whole. The useful views are:

- a timeline of steps per worker;
- the critical path;
- the neighbourhood of one target;
- "why did this step wait".

The last is the tool that the silent-serialisation problem above needs.

## What GCSL needs

Facts from the survey are on PEI-1594. The changes the model implies:

- **An action-graph evaluation mode.** GCSL folds what is pure, exposes call
  instances with data and order edges, and accepts "here is this node's value"
  or "evaluate this node in this context". This includes pausing a function
  where a result is first used, and resuming it later.
- **Host-declared, pass-through attributes,** such as `hermetic`
  (open question 2).
- **Per-node effect contexts.** These say how `exec` and `fs.*` run for a node.
- **Effects with explicit environment and working directory.** Today `exec`
  inherits pekit's process environment, which is why pekit sets
  `SOURCE_DATE_EPOCH` globally.
- **A `File` type with type-resolved methods** (open question 1).
- **Directories you can run commands in** (example 4).
  - `fs.compose_dir(map)` is an ordinary function that builds a directory from
    a map of paths to `FileSet`s.
  - `Dir.exec(...)` runs a command with that directory as its working
    directory. This is plain behaviour with no isolation.
  - `Dir.sandbox(...)` returns a `Sandbox`: one place with state, whose `/` is
    that directory. Commands run in it with `sandbox.exec(...)`. This is how a
    step gets `/bin/sh`, `make` and the like at the paths upstream build
    systems expect. (Agreed, 2026-10-23.)
    - A sandbox is a value, not a verb on `Dir`. It takes default `cwd` and
      `env`, so the commands in it don't repeat them. A library can be handed
      a sandbox without knowing how it was set up.
    - `exec` that silently swapped the whole root would be unpredictable
      (Jack).
  - A sandbox is a live, copy-on-write layer over its directory. Its commands
    change it, and later lines read results out of it with `.dir(...)` or
    `.file(...)`, as a script would. One directory can back several
    independent sandboxes (Jack), for example a build and a separate test run
    from the same base. None of them changes the directory or the others.
    Pekit can implement this with overlay mounts.
  - `exec` also gains `cwd` and `env` options, to override the sandbox's
    defaults.
  - Pekit implements this with mount namespaces, not copies. A plain GCSL
    host can use the same mechanism or refuse.
- **Definitions named by directory.** Today a definition's path is unique
  for each def-type across the *whole workspace* (PEI-1590's finding). So
  examples 1 to 4 each defining `build toolchain` and `build main` would
  collide in one workspace. Jack's command-line design,
  `pekit build pekit2/examples/1/hello`, assumes a target's name is qualified
  by its directory. GCSL needs definitions scoped by directory, or pekit needs
  to qualify them.

  *Direction (Jack, example 5):* a directory's canonical files are
  `build.gcsl` and `package.gcsl`. Files in one directory see each other's
  definitions with no import, so `package.gcsl` writes `build main()`.

  GCSL's workspace rules already provide this for ordinary declarations
  (see GCSL's Workspaces page):
  - files in one directory share public declarations: fields, `def`s and
    types;
  - `let`, `let mut` and `fn` are private to their file;
  - other directories are reached with `import`, named from the workspace
    root;
  - child directories inherit the public declarations above them.

  The change GCSL needs is that def-type definitions such as
  `def build main` follow the same directory scoping, instead of being global
  across the workspace. Inheritance then lets a `build toolchain()` near the
  workspace root serve every directory below it.

  Caveat (Jack): finding a target from the command line is separate from
  scoping. Pekit searches the whole workspace for definitions of its
  def-types through the embedding API, which must report each definition's
  directory. So `pekit package dev.peios.kernel` works from anywhere in the
  workspace:
  - a unique name resolves from anywhere;
  - a path-qualified name always works;
  - an ambiguous short name is an error that lists the candidates.

  Packages use exactly the same resolution as builds (Jack). For example,
  `pekit build dev.peios.kernel/main` qualifies a build by its directory.
  Package names *should* be unique, because package managers want them to
  be, but pekit does not require it. If two definitions collide, the
  packager's publish rules catch it, such as ppk refusing a version whose
  content differs. Claude's proposal to make uniqueness a pekit rule was
  withdrawn.
- **Slash-path imports** (agreed, example 10). Imports name paths with
  slashes, for example `import lib/helpers` and
  `import pkgs/net.busybox.busybox as busybox`, matching the existing remote
  form (`import "github.com/gcslib/gcc"`). This replaces dotted paths, which
  clash with `pkgs/`'s reverse-DNS directory names. The default alias is the
  last segment, and a last segment containing dots needs an explicit `as`.
  Rejected alternative: resolving dotted paths by trying the full name and
  pruning segments. It guesses, and a new directory elsewhere could silently
  change an existing import. Also rejected: resolving unknown def-type
  references across the whole workspace (Claude's option C). Build names such
  as `main` repeat throughout `pkgs/`, and adding a definition elsewhere could
  break a working reference.
- **Importing every definition of one def-type** (Jack, example 10). It
  brings all `package` definitions under a directory tree into scope, so that
  an image can list 200 packages by name with no 200 imports. It is an error,
  at the import, if any two of those names collide; the error lists the
  duplicates. It breaks the usual rule that only what is named is imported,
  so the syntax must make the recursion visible (Jack). Claude's suggestion
  is a recursive glob:
  - `import package * from pkgs/` keeps ordinary import meaning: `package`
    definitions in the files directly in `pkgs/` only.
  - `import package * from pkgs/*` searches the folders one level below
    `pkgs/`. This is what an image over today's `pkgs/` uses (agreed).
  - `import package * from pkgs/**` searches any depth (agreed: both forms
    exist).
  - Both stop at nested workspaces and skip hidden folders, as GCSL's
    source discovery already does. The
  importing file then depends on every file it imported from, so evaluation
  units should be per definition, not per file, or any change under `pkgs/`
  would re-evaluate the image.
- **Methods, declared with a receiver** (agreed, example 8). Jack's
  motivation is that GCSL's many global functions are hard to discover,
  whereas methods autocomplete after `.` on a variable's static type.
  - Syntax: `def (files: List<File>) file(path: String): File { ... }`.
  - Methods live in their type's own namespace, so `files.file(...)` never
    collides with the global `file(...)`. That collision is why UFCS
    (calling any function whose first parameter matches with dot syntax)
    was rejected.
  - A method is visible where its declaring module is visible, in the
    manner of extension methods. Two visible methods with the same name on
    the same type are an error. A record field and a method with the same
    name are an error. A receiver can be generic (`List<T>`) or specific
    (`List<File>`).
  - `FileSet` stays as an alias, `type FileSet = List<File>`, so every list
    operation works on it and returns the same type, with no conversions.
    All GCSL lists become persistent internally, so concatenation is cheap
    for transitive data of any kind, not just files. Lists keep duplicates:
    pekit drops identical files when staging, and an order-preserving
    `unique()` exists. `Dir` stays a separate type for whole trees.
  - This implies moving GCSL's global prelude functions (`len`, `split`,
    `trim` and so on) to methods, with editor completion on `.`.
- **A shorthand for named arguments.** A bare `compiler` stands for
  `compiler = compiler` (Jack, 2026-10-23).
- **Non-constant defaults** (Jack, 2026-10-23). Today GCSL defaults must be
  constants. For a hermetic function, a default must be evaluated at the call
  site, before the hermetic boundary, because its value is an input: it goes
  into the cache key and, if it holds `File`s, into the sandbox.

## Example curriculum

Each example targets one north-star property. The ones that shape the
architecture come early. For each, Claude says what must happen, Jack writes
the build file as Jack would like it to look, and Claude critiques it against
this model and the later examples.

1. One C file to one binary. The bare minimum.
2. Several files, a library and headers: incremental rebuilds, and
   dependencies the compiler discovers.
3. A tool built during the build that generates source for another target:
   the machine that builds versus the machine the output runs on.
4. Fetching an external source tarball pinned by hash: network access and
   reproducibility.
5. Packaging outputs into a ppk package: today's pekit job as part of the graph.
6. One target built for two architectures in a single build: configurations and
   toolchains.
7. Tests as cached steps.
8. A user-defined library for a language pekit doesn't know: extensibility.
9. Dependencies only discoverable mid-build, such as Rust crates or C++
   modules: dynamic dependencies.
10. A small OS image made from many packages, with a shared cache: scale and
    remote caching.

Today's recipes add three real hermeticity problems to test the model against:

- The prelude recipe reads `$HOME` and `$PEIOS_SDK` inside `sh`.
- The kernel recipe runs Docker.
- The kernel recipe writes `{out_path}/../manifest.json`, outside its output.

## Secrets and signing (agreed, 2026-10-23)

What Peios signs:
- **Embedded signatures, made during the build:** kernel modules, which
  today's pekit signs with ML-DSA before packing, and Secure Boot images.
- **Detached signatures, made in a packager's `publish`:** packages,
  repositories and release ISOs.

The design:
- **`Secret<T>` is a core GCSL type (Jack).** GCSL itself guarantees that a
  secret does not leak: `show`, string formatting, logs, the event stream
  and the web view print it as `***`, under any host.
- **Secrets are contagious.** Any GCSL operation involving a `Secret`
  produces a `Secret`: interpolation, concatenation, records and lists
  containing one. The type checker enforces this.
- **`secret.reveal()` returns `Revealed<T>` (Jack).** It is the only way a
  secret leaves GCSL. Only the places where a value leaves GCSL accept it:
  `exec` arguments, environment variables and mounted files. A
  `Revealed<T>` is still redacted, and still contagious.
  - `reveal()` must not return the plain value. If it did, the secret would
    reach the per-`exec` receipt, the logged command line and the event
    stream.
  - Trust lies at processes. Passing a `Secret` into a GCSL function needs
    no reveal, because GCSL code cannot leak it. Searching for `.reveal()`
    lists exactly the external programs that ever receive a secret.
- **`secret.unsafe_value()`** is the rare, deliberate escape to the raw
  value, which strict mode or a linter flags.
- **A secret carries a public identity,** such as its name, version or
  public-key fingerprint, supplied by the keychain. Receipts record the
  identity and never the value, so outputs signed with a developer's test
  key and with the release key never mix in a shared cache. A derived
  secret's identity is derived from its sources' identities.
- **`keychain` is a stdlib library,** host-answered like `toolchain`, for
  example `keychain.file("module-signing")` or
  `keychain.string("api-token")`, resolved in parameter defaults.
  - Plain GCSL reads the OS keyring, environment variables or configured
    files.
  - Pekit reads from the workspace configuration.
  - Which key is used is context. A developer build uses a generated test
    key; a release profile uses the real key, held only by CI.
- **Under pekit, a `Secret` argument is mounted into that step's sandbox
  only.** It is never stored in any cache, never uploaded, and the step
  runs only on local or explicitly trusted workers. Outputs, such as signed
  files, are cached normally.
- **Use deterministic signatures where the algorithm allows,** for example
  ML-DSA's deterministic mode, which FIPS 204 permits. Re-signing then
  reproduces the same bytes, so `--check-reproducible` keeps working.

## Parity with today's pekit (agreed, 2026-10-23)

Today's pekit is the fresh start from PEI-1590. Features of the earlier
pekit (signed upstream tags, checksum manifests, Sigstore, `+dirty`) are out
of scope.

- **`init`** is probably unnecessary. The pekit def-types and attributes come
  from an importable module, not a copied `pekit-include.gcsl`.
- **`package`** goes through the configured packagers. `--rebuild` becomes a
  general flag. `--overwrite` becomes a `publish` option for local artifact
  directories.
- **`release`** is strict mode, then `publish`, then git tags (`name@version`,
  only ever added). Strict mode means a committed tree only, and no unpinned
  or undeclared inputs.
- **Local sources outside the workspace,** such as `../kernel`: the files Git
  would commit, including uncommitted changes, kept as something like
  `local("../kernel")`. They cannot be released.
- **Module signing** is a signing step with a `Secret` argument, cached under
  the key's identity.
- **`#[scratch]` (gap A).** A step marked with it gets a private directory
  that persists between runs of that step, so a coarse step such as the
  kernel's `make` can rebuild incrementally, as today's kept work
  directories allow. Its contents are not inputs and not on the receipt.
  Development builds use it; `pekit release` and strict mode always start
  it empty. It is a scheduler concern, so it is an attribute.
- **`SOURCE_DATE_EPOCH` (gap B)** is a function of the step's inputs:
  - a fetched archive's newest internal file time, fixed by its hash;
  - a fixed constant for local files, whose file times are not content.

  It is never the commit time, which would invalidate the whole cache on
  every commit.

## Commands, scripts and release (agreed, 2026-10-23)

**Commands:**

| Command | What it does |
|---|---|
| `build` | builds targets |
| `test` | runs tests |
| `package` | packs with the configured packagers |
| `publish` | packs, then publishes |
| `release` | strict mode, then publish, then git tags |
| `run` | runs a script, or a build whose result is an executable |
| `lock` | resolves external versions into `pekit.lock` |
| `query` | answers questions about the graph |
| `clean` | evicts the cache and clears `pekit-out/` |

Shared flags are `--platform`, `--rebuild`, `--strict`, `--check-reproducible`,
`--no-dynamic`, `--jobs` and `--trace`. Targets are named as in example 1.
Many can be selected with the same globs as imports (`pkgs/*`, `**`).

**`def script` (Jack).** A script's body is undeclared effectful GCSL that
runs only on `pekit run`, outside any sandbox, with the real terminal and
environment. Builds, tests and packages may not call a script; that is a
check-time error, so "building never boots or publishes" is enforced. A
script may call builds. Scripts also cover repository chores such as `fmt`
and `deploy`. As a shortcut, `pekit run` on a `def build` whose result is an
executable `File` runs it directly. A `build` and a `script` with the same
name in the same scope, which is a directory, are an error, because
`pekit run NAME` would be ambiguous (Jack).

**`pekit release NAME`:**
1. Builds from git `HEAD` and requires a clean, committed tree.
2. Builds in strict mode: no undeclared effects or local sources, empty
   `#[scratch]`, a fixed execution-platform order, `pekit.lock` exactly as
   written, and the release keychain profile.
3. Builds every step without reading the cache. Wherever the cache holds a
   result with the same receipt, it compares the bytes, and reports a
   mismatch as non-determinism or cache poisoning.
4. Packs and publishes through the packagers, with detached signatures
   made in `publish`.
5. Tags the commit `name@version`. Tags are only added, and an existing tag
   on a different commit is an error.

A provenance record for each release (commit, toolchains, receipts) is a
later option.

## Cache eviction (proposed, example 10)

Jack's requirement: eviction should weigh what an entry would cost to
recreate against the space it takes. A package can take an hour to build and
is small once compressed, while a `.o` file takes seconds, so the package
should outlive many `.o` files. Claude's proposal:

- **Use GreedyDual-Size.** This is a known caching algorithm, often with a
  frequency term (GDSF). Each entry's priority is its recreation cost divided
  by its size, plus an ageing term that rises as entries are evicted, so
  stale expensive entries eventually go too. The cheapest-per-byte entry is
  evicted first.
- **Use cumulative cost.** An entry's cost is what it would take to recreate
  it if its inputs were also gone: its own step's time plus the steps that
  produced its inputs. A package's own pack step takes seconds, but rebuilding
  it from scratch takes the hour, and the hour is what eviction should weigh.
  Pekit records each step's duration when it runs.
- **Keep metadata separate from content.** A step's cache record (its
  receipt, result listing and fingerprints) is tiny, so keep it nearly
  forever. Evict only file contents. Pekit can then still compute downstream
  receipts without the bytes, so a cached package remains usable even after
  the large install tree it was packed from has been evicted. This mirrors
  REAPI's split between its action cache and its content-addressed store.

## Packaging (agreed, example 5)

Jack's proposal, 2026-10-23, with Claude's refinements:

- **A package definition is format-neutral metadata:** name, version, files
  and dependencies. This matches the `def package` already used in `pkgs/`.
- **Each format can add its own section.** Formats disagree on dependencies,
  install scripts, file metadata and other features, so a package can carry
  per-format sections such as `ppk = {...}` or `deb = {...}`. Only that
  format's packager reads its section. nfpm works the same way.
- **A packager is an ordinary GCSL library, not a separate plugin
  mechanism.** Every packager provides two functions:
  - *pack*: hermetic and cached. It produces the package into `pekit-out/`.
  - *publish*: changes the world outside the build, by copying a packed
    package into a store. It runs only on `pekit publish`. Format rules, such
    as ppk refusing a version whose content changed, live here.
- **Workspace configuration selects the packagers.** The command line can
  override the selection, and several formats can be built at once.
- **Pekit core knows nothing about any package format.**

## Ideas for later

- **Generating fine-grained builds from upstream build systems** (Jack,
  example 4). A tool reads an upstream build, such as a Makefile or CMake
  project, and writes pekit build files with one step per compile. Pekit can
  then parallelise and cache third-party code finely. Bazel's ecosystem has
  `rules_foreign_cc` for the coarse approach, and generators such as Gazelle
  for its own build files.
