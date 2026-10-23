# Example 6: one program, two architectures, one build

This is example 3's program again, with the `embed` tool that turns
`data/message.txt` into C during the build. This time `app` is built for two
architectures at once: `x86_64` and `aarch64` (64-bit ARM). The build machine
is `x86_64`.

The sources are copied from example 3.

## What must happen

By hand, with a cross-compiler for ARM installed as `aarch64-linux-gnu-gcc`:

```sh
cc -o embed gen/embed.c                              # 1. the tool, for THIS machine
./embed data/message.txt message out/                # 2. run it, once

cc -c -Iout -o x86_64/main.o main.c                  # 3. app for x86_64
cc -c -o x86_64/message.o out/message.c
cc -o x86_64/app x86_64/main.o x86_64/message.o

aarch64-linux-gnu-gcc -c -Iout -o aarch64/main.o main.c    # 4. app for aarch64
aarch64-linux-gnu-gcc -c -o aarch64/message.o out/message.c
aarch64-linux-gnu-gcc -o aarch64/app aarch64/main.o aarch64/message.o
```

These commands haven't been run, because this machine has no ARM
cross-compiler. Examples 1 to 5 were run.

Points to notice:

- **`embed` is built once, for `x86_64`, even though half the output is for
  ARM.** It runs on the build machine. An ARM `embed` couldn't run here at
  all.
- **Step 2 runs once.** Its output, C source, is the same for both
  architectures, so both builds share it.
- **Steps 3 and 4 are the same build with a different compiler.** Nothing in
  `main.c` or the build logic differs, only which toolchain is used.
- If the build machine were itself ARM, `embed` would be built with the ARM
  compiler, and nothing in the build file should need to change.

## The two problems this example is about

**1. Choosing a toolchain without threading it everywhere.** Since example 1,
every compile has said `compiler = ...`. With two architectures, every target
would need to know which compiler to pass, and whoever calls it would have to
tell it. Example 2 already showed how easy one of those is to miss. Bazel's
answer is *toolchain resolution*: a target says "I need a C compiler", and the
current *configuration* decides which one it gets.

**2. "For the build machine" versus "for the target".** `embed` must be
compiled for whatever machine runs the build. `app` must be compiled for
whatever it's being built for. Bazel calls these the *exec* and *target*
configurations, and calls switching between them a *transition*.

Together these mean a target can be built more than once in one build, in
different configurations. Each one is a separate node with its own cache
entries, so the `x86_64` and `aarch64` objects never collide.

## What to write

We'll work through this one together. The first question is the shape: how
does the current configuration reach `gcc.compile`?
