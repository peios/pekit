---
title: Building Upstream Software
type: how-to
description: Build software that has its own build system, such as
  ./configure and make, as one step with the buildenv library, in a pinned
  environment or with the host's tools.
related:
  - pekit/languages/building-c
  - pekit/building/fetching-files
  - pekit/building/registering-toolchains
---

Code from elsewhere usually comes with its own build system. The buildenv
library runs one as a single step: `configure_make` runs `./configure`,
`make`, and `make install` on a source directory, and gives the directory
the build installed into. Import it as `github.com/gcslib/buildenv`.

## Before you start

- The buildenv and gcc libraries. See [Libraries](~pekit/libraries).
- A C toolchain and a build environment registered in `pekitconf`. This page
  registers pinned ones.

## Build zlib from source

1. Register a C toolchain and a build environment in `workspace.gcsl`:

   ```gcsl
   import Toolchain::* from "github.com/gcslib/gcc"
   import Toolchain::* from "github.com/gcslib/buildenv"
   import Toolchain from toolchain

   pekitconf = {toolchains = [Toolchain::C gcc_x86_64, Toolchain::BuildEnv busybox_x86_64]}
   ```

   `busybox_x86_64` gives `/bin/sh` and the usual tools from a static
   busybox, and GNU make built from its pinned source, all fetched by their
   digests.

2. In a build file, fetch and unpack the source, and build it:

   ```gcsl
   import unpack from archive
   import "github.com/gcslib/buildenv"

   def build zlib() {
       let src = unpack(fetch("https://zlib.net/fossils/zlib-1.3.1.tar.gz", sha256 = "9a93b2b7dfdac77ceba5a558a580e74667dd6fede4585b91eefb60f03b72df23"))
       let out = buildenv.configure_make(src.dir("zlib-1.3.1"), configure = ["--static"])
       headers = [out.file("include/zlib.h"), out.file("include/zconf.h")]
       archive = out.file("lib/libz.a")
   }
   ```

   `configure` words go to `./configure` after `--prefix`, and `make`
   words go to `make`. Each word is one argument, as written.

3. Build it:

   ```sh
   pekit build zlib
   ```

## Check the result

pekit prints the paths of the two headers and `libz.a`, in the directory
the build installed into. The target gives them in the same shape as the
gcc library's own libraries, so a program can compile against `headers`
and link `archive`.

## What the step does

The build runs with the registered C toolchain, given to it as `CC`, `AR`,
`RANLIB`, `OBJCOPY`, and `LD`, in the registered build environment. The
step copies the source to a writable directory first, as a build system
expects. Every path is the same on every run, so anything the build
records, such as its install prefix, doesn't change from one run to the
next. Each static archive it installs is made deterministic with
`objcopy -D`, so the same source gives the same archive.

pekit can't see inside the step, so any change to the source rebuilds all
of it.

## Use the host's tools

To build with the host's own compiler and tools instead, register
`host_tools` from the buildenv library and `system_c` from the `toolchain`
module:

```gcsl
import Toolchain::BuildEnv host_tools from "github.com/gcslib/buildenv"
import * from toolchain

pekitconf = {toolchains = [Toolchain::C system_c, Toolchain::BuildEnv host_tools]}
```

The step then runs in its own sandbox with the host's tools, and a strict
build refuses it.
