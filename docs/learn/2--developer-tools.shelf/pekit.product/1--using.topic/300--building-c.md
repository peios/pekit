---
title: Building C
type: guide
description: Compile, archive, and link C with the gcc library, for the host or for another platform.
---

The gcc library builds C programs and static libraries. Import it as
`github.com/gcslib/gcc`. Each of its rules is one step, so pekit reruns
only the steps whose inputs changed.

```text
import "github.com/gcslib/gcc"

def build hello() {
    = gcc.link([gcc.compile(file("hello.c"))], output = "hello")
}
```

## The rules

| Rule | Makes |
|---|---|
| `compile(src, headers, output, flags)` | The object of one C source, by default its name with `.o` |
| `archive(objs, output)` | A static library of objects, by `ar rcsD`, with no times or owners recorded |
| `link(objs, libraries, output, flags)` | A program of objects and static libraries |

A compile's sandbox holds its source and the `headers` you give it, and
nothing else of the workspace. Each header's directory is passed to the
compiler with `-I`. Give a compile exactly the headers it includes: one it
isn't given fails to compile, and one it's given but doesn't include
reruns it for nothing when it changes.

A library target can give its archive and its public headers together,
so that a program compiles against the public headers alone:

```text
def build greet() {
    let obj = gcc.compile(file("lib/greet.c"), headers = [file("lib/greet.h"), file("lib/internal.h")])
    archive = gcc.archive([obj], output = "libgreet.a")
    headers = [file("lib/greet.h")]
}

def build app() {
    let g = build greet()
    let obj = gcc.compile(file("main.c"), headers = g.headers)
    = gcc.link([obj], libraries = [g.archive], output = "app")
}
```

Editing `lib/internal.h` then reruns the library's compile, its archive,
and the link, but never the program's compile.

## Building upstream code

Code from elsewhere, built with its own build system, is one step:
`buildenv.configure_make` runs `./configure`, `make`, and `make install`
on a source directory and gives the directory it installed into.

```text
import unpack from archive
import "github.com/gcslib/buildenv"

def build zlib() {
    let src = unpack(fetch("https://zlib.net/fossils/zlib-1.3.1.tar.gz", sha256 = "9a93..."))
    let out = buildenv.configure_make(src.dir("zlib-1.3.1"), configure = ["--static"])
    headers = [out.file("include/zlib.h"), out.file("include/zconf.h")]
    archive = out.file("lib/libz.a")
}
```

The build runs with the registered C toolchain, given to it as `CC`,
`AR`, `RANLIB`, and `LD`, in the registered build environment. The
buildenv library's `busybox_x86_64` gives `/bin/sh` and the usual tools
from a static busybox, and GNU make built from source, all pinned. The
source is copied to a writable directory first, and every path is the
same on every run, so anything the build records, such as its install
prefix, doesn't change from one run to the next. Pekit can't see inside
the step, so any change to the source rebuilds all of it.

To use the host's own tools instead, register `host_tools` from the
buildenv library and `system_c` from the `toolchain` module:

```text
import * from "github.com/gcslib/buildenv"
import * from toolchain

pekitconf = {toolchains = [Toolchain::C system_c, Toolchain::BuildEnv host_tools]}
```

## Tools built during the build

A program the build makes and then runs, such as a code generator, is a
file like any other: pass it to the step that runs it. Build it for the
machine steps run on with `with { platform = ^host_platform }`, and link it
with `flags = ["-static"]`, so that it runs in a step's sandbox with
nothing of the toolchain that built it.

## Building for another platform

Each rule resolves its toolchain for the `platform` context, so the same
target builds for another platform when called in a `with`:

```text
def build all() {
    x86_64 = build app() with { platform = {arch = "x86_64"; os = "linux"} }
    aarch64 = build app() with { platform = {arch = "aarch64"; os = "linux"} }
}
```

That needs a toolchain registered for each platform (see
~pekit/building-a-target). The gcc library's Bootlin toolchains are:

| Toolchain | Runs on | Builds for |
|---|---|---|
| `gcc_x86_64` | x86-64 Linux | x86-64 Linux, with glibc |
| `gcc_x86_64_to_aarch64` | x86-64 Linux | aarch64 Linux, with glibc |

Each is fetched by its SHA-256 digest and unpacked once. Its compiler runs
in a sandbox whose root is the toolchain's own runtime, so a build with
pinned toolchains reads nothing of the host's compiler or libraries.
