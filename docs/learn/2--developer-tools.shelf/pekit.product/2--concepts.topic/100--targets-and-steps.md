---
title: Targets and Steps
type: concept
description: How pekit evaluates a target's GCSL code, turns calls of
  hermetic functions into steps, runs steps in parallel and shares them
  between targets, and handles a target that waits for a step's result.
related:
  - pekit/concepts/step-sandboxes
  - pekit/concepts/reuse-and-the-store
  - pekit/building/writing-steps
---

A build has two kinds of work. A target's own code is GCSL that decides
what to build. A step is one piece of the actual building, such as running
a compiler, and pekit runs it in a sandbox and keeps its result. Knowing
which code is which explains what pekit reuses, what it runs in parallel,
and what a build can see.

## Definitions and targets

A build file declares definitions, each made with a definition type:

| Definition type | Declares | Run by |
|---|---|---|
| `build` | Something to build: a file, a directory, or a record of them. | `pekit build` |
| `test` | A check that passes or fails. | `pekit test` |
| `package` | A package's name, version, and files. | `pekit package` |
| `script` | A task you run by hand, which can change things outside the build. | `pekit run` |

A target is a definition you name on the command line. `pekit build hello`
builds the `build` definition called `hello`. Its body is GCSL code, and
whatever the body gives is what the target built.

## Steps

A step is a call of a function marked `#[hermetic]`:

```gcsl
#[hermetic]
fn compile(source: File) -> File {
    let compiled = exec(["cc", "-c", "-o", "out.o", source.path])
    = working_dir().file("out.o")
}
```

When a target's code calls `compile`, pekit doesn't run the call there.
It records the call as a step, with its arguments, and the target's code
carries on. pekit runs the step separately, in a sandbox holding only the
step's arguments, and keeps what it gives. Libraries such as the gcc
library are made of functions like this one, so a target that calls
`gcc.compile` and `gcc.link` makes two steps.

A step is identified by everything that can change what it gives: its
function's code, its arguments, the contents of the files it's given, and
the platform it runs on. A step with the same identity as one pekit has
already run is found in the store, and doesn't run again. [Reuse and the
Store](~pekit/reuse-and-the-store) describes how.

A call of a function without `#[hermetic]` is ordinary GCSL. It runs as
part of whatever code called it: in the target's code, or inside the step
that called it.

## A build is a graph

Because a target's code only records each step, pekit learns every step it
needs before running any of them. A step that takes another step's result
as an argument runs after that step. Steps that don't depend on each other
run at the same time, up to the number of programs `--jobs` allows, which
is by default the number of processors.

```gcsl
import "github.com/gcslib/gcc"

def build app() {
    let objects = [gcc.compile(file("main.c")), gcc.compile(file("util.c"))]
    = gcc.link(objects, output = "app")
}
```

Here the two compiles run at once, and the link runs when both have
finished. The target's code ran once, start to finish, before any of them.

In a terminal, pekit shows a line with the steps running and how many have
finished while the build goes, and clears it when the build ends.

## Several targets are one build

`pekit build app tool` builds both targets as one build. A step that both
targets need, such as a library they both link, runs once. Their other
steps run at the same time as each other.

A step's identity doesn't depend on which target asked for it. Two targets
that make the same call share the step, and so does the next build that
makes it, in this workspace or any other on the machine.

If one target fails, the others carry on. pekit reports each target, and
exits with status 1 if any failed.

## Waiting for a step's result

A target's code can use what a step made to decide what to build next. In
this target, `generate` is a hermetic function that runs a generator,
`msggen`, which writes a directory of C files. The code lists those files
and compiles each:

```gcsl
def build app() {
    let out = generate(build msggen(), file("data/messages.txt"))
    let objects = for f in out.files() if f.path.ends_with(".c") {
        gcc.compile(f)
    }
    = gcc.link(objects, output = "app")
}
```

`out.files()` needs the generator's result. The target's code stops there,
pekit runs the generator's step, and the code carries on from where it
stopped, with the files. This is a dynamic dependency: the compile steps
aren't known until the generator has run.

Dynamic dependencies cost a pause, but nothing else. The steps before the
pause still run in parallel, and the code never runs twice. A build that must
know all its steps before anything runs, such as one in continuous
integration, can forbid them with `--no-dynamic`. It fails a target at the
point the target waits.

Waiting for a fetch, or for an archive to unpack, isn't a dynamic
dependency. pekit does those for the target's code itself, as part of
evaluating it.

## What a target's own code can do

A target's code runs outside any sandbox, on your machine. It can fetch
files, unpack archives, read the workspace and what steps made, and run
programs. It sees the workspace at `/w` and outputs at `/pekit-out`, as
steps do, but its effects aren't cached: they happen every time the code
runs.

So keep the work in steps. Code that runs a compiler in a target's own
body runs it on every build, however little changed. A strict build goes
further, and refuses a target's own code anything but fetching, unpacking,
reading, and asking for secrets. See [Hermetic and Strict
Builds](~pekit/hermetic-and-strict-builds).

## Calls inside a step

Only a call made by a target's own code becomes a step. Inside a step, a
call of another hermetic function runs as part of the step, in the same
sandbox, and isn't cached on its own. To cache two pieces of work
separately, call both from the target's code, and pass the first one's
result to the second.
