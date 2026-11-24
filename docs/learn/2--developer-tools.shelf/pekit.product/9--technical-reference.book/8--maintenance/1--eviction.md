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

As with eviction at the end, room is made beside other processes only as
[Eviction beside other builds](#eviction-beside-other-builds) describes.
Otherwise the build warns that nothing was evicted, and why, and goes on.
It warns too if it could go on only by starting a step it holds, and it
then starts that step.

## Outputs of evicted steps

A build exposes outputs under its output directory as hard links to the
store's blobs (see [Exposure](~pekit/where-results-go)). An eviction removes
a blob's name in the store, but an exposure's link keeps its contents on the
disk, though the store's size leaves them out. No build can use them,
because a build exposes what it needs again from the store.

Each build notes its workspace's output directory beside its stamps, in
`seen/<digest>.outputs`. An eviction that evicted something with the store to
itself, and every `pekit store evict`, then removes each exposure under
`.steps/`, `.unpack/`, `.git/`, or `.fetch/` of a noted output directory that
holds a file of the store's file system that no name in `cas/` links. It
never touches the files a target gives, nor anything on another file system,
which is a copy rather than a link. What it found is kept in `orphaned`, which
`pekit store stats` shows.

## Eviction beside other builds

While other processes have the store open, a build evicts beside them, if
every process that holds the store publishes what it uses. Each process
keeps a file of its own in `using/`, `<pid>-<token>`, locked for as long as
it lives. Before it relies on anything of the store, it appends it there,
once: each record before it reads it, each blob before it looks for it or
links it, and each tree it takes as complete. The kernel's `/proc/locks` says
which processes hold the store. One that publishes nothing, an older pekit
say, keeps the build from evicting beside it, and the build warns as it
always did. So does a process that could not write its file, the disk full
say: it removes the file, and publishes no more.

One such eviction runs at a time, holding the `evicting` lock. It also holds
the store's `gate` for its whole length, so that no process opens the store
meanwhile; if either is held, the eviction does not run:

1. It evicts as an eviction alone does, keeping everything any live
   process has published, but it moves each blob it evicts into
   `tmp/evicting-<token>/` rather than deleting it.
2. It reads what each live process has published again, and moves back
   each blob published meanwhile, directly or through a record or a tree.
   If a process has meanwhile put a blob of the same name, of the same
   contents, the one moved aside is dropped instead.
3. It deletes what is still aside.

A process that published a blob before step 2 has it moved back. One that
published it after finds it gone, and puts or builds it again. So no
process relies on what is deleted. Such an eviction removes every mark that
a tree is complete, but it leaves the materialized trees, which a running
step might have mounted, and the outputs of what it evicted, for an
eviction with the store to itself. `pekit store stats` shows how many
materialized trees there are. A file in `using/` whose lock no process
holds is a dead process's, and is removed. What a dead evictor moved aside
is moved back by the next eviction or sweep, unless a blob of the same name
is there again, and the rest is deleted.

An eviction as a build ends keeps everything that build used, not only what
other processes use: it still holds the store as it evicts. A store that only
such evictions trim keeps each running build's working set, and is trimmed
further by `pekit store evict` or by an eviction with the store to itself.
