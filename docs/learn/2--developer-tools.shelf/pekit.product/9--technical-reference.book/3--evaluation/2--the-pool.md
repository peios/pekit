---
title: The Pool
description: The threads that run a build's effects and pekit's own work
  for steps, what each kind of work is, and why evaluation stays on one
  thread.
---

GCSL's values are reference-counted for one thread, so one thread evaluates
every unit of a build. What runs in parallel is work that needs no GCSL
value: programs, file-system effects, and pekit's own handling of whole
trees. That work runs on the *pool* (`Pool`), a fixed number of threads
taking work from one queue.

## Its size

The pool has `--jobs` threads, by default as many as the machine has
processors. A running program holds its thread until it ends, so `--jobs`
is the most programs that run at once. A step's own `make -j` or Cargo
starts as many processes as it chooses, inside the one program.

## Kinds of work

Each piece of work (`Work`) is one of these:

- **An effect of a unit,** with its effect context and its deadline: a
  step's effect in the step's sandbox, or a target's effect on the host
  view.
- **A fetch,** answered from the store or downloaded.
- **A read of an output** by a target's code, answered from the store's
  tree of it while the output isn't exposed.
- **Staging** a step's sandbox before its body runs.
- **Keeping** a step's result: putting every file and directory it names
  into the store, once it has given one.
- **A program a step runs whose result can be recorded,** with the hashing
  and placing of trees that finding or recording it takes. See [Program
  Records](~pekit/program-records).

Staging and keeping place or hash whole trees, often of tens of thousands
of files, so they run on the pool beside the programs, never on the thread
that evaluates. A step's unit runs once its sandbox is staged, and the step
is recorded once its files are kept.

Each piece of work reports what it came to, with the thread that ran it and
when it started and ended, for the build's events, and the store's files it
used, for eviction.

## Order

Effects of different steps run in any order and at the same time. A step's
own effects run in its program order, one after another, since they share
the step's sandbox. A target's own effects run one at a time in the
target's program order, since they share the host. Effects that pekit
answers by content, a fetch or an unpacking, run in any order.
