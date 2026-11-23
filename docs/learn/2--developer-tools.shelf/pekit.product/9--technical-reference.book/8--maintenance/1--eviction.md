---
title: Eviction
description: How pekit keeps its store within PEKIT_CACHE_LIMIT by
  GreedyDual-Size, what an eviction removes and in what order, how the
  store's size is known, and when an eviction runs.
---

The store can grow to the size `PEKIT_CACHE_LIMIT` gives: 100 GiB by default,
or no limit for `0` (`evict::limit_of`). As a build ends, pekit journals
what the build used and, if the store is larger than its limit, evicts until
it is below nine tenths of the limit (`evict`).

## What is evicted

An eviction removes a record's contents together: every blob and tree a
record names, whether a step's, a program's, an unpacking's, or a
download's. The record stays, but isn't *live*: a later build that
finds it runs what made it again. A blob is removed once no live record
names it at any depth.

An eviction never lists a record's contents whole. Each blob counts what
names it: each live record that names it directly, and each tree a live
record reaches that holds it as an entry. Evicting a record lets go of what
it names. A blob that nothing names any more is removed, and a removed tree
lets go of its entries in turn. Trees share their subtrees. So the memory
an eviction needs depends on how many blobs the store holds, together with
how many entries its trees have. It doesn't depend on how much each record
names. A store of 37 GiB in 128,000 records
evicts in about 200 MiB.

Before it removes any blob, an eviction removes every materialized tree and
every completeness mark, since both depend on blobs being there. In order,
it then removes:

1. Blobs no record names, and numbered and executable copies, which cost
   nothing to make again.
2. Records' contents, and scratch directories, by priority, lowest first.

A scratch directory is evicted whole, as costing nothing to make again, and
never while a run holds it.

## The order

The order is GreedyDual-Size. Each record's priority is `L + cost / size`,
set when the record was last used, where:

- `cost` is the time it took to make, in milliseconds.
- `size` is the size of its contents, in bytes. A file that two of its
  directories hold counts for each.
- `L` is the priority of the last record evicted.

So what is cheapest to make again for the space it takes goes first, and
among records alike in that, what was used longest ago, since `L` grows as
evictions go on. A large file that took a second to make goes before a
small one that took a minute.

## What was used

Each build writes a journal of the records and scratch directories it used
into the store's `usage/`, one file per build. An eviction reads every
journal, and keeps each record's priority, with `L`, in `eviction.json`,
with which records it evicted.

## The store's size

The store's size is its blobs' and its scratch directories'. Walking every
blob on every build would cost too much, so the blobs' size is kept in
`size`, a *ledger*: what an eviction last measured, walking every blob,
plus the bytes of the blobs each build has written since, added under
`size.lock` as each of its steps is recorded and as it ends.

The blobs are walked again only when the ledger is above the limit, is a day
old or older, or isn't there. A build killed part way has added what it
wrote up to its last recorded step, so the ledger falls short by little
more than one step's output, which the next walk sets right.

## When it runs

An eviction takes the store's lock exclusively without waiting. If another
process has the store open, the build evicts nothing, and a later build
does. A store that can't be kept within its limit is a warning, not a
failure:

```text
warning: the store could not be kept within `PEKIT_CACHE_LIMIT`: MESSAGE
```

If another process has the store open and the ledger says the store is over
its limit, the build says so in the same warning, with the store's size and
its limit, rather than evicting nothing silently. A command shows each
warning once, however many builds it runs.

## Room during a build

With `keep_free` set, a build evicts during the run as well
(`evict::make_room`), when less than that much is free on the file system
of the store or the scratch directory. It starts no new step until none of
its own steps, effects, or jobs is running. It then evicts until the floor,
and a tenth more, is free. Whatever it costs, it keeps:

- every blob the build has used: put, looked up, or taken by its stamp;
- every record it found or wrote;
- every scratch directory it held.

A step that fails for want of room is held, unstaged, and run once more after
room is made for it: at least the floor, and at least 10 GiB. A step counts as
failing for want of room if its error says "No space left on device", "Disk
quota exceeded", or tar's "Wrote only", or if less than the floor, or 1 GiB,
is free once it has failed. If it fails so again, or no room could be made,
its error ends with a note:

```text
= note: not enough space: the file system of the store and the scratch directory has FREE free, and the build could make no more room
```

As with eviction at the end, room is made only if no other process has the
store open. Otherwise the build warns that nothing was evicted, and goes on.
It warns too if it could go on only by starting a step it holds, and it
then starts that step.
