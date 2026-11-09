---
title: The Machine's Budget
description: The lock files every pekit process on a machine shares to
  bound the programs and memory of all its builds, how a program waits for
  them, how its memory is estimated and measured, and the jobserver each
  program is offered.
---

Every pekit process on a machine shares one budget (`Budget`): at most
`jobs` programs at once, expected to use at most `memory` bytes at once.
Nothing runs to keep it. It's a directory of lock files, each held with
`flock` by whatever uses it, so a process that ends, however it ends, lets
go of everything it held.

## Its limits

Each limit is read, in order of precedence, from:

1. the environment: `PEKIT_MACHINE_JOBS`, `PEKIT_MACHINE_MEMORY`, and
   `PEKIT_MACHINE_UNSEEN`;
2. `machine.conf` in the configuration directory, `$XDG_CONFIG_HOME/pekit`
   or `~/.config/pekit`, a line `KEY = VALUE` for each of `jobs`, `memory`,
   and `unseen`, with `#` starting a comment;
3. the machine: a job for each processor, half of `MemTotal` in
   `/proc/meminfo`, and 1 GiB.

`jobs` is a count above 0. `memory` and `unseen` are sizes, written as
`PEKIT_CACHE_LIMIT`'s are, above 0. Anything else, or a line of
`machine.conf` that sets no limit, fails every command that builds.

## Its files

The budget's directory is `pekit/budget` under `$XDG_RUNTIME_DIR`, which the
system empties when the user's last session ends, or `budget` in the cache
directory where that's unset. It holds:

| File | Held by |
|---|---|
| `job-<n>`, for `n` below `jobs` | A running program, exclusively, or a job a program's jobserver lent. |
| `memory-<n>`, for `n` below `memory` / 256 MiB | A running program, exclusively, a file for each 256 MiB it's expected to use. |
| `waiting` | Every process with a program waiting for the budget, shared. |
| `gate` | A program that has waited 2 seconds, exclusively, until it starts; every other process, shared, while it tries to start one. |
| `fifo-<pid>-<n>` | A program's jobserver, made by its meter and removed when it ends. |

Each process uses the files numbered below its own limits. Processes that
read different limits share the larger.

## Waiting for the budget

A program of a step takes its job and its memory on the pool's thread,
before it runs (`Budget::admit`). These programs wait:

- `exec`, `exec_live`, and `exec_secret`;
- a sandbox's `exec`;

unless `interactive`. A program found in the store doesn't run, and takes
nothing. A target's own effects, which run one at a time, don't wait.

A process's programs wait in the order they asked, and only the first of
them tries the files. It tries to hold, without waiting, a `job-<n>` file
and as many `memory-<n>` files as it needs, at least one and at most all of
them. It holds all of them or none, so no two programs wait for each
other. While it can't, it holds `waiting`, and tries again, at first after
2 ms and then less often, up to every 50 ms, or as soon as a program of its
process ends.

Once it has waited 2 seconds, it holds `gate` exclusively, waiting for it.
No program of another process starts while it does, since each holds
`gate` shared while it tries, so the program starts as soon as running
programs end, however many smaller programs would otherwise start first.

A budget whose files can't be opened lets the program run at once: the
budget paces programs, and never fails one.

The time a program waits is added to its step's deadline and left out of
its cost, as the time its staging took is. Programs wait on the pool's
threads for effects, so they hold up none of the engine's own work. See
[The Pool](~pekit/the-pool).

## Estimates

A program of a step is expected to use a quarter more than the largest of
its kept peaks, each in bytes (`Budget::estimate`):

1. its own, kept under the step's name `<function>/<identity>` and the
   program's file name;
2. otherwise its kind's, kept under the step's function and the program's
   file name, by every step of that function;
3. otherwise `unseen`, or half as much again for a program named `rustc`
   (`Budget::unseen`).

Peaks are kept in the state directory under `peaks/`: a file for each,
named by a SHA-256 of the names it's kept under, the kind's prefixed
`kind-`, holding the last four peaks, oldest first, a decimal number of
bytes on each line. Each file is replaced whole. Peaks key nothing.

## Metering

Each program of a step runs under `pekit __meter`, outside its sandbox,
which runs the program's bubblewrap and waits for it. The meter:

- dies with the pool's thread that started it, by `PR_SET_PDEATHSIG`, and
  exits at once if that has already ended;
- is a *subreaper*, so that the process bubblewrap starts as the sandbox's
  `init`, which bubblewrap's own process doesn't wait for, is reparented to
  it and waited for. Its peak, the largest of the program's processes',
  then reaches the meter;
- waits for what the program left, its `init`, for 5 seconds at most;
- keeps `ru_maxrss` of `getrusage(RUSAGE_CHILDREN)` as the program's peak,
  for the step and its kind;
- exits with the program's status, or 128 and the signal's number for a
  program a signal killed, which `exec` reports alike.

The meter is added after the program is keyed. Neither it nor anything it
adds reaches a receipt or a record.

## Jobservers

Each metered program is offered a GNU make jobserver. The meter makes a
FIFO, `fifo-<pid>-<n>` in the budget's directory, and bubblewrap binds it
at `/dev/pekit-jobserver`. `MAKEFLAGS` and `CARGO_MAKEFLAGS` are set to
`--jobserver-auth=fifo:/dev/pekit-jobserver`, each unless the program's
`exec` sets it. Neither has `-j`, which a make older than 4.4, reading no
FIFO, would take as no limit.

The program holds one job, which the protocol leaves implicit. Every 10 ms,
the meter:

- reads back every token the FIFO holds: tokens unread, or given back;
- writes one back, unless a program of any process waits for the budget,
  since `waiting` is held, or a token has sat unread for 250 ms; and gives
  back to the budget the job of every other token it read;
- if the FIFO held nothing, so every token it lent is the program's, lends
  one more, taking a free job without waiting, if one is free, no program
  waits, `gate` isn't held, it has lent fewer than `jobs` less one, and it
  isn't backing off.

A token that sat unread for 250 ms makes the meter back off before it lends
again: 250 ms, doubled each time up to 4 seconds, and reset once a token is
read. So a program that never reads the jobserver holds a second job for
at most 250 ms at a time, less and less often. When the program ends, the
meter gives back every job it lent.
