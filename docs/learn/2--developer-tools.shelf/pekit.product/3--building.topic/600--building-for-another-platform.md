---
title: Building for Another Platform
type: how-to
description: Cross-compile a target for another architecture with
  --platform or a with block, register a toolchain for each platform, and
  build tools that run during the build for the machine itself.
related:
  - pekit/concepts/platforms-and-toolchains
  - pekit/building/registering-toolchains
  - pekit/languages/building-c
---

pekit can build a target for a platform other than the machine it runs
on, such as 64-bit Arm Linux from an x86-64 laptop. The target's code
doesn't change. Each library step asks for a toolchain that builds for the
platform, and pekit picks a cross compiler.

## Before you start

- A workspace that registers a toolchain building for the platform, as
  [Registering Toolchains](~pekit/registering-toolchains) shows. The gcc
  library's `gcc_x86_64_to_aarch64` runs on x86-64 Linux and builds for
  aarch64 Linux.

## Build one target for another platform

1. Register a toolchain for each platform in `workspace.gcsl`:

   ```gcsl
   import Toolchain::* from "github.com/gcslib/gcc"
   import Toolchain from toolchain

   pekitconf = {toolchains = [
       Toolchain::C gcc_x86_64,
       Toolchain::C gcc_x86_64_to_aarch64,
   ]}
   ```

2. Build with `--platform` and the platform's name:

   ```sh
   pekit build --platform aarch64 hello
   ```

   A platform's name is its architecture and operating system, such as
   `aarch64-linux`. A name without an operating system, such as `aarch64`,
   keeps the machine's.

## Check the result

Check the program pekit printed:

```sh
file -L pekit-out/hello/link/hello
```

The output names the architecture:

```text
pekit-out/hello/link/hello: ELF 64-bit LSB pie executable, ARM aarch64, version 1 (SYSV), dynamically linked, interpreter /lib/ld-linux-aarch64.so.1, for GNU/Linux 3.7.0, not stripped
```

Building again without `--platform` builds for the machine, with its own
steps. Each platform's steps are keyed apart, so switching back and forth
reuses both from the store.

## Build for several platforms at once

A target can build the same thing for several platforms by setting the
`platform` context with `with`:

```gcsl
def build all() {
    x86_64 = build hello() with { platform = {arch = "x86_64"; os = "linux"} }
    aarch64 = build hello() with { platform = {arch = "aarch64"; os = "linux"} }
}
```

`pekit build all` builds both, as one build. Both programs come from a
step of the gcc library's `link`, so the second is under `link.2`:

```text
built /all: 0 steps run, 4 found in the store
  pekit-out/all/link/hello
  pekit-out/all/link.2/hello
```

Here every step was found in the store, from the two earlier builds.

## Build tools that run during the build

A program the build makes and then runs, such as a code generator, has to
run on the machine the steps run on, whatever the build is for. Build it
for `^host_platform`:

```gcsl
let generator = build msggen() with { platform = ^host_platform }
```

Link such a tool statically, as with the gcc library's `flags =
["-static"]`. A step that runs it then needs nothing of the toolchain that
built it.
