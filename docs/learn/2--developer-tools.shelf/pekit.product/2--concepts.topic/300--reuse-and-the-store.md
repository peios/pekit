---
title: Reuse and the Store
type: concept
description: How pekit decides that a step's result can be reused, what
  keys a step, how it tells a changed file, how it reuses the programs of a
  step that runs again, and how the store is shared and kept to its size
  limit.
related:
  - pekit/concepts/targets-and-steps
  - pekit/building/managing-the-store
  - pekit/concepts/hermetic-and-strict-builds
---

pekit keeps the result of every step that succeeds in its store. When a build
needs a step that pekit has already run with the same inputs, pekit takes the
result from the store instead of running the step again. This page explains
what "the same" means, so you can predict what a change rebuilds.

## What keys a step

A step's key is everything that can change what it gives:

- **Its code.** The source files that declare its function and every
  function it calls, library files included. Any change to one of those
  files counts, even to a comment.
- **Its arguments.** Every value passed to it, and the contents of every
  file and directory among them.
- **Where it writes.** The step's own directory, which is named by what the
  step is.
- **Its platform.** The kind of machine it runs on.
- **The contexts it reads.** Only those it actually read as it ran.
- **The host's packages.** A fingerprint of what's installed, unless the
  build is strict. See [Hermetic and Strict Builds](~pekit/hermetic-and-strict-builds).

When every part of the key matches a step pekit recorded, the build reports
the step as found in the store:

```text
built app/hello: 0 steps run, 2 found in the store
```

Change a file a step reads, and that step runs again. So does every step
that takes what it gives, unless the step that ran again gave exactly the
same files as before. A comment changed in a C source, say, reruns its
compile, but if the object file comes out the same, the link is found in
the store.

## How pekit tells a changed file

pekit compares files by their contents, never by their modification times. To
avoid reading the whole workspace on every build, it remembers each file's
size, times, and inode from the last build. It reads again only a file where
one of those changed. A workspace where nothing changed builds without reading
any file.

Saving a file without changing it doesn't rebuild anything, and neither
does changing a file and changing it back.

## Contexts a step reads

GCSL contexts, such as `^platform`, pass settings through every call
without being arguments. A step is keyed only by the contexts it read as it
ran. Registering another toolchain, or binding a context the step never
reads, doesn't make it run again.

A step that read a context nothing bound, and so got the context's default,
is keyed apart from the same step with the context bound, even to the same
value. A step is given the files a context names only once it reads that
context, so a program it runs can't reach a file its key doesn't cover.

## Programs found in a step that runs

A step runs again when anything in its key changes, including its code.
Editing a comment in the file that declares a step, or in a library it
uses, makes it run. Most of the programs it runs then see exactly what they
saw before.

So pekit also records each program a step runs. A program's key is what it can
see: the files the step was given, the step's own directory and `/tmp` as they
are when the program starts, and everything the step did before it. When a
step runs again, a program that sees the same as before isn't run. pekit puts
back what it wrote, and the step goes on:

```text
built app/hello: 1 step run (2 programs found), 0 found in the store
```

The first program that sees something new runs, and so does each program
after it that sees what that program wrote.

pekit records no program of a step from the moment the step is given a
secret. Nor does it record a program once a sandbox of the step removes a
file of the directory the sandbox was made of, because the store can't hold
a removal.

## What's never reused

Some results are never taken from the store:

- A step whose result holds a secret. It runs on every build.
- A step that failed. Only a step that succeeds is recorded.
- Everything, in a build run with `--rebuild`, which runs every step and
  every program again and records each as it goes.

## One store for the machine

Every workspace on a machine shares one store. A step's key doesn't include
where the workspace is, so two checkouts of the same commit, or two
workspaces that build the same library, share their results.

The store is in pekit's cache directory: `$PEKIT_CACHE_DIR` if you set it,
otherwise `pekit` under `$XDG_CACHE_HOME`, otherwise `~/.cache/pekit`.

Files in the store are kept by their SHA-256 digests, so a file that
several steps give is stored once. That includes every file pekit fetches:
a pinned download already in the store is never downloaded again.

## Keeping the store to its size

The store has a size limit, 50 GiB unless you set `PEKIT_CACHE_LIMIT`.
When a build ends with the store over its limit, pekit evicts until the
store is under nine tenths of it. It removes first what is cheapest to make
again for the space it takes, and among those, what was used longest ago.
A large file that took a second to make goes before a small one that took a
minute.

Eviction removes files, not records. A step whose outputs were evicted
isn't found in the store: it runs again and puts them back. While another
pekit process is using the store, a build evicts nothing, and a later
build does it instead. [Managing the Store](~pekit/managing-the-store)
shows how to set the limit, and how to clear the store.
