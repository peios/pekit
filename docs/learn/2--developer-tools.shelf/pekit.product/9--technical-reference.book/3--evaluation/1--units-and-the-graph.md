---
title: Units and the Action Graph
description: How pekit evaluates each target as a GCSL graph-mode unit,
  which calls become steps, how a unit stops and resumes, and how a build
  that cannot progress fails.
---

pekit evaluates every target in GCSL's graph mode, against one action
graph for the whole build. GCSL's [Running Steps for a
Host](~gcsl/graph-mode) page describes graph mode; this article describes
what pekit does with it.

## Prepared calls

Each selected target is a *prepared call* of its definition: `build hello()`,
`test greeting()`, or `script deploy()`. Two commands wrap the call in a
function of the standard `packaging` module, since pekit can't call a GCSL
function value from outside GCSL:

- `pekit package` prepares `packaging.pack(package NAME())`, which packs the
  package with each of `^packagers`.
- `pekit publish` and `pekit release` prepare
  `packaging.publish(package NAME(), "STORE")`. The store is an argument,
  not a context, so that publishing into a second store keys no step
  differently.

A prepared call binds its own definition as a reference's call does
(GCSL's `run_prepared`), so the steps it makes are named under that
definition. Each prepared call is evaluated as a *unit* of its own.

## Steps and other calls

In graph mode, a call of a function whose attributes defer it is not run
where it is made. GCSL adds it to the graph as a node, and the calling code
goes on with a *hole* where its result will be. pekit takes a deferred node
as a step if one of its function's attributes is `hermetic` and not
`hermetic(false)`.

Inside a step's own unit, a deferred call is not a node: it runs inline,
as part of the step. So only a call that a target's code makes, directly or
through any number of ordinary functions, becomes a step.

Every effect, a built-in that touches the world, is a node too, carrying
its position in program order. pekit runs a step's effects in the step's
sandbox, in the step's program order, and a target's own effects on the
host view, one at a time, in the target's program order.

## Stopping and resuming

A unit runs until it ends or until it needs a node's result that pekit
hasn't given: a step's value it reads, an effect's result, or a memoized
call another unit is computing. It then stops, and GCSL keeps its position
as data. Once pekit gives the result, the unit resumes from where it
stopped, without running any code twice.

A unit that ends with a value whose holes are steps not yet run is done
evaluating: its value is complete once those steps give their results. A
target is built when its value has no holes.

Within a unit, GCSL schedules its own branches, and runs everything it can
before stopping. Where several branches fail, the failure reported is the
one a sequential run would have reached first.

## The build loop

The engine repeats, until every target is done (`build_all_observed`):

1. Run each unit that can progress: one whose results are given.
2. For each step node a unit needs whose inputs are given, look its
   receipt up in the store. A hit gives the step's recorded result. A miss
   stages the step's sandbox on the pool, then runs the step's body as a
   unit of its own.
3. Queue each ready effect of a step's unit on the pool, and run each
   target's next effect in program order.
4. Take each finished piece of work from the pool, and give its result to
   the node that waited for it.

A memoized call or a `lazy` that two units need is computed by the first
to reach it, and the other waits for it. If no unit can progress and no
work is running, the units wait for each other, and the build fails with
"the build's targets and steps wait for each other's shared results".

## Naming steps under definitions

A step is reported, and its outputs linked, under the target whose unit
first met it, or under the target of the step that met it. Its identity
and directory don't depend on that, as [Identities](~pekit/identities)
describes. A step made inside a `build` called from another target, a
test, or a script is made under the innermost definition around the call
(GCSL's `Workspace::step_definition`), so `build lib()` called anywhere
makes the steps `pekit build lib` makes.
