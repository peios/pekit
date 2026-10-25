---
title: Querying What Targets Reach
type: guide
description: Ask which steps and files a target reaches, or which targets a file change reaches, without building anything.
---

`pekit query` evaluates targets as a build does, but runs no step. It
answers from the code alone, so it's quick, and it doesn't fill the
store.

## What a target reaches

`pekit query deps` lists each step a target reaches, with the
workspace files each reads: the files of its code, the files and
directories it's given, and the steps whose results it's given:

```text
$ pekit query deps chain
app/chain
  code: app/build.gcsl
  step app/chain/upper
    code: app/build.gcsl
    reads: app/in.txt
  step app/chain/steady
    code: app/build.gcsl
  step app/chain/joined
    code: app/build.gcsl
    after: app/chain/steady, app/chain/upper
```

Name targets as `pekit build` names them (see ~pekit/naming-targets).
`--kind test` asks of tests instead of builds, and `--kind package` of
packages.

A target's own code, outside its steps, still runs: its fetches, its
unpacks, and its reads of the workspace, which it needs to say what it
builds. A target that reads what a step made, a dynamic dependency,
can't go further without running that step, so the query stops there:

```text
  unknown: it waits at app/build.gcsl:15 for a step's result, so what it reaches after is known only once that step runs
```

## What a change reaches

`pekit query rdeps` lists the targets that read a file, one to a line:
the question to ask in CI to run only the tests a change reaches.

```sh
pekit test $(pekit query rdeps --kind test app/main.c)
```

If no test reads the file, the list is empty, and `pekit test` given no
names runs every test: check for an empty list first if you'd rather run
none.

It asks of every target below the current directory, or of those you
name after the file. A GCSL file is read by every target whose code is
in its directory or below, since what it defines is seen there, whether
or not anything uses it yet. `workspace.gcsl` is read by everything.

A target that waits for a step's result might read the file after, so
it's listed too, with a note on standard error. An unknown is never
taken to mean the file isn't read.
