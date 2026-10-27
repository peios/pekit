# Suspension: an evaluator that pauses and resumes

Status: proposed 2026-10-27 (Claude) for Jack's review, before building.
Tracked on PEI-1598. This replaces decision D2 of `action-graph.md`
("Resuming: restart"). The rest of `action-graph.md` still stands, except
where this note says otherwise.

## Why D2 is replaced

D2 made a graph-mode unit run again from its start every time it waited on
a result. It assumed that "pure evaluation is fast next to the steps it
waits on". That fails whenever steps are cached. On a no-change build of
authd, no step runs, so evaluation is the whole cost. The six targets
there ran 1,576 times between them, and `fetch` was called 95,447 times
for about 100 fetches. The cost grows with the square of the number of
effects a unit meets.

D2 also departed from the model Jack specified. He confirmed that model
again on 2026-10-27:

- GCSL evaluates everything it can.
- When it reaches something top-level (a step or an effect), it hands it to
  the host and moves on to other independent work, until everything is
  blocked.
- When the host gives a result back, GCSL picks up the waiting code where
  it stopped, as if it had run in one go.
- GCSL keeps its low-level scheduling. Only top-level scheduling is the
  host's.

None of this is visible to GCSL code: there are no new types and no new
syntax.

## Agreed rules (2026-10-27)

1. **Scheduling is split in two.** GCSL schedules its own branches. The
   host schedules nodes (steps and effects). When the host resolves a
   node, GCSL wakes the code that waits on it and runs everything it can.
2. **The host decides effect order.** This was refined with Jack on
   2026-10-27. GCSL makes no ordering decision of its own. Every effect
   becomes a node that carries its program position, and the host decides
   whether that order matters. A node the host isolates or makes
   content-addressed runs in any order. A node it runs with shared state
   runs in the order a sequential run would perform it. Examples of the
   latter are the `exec`s of one step's sandbox and anything run
   unisolated on the host. GCSL publishes the information that rule needs
   (see Program order).
3. **A memoised call that pauses is shared.** A second call with the same
   key waits for the first to finish, rather than running the body again.
4. **Failures are reported in program order.** If several branches fail,
   the one reported is the one a sequential run would have reached first,
   whatever order the branches actually ran in.
5. **Explicit-stack machine.** The evaluator's position is held as data,
   not on Rust's call stack. Async was rejected, because recursion through
   `async fn` allocates per nested expression, plain GCSL included.
   Stackful coroutines were rejected, because they need a stack per branch
   and tie a paused branch to its thread.
6. **Parallel evaluation is allowed, but not built yet.** Branches share
   state only through named shared structures, so that a multi-threaded
   scheduler is a contained change later. Try it on authd once suspension
   works.

## The machine

### Tasks

A *task* is one branch of evaluation. It holds everything that is now
implicit in the Rust call stack, or that is global to a run but really
belongs to one line of control:

- **Control frames.** A `Vec` of frames. Each frame says what to do with
  the value that the frame above it produces: the next operand of an
  `IntAdd`, the next element of a `MakeList`, the next iteration of a
  `For`, the restore of a `with`, the end of a call, a `recover`'s handler,
  a `#[retry]` attempt, and so on. There is one frame variant per compound
  IR construct.
- **Value stack.** The task's own `Vec<Value>` of slot frames, as the
  evaluator's stack is today (slot `n` is `stack[base + n]`).
- **Per-task state:**
  - the current context frame (today `Shared.context`);
  - the deadline stack (today `Shared.deadlines`);
  - the foreign-call sites (today `Shared.sites`);
  - the running memoised calls and futures on this task's line of control
    (today `MemoTables.running`);
  - the caller and helper flag;
  - the nesting levels.

State shared by every task stays shared:

- the IR;
- the workspace's top-level frame;
- the memo tables' entries;
- the graph;
- the profile;
- the scheduler's queues.

These are the only things a later multi-threaded scheduler has to make
concurrent.

### Program position

Every point in a run has a *program position*. This is the path of piece
indices through every enclosing forkable construct (see Forking), whether
or not that construct actually forked. One example is "item 3 of the
file, argument 2 of this call, iteration 17 of this `for`". Positions
compare lexicographically, which is the order a sequential run reaches
them in.

A task's position is where its work currently is, and it advances as the
task moves on to later pieces. A parent waiting at a join is at the end
of the construct, after all of its children.

Positions do not depend on whether a fork happened. Forks are lazy, and
whether one happens depends on whether the host already had a result. So
everything defined by position is deterministic: the effect order,
failure reporting and node identity.

### Running

The machine pops the top frame of a runnable task and feeds it the value
just produced. That either pushes frames to evaluate a child, produces a
value for the frame below, or ends in one of these:

- **Done:** the task's bottom frame received its value.
- **Failed or stopped:** an error, or a `break`, `continue` or `return`,
  unwinds frame by frame. Each frame does its cleanup, as each Rust
  function does today (restore the context, pop the deadline, pop the
  site, end the memo entry), until a frame catches it. The catching
  frames are the same ones as today: `For`/`Loop` for stops, `Catch` and
  the call boundary for `return`, `Recover` and `#[retry]` for unmarked
  errors.
- **Blocked:** the task needs something not yet there (see Blocking).

**Leaf children are evaluated in place.** An operand that is a constant, a
`Load` or a `LoadCapture` is read straight away, without pushing a frame.

**Simple subtrees stay recursive.** At lowering, every IR node gets a
`simple` flag. A node is simple if its subtree contains no call, `for`,
`loop`, `with`, `recover`, `await`, context read, stop or `Catch`. So a
simple subtree is built only from operators, constants, loads, field
accesses, indexes, `if`, blocks, `let` stores, record and list literals,
interpolations and the like. Such a subtree cannot block on a node, so the
machine hands it to a plain recursive function: the existing typed
walkers, cut down to those constructs. Operators keep one implementation,
shared by both paths.

- **Two kinds of construct are implemented twice.** `if`, blocks, stores,
  and record and list literals exist both in the walkers and as machine
  frames, because they occur both inside and outside simple subtrees.
  These are small. Every other control construct has exactly one
  implementation, in the machine.
- **A simple subtree can still read a pending slot.** In
  `let text = f.read(); let lines = text.split("\n")`, the slot `text`
  may be pending (see Forking). So lowering records each simple root's
  read set: the slots and top-level slots it loads. The machine checks
  them before handing off, and blocks the task on any that is pending.
  The walkers therefore never see a pending value, and plain mode pays
  nothing in them.

**Nesting limits are unchanged.** "call nested too deeply" is charged
statically, exactly as today (`levels`, `CALL_LEVELS`, the body's height),
so programs fail at the same calls. The machine's frames live on the heap,
so deep GCSL recursion no longer depends on `STACK_SIZE`. A simple
subtree's depth is bounded by the parser.

### Blocking

A task blocks when it needs:

- **a node's result** the host has not given (an await of a host future,
  or an effect in graph mode);
- **a pending slot**, which another task will fill;
- **a future another task is running** (a `lazy` body);
- **a memoised entry another task is running** (rule 3);
- **its forked children** (a join).

A task never waits for its turn to make an effect. Ordering is the host's
(rule 2).

A blocked task is filed under what it waits for. When that arrives, the
task is runnable again and carries on exactly where it stopped. Nothing
restarts. `Mark::PENDING`, `GraphRun`'s restart loop and the "evaluation
did not converge" guard all go away.

In plain mode, effects run at once and no host futures exist. Plain GCSL
therefore never blocks and never forks: it is one task from start to
finish.

### Forking

This is Jack's "move on to the next branch".

Forking is lazy: a task never forks in advance. When a task is about to
block, the scheduler looks down its control frames for the innermost
*forkable* frame below the blocking point. The forkable frames are:

- the elements of a `MakeList`;
- the fields of a `MakeRecord`;
- the arguments of a call;
- the pieces of an interpolation;
- the iterations of a collecting or plain `for`;
- the items of a block, including a file's top level.

If one is found, the task splits at that frame:

- The frames above it, with a copy of the slot frame they run in, become a
  **child task**, which blocks.
- The parent carries on with the next element, iteration or item.
- The child's result has a reserved **destination** in the parent, which
  is marked pending:
  - an element position, field or argument;
  - for a block item, the set of slots that the item declares. A `let`
    declares one, and a destructuring `let` declares several. The item's
    stores go to the parent's pending slots, not to the child's copy.

The parent eventually reaches the end of the construct. If children are
still pending there, it joins: it blocks until they finish. A read of a
pending slot blocks the reader on the child that will fill it. That
includes a top-level slot read by another unit's prepared call.

A non-constant parameter default can read earlier parameters. If those
arguments were forked, the default's frame sees their pending markers and
blocks on them, as any read does.

So code costs nothing extra until something actually blocks. Then
everything independent after the blocking point runs before the host is
asked for anything.

**A construct is forkable only if its pieces are independent.** The
checker marks this, and the marks are conservative:

- **No outward writes.** No piece may assign (`:=`) a variable declared
  outside that piece. An accumulator such as
  `let mut total = 0; for x in xs { total := total + x }` keeps its
  iterations in order. Variables declared inside a piece are its own: the
  child has its own copy of the slot frame.
- **No outward `break` or `return`.** No piece may `break` or `return`
  past the construct. A `for` whose body can `break` runs its iterations in
  order, so a search loop never speculates past the iteration that ends
  it. `loop` (`while`/`do`) is never forkable, since each iteration's
  condition depends on the one before.
- **`continue` is allowed.** It targets the `for` itself, so a forked
  iteration can end with `continue`, with or without a collected value.
  A child's outcome is therefore one of:
  - a value;
  - an error;
  - `Continue`, with or without a value.

  The `for` frame takes these at the join, as a sequential loop would.
- **Program order wins.** A child sees the slot values of its fork point.
  It never sees later writes by its parent, which a sequential run would
  not have made yet either.

These conditions mean every forked piece is one a sequential run would
also evaluate, unless an earlier piece fails (see Failures).

### Program order

**Effects (rule 2).** Today a node's order edge points at the node met
just before it. With branches, that node may not exist yet. For example,
iteration 1 of a `for` may be waiting on its `fetch`, so its later `exec`
hasn't been reached, while iteration 2 has already made its own `exec`. An
edge cannot point at a node that does not exist, so GCSL gives the host
two things instead:

- **Each node's program position.** This means "after everything earlier
  than this point", whether or not those earlier nodes exist yet.
- **The frontier.** This is the earliest position of any live task. No
  new node can ever appear before it. A parent at a join is after its
  children, so it never holds the frontier back.

The host's rule for a node it does not isolate is to run it once the
frontier has passed it and every earlier node it does not isolate has
finished. The host ignores order for the nodes it does isolate. GCSL never
waits on any of this: it makes the node, and the task blocks only if it
needs the node's result.

**Failures (rule 4).** An error in a forked piece goes to the construct's
frame in the parent, not up through the parent's frames. The construct
keeps each piece's outcome. On the first error, the construct
immediately cancels every piece after it in program order, so that the
host is not asked to run nodes for discarded work.

At its join, the construct takes the outcomes in program order, as a
sequential run would, and the first error wins. An error in the parent's
own later pieces is later in program order than every pending child, so
it waits at the join behind them.

**Cancellation.** A cancelled task unwinds all its frames as an error
unwind does today, with all their cleanup. That includes finishing any
memo entry or `lazy` future it owns with no result, so their waiters run
them again, and cancelling its own children. Its waits on nodes are
forgotten, and a node nothing else waits on is no longer demanded.

### Memoised calls and futures (rule 3)

A memo entry being computed is `Running`, and records the task that owns
it. A second task looking the key up waits on the entry. When the entry
completes, every waiter takes its value.

**"This call depends on its own result."** A call is a self-dependency
when the running entry is on the caller's own line of control: in its
running list, which a child inherits from its parent at the fork point.
The error is then raised exactly as today.

**Cross-task cycles.** Two siblings can each run one memoised function
and call the other's, so that each waits on the other. A sequential run
would have nested one inside the other and reported a self-dependency
there.

When a wait would close a cycle, the cycle is resolved by **wound-wait**.
The task later in program order abandons its run of the entry: it
cancels everything it did inside that call, its children included,
removes the entry, and waits on the earlier task's entry instead. The
earlier task then proceeds as a sequential run would: it either finishes,
or meets its own self-dependency and raises the error at the call where a
sequential run would.

Abandoning is safe, for two reasons. Forkable pieces have no outward
writes. And an ordered node made by the abandoned work lies after the
earlier task's position, so the frontier has not passed it and the host
has not run it. Abandoning withdraws such nodes. Cycles are checked when a
task is about to wait. The wait-for graph is small: one edge per task,
plus a parent's join edges.

**Futures.** A `lazy` future being run works the same way. Its running
flag records the owning task, and a second awaiting task waits.

### Contexts, deadlines and `#[retry]`

- **`with`** is a frame that sets the task's context and restores it when
  the task leaves the frame, whether by value, error or stop. A child
  inherits its parent's context at the fork point.
- **Deadlines.** `#[timeout]` measures an attempt's running time, not time
  spent waiting on nodes, as D2 also specified. A task's deadlines are
  extended by the time it spends blocked.
- **`#[retry]`** is an attempt frame. A failed attempt cancels its
  children, clears the frame's locals, and runs the body again, as today.

### Graph mode and the host

- A unit (a file or a prepared call) is a set of tasks kept in the
  workspace between host calls.
- `run_prepared` and `run_graph` run until every task of the unit is done
  or blocked. They return `Done(value)`, or `Blocked` with the nodes its
  tasks wait on.
- After `graph.resolve(node, outcome)`, the host calls `advance`. It wakes
  every task waiting on that node, in any unit, and runs everything it can.
  This is the `graph.advance()` loop already sketched in `action-graph.md`.
- **Demand.** The host runs a node only if a live task waits on it, or a
  finished unit's value holds it, as today.
- **Effect identity.** D3 numbered effects by order within a scope so that
  restarts would find them again. With no restarts, an effect's identity
  is its program position plus its number within the innermost piece.
  That is deterministic whatever order tasks ran in, and whether or not
  anything forked. Before changing this, I will check what pekit keys on
  effect identity.
- **Step bodies** (`run_step`) are units like any other. Their `exec`s
  share the step's sandbox, so pekit keeps them in order by rule 2.
- **Order.** The graph gives each node's program position and the current
  frontier. It replaces the order edge with them.
- **The profiler** must not count time a task spends blocked as a
  function's own or total time.

## Plain-mode cost

The machine is the only evaluator for most control constructs, so plain
GCSL pays for it too. The baseline is five programs measured with the
release `gcsl` at `7d83d32`, in `local/pekit2-data/bench/`:

| Program | Time |
|---|---|
| recursive calls | 229 ms |
| loops | 70 ms |
| memoised calls | 23 ms |
| string splitting | 80 ms |
| empty program (startup) | 4 ms |

**Proposed target: no program more than 10% slower.** Widening `simple`
and in-place evaluation help everything except calls.

Every call of a GCSL function goes through the machine. So the
recursive-calls benchmark (`fib`) measures the machine's call path
directly, and nothing in this design makes it faster than a frame per
call. If that misses the target, the remedy is to treat a call of a
function proven never to block as simple. That means a second, recursive
implementation of calls (arguments, frames, memoisation, `#[retry]`),
which is exactly the duplication this design otherwise avoids. See Open
points.

**Measured, 2026-10-27, and what was done.** With calls, loops and stops
on the machine, the costs against the baseline were:

| Program | Change |
|---|---|
| recursive calls | +4% |
| plain loops | +3% |
| memoised calls | +25% |
| string splitting | +10% |
| a call inside a `for` body | about +100% |

Each machine step costs about 7 ns that native recursion does not, and a
loop whose body calls a function pays several steps per iteration.

So the agreed remedy, a recursive path for code that never blocks, is
taken to its end. Plain evaluation never blocks, so it stays wholly on the
existing recursive walkers, at no cost, and graph mode runs on the
machine. This keeps two implementations of each control construct. They
are kept equal by running the whole test suite twice: once as is, and
once with the crate feature `machine-everywhere`, which runs plain
evaluation on the machine too. The gate does both. In graph mode, calls of
functions proven never to block can later move back to the walkers, if
evaluation's own cost there matters once restarts are gone.

## How it gets built

Each stage leaves GCSL working, with the full test suite passing.

1. **Analyses.**
   - The `simple` flag and each simple root's read set.
   - Forkability marks: no outward writes and no outward `break` or
     `return`.
   - Unit tests for each.
2. **Machine core, one task.**
   - Every control construct as frames.
   - Graph mode keeps restarting during this stage. A pause is still an
     error marked `PENDING` that unwinds through the frames, as it does
     through Rust functions today. So authd runs under the new machine
     too.
   - The old recursive evaluator stays in the crate. The whole test
     suite, pekit's examples and authd run under both, and the results,
     values and diagnostics, must be identical.
   - Benchmark against the baseline.
3. **Blocking and resuming.**
   - Graph mode suspends instead of restarting.
   - The host API changes to `Blocked` and `advance`, and pekit follows.
   - Restart, `PENDING` and the convergence guard are removed.
   - The profiler stops counting blocked time.
   - Measure authd.
4. **Forking.**
   - Lazy splits, destinations, pending slots and joins.
   - Program positions and the frontier, failures in program order, and
     cancellation.
   - Shared memo entries and futures, and wound-wait.
   - Effect identity by program position.
   - Measure authd.
5. **Clean-up.**
   - Delete the old recursive evaluator's control handling.
   - Rewrite the evaluator's module documentation and the Learn page on
     graph mode.
   - Update `action-graph.md`.
6. **Later, as agreed: parallel evaluation.** `Arc` and concurrent shared
   structures, and a multi-threaded scheduler. Measure on authd.

## Progress

**Stage 3, done (2026-10-27).** Graph mode never restarts a unit: every
pause, in a target's code, a file's or a step's body, is on the machine,
and a walker that would stop in graph mode is an internal error. Host API
changes made:

- `Progress::Waiting(unit)`: the unit needs a memoised call or a `lazy`
  that another unit is running and has stopped in. pekit runs it again
  once anything else in the build has run, and fails the build if
  nothing can (the units wait for each other).
- Memo entries and futures being run record their owning unit; a unit
  meeting its own running entry is a self-dependency, as before.

On authd, a no-change build went from 1.87 s to 0.78 s, and GCSL's
evaluation from 1.13 s to 0.26 s: `fetch` is called once per fetch.

## Stage 4 proposal (2026-10-27, for Jack before pekit depends on it)

Forking is the remaining lever: a target still stops at each of its
effects in turn, about 250 round trips for `nss`. The first cut forks
only the iterations of a `for` inside a function's body, which covers the
crate loops that dominate. Before any fork code:

1. **Forkability marks in the checker** (stage 1's missing piece): a
   `for` is forkable only if its body assigns no outer variable and has
   no `break` or `return` out of it. Tested on the four cases.
2. **Per-task state.** Everything stage 3 sets aside per unit becomes per
   task, swapped on each task switch; a child inherits its parent's
   running memoised calls. Memo and future owners become `(unit, task)`.
   A walker reads top-level cells through a slice of its own, so that a
   task with its own stack can read them.

**API changes proposed:**

- **`Progress::Paused` carries a set of nodes**, since one unit now waits
  on many at once. pekit's target state holds the set, demands them all,
  and runs the unit again when any is given. `--no-dynamic` and query
  mode, which look at one node today, look at each.
- **`GraphHost` carries the host's isolation predicate**: which effects it
  isolates. This is rule 2's frontier, enforced inside GCSL rather than
  published: a task makes a node for an effect the host does not isolate
  only when no task earlier in program order is live, so such effects
  still reach the host in program order, one at a time, as pekit relies
  on today. pekit's predicate isolates `fetch`, `fetch_git`,
  `archive_unpack` and the reads of files; every other effect is
  ordered, the conservative default.
- **`#[timeout]` and forks**: a child inherits its parent's deadlines,
  each moved on by the child's own stopped time.

## Open points for Jack

- **Speculation policy.** Forking happens only where every piece would be
  evaluated by a sequential run anyway (no outward writes, no outward
  `break` or `return`). This never does work a sequential run would skip,
  except after a failure, which cancels the rest. A bolder policy could
  run past a `break`, but it could make arbitrarily many unneeded nodes.
  Recommendation: the conservative policy.
- **Time spent blocked does not count towards `#[timeout]`.** This is
  carried over from D2. Recommendation: keep it, since a step's own
  `#[timeout]` already limits the work the host does for it.
- **The plain-mode budget, and calls.** The proposed target is 10% on
  every benchmark. If the call path misses it, the choice is between:
  - accepting a slower plain-mode call; or
  - a second, recursive call path for functions proven never to block,
    which duplicates call handling.

  Recommendation: measure in stage 2 first. If calls miss the target, take
  the second call path. That fits your rule that pekit's build speed is
  worth extra code, and the duplication would be confined to calls.
