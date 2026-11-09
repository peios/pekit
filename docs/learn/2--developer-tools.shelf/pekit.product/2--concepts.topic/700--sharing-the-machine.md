---
title: Sharing the Machine
type: concept
description: How every pekit build on a machine shares one budget of
  programs and memory, how pekit estimates what a program uses, how make
  and Cargo borrow from the budget, and how two builds that need the same
  step run it once.
related:
  - pekit/concepts/targets-and-steps
  - pekit/concepts/reuse-and-the-store
  - pekit/building/managing-the-store
---

Several pekit builds often run on one machine at once: a build in each
worktree of a repository, or one for each agent working on it. Each build
runs as many programs as `--jobs` allows, so without anything shared, twelve
builds would run twelve times as many compilers as the machine has
processors, and could run it out of memory. pekit shares one budget between
every build on the machine instead, and runs a step that two builds need at
the same time only once.

## The budget

The machine's budget has two limits:

- **Jobs:** how many programs can run at once, by default one for each
  processor.
- **Memory:** how many bytes those programs are expected to use at once, by
  default half the machine's memory, leaving the rest to editors, other
  builds, and everything else the machine runs.

Before a program of a step runs, pekit takes one job from the budget, and as
much memory as it expects the program to use. When the program ends, it
gives them back. A program that can't have both waits, holding neither,
while the build goes on with everything else it can do. A build's programs
wait in the order the build asked for them.

`--jobs` still limits one build: a build runs at most that many programs,
however much of the budget is free. The budget limits every build together.

A program expected to use more memory than the whole budget runs alone,
once every other program has given its memory back. A program that has
waited long for the budget stops every other program from starting until
it starts, so a stream of smaller programs can't keep it waiting forever.

The budget only changes when a program runs. It's never part of a step's
key, so a build with another budget finds the same steps in the store.

## What a program is expected to use

pekit measures the peak memory of every program a step runs, and keeps the
last few peaks of each program of each step. It expects a program to use a
quarter more than the most it used in those runs:

1. by the same program in the same step, if that was measured;
2. otherwise by the same program in other steps of the same function, such
   as `rustc` in another crate's compile;
3. otherwise 1 GiB, or what you set, and half as much again for `rustc`:
   one `rustc` compiling a large crate, such as a GUI application's, can
   use 0.7 to 1.4 GB.

The peak pekit measures is that of the program's largest process, not the
sum of its processes. A `make -j` that runs several compilers at once uses
more than its peak says. If such steps overrun the machine, lower the
budget's memory to leave room.

## Setting the budget

Set the budget for every build on the machine in `machine.conf`, in pekit's
configuration directory: `$XDG_CONFIG_HOME/pekit`, otherwise
`~/.config/pekit`.

```text
# This machine: 16 processors, 64 GiB, other work alongside.
jobs = 12
memory = 40GiB
unseen = 2GiB
```

`unseen` is what a program is expected to use when pekit has never measured
it. Each setting can also be given in the environment, which overrides the
file: `PEKIT_MACHINE_JOBS`, `PEKIT_MACHINE_MEMORY`, and
`PEKIT_MACHINE_UNSEEN`. Sizes are written as `PEKIT_CACHE_LIMIT`'s are,
such as `512MiB` or `24GiB`.

Each build reads the budget for itself. Builds that read different limits
share the larger, so set the budget in `machine.conf` rather than in one
build's environment.

## Jobservers for `make` and Cargo

A program can use more than one processor. `make -j`, Cargo, `rustc`, and
the `cc` crate share processors through a GNU make *jobserver*: a pipe of
tokens, each a job they can run. pekit gives every program of a step a
jobserver, by setting `MAKEFLAGS` and `CARGO_MAKEFLAGS`. Every token in it
is a job of the machine's budget that pekit lends while it's free.

So a step's `make` runs its recipes one at a time on a busy machine, and as
many at once as there are free jobs on an idle one. Run `make` without `-j`
in a step: GNU make 4.4 or later takes its jobs from the jobserver, and
an older make runs one recipe at a time.

pekit takes lent jobs back as soon as another program waits for the
budget. A step that sets `MAKEFLAGS` or `CARGO_MAKEFLAGS` itself keeps its
own value. Neither variable is part of a step's key.

## A step two builds need at once

Two worktrees at the same commit need the same steps, and both would run
each one before either had recorded it. Instead, a build that doesn't find a
step in the store claims it, and holds the claim until the step is
recorded. Another build that needs the step meanwhile waits for the claim,
going on with its other steps, and then finds the step in the store:

```text
built app/hello: 0 steps run, 2 found in the store
```

If the build that held the claim fails the step, or stops, the waiting
build runs the step itself.
