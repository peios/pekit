---
title: Writing Steps
type: how-to
description: Write hermetic functions that pekit runs as cached steps,
  covering their inputs, the files they give, toolchains, composed
  directories, failures, retries, timeouts, devices, and scratch
  directories.
related:
  - pekit/concepts/targets-and-steps
  - pekit/concepts/step-sandboxes
  - pekit/libraries/writing-a-library
---

A step is a GCSL function marked `#[hermetic]`. Libraries such as the gcc
library provide steps for common work. Any build can also declare its own: to
run a code generator, to call a tool no library wraps, or to arrange files in
a directory. This page writes a small C build from scratch, then covers the
rest of what a step can do.

## Before you start

You need a workspace with a `src` directory holding these three files.
`src/add.c`:

```c
int add(int a, int b) { return a + b; }
```

`src/add.h`:

```c
int add(int a, int b);
```

`src/main.c`:

```c
#include "add.h"
#include <stdio.h>

int main(void) {
    printf("%d\n", add(2, 3));
    return 0;
}
```

## Write the steps

1. In a `build.gcsl` beside `src`, declare a step that compiles one source
   file:

   ```gcsl
   import path

   #[hermetic]
   fn compile(source: File, headers: List<File> = []) -> File {
       let object = path.basename(source.path).strip_suffix(".c") + ".o"
       let includes = for h in headers { "-I{path.dirname(h.path)}" }
       let compiled = exec(["cc", "-c", ...includes, "-o", object, source.path])
       = working_dir().file(object)
   }
   ```

   The step's sandbox holds only `source` and `headers`, at their paths
   under `/w`. A header the step isn't given doesn't exist there, so the
   compile fails rather than reading a file pekit doesn't know about.

2. Declare a step that links objects into a program. A step can give a
   record of several files:

   ```gcsl
   #[hermetic]
   fn link(objects: List<File>, name: String) -> {program: File, map: File} {
       let paths = for o in objects { o.path }
       let linked = exec(["cc", "-o", name, "-Wl,-Map={name}.map", ...paths])
       program = working_dir().file(name)
       map = working_dir().file("{name}.map")
   }
   ```

3. Declare a target that calls them:

   ```gcsl
   def build adder() {
       let headers = [file("src/add.h")]
       let objects = [compile(file("src/add.c")), compile(file("src/main.c"), headers = headers)]
       = link(objects, "adder")
   }
   ```

4. Build it:

   ```sh
   pekit build adder
   ```

   pekit runs the two compiles at the same time, then the link:

   ```text
   built /adder: 3 steps run, 0 found in the store
     pekit-out/adder/link/adder
     pekit-out/adder/link/adder.map
   ```

## Check the result

Run `./pekit-out/adder/link/adder`, which prints `5`. Then add a comment
to `src/add.c` and build again. The compile of `add.c` runs, but it makes
the same object file as before, so the link is found in the store:

```text
built /adder: 1 step run, 2 found in the store
```

## What a step takes and gives

A step's arguments can be any GCSL values. pekit gives the step every
`File` and `Dir` among them, at their paths, and keys the step by their
contents. Other values, such as strings and numbers, key the step by their
values.

What a step gives has to be files it made in its own directory, files or
directories it was given, or values without files. `working_dir()` is the
step's own directory, and `working_dir().file(name)` and
`working_dir().dir(name)` name what's in it. A step that gives a path
outside these fails.

A relative path such as `file("data.txt")` is relative to the build file
it's written in, even inside a step. It never names a file the step made.

## Compose a directory

`fs.compose` builds a directory from files and directories, each at the
path you give it. This step lays out a program and its documentation as an
install tree:

```gcsl
import fs
import path

#[hermetic]
fn layout(program: File, readme: File) -> Dir {
    = fs.compose([
        {at = "bin/{path.basename(program.path)}"; place = program},
        {at = "share/doc/README"; place = readme},
    ])
}
```

pekit links the files rather than copying them, and mounts a large
directory whole. Once it's made, nothing can change the composed
directory. To change files in it, run programs in a `Dir.sandbox` of it,
which is a copy of its own. GCSL's [File and Process
Effects](~gcsl/file-and-process-effects) page describes both.

## Use a toolchain

A step that runs a compiler should take the compiler as an argument, so
that pekit keys the step by the compiler's files. Resolve the toolchain in
a parameter default, which runs where the step is called:

```gcsl
import toolchain
import Toolchain from toolchain

#[hermetic]
fn compile(source: File, cchain: toolchain.CToolchain = toolchain.get(Toolchain::C)) -> File {
    let compiled = exec([cchain.cc.path, "-c", "-o", "out.o", source.path])
    = working_dir().file("out.o")
}
```

This step works with the host's own compiler, whose runtime is `/`. A
pinned toolchain's programs need its own runtime as their root, so a step
runs them in a `Dir.sandbox` of the runtime. The gcc library's `rules.gcsl`
shows how. See [Registering Toolchains](~pekit/registering-toolchains).

## Report errors at the call

When a program a step runs fails, pekit reports the error at the `exec`
in the step's code, with the program's standard error:

```text
error: `/bin/sh` exited with status 4
  --> build.gcsl:17:15
   |
17 |     let ran = exec(["/bin/sh", "-c", "echo to stdout; echo to stderr >&2; exit 4"])
   |               ^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^^
   |
   = note: its standard error: to stderr
```

In a library, mark a step `#[track_caller]` too, and an error in its body
is reported where your code called it. GCSL's
[Attributes](~gcsl/attributes) page describes it.

## Retry a step, or limit its time

Two attributes change how pekit runs a step:

- **`#[retry(n)]`** runs a step that failed up to `n` more times, each time
  from the start in a fresh sandbox. A step that passes only on a retry is
  flaky, and pekit warns about it:

  ```text
  warning: step `fetch_index` passed only when it was run again: it is flaky
  ```

- **`#[timeout(limit)]`** fails a step that runs longer than `limit`, such
  as `5m` or `30s`. The limit covers every attempt together, not each one.
  The time pekit spends placing the step's inputs doesn't count.

```gcsl
#[retry(2)]
#[timeout(5m)]
#[hermetic]
fn integration(server: File) -> File {
    let ran = exec([server.path, "--self-test"])
    = working_dir().file("report.txt")
}
```

A step that times out fails with `the step timed out`, at the program it
was running.

## Give a step a device

A step sees no device but the usual few, such as `/dev/null`. A step that
runs a virtual machine can ask for KVM, so the machine runs at full speed:

```gcsl
#[device("kvm")]
#[hermetic]
fn boots(iso: File) -> File {
    # Runs QEMU with -enable-kvm.
}
```

A device changes how fast a step runs, never what it gives, so it isn't
part of the step's key. On a machine without KVM, the step runs anyway, and
more slowly if its program falls back to emulation. `kvm` is the only
device pekit gives; asking for another fails the step.

## Keep state between runs

Some tools do much less on a second run when they keep state: a compiler
cache, Cargo's target directory, or a kernel's build tree. A step marked
`#[scratch]` sees a directory of its own at `/pekit-scratch` that pekit
keeps between its runs:

```gcsl
#[scratch]
#[hermetic]
fn kernel(source: Dir, config: File) -> File {
    # Runs make with its object directory under /pekit-scratch, then copies
    # the kernel image into the step's own directory.
}
```

The directory belongs to the step's own directory under `pekit-out/` and to
the platform it builds for. Changing the contents of the step's inputs
keeps it, and it isn't part of the step's key. So a step has to
give exactly what it would give with an empty directory. Use it only for
state that saves time, never for anything that changes the result. pekit
relies on this:

- A strict build gives the step an empty directory every time.
- When another build is running the same step at the same moment, this run
  gets an empty directory of its own rather than waiting.
- A program found in the store leaves the directory as it is, as
  [Programs found in a step that
  runs](~pekit/reuse-and-the-store#programs-found-in-a-step-that-runs)
  describes.

`pekit clean --cache` removes scratch directories, with the store.
