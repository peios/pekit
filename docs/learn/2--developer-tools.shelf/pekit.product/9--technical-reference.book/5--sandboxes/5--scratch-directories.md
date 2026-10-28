---
title: Scratch Directories
description: "Where pekit keeps the directory of a step marked #[scratch],
  whose it is, how runs share it, and when a run gets an empty one."
---

A step marked `#[scratch]` has a directory at `/pekit-scratch`, kept
between its runs, for a tool that does less on a second run: a build
system's object directory, Cargo's target directory, or a compiler cache
(`Scratch`).

## Whose it is

A scratch directory belongs to a step's output directory under
`/pekit-out`, which its identity names, and to the platform it builds for.
So it is kept when the contents of the step's inputs change, and a
different call of the same function, or the same call for another
platform, has its own.

It keys nothing. A step has to give exactly what it would give with an
empty directory, and pekit relies on that.

## Where it is

Scratch directories are in the store's directory, under `scratch/`, each
named by a digest of whose it is. Beside each is a lock file, which a run
holds while it uses the directory, and whose modification time is when it
was last held, and a size file, which eviction reads.

## When a run gets an empty one

A run gets an empty directory of its own, removed when it ends, when:

- The build is strict, so that what a release gives depends on nothing a
  scratch directory kept.
- Another run, of this process or another, holds the step's directory, so
  that no run waits for one.

A program of the step found in the store leaves the directory as it is.

## Removing them

An eviction removes scratch directories whole, ordered as other contents
are, as costing nothing to make again, and never one a run holds.
`pekit clean --cache` removes them with the store.
