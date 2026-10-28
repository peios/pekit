---
title: Querying What Targets Reach
type: how-to
description: List the steps and workspace files a target reaches, or the
  targets a changed file reaches, with pekit query, without building
  anything.
related:
  - pekit/concepts/targets-and-steps
  - pekit/testing-and-running/testing
---

`pekit query` evaluates targets as a build does, but runs no step. It
answers from the code alone, so it's quick, and it doesn't fill the store.
Use it to see what a target depends on, or to find the targets a change
affects.

## List what a target reaches

Run `pekit query deps` with the target's name:

```sh
pekit query deps app
```

pekit lists each step the target reaches, with the workspace files each
reads, and the steps whose results each is given:

```text
example/app
  code: example/build.gcsl
  step .steps/compile/57b56f08130b0576
    reads: example/lib/greet.c, example/lib/greet.h, example/lib/internal.h
  step .steps/archive/cbefc0a26b666e11
    after: .steps/compile/57b56f08130b0576
  step .steps/compile/225fcd6be0b8661d
    reads: example/lib/greet.h, example/main.c
  step .steps/link/7f71dca914d576d2
    after: .steps/archive/cbefc0a26b666e11, .steps/compile/225fcd6be0b8661d, .steps/compile/57b56f08130b0576
```

`code` lists the build files the target's code is in. Each step is named by
its directory under `pekit-out/`.

Name targets as `pekit build` does. `--kind test` asks about tests instead
of builds, and `--kind package` about packages.

## When a query can't see further

A target's own code still runs: its fetches, its unpacks, and its reads of
the workspace, which it needs to say what it builds. A target that waits
for a step's result, a dynamic dependency, can't go further without running
that step. The query stops there, and says so:

```text
  unknown: it waits at build.gcsl:22 for a step's result, so what it reaches after is known only once that step runs
```

## List the targets a change reaches

Run `pekit query rdeps` with a file's path, relative to the current
directory:

```sh
pekit query rdeps example/main.c
```

pekit lists each target that reads the file, one to a line. It asks about
every target below the current directory, or about those you name after
the file.

A GCSL file is read by every target whose code is in its directory or
below, since what it declares is visible there, whether or not anything
uses it yet. `workspace.gcsl` is read by every target.

A target that waits for a step's result might read the file after the
wait, so it's listed too, with a note on standard error:

```text
note: `/app` waits at build.gcsl:22 for a step's result, so it might read `main.c` after: it is listed
```

An unknown is never taken to mean that the file isn't read.

## Run only the tests a change reaches

In continuous integration, you can pass the list to `pekit test`:

```sh
pekit test $(pekit query rdeps --kind test app/main.c)
```

If the list is empty, `pekit test` given no names runs every test. Check
for an empty list first if you'd rather run none.

Every test waits for the result of its own step, so `pekit query rdeps
--kind test` lists every test, with a note for each one it can't rule out.
For tests, the list is complete, but it holds every test, so this runs
them all.
