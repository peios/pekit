---
title: Queries
description: How pekit query evaluates targets without running a step,
  what it counts as a file a target reads, and how it treats a target that
  waits for a step's result.
---

`pekit query deps` and `pekit query rdeps` evaluate targets as a build
does, in a query mode of the engine (`engine::query_all`), but start no
step.

## What a query runs

A target's own effects run, since evaluation needs them: its fetches,
unpacks, and reads of the workspace. No step starts, and no step's record
is looked up. What a target reaches is the steps its value needs, or the
steps the result it stopped for needs, each with the steps it is given.

A step's outputs are known only once it runs. A target that stops for a
step's result is done there: what it reached so far is reported, and where
it waits (`TargetDeps::waits`). The query never builds to find out more.

## What a target reads

A target reads a workspace file if any of these holds:

- Its prepared call's code needs the file, as a GCSL source.
- A step it reaches has code in the file.
- One of its own effects reads the file.
- A step it reaches is given the file as an argument, capture, or context
  value, or is given a directory that holds it.

A GCSL file is also read by every target whose code is in its directory or
below, since its declarations are visible there. A change to it can change
what a name there means, whether or not anything names it yet.
`workspace.gcsl` is read by every target.

## Reverse dependencies

`pekit query rdeps FILE` lists, of the targets it selects, those that read
the file (`TargetDeps::reads_file`). A target that waits for a step's
result might read the file after the wait, so it is listed too, with a note
on standard error: an unknown is never taken for no.

The prelude's `test` definition type awaits its definition's step inside a
`recover`, so every test's unit waits for its own step's result. So
`pekit query rdeps --kind test` lists every test, those it can't rule out
with a note.
