---
title: Building a First Program
type: how-to
description: Create a workspace, write a build file with one step that
  compiles a C program, build it with pekit build, and watch pekit reuse
  the result.
related:
  - pekit/getting-started/workspaces
  - pekit/concepts/targets-and-steps
  - pekit/languages/building-c
---

This page builds a one-file C program from an empty directory. Along the
way it shows the parts every pekit build has: a workspace, a build file, a
target, and a step. It takes about five minutes.

## Before you start

You need:

- pekit. To build it from a checkout of its source, run
  `cargo install --path crates/pekit --locked` in the checkout.
- bubblewrap, the `bwrap` program, which pekit runs every step in.
- A C compiler from your distribution's packages, as `/usr/bin/cc`. A step
  can see the host's `/usr`, but not `/usr/local`.

## Create the workspace

1. Make a directory for the workspace, and change into it:

   ```sh
   mkdir hello
   cd hello
   ```

2. Create an empty `workspace.gcsl`. Its presence makes the directory a
   workspace; it can hold settings, but this one doesn't need any.

   ```sh
   touch workspace.gcsl
   ```

3. Save this program as `hello.c`:

   ```c
   #include <stdio.h>

   int main(void) {
       puts("Hello from pekit");
       return 0;
   }
   ```

## Write the build file

Save this as `build.gcsl`, beside `hello.c`:

```gcsl
#[hermetic]
fn compile(source: File) -> File {
    let compiled = exec(["cc", "-o", "hello", source.path])
    = working_dir().file("hello")
}

def build hello() {
    = compile(file("hello.c"))
}
```

The file declares two things:

- **`compile`**, a function that runs the compiler on one source file and
  gives the program it made. `#[hermetic]` makes each call of it a step:
  pekit runs the call in a sandbox that holds only `source`, and keeps
  what it gives. `working_dir()` is the step's own directory, where the
  compiler writes `hello`.
- **`hello`**, a target. `def build` declares a definition you can name to
  `pekit build`. Its body calls `compile` with the file `hello.c`, found
  relative to the build file.

## Build it

1. Build the target:

   ```sh
   pekit build hello
   ```

   pekit runs the one step, and shows what the target gave:

   ```text
   built /hello: 1 step run, 0 found in the store
     pekit-out/hello/compile/hello
   note: steps ran with the host's own tools, so this build is not hermetic
   ```

   The note says that the step could use your machine's own tools, and
   this one did: it ran your `cc`. That's fine to start with. [Hermetic
   and Strict Builds](~pekit/hermetic-and-strict-builds) explains how to
   build without them.

2. Run the program:

   ```sh
   ./pekit-out/hello/compile/hello
   ```

   It prints `Hello from pekit`.

3. Build again:

   ```sh
   pekit build hello
   ```

   Nothing changed, so pekit takes the step's result from its store
   instead of running it:

   ```text
   built /hello: 0 steps run, 1 found in the store
     pekit-out/hello/compile/hello
   ```

## Change the program

1. In `hello.c`, change the message to `Hello again`.

2. Build again. pekit sees that the step's input changed, and runs it:

   ```text
   built /hello: 1 step run, 0 found in the store
     pekit-out/hello/compile/hello
   ```

3. Change the message back to `Hello from pekit`, and build once more.
   pekit has run the step with that input before, so it finds the result
   in the store and runs nothing.

pekit tells a changed file by its contents, never by its modification
time. Saving a file without changing it, or changing it and changing it
back, rebuilds nothing.

## Check the result

`pekit-out/hello/compile/hello` is the program the last build made, and
`./pekit-out/hello/compile/hello` prints `Hello from pekit`. The path names
the target, `hello`, then the step that made the file, `compile`. [Where
Results Go](~pekit/where-results-go) describes the rest of `pekit-out/`.

## Next steps

A real C build compiles and links many files, and uses a compiler pekit
pins rather than the host's. The gcc library does both: see
[Building C](~pekit/building-c). [Targets and Steps](~pekit/targets-and-steps)
explains what pekit did during each build.
