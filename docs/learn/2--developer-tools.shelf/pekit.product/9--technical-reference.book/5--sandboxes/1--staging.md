---
title: Staging
description: How pekit makes a step's staging directory from the store,
  which files it links, which trees it mounts, and how it removes the
  directory when the step is done.
---

A step's sandbox is made of a *staging directory* on the host, under the
cache directory's `scratch/`, named `step-<token>-<node>-<attempt>`. The
pool stages it before the step's body runs (`Work::Stage`).

## What it holds

The staging directory has four parts, which the sandbox binds at their
logical paths:

| On the host | In the sandbox | Holds |
|---|---|---|
| `w/` | `/w` | The workspace files and directories the step is given. |
| `out/` | `/pekit-out` | The outputs of other steps, fetches, and unpacks the step is given. |
| `out/.steps/<function>/<identity>/` | The same path, writable | The step's own output directory, empty at the start. |
| `tmp/` | `/tmp` | An empty directory of the step's own. |

Every input is staged from the store, never from the workspace, so the
step sees exactly the contents its receipt holds, even if a workspace file
changes while the build runs.

## Files and trees

Each input is staged by what the store keeps of it (`StepSandbox::stage`):

- **A file** is a hard link to its blob, `cas/<xx>/<digest>`, or to
  `<digest>.x` if it is executable. A blob is read-only, so the step can't
  change it.
- **A directory of fewer than 64 files** is made file by file, each a link
  to its blob.
- **A directory of 64 files or more** is mounted: the store's materialized
  tree of it is bound read-only on an empty directory at the input's path.
  See [Materialized Trees](~pekit/materialized-trees).

A tree that holds one content at several paths gives the first a link to
the blob and each other a link to a numbered copy, `<digest>.r1` and on,
which the store keeps. So no two files of a staged tree are one file, and
placing the tree copies nothing that was copied before.

Where linking fails, as across file systems, a file is copied.

## Removing it

When a step is done, its staging directory is renamed, which takes no
time, and a thread of its own removes it while the build goes on
(`sweep::Sweeper`). The build waits for that thread before it ends, and a
directory that can't be removed fails the build at its end.

A process killed before it removed a staging directory leaves it behind.
The next build that holds the store's lock alone removes it, as
[Concurrency and Sweeping](~pekit/concurrency-and-sweeping) describes.
